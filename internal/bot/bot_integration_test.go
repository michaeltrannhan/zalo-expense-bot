//go:build integration

package bot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
	"zl-expese-bot/internal/summaryschedule"
)

type failFirstReceiptEnqueue struct {
	queue.Queue
	mu     sync.Mutex
	failed bool
}

func (q *failFirstReceiptEnqueue) Enqueue(ctx context.Context, kind domain.JobKind, payload []byte, dedupeKey *string, maxAttempts int) (uuid.UUID, error) {
	q.mu.Lock()
	if kind == domain.JobReceiptProcess && !q.failed {
		q.failed = true
		q.mu.Unlock()
		return uuid.Nil, errors.New("injected receipt enqueue failure")
	}
	q.mu.Unlock()
	return q.Queue.Enqueue(ctx, kind, payload, dedupeKey, maxAttempts)
}

func TestFailedImageWebhookRetryResumesSinglePipeline(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	clk := clock.Fixed{T: now}
	baseQueue := queue.NewPG(pool, clk)
	retryingQueue := &failFirstReceiptEnqueue{Queue: baseQueue}
	replies := notify.NewEnqueuer(st, retryingQueue, clk)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(st, pool, retryingQueue, nil, replies, nil, nil, clk, log, config.Config{
		PerUserDailyReceiptLimit: 20,
	})

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := st.CreateIdentity(ctx, userID, domain.ProviderZaloBot, "sender-retry", zaloScope); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if err := st.SetConsent(ctx, userID, consentVersion, now); err != nil {
		t.Fatalf("activate user: %v", err)
	}

	ev := events.InboundEvent{
		Provider:          string(domain.ProviderZaloBot),
		ProviderUserID:    "sender-retry",
		ProviderChatID:    "chat-retry",
		ProviderMessageID: "provider-msg-retry",
		EventType:         domain.EventImageReceived,
		Media: []events.MediaReference{{
			Provider: string(domain.ProviderZaloBot),
			URL:      "MOCK-FIXTURE:coopmart-clean",
			MimeType: "image/jpeg",
		}},
		ReceivedAt:     now,
		RawPayloadHash: "stable-payload-hash",
		RawPayload:     []byte(`{"event":"image"}`),
	}

	if err := handler.HandleEvent(ctx, ev); err == nil {
		t.Fatal("first attempt error = nil; want injected enqueue failure")
	}
	if err := handler.HandleEvent(ctx, ev); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := handler.HandleEvent(ctx, ev); err != nil {
		t.Fatalf("completed duplicate: %v", err)
	}

	var messageCount, receiptCount, receiptUsage, receiptJobs, outboundRows int
	var status string
	var owner *uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_messages
		WHERE provider_chat_id = $1 AND provider_message_id = $2`,
		ev.ProviderChatID, ev.ProviderMessageID).Scan(&messageCount); err != nil {
		t.Fatalf("inspect provider message: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT status, user_id FROM provider_messages
		WHERE provider_chat_id = $1 AND provider_message_id = $2`,
		ev.ProviderChatID, ev.ProviderMessageID).Scan(&status, &owner); err != nil {
		t.Fatalf("inspect provider message state: %v", err)
	}
	if messageCount != 1 || status != string(domain.MessageProcessed) || owner == nil || *owner != userID {
		t.Fatalf("provider message count/status/owner = %d/%s/%v", messageCount, status, owner)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM receipt_documents WHERE user_id = $1`, userID).Scan(&receiptCount); err != nil {
		t.Fatalf("inspect receipts: %v", err)
	}
	if receiptCount != 1 {
		t.Fatalf("receipt count = %d, want 1", receiptCount)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count FROM usage_counters
		WHERE scope = 'user' AND scope_id = $1 AND period = $2 AND metric = 'receipts'`,
		userID.String(), now.Format("2006-01-02")).Scan(&receiptUsage); err != nil {
		t.Fatalf("inspect receipt usage: %v", err)
	}
	if receiptUsage != 1 {
		t.Fatalf("receipt usage = %d, want 1", receiptUsage)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM queue_jobs WHERE kind = $1`, string(domain.JobReceiptProcess)).Scan(&receiptJobs); err != nil {
		t.Fatalf("inspect receipt jobs: %v", err)
	}
	if receiptJobs != 1 {
		t.Fatalf("receipt jobs = %d, want 1", receiptJobs)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbound_messages WHERE user_id = $1`, userID).Scan(&outboundRows); err != nil {
		t.Fatalf("inspect outbound rows: %v", err)
	}
	if outboundRows != 1 {
		t.Fatalf("outbound rows = %d, want 1", outboundRows)
	}
}

