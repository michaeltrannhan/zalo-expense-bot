package insight

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/clock"
)

const (
	// GeneratorVersion identifies the aggregation code behind an insights
	// row; bump it whenever Summary semantics change.
	GeneratorVersion = "insight-v2"
	// EvidenceMethod records how the evidence transaction set was chosen.
	EvidenceMethod = "sql-aggregate-v1"
)

// Insight row types written to insights.insight_type.
const (
	TypeDaily   = "daily"
	TypeWeekly  = "weekly"
	TypeMonthly = "monthly"
)

// CategoryTotal aggregates expense minor units for one category. A category
// with expenses in several currencies yields one entry per currency, so
// amounts are never summed across currencies.
type CategoryTotal struct {
	CategoryKey string `json:"category_key"`
	DisplayName string `json:"display_name"`
	Currency    string `json:"currency"`
	Minor       int64  `json:"minor"`
	Count       int    `json:"count"`
}

// MerchantTotal aggregates expense minor units for one merchant display
// name, one entry per (merchant, currency) pair.
type MerchantTotal struct {
	Name     string `json:"name"`
	Currency string `json:"currency"`
	Minor    int64  `json:"minor"`
	Count    int    `json:"count"`
}

// LargestTx is the biggest recorded expense within one currency in the period.
type LargestTx struct {
	Merchant   string    `json:"merchant"`
	Minor      int64     `json:"minor"`
	Currency   string    `json:"currency"`
	OccurredAt time.Time `json:"occurred_at"`
}

// Summary aggregates one user's recorded transactions over a Period. Only
// confirmed/amended, non-deleted transactions contribute. Expense money and
// refund money are tracked separately and never summed across currencies;
// rendering (Vietnamese copy) lives in the conversation layer.
type Summary struct {
	CurrencyTotals    map[string]int64 `json:"currency_totals"` // expense only, per currency
	RefundTotals      map[string]int64 `json:"refund_totals"`   // refunds only, per currency
	ByCategory        []CategoryTotal  `json:"by_category"`     // expense only, per currency
	ByMerchant        []MerchantTotal  `json:"by_merchant"`     // expense only, per currency
	TxCount           int              `json:"tx_count"`        // all in-scope transactions, any type
	LargestByCurrency []LargestTx      `json:"largest_by_currency"`
	NoSpendDays       int              `json:"no_spend_days"` // period days (user tz) with zero expense txs
}

// Service computes and persists insight summaries.
type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

// NewService builds a Service on pool; clk timestamps generated rows.
func NewService(pool *pgxpool.Pool, clk clock.Clock) *Service {
	return &Service{pool: pool, clock: clk}
}

