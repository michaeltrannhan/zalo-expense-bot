package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

const recordedTx = `
		user_id = $1
		AND status IN ('confirmed','amended')
		AND deleted_at IS NULL
		AND occurred_at >= $2 AND occurred_at < $3`

// UpsertInsight inserts or replaces one (user, type, period_start) insight row.
func (s *Store) UpsertInsight(ctx context.Context, userID uuid.UUID, insightType string, periodStart, periodEnd time.Time, payload, evidence []byte, generatorVersion string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO insights (id, user_id, insight_type, period_start, period_end,
		                      payload_json, evidence_json, generator_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_id, insight_type, period_start)
		DO UPDATE SET period_end = EXCLUDED.period_end,
		              payload_json = EXCLUDED.payload_json,
		              evidence_json = EXCLUDED.evidence_json,
		              generator_version = EXCLUDED.generator_version,
		              created_at = now()`,
		uuid.New(), userID, insightType, periodStart, periodEnd, payload, evidence, generatorVersion)
	if err != nil {
		return internalErr("UpsertInsight", err)
	}
	return nil
}

// InsightTotalsRow is one (type, currency) spend/refund aggregate.
type InsightTotalsRow struct {
	Type     string
	Currency string
	Minor    int64
	Count    int
}

// ListInsightPeriodTotals groups recorded transactions by type and currency.
func (s *Store) ListInsightPeriodTotals(ctx context.Context, userID uuid.UUID, start, end time.Time) ([]InsightTotalsRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT type, currency, SUM(amount_minor)::bigint, COUNT(*)::int
		FROM transactions
		WHERE `+recordedTx+`
		GROUP BY type, currency`, userID, start, end)
	if err != nil {
		return nil, internalErr("ListInsightPeriodTotals", err)
	}
	defer rows.Close()
	var out []InsightTotalsRow
	for rows.Next() {
		var row InsightTotalsRow
		if err := rows.Scan(&row.Type, &row.Currency, &row.Minor, &row.Count); err != nil {
			return nil, internalErr("ListInsightPeriodTotals", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListInsightPeriodTotals", err)
	}
	return out, nil
}

// InsightCategoryRow is one (category, currency) expense aggregate.
type InsightCategoryRow struct {
	CategoryKey string
	DisplayName string
	Currency    string
	Minor       int64
	Count       int
}

// ListInsightCategoryTotals groups expense totals by category and currency.
func (s *Store) ListInsightCategoryTotals(ctx context.Context, userID uuid.UUID, start, end time.Time) ([]InsightCategoryRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(c.system_key, 'khac'), COALESCE(c.display_name, 'Khác'),
		       t.currency, SUM(t.amount_minor)::bigint, COUNT(*)::int
		FROM transactions t
		LEFT JOIN categories c ON c.id = t.category_id
		WHERE t.user_id = $1
		  AND t.status IN ('confirmed','amended')
		  AND t.deleted_at IS NULL
		  AND t.type = 'expense'
		  AND t.occurred_at >= $2 AND t.occurred_at < $3
		GROUP BY 1, 2, t.currency
		ORDER BY t.currency, 4 DESC, 1 ASC`, userID, start, end)
	if err != nil {
		return nil, internalErr("ListInsightCategoryTotals", err)
	}
	defer rows.Close()
	var out []InsightCategoryRow
	for rows.Next() {
		var row InsightCategoryRow
		if err := rows.Scan(&row.CategoryKey, &row.DisplayName, &row.Currency, &row.Minor, &row.Count); err != nil {
			return nil, internalErr("ListInsightCategoryTotals", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListInsightCategoryTotals", err)
	}
	return out, nil
}

// InsightMerchantRow is one (merchant, currency) expense aggregate.
type InsightMerchantRow struct {
	Name     string
	Currency string
	Minor    int64
	Count    int
}

// ListInsightMerchantTotals groups expense totals by merchant and currency.
func (s *Store) ListInsightMerchantTotals(ctx context.Context, userID uuid.UUID, start, end time.Time) ([]InsightMerchantRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT merchant_name, currency, SUM(amount_minor)::bigint, COUNT(*)::int
		FROM transactions
		WHERE `+recordedTx+`
		  AND type = 'expense'
		GROUP BY merchant_name, currency
		ORDER BY currency, 3 DESC, 1 ASC`, userID, start, end)
	if err != nil {
		return nil, internalErr("ListInsightMerchantTotals", err)
	}
	defer rows.Close()
	var out []InsightMerchantRow
	for rows.Next() {
		var row InsightMerchantRow
		if err := rows.Scan(&row.Name, &row.Currency, &row.Minor, &row.Count); err != nil {
			return nil, internalErr("ListInsightMerchantTotals", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListInsightMerchantTotals", err)
	}
	return out, nil
}

// InsightLargestRow is the biggest expense in one currency.
type InsightLargestRow struct {
	Merchant   string
	Minor      int64
	Currency   string
	OccurredAt time.Time
}

// ListInsightLargestByCurrency returns the largest expense per currency.
func (s *Store) ListInsightLargestByCurrency(ctx context.Context, userID uuid.UUID, start, end time.Time) ([]InsightLargestRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (currency)
		       merchant_name, amount_minor, currency, occurred_at
		FROM transactions
		WHERE `+recordedTx+`
		  AND type = 'expense'
		ORDER BY currency, amount_minor DESC, occurred_at ASC, id ASC`,
		userID, start, end)
	if err != nil {
		return nil, internalErr("ListInsightLargestByCurrency", err)
	}
	defer rows.Close()
	var out []InsightLargestRow
	for rows.Next() {
		var row InsightLargestRow
		if err := rows.Scan(&row.Merchant, &row.Minor, &row.Currency, &row.OccurredAt); err != nil {
			return nil, internalErr("ListInsightLargestByCurrency", err)
		}
		row.OccurredAt = row.OccurredAt.UTC()
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListInsightLargestByCurrency", err)
	}
	return out, nil
}

// CountInsightExpenseDays counts distinct local calendar days with an expense.
func (s *Store) CountInsightExpenseDays(ctx context.Context, userID uuid.UUID, start, end time.Time, tz string) (int, error) {
	var spendDays int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT (occurred_at AT TIME ZONE $4)::date)::int
		FROM transactions
		WHERE `+recordedTx+`
		  AND type = 'expense'`,
		userID, start, end, tz).Scan(&spendDays)
	if err != nil {
		return 0, internalErr("CountInsightExpenseDays", err)
	}
	return spendDays, nil
}

// ListInsightEvidenceIDs lists in-scope transaction IDs in deterministic order.
func (s *Store) ListInsightEvidenceIDs(ctx context.Context, userID uuid.UUID, start, end time.Time) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id
		FROM transactions
		WHERE `+recordedTx+`
		ORDER BY id ASC`, userID, start, end)
	if err != nil {
		return nil, internalErr("ListInsightEvidenceIDs", err)
	}
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, internalErr("ListInsightEvidenceIDs", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListInsightEvidenceIDs", err)
	}
	return ids, nil
}
