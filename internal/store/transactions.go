package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/postgres"
)

// ---------------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------------

const txColumns = `id, user_id, receipt_document_id, account_id, type, merchant_id,
	merchant_name, description, amount_minor, currency, occurred_at, category_id,
	status, source, confidence_summary, confirmed_at, version, created_at, updated_at, deleted_at`

func scanTransaction(row pgx.Row) (*domain.Transaction, error) {
	var t domain.Transaction
	err := row.Scan(&t.ID, &t.UserID, &t.ReceiptDocumentID, &t.AccountID, &t.Type, &t.MerchantID,
		&t.MerchantName, &t.Description, &t.AmountMinor, &t.Currency, &t.OccurredAt, &t.CategoryID,
		&t.Status, &t.Source, &t.ConfidenceSummary, &t.ConfirmedAt, &t.Version,
		&t.CreatedAt, &t.UpdatedAt, &t.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTransaction inserts a transaction row; the caller owns the ID,
// status and initial version (1). Duplicate active receipt drafts map to
// CodeConflict via idx_tx_receipt_active.
func (s *Store) CreateTransaction(ctx context.Context, t *domain.Transaction) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO transactions
			(id, user_id, receipt_document_id, account_id, type, merchant_id,
			 merchant_name, description, amount_minor, currency, occurred_at, category_id,
			 status, source, confidence_summary, confirmed_at, version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		t.ID, t.UserID, t.ReceiptDocumentID, t.AccountID, string(t.Type), t.MerchantID,
		t.MerchantName, t.Description, t.AmountMinor, t.Currency, t.OccurredAt.UTC(), t.CategoryID,
		string(t.Status), string(t.Source), t.ConfidenceSummary, t.ConfirmedAt, t.Version)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.E(domain.CodeConflict, "store.CreateTransaction: duplicate", nil)
		}
		return internalErr("CreateTransaction", err)
	}
	return nil
}

// GetActiveTransactionByReceipt returns the non-deleted transaction linked to
// a receipt, if any. Used for crash-safe draft resume.
func (s *Store) GetActiveTransactionByReceipt(ctx context.Context, receiptID uuid.UUID) (*domain.Transaction, error) {
	t, err := scanTransaction(s.pool.QueryRow(ctx,
		`SELECT `+txColumns+` FROM transactions
		 WHERE receipt_document_id = $1 AND deleted_at IS NULL AND status <> 'deleted'
		 ORDER BY created_at DESC LIMIT 1`,
		receiptID))
	if isNoRows(err) {
		return nil, notFound("GetActiveTransactionByReceipt", "transaction")
	}
	if err != nil {
		return nil, internalErr("GetActiveTransactionByReceipt", err)
	}
	return t, nil
}

// ReceiptReviewDraft is the atomic unit-of-work that turns extraction output
// into a reviewable draft: transaction, provenance, predictions, receipt
// status, and pending action commit together or not at all.
type ReceiptReviewDraft struct {
	FromStatus  domain.ReceiptStatus
	Transaction *domain.Transaction
	Fields      []domain.ExtractedField
	Predictions []domain.Prediction
	Pending     *domain.PendingAction
}

