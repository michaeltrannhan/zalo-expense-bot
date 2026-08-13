//go:build integration

package receipt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/categorisation"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/mock"
	"zl-expese-bot/internal/messaging/logprovider"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/receipt"
	"zl-expese-bot/internal/store"
)

func TestProcessorResumesFromMidPipelineStatuses(t *testing.T) {
	for _, status := range []domain.ReceiptStatus{
		domain.ReceiptQueued,
		domain.ReceiptDownloading,
		domain.ReceiptStored,
		domain.ReceiptExtracting,
	} {
		t.Run(string(status), func(t *testing.T) {
			runProcessorResume(t, status)
		})
	}
}

func runProcessorResume(t *testing.T, start domain.ReceiptStatus) {
	t.Helper()
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	clk := clock.Real{}
	ctx := context.Background()
	objects, err := objectstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := queue.NewPG(pool, clk)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	replies := notify.NewEnqueuer(st, q, clk)
	proc := receipt.NewProcessor(st, objects, mock.New(), logprovider.New(log),
		categorisation.NewService(st, clk), replies, clk, log)

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	fixture := mock.MockPrefix + "coopmart-clean"
	ev := events.InboundEvent{
		Provider:          string(domain.ProviderZaloBot),
		ProviderUserID:    "sender-resume",
		ProviderChatID:    "chat-resume",
		ProviderMessageID: "msg-" + string(start),
		EventType:         domain.EventImageReceived,
		Media:             []events.MediaReference{{Provider: string(domain.ProviderZaloBot), URL: fixture}},
		ReceivedAt:        clk.Now(),
	}
	raw, _ := json.Marshal(ev)
	pm := &domain.ProviderMessage{
		ID: uuid.New(), Provider: domain.ProviderZaloBot,
		ProviderChatID: ev.ProviderChatID, ProviderMessageID: ev.ProviderMessageID,
		EventType: ev.EventType, PayloadHash: "h-" + string(start), RawPayload: raw,
		ReceivedAt: ev.ReceivedAt, UserID: &userID,
	}
	if _, err := st.ClaimProviderMessage(ctx, pm, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := st.SetProviderMessageUser(ctx, pm.ID, userID); err != nil {
		t.Fatal(err)
	}

	r := &domain.ReceiptDocument{
		ID: uuid.New(), UserID: userID, ProviderMessageID: &pm.ID,
		Status: domain.ReceiptReceived, RetentionPolicy: "originals_30d",
	}
	if err := st.CreateReceipt(ctx, r); err != nil {
		t.Fatal(err)
	}
	advance := []struct{ from, to domain.ReceiptStatus }{
		{domain.ReceiptReceived, domain.ReceiptQueued},
		{domain.ReceiptQueued, domain.ReceiptDownloading},
		{domain.ReceiptDownloading, domain.ReceiptStored},
		{domain.ReceiptStored, domain.ReceiptExtracting},
	}
	cur := domain.ReceiptReceived
	for _, step := range advance {
		if cur == start {
			break
		}
		if err := st.TransitionReceipt(ctx, r.ID, step.from, step.to); err != nil {
			t.Fatalf("advance %s -> %s: %v", step.from, step.to, err)
		}
		cur = step.to
	}
	if start == domain.ReceiptStored || start == domain.ReceiptExtracting {
		key := receipt.StorageKey(userID, r.ID)
		stored, err := objects.Put(ctx, key, bytes.NewReader([]byte(fixture)), "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetReceiptStored(ctx, r.ID, stored.Key, stored.SHA256, stored.ContentType, stored.ByteSize); err != nil {
			t.Fatal(err)
		}
	}

	jobPayload, _ := json.Marshal(events.ReceiptJob{
		SchemaVersion: events.SchemaV1, JobID: uuid.New(), ReceiptID: r.ID, UserID: userID,
		Provider: ev.Provider, ProviderChatID: ev.ProviderChatID, ProviderMessageID: ev.ProviderMessageID,
		Attempt: 1, EnqueuedAt: clk.Now(),
	})
	job := &domain.QueueJob{ID: uuid.New(), Kind: domain.JobReceiptProcess, Payload: jobPayload, Attempts: 1}
	if err := proc.Handle(ctx, job); err != nil {
		t.Fatalf("Handle from %s: %v", start, err)
	}
	got, err := st.GetReceipt(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.ReceiptReviewRequired {
		t.Fatalf("status after resume from %s = %s, want review_required", start, got.Status)
	}
	tx, err := st.GetActiveTransactionByReceipt(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status != domain.TxAwaitingConfirmation {
		t.Fatalf("tx status = %s", tx.Status)
	}
}
