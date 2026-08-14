//go:build integration

package bot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/categorisation"
	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/insight"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/store"
)

type failOutboundEnqueue struct {
	queue.Queue
}

func (q *failOutboundEnqueue) Enqueue(ctx context.Context, kind domain.JobKind, payload []byte, dedupeKey *string, maxAttempts int) (uuid.UUID, error) {
	if kind == domain.JobOutboundSend {
		return uuid.Nil, errors.New("injected outbound enqueue failure")
	}
	return q.Queue.Enqueue(ctx, kind, payload, dedupeKey, maxAttempts)
}

func testHandler(t *testing.T, pool *pgxpool.Pool, q queue.Queue, cfg config.Config) (*Handler, *store.Store, clock.Fixed) {
	t.Helper()
	st := store.New(pool)
	// Use a wall-clock-relative instant rather than a hardcoded date: the
	// store's pending-action expiry check compares against time.Now(), so a
	// fixed date in the past makes every pending action appear expired.
	now := time.Now().UTC().Add(time.Hour)
	clk := clock.Fixed{T: now}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	replies := notify.NewEnqueuer(st, q, clk)
	h := NewHandler(st, q, nil, replies, categorisation.NewService(st, clk),
		insight.NewService(st), clk, log, cfg)
	return h, st, clk
}

func isRedactedEnvelope(raw []byte) bool {
	var env map[string]any
	if json.Unmarshal(raw, &env) != nil {
		return false
	}
	return len(env) == 1 && env["redacted"] == true
}

func textEvent(user, chat, msg, text string, now time.Time) events.InboundEvent {
	raw, _ := json.Marshal(map[string]string{"text": text, "provider_user_id": user})
	return events.InboundEvent{
		Provider: string(domain.ProviderZaloBot), ProviderUserID: user,
		ProviderChatID: chat, ProviderMessageID: msg,
		EventType: domain.EventTextReceived, Text: text, ReceivedAt: now,
		RawPayloadHash: "hash-" + msg, RawPayload: raw,
	}
}

