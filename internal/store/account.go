package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/postgres"
)

// AccountPurgeStats tallies rows removed by PurgeAccountData.
type AccountPurgeStats struct {
	ReceiptsDeleted     int
	TransactionsDeleted int
	RowsPurged          int
	JobsPurged          int
}

// BeginAccountDeletion marks the user deleting (if not already) and returns
// every non-empty receipt object key. Already-deleted users are CodeConflict.
func (s *Store) BeginAccountDeletion(ctx context.Context, userID uuid.UUID) ([]string, error) {
	var keys []string
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		var deletedAt *time.Time
		err := tx.QueryRow(ctx,
			`SELECT status, deleted_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&status, &deletedAt)
		if isNoRows(err) {
			return domain.Ef(domain.CodeNotFound, nil, "user %s not found", userID)
		}
		if err != nil {
			return internalErr("BeginAccountDeletion", err)
		}
		if deletedAt != nil || status == string(domain.UserDeleted) {
			return domain.Ef(domain.CodeConflict, nil, "user %s already deleted", userID)
		}
		if status != string(domain.UserDeleting) {
			if _, err := tx.Exec(ctx,
				`UPDATE users SET status = 'deleting', updated_at = now() WHERE id = $1`, userID); err != nil {
				return internalErr("BeginAccountDeletion", err)
			}
		}
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT storage_key FROM receipt_documents
			WHERE user_id = $1 AND storage_key <> ''`, userID)
		if err != nil {
			return internalErr("BeginAccountDeletion", err)
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return internalErr("BeginAccountDeletion", err)
			}
			keys = append(keys, key)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// PurgeAccountData removes all user-linked rows and writes the deleted
// tombstone. The user must already be in status deleting.
func (s *Store) PurgeAccountData(ctx context.Context, userID, triggeringMessageID uuid.UUID) (AccountPurgeStats, error) {
	var stats AccountPurgeStats
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		var deletedAt *time.Time
		err := tx.QueryRow(ctx,
			`SELECT status, deleted_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&status, &deletedAt)
		if isNoRows(err) {
			return domain.Ef(domain.CodeNotFound, nil, "user %s not found", userID)
		}
		if err != nil {
			return internalErr("PurgeAccountData", err)
		}
		if deletedAt != nil || status == string(domain.UserDeleted) {
			return domain.Ef(domain.CodeConflict, nil, "user %s already deleted", userID)
		}
		if status != string(domain.UserDeleting) {
			return domain.Ef(domain.CodeConflict, nil, "user %s is not deleting", userID)
		}

		if stats.JobsPurged, err = execCount(ctx, tx, `
			DELETE FROM queue_jobs
			WHERE payload_json->>'user_id' = $1::text`, userID); err != nil {
			return internalErr("PurgeAccountData", err)
		}

		for _, statement := range []struct {
			op  string
			sql string
		}{
			{"pending", `DELETE FROM pending_actions WHERE user_id = $1`},
			{"schedules", `DELETE FROM scheduled_summary_preferences WHERE user_id = $1`},
			{"outbound", `DELETE FROM outbound_messages WHERE user_id = $1`},
			{"insights", `DELETE FROM insights WHERE user_id = $1`},
			{"rules", `DELETE FROM user_merchant_rules WHERE user_id = $1`},
			{"usage", `DELETE FROM usage_counters WHERE scope = 'user' AND scope_id = $1::text`},
			{"corrections", `
				DELETE FROM corrections
				WHERE transaction_id IN (SELECT id FROM transactions WHERE user_id = $1)`},
			{"predictions", `
				DELETE FROM predictions
				WHERE transaction_id IN (SELECT id FROM transactions WHERE user_id = $1)`},
		} {
			n, execErr := execCount(ctx, tx, statement.sql, userID)
			if execErr != nil {
				return internalErr("PurgeAccountData", execErr)
			}
			stats.RowsPurged += n
		}

		if stats.TransactionsDeleted, err = execCount(ctx, tx,
			`DELETE FROM transactions WHERE user_id = $1`, userID); err != nil {
			return internalErr("PurgeAccountData", err)
		}
		stats.RowsPurged += stats.TransactionsDeleted

		for _, statement := range []struct {
			sql string
		}{
			{`DELETE FROM extracted_fields
				WHERE receipt_document_id IN (SELECT id FROM receipt_documents WHERE user_id = $1)`},
			{`DELETE FROM receipt_processing_attempts
				WHERE receipt_id IN (SELECT id FROM receipt_documents WHERE user_id = $1)`},
		} {
			n, execErr := execCount(ctx, tx, statement.sql, userID)
			if execErr != nil {
				return internalErr("PurgeAccountData", execErr)
			}
			stats.RowsPurged += n
		}

		if stats.ReceiptsDeleted, err = execCount(ctx, tx,
			`DELETE FROM receipt_documents WHERE user_id = $1`, userID); err != nil {
			return internalErr("PurgeAccountData", err)
		}
		stats.RowsPurged += stats.ReceiptsDeleted

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
			return internalErr("PurgeAccountData", execErr)
		} else {
			stats.RowsPurged += n
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
				return internalErr("PurgeAccountData", execErr)
			}
			if tag.RowsAffected() != 1 {
				return domain.E(domain.CodeConflict, "deletion provider message is missing or has another owner", nil)
			}
		}
		if n, execErr := execCount(ctx, tx,
			`DELETE FROM user_identities WHERE user_id = $1`, userID); execErr != nil {
			return internalErr("PurgeAccountData", execErr)
		} else {
			stats.RowsPurged += n
		}

		if _, err = execCount(ctx, tx, `
			UPDATE users
			SET status = 'deleted', deleted_at = now(), updated_at = now(),
			    consent_version = '', consented_at = NULL,
			    timezone = 'UTC', default_currency = '', locale = ''
			WHERE id = $1`, userID); err != nil {
			return internalErr("PurgeAccountData", err)
		}
		return nil
	})
	return stats, err
}

// ExportTransaction is one confirmed/amended row for portable CSV export.
type ExportTransaction struct {
	ID          uuid.UUID
	OccurredAt  time.Time
	Type        string
	Merchant    string
	Description string
	AmountMinor int64
	Currency    string
	CategoryKey string
	Status      string
	Source      string
}

// ListExportTransactions returns recorded transactions for a user export.
func (s *Store) ListExportTransactions(ctx context.Context, userID uuid.UUID) ([]ExportTransaction, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.occurred_at, t.type, t.merchant_name, t.description,
		       t.amount_minor, t.currency, COALESCE(c.system_key, ''), t.status, t.source
		FROM transactions t
		LEFT JOIN categories c ON c.id = t.category_id
		WHERE t.user_id = $1 AND t.deleted_at IS NULL
		  AND t.status IN ('confirmed','amended')
		ORDER BY t.occurred_at ASC, t.id ASC`, userID)
	if err != nil {
		return nil, internalErr("ListExportTransactions", err)
	}
	defer rows.Close()
	var out []ExportTransaction
	for rows.Next() {
		var row ExportTransaction
		if err := rows.Scan(&row.ID, &row.OccurredAt, &row.Type, &row.Merchant, &row.Description,
			&row.AmountMinor, &row.Currency, &row.CategoryKey, &row.Status, &row.Source); err != nil {
			return nil, internalErr("ListExportTransactions", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListExportTransactions", err)
	}
	return out, nil
}

// AccountExportMeta is the portable account-metadata snapshot.
type AccountExportMeta struct {
	User             domain.User
	Identities       []domain.UserIdentity
	SummarySchedules []domain.SummarySchedule
	TransactionCount int
	ReceiptCount     int
	InsightCount     int
	ScheduleCount    int
}

// LoadAccountExport loads the user's metadata for a JSON export.
func (s *Store) LoadAccountExport(ctx context.Context, userID uuid.UUID) (AccountExportMeta, error) {
	var doc AccountExportMeta
	u, err := s.GetUser(ctx, userID)
	if err != nil {
		return doc, err
	}
	doc.User = *u

	idRows, err := s.pool.Query(ctx, `
		SELECT id, user_id, provider, provider_subject, provider_scope, created_at, last_seen_at
		FROM user_identities WHERE user_id = $1
		ORDER BY provider, provider_subject`, userID)
	if err != nil {
		return doc, internalErr("LoadAccountExport", err)
	}
	defer idRows.Close()
	doc.Identities = []domain.UserIdentity{}
	for idRows.Next() {
		var ident domain.UserIdentity
		if err := idRows.Scan(&ident.ID, &ident.UserID, &ident.Provider, &ident.ProviderSubject,
			&ident.ProviderScope, &ident.CreatedAt, &ident.LastSeenAt); err != nil {
			return doc, internalErr("LoadAccountExport", err)
		}
		doc.Identities = append(doc.Identities, ident)
	}
	if err := idRows.Err(); err != nil {
		return doc, internalErr("LoadAccountExport", err)
	}

	schedules, err := s.ListSummarySchedules(ctx, userID)
	if err != nil {
		return doc, err
	}
	doc.SummarySchedules = schedules

	err = s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*)::int FROM transactions
		    WHERE user_id = $1 AND deleted_at IS NULL AND status IN ('confirmed','amended')),
		  (SELECT COUNT(*)::int FROM receipt_documents
		    WHERE user_id = $1 AND deleted_at IS NULL),
		  (SELECT COUNT(*)::int FROM insights WHERE user_id = $1),
		  (SELECT COUNT(*)::int FROM scheduled_summary_preferences
		    WHERE user_id = $1)`, userID).
		Scan(&doc.TransactionCount, &doc.ReceiptCount, &doc.InsightCount, &doc.ScheduleCount)
	if err != nil {
		return doc, internalErr("LoadAccountExport", err)
	}
	return doc, nil
}

func execCount(ctx context.Context, tx pgx.Tx, sql string, args ...any) (int, error) {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	return int(tag.RowsAffected()), nil
}
