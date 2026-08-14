//go:build integration

package insight

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/store"
)

// Fixed category UUIDs from db/migrations/0002_seed_categories.up.sql.
const (
	catAnUong   = "11111111-1111-1111-1111-111111111101"
	catThucPham = "11111111-1111-1111-1111-111111111102"
	catMuaSam   = "11111111-1111-1111-1111-111111111105"
	catHoanTien = "11111111-1111-1111-1111-111111111111"
)

var (
	user1 = uuid.MustParse("10000000-0000-0000-0000-000000000001")
	user2 = uuid.MustParse("10000000-0000-0000-0000-000000000002")

	// ThisWeek for 2026-07-19 in Asia/Ho_Chi_Minh: local Mon Jul 13 through
	// Sun Jul 19.
	weekPeriod = Period{
		Start: time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC),
	}
	lastWeekPeriod = Period{
		Start: time.Date(2026, 7, 5, 17, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 12, 17, 0, 0, 0, time.UTC),
	}
)

type seedTx struct {
	id         string
	userID     uuid.UUID
	txType     string
	merchant   string
	amount     int64
	currency   string
	occurredAt string // RFC3339
	categoryID string // "" -> NULL
	status     string
	deletedAt  string // "" -> NULL
}

func seedUser(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, status, timezone, default_currency, locale,
			consent_version, consented_at)
		VALUES ($1, 'active', 'Asia/Ho_Chi_Minh', 'VND', 'vi-VN', 'v1', now())`, id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func seedTransaction(t *testing.T, pool *pgxpool.Pool, tx seedTx) {
	t.Helper()
	occurred, err := time.Parse(time.RFC3339, tx.occurredAt)
	if err != nil {
		t.Fatalf("parse occurredAt: %v", err)
	}
	var categoryID *uuid.UUID
	if tx.categoryID != "" {
		c := uuid.MustParse(tx.categoryID)
		categoryID = &c
	}
	var deletedAt *time.Time
	if tx.deletedAt != "" {
		d, err := time.Parse(time.RFC3339, tx.deletedAt)
		if err != nil {
			t.Fatalf("parse deletedAt: %v", err)
		}
		deletedAt = &d
	}
	_, err = pool.Exec(context.Background(), `
		INSERT INTO transactions (id, user_id, type, merchant_name, amount_minor,
			currency, occurred_at, category_id, status, source, deleted_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'receipt', $10)`,
		uuid.MustParse(tx.id), tx.userID, tx.txType, tx.merchant, tx.amount,
		tx.currency, occurred.UTC(), categoryID, tx.status, deletedAt)
	if err != nil {
		t.Fatalf("seed transaction %s: %v", tx.id, err)
	}
}

// seedTwoUsers installs the shared fixture: user1 with transactions across
// types/currencies/categories (plus deleted, draft, boundary and
// outside-period rows) and user2 with a single in-period expense.
func seedTwoUsers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedUser(t, pool, user1)
	seedUser(t, pool, user2)

	txs := []seedTx{
		{"20000000-0000-0000-0000-000000000001", user1, "expense", "Highlands Coffee", 100000, "VND", "2026-07-13T02:00:00Z", catAnUong, "confirmed", ""},
		{"20000000-0000-0000-0000-000000000002", user1, "expense", "Co.opmart", 250000, "VND", "2026-07-14T03:00:00Z", catThucPham, "amended", ""},
		{"20000000-0000-0000-0000-000000000003", user1, "expense", "Cafe Oz", 1234, "AUD", "2026-07-14T10:00:00Z", catAnUong, "confirmed", ""},
		{"20000000-0000-0000-0000-000000000004", user1, "refund", "Shopee", 50000, "VND", "2026-07-15T01:00:00Z", catHoanTien, "confirmed", ""},
		{"20000000-0000-0000-0000-000000000005", user1, "expense", "Street Vendor", 75000, "VND", "2026-07-15T12:00:00Z", "", "confirmed", ""},
		{"20000000-0000-0000-0000-000000000006", user1, "expense", "Big Shop", 999999, "VND", "2026-07-16T05:00:00Z", catMuaSam, "confirmed", ""},
		{"20000000-0000-0000-0000-000000000007", user1, "income", "Salary", 5000000, "VND", "2026-07-16T08:00:00Z", "", "confirmed", ""},
		// Deleted rows never contribute.
		{"20000000-0000-0000-0000-000000000008", user1, "expense", "Deleted Place", 777777, "VND", "2026-07-14T06:00:00Z", catAnUong, "confirmed", "2026-07-17T00:00:00Z"},
		// Unconfirmed rows never contribute.
		{"20000000-0000-0000-0000-000000000009", user1, "expense", "Draft Place", 888888, "VND", "2026-07-14T07:00:00Z", catAnUong, "draft", ""},
		// Half-open bounds: exactly at Start is in, exactly at End is out.
		{"20000000-0000-0000-0000-00000000000a", user1, "expense", "Boundary In", 1, "VND", "2026-07-12T17:00:00Z", "", "confirmed", ""},
		{"20000000-0000-0000-0000-00000000000b", user1, "expense", "Boundary Out", 2, "VND", "2026-07-19T17:00:00Z", "", "confirmed", ""},
		// Last week: adjacent period must not double-count the boundary row.
		{"20000000-0000-0000-0000-00000000000c", user1, "expense", "Last Week Shop", 5000, "VND", "2026-07-08T10:00:00Z", catAnUong, "confirmed", ""},
		// Other user in-period.
		{"20000000-0000-0000-0000-000000000101", user2, "expense", "User Two Mart", 424242, "VND", "2026-07-14T05:00:00Z", catThucPham, "confirmed", ""},
	}
	for _, tx := range txs {
		seedTransaction(t, pool, tx)
	}
}

// TestPersistStoresEvidenceBackedRow verifies P4-B02: a generated summary
// lands in insights with payload + evidence, and regenerating the same
// period upserts instead of duplicating.
func TestPersistStoresEvidenceBackedRow(t *testing.T) {
	pool := pgtest.NewPool(t)
	seedTwoUsers(t, pool)
	svc := NewService(store.New(pool))
	ctx := context.Background()

	sum, err := svc.Summarise(ctx, user1, weekPeriod)
	if err != nil {
		t.Fatalf("summarise: %v", err)
	}
	if err := svc.Persist(ctx, user1, TypeWeekly, weekPeriod, sum); err != nil {
		t.Fatalf("persist: %v", err)
	}
	// Regeneration upserts: same user/type/period, still exactly one row.
	if err := svc.Persist(ctx, user1, TypeWeekly, weekPeriod, sum); err != nil {
		t.Fatalf("persist again: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)::int FROM insights
		WHERE user_id = $1 AND insight_type = $2 AND period_start = $3`,
		user1, TypeWeekly, weekPeriod.Start).Scan(&count); err != nil {
		t.Fatalf("count insights: %v", err)
	}
	if count != 1 {
		t.Fatalf("insight rows for period = %d, want 1", count)
	}

	var payload, evidence []byte
	var version string
	if err := pool.QueryRow(ctx, `
		SELECT payload_json, evidence_json, generator_version FROM insights
		WHERE user_id = $1 AND insight_type = $2 AND period_start = $3`,
		user1, TypeWeekly, weekPeriod.Start).Scan(&payload, &evidence, &version); err != nil {
		t.Fatalf("load insight: %v", err)
	}
	if version != GeneratorVersion {
		t.Errorf("generator_version = %q, want %q", version, GeneratorVersion)
	}

	var got Summary
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got.TxCount != sum.TxCount || !reflect.DeepEqual(got.CurrencyTotals, sum.CurrencyTotals) {
		t.Errorf("payload summary mismatch: got %+v want %+v", got.CurrencyTotals, sum.CurrencyTotals)
	}

	var ev struct {
		Method         string   `json:"method"`
		TransactionIDs []string `json:"transaction_ids"`
	}
	if err := json.Unmarshal(evidence, &ev); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	if ev.Method != EvidenceMethod {
		t.Errorf("evidence method = %q, want %q", ev.Method, EvidenceMethod)
	}
	// user1 has 8 confirmed/amended, non-deleted transactions inside
	// weekPeriod (draft, deleted, boundary-out and last-week rows excluded).
	if len(ev.TransactionIDs) != 8 {
		t.Errorf("evidence lists %d transactions, want 8", len(ev.TransactionIDs))
	}
}

