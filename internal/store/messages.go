package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zl-expese-bot/internal/domain"
)

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
