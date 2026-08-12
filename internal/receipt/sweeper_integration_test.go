//go:build integration

package receipt_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/logging"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/receipt"
	"zl-expese-bot/internal/store"
)

// TestRetentionSweep exercises the full sweep: expired originals are
// removed from the object store and marked deleted, fresh receipts are
// untouched, and a replayed job is a no-op.
func TestRetentionSweep(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	clk := clock.Real{}
	q := queue.NewPG(pool, clk)
	objects, err := objectstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("object store: %v", err)
	}
	sweeper := receipt.NewSweeper(st, objects, q, clk, logging.New("error"))
	ctx := context.Background()

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatalf("create user: %v", err)
	}

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)

	// Expired receipt with a stored object.
	expired := uuid.New()
	if err := st.CreateReceipt(ctx, &domain.ReceiptDocument{
		ID: expired, UserID: userID, RetentionPolicy: "originals_30d", DeleteAfter: &past,
	}); err != nil {
		t.Fatalf("create expired receipt: %v", err)
	}
	key := receipt.StorageKey(userID, expired)
	if _, err := objects.Put(ctx, key, strings.NewReader("fake-image"), "image/jpeg"); err != nil {
		t.Fatalf("put object: %v", err)
	}
	if err := st.SetReceiptStored(ctx, expired, key, "sha", "image/jpeg", 10); err != nil {
		t.Fatalf("set stored: %v", err)
	}

	// Expired receipt without an object (never stored).
	expiredNoObj := uuid.New()
	if err := st.CreateReceipt(ctx, &domain.ReceiptDocument{
		ID: expiredNoObj, UserID: userID, RetentionPolicy: "originals_30d", DeleteAfter: &past,
	}); err != nil {
		t.Fatalf("create objectless receipt: %v", err)
	}

	// Fresh receipt: must survive the sweep.
	fresh := uuid.New()
	if err := st.CreateReceipt(ctx, &domain.ReceiptDocument{
		ID: fresh, UserID: userID, RetentionPolicy: "originals_30d", DeleteAfter: &future,
	}); err != nil {
		t.Fatalf("create fresh receipt: %v", err)
	}
	expiredTombstone, freshTombstone := uuid.New(), uuid.New()
	for _, tombstone := range []struct {
		id          uuid.UUID
		messageID   string
		deleteAfter time.Time
	}{
		{expiredTombstone, "expired-tombstone", past},
		{freshTombstone, "fresh-tombstone", future},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO provider_messages
				(id, provider, provider_chat_id, provider_message_id, event_type,
				 payload_hash, raw_payload_json, received_at, processed_at, delete_after, status)
			VALUES ($1, 'zalo_bot', 'deleted-chat', $2, 'account.deleted',
			        '', '{}', now(), now(), $3, 'processed')`,
			tombstone.id, tombstone.messageID, tombstone.deleteAfter); err != nil {
			t.Fatalf("create provider tombstone: %v", err)
		}
	}

	payload, _ := json.Marshal(events.RetentionSweepJob{SchemaVersion: events.SchemaV1, EnqueuedAt: clk.Now()})
	job := &domain.QueueJob{ID: uuid.New(), Kind: domain.JobRetentionSweep, Payload: payload}

	for run := 1; run <= 2; run++ {
		if err := sweeper.Handle(ctx, job); err != nil {
			t.Fatalf("sweep run %d: %v", run, err)
		}
	}

	got, err := st.GetReceipt(ctx, expired)
	if err != nil {
		t.Fatalf("load expired receipt: %v", err)
	}
	if got.Status != domain.ReceiptDeleted {
		t.Errorf("expired receipt status = %s, want deleted", got.Status)
	}
	if _, err := objects.Open(ctx, key); err != objectstore.ErrNotFound {
		t.Errorf("expired object still present: %v", err)
	}

	got, err = st.GetReceipt(ctx, expiredNoObj)
	if err != nil {
		t.Fatalf("load objectless receipt: %v", err)
	}
	if got.Status != domain.ReceiptDeleted {
		t.Errorf("objectless expired receipt status = %s, want deleted", got.Status)
	}

	got, err = st.GetReceipt(ctx, fresh)
	if err != nil {
		t.Fatalf("load fresh receipt: %v", err)
	}
	if got.Status == domain.ReceiptDeleted {
		t.Errorf("fresh receipt was deleted; retention deadline must be respected")
	}
	var expiredCount, freshCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_messages WHERE id = $1`, expiredTombstone).Scan(&expiredCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_messages WHERE id = $1`, freshTombstone).Scan(&freshCount); err != nil {
		t.Fatal(err)
	}
	if expiredCount != 0 || freshCount != 1 {
		t.Errorf("provider tombstones expired/fresh = %d/%d, want 0/1", expiredCount, freshCount)
	}
}