func TestDeletionConfirmationReplayDoesNotRecreateAccount(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	now := time.Now().UTC()
	clk := clock.Fixed{T: now}
	q := queue.NewPG(pool, clk)
	replies := notify.NewEnqueuer(st, q, clk)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(st, pool, q, nil, replies, nil, nil, clk, log, config.Config{
		AppEnv:  config.EnvDevelopment,
		DataDir: t.TempDir(),
	})

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIdentity(ctx, userID, domain.ProviderZaloBot, "sender-delete", zaloScope); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, consentVersion, now); err != nil {
		t.Fatal(err)
	}
	event := func(messageID, text string) events.InboundEvent {
		return events.InboundEvent{
			Provider:          string(domain.ProviderZaloBot),
			ProviderUserID:    "sender-delete",
			ProviderChatID:    "chat-delete",
			ProviderMessageID: messageID,
			EventType:         domain.EventTextReceived,
			Text:              text,
			ReceivedAt:        now,
			RawPayloadHash:    "hash-" + messageID,
			RawPayload:        []byte(`{"provider_user_id":"sender-delete"}`),
		}
	}
	if err := handler.HandleEvent(ctx, event("delete-request", "/xoadulieu")); err != nil {
		t.Fatal(err)
	}
	confirm := event("delete-confirm", "xác nhận")
	if err := handler.HandleEvent(ctx, confirm); err != nil {
		t.Fatal(err)
	}
	if err := handler.HandleEvent(ctx, confirm); err != nil {
		t.Fatalf("replayed deletion confirmation: %v", err)
	}

	var users, identities, tombstones int
	var status string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(status) FROM users`).Scan(&users, &status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_identities`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM provider_messages
		WHERE provider_chat_id='chat-delete' AND provider_message_id='delete-confirm'
		  AND user_id IS NULL AND event_type='account.deleted'
		  AND raw_payload_json='{}' AND delete_after IS NOT NULL`).Scan(&tombstones); err != nil {
		t.Fatal(err)
	}
	if users != 1 || status != string(domain.UserDeleted) || identities != 0 || tombstones != 1 {
		t.Fatalf("post-delete users/status/identities/tombstones = %d/%s/%d/%d",
			users, status, identities, tombstones)
	}
}

func TestSettingsUpdateProfileScheduleAndManualCurrency(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 6, 30, 0, 0, time.UTC)
	clk := clock.Fixed{T: now}
	q := queue.NewPG(pool, clk)
	replies := notify.NewEnqueuer(st, q, clk)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := NewHandler(st, pool, q, nil, replies,
		categorisation.NewService(st, clk), insight.NewService(pool, clk), clk, log,
		config.Config{AppEnv: config.EnvDevelopment})

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIdentity(ctx, userID, domain.ProviderZaloBot, "sender-settings", zaloScope); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, consentVersion, now); err != nil {
		t.Fatal(err)
	}
	event := func(id, text string) events.InboundEvent {
		return events.InboundEvent{
			Provider: string(domain.ProviderZaloBot), ProviderUserID: "sender-settings",
			ProviderChatID: "chat-settings", ProviderMessageID: id,
			EventType: domain.EventTextReceived, Text: text, ReceivedAt: now,
			RawPayloadHash: "hash-" + id, RawPayload: []byte(`{"text":"settings test"}`),
		}
	}
	for _, message := range []struct{ id, text string }{
		{"schedule", "/tongket ngay 20:00"},
		{"timezone", "/caidat muigio UTC"},
		{"currency", "/caidat tiente USD"},
		{"manual", "50 lunch"},
	} {
		if err := handler.HandleEvent(ctx, event(message.id, message.text)); err != nil {
			t.Fatalf("handle %s: %v", message.id, err)
		}
	}

	user, err := st.GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.Timezone != "UTC" || user.DefaultCurrency != "USD" {
		t.Fatalf("settings = %q/%q, want UTC/USD", user.Timezone, user.DefaultCurrency)
	}
	preferences, err := st.ListSummarySchedules(ctx, userID)
	if err != nil || len(preferences) != 1 {
		t.Fatalf("preferences = %v, %v", preferences, err)
	}
	wantNext, err := summaryschedule.NextDelivery(now, time.UTC, domain.SummaryDaily, 20*60)
	if err != nil {
		t.Fatal(err)
	}
	if !preferences[0].NextDeliveryAt.Equal(wantNext) {
		t.Fatalf("next delivery = %s, want %s", preferences[0].NextDeliveryAt, wantNext)
	}

	var amount int64
	var currency string
	if err := pool.QueryRow(ctx, `
		SELECT amount_minor, currency FROM transactions
		WHERE user_id = $1 AND source = 'manual'`, userID).Scan(&amount, &currency); err != nil {
		t.Fatal(err)
	}
	if amount != 5000 || currency != "USD" {
		t.Fatalf("manual amount = %d %s, want 5000 USD", amount, currency)
	}
	var settingsReplies int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbound_messages
		WHERE user_id = $1 AND body LIKE '%Cài đặt của bạn%'`, userID).Scan(&settingsReplies); err != nil {
		t.Fatal(err)
	}
	if settingsReplies != 2 {
		t.Fatalf("settings replies = %d, want 2 refreshed views", settingsReplies)
	}
}
