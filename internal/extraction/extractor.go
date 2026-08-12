// Package extraction defines the extractor contract (plan §10.4). The MVP
// ships a deterministic mock (internal/extraction/mock); a Textract adapter
// can implement this same interface later without touching the pipeline.
package extraction

import (
	"context"
	"io"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
)

// Input describes the receipt image to analyse.
type Input struct {
	ReceiptID   string
	SHA256      string
	ContentType string
}

// Extractor turns receipt bytes into structured fields with confidence.
type Extractor interface {
	// Name and Version are persisted on every extracted field.
	Name() string
	Version() string
	// Extract must be deterministic for identical bytes. It returns
	// *domain.Error with CodeUnsupported when the image is not a receipt
	// and CodeTransient for retryable downstream failures.
	Extract(ctx context.Context, r io.Reader, in Input) (events.ExtractionResult, error)
}

// Confidence policy thresholds (plan P3-C02). Refund/transfer type is NEVER
// accepted silently regardless of confidence; totals always require user
// confirmation before a transaction becomes final.
const (
	ConfTotalPrefill    = 0.95
	ConfCurrencyPrefill = 0.95
	ConfDatePrefill     = 0.90
	ConfMerchantPrefill = 0.80
	ConfMerchantReject  = 0.60
	ConfCategoryPrefill = 0.85
)

// FieldNeedsFlag reports whether a field should be visually flagged as
// uncertain in the confirmation card (⚠️).
func FieldNeedsFlag(field string, confidence float64) bool {
	switch field {
	case "total_minor":
		return confidence < ConfTotalPrefill
	case "currency":
		return confidence < ConfCurrencyPrefill
	case "occurred_at":
		return confidence < ConfDatePrefill
	case "merchant":
		return confidence < ConfMerchantReject
	case "category":
		return confidence < ConfCategoryPrefill
	}
	return false
}

// TypeNeedsConfirmation is the dangerous-error guard: refunds and transfers
// are never silently accepted (plan §4).
func TypeNeedsConfirmation(t domain.TxType) bool {
	return t == domain.TxRefund || t == domain.TxTransfer || t == domain.TxIncome
}
