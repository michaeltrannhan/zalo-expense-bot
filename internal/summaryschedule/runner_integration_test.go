//go:build integration

package summaryschedule

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/insight"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/store"
)

func TestRunnerEnqueuesAndAdvancesDueDailySummary(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	now, _ := time.Parse(time.RFC3339, "2026-07-23T20:05:00+07:00")
	clk := clock.Fixed{T: now.UTC()}

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, "consent-v1", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertSummarySchedule(ctx, &domain.SummarySchedule{
		ID:             uuid.New(),
		UserID:         userID,
		Frequency:      domain.SummaryDaily,
		DeliveryMinute: 20 * 60,
		Provider:       domain.ProviderZaloBot,
		ProviderChatID: "private-chat",
		Enabled:        true,
		NextDeliveryAt: now.Add(-5 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	occurredAt, _ := time.Parse(time.RFC3339, "2026-07-22T12:00:00+07:00")
	if _, err := pool.Exec(ctx, `
		INSERT INTO transactions
			(id, user_id, type, merchant_name, amount_minor, currency,
			 occurred_at, status, source, confirmed_at)
		VALUES ($1, $2, 'expense', 'Co.opmart', 325000, 'VND',
		        $3, 'confirmed', 'manual', $3)`,
		uuid.New(), userID, occurredAt.UTC()); err != nil {
		t.Fatal(err)
	}

	q := queue.NewPG(pool, clk)
	replies := notify.NewEnqueuer(st, q, clk)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := NewRunner(st, insight.NewService(st), replies, clk, log)
	processed, err := runner.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 {
		t.Fatalf("processed = %d, want 1", processed)
	}

	var body, status string
	if err := pool.QueryRow(ctx, `
		SELECT body, status FROM outbound_messages WHERE user_id = $1`, userID).
		Scan(&body, &status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || !strings.Contains(body, "Hôm qua") || !strings.Contains(body, "325.000 ₫") {
		t.Fatalf("unexpected outbound status/body: %q %q", status, body)
	}
	var jobs, insights int
	if err := pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*)::int FROM queue_jobs WHERE kind = 'outbound_send' AND status = 'queued'),
		  (SELECT COUNT(*)::int FROM insights WHERE user_id = $1 AND insight_type = 'daily')`,
		userID).Scan(&jobs, &insights); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || insights != 1 {
		t.Fatalf("jobs/insights = %d/%d, want 1/1", jobs, insights)
	}
	preferences, err := st.ListSummarySchedules(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(preferences) != 1 || !preferences[0].NextDeliveryAt.After(now) || preferences[0].LastDeliveredAt == nil {
		t.Fatalf("preference was not advanced: %+v", preferences)
	}

	processed, err = runner.RunOnce(ctx)
	if err != nil || processed != 0 {
		t.Fatalf("second pass = %d, %v; want 0, nil", processed, err)
	}
}
