// Package store is the single persistence layer over the 0001+0002 schema.
// It owns every SQL statement in the application: domain types in and out,
// placeholders only, and *domain.Error codes for expected failures
// (NotFound/Conflict/Validation). Nullable columns map to pointer fields.
package store

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/postgres"
)

// Store wraps the connection pool; all methods are safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a Store on an existing pool (see platform/postgres.Connect).
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// WithUserLock serializes multi-step user work with account deletion.
func (s *Store) WithUserLock(ctx context.Context, userID uuid.UUID, fn func(context.Context) error) error {
	return postgres.WithUserLock(ctx, s.pool, userID, fn)
}

func internalErr(op string, err error) error {
	return classifyStoreErr(op, err)
}

// classifyStoreErr maps connectivity, timeout, and serialization failures to
// CodeTransient so callers can retry; everything else stays CodeInternal.
func classifyStoreErr(op string, err error) error {
	if err == nil {
		return nil
	}
	code := domain.CodeInternal
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		code = domain.CodeTransient
	case errors.Is(err, context.Canceled):
		// Caller canceled the request; not a retryable store failure.
		code = domain.CodeInternal
	case isTransientPgError(err),
		isTransientNetError(err),
		errors.Is(err, io.ErrUnexpectedEOF),
		isTransientStoreMessage(err):
		code = domain.CodeTransient
	}
	return domain.E(code, "store."+op, err)
}

func isTransientPgError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "40001", // serialization_failure
		"40P01", // deadlock_detected
		"55P03", // lock_not_available
		"57P01", // admin_shutdown
		"57P02", // crash_shutdown
		"57P03", // cannot_connect_now
		"53300", // too_many_connections
		"08000", // connection_exception
		"08003", // connection_does_not_exist
		"08006": // connection_failure
		return true
	default:
		return false
	}
}

func isTransientNetError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func isTransientStoreMessage(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"connection refused",
		"i/o timeout",
		"broken pipe",
		"conn closed",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

func notFound(op, what string) error {
	return domain.Ef(domain.CodeNotFound, nil, "store.%s: %s not found", op, what)
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ---------------------------------------------------------------------------
// Users and identities
// ---------------------------------------------------------------------------

const userColumns = `id, status, timezone, default_currency, locale,
	consent_version, consented_at, created_at, updated_at, deleted_at`

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Status, &u.Timezone, &u.DefaultCurrency, &u.Locale,
		&u.ConsentVersion, &u.ConsentedAt, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateUser inserts a pending user; timezone/currency/locale defaults come
// from the schema (Asia/Ho_Chi_Minh, VND, vi-VN).
func (s *Store) CreateUser(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO users (id) VALUES ($1)`, id)
	if err != nil {
		return internalErr("CreateUser", err)
	}
	return nil
}

// GetUser loads a user by primary key.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, notFound("GetUser", "user")
	}
	if err != nil {
		return nil, internalErr("GetUser", err)
	}
	return u, nil
}

// GetUserByIdentity resolves a provider subject to its local user.
func (s *Store) GetUserByIdentity(ctx context.Context, provider domain.Provider, subject, scope string) (*domain.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT u.id, u.status, u.timezone, u.default_currency, u.locale,
		 u.consent_version, u.consented_at, u.created_at, u.updated_at, u.deleted_at
		 FROM users u
		 JOIN user_identities i ON i.user_id = u.id
		 WHERE i.provider = $1 AND i.provider_subject = $2 AND i.provider_scope = $3`,
		string(provider), subject, scope))
	if isNoRows(err) {
		return nil, notFound("GetUserByIdentity", "identity")
	}
	if err != nil {
		return nil, internalErr("GetUserByIdentity", err)
	}
	return u, nil
}

