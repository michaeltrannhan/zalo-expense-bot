//go:build integration

package notify

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/messaging"
	"zl-expese-bot/internal/messaging/logprovider"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/store"
)

type recordingProvider struct {
	messaging.Provider
	mu    sync.Mutex
	calls int
	ref   messaging.ProviderMessageRef
	err   error
}

func (p *recordingProvider) Send(context.Context, messaging.OutboundMessage) (messaging.ProviderMessageRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.ref, p.err
}

func (p *recordingProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestReplyRepairsMissingQueueJob(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	rec := &domain.OutboundRecord{
		ID:             uuid.New(),
		UserID:         userID,
		Provider:       domain.ProviderZaloBot,
		ProviderChatID: "chat",
		IdempotencyKey: "repair-me",
		Body:           "hello",
	}
	if created, err := st.EnqueueOutbound(ctx, rec); err != nil || !created {
		t.Fatalf("seed outbox = %v, %v", created, err)
	}

	clk := clock.Fixed{T: time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)}
	enqueuer := NewEnqueuer(st, queue.NewPG(pool, clk), clk)
	if err := enqueuer.Reply(ctx, userID, domain.ProviderZaloBot, "chat", "hello", "repair-me"); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)::int FROM queue_jobs
		WHERE kind = 'outbound_send' AND dedupe_key = $1`,
		"outbound:"+rec.ID.String()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("repaired queue jobs = %d, want 1", jobs)
	}
}

func TestDeletionConfirmationIsPurgedAfterSend(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserStatus(ctx, userID, domain.UserDeleted); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Minute)
	clk := clock.Fixed{T: now}
	q := queue.NewPG(pool, clk)
	enqueuer := NewEnqueuer(st, q, clk)
	if err := enqueuer.DeletionConfirmation(ctx, userID, domain.ProviderZaloBot,
		"chat-delete", "deleted", "deleted:provider-message"); err != nil {
		t.Fatal(err)
	}
	job, err := q.Dequeue(ctx, []domain.JobKind{domain.JobOutboundSend}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	provider := logprovider.New(log)
	sender := NewSender(st, provider, clk, log, true, 3000)
	if err := sender.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}

	var outbound, jobs int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbound_messages WHERE user_id = $1`, userID).Scan(&outbound); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM queue_jobs WHERE id = $1`, job.ID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if outbound != 0 || jobs != 0 || len(provider.Sent()) != 1 {
		t.Fatalf("ephemeral state after send: outbound=%d jobs=%d sent=%d", outbound, jobs, len(provider.Sent()))
	}
}

func TestSuccessfulSendChargesSuccessQuotaOnce(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, "consent-v1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clk := clock.Fixed{T: time.Now().UTC().Add(time.Minute)}
	q := queue.NewPG(pool, clk)
	enqueuer := NewEnqueuer(st, q, clk)
	if err := enqueuer.Reply(ctx, userID, domain.ProviderZaloBot, "chat",
		"hello", "success-once"); err != nil {
		t.Fatal(err)
	}
	job, err := q.Dequeue(ctx, []domain.JobKind{domain.JobOutboundSend}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingProvider{ref: messaging.ProviderMessageRef{
		Provider: string(domain.ProviderZaloBot), ProviderMessageID: "zalo-123",
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := NewSender(st, provider, clk, log, true, 3000)
	if err := sender.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := sender.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}

	var status, providerMessageID string
	var attempts int
	if err := pool.QueryRow(ctx, `
		SELECT status, attempts, provider_message_id
		FROM outbound_messages WHERE idempotency_key = 'success-once'`).
		Scan(&status, &attempts, &providerMessageID); err != nil {
		t.Fatal(err)
	}
	var attemptCount, sentCount int
	if err := pool.QueryRow(ctx, `
		SELECT
		  COALESCE((SELECT count FROM usage_counters
		    WHERE scope='global' AND scope_id='' AND period=$1 AND metric='zalo_message_attempts'), 0),
		  COALESCE((SELECT count FROM usage_counters
		    WHERE scope='global' AND scope_id='' AND period=$1 AND metric='zalo_messages_sent'), 0)`,
		clk.Now().Format("2006-01")).Scan(&attemptCount, &sentCount); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.OutboundSent) || attempts != 1 || providerMessageID != "zalo-123" ||
		provider.Calls() != 1 || attemptCount != 1 || sentCount != 1 {
		t.Fatalf("delivery state=%s attempts=%d provider_id=%s calls=%d quota=%d/%d",
			status, attempts, providerMessageID, provider.Calls(), attemptCount, sentCount)
	}
}

func TestTransientProviderOutcomeBecomesAmbiguousWithoutRetry(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, "consent-v1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clk := clock.Fixed{T: time.Now().UTC().Add(time.Minute)}
	q := queue.NewPG(pool, clk)
	enqueuer := NewEnqueuer(st, q, clk)
	if err := enqueuer.Reply(ctx, userID, domain.ProviderZaloBot, "chat",
		"uncertain", "ambiguous-once"); err != nil {
		t.Fatal(err)
	}
	job, err := q.Dequeue(ctx, []domain.JobKind{domain.JobOutboundSend}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingProvider{err: domain.E(domain.CodeTransient, "connection reset after write", nil)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := NewSender(st, provider, clk, log, true, 3000)
	if err := sender.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := sender.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}

	var status string
	var attempts, sentCount int
	if err := pool.QueryRow(ctx, `
		SELECT status, attempts FROM outbound_messages
		WHERE idempotency_key = 'ambiguous-once'`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT count FROM usage_counters
		  WHERE metric='zalo_messages_sent' AND period=$1), 0)`,
		clk.Now().Format("2006-01")).Scan(&sentCount); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.OutboundAmbiguous) || attempts != 1 || provider.Calls() != 1 || sentCount != 0 {
		t.Fatalf("ambiguous state=%s attempts=%d calls=%d sent_quota=%d",
			status, attempts, provider.Calls(), sentCount)
	}
}

