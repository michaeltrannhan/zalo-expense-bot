package mock

import (
	"time"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
)

// corpusEntry is one synthetic receipt. Timestamps are fixed so extraction
// is byte-deterministic; test/fixtures/receipts/ground_truth.json mirrors
// this table exactly (enforced by TestCorpusMatchesGroundTruth).
type corpusEntry struct {
	id          string
	canonical   string // canonical merchant display name
	merchantRaw string // raw text as it appears on the receipt
	merchantKey string
	totalMinor  int64
	totalRaw    string
	currency    string
	currencyRaw string
	occurredAt  time.Time
	occurredRaw string
	categoryKey string
	txType      domain.TxType
	typeHintRaw string // non-empty only for refund/transfer receipts
	conf        map[string]float64
	warnings    []string
	unsupported bool
}

func at(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

var corpus = []corpusEntry{
	{
		id: "coopmart-clean", canonical: "Co.opmart",
		merchantRaw: "CO.OPMART NGUYỄN TRÃI", merchantKey: "coopmart",
		totalMinor: 325000, totalRaw: "325.000 ₫", currency: "VND", currencyRaw: "₫",
		occurredAt: at(2026, time.July, 15, 9, 24), occurredRaw: "15/07/2026 09:24",
		categoryKey: "thuc-pham", txType: domain.TxExpense,
		conf: map[string]float64{"merchant": 0.98, "total_minor": 0.99, "currency": 0.99, "occurred_at": 0.97, "category": 0.95},
	},
	{
		id: "highlands-clean", canonical: "Highlands Coffee",
		merchantRaw: "HIGHLANDS COFFEE LÊ LAI", merchantKey: "highlands coffee",
		totalMinor: 65000, totalRaw: "65.000đ", currency: "VND", currencyRaw: "đ",
		occurredAt: at(2026, time.July, 16, 14, 5), occurredRaw: "16/07/2026 14:05",
		categoryKey: "an-uong", txType: domain.TxExpense,
		conf: map[string]float64{"merchant": 0.97, "total_minor": 0.98, "currency": 0.99, "occurred_at": 0.96, "category": 0.93},
	},
	{
		id: "petrolimex-clean", canonical: "Petrolimex",
		merchantRaw: "PETROLIMEX SÀI GÒN", merchantKey: "petrolimex",
		totalMinor: 500000, totalRaw: "500.000đ", currency: "VND", currencyRaw: "đ",
		occurredAt: at(2026, time.July, 17, 8, 12), occurredRaw: "17/07/2026 08:12",
		categoryKey: "di-lai", txType: domain.TxExpense,
		conf: map[string]float64{"merchant": 0.96, "total_minor": 0.97, "currency": 0.99, "occurred_at": 0.95, "category": 0.90},
	},
	{
		// Total below ConfTotalPrefill: the confirmation card must flag ⚠️.
		id: "guardian-low-total", canonical: "Guardian",
		merchantRaw: "GUARDIAN BẾN THÀNH", merchantKey: "guardian",
		totalMinor: 142000, totalRaw: "142.000đ", currency: "VND", currencyRaw: "đ",
		occurredAt: at(2026, time.July, 14, 19, 41), occurredRaw: "14/07/2026 19:41",
		categoryKey: "suc-khoe", txType: domain.TxExpense,
		conf: map[string]float64{"merchant": 0.94, "total_minor": 0.72, "currency": 0.98, "occurred_at": 0.93, "category": 0.88},
	},
	{
		// Refund: never silent — type_hint always surfaces for confirmation.
		id: "shopee-refund", canonical: "Shopee",
		merchantRaw: "SHOPEE VN", merchantKey: "shopee",
		totalMinor: 89000, totalRaw: "89.000đ", currency: "VND", currencyRaw: "đ",
		occurredAt: at(2026, time.July, 13, 11, 2), occurredRaw: "13/07/2026 11:02",
		categoryKey: "hoan-tien", txType: domain.TxRefund, typeHintRaw: "HOAN TIEN",
		conf: map[string]float64{"merchant": 0.95, "total_minor": 0.96, "currency": 0.99, "occurred_at": 0.94, "category": 0.97, "type_hint": 0.99},
	},
	{
		id: "vietcombank-transfer", canonical: "Vietcombank",
		merchantRaw: "Vietcombank", merchantKey: "vietcombank",
		totalMinor: 1000000, totalRaw: "1.000.000đ", currency: "VND", currencyRaw: "đ",
		occurredAt: at(2026, time.July, 12, 16, 30), occurredRaw: "12/07/2026 16:30",
		categoryKey: "chuyen-khoan", txType: domain.TxTransfer, typeHintRaw: "CHUYEN KHOAN",
		conf: map[string]float64{"merchant": 0.90, "total_minor": 0.95, "currency": 0.99, "occurred_at": 0.92, "category": 0.96, "type_hint": 0.98},
	},
	{
		id: "woolworths-aud", canonical: "Woolworths",
		merchantRaw: "Woolworths Metro", merchantKey: "woolworths",
		totalMinor: 1890, totalRaw: "A$18.90", currency: "AUD", currencyRaw: "A$",
		occurredAt: at(2026, time.March, 12, 10, 45), occurredRaw: "12/03/2026",
		categoryKey: "thuc-pham", txType: domain.TxExpense,
		conf: map[string]float64{"merchant": 0.91, "total_minor": 0.93, "currency": 0.97, "occurred_at": 0.90, "category": 0.86},
	},
	{
		id: "coffeehouse-multi-total", canonical: "The Coffee House",
		merchantRaw: "THE COFFEE HOUSE", merchantKey: "the coffee house",
		totalMinor: 87000, totalRaw: "87.000đ", currency: "VND", currencyRaw: "đ",
		occurredAt: at(2026, time.July, 11, 7, 55), occurredRaw: "11/07/2026 07:55",
		categoryKey: "an-uong", txType: domain.TxExpense,
		conf:     map[string]float64{"merchant": 0.93, "total_minor": 0.88, "currency": 0.99, "occurred_at": 0.91, "category": 0.84},
		warnings: []string{"multiple_totals_detected"},
	},
	{
		id: "grab-mid", canonical: "Grab",
		merchantRaw: "GRAB BIKE", merchantKey: "grab",
		totalMinor: 52000, totalRaw: "52.000đ", currency: "VND", currencyRaw: "đ",
		occurredAt: at(2026, time.July, 10, 21, 18), occurredRaw: "10/07/2026 21:18",
		categoryKey: "di-lai", txType: domain.TxExpense,
		conf: map[string]float64{"merchant": 0.86, "total_minor": 0.89, "currency": 0.98, "occurred_at": 0.88, "category": 0.85},
	},
	{
		// Not a receipt at all: Extract must fail with CodeUnsupported.
		id: "not-a-receipt", unsupported: true,
	},
}

// result builds the deterministic ExtractionResult for an entry.
func (e corpusEntry) result(name, version string) events.ExtractionResult {
	fields := map[string]events.FieldValue{
		"merchant":    {Raw: e.merchantRaw, Normalised: e.merchantKey, Confidence: e.conf["merchant"]},
		"total_minor": {Raw: e.totalRaw, Normalised: e.totalMinor, Confidence: e.conf["total_minor"]},
		"currency":    {Raw: e.currencyRaw, Normalised: e.currency, Confidence: e.conf["currency"]},
		"occurred_at": {Raw: e.occurredRaw, Normalised: e.occurredAt.Format(time.RFC3339), Confidence: e.conf["occurred_at"]},
		"category":    {Raw: e.categoryKey, Normalised: e.categoryKey, Confidence: e.conf["category"]},
	}
	if e.typeHintRaw != "" {
		fields["type_hint"] = events.FieldValue{
			Raw: e.typeHintRaw, Normalised: string(e.txType), Confidence: e.conf["type_hint"],
		}
	}
	return events.ExtractionResult{
		SchemaVersion: events.SchemaV1,
		Extractor:     events.ExtractorInfo{Name: name, Version: version},
		Fields:        fields,
		Warnings:      e.warnings,
	}
}
