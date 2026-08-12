package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/postgres"
)

// Report tallies the user-linked material removed by DeleteAccount.
type Report struct {
	ReceiptsDeleted     int `json:"receipts_deleted"`
	TransactionsDeleted int `json:"transactions_deleted"`
	FilesRemoved        int `json:"files_removed"`
	ExportsRemoved      int `json:"exports_removed"`
	RowsPurged          int `json:"rows_purged"`
	JobsPurged          int `json:"jobs_purged"`
}

// DeleteAccount executes the data-deletion right for userID.
//
// Every application path that can mutate user data participates in the same
// advisory lock. Deletion therefore waits for in-flight work, purges queued
// work, and prevents a worker from recreating rows after commit. The users
// row remains only as a de-identified lifecycle tombstone; all provider IDs,
// payloads, receipts, derived records, analytics, message copies and local
// CSV/JSON export artifacts are physically removed.
//
// Object deletion cannot join the database transaction. Objects are removed
// first and any failure aborts the database purge, making the operation
// safely retryable (successful object deletes are idempotent).
func DeleteAccount(ctx context.Context, pool *pgxpool.Pool, objects objectstore.Store, dataDir string, userID, triggeringMessageID uuid.UUID) (Report, error) {
	var report Report
	err := postgres.WithUserLock(ctx, pool, userID, func(lockedCtx context.Context) error {
		var err error
		report, err = deleteAccountLocked(lockedCtx, pool, objects, dataDir, userID, triggeringMessageID)
		return err
	})
	return report, err
}

