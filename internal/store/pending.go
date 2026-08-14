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
