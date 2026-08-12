// Package textract implements extraction.Extractor over Amazon Textract
// AnalyzeExpense (plan P3-B01): receipt-specialised OCR with per-field
// confidences. The adapter is pure — image-in, ExtractionResult-out — with
// all retries classified by domain code: throttling and 5xx are transient,
// bad documents are permanent, access problems are forbidden.
package textract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/textract"
	"github.com/aws/aws-sdk-go-v2/service/textract/types"
	"github.com/aws/smithy-go"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
	"zl-expese-bot/internal/extraction/normalise"
)

// maxDocBytes is the AnalyzeExpense synchronous payload limit.
const maxDocBytes = 10 << 20 // 10 MiB

// API is the slice of the Textract client the extractor needs (fakeable).
type API interface {
	AnalyzeExpense(ctx context.Context, in *textract.AnalyzeExpenseInput, optFns ...func(*textract.Options)) (*textract.AnalyzeExpenseOutput, error)
}

// Extractor implements extraction.Extractor via AnalyzeExpense.
type Extractor struct {
	client  API
	timeout time.Duration
}

var _ extraction.Extractor = (*Extractor)(nil)

// New builds the adapter; calls are bounded by a 12s timeout.
func New(client API) *Extractor {
	return &Extractor{client: client, timeout: 12 * time.Second}
}

func (e *Extractor) Name() string    { return "textract-analyze-expense" }
func (e *Extractor) Version() string { return "v1" }

// Extract reads the image, calls AnalyzeExpense and maps the first expense
// document onto the §10.4 contract. An image Textract finds no expense in
// is CodeUnsupported ("not a receipt"), mirroring the mock.
func (e *Extractor) Extract(ctx context.Context, r io.Reader, _ extraction.Input) (events.ExtractionResult, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxDocBytes+1))
	if err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "reading receipt bytes", err)
	}
	if len(data) == 0 {
		return events.ExtractionResult{}, domain.E(domain.CodeValidation, "empty image", nil)
	}
	if len(data) > maxDocBytes {
		return events.ExtractionResult{}, domain.Ef(domain.CodeValidation, nil, "image exceeds %d bytes", maxDocBytes)
	}

	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	out, err := e.client.AnalyzeExpense(ctx, &textract.AnalyzeExpenseInput{
		Document: &types.Document{Bytes: data},
	})
	if err != nil {
		return events.ExtractionResult{}, classify(err)
	}
	if len(out.ExpenseDocuments) == 0 {
		return events.ExtractionResult{}, domain.E(domain.CodeUnsupported, "image is not a receipt", nil)
	}
	return mapDocument(out.ExpenseDocuments[0], e.Name(), e.Version()), nil
}

// mapDocument converts one ExpenseDocument. Every confidence is rescaled
// 0-100 → 0-1; only fields Textract actually found are emitted, so missing
// data surfaces as low/no confidence downstream instead of invented values.
func mapDocument(doc types.ExpenseDocument, name, version string) events.ExtractionResult {
	res := events.ExtractionResult{
		SchemaVersion: events.SchemaV1,
		Extractor:     events.ExtractorInfo{Name: name, Version: version},
		Fields:        map[string]events.FieldValue{},
		LineItems:     []events.LineItem{},
		Warnings:      []string{},
	}

	var totals []rawField
	for _, f := range doc.SummaryFields {
		typeText := aws.ToString(f.Type.Text)
		value := detectionText(f.ValueDetection)
		conf := detectionConf(f.ValueDetection, f.Type)
		switch typeText {
		case "VENDOR_NAME":
			if _, done := res.Fields["merchant"]; done || value == "" {
				continue
			}
			_, key := normalise.NormaliseMerchant(value)
			res.Fields["merchant"] = events.FieldValue{Raw: value, Normalised: key, Confidence: conf}
		case "TOTAL":
			if value != "" {
				totals = append(totals, rawField{raw: value, conf: conf})
			}
		case "INVOICE_RECEIPT_DATE":
			if _, done := res.Fields["occurred_at"]; done || value == "" {
				continue
			}
			if t, ok := parseTextractDate(value); ok {
				res.Fields["occurred_at"] = events.FieldValue{
					Raw: value, Normalised: t.UTC().Format(time.RFC3339), Confidence: conf,
				}
			}
		}
	}
	mapTotal(&res, totals)
	mapLineItems(&res, doc.LineItemGroups)
	return res
}