// UpdateUserPreferences stores the user-facing timezone and default
// currency together. Callers validate IANA/ISO semantics before writing;
// the store still rejects empty values so corrupted settings cannot enter
// through another call site.
func (s *Store) UpdateUserPreferences(ctx context.Context, userID uuid.UUID, timezone, currency string) error {
	timezone = strings.TrimSpace(timezone)
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if timezone == "" || currency == "" {
		return domain.E(domain.CodeValidation, "store.UpdateUserPreferences: timezone and currency are required", nil)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE users
		SET timezone = $2, default_currency = $3, updated_at = now()
		WHERE id = $1 AND status <> 'deleted'`,
		userID, timezone, currency)
	if err != nil {
		return internalErr("UpdateUserPreferences", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("UpdateUserPreferences", "active user")
	}
	return nil
}

// CreateIdentity links a provider subject to a user. The UNIQUE constraint
// on (provider, subject, scope) is the guard against double registration.
func (s *Store) CreateIdentity(ctx context.Context, userID uuid.UUID, provider domain.Provider, subject, scope string) (*domain.UserIdentity, error) {
	id := uuid.New()
	var ident domain.UserIdentity
	err := s.pool.QueryRow(ctx,
		`INSERT INTO user_identities (id, user_id, provider, provider_subject, provider_scope)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, user_id, provider, provider_subject, provider_scope, created_at, last_seen_at`,
		id, userID, string(provider), subject, scope).
		Scan(&ident.ID, &ident.UserID, &ident.Provider, &ident.ProviderSubject, &ident.ProviderScope,
			&ident.CreatedAt, &ident.LastSeenAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, domain.E(domain.CodeConflict, "store.CreateIdentity: identity exists", nil)
		}
		return nil, internalErr("CreateIdentity", err)
	}
	return &ident, nil
}

// CreateUserWithIdentity inserts a pending user and its provider identity in
// one transaction. A unique conflict on the identity rolls back the user row
// so concurrent first contact cannot leave orphans.
func (s *Store) CreateUserWithIdentity(ctx context.Context, userID uuid.UUID, provider domain.Provider, subject, scope string) (*domain.User, error) {
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
			return internalErr("CreateUserWithIdentity", err)
		}
		id := uuid.New()
		_, err := tx.Exec(ctx,
			`INSERT INTO user_identities (id, user_id, provider, provider_subject, provider_scope)
			 VALUES ($1, $2, $3, $4, $5)`,
			id, userID, string(provider), subject, scope)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.E(domain.CodeConflict, "store.CreateUserWithIdentity: identity exists", nil)
			}
			return internalErr("CreateUserWithIdentity", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, userID)
}

// TouchIdentityLastSeen refreshes last_seen_at on every verified webhook.
func (s *Store) TouchIdentityLastSeen(ctx context.Context, provider domain.Provider, subject, scope string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE user_identities SET last_seen_at = now()
		 WHERE provider = $1 AND provider_subject = $2 AND provider_scope = $3`,
		string(provider), subject, scope)
	if err != nil {
		return internalErr("TouchIdentityLastSeen", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("TouchIdentityLastSeen", "identity")
	}
	return nil
}

// SetConsent records PDPD consent and activates the user. Only a pending
// user can consent; anything else is a state conflict.
func (s *Store) SetConsent(ctx context.Context, userID uuid.UUID, version string, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET status = 'active', consent_version = $2, consented_at = $3, updated_at = now()
		 WHERE id = $1 AND status = 'pending'`,
		userID, version, at.UTC())
	if err != nil {
		return internalErr("SetConsent", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.E(domain.CodeConflict, "store.SetConsent: user not pending", nil)
	}
	return nil
}

// SetUserStatus moves a user through the lifecycle; deletion stamps deleted_at.
func (s *Store) SetUserStatus(ctx context.Context, userID uuid.UUID, status domain.UserStatus) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET status = $2, updated_at = now(),
			deleted_at = CASE WHEN $2 = 'deleted' THEN now() ELSE deleted_at END
		 WHERE id = $1`,
		userID, string(status))
	if err != nil {
		return internalErr("SetUserStatus", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("SetUserStatus", "user")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Provider messages (webhook idempotency anchor)
// ---------------------------------------------------------------------------

// ClaimProviderMessageOpts controls optional claim behaviour.
type ClaimProviderMessageOpts struct {
	// Redact, when true, inserts the supplied payload then overwrites
	// raw_payload_json with a sanitized envelope before commit, so a crash
	// cannot leave text/media durable.
	Redact bool
}

// ClaimProviderMessage records and leases an inbound webhook for processing.
// Completed duplicates return claimed=false. Failed messages are reclaimed
// immediately; a received message is reclaimed only after claimLease, which
// recovers a process crash without allowing concurrent handlers to run it.
// On any existing row pm.ID is replaced with the durable original ID so all
// downstream idempotency keys remain stable across provider retries.
func (s *Store) ClaimProviderMessage(ctx context.Context, pm *domain.ProviderMessage, claimLease time.Duration) (claimed bool, err error) {
	return s.ClaimProviderMessageOpts(ctx, pm, claimLease, ClaimProviderMessageOpts{})
}

func (s *Store) ClaimProviderMessageOpts(ctx context.Context, pm *domain.ProviderMessage, claimLease time.Duration, opts ClaimProviderMessageOpts) (claimed bool, err error) {
	if claimLease <= 0 {
		return false, domain.E(domain.CodeValidation, "store.ClaimProviderMessage: claim lease must be positive", nil)
	}
	status := pm.Status
	if status == "" {
		status = domain.MessageReceived
	}
	now := time.Now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, internalErr("ClaimProviderMessage", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, err := scanProviderMessage(tx.QueryRow(ctx,
		`SELECT `+providerMessageColumns+` FROM provider_messages
		 WHERE provider = $1 AND provider_chat_id = $2 AND provider_message_id = $3
		 FOR UPDATE`,
		string(pm.Provider), pm.ProviderChatID, pm.ProviderMessageID))
	if isNoRows(err) {
		pm.ProcessingStartedAt = &now
		_, err = tx.Exec(ctx,
			`INSERT INTO provider_messages
				(id, provider, provider_chat_id, provider_message_id, user_id,
				 event_type, payload_hash, raw_payload_json, received_at, processing_started_at, status)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			pm.ID, string(pm.Provider), pm.ProviderChatID, pm.ProviderMessageID, pm.UserID,
			pm.EventType, pm.PayloadHash, pm.RawPayload, pm.ReceivedAt.UTC(), now, string(status))
		if err != nil {
			return false, internalErr("ClaimProviderMessage", err)
		}
		if opts.Redact {
			if _, err := tx.Exec(ctx,
				`UPDATE provider_messages SET raw_payload_json = '{"redacted":true}'::jsonb WHERE id = $1`,
				pm.ID); err != nil {
				return false, internalErr("ClaimProviderMessage", err)
			}
			pm.RawPayload = []byte(`{"redacted":true}`)
		}
		if err := tx.Commit(ctx); err != nil {
			return false, internalErr("ClaimProviderMessage", err)
		}
		return true, nil
	}
	if err != nil {
		return false, internalErr("ClaimProviderMessage", err)
	}

	pm.ID = existing.ID
	pm.UserID = existing.UserID
	pm.ProcessingStartedAt = existing.ProcessingStartedAt
	pm.ProcessedAt = existing.ProcessedAt
	pm.Status = existing.Status
	if existing.PayloadHash != "" && pm.PayloadHash != "" && existing.PayloadHash != pm.PayloadHash {
		return false, domain.E(domain.CodeValidation, "store.ClaimProviderMessage: payload changed for provider message ID", nil)
	}

	switch existing.Status {
	case domain.MessageProcessed, domain.MessageDuplicate:
		if err := tx.Commit(ctx); err != nil {
			return false, internalErr("ClaimProviderMessage", err)
		}
		return false, nil
	case domain.MessageReceived:
		if existing.ProcessingStartedAt != nil && existing.ProcessingStartedAt.After(now.Add(-claimLease)) {
			return false, domain.E(domain.CodeConflict, "store.ClaimProviderMessage: message is already being processed", nil)
		}
	case domain.MessageFailed:
		// A provider retry owns the next attempt immediately.
	default:
		return false, domain.Ef(domain.CodeValidation, nil,
			"store.ClaimProviderMessage: unsupported status %q", existing.Status)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE provider_messages
		 SET status = 'received', processing_started_at = $2, processed_at = NULL,
		     raw_payload_json = CASE WHEN $3 THEN '{"redacted":true}'::jsonb ELSE raw_payload_json END
		 WHERE id = $1`,
		existing.ID, now, opts.Redact); err != nil {
		return false, internalErr("ClaimProviderMessage", err)
	}
	if opts.Redact {
		pm.RawPayload = []byte(`{"redacted":true}`)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, internalErr("ClaimProviderMessage", err)
	}
	pm.Status = domain.MessageReceived
	pm.ProcessingStartedAt = &now
	pm.ProcessedAt = nil
	return true, nil
}

// SetProviderMessageUser persists the resolved owner. The guard prevents a
// provider-key collision from ever reassigning an inbound message.
func (s *Store) SetProviderMessageUser(ctx context.Context, id, userID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE provider_messages SET user_id = $2
		 WHERE id = $1 AND (user_id IS NULL OR user_id = $2)`,
		id, userID)
	if err != nil {
		return internalErr("SetProviderMessageUser", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.E(domain.CodeConflict, "store.SetProviderMessageUser: message belongs to another user", nil)
	}
	return nil
}

// RedactProviderMessageRaw strips text/media from a claimed inbound payload
// while keeping the idempotency key and original payload_hash.
func (s *Store) RedactProviderMessageRaw(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE provider_messages SET raw_payload_json = '{"redacted":true}'::jsonb WHERE id = $1`,
		id)
	if err != nil {
		return internalErr("RedactProviderMessageRaw", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("RedactProviderMessageRaw", "provider message")
	}
	return nil
}

// MarkProviderMessage sets the terminal processing status; processed stamps
// processed_at.
func (s *Store) MarkProviderMessage(ctx context.Context, id uuid.UUID, status domain.MessageStatus) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE provider_messages SET status = $2,
			processed_at = CASE WHEN $2 = 'processed' THEN now() ELSE processed_at END,
			processing_started_at = NULL
		 WHERE id = $1`,
		id, string(status))
	if err != nil {
		return internalErr("MarkProviderMessage", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("MarkProviderMessage", "provider_message")
	}
	return nil
}

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
		 WHERE (delete_after IS NOT NULL AND delete_after < $1 AND deleted_at IS NULL)
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

// ListRecentTransactions returns the user's newest non-deleted transactions,
// most recent first.
func (s *Store) ListRecentTransactions(ctx context.Context, userID uuid.UUID, limit int) ([]domain.Transaction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+txColumns+` FROM transactions
		 WHERE user_id = $1 AND deleted_at IS NULL
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

// ---------------------------------------------------------------------------
// Extracted fields, predictions, corrections (audit trail)
// ---------------------------------------------------------------------------

// InsertExtractedFields persists OCR/extraction provenance for one receipt.
func (s *Store) InsertExtractedFields(ctx context.Context, fields []domain.ExtractedField) error {
	if len(fields) == 0 {
		return nil
	}
	return postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		for i := range fields {
			f := &fields[i]
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
				return internalErr("InsertExtractedFields", err)
			}
		}
		return nil
	})
}

// InsertPredictions persists what the system guessed, for audit and learning.
func (s *Store) InsertPredictions(ctx context.Context, preds []domain.Prediction) error {
	if len(preds) == 0 {
		return nil
	}
	return postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		for i := range preds {
			p := &preds[i]
			if p.ID == uuid.Nil {
				p.ID = uuid.New()
			}
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
				return internalErr("InsertPredictions", err)
			}
		}
		return nil
	})
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