func deleteAccountLocked(ctx context.Context, pool *pgxpool.Pool, objects objectstore.Store, dataDir string, userID, triggeringMessageID uuid.UUID) (Report, error) {
	var report Report
	err := postgres.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var deletedAt *time.Time
		err := tx.QueryRow(ctx,
			`SELECT deleted_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&deletedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Ef(domain.CodeNotFound, nil, "user %s not found", userID)
		}
		if err != nil {
			return domain.E(domain.CodeTransient, "load user for deletion", err)
		}
		if deletedAt != nil {
			return domain.Ef(domain.CodeConflict, nil, "user %s already deleted", userID)
		}

		keys, err := receiptKeys(ctx, tx, userID)
		if err != nil {
			return err
		}
		if len(keys) > 0 && objects == nil {
			return domain.E(domain.CodeTransient, "receipt object store unavailable during account deletion", nil)
		}
		for _, key := range keys {
			if err := objects.Delete(ctx, key); err != nil {
				return domain.Ef(domain.CodeTransient, err, "delete receipt object %s", key)
			}
			report.FilesRemoved++
		}
		exportsRemoved, err := removeExportArtifacts(dataDir, userID)
		if err != nil {
			return domain.E(domain.CodeTransient, "delete account exports", err)
		}
		report.ExportsRemoved = exportsRemoved

		// Stop every not-yet-acknowledged job before deleting its source rows.
		if report.JobsPurged, err = execCount(ctx, tx, `
			DELETE FROM queue_jobs
			WHERE payload_json->>'user_id' = $1::text`, userID); err != nil {
			return domain.E(domain.CodeTransient, "delete queued user jobs", err)
		}

		for _, statement := range []struct {
			op  string
			sql string
		}{
			{"delete pending actions", `DELETE FROM pending_actions WHERE user_id = $1`},
			{"delete summary schedules", `DELETE FROM scheduled_summary_preferences WHERE user_id = $1`},
			{"delete outbound messages", `DELETE FROM outbound_messages WHERE user_id = $1`},
			{"delete insights", `DELETE FROM insights WHERE user_id = $1`},
			{"delete merchant rules", `DELETE FROM user_merchant_rules WHERE user_id = $1`},
			{"delete user usage", `DELETE FROM usage_counters WHERE scope = 'user' AND scope_id = $1::text`},
			{"delete corrections", `
				DELETE FROM corrections
				WHERE transaction_id IN (SELECT id FROM transactions WHERE user_id = $1)`},
			{"delete predictions", `
				DELETE FROM predictions
				WHERE transaction_id IN (SELECT id FROM transactions WHERE user_id = $1)`},
		} {
			n, execErr := execCount(ctx, tx, statement.sql, userID)
			if execErr != nil {
				return domain.E(domain.CodeTransient, statement.op, execErr)
			}
			report.RowsPurged += n
		}

		if report.TransactionsDeleted, err = execCount(ctx, tx,
			`DELETE FROM transactions WHERE user_id = $1`, userID); err != nil {
			return domain.E(domain.CodeTransient, "delete transactions", err)
		}
		report.RowsPurged += report.TransactionsDeleted

		for _, statement := range []struct {
			op  string
			sql string
		}{
			{"delete extracted fields", `
				DELETE FROM extracted_fields
				WHERE receipt_document_id IN (SELECT id FROM receipt_documents WHERE user_id = $1)`},
			{"delete receipt attempts", `
				DELETE FROM receipt_processing_attempts
				WHERE receipt_id IN (SELECT id FROM receipt_documents WHERE user_id = $1)`},
		} {
			n, execErr := execCount(ctx, tx, statement.sql, userID)
			if execErr != nil {
				return domain.E(domain.CodeTransient, statement.op, execErr)
			}
			report.RowsPurged += n
		}

		if report.ReceiptsDeleted, err = execCount(ctx, tx,
			`DELETE FROM receipt_documents WHERE user_id = $1`, userID); err != nil {
			return domain.E(domain.CodeTransient, "delete receipts", err)
		}
		report.RowsPurged += report.ReceiptsDeleted

		// user_id covers fixed/new rows. The JSON predicates remove legacy
		// rows written before ownership was persisted.
		if n, execErr := execCount(ctx, tx, `
			DELETE FROM provider_messages pm
			WHERE pm.id <> $2
			  AND (pm.user_id = $1
			   OR EXISTS (
					SELECT 1 FROM user_identities i
					WHERE i.user_id = $1
					  AND i.provider = pm.provider
					  AND i.provider_subject IN (
						COALESCE(pm.raw_payload_json->>'provider_user_id', ''),
						COALESCE(pm.raw_payload_json#>>'{message,from,id}', '')
					  )
			   ))`, userID, triggeringMessageID); execErr != nil {
			return domain.E(domain.CodeTransient, "delete provider messages", execErr)
		} else {
			report.RowsPurged += n
		}
		if triggeringMessageID != uuid.Nil {
			tag, execErr := tx.Exec(ctx, `
				UPDATE provider_messages
				SET user_id = NULL, event_type = 'account.deleted',
				    payload_hash = '', raw_payload_json = '{}',
				    status = 'processed', processing_started_at = NULL,
				    processed_at = now(), delete_after = now() + interval '24 hours'
				WHERE id = $1 AND user_id = $2`,
				triggeringMessageID, userID)
			if execErr != nil {
				return domain.E(domain.CodeTransient, "sanitize deletion provider message", execErr)
			}
			if tag.RowsAffected() != 1 {
				return domain.E(domain.CodeConflict, "deletion provider message is missing or has another owner", nil)
			}
		}
		if n, execErr := execCount(ctx, tx,
			`DELETE FROM user_identities WHERE user_id = $1`, userID); execErr != nil {
			return domain.E(domain.CodeTransient, "delete identities", execErr)
		} else {
			report.RowsPurged += n
		}

		if _, err = execCount(ctx, tx, `
			UPDATE users
			SET status = 'deleted', deleted_at = now(), updated_at = now(),
			    consent_version = '', consented_at = NULL,
			    timezone = 'UTC', default_currency = '', locale = ''
			WHERE id = $1`, userID); err != nil {
			return domain.E(domain.CodeTransient, "de-identify user tombstone", err)
		}
		return nil
	})
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) {
			return report, de
		}
		return report, domain.E(domain.CodeTransient, "delete account transaction", err)
	}
	return report, nil
}

func removeExportArtifacts(dataDir string, userID uuid.UUID) (int, error) {
	if strings.TrimSpace(dataDir) == "" {
		return 0, nil
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "exports"))
	if err != nil {
		return 0, fmt.Errorf("resolve export root: %w", err)
	}
	target := filepath.Join(root, userID.String())
	rel, err := filepath.Rel(root, target)
	if err != nil || rel != userID.String() {
		return 0, fmt.Errorf("refuse unsafe export path %q", target)
	}
	entries, err := os.ReadDir(target)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read export directory: %w", err)
	}
	if err := os.RemoveAll(target); err != nil {
		return 0, fmt.Errorf("remove export directory: %w", err)
	}
	return len(entries), nil
}

// receiptKeys lists every non-empty object key, including already
// soft-deleted rows whose file might have survived an earlier partial flow.
func receiptKeys(ctx context.Context, tx pgx.Tx, userID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT storage_key FROM receipt_documents
		WHERE user_id = $1 AND storage_key <> ''`, userID)
	if err != nil {
		return nil, domain.E(domain.CodeTransient, "list receipts for deletion", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, domain.E(domain.CodeTransient, "scan receipt key", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, domain.E(domain.CodeTransient, "iterate receipt keys", err)
	}
	return keys, nil
}

func execCount(ctx context.Context, tx pgx.Tx, sql string, args ...any) (int, error) {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	return int(tag.RowsAffected()), nil
}