// FinalizeReceiptReview persists a receipt draft transactionally. If an
// active transaction already exists for the receipt, that row is reused
// (idempotent resume) and fields/predictions are refreshed.
func (s *Store) FinalizeReceiptReview(ctx context.Context, d ReceiptReviewDraft) error {
	if d.Transaction == nil || d.Pending == nil {
		return domain.E(domain.CodeValidation, "store.FinalizeReceiptReview: missing draft fields", nil)
	}
	if d.Transaction.ReceiptDocumentID == nil {
		return domain.E(domain.CodeValidation, "store.FinalizeReceiptReview: receipt id required", nil)
	}
	receiptID := *d.Transaction.ReceiptDocumentID
	return postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		existing, err := scanTransaction(tx.QueryRow(ctx,
			`SELECT `+txColumns+` FROM transactions
			 WHERE receipt_document_id = $1 AND deleted_at IS NULL AND status <> 'deleted'
			 ORDER BY created_at DESC LIMIT 1
			 FOR UPDATE`,
			receiptID))
		switch {
		case isNoRows(err):
			if d.Transaction.ID == uuid.Nil {
				d.Transaction.ID = uuid.New()
			}
			_, err = tx.Exec(ctx,
				`INSERT INTO transactions
					(id, user_id, receipt_document_id, account_id, type, merchant_id,
					 merchant_name, description, amount_minor, currency, occurred_at, category_id,
					 status, source, confidence_summary, confirmed_at, version)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
				d.Transaction.ID, d.Transaction.UserID, d.Transaction.ReceiptDocumentID, d.Transaction.AccountID,
				string(d.Transaction.Type), d.Transaction.MerchantID, d.Transaction.MerchantName,
				d.Transaction.Description, d.Transaction.AmountMinor, d.Transaction.Currency,
				d.Transaction.OccurredAt.UTC(), d.Transaction.CategoryID, string(d.Transaction.Status),
				string(d.Transaction.Source), d.Transaction.ConfidenceSummary, d.Transaction.ConfirmedAt,
				d.Transaction.Version)
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return domain.E(domain.CodeConflict, "store.FinalizeReceiptReview: duplicate draft", nil)
				}
				return internalErr("FinalizeReceiptReview.insertTx", err)
			}
		case err != nil:
			return internalErr("FinalizeReceiptReview.loadTx", err)
		default:
			d.Transaction.ID = existing.ID
			d.Transaction.Version = existing.Version
			_, err = tx.Exec(ctx,
				`UPDATE transactions SET
					type = $2, merchant_id = $3, merchant_name = $4, description = $5,
					amount_minor = $6, currency = $7, occurred_at = $8, category_id = $9,
					status = $10, confidence_summary = $11, version = version + 1, updated_at = now()
				 WHERE id = $1 AND deleted_at IS NULL`,
				existing.ID, string(d.Transaction.Type), d.Transaction.MerchantID, d.Transaction.MerchantName,
				d.Transaction.Description, d.Transaction.AmountMinor, d.Transaction.Currency,
				d.Transaction.OccurredAt.UTC(), d.Transaction.CategoryID, string(d.Transaction.Status),
				d.Transaction.ConfidenceSummary)
			if err != nil {
				return internalErr("FinalizeReceiptReview.updateTx", err)
			}
			d.Transaction.Version = existing.Version + 1
			if _, err := tx.Exec(ctx, `DELETE FROM predictions WHERE transaction_id = $1`, existing.ID); err != nil {
				return internalErr("FinalizeReceiptReview.clearPreds", err)
			}
		}

		if _, err := tx.Exec(ctx, `DELETE FROM extracted_fields WHERE receipt_document_id = $1`, receiptID); err != nil {
			return internalErr("FinalizeReceiptReview.clearFields", err)
		}
		for i := range d.Fields {
			f := &d.Fields[i]
			if f.ID == uuid.Nil {
				f.ID = uuid.New()
			}
			evidence := f.Evidence
			if len(evidence) == 0 {
				evidence = []byte("{}")
			}
			source := f.Source
			if source == "" {
				source = "extractor"
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO extracted_fields
					(id, receipt_document_id, field_name, raw_value, normalised_value,
					 confidence, source, evidence_json, extractor_name, extractor_version)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
				f.ID, f.ReceiptDocumentID, f.FieldName, f.RawValue, f.NormalisedValue,
				f.Confidence, source, evidence, f.ExtractorName, f.ExtractorVersion)
			if err != nil {
				return internalErr("FinalizeReceiptReview.fields", err)
			}
		}
		for i := range d.Predictions {
			p := &d.Predictions[i]
			if p.ID == uuid.Nil {
				p.ID = uuid.New()
			}
			p.TransactionID = d.Transaction.ID
			features := p.FeatureSnapshot
			if len(features) == 0 {
				features = []byte("{}")
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO predictions
					(id, transaction_id, prediction_type, predicted_value,
					 confidence, model_name, model_version, feature_snapshot_json)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				p.ID, p.TransactionID, p.PredictionType, p.PredictedValue,
				p.Confidence, p.ModelName, p.ModelVersion, features)
			if err != nil {
				return internalErr("FinalizeReceiptReview.preds", err)
			}
		}

		if !d.FromStatus.CanTransition(domain.ReceiptReviewRequired) {
			return domain.Ef(domain.CodeValidation, nil,
				"store.FinalizeReceiptReview: illegal receipt transition %s -> review_required", d.FromStatus)
		}
		tag, err := tx.Exec(ctx,
			`UPDATE receipt_documents SET status = 'review_required', updated_at = now()
			 WHERE id = $1 AND status = $2`,
			receiptID, string(d.FromStatus))
		if err != nil {
			return internalErr("FinalizeReceiptReview.receipt", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.Ef(domain.CodeConflict, nil,
				"store.FinalizeReceiptReview: receipt not in status %s", d.FromStatus)
		}

		if d.Pending.ID == uuid.Nil {
			d.Pending.ID = uuid.New()
		}
		d.Pending.TransactionID = &d.Transaction.ID
		if _, err := tx.Exec(ctx, `DELETE FROM pending_actions WHERE user_id = $1`, d.Pending.UserID); err != nil {
			return internalErr("FinalizeReceiptReview.clearPending", err)
		}
		payload := d.Pending.Payload
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO pending_actions (id, user_id, kind, transaction_id, payload_json, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			d.Pending.ID, d.Pending.UserID, string(d.Pending.Kind), d.Pending.TransactionID,
			payload, d.Pending.ExpiresAt.UTC())
		if err != nil {
			return internalErr("FinalizeReceiptReview.pending", err)
		}
		return nil
	})
}

// ManualDraftBundle atomically creates a manual transaction, predictions, and
// pending confirmation action.
type ManualDraftBundle struct {
	Transaction       *domain.Transaction
	Predictions       []domain.Prediction
	Pending           *domain.PendingAction
	ProviderMessageID *uuid.UUID
}

// CreateManualDraft persists a manual entry unit-of-work. When
// ProviderMessageID is set, a retry of the same inbound message reuses the
// existing draft instead of inserting a second transaction.
func (s *Store) CreateManualDraft(ctx context.Context, d ManualDraftBundle) error {
	if d.Transaction == nil || d.Pending == nil {
		return domain.E(domain.CodeValidation, "store.CreateManualDraft: missing draft fields", nil)
	}
	return postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if d.ProviderMessageID != nil {
			existing, err := scanTransaction(tx.QueryRow(ctx,
				`SELECT `+txColumns+` FROM transactions
				 WHERE provider_message_id = $1 AND deleted_at IS NULL AND status <> 'deleted'
				 LIMIT 1
				 FOR UPDATE`,
				*d.ProviderMessageID))
			if err == nil {
				d.Transaction.ID = existing.ID
				d.Transaction.Version = existing.Version
				d.Transaction.AmountMinor = existing.AmountMinor
				d.Transaction.Currency = existing.Currency
				d.Transaction.MerchantName = existing.MerchantName
				d.Transaction.Type = existing.Type
				d.Transaction.CategoryID = existing.CategoryID
				d.Transaction.OccurredAt = existing.OccurredAt
				d.Transaction.Status = existing.Status
				if d.Pending.ID == uuid.Nil {
					d.Pending.ID = uuid.New()
				}
				d.Pending.TransactionID = &existing.ID
				if _, err := tx.Exec(ctx, `DELETE FROM pending_actions WHERE user_id = $1`, d.Pending.UserID); err != nil {
					return internalErr("CreateManualDraft.clearPending", err)
				}
				payload := d.Pending.Payload
				if len(payload) == 0 {
					payload = []byte("{}")
				}
				_, err = tx.Exec(ctx,
					`INSERT INTO pending_actions (id, user_id, kind, transaction_id, payload_json, expires_at)
					 VALUES ($1, $2, $3, $4, $5, $6)`,
					d.Pending.ID, d.Pending.UserID, string(d.Pending.Kind), d.Pending.TransactionID,
					payload, d.Pending.ExpiresAt.UTC())
				if err != nil {
					return internalErr("CreateManualDraft.pending", err)
				}
				return nil
			}
			if !isNoRows(err) {
				return internalErr("CreateManualDraft.lookup", err)
			}
		}
		if d.Transaction.ID == uuid.Nil {
			d.Transaction.ID = uuid.New()
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO transactions
				(id, user_id, receipt_document_id, account_id, type, merchant_id,
				 merchant_name, description, amount_minor, currency, occurred_at, category_id,
				 status, source, confidence_summary, confirmed_at, version, provider_message_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
			d.Transaction.ID, d.Transaction.UserID, d.Transaction.ReceiptDocumentID, d.Transaction.AccountID,
			string(d.Transaction.Type), d.Transaction.MerchantID, d.Transaction.MerchantName,
			d.Transaction.Description, d.Transaction.AmountMinor, d.Transaction.Currency,
			d.Transaction.OccurredAt.UTC(), d.Transaction.CategoryID, string(d.Transaction.Status),
			string(d.Transaction.Source), d.Transaction.ConfidenceSummary, d.Transaction.ConfirmedAt,
			d.Transaction.Version, d.ProviderMessageID)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.E(domain.CodeConflict, "store.CreateManualDraft: duplicate provider message", nil)
			}
			return internalErr("CreateManualDraft.insertTx", err)
		}
		for i := range d.Predictions {
			p := &d.Predictions[i]
			if p.ID == uuid.Nil {
				p.ID = uuid.New()
			}
			p.TransactionID = d.Transaction.ID
			features := p.FeatureSnapshot
			if len(features) == 0 {
				features = []byte("{}")
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO predictions
					(id, transaction_id, prediction_type, predicted_value,
					 confidence, model_name, model_version, feature_snapshot_json)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				p.ID, p.TransactionID, p.PredictionType, p.PredictedValue,
				p.Confidence, p.ModelName, p.ModelVersion, features)
			if err != nil {
				return internalErr("CreateManualDraft.preds", err)
			}
		}
		if d.Pending.ID == uuid.Nil {
			d.Pending.ID = uuid.New()
		}
		d.Pending.TransactionID = &d.Transaction.ID
		if _, err := tx.Exec(ctx, `DELETE FROM pending_actions WHERE user_id = $1`, d.Pending.UserID); err != nil {
			return internalErr("CreateManualDraft.clearPending", err)
		}
		payload := d.Pending.Payload
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO pending_actions (id, user_id, kind, transaction_id, payload_json, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			d.Pending.ID, d.Pending.UserID, string(d.Pending.Kind), d.Pending.TransactionID,
			payload, d.Pending.ExpiresAt.UTC())
		if err != nil {
			return internalErr("CreateManualDraft.pending", err)
		}
		return nil
	})
}

// GetTransaction loads a transaction by primary key, including soft-deleted
// ones (callers filter on DeletedAt when needed).
func (s *Store) GetTransaction(ctx context.Context, id uuid.UUID) (*domain.Transaction, error) {
	t, err := scanTransaction(s.pool.QueryRow(ctx,
		`SELECT `+txColumns+` FROM transactions WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, notFound("GetTransaction", "transaction")
	}
	if err != nil {
		return nil, internalErr("GetTransaction", err)
	}
	return t, nil
}

// GetTransactionForUser is the isolation boundary: a transaction that exists
// but belongs to another user is CodeNotFound, never leaked.
func (s *Store) GetTransactionForUser(ctx context.Context, id, userID uuid.UUID) (*domain.Transaction, error) {
	t, err := scanTransaction(s.pool.QueryRow(ctx,
		`SELECT `+txColumns+` FROM transactions WHERE id = $1 AND user_id = $2`,
		id, userID))
	if isNoRows(err) {
		return nil, notFound("GetTransactionForUser", "transaction")
	}
	if err != nil {
		return nil, internalErr("GetTransactionForUser", err)
	}
	return t, nil
}

// ListRecentTransactions returns the user's newest confirmed/amended,
// non-deleted transactions, most recent first. Drafts are excluded so
// they never appear as recorded spending.
func (s *Store) ListRecentTransactions(ctx context.Context, userID uuid.UUID, limit int) ([]domain.Transaction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+txColumns+` FROM transactions
		 WHERE user_id = $1 AND deleted_at IS NULL AND status IN ('confirmed','amended')
		 ORDER BY occurred_at DESC, created_at DESC LIMIT $2`,
		userID, limit)
	if err != nil {
		return nil, internalErr("ListRecentTransactions", err)
	}
	defer rows.Close()
	var out []domain.Transaction
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, internalErr("ListRecentTransactions", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListRecentTransactions", err)
	}
	return out, nil
}