// ---------------------------------------------------------------------------
// Merchants and aliases
// ---------------------------------------------------------------------------

const merchantColumns = `id, canonical_name, normalised_name, merchant_family, created_at`

func scanMerchant(row pgx.Row) (*domain.Merchant, error) {
	var m domain.Merchant
	err := row.Scan(&m.ID, &m.CanonicalName, &m.NormalisedName, &m.MerchantFamily, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// FindMerchantByNormalisedName matches on the canonical normalised name.
func (s *Store) FindMerchantByNormalisedName(ctx context.Context, key string) (*domain.Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`SELECT `+merchantColumns+` FROM merchants WHERE normalised_name = $1`, key))
	if isNoRows(err) {
		return nil, notFound("FindMerchantByNormalisedName", "merchant")
	}
	if err != nil {
		return nil, internalErr("FindMerchantByNormalisedName", err)
	}
	return m, nil
}

// FindMerchantByAlias matches on any known alias spelling.
func (s *Store) FindMerchantByAlias(ctx context.Context, key string) (*domain.Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`SELECT m.id, m.canonical_name, m.normalised_name, m.merchant_family, m.created_at
		 FROM merchants m
		 JOIN merchant_aliases a ON a.merchant_id = m.id
		 WHERE a.normalised_alias = $1`, key))
	if isNoRows(err) {
		return nil, notFound("FindMerchantByAlias", "merchant")
	}
	if err != nil {
		return nil, internalErr("FindMerchantByAlias", err)
	}
	return m, nil
}