func TestSummariseIntegration(t *testing.T) {
	pool := pgtest.NewPool(t)
	seedTwoUsers(t, pool)
	svc := NewService(store.New(pool))

	t.Run("user1 this week", func(t *testing.T) {
		sum, err := svc.Summarise(context.Background(), user1, weekPeriod)
		if err != nil {
			t.Fatalf("Summarise: %v", err)
		}

		wantCurrency := map[string]int64{"VND": 1425000, "AUD": 1234}
		if !reflect.DeepEqual(sum.CurrencyTotals, wantCurrency) {
			t.Errorf("CurrencyTotals = %v, want %v", sum.CurrencyTotals, wantCurrency)
		}
		wantRefund := map[string]int64{"VND": 50000}
		if !reflect.DeepEqual(sum.RefundTotals, wantRefund) {
			t.Errorf("RefundTotals = %v, want %v", sum.RefundTotals, wantRefund)
		}

		// Hand-computed per (category, currency) expectation; never mixes
		// currencies and reconciles exactly with CurrencyTotals.
		wantCats := []CategoryTotal{
			{CategoryKey: "an-uong", DisplayName: "Ăn uống", Currency: "AUD", Minor: 1234, Count: 1},
			{CategoryKey: "mua-sam", DisplayName: "Mua sắm", Currency: "VND", Minor: 999999, Count: 1},
			{CategoryKey: "thuc-pham", DisplayName: "Thực phẩm", Currency: "VND", Minor: 250000, Count: 1},
			{CategoryKey: "an-uong", DisplayName: "Ăn uống", Currency: "VND", Minor: 100000, Count: 1},
			{CategoryKey: "khac", DisplayName: "Khác", Currency: "VND", Minor: 75001, Count: 2},
		}
		if !reflect.DeepEqual(sum.ByCategory, wantCats) {
			t.Errorf("ByCategory = %+v,\nwant %+v", sum.ByCategory, wantCats)
		}
		var vndByCat, audByCat int64
		for _, ct := range sum.ByCategory {
			switch ct.Currency {
			case "AUD":
				audByCat += ct.Minor
			case "VND":
				vndByCat += ct.Minor
			}
		}
		if vndByCat != wantCurrency["VND"] || audByCat != wantCurrency["AUD"] {
			t.Errorf("category sums (%d VND, %d AUD) do not reconcile with currency totals %v",
				vndByCat, audByCat, wantCurrency)
		}

		wantMerchants := []MerchantTotal{
			{Name: "Cafe Oz", Currency: "AUD", Minor: 1234, Count: 1},
			{Name: "Big Shop", Currency: "VND", Minor: 999999, Count: 1},
			{Name: "Co.opmart", Currency: "VND", Minor: 250000, Count: 1},
			{Name: "Highlands Coffee", Currency: "VND", Minor: 100000, Count: 1},
			{Name: "Street Vendor", Currency: "VND", Minor: 75000, Count: 1},
			{Name: "Boundary In", Currency: "VND", Minor: 1, Count: 1},
		}
		if !reflect.DeepEqual(sum.ByMerchant, wantMerchants) {
			t.Errorf("ByMerchant = %+v,\nwant %+v", sum.ByMerchant, wantMerchants)
		}

		if sum.TxCount != 8 {
			t.Errorf("TxCount = %d, want 8 (6 expenses + 1 refund + 1 income)", sum.TxCount)
		}

		wantLargest := []LargestTx{
			{
				Merchant: "Cafe Oz", Minor: 1234, Currency: "AUD",
				OccurredAt: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC),
			},
			{
				Merchant: "Big Shop", Minor: 999999, Currency: "VND",
				OccurredAt: time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC),
			},
		}
		if !reflect.DeepEqual(sum.LargestByCurrency, wantLargest) {
			t.Errorf("LargestByCurrency = %+v, want %+v", sum.LargestByCurrency, wantLargest)
		}

		// 7 local days; expenses on Jul 13, 14, 15, 16 -> 3 no-spend days.
		if sum.NoSpendDays != 3 {
			t.Errorf("NoSpendDays = %d, want 3", sum.NoSpendDays)
		}
	})

	t.Run("user1 last week no double count", func(t *testing.T) {
		sum, err := svc.Summarise(context.Background(), user1, lastWeekPeriod)
		if err != nil {
			t.Fatalf("Summarise: %v", err)
		}
		if got := sum.CurrencyTotals["VND"]; got != 5000 {
			t.Errorf("last week VND = %d, want 5000", got)
		}
		if sum.TxCount != 1 {
			t.Errorf("last week TxCount = %d, want 1", sum.TxCount)
		}
		if sum.NoSpendDays != 6 {
			t.Errorf("last week NoSpendDays = %d, want 6", sum.NoSpendDays)
		}
	})

	t.Run("user2 isolation", func(t *testing.T) {
		sum, err := svc.Summarise(context.Background(), user2, weekPeriod)
		if err != nil {
			t.Fatalf("Summarise: %v", err)
		}
		if got := sum.CurrencyTotals["VND"]; got != 424242 {
			t.Errorf("user2 VND = %d, want 424242", got)
		}
		if len(sum.CurrencyTotals) != 1 || sum.TxCount != 1 {
			t.Errorf("user2 summary = %+v, want single VND expense", sum)
		}
		if len(sum.RefundTotals) != 0 {
			t.Errorf("user2 RefundTotals = %v, want empty", sum.RefundTotals)
		}
		if sum.NoSpendDays != 6 {
			t.Errorf("user2 NoSpendDays = %d, want 6", sum.NoSpendDays)
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		_, err := svc.Summarise(context.Background(), uuid.New(), weekPeriod)
		if !domain.IsCode(err, domain.CodeNotFound) {
			t.Errorf("err = %v, want CodeNotFound", err)
		}
	})
}