func TestRecoveredSendingStateIsNotSentAgain(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, "consent-v1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clk := clock.Fixed{T: time.Now().UTC().Add(time.Minute)}
	q := queue.NewPG(pool, clk)
	enqueuer := NewEnqueuer(st, q, clk)
	if err := enqueuer.Reply(ctx, userID, domain.ProviderZaloBot, "chat",
		"may already be delivered", "crash-boundary"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE outbound_messages
		SET status='sending', attempts=1, attempted_at=now()
		WHERE idempotency_key='crash-boundary'`); err != nil {
		t.Fatal(err)
	}
	job, err := q.Dequeue(ctx, []domain.JobKind{domain.JobOutboundSend}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingProvider{ref: messaging.ProviderMessageRef{ProviderMessageID: "must-not-send"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := NewSender(st, provider, clk, log, true, 3000)
	if err := sender.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM outbound_messages WHERE idempotency_key='crash-boundary'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.OutboundAmbiguous) || provider.Calls() != 0 {
		t.Fatalf("recovered status=%s provider_calls=%d", status, provider.Calls())
	}
}

func TestAttemptQuotaSuppressesBeforeProviderCall(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, "consent-v1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	clk := clock.Fixed{T: time.Now().UTC().Add(time.Minute)}
	q := queue.NewPG(pool, clk)
	enqueuer := NewEnqueuer(st, q, clk)
	if err := enqueuer.Reply(ctx, userID, domain.ProviderZaloBot, "chat", "one", "quota-one"); err != nil {
		t.Fatal(err)
	}
	if err := enqueuer.Reply(ctx, userID, domain.ProviderZaloBot, "chat", "two", "quota-two"); err != nil {
		t.Fatal(err)
	}
	provider := &recordingProvider{ref: messaging.ProviderMessageRef{ProviderMessageID: "ok"}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := NewSender(st, provider, clk, log, true, 1)
	for range 2 {
		job, err := q.Dequeue(ctx, []domain.JobKind{domain.JobOutboundSend}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := sender.Handle(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	var sent, suppressed, attempts int
	if err := pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE status='sent'),
		  count(*) FILTER (WHERE status='suppressed')
		FROM outbound_messages WHERE user_id=$1`, userID).Scan(&sent, &suppressed); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count FROM usage_counters
		WHERE metric='zalo_message_attempts' AND period=$1`,
		clk.Now().Format("2006-01")).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if sent != 1 || suppressed != 1 || attempts != 1 || provider.Calls() != 1 {
		t.Fatalf("quota sent=%d suppressed=%d attempts=%d calls=%d",
			sent, suppressed, attempts, provider.Calls())
	}
}