// FindMerchantFuzzy does an escaped ILIKE substring match on normalised
// names and returns the longest match (shortest edit distance proxy).
// CodeNotFound when nothing contains the key.
func (s *Store) FindMerchantFuzzy(ctx context.Context, key string) (*domain.Merchant, error) {
	pattern := "%" + escapeLike(key) + "%"
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`SELECT `+merchantColumns+` FROM merchants
		 WHERE normalised_name ILIKE $1 ESCAPE '\'
		 ORDER BY length(normalised_name) DESC LIMIT 1`,
		pattern))
	if isNoRows(err) {
		return nil, notFound("FindMerchantFuzzy", "merchant")
	}
	if err != nil {
		return nil, internalErr("FindMerchantFuzzy", err)
	}
	return m, nil
}

// escapeLike escapes ILIKE metacharacters; the pattern itself always travels
// as a placeholder, never concatenated into SQL.
func escapeLike(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// CreateMerchant inserts a merchant learned on the fly. Concurrent creation
// of the same normalised name returns the existing row.
func (s *Store) CreateMerchant(ctx context.Context, canonical, key, family string) (*domain.Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`INSERT INTO merchants (id, canonical_name, normalised_name, merchant_family)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (normalised_name) DO UPDATE SET canonical_name = merchants.canonical_name
		 RETURNING `+merchantColumns,
		uuid.New(), canonical, key, family))
	if err != nil {
		return nil, internalErr("CreateMerchant", err)
	}
	return m, nil
}

