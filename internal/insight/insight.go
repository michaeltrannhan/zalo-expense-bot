package insight

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/store"
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
	st *store.Store
}

// NewService builds a Service on st.
func NewService(st *store.Store) *Service {
	return &Service{st: st}
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
	ids, err := s.st.ListInsightEvidenceIDs(ctx, userID, p.Start, p.End)
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
	return s.st.UpsertInsight(ctx, userID, insightType, p.Start, p.End, payload, evidence, GeneratorVersion)
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

	totals, err := s.st.ListInsightPeriodTotals(ctx, userID, p.Start, p.End)
	if err != nil {
		return Summary{}, err
	}
	for _, row := range totals {
		sum.TxCount += row.Count
		switch domain.TxType(row.Type) {
		case domain.TxExpense:
			sum.CurrencyTotals[row.Currency] = row.Minor
		case domain.TxRefund:
			sum.RefundTotals[row.Currency] = row.Minor
		}
	}

	cats, err := s.st.ListInsightCategoryTotals(ctx, userID, p.Start, p.End)
	if err != nil {
		return Summary{}, err
	}
	for _, row := range cats {
		sum.ByCategory = append(sum.ByCategory, CategoryTotal{
			CategoryKey: row.CategoryKey, DisplayName: row.DisplayName,
			Currency: row.Currency, Minor: row.Minor, Count: row.Count,
		})
	}

	merchants, err := s.st.ListInsightMerchantTotals(ctx, userID, p.Start, p.End)
	if err != nil {
		return Summary{}, err
	}
	for _, row := range merchants {
		sum.ByMerchant = append(sum.ByMerchant, MerchantTotal{
			Name: row.Name, Currency: row.Currency, Minor: row.Minor, Count: row.Count,
		})
	}

	largest, err := s.st.ListInsightLargestByCurrency(ctx, userID, p.Start, p.End)
	if err != nil {
		return Summary{}, err
	}
	for _, row := range largest {
		sum.LargestByCurrency = append(sum.LargestByCurrency, LargestTx{
			Merchant: row.Merchant, Minor: row.Minor, Currency: row.Currency, OccurredAt: row.OccurredAt,
		})
	}

	spendDays, err := s.st.CountInsightExpenseDays(ctx, userID, p.Start, p.End, loc.String())
	if err != nil {
		return Summary{}, err
	}
	sum.NoSpendDays = localDays(p, loc) - spendDays
	return sum, nil
}

func (s *Service) userLocation(ctx context.Context, userID uuid.UUID) (*time.Location, error) {
	u, err := s.st.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(u.Timezone)
	if err != nil {
		return nil, domain.Ef(domain.CodeInternal, err, "invalid stored timezone %q", u.Timezone)
	}
	return loc, nil
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
