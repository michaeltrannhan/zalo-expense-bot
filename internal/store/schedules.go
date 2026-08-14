package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zl-expese-bot/internal/domain"
)

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