// rawField is one detected value with its rescaled confidence.
type rawField struct {
	raw  string
	conf float64
}

// mapTotal parses the TOTAL summary field(s) into total_minor + currency.
// Distinct multiple totals earn the multiple_totals_detected warning (the
// confirmation card then flags the field instead of silently choosing).
func mapTotal(res *events.ExtractionResult, totals []rawField) {
	seen := map[int64]bool{}
	for _, tf := range totals {
		minor, currency, err := normalise.ParseAmount(tf.raw, "")
		if err != nil {
			continue
		}
		seen[minor] = true
		if _, done := res.Fields["total_minor"]; !done {
			res.Fields["total_minor"] = events.FieldValue{Raw: tf.raw, Normalised: minor, Confidence: tf.conf}
			res.Fields["currency"] = events.FieldValue{Raw: tf.raw, Normalised: currency, Confidence: tf.conf}
		}
	}
	if len(seen) > 1 {
		res.Warnings = append(res.Warnings, "multiple_totals_detected")
	}
}

// mapLineItems converts itemised rows best-effort: unparseable rows are
// skipped, because line items are advisory — the total drives the draft.
func mapLineItems(res *events.ExtractionResult, groups []types.LineItemGroup) {
	for _, g := range groups {
		for _, item := range g.LineItems {
			var li events.LineItem
			for _, f := range item.LineItemExpenseFields {
				value := detectionText(f.ValueDetection)
				switch aws.ToString(f.Type.Text) {
				case "ITEM":
					li.Name = value
				case "QUANTITY":
					var q int
					if _, err := fmt.Sscanf(value, "%d", &q); err == nil && q > 0 {
						li.Quantity = q
					}
				case "PRICE":
					if minor, _, err := normalise.ParseAmount(value, ""); err == nil {
						li.AmountMinor = minor
					}
				}
			}
			if li.Name != "" || li.AmountMinor != 0 {
				res.LineItems = append(res.LineItems, li)
			}
		}
	}
}

// detectionText reads a detection's text safely.
func detectionText(d *types.ExpenseDetection) string {
	if d == nil {
		return ""
	}
	return strings.TrimSpace(aws.ToString(d.Text))
}

// detectionConf rescales 0-100 confidence to 0-1, falling back to the
// field-type confidence when the value detection carries none.
func detectionConf(d *types.ExpenseDetection, t *types.ExpenseType) float64 {
	if d != nil && d.Confidence != nil {
		return float64(*d.Confidence) / 100
	}
	if t != nil && t.Confidence != nil {
		return float64(*t.Confidence) / 100
	}
	return 0
}

// dateLayouts tried before the chat-format parser (which handles
// dd/mm/yyyy variants in UTC).
var dateLayouts = []string{
	"2006-01-02",
	"02/01/2006",
	"01/02/2006",
	"02-01-2006",
	"Jan 2, 2006",
	"January 2, 2006",
	"2 Jan 2006",
	"02 Jan 2006",
}

// parseTextractDate parses a Textract date string to UTC. Receipt dates
// carry no timezone, so the result is a UTC calendar timestamp.
func parseTextractDate(s string) (time.Time, bool) {
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	if t, err := normalise.ParseDate(s, time.UTC); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// classify maps service failures onto domain retry classes: throttling and
// 5xx retry with backoff, bad documents are permanent, access problems are
// forbidden, and the OCR page quota surfaces distinctly.
func classify(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return domain.E(domain.CodeTransient, "textract call timed out", err)
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "ThrottlingException", "ProvisionedThroughputExceededException",
			"InternalServerError", "ServiceUnavailableException", "LimitExceededException":
			return domain.E(domain.CodeTransient, "textract throttled or unavailable", err)
		case "UnsupportedDocumentException", "InvalidParameterException",
			"DocumentTooLargeException", "BadDocumentException", "InvalidS3ObjectException":
			return domain.E(domain.CodeValidation, "textract rejected document", err)
		case "AccessDeniedException":
			return domain.E(domain.CodeForbidden, "textract access denied", err)
		case "HumanLoopQuotaExceededException":
			return domain.E(domain.CodeQuota, "textract quota exceeded", err)
		}
	}
	return domain.E(domain.CodeTransient, "textract call failed", err)
}
