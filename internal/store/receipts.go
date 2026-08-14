package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zl-expese-bot/internal/domain"
)

// ---------------------------------------------------------------------------
// Receipts
// ---------------------------------------------------------------------------

const receiptColumns = `id, user_id, provider_message_id, storage_key, source_url_hash,
	content_type, byte_size, sha256, perceptual_hash, status, retention_policy,
	delete_after, created_at, updated_at, deleted_at`

func scanReceipt(row pgx.Row) (*domain.ReceiptDocument, error) {
	var r domain.ReceiptDocument
	err := row.Scan(&r.ID, &r.UserID, &r.ProviderMessageID, &r.StorageKey, &r.SourceURLHash,
		&r.ContentType, &r.ByteSize, &r.SHA256, &r.PerceptualHash, &r.Status, &r.RetentionPolicy,
		&r.DeleteAfter, &r.CreatedAt, &r.UpdatedAt, &r.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateReceipt inserts a receipt row in status received (schema default).
func (s *Store) CreateReceipt(ctx context.Context, r *domain.ReceiptDocument) error {
	status := r.Status
	if status == "" {
		status = domain.ReceiptReceived
	}
	retention := r.RetentionPolicy
	if retention == "" {
		retention = "originals_30d"
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO receipt_documents
			(id, user_id, provider_message_id, storage_key, source_url_hash,
			 content_type, byte_size, sha256, perceptual_hash, status, retention_policy, delete_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		r.ID, r.UserID, r.ProviderMessageID, r.StorageKey, r.SourceURLHash,
		r.ContentType, r.ByteSize, r.SHA256, r.PerceptualHash, string(status), retention, r.DeleteAfter)
	if err != nil {
		return internalErr("CreateReceipt", err)
	}
	return nil
}

// GetOrCreateReceiptForProviderMessage makes the webhook-to-receipt boundary
// retry-safe. A reclaimed provider message always receives the original
// receipt row, never a second pipeline.
func (s *Store) GetOrCreateReceiptForProviderMessage(ctx context.Context, r *domain.ReceiptDocument) (*domain.ReceiptDocument, bool, error) {
	if r.ProviderMessageID == nil {
		return nil, false, domain.E(domain.CodeValidation, "store.GetOrCreateReceiptForProviderMessage: provider message is required", nil)
	}
	status := r.Status
	if status == "" {
		status = domain.ReceiptReceived
	}
	retention := r.RetentionPolicy
	if retention == "" {
		retention = "originals_30d"
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO receipt_documents
			(id, user_id, provider_message_id, storage_key, source_url_hash,
			 content_type, byte_size, sha256, perceptual_hash, status, retention_policy, delete_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 ON CONFLICT (provider_message_id) WHERE provider_message_id IS NOT NULL DO NOTHING`,
		r.ID, r.UserID, r.ProviderMessageID, r.StorageKey, r.SourceURLHash,
		r.ContentType, r.ByteSize, r.SHA256, r.PerceptualHash, string(status), retention, r.DeleteAfter)
	if err != nil {
		return nil, false, internalErr("GetOrCreateReceiptForProviderMessage", err)
	}
	created := tag.RowsAffected() == 1
	stored, err := scanReceipt(s.pool.QueryRow(ctx,
		`SELECT `+receiptColumns+` FROM receipt_documents WHERE provider_message_id = $1`,
		*r.ProviderMessageID))
	if err != nil {
		return nil, false, internalErr("GetOrCreateReceiptForProviderMessage", err)
	}
	if stored.UserID != r.UserID {
		return nil, false, domain.E(domain.CodeConflict, "store.GetOrCreateReceiptForProviderMessage: receipt belongs to another user", nil)
	}
	return stored, created, nil
}

// DeleteReceivedReceipt removes an unstarted registration after a failure
// before it was queued. The status guard prevents deleting worker-owned data.
func (s *Store) DeleteReceivedReceipt(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM receipt_documents WHERE id = $1 AND status = 'received'`, id)
	if err != nil {
		return internalErr("DeleteReceivedReceipt", err)
	}
	return nil
}

// GetReceipt loads a receipt by primary key.
func (s *Store) GetReceipt(ctx context.Context, id uuid.UUID) (*domain.ReceiptDocument, error) {
	r, err := scanReceipt(s.pool.QueryRow(ctx,
		`SELECT `+receiptColumns+` FROM receipt_documents WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, notFound("GetReceipt", "receipt")
	}
	if err != nil {
		return nil, internalErr("GetReceipt", err)
	}
	return r, nil
}

// TransitionReceipt moves a receipt along its state machine. The domain
// machine is checked first (CodeValidation); the WHERE status = from guard
// turns concurrent/duplicate transitions into CodeConflict.
func (s *Store) TransitionReceipt(ctx context.Context, id uuid.UUID, from, to domain.ReceiptStatus) error {
	if !from.CanTransition(to) {
		return domain.Ef(domain.CodeValidation, nil,
			"store.TransitionReceipt: illegal receipt transition %s -> %s", from, to)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE receipt_documents SET status = $3, updated_at = now(),
			deleted_at = CASE WHEN $3 = 'deleted' THEN now() ELSE deleted_at END
		 WHERE id = $1 AND status = $2`,
		id, string(from), string(to))
	if err != nil {
		return internalErr("TransitionReceipt", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Ef(domain.CodeConflict, nil,
			"store.TransitionReceipt: receipt not in status %s", from)
	}
	return nil
}

// SetReceiptStored records the object-store location and content hash after
// a successful download.
func (s *Store) SetReceiptStored(ctx context.Context, id uuid.UUID, key, sha256, contentType string, size int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE receipt_documents SET storage_key = $2, sha256 = $3, content_type = $4,
			byte_size = $5, updated_at = now()
		 WHERE id = $1`,
		id, key, sha256, contentType, size)
	if err != nil {
		return internalErr("SetReceiptStored", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("SetReceiptStored", "receipt")
	}
	return nil
}

// FindReceiptByHash finds the user's most recent non-deleted receipt with
// this content hash (duplicate image check before OCR). CodeNotFound when
// none — that is the common case, not an error worth logging.
func (s *Store) FindReceiptByHash(ctx context.Context, userID uuid.UUID, sha256 string) (*domain.ReceiptDocument, error) {
	r, err := scanReceipt(s.pool.QueryRow(ctx,
		`SELECT `+receiptColumns+` FROM receipt_documents
		 WHERE user_id = $1 AND sha256 = $2 AND sha256 <> '' AND deleted_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`,
		userID, sha256))
	if isNoRows(err) {
		return nil, notFound("FindReceiptByHash", "receipt")
	}
	if err != nil {
		return nil, internalErr("FindReceiptByHash", err)
	}
	return r, nil
}

// ListReceiptsPendingDeletion feeds the retention sweep: past delete_after
// and not yet deleted, plus soft-deleted receipts that still hold a storage
// object key (discarded originals left behind).
func (s *Store) ListReceiptsPendingDeletion(ctx context.Context, now time.Time) ([]domain.ReceiptDocument, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+receiptColumns+` FROM receipt_documents
		 WHERE (
		         delete_after IS NOT NULL AND delete_after < $1 AND deleted_at IS NULL
		         AND status IN ('confirmed', 'failed_permanent', 'review_required')
		       )
		    OR (deleted_at IS NOT NULL AND storage_key <> '')
		 ORDER BY delete_after NULLS LAST`,
		now.UTC())
	if err != nil {
		return nil, internalErr("ListReceiptsPendingDeletion", err)
	}
	defer rows.Close()
	var out []domain.ReceiptDocument
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, internalErr("ListReceiptsPendingDeletion", err)
		}
		out = append(out, *r)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListReceiptsPendingDeletion", err)
	}
	return out, nil
}

// ClearReceiptStorageKey empties storage_key after the object has been removed
// (or was already absent), so the sweeper stops selecting the row.
func (s *Store) ClearReceiptStorageKey(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE receipt_documents SET storage_key = '', updated_at = now()
		 WHERE id = $1`,
		id)
	if err != nil {
		return internalErr("ClearReceiptStorageKey", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("ClearReceiptStorageKey", "receipt")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Receipt processing attempts
// ---------------------------------------------------------------------------

// RecordAttempt inserts one processing-attempt state. A later terminal state
// (succeeded/failed) promotes the same started row in place; once terminal,
// duplicate or out-of-order writes are no-ops so completion evidence cannot
// be reset to started or changed to a different terminal outcome.
func (s *Store) RecordAttempt(ctx context.Context, receiptID uuid.UUID, processor, version string, attempt int, status, errClass, errCode string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO receipt_processing_attempts
			(receipt_id, processor_name, processor_version, attempt_number, status,
			 completed_at, error_class, error_code)
		 VALUES ($1, $2, $3, $4, $5,
			 CASE WHEN $5 = 'started' THEN NULL ELSE now() END, $6, $7)
			 ON CONFLICT (receipt_id, processor_name, processor_version, attempt_number)
			 DO UPDATE SET status = EXCLUDED.status,
			 	completed_at = EXCLUDED.completed_at,
			 	error_class = EXCLUDED.error_class,
			 	error_code = EXCLUDED.error_code
			 WHERE receipt_processing_attempts.status = 'started'
			 	AND EXCLUDED.status IN ('succeeded', 'failed')`,
		receiptID, processor, version, attempt, status, errClass, errCode)
	if err != nil {
		return internalErr("RecordAttempt", err)
	}
	return nil
}