// Persist stores a generated summary as an evidence-backed insight row
// (P4-B02): the payload is the full Summary, the evidence lists every
// contributing transaction ID plus the aggregation method. One row per
// (user, type, period_start) — regenerating a period upserts, so repeated
// summary commands never duplicate records.
func (s *Service) Persist(ctx context.Context, userID uuid.UUID, insightType string, p Period, sum Summary) error {
	payload, err := json.Marshal(sum)
	if err != nil {
		return domain.E(domain.CodeInternal, "marshal insight payload", err)
	}
	ids, err := s.evidenceIDs(ctx, userID, p)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(map[string]any{
		"method":          EvidenceMethod,
		"transaction_ids": ids,
		"period":          map[string]time.Time{"start": p.Start, "end": p.End},
	})
	if err != nil {
		return domain.E(domain.CodeInternal, "marshal insight evidence", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO insights (id, user_id, insight_type, period_start, period_end,
		                      payload_json, evidence_json, generator_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_id, insight_type, period_start)
		DO UPDATE SET period_end = EXCLUDED.period_end,
		              payload_json = EXCLUDED.payload_json,
		              evidence_json = EXCLUDED.evidence_json,
		              generator_version = EXCLUDED.generator_version,
		              created_at = now()`,
		uuid.New(), userID, insightType, p.Start, p.End, payload, evidence, GeneratorVersion)
	if err != nil {
		return domain.E(domain.CodeTransient, "persist insight", err)
	}
	return nil
}

// Summarise aggregates the user's recorded transactions in [p.Start, p.End).
func (s *Service) Summarise(ctx context.Context, userID uuid.UUID, p Period) (Summary, error) {
	if !p.End.After(p.Start) {
		return Summary{}, domain.E(domain.CodeValidation, "period end must be after period start", nil)
	}
	loc, err := s.userLocation(ctx, userID)
	if err != nil {
		return Summary{}, err
	}

	sum := Summary{
		CurrencyTotals:    map[string]int64{},
		RefundTotals:      map[string]int64{},
		ByCategory:        []CategoryTotal{},
		ByMerchant:        []MerchantTotal{},
		LargestByCurrency: []LargestTx{},
	}

	// One GROUP BY over the scoped set feeds the per-currency totals and
	// the overall transaction count.
	totals, err := s.pool.Query(ctx, `
		SELECT type, currency, SUM(amount_minor)::bigint, COUNT(*)::int
		FROM transactions
		WHERE user_id = $1
		  AND status IN ('confirmed','amended')
		  AND deleted_at IS NULL
		  AND occurred_at >= $2 AND occurred_at < $3
		GROUP BY type, currency`, userID, p.Start, p.End)
	if err != nil {
		return Summary{}, domain.E(domain.CodeTransient, "query summary totals", err)
	}
	defer totals.Close()
	for totals.Next() {
		var txType, currency string
		var minor int64
		var count int
		if err := totals.Scan(&txType, &currency, &minor, &count); err != nil {
			return Summary{}, domain.E(domain.CodeTransient, "scan summary totals", err)
		}
		sum.TxCount += count
		switch domain.TxType(txType) {
		case domain.TxExpense:
			sum.CurrencyTotals[currency] = minor
		case domain.TxRefund:
			sum.RefundTotals[currency] = minor
		}
	}
	if err := totals.Err(); err != nil {
		return Summary{}, domain.E(domain.CodeTransient, "iterate summary totals", err)
	}

	if err := s.fillCategories(ctx, userID, p, &sum); err != nil {
		return Summary{}, err
	}
	if err := s.fillMerchants(ctx, userID, p, &sum); err != nil {
		return Summary{}, err
	}
	if err := s.fillLargest(ctx, userID, p, &sum); err != nil {
		return Summary{}, err
	}
	if err := s.fillNoSpendDays(ctx, userID, p, loc, &sum); err != nil {
		return Summary{}, err
	}
	return sum, nil
}

// Record persists sum as an insights row. The evidence transaction IDs are
// re-selected from the same scope Summarise uses, so evidence always matches
// the recorded transactions behind the numbers.
func (s *Service) Record(ctx context.Context, userID uuid.UUID, insightType string, p Period, sum Summary) error {
	if insightType == "" {
		return domain.E(domain.CodeValidation, "insight type is required", nil)
	}
	if !p.End.After(p.Start) {
		return domain.E(domain.CodeValidation, "period end must be after period start", nil)
	}
	ids, err := s.evidenceIDs(ctx, userID, p)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(sum)
	if err != nil {
		return domain.E(domain.CodeInternal, "marshal insight payload", err)
	}
	evidence, err := json.Marshal(struct {
		Method         string      `json:"method"`
		TransactionIDs []uuid.UUID `json:"transaction_ids"`
	}{Method: EvidenceMethod, TransactionIDs: ids})
	if err != nil {
		return domain.E(domain.CodeInternal, "marshal insight evidence", err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO insights (id, user_id, insight_type, period_start, period_end,
			payload_json, evidence_json, generator_version, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'ready', $9)`,
		uuid.New(), userID, insightType, p.Start, p.End,
		payload, evidence, GeneratorVersion, s.clock.Now().UTC())
	if err != nil {
		return domain.E(domain.CodeTransient, "insert insight", err)
	}
	return nil
}

func (s *Service) userLocation(ctx context.Context, userID uuid.UUID) (*time.Location, error) {
	var tz string
	err := s.pool.QueryRow(ctx, `SELECT timezone FROM users WHERE id = $1`, userID).Scan(&tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.Ef(domain.CodeNotFound, nil, "user %s not found", userID)
	}
	if err != nil {
		return nil, domain.E(domain.CodeTransient, "load user timezone", err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, domain.Ef(domain.CodeInternal, err, "invalid stored timezone %q", tz)
	}
	return loc, nil
}

func (s *Service) fillCategories(ctx context.Context, userID uuid.UUID, p Period, sum *Summary) error {
	// Grouped by (category, currency): never sum minor units across
	// currencies. NULL categories fold into the 'khac' system bucket.
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
		ORDER BY t.currency, 4 DESC, 1 ASC`, userID, p.Start, p.End)
	if err != nil {
		return domain.E(domain.CodeTransient, "query category totals", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ct CategoryTotal
		if err := rows.Scan(&ct.CategoryKey, &ct.DisplayName, &ct.Currency, &ct.Minor, &ct.Count); err != nil {
			return domain.E(domain.CodeTransient, "scan category total", err)
		}
		sum.ByCategory = append(sum.ByCategory, ct)
	}
	if err := rows.Err(); err != nil {
		return domain.E(domain.CodeTransient, "iterate category totals", err)
	}
	return nil
}

func (s *Service) fillMerchants(ctx context.Context, userID uuid.UUID, p Period, sum *Summary) error {
	rows, err := s.pool.Query(ctx, `
		SELECT merchant_name, currency, SUM(amount_minor)::bigint, COUNT(*)::int
		FROM transactions
		WHERE user_id = $1
		  AND status IN ('confirmed','amended')
		  AND deleted_at IS NULL
		  AND type = 'expense'
		  AND occurred_at >= $2 AND occurred_at < $3
		GROUP BY merchant_name, currency
		ORDER BY currency, 3 DESC, 1 ASC`, userID, p.Start, p.End)
	if err != nil {
		return domain.E(domain.CodeTransient, "query merchant totals", err)
	}
	defer rows.Close()
	for rows.Next() {
		var mt MerchantTotal
		if err := rows.Scan(&mt.Name, &mt.Currency, &mt.Minor, &mt.Count); err != nil {
			return domain.E(domain.CodeTransient, "scan merchant total", err)
		}
		sum.ByMerchant = append(sum.ByMerchant, mt)
	}
	if err := rows.Err(); err != nil {
		return domain.E(domain.CodeTransient, "iterate merchant totals", err)
	}
	return nil
}

func (s *Service) fillLargest(ctx context.Context, userID uuid.UUID, p Period, sum *Summary) error {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (currency)
		       merchant_name, amount_minor, currency, occurred_at
		FROM transactions
		WHERE user_id = $1
		  AND status IN ('confirmed','amended')
		  AND deleted_at IS NULL
		  AND type = 'expense'
		  AND occurred_at >= $2 AND occurred_at < $3
		ORDER BY currency, amount_minor DESC, occurred_at ASC, id ASC`,
		userID, p.Start, p.End)
	if err != nil {
		return domain.E(domain.CodeTransient, "query largest transaction", err)
	}
	defer rows.Close()
	for rows.Next() {
		var largest LargestTx
		if err := rows.Scan(&largest.Merchant, &largest.Minor, &largest.Currency, &largest.OccurredAt); err != nil {
			return domain.E(domain.CodeTransient, "scan largest transaction", err)
		}
		largest.OccurredAt = largest.OccurredAt.UTC()
		sum.LargestByCurrency = append(sum.LargestByCurrency, largest)
	}
	if err := rows.Err(); err != nil {
		return domain.E(domain.CodeTransient, "iterate largest transactions", err)
	}
	return nil
}

func (s *Service) fillNoSpendDays(ctx context.Context, userID uuid.UUID, p Period, loc *time.Location, sum *Summary) error {
	var spendDays int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT (occurred_at AT TIME ZONE $4)::date)::int
		FROM transactions
		WHERE user_id = $1
		  AND status IN ('confirmed','amended')
		  AND deleted_at IS NULL
		  AND type = 'expense'
		  AND occurred_at >= $2 AND occurred_at < $3`,
		userID, p.Start, p.End, loc.String()).Scan(&spendDays)
	if err != nil {
		return domain.E(domain.CodeTransient, "query spend days", err)
	}
	sum.NoSpendDays = localDays(p, loc) - spendDays
	return nil
}

// evidenceIDs lists the in-scope transaction IDs in deterministic order.
func (s *Service) evidenceIDs(ctx context.Context, userID uuid.UUID, p Period) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id
		FROM transactions
		WHERE user_id = $1
		  AND status IN ('confirmed','amended')
		  AND deleted_at IS NULL
		  AND occurred_at >= $2 AND occurred_at < $3
		ORDER BY id ASC`, userID, p.Start, p.End)
	if err != nil {
		return nil, domain.E(domain.CodeTransient, "query evidence transactions", err)
	}
	defer rows.Close()
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, domain.E(domain.CodeTransient, "scan evidence transaction", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, domain.E(domain.CodeTransient, "iterate evidence transactions", err)
	}
	return ids, nil
}

// localDays counts the calendar days the period covers in loc. Period
// bounds are local midnights, so this walks whole days only.
func localDays(p Period, loc *time.Location) int {
	end := p.End.In(loc)
	days := 0
	for d := p.Start.In(loc); d.Before(end); d = d.AddDate(0, 0, 1) {
		days++
	}
	return days
}