func TestRecordIntegration(t *testing.T) {
	pool := pgtest.NewPool(t)
	seedTwoUsers(t, pool)
	svc := NewService(store.New(pool))

	sum, err := svc.Summarise(context.Background(), user1, weekPeriod)
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if err := svc.Persist(context.Background(), user1, "weekly", weekPeriod, sum); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	var (
		insightType      string
		periodStart      time.Time
		periodEnd        time.Time
		payload          []byte
		evidence         []byte
		generatorVersion string
		status           string
		createdAt        time.Time
	)
	err = pool.QueryRow(context.Background(), `
		SELECT insight_type, period_start, period_end, payload_json,
		       evidence_json, generator_version, status, created_at
		FROM insights WHERE user_id = $1`, user1).
		Scan(&insightType, &periodStart, &periodEnd, &payload, &evidence,
			&generatorVersion, &status, &createdAt)
	if err != nil {
		t.Fatalf("read insight row: %v", err)
	}

	if insightType != "weekly" {
		t.Errorf("insight_type = %q, want weekly", insightType)
	}
	if !periodStart.Equal(weekPeriod.Start) || !periodEnd.Equal(weekPeriod.End) {
		t.Errorf("period = [%s, %s), want [%s, %s)",
			periodStart, periodEnd, weekPeriod.Start, weekPeriod.End)
	}
	if generatorVersion != GeneratorVersion {
		t.Errorf("generator_version = %q, want %q", generatorVersion, GeneratorVersion)
	}
	if status != "ready" {
		t.Errorf("status = %q, want ready", status)
	}
	if createdAt.IsZero() {
		t.Error("created_at is zero")
	}

	var gotSummary Summary
	if err := json.Unmarshal(payload, &gotSummary); err != nil {
		t.Fatalf("payload_json unmarshal: %v", err)
	}
	if gotSummary.TxCount != sum.TxCount || gotSummary.NoSpendDays != sum.NoSpendDays ||
		!reflect.DeepEqual(gotSummary.CurrencyTotals, sum.CurrencyTotals) {
		t.Errorf("payload numbers %+v do not match summary %+v", gotSummary, sum)
	}

	var ev struct {
		Method         string      `json:"method"`
		TransactionIDs []uuid.UUID `json:"transaction_ids"`
	}
	if err := json.Unmarshal(evidence, &ev); err != nil {
		t.Fatalf("evidence_json unmarshal: %v", err)
	}
	if ev.Method != EvidenceMethod {
		t.Errorf("evidence method = %q, want %q", ev.Method, EvidenceMethod)
	}
	wantIDs := map[uuid.UUID]bool{}
	for _, s := range []string{
		"20000000-0000-0000-0000-000000000001",
		"20000000-0000-0000-0000-000000000002",
		"20000000-0000-0000-0000-000000000003",
		"20000000-0000-0000-0000-000000000004",
		"20000000-0000-0000-0000-000000000005",
		"20000000-0000-0000-0000-000000000006",
		"20000000-0000-0000-0000-000000000007",
		"20000000-0000-0000-0000-00000000000a",
	} {
		wantIDs[uuid.MustParse(s)] = true
	}
	if len(ev.TransactionIDs) != len(wantIDs) {
		t.Fatalf("evidence ids = %v, want %d ids", ev.TransactionIDs, len(wantIDs))
	}
	for _, id := range ev.TransactionIDs {
		if !wantIDs[id] {
			t.Errorf("unexpected evidence id %s", id)
		}
	}
}