// UpdateTransaction is the optimistic-concurrency write path: every mutable
// field is replaced, but only if the row is still at expectedVersion and not
// deleted. Zero rows means a concurrent edit won — CodeConflict.
func (s *Store) UpdateTransaction(ctx context.Context, t *domain.Transaction, expectedVersion int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE transactions SET
			receipt_document_id = $2, account_id = $3, type = $4, merchant_id = $5,
			merchant_name = $6, description = $7, amount_minor = $8, currency = $9,
			occurred_at = $10, category_id = $11, status = $12, source = $13,
			confidence_summary = $14, confirmed_at = $15,
			version = version + 1, updated_at = now()
		 WHERE id = $1 AND version = $16 AND deleted_at IS NULL`,
		t.ID, t.ReceiptDocumentID, t.AccountID, string(t.Type), t.MerchantID,
		t.MerchantName, t.Description, t.AmountMinor, t.Currency,
		t.OccurredAt.UTC(), t.CategoryID, string(t.Status), string(t.Source),
		t.ConfidenceSummary, t.ConfirmedAt, expectedVersion)
	if err != nil {
		return internalErr("UpdateTransaction", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Ef(domain.CodeConflict, nil,
			"store.UpdateTransaction: version conflict at %d", expectedVersion)
	}
	t.Version = expectedVersion + 1
	return nil
}

// SoftDeleteTransaction applies the user-facing delete. Illegal transitions
// (already deleted) are CodeValidation per the domain state machine.
func (s *Store) SoftDeleteTransaction(ctx context.Context, id, userID uuid.UUID) error {
	t, err := s.GetTransactionForUser(ctx, id, userID)
	if err != nil {
		return err
	}
	if !t.Status.CanTransition(domain.TxDeleted) {
		return domain.Ef(domain.CodeValidation, nil,
			"store.SoftDeleteTransaction: illegal transition %s -> deleted", t.Status)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE transactions SET status = 'deleted', deleted_at = now(),
			version = version + 1, updated_at = now()
		 WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		id, userID)
	if err != nil {
		return internalErr("SoftDeleteTransaction", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.E(domain.CodeConflict, "store.SoftDeleteTransaction: concurrent delete", nil)
	}
	return nil
}

// InsertCorrection records one user fix; corrections feed user_merchant_rules.
func (s *Store) InsertCorrection(ctx context.Context, c *domain.Correction) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO corrections (id, transaction_id, field_name, predicted_value, corrected_value, source)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		c.ID, c.TransactionID, c.FieldName, c.PredictedValue, c.CorrectedValue, c.Source)
	if err != nil {
		return internalErr("InsertCorrection", err)
	}
	return nil
}

// ListCorrections returns the audit trail of a transaction, oldest first.
func (s *Store) ListCorrections(ctx context.Context, txID uuid.UUID) ([]domain.Correction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, transaction_id, field_name, predicted_value, corrected_value, source, created_at
		 FROM corrections WHERE transaction_id = $1 ORDER BY created_at`,
		txID)
	if err != nil {
		return nil, internalErr("ListCorrections", err)
	}
	defer rows.Close()
	var out []domain.Correction
	for rows.Next() {
		var c domain.Correction
		if err := rows.Scan(&c.ID, &c.TransactionID, &c.FieldName,
			&c.PredictedValue, &c.CorrectedValue, &c.Source, &c.CreatedAt); err != nil {
			return nil, internalErr("ListCorrections", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListCorrections", err)
	}
	return out, nil
}

// LatestConfirmedTransaction returns the user's newest confirmed/amended,
// non-deleted transaction — the target of "đổi khoản gần nhất sang …".
func (s *Store) LatestConfirmedTransaction(ctx context.Context, userID uuid.UUID) (*domain.Transaction, error) {
	t, err := scanTransaction(s.pool.QueryRow(ctx,
		`SELECT `+txColumns+` FROM transactions
		 WHERE user_id = $1 AND deleted_at IS NULL AND status IN ('confirmed','amended')
		 ORDER BY occurred_at DESC, created_at DESC LIMIT 1`,
		userID))
	if isNoRows(err) {
		return nil, notFound("LatestConfirmedTransaction", "transaction")
	}
	if err != nil {
		return nil, internalErr("LatestConfirmedTransaction", err)
	}
	return t, nil
}

// FindPotentialDuplicates powers the soft-duplicate warning (P4-C01): a
// recorded transaction with the same amount and currency, an occurred_at
// within ±window of the new one, and the same merchant (by ID when known,
// else by case-insensitive name). Only confirmed/amended rows match — the
// draft being created never matches itself, and nothing here ever deletes;
// the caller only warns. At most 3 rows, newest first.
func (s *Store) FindPotentialDuplicates(ctx context.Context, userID uuid.UUID, amountMinor int64, currency string, occurredAt time.Time, window time.Duration, merchantID *uuid.UUID, merchantName string) ([]domain.Transaction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+txColumns+` FROM transactions
		 WHERE user_id = $1 AND deleted_at IS NULL
		   AND status IN ('confirmed','amended')
		   AND amount_minor = $2 AND currency = $3
		   AND occurred_at >= $4 AND occurred_at <= $5
		   AND (
		         ($6::uuid IS NOT NULL AND merchant_id = $6)
		      OR ($6::uuid IS NULL AND merchant_name <> '' AND lower(merchant_name) = lower($7))
		   )
		 ORDER BY occurred_at DESC, id DESC
		 LIMIT 3`,
		userID, amountMinor, currency,
		occurredAt.Add(-window), occurredAt.Add(window), merchantID, merchantName)
	if err != nil {
		return nil, internalErr("FindPotentialDuplicates", err)
	}
	defer rows.Close()
	out := []domain.Transaction{}
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, internalErr("FindPotentialDuplicates", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("FindPotentialDuplicates", err)
	}
	return out, nil
}