func TestRejectedAndPreConsentPayloadsAreRedacted(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	q := queue.NewPG(pool, clock.Real{})
	h, st, clk := testHandler(t, pool, q, config.Config{
		AppEnv:         config.EnvDevelopment,
		PilotAllowlist: map[string]bool{"allowed-user": true},
	})

	rejected := textEvent("stranger", "chat-r", "msg-rejected", "secret receipt text", clk.Now())
	if err := h.HandleEvent(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := pool.QueryRow(ctx, `
		SELECT raw_payload_json FROM provider_messages WHERE provider_message_id = 'msg-rejected'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !isRedactedEnvelope(raw) {
		t.Fatalf("rejected payload = %s, want redacted envelope", raw)
	}

	pending := textEvent("allowed-user", "chat-p", "msg-pending", "150000 ăn trưa secret", clk.Now())
	if err := h.HandleEvent(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT raw_payload_json FROM provider_messages WHERE provider_message_id = 'msg-pending'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !isRedactedEnvelope(raw) {
		t.Fatalf("pre-consent payload = %s, want redacted envelope", raw)
	}
	user, err := st.GetUserByIdentity(ctx, domain.ProviderZaloBot, "allowed-user", zaloScope)
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != domain.UserPending {
		t.Fatalf("status = %s, want pending", user.Status)
	}
}

func TestConcurrentFirstContactCreatesOneUser(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	q := queue.NewPG(pool, clock.Real{})
	h, _, clk := testHandler(t, pool, q, config.Config{AppEnv: config.EnvDevelopment})

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ev := textEvent("first-contact", "chat-c", "msg-c-"+uuid.NewString()[:8], "xin chào", clk.Now())
			errs <- h.HandleEvent(ctx, ev)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("HandleEvent: %v", err)
		}
	}
	var users, identities int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_identities`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if users != 1 || identities != 1 {
		t.Fatalf("users/identities = %d/%d, want 1/1", users, identities)
	}
}

func TestManualEntryRetryReusesDraft(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	base := queue.NewPG(pool, clock.Real{})
	h, st, clk := testHandler(t, pool, &failOutboundEnqueue{Queue: base}, config.Config{AppEnv: config.EnvDevelopment})

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIdentity(ctx, userID, domain.ProviderZaloBot, "sender-manual", zaloScope); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, consentVersion, clk.Now()); err != nil {
		t.Fatal(err)
	}

	ev := textEvent("sender-manual", "chat-m", "msg-manual-once", "50 lunch", clk.Now())
	if err := h.HandleEvent(ctx, ev); err == nil {
		t.Fatal("first attempt: want outbound failure")
	}
	if err := h.HandleEvent(ctx, ev); err == nil {
		t.Fatal("retry: want outbound failure")
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM transactions WHERE user_id = $1 AND source = 'manual'`,
		userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("manual drafts = %d, want 1", n)
	}
}

func TestPendingActionsPropagateOutboundAndStorageFailures(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	base := queue.NewPG(pool, clock.Real{})
	failQ := &failOutboundEnqueue{Queue: base}
	h, st, clk := testHandler(t, pool, failQ, config.Config{AppEnv: config.EnvDevelopment})

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIdentity(ctx, userID, domain.ProviderZaloBot, "sender-pa", zaloScope); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, consentVersion, clk.Now()); err != nil {
		t.Fatal(err)
	}

	txID := uuid.New()
	if err := st.CreateTransaction(ctx, &domain.Transaction{
		ID: txID, UserID: userID, Type: domain.TxExpense, AmountMinor: 10000, Currency: "VND",
		OccurredAt: clk.Now(), Status: domain.TxAwaitingConfirmation, Source: domain.TxSourceManual,
		Version: 1, CreatedAt: clk.Now(), UpdatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPendingAction(ctx, &domain.PendingAction{
		UserID: userID, Kind: domain.PendingConfirmExtraction, TransactionID: &txID,
		ExpiresAt: clk.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	actions := []struct{ id, text string }{
		{"pa-confirm", "ok"},
		{"pa-edit", "sửa tiền"},
	}
	for _, a := range actions {
		if err := st.SetPendingAction(ctx, &domain.PendingAction{
			UserID: userID, Kind: domain.PendingConfirmExtraction, TransactionID: &txID,
			ExpiresAt: clk.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		ev := textEvent("sender-pa", "chat-pa", a.id, a.text, clk.Now())
		if err := h.HandleEvent(ctx, ev); err == nil {
			t.Fatalf("%s: want outbound error", a.id)
		}
		var status string
		if err := pool.QueryRow(ctx, `
			SELECT status FROM provider_messages WHERE provider_message_id = $1`, a.id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != string(domain.MessageFailed) {
			t.Fatalf("%s status = %s, want failed", a.id, status)
		}
	}

	if err := st.SetPendingAction(ctx, &domain.PendingAction{
		UserID: userID, Kind: domain.PendingConfirmExtraction, TransactionID: &txID,
		ExpiresAt: clk.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	discardEv := textEvent("sender-pa", "chat-pa", "pa-discard", "bỏ qua", clk.Now())
	if err := h.HandleEvent(ctx, discardEv); err == nil {
		t.Fatal("discard: want outbound error")
	}

	freshID := uuid.New()
	if err := st.CreateTransaction(ctx, &domain.Transaction{
		ID: freshID, UserID: userID, Type: domain.TxExpense, AmountMinor: 20000, Currency: "VND",
		OccurredAt: clk.Now(), Status: domain.TxAwaitingConfirmation, Source: domain.TxSourceManual,
		Version: 1, CreatedAt: clk.Now(), UpdatedAt: clk.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION inject_tx_fail() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'injected storage failure'; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS inject_tx_fail() CASCADE`)
	})
	if _, err := pool.Exec(ctx, `
		CREATE TRIGGER inject_tx_fail BEFORE UPDATE ON transactions
		FOR EACH ROW EXECUTE FUNCTION inject_tx_fail()`); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPendingAction(ctx, &domain.PendingAction{
		UserID: userID, Kind: domain.PendingConfirmExtraction, TransactionID: &freshID,
		ExpiresAt: clk.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	okH, _, _ := testHandler(t, pool, base, config.Config{AppEnv: config.EnvDevelopment})
	ev := textEvent("sender-pa", "chat-pa", "pa-store", "ok", clk.Now())
	if err := okH.HandleEvent(ctx, ev); err == nil {
		t.Fatal("storage inject: want error")
	}
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM provider_messages WHERE provider_message_id = 'pa-store'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.MessageFailed) {
		t.Fatalf("storage inject status = %s, want failed", status)
	}
}
