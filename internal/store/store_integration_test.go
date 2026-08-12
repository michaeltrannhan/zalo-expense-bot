//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/store"
)

type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func providerMessage(id uuid.UUID, messageID, hash string) *domain.ProviderMessage {
	return &domain.ProviderMessage{
		ID:                id,
		Provider:          domain.ProviderZaloBot,
		ProviderChatID:    "chat-claims",
		ProviderMessageID: messageID,
		EventType:         domain.EventTextReceived,
		PayloadHash:       hash,
		RawPayload:        []byte(`{"text":"hello"}`),
		ReceivedAt:        time.Now().UTC(),
	}
}

func TestUpdateUserPreferences(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateUserPreferences(ctx, userID, "UTC", "usd"); err != nil {
		t.Fatalf("update preferences: %v", err)
	}
	user, err := st.GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if user.Timezone != "UTC" || user.DefaultCurrency != "USD" {
		t.Fatalf("preferences = %q/%q, want UTC/USD", user.Timezone, user.DefaultCurrency)
	}
	if err := st.UpdateUserPreferences(ctx, userID, "", "VND"); !domain.IsCode(err, domain.CodeValidation) {
		t.Fatalf("empty timezone error = %v, want validation", err)
	}
}

func TestProviderMessageClaimLifecycle(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	const lease = 2 * time.Minute

	originalID := uuid.New()
	first := providerMessage(originalID, "msg-lifecycle", "hash-1")
	claimed, err := st.ClaimProviderMessage(ctx, first, lease)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v; want true, nil", claimed, err)
	}

	concurrent := providerMessage(uuid.New(), "msg-lifecycle", "hash-1")
	claimed, err = st.ClaimProviderMessage(ctx, concurrent, lease)
	if claimed || !domain.IsCode(err, domain.CodeConflict) {
		t.Fatalf("concurrent claim = %v, %v; want conflict", claimed, err)
	}
	if concurrent.ID != originalID {
		t.Fatalf("concurrent durable ID = %s, want %s", concurrent.ID, originalID)
	}

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.SetProviderMessageUser(ctx, originalID, userID); err != nil {
		t.Fatalf("set provider message user: %v", err)
	}
	if err := st.MarkProviderMessage(ctx, originalID, domain.MessageProcessed); err != nil {
		t.Fatalf("mark processed: %v", err)
	}

	completed := providerMessage(uuid.New(), "msg-lifecycle", "hash-1")
	claimed, err = st.ClaimProviderMessage(ctx, completed, lease)
	if err != nil || claimed {
		t.Fatalf("completed duplicate = %v, %v; want false, nil", claimed, err)
	}
	if completed.ID != originalID {
		t.Fatalf("completed durable ID = %s, want %s", completed.ID, originalID)
	}
	stored, err := st.GetProviderMessage(ctx, originalID)
	if err != nil {
		t.Fatalf("get provider message: %v", err)
	}
	if stored.UserID == nil || *stored.UserID != userID {
		t.Fatalf("stored user = %v, want %s", stored.UserID, userID)
	}
	if stored.ProcessingStartedAt != nil {
		t.Fatalf("completed processing_started_at = %v, want nil", stored.ProcessingStartedAt)
	}

	changed := providerMessage(uuid.New(), "msg-lifecycle", "different-hash")
	if claimed, err := st.ClaimProviderMessage(ctx, changed, lease); claimed || !domain.IsCode(err, domain.CodeValidation) {
		t.Fatalf("changed payload claim = %v, %v; want validation error", claimed, err)
	}
}