// AddMerchantAlias attaches a new spelling; duplicates are a no-op.
func (s *Store) AddMerchantAlias(ctx context.Context, merchantID uuid.UUID, alias, key, source string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO merchant_aliases (id, merchant_id, alias, normalised_alias, source)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (normalised_alias) DO NOTHING`,
		uuid.New(), merchantID, alias, key, source)
	if err != nil {
		return internalErr("AddMerchantAlias", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// User merchant rules (learned preferences)
// ---------------------------------------------------------------------------

const ruleColumns = `user_id, merchant_id, preferred_category_id,
	preferred_transaction_type, sample_count, confidence, last_confirmed_at`

func scanRule(row pgx.Row) (*domain.UserMerchantRule, error) {
	var r domain.UserMerchantRule
	err := row.Scan(&r.UserID, &r.MerchantID, &r.PreferredCategoryID,
		&r.PreferredTransactionType, &r.SampleCount, &r.Confidence, &r.LastConfirmedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetUserMerchantRule loads the learned preference; CodeNotFound means the
// user has never confirmed this merchant, which is normal.
func (s *Store) GetUserMerchantRule(ctx context.Context, userID, merchantID uuid.UUID) (*domain.UserMerchantRule, error) {
	r, err := scanRule(s.pool.QueryRow(ctx,
		`SELECT `+ruleColumns+` FROM user_merchant_rules WHERE user_id = $1 AND merchant_id = $2`,
		userID, merchantID))
	if isNoRows(err) {
		return nil, notFound("GetUserMerchantRule", "rule")
	}
	if err != nil {
		return nil, internalErr("GetUserMerchantRule", err)
	}
	return r, nil
}

// UpsertUserMerchantRule inserts or grows the learned rule. SampleCount on
// the argument is a delta: the stored count increases by it, while the
// caller-supplied confidence (computed from the expected new count) and the
// preferred values are replaced.
func (s *Store) UpsertUserMerchantRule(ctx context.Context, rule *domain.UserMerchantRule) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_merchant_rules
			(user_id, merchant_id, preferred_category_id, preferred_transaction_type,
			 sample_count, confidence, last_confirmed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (user_id, merchant_id) DO UPDATE SET
			preferred_category_id = EXCLUDED.preferred_category_id,
			preferred_transaction_type = EXCLUDED.preferred_transaction_type,
			sample_count = user_merchant_rules.sample_count + EXCLUDED.sample_count,
			confidence = EXCLUDED.confidence,
			last_confirmed_at = EXCLUDED.last_confirmed_at`,
		rule.UserID, rule.MerchantID, rule.PreferredCategoryID,
		string(rule.PreferredTransactionType), rule.SampleCount, rule.Confidence,
		rule.LastConfirmedAt.UTC())
	if err != nil {
		return internalErr("UpsertUserMerchantRule", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

const categoryColumns = `id, system_key, display_name, parent_id, is_system, created_at`

func scanCategory(row pgx.Row) (*domain.Category, error) {
	var c domain.Category
	err := row.Scan(&c.ID, &c.SystemKey, &c.DisplayName, &c.ParentID, &c.IsSystem, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListCategories returns the full taxonomy (system seeds + user customs).
func (s *Store) ListCategories(ctx context.Context) ([]domain.Category, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+categoryColumns+` FROM categories ORDER BY system_key`)
	if err != nil {
		return nil, internalErr("ListCategories", err)
	}
	defer rows.Close()
	var out []domain.Category
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, internalErr("ListCategories", err)
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListCategories", err)
	}
	return out, nil
}

// GetCategoryByKey resolves a stable machine key such as "an-uong".
func (s *Store) GetCategoryByKey(ctx context.Context, key string) (*domain.Category, error) {
	c, err := scanCategory(s.pool.QueryRow(ctx,
		`SELECT `+categoryColumns+` FROM categories WHERE system_key = $1`, key))
	if isNoRows(err) {
		return nil, notFound("GetCategoryByKey", "category")
	}
	if err != nil {
		return nil, internalErr("GetCategoryByKey", err)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Usage counters (quota enforcement)
// ---------------------------------------------------------------------------

// IncrementUsage atomically adds delta to a counter, creating it on first
// use, and returns the new count for limit checks.
func (s *Store) IncrementUsage(ctx context.Context, scope, scopeID, period, metric string, delta, limit int64) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO usage_counters (scope, scope_id, period, metric, count, limit_value)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (scope, scope_id, period, metric) DO UPDATE SET
			count = usage_counters.count + EXCLUDED.count,
			limit_value = EXCLUDED.limit_value,
			updated_at = now()
		 RETURNING count`,
		scope, scopeID, period, metric, delta, limit).Scan(&count)
	if err != nil {
		return 0, internalErr("IncrementUsage", err)
	}
	return count, nil
}

// GetUsage reads a counter; a missing counter is 0, not an error.
func (s *Store) GetUsage(ctx context.Context, scope, scopeID, period, metric string) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx,
		`SELECT count FROM usage_counters
		 WHERE scope = $1 AND scope_id = $2 AND period = $3 AND metric = $4`,
		scope, scopeID, period, metric).Scan(&count)
	if isNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, internalErr("GetUsage", err)
	}
	return count, nil
}

// ---------------------------------------------------------------------------
// Scheduled summaries (P4-B03)
// ---------------------------------------------------------------------------

const summaryScheduleColumns = `id, user_id, frequency, delivery_minute,
	provider, provider_chat_id, enabled, next_delivery_at, last_delivered_at,
	created_at, updated_at`

func scanSummarySchedule(row pgx.Row) (*domain.SummarySchedule, error) {
	var p domain.SummarySchedule
	err := row.Scan(&p.ID, &p.UserID, &p.Frequency, &p.DeliveryMinute,
		&p.Provider, &p.ProviderChatID, &p.Enabled, &p.NextDeliveryAt,
		&p.LastDeliveredAt, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// UpsertSummarySchedule enables or updates one frequency for a user. The
// destination chat is refreshed from the command that changed the setting.
func (s *Store) UpsertSummarySchedule(ctx context.Context, p *domain.SummarySchedule) (*domain.SummarySchedule, error) {
	if p.DeliveryMinute < 0 || p.DeliveryMinute >= 24*60 {
		return nil, domain.E(domain.CodeValidation, "store.UpsertSummarySchedule: invalid delivery minute", nil)
	}
	switch p.Frequency {
	case domain.SummaryDaily, domain.SummaryWeekly, domain.SummaryMonthly:
	default:
		return nil, domain.E(domain.CodeValidation, "store.UpsertSummarySchedule: invalid frequency", nil)
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	saved, err := scanSummarySchedule(s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_summary_preferences
			(id, user_id, frequency, delivery_minute, provider, provider_chat_id,
			 enabled, next_delivery_at)
		VALUES ($1, $2, $3, $4, $5, $6, true, $7)
		ON CONFLICT (user_id, frequency)
		DO UPDATE SET delivery_minute = EXCLUDED.delivery_minute,
		              provider = EXCLUDED.provider,
		              provider_chat_id = EXCLUDED.provider_chat_id,
		              enabled = true,
		              next_delivery_at = EXCLUDED.next_delivery_at,
		              updated_at = now()
		RETURNING `+summaryScheduleColumns,
		p.ID, p.UserID, string(p.Frequency), p.DeliveryMinute,
		string(p.Provider), p.ProviderChatID, p.NextDeliveryAt.UTC()))
	if err != nil {
		return nil, internalErr("UpsertSummarySchedule", err)
	}
	return saved, nil
}

// ListSummarySchedules returns all configured frequencies for one user.
func (s *Store) ListSummarySchedules(ctx context.Context, userID uuid.UUID) ([]domain.SummarySchedule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+summaryScheduleColumns+`
		FROM scheduled_summary_preferences
		WHERE user_id = $1
		ORDER BY CASE frequency WHEN 'daily' THEN 1 WHEN 'weekly' THEN 2 ELSE 3 END`,
		userID)
	if err != nil {
		return nil, internalErr("ListSummarySchedules", err)
	}
	defer rows.Close()
	out := []domain.SummarySchedule{}
	for rows.Next() {
		p, err := scanSummarySchedule(rows)
		if err != nil {
			return nil, internalErr("ListSummarySchedules", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListSummarySchedules", err)
	}
	return out, nil
}

// ListDueSummarySchedules returns enabled schedules for active users only.
// Idempotent outbound keys make concurrent scheduler instances safe.
func (s *Store) ListDueSummarySchedules(ctx context.Context, now time.Time, limit int) ([]domain.SummarySchedule, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.user_id, p.frequency, p.delivery_minute,
		       p.provider, p.provider_chat_id, p.enabled, p.next_delivery_at,
		       p.last_delivered_at, p.created_at, p.updated_at
		FROM scheduled_summary_preferences p
		JOIN users u ON u.id = p.user_id
		WHERE p.enabled AND p.next_delivery_at <= $1
		  AND u.status = 'active' AND u.deleted_at IS NULL
		ORDER BY p.next_delivery_at, p.id
		LIMIT $2`,
		now.UTC(), limit)
	if err != nil {
		return nil, internalErr("ListDueSummarySchedules", err)
	}
	defer rows.Close()
	out := []domain.SummarySchedule{}
	for rows.Next() {
		p, err := scanSummarySchedule(rows)
		if err != nil {
			return nil, internalErr("ListDueSummarySchedules", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListDueSummarySchedules", err)
	}
	return out, nil
}

// AdvanceSummarySchedule records a successful enqueue and moves the next
// due instant. expectedDue is an optimistic guard for concurrent runners.
func (s *Store) AdvanceSummarySchedule(ctx context.Context, id uuid.UUID, expectedDue, next, deliveredAt time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE scheduled_summary_preferences
		SET next_delivery_at = $3, last_delivered_at = $4, updated_at = now()
		WHERE id = $1 AND enabled AND next_delivery_at = $2`,
		id, expectedDue.UTC(), next.UTC(), deliveredAt.UTC())
	if err != nil {
		return false, internalErr("AdvanceSummarySchedule", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DisableSummarySchedules opts out one frequency, or all when frequency is
// nil. Rows remain as preference history and can be re-enabled by upsert.
func (s *Store) DisableSummarySchedules(ctx context.Context, userID uuid.UUID, frequency *domain.SummaryFrequency) (int64, error) {
	var value any
	if frequency != nil {
		value = string(*frequency)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE scheduled_summary_preferences
		SET enabled = false, updated_at = now()
		WHERE user_id = $1 AND enabled
		  AND ($2::text IS NULL OR frequency = $2)`,
		userID, value)
	if err != nil {
		return 0, internalErr("DisableSummarySchedules", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Pending actions (chat state machine, one open row per user)
// ---------------------------------------------------------------------------

// GetPendingAction loads the user's open action. An expired action is
// deleted on read and reported as CodeNotFound.
func (s *Store) GetPendingAction(ctx context.Context, userID uuid.UUID) (*domain.PendingAction, error) {
	var p domain.PendingAction
	err := s.pool.QueryRow(ctx,
		`SELECT id, user_id, kind, transaction_id, payload_json, expires_at, created_at
		 FROM pending_actions WHERE user_id = $1`,
		userID).
		Scan(&p.ID, &p.UserID, &p.Kind, &p.TransactionID, &p.Payload, &p.ExpiresAt, &p.CreatedAt)
	if isNoRows(err) {
		return nil, notFound("GetPendingAction", "pending_action")
	}
	if err != nil {
		return nil, internalErr("GetPendingAction", err)
	}
	if p.ExpiresAt.Before(time.Now().UTC()) {
		if _, err := s.pool.Exec(ctx, `DELETE FROM pending_actions WHERE id = $1`, p.ID); err != nil {
			return nil, internalErr("GetPendingAction", err)
		}
		return nil, notFound("GetPendingAction", "pending_action")
	}
	return &p, nil
}

// SetPendingAction replaces the user's open action atomically: one open row
// per user, enforced by the unique index and this delete-then-insert.
func (s *Store) SetPendingAction(ctx context.Context, p *domain.PendingAction) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM pending_actions WHERE user_id = $1`, p.UserID); err != nil {
			return internalErr("SetPendingAction", err)
		}
		payload := p.Payload
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO pending_actions (id, user_id, kind, transaction_id, payload_json, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			p.ID, p.UserID, string(p.Kind), p.TransactionID, payload, p.ExpiresAt.UTC())
		if err != nil {
			return internalErr("SetPendingAction", err)
		}
		return nil
	})
}

// ClearPendingAction removes any open action (idempotent).
func (s *Store) ClearPendingAction(ctx context.Context, userID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM pending_actions WHERE user_id = $1`, userID)
	if err != nil {
		return internalErr("ClearPendingAction", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Outbound outbox (notification worker is the only sender)
// ---------------------------------------------------------------------------

const outboundColumns = `id, user_id, provider, provider_chat_id, idempotency_key,
	body, ephemeral, status, attempts, last_error, attempted_at, sent_at,
	provider_message_id, created_at`

func scanOutbound(row pgx.Row) (*domain.OutboundRecord, error) {
	var o domain.OutboundRecord
	err := row.Scan(&o.ID, &o.UserID, &o.Provider, &o.ProviderChatID, &o.IdempotencyKey,
		&o.Body, &o.Ephemeral, &o.Status, &o.Attempts, &o.LastError, &o.AttemptedAt,
		&o.SentAt, &o.ProviderMessageID, &o.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// EnqueueOutbound adds an outbox row; a replayed idempotency key is a no-op
// (created=false), which gives exactly-once enqueue intent. Provider delivery
// uses explicit sending/ambiguous states because Zalo cannot dedupe requests.
func (s *Store) EnqueueOutbound(ctx context.Context, rec *domain.OutboundRecord) (created bool, err error) {
	status := rec.Status
	if status == "" {
		status = domain.OutboundQueued
	}
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO outbound_messages
			(id, user_id, provider, provider_chat_id, idempotency_key, body, ephemeral, status)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (idempotency_key) DO NOTHING`,
		rec.ID, rec.UserID, string(rec.Provider), rec.ProviderChatID,
		rec.IdempotencyKey, rec.Body, rec.Ephemeral, string(status))
	if err != nil {
		return false, internalErr("EnqueueOutbound", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetOutboundByIdempotencyKey loads the canonical row behind a replayed
// reply, allowing the producer to repair a missing queue job after a crash.
func (s *Store) GetOutboundByIdempotencyKey(ctx context.Context, key string) (*domain.OutboundRecord, error) {
	o, err := scanOutbound(s.pool.QueryRow(ctx,
		`SELECT `+outboundColumns+` FROM outbound_messages WHERE idempotency_key = $1`, key))
	if isNoRows(err) {
		return nil, notFound("GetOutboundByIdempotencyKey", "outbound")
	}
	if err != nil {
		return nil, internalErr("GetOutboundByIdempotencyKey", err)
	}
	return o, nil
}

// GetOutbound loads an outbox row by primary key.
func (s *Store) GetOutbound(ctx context.Context, id uuid.UUID) (*domain.OutboundRecord, error) {
	o, err := scanOutbound(s.pool.QueryRow(ctx,
		`SELECT `+outboundColumns+` FROM outbound_messages WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, notFound("GetOutbound", "outbound")
	}
	if err != nil {
		return nil, internalErr("GetOutbound", err)
	}
	return o, nil
}

// BeginOutboundAttempt atomically reserves one provider-call slot and moves
// a queued row to sending. The attempt metric enforces the hard provider cap;
// successful sends are counted separately by CompleteOutboundSend.
func (s *Store) BeginOutboundAttempt(ctx context.Context, id uuid.UUID, period string, limit int64) (*domain.OutboundRecord, bool, int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, 0, internalErr("BeginOutboundAttempt", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rec, err := scanOutbound(tx.QueryRow(ctx,
		`SELECT `+outboundColumns+` FROM outbound_messages WHERE id = $1 FOR UPDATE`, id))
	if isNoRows(err) {
		return nil, false, 0, notFound("BeginOutboundAttempt", "outbound")
	}
	if err != nil {
		return nil, false, 0, internalErr("BeginOutboundAttempt", err)
	}
	if rec.Status != domain.OutboundQueued {
		if err := tx.Commit(ctx); err != nil {
			return nil, false, 0, internalErr("BeginOutboundAttempt", err)
		}
		return rec, false, 0, nil
	}

	var count int64
	if limit > 0 {
		err = tx.QueryRow(ctx, `
			INSERT INTO usage_counters (scope, scope_id, period, metric, count, limit_value)
			VALUES ('global', '', $1, 'zalo_message_attempts', 1, $2)
			ON CONFLICT (scope, scope_id, period, metric)
			DO UPDATE SET count = usage_counters.count + 1,
			              limit_value = EXCLUDED.limit_value, updated_at = now()
			WHERE usage_counters.count < $2
			RETURNING count`, period, limit).Scan(&count)
		if isNoRows(err) {
			if err := tx.QueryRow(ctx, `
				SELECT count FROM usage_counters
				WHERE scope = 'global' AND scope_id = '' AND period = $1
				  AND metric = 'zalo_message_attempts'`, period).Scan(&count); err != nil {
				return nil, false, 0, internalErr("BeginOutboundAttempt", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE outbound_messages
				SET status = 'suppressed', last_error = 'monthly message attempt quota'
				WHERE id = $1`, id); err != nil {
				return nil, false, 0, internalErr("BeginOutboundAttempt", err)
			}
			rec.Status = domain.OutboundSuppressed
			rec.LastError = "monthly message attempt quota"
			if err := tx.Commit(ctx); err != nil {
				return nil, false, 0, internalErr("BeginOutboundAttempt", err)
			}
			return rec, false, count, nil
		}
		if err != nil {
			return nil, false, 0, internalErr("BeginOutboundAttempt", err)
		}
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO usage_counters (scope, scope_id, period, metric, count, limit_value)
			VALUES ('global', '', $1, 'zalo_message_attempts', 1, 0)
			ON CONFLICT (scope, scope_id, period, metric)
			DO UPDATE SET count = usage_counters.count + 1, updated_at = now()
			RETURNING count`, period).Scan(&count)
		if err != nil {
			return nil, false, 0, internalErr("BeginOutboundAttempt", err)
		}
	}

	rec, err = scanOutbound(tx.QueryRow(ctx, `
		UPDATE outbound_messages
		SET status = 'sending', attempts = attempts + 1,
		    attempted_at = now(), last_error = ''
		WHERE id = $1
		RETURNING `+outboundColumns, id))
	if err != nil {
		return nil, false, 0, internalErr("BeginOutboundAttempt", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, 0, internalErr("BeginOutboundAttempt", err)
	}
	return rec, true, count, nil
}

// CompleteOutboundSend records a provider receipt and increments the success
// metric in the same transaction. The sending guard makes the quota charge
// exactly once even if completion is retried. Ephemeral deletion receipts
// are purged with their queue job instead of retained.
func (s *Store) CompleteOutboundSend(ctx context.Context, outboundID, jobID uuid.UUID, period string, limit int64, providerMessageID string) (int64, error) {
	var successCount int64
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var ephemeral bool
		err := tx.QueryRow(ctx, `
			UPDATE outbound_messages
			SET status = 'sent', sent_at = now(), provider_message_id = $2, last_error = ''
			WHERE id = $1 AND status = 'sending'
			RETURNING ephemeral`, outboundID, providerMessageID).Scan(&ephemeral)
		if isNoRows(err) {
			return domain.E(domain.CodeConflict, "store.CompleteOutboundSend: outbound is not sending", nil)
		}
		if err != nil {
			return internalErr("CompleteOutboundSend", err)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO usage_counters (scope, scope_id, period, metric, count, limit_value)
			VALUES ('global', '', $1, 'zalo_messages_sent', 1, $2)
			ON CONFLICT (scope, scope_id, period, metric)
			DO UPDATE SET count = usage_counters.count + 1,
			              limit_value = EXCLUDED.limit_value, updated_at = now()
			RETURNING count`, period, limit).Scan(&successCount); err != nil {
			return internalErr("CompleteOutboundSend", err)
		}
		if ephemeral {
			if _, err := tx.Exec(ctx, `DELETE FROM outbound_messages WHERE id = $1`, outboundID); err != nil {
				return internalErr("CompleteOutboundSend", err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM queue_jobs WHERE id = $1`, jobID); err != nil {
				return internalErr("CompleteOutboundSend", err)
			}
		}
		return nil
	})
	return successCount, err
}

// MarkOutboundStatus sets the worker outcome; sent carries sent_at.
func (s *Store) MarkOutboundStatus(ctx context.Context, id uuid.UUID, status domain.OutboundStatus, lastError string, sentAt *time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE outbound_messages SET status = $2, last_error = $3, sent_at = $4
		 WHERE id = $1`,
		id, string(status), lastError, sentAt)
	if err != nil {
		return internalErr("MarkOutboundStatus", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("MarkOutboundStatus", "outbound")
	}
	return nil
}

// PurgeOutboundAndJob removes an ephemeral message (or an orphaned job)
// once no further delivery attempt is allowed.
func (s *Store) PurgeOutboundAndJob(ctx context.Context, outboundID, jobID uuid.UUID) error {
	return postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM outbound_messages WHERE id = $1`, outboundID); err != nil {
			return internalErr("PurgeOutboundAndJob", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM queue_jobs WHERE id = $1`, jobID); err != nil {
			return internalErr("PurgeOutboundAndJob", err)
		}
		return nil
	})
}

// DeleteQueueJob removes an orphan whose source row was purged by account
// deletion. Worker Ack is intentionally idempotent if the row is gone.
func (s *Store) DeleteQueueJob(ctx context.Context, jobID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM queue_jobs WHERE id = $1`, jobID)
	if err != nil {
		return internalErr("DeleteQueueJob", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Lookups added for the runtime wiring (bot handler + receipt worker)
// ---------------------------------------------------------------------------

const providerMessageColumns = `id, provider, provider_chat_id, provider_message_id,
	user_id, event_type, payload_hash, raw_payload_json, received_at,
	processing_started_at, processed_at, delete_after, status`

func scanProviderMessage(row pgx.Row) (*domain.ProviderMessage, error) {
	var pm domain.ProviderMessage
	var provider string
	err := row.Scan(&pm.ID, &provider, &pm.ProviderChatID, &pm.ProviderMessageID,
		&pm.UserID, &pm.EventType, &pm.PayloadHash, &pm.RawPayload, &pm.ReceivedAt,
		&pm.ProcessingStartedAt, &pm.ProcessedAt, &pm.DeleteAfter, &pm.Status)
	if err != nil {
		return nil, err
	}
	pm.Provider = domain.Provider(provider)
	return &pm, nil
}

// GetProviderMessage loads one inbound webhook record, e.g. so the receipt
// worker can recover the media URL from the retained raw payload.
func (s *Store) GetProviderMessage(ctx context.Context, id uuid.UUID) (*domain.ProviderMessage, error) {
	pm, err := scanProviderMessage(s.pool.QueryRow(ctx,
		`SELECT `+providerMessageColumns+` FROM provider_messages WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, notFound("GetProviderMessage", "provider message")
	}
	if err != nil {
		return nil, internalErr("GetProviderMessage", err)
	}
	return pm, nil
}

// DeleteExpiredProviderMessageTombstones removes sanitized account-deletion
// idempotency anchors after their short retry window.
func (s *Store) DeleteExpiredProviderMessageTombstones(ctx context.Context, now time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM provider_messages
		 WHERE delete_after IS NOT NULL AND delete_after <= $1`, now.UTC())
	if err != nil {
		return 0, internalErr("DeleteExpiredProviderMessageTombstones", err)
	}
	return tag.RowsAffected(), nil
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
