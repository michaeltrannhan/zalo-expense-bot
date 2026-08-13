//go:build integration

package account

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/postgres"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/store"
)

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v\n%s", err, sql)
	}
}

func TestDeleteAccountPurgesCompleteUserGraph(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID, otherID := uuid.New(), uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIdentity(ctx, userID, domain.ProviderZaloBot, "delete-subject", "zalo_bot"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIdentity(ctx, otherID, domain.ProviderZaloBot, "other-subject", "zalo_bot"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConsent(ctx, userID, "consent-v1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	objects, err := objectstore.NewLocal(filepath.Join(dataDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	exportDir := filepath.Join(dataDir, "exports", userID.String())
	if err := os.MkdirAll(exportDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exportDir, "transactions.csv"), []byte("private export"), 0o600); err != nil {
		t.Fatal(err)
	}
	stored, err := objects.Put(ctx, "receipts/"+userID.String()+"/receipt.jpg",
		strings.NewReader("receipt bytes"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}

	pmID, legacyPMID, receiptID, txID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	merchantID := uuid.New()
	var categoryID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM categories ORDER BY system_key LIMIT 1`).Scan(&categoryID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `
		INSERT INTO provider_messages
			(id, provider, provider_chat_id, provider_message_id, user_id, event_type,
			 payload_hash, raw_payload_json, received_at, status)
		VALUES ($1, 'zalo_bot', 'chat-delete', 'pm-delete', $2, 'message.image.received',
		        'hash', '{"message":{"from":{"id":"delete-subject"}}}', now(), 'processed')`,
		pmID, userID)
	mustExec(t, pool, `
		INSERT INTO provider_messages
			(id, provider, provider_chat_id, provider_message_id, event_type,
			 payload_hash, raw_payload_json, received_at, status)
		VALUES ($1, 'zalo_bot', 'legacy-chat', 'legacy-pm', 'message.text.received',
		        'legacy-hash', '{"message":{"from":{"id":"delete-subject"}}}', now(), 'processed')`,
		legacyPMID)
	mustExec(t, pool, `
		INSERT INTO receipt_documents
			(id, user_id, provider_message_id, storage_key, status)
		VALUES ($1, $2, $3, $4, 'stored')`,
		receiptID, userID, pmID, stored.Key)
	mustExec(t, pool, `
		INSERT INTO receipt_processing_attempts
			(receipt_id, processor_name, processor_version, attempt_number, status, completed_at)
		VALUES ($1, 'mock', '1', 1, 'succeeded', now())`, receiptID)
	mustExec(t, pool, `
		INSERT INTO extracted_fields
			(id, receipt_document_id, field_name, raw_value, confidence)
		VALUES ($1, $2, 'merchant', 'Private Shop', 0.9)`, uuid.New(), receiptID)
	mustExec(t, pool, `
		INSERT INTO merchants (id, canonical_name, normalised_name)
		VALUES ($1, 'Private Shop', $2)`, merchantID, "private-shop-"+userID.String())
	mustExec(t, pool, `
		INSERT INTO transactions
			(id, user_id, receipt_document_id, type, merchant_id, merchant_name,
			 amount_minor, currency, occurred_at, status, source)
		VALUES ($1, $2, $3, 'expense', $4, 'Private Shop',
		        10000, 'VND', now(), 'confirmed', 'receipt')`,
		txID, userID, receiptID, merchantID)
	mustExec(t, pool, `
		INSERT INTO predictions
			(id, transaction_id, prediction_type, predicted_value, confidence, model_name, model_version)
		VALUES ($1, $2, 'category', 'food', 0.8, 'test', '1')`, uuid.New(), txID)
	mustExec(t, pool, `
		INSERT INTO corrections
			(id, transaction_id, field_name, predicted_value, corrected_value)
		VALUES ($1, $2, 'merchant', 'Shop', 'Private Shop')`, uuid.New(), txID)
	mustExec(t, pool, `
		INSERT INTO user_merchant_rules
			(user_id, merchant_id, preferred_category_id, sample_count, confidence)
		VALUES ($1, $2, $3, 1, 1)`, userID, merchantID, categoryID)
	mustExec(t, pool, `
		INSERT INTO insights
			(id, user_id, insight_type, period_start, period_end, payload_json, generator_version)
		VALUES ($1, $2, 'daily', now() - interval '1 day', now(), '{}', 'test')`,
		uuid.New(), userID)
	mustExec(t, pool, `
		INSERT INTO usage_counters (scope, scope_id, period, metric, count)
		VALUES ('user', $1, '2026-07', 'receipts', 1),
		       ('global', '', '2026-07', 'ocr_pages', 1)`, userID.String())
	mustExec(t, pool, `
		INSERT INTO outbound_messages
			(id, user_id, provider, provider_chat_id, idempotency_key, body, status)
		VALUES ($1, $2, 'zalo_bot', 'chat-delete', $3, 'private reply', 'sent')`,
		uuid.New(), userID, "sent-"+userID.String())
	mustExec(t, pool, `
		INSERT INTO pending_actions (id, user_id, kind, expires_at)
		VALUES ($1, $2, 'delete_account', now() + interval '15 minutes')`,
		uuid.New(), userID)
	mustExec(t, pool, `
		INSERT INTO scheduled_summary_preferences
			(id, user_id, frequency, delivery_minute, provider, provider_chat_id, next_delivery_at)
		VALUES ($1, $2, 'daily', 1200, 'zalo_bot', 'chat-delete', now())`,
		uuid.New(), userID)
	mustExec(t, pool, `
		INSERT INTO queue_jobs (id, kind, payload_json, dedupe_key)
		VALUES ($1, 'receipt_process', $2, $3)`,
		uuid.New(), []byte(`{"user_id":"`+userID.String()+`"}`), "delete-job-"+userID.String())

	report, err := DeleteAccount(ctx, pool, objects, dataDir, userID, pmID)
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if report.ReceiptsDeleted != 1 || report.TransactionsDeleted != 1 || report.FilesRemoved != 1 ||
		report.ExportsRemoved != 1 || report.JobsPurged != 1 {
		t.Fatalf("unexpected report: %+v", report)
	}

	for _, check := range []struct {
		name string
		sql  string
		arg  any
	}{
		{"identities", `SELECT count(*) FROM user_identities WHERE user_id = $1`, userID},
		{"receipts", `SELECT count(*) FROM receipt_documents WHERE id = $1`, receiptID},
		{"attempts", `SELECT count(*) FROM receipt_processing_attempts WHERE receipt_id = $1`, receiptID},
		{"extracted fields", `SELECT count(*) FROM extracted_fields WHERE receipt_document_id = $1`, receiptID},
		{"transactions", `SELECT count(*) FROM transactions WHERE id = $1`, txID},
		{"predictions", `SELECT count(*) FROM predictions WHERE transaction_id = $1`, txID},
		{"corrections", `SELECT count(*) FROM corrections WHERE transaction_id = $1`, txID},
		{"merchant rules", `SELECT count(*) FROM user_merchant_rules WHERE user_id = $1`, userID},
		{"insights", `SELECT count(*) FROM insights WHERE user_id = $1`, userID},
		{"usage", `SELECT count(*) FROM usage_counters WHERE scope = 'user' AND scope_id = $1`, userID.String()},
		{"outbound", `SELECT count(*) FROM outbound_messages WHERE user_id = $1`, userID},
		{"pending", `SELECT count(*) FROM pending_actions WHERE user_id = $1`, userID},
		{"schedules", `SELECT count(*) FROM scheduled_summary_preferences WHERE user_id = $1`, userID},
		{"jobs", `SELECT count(*) FROM queue_jobs WHERE payload_json->>'user_id' = $1`, userID.String()},
	} {
		var count int
		queryErr := pool.QueryRow(ctx, check.sql, check.arg).Scan(&count)
		if queryErr != nil {
			t.Fatalf("%s count: %v", check.name, queryErr)
		}
		if count != 0 {
			t.Errorf("%s remaining = %d, want 0", check.name, count)
		}
	}
	var (
		tombstoneUser   *uuid.UUID
		tombstoneEvent  string
		tombstoneHash   string
		tombstoneRaw    []byte
		tombstoneExpiry *time.Time
		legacyCount     int
	)
	if err := pool.QueryRow(ctx, `
		SELECT user_id, event_type, payload_hash, raw_payload_json, delete_after
		FROM provider_messages WHERE id = $1`, pmID).
		Scan(&tombstoneUser, &tombstoneEvent, &tombstoneHash, &tombstoneRaw, &tombstoneExpiry); err != nil {
		t.Fatalf("load deletion tombstone: %v", err)
	}
	if tombstoneUser != nil || tombstoneEvent != "account.deleted" || tombstoneHash != "" ||
		string(tombstoneRaw) != "{}" || tombstoneExpiry == nil {
		t.Errorf("unsafe deletion tombstone: user=%v event=%q hash=%q raw=%s expiry=%v",
			tombstoneUser, tombstoneEvent, tombstoneHash, tombstoneRaw, tombstoneExpiry)
	}
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM provider_messages WHERE id = $1`, legacyPMID).Scan(&legacyCount); err != nil {
		t.Fatal(err)
	}
	if legacyCount != 0 {
		t.Errorf("legacy provider message remaining = %d", legacyCount)
	}

	var status, timezone, currency, locale, consent string
	var consentedAt *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT status, timezone, default_currency, locale, consent_version, consented_at
		FROM users WHERE id = $1`, userID).
		Scan(&status, &timezone, &currency, &locale, &consent, &consentedAt); err != nil {
		t.Fatal(err)
	}
	if status != "deleted" || timezone != "UTC" || currency != "" || locale != "" || consent != "" || consentedAt != nil {
		t.Errorf("user tombstone not de-identified: %q %q %q %q %q %v",
			status, timezone, currency, locale, consent, consentedAt)
	}
	var otherCount, globalUsage int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_identities WHERE user_id = $1`, otherID).Scan(&otherCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM usage_counters WHERE scope = 'global'`).Scan(&globalUsage); err != nil {
		t.Fatal(err)
	}
	if otherCount != 1 || globalUsage != 1 {
		t.Errorf("unrelated data changed: identity=%d global_usage=%d", otherCount, globalUsage)
	}
	if _, err := objects.Open(ctx, stored.Key); !errors.Is(err, objectstore.ErrNotFound) {
		t.Errorf("receipt object still opens: %v", err)
	}
	if _, err := os.Stat(exportDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("export directory still exists: %v", err)
	}
}

type failingObjectStore struct{}

func (failingObjectStore) Put(context.Context, string, io.Reader, string) (objectstore.Stored, error) {
	return objectstore.Stored{}, errors.New("not implemented")
}
func (failingObjectStore) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (failingObjectStore) Delete(context.Context, string) error {
	return errors.New("injected object delete failure")
}

func TestDeleteAccountObjectFailureLeavesRetryableDatabaseState(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `
		INSERT INTO receipt_documents (id, user_id, storage_key)
		VALUES ($1, $2, 'receipts/fail.jpg')`, uuid.New(), userID)

	if _, err := DeleteAccount(ctx, pool, failingObjectStore{}, "", userID, uuid.Nil); !domain.IsCode(err, domain.CodeTransient) {
		t.Fatalf("DeleteAccount error = %v, want transient", err)
	}
	var status string
	var receipts int
	if err := pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM receipt_documents WHERE user_id = $1`, userID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if status != string(domain.UserDeleting) || receipts != 1 {
		t.Fatalf("expected durable deleting saga state: status=%s receipts=%d", status, receipts)
	}
}

func TestDeleteAccountWaitsForInFlightUserWork(t *testing.T) {
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatal(err)
	}

	locked := make(chan struct{})
	release := make(chan struct{})
	workDone := make(chan error, 1)
	go func() {
		workDone <- postgres.WithUserLock(ctx, pool, userID, func(context.Context) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	deleteDone := make(chan error, 1)
	go func() {
		_, err := DeleteAccount(ctx, pool, nil, "", userID, uuid.Nil)
		deleteDone <- err
	}()
	select {
	case err := <-deleteDone:
		t.Fatalf("deletion bypassed in-flight lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-workDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatal(err)
	}
}