func TestProviderMessageFailedAndStaleClaimsCanRetry(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	const lease = time.Minute

	failedID := uuid.New()
	if claimed, err := st.ClaimProviderMessage(ctx, providerMessage(failedID, "msg-failed", "hash-f"), lease); err != nil || !claimed {
		t.Fatalf("claim failed fixture: %v, %v", claimed, err)
	}
	if err := st.MarkProviderMessage(ctx, failedID, domain.MessageFailed); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	failedRetry := providerMessage(uuid.New(), "msg-failed", "hash-f")
	if claimed, err := st.ClaimProviderMessage(ctx, failedRetry, lease); err != nil || !claimed {
		t.Fatalf("failed retry = %v, %v; want claimed", claimed, err)
	}
	if failedRetry.ID != failedID {
		t.Fatalf("failed retry ID = %s, want %s", failedRetry.ID, failedID)
	}

	staleID := uuid.New()
	if claimed, err := st.ClaimProviderMessage(ctx, providerMessage(staleID, "msg-stale", "hash-s"), lease); err != nil || !claimed {
		t.Fatalf("claim stale fixture: %v, %v", claimed, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE provider_messages SET processing_started_at = now() - interval '10 minutes' WHERE id = $1`,
		staleID); err != nil {
		t.Fatalf("age claim: %v", err)
	}
	staleRetry := providerMessage(uuid.New(), "msg-stale", "hash-s")
	if claimed, err := st.ClaimProviderMessage(ctx, staleRetry, lease); err != nil || !claimed {
		t.Fatalf("stale retry = %v, %v; want claimed", claimed, err)
	}
	if staleRetry.ID != staleID {
		t.Fatalf("stale retry ID = %s, want %s", staleRetry.ID, staleID)
	}
}

func TestReceiptRegistrationIsIdempotentPerProviderMessage(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	pmID := uuid.New()
	if claimed, err := st.ClaimProviderMessage(ctx, providerMessage(pmID, "msg-image", "hash-image"), time.Minute); err != nil || !claimed {
		t.Fatalf("claim provider message: %v, %v", claimed, err)
	}

	firstID := uuid.New()
	first, created, err := st.GetOrCreateReceiptForProviderMessage(ctx, &domain.ReceiptDocument{
		ID: firstID, UserID: userID, ProviderMessageID: &pmID,
	})
	if err != nil || !created {
		t.Fatalf("first receipt = %v, %v; want created", created, err)
	}
	replayed, created, err := st.GetOrCreateReceiptForProviderMessage(ctx, &domain.ReceiptDocument{
		ID: uuid.New(), UserID: userID, ProviderMessageID: &pmID,
	})
	if err != nil || created {
		t.Fatalf("replayed receipt = %v, %v; want existing", created, err)
	}
	if replayed.ID != first.ID || replayed.ID != firstID {
		t.Fatalf("replayed receipt ID = %s, want %s", replayed.ID, firstID)
	}
}

func TestRecordAttemptUpdatesLifecycle(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	receiptID := uuid.New()
	if err := st.CreateReceipt(ctx, &domain.ReceiptDocument{ID: receiptID, UserID: userID}); err != nil {
		t.Fatalf("create receipt: %v", err)
	}
	if err := st.RecordAttempt(ctx, receiptID, "gemini-vision", "gemini-test", 1, "started", "", ""); err != nil {
		t.Fatalf("record started: %v", err)
	}
	if err := st.RecordAttempt(ctx, receiptID, "gemini-vision", "gemini-test", 1, "succeeded", "", ""); err != nil {
		t.Fatalf("record succeeded: %v", err)
	}

	var status, errClass, errCode string
	var completedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT status, completed_at, error_class, error_code
		FROM receipt_processing_attempts
		WHERE receipt_id = $1 AND processor_name = 'gemini-vision'
		  AND processor_version = 'gemini-test' AND attempt_number = 1`, receiptID).
		Scan(&status, &completedAt, &errClass, &errCode); err != nil {
		t.Fatalf("query attempt: %v", err)
	}
	if status != "succeeded" || completedAt == nil || errClass != "" || errCode != "" {
		t.Fatalf("attempt = status %q completed %v class %q code %q", status, completedAt, errClass, errCode)
	}

	// A stale replay cannot reset or rewrite terminal evidence.
	if err := st.RecordAttempt(ctx, receiptID, "gemini-vision", "gemini-test", 1, "started", "", ""); err != nil {
		t.Fatalf("replay started: %v", err)
	}
	if err := st.RecordAttempt(ctx, receiptID, "gemini-vision", "gemini-test", 1, "failed", "transient", "timeout"); err != nil {
		t.Fatalf("replay conflicting terminal: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT status, completed_at, error_class, error_code
		FROM receipt_processing_attempts
		WHERE receipt_id = $1 AND processor_name = 'gemini-vision'
		  AND processor_version = 'gemini-test' AND attempt_number = 1`, receiptID).
		Scan(&status, &completedAt, &errClass, &errCode); err != nil {
		t.Fatalf("query replayed attempt: %v", err)
	}
	if status != "succeeded" || completedAt == nil || errClass != "" || errCode != "" {
		t.Fatalf("replayed attempt = status %q completed %v class %q code %q", status, completedAt, errClass, errCode)
	}
}

// seedTx inserts one transaction row directly.
func seedTx(t *testing.T, pool execer, userID uuid.UUID, txType domain.TxType, merchantID *uuid.UUID, merchantName string, amount int64, currency string, occurredAt time.Time, status domain.TxStatus) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO transactions (id, user_id, type, merchant_id, merchant_name,
		                          amount_minor, currency, occurred_at, status, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'receipt')`,
		id, userID, string(txType), merchantID, merchantName, amount, currency, occurredAt, string(status))
	if err != nil {
		t.Fatalf("seed transaction: %v", err)
	}
	return id
}

// TestFindPotentialDuplicates verifies the soft-duplicate signal (P4-C01):
// same user/amount/currency/merchant within ±window, confirmed rows only.
func TestFindPotentialDuplicates(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()

	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	otherID := uuid.New()
	if err := st.CreateUser(ctx, otherID); err != nil {
		t.Fatalf("create other user: %v", err)
	}
	merchant, err := st.CreateMerchant(ctx, "Co.opmart", "coopmart", "supermarket")
	if err != nil {
		t.Fatalf("create merchant: %v", err)
	}

	ref := time.Date(2026, 7, 15, 9, 24, 0, 0, time.UTC)
	window := 72 * time.Hour

	// In-scope: same everything, inside window.
	match := seedTx(t, pool, userID, domain.TxExpense, &merchant.ID, "Co.opmart", 325000, "VND", ref.Add(-time.Hour), domain.TxConfirmed)
	// Excluded variants.
	seedTx(t, pool, userID, domain.TxExpense, &merchant.ID, "Co.opmart", 325001, "VND", ref, domain.TxConfirmed)                     // amount differs
	seedTx(t, pool, userID, domain.TxExpense, &merchant.ID, "Co.opmart", 325000, "AUD", ref, domain.TxConfirmed)                     // currency differs
	seedTx(t, pool, userID, domain.TxExpense, &merchant.ID, "Co.opmart", 325000, "VND", ref.Add(4*24*time.Hour), domain.TxConfirmed) // outside window
	seedTx(t, pool, userID, domain.TxExpense, nil, "Grab", 325000, "VND", ref, domain.TxConfirmed)                                   // other merchant
	seedTx(t, pool, otherID, domain.TxExpense, &merchant.ID, "Co.opmart", 325000, "VND", ref, domain.TxConfirmed)                    // other user
	seedTx(t, pool, userID, domain.TxExpense, &merchant.ID, "Co.opmart", 325000, "VND", ref, domain.TxAwaitingConfirmation)          // not recorded yet
	deletedID := seedTx(t, pool, userID, domain.TxExpense, &merchant.ID, "Co.opmart", 325000, "VND", ref, domain.TxConfirmed)

	// Soft-delete the last one directly.
	if _, err := pool.Exec(ctx, `UPDATE transactions SET deleted_at = now() WHERE id = $1`, deletedID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	dups, err := st.FindPotentialDuplicates(ctx, userID, 325000, "VND", ref, window, &merchant.ID, "Co.opmart")
	if err != nil {
		t.Fatalf("FindPotentialDuplicates: %v", err)
	}
	if len(dups) != 1 || dups[0].ID != match {
		t.Fatalf("merchant-ID match: got %d rows, want exactly %s", len(dups), match)
	}

	// Merchant-name fallback: nil merchant ID matches case-insensitively.
	dups, err = st.FindPotentialDuplicates(ctx, userID, 325000, "VND", ref, window, nil, "co.opmart")
	if err != nil {
		t.Fatalf("FindPotentialDuplicates by name: %v", err)
	}
	if len(dups) != 1 || dups[0].ID != match {
		t.Fatalf("merchant-name match: got %d rows, want exactly %s", len(dups), match)
	}

	// No match when nothing qualifies.
	dups, err = st.FindPotentialDuplicates(ctx, userID, 999999, "VND", ref, window, &merchant.ID, "Co.opmart")
	if err != nil {
		t.Fatalf("FindPotentialDuplicates empty: %v", err)
	}
	if len(dups) != 0 {
		t.Fatalf("want no duplicates, got %d", len(dups))
	}
}
