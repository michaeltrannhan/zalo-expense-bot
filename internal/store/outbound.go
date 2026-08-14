package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/postgres"
)

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
