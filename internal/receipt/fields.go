package receipt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/normalise"
)

func fieldRaw(res events.ExtractionResult, name string) string {
	f, ok := res.Fields[name]
	if !ok {
		return ""
	}
	return strings.TrimSpace(f.Raw)
}

// fieldString reads a normalised string field.
func fieldString(res events.ExtractionResult, name string) string {
	f, ok := res.Fields[name]
	if !ok || f.Normalised == nil {
		return ""
	}
	if s, ok := f.Normalised.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", f.Normalised))
}

// fieldInt reads a normalised integer field (minor units). JSON round-trips
// turn numbers into float64; all numeric shapes are accepted.
func fieldInt(res events.ExtractionResult, name string) (int64, error) {
	f, ok := res.Fields[name]
	if !ok || f.Normalised == nil {
		return 0, domain.Ef(domain.CodeValidation, nil, "extraction missing %s", name)
	}
	switch v := f.Normalised.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case float64:
		return int64(v), nil
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, domain.E(domain.CodeValidation, "parse "+name, err)
		}
		return n, nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, domain.E(domain.CodeValidation, "parse "+name, err)
		}
		return n, nil
	}
	return 0, domain.Ef(domain.CodeValidation, nil, "extraction %s is not numeric", name)
}

// fieldTime reads the normalised timestamp: RFC3339 first, then the chat
// date formats via normalise.ParseDate in the user's timezone.
func fieldTime(res events.ExtractionResult, name string, loc *time.Location) (time.Time, error) {
	if s := fieldString(res, name); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC(), nil
		}
	}
	if raw := fieldRaw(res, name); raw != "" {
		if t, err := normalise.ParseDate(raw, loc); err == nil {
			return t, nil
		}
	}
	// A missing date is not fatal: default to now in the caller's hands.
	return time.Time{}, domain.Ef(domain.CodeValidation, nil, "extraction missing %s", name)
}

// fieldConf reads a field confidence, defaulting to 0 (flag everything).
func fieldConf(res events.ExtractionResult, name string) float64 {
	if f, ok := res.Fields[name]; ok {
		return f.Confidence
	}
	return 0
}

// confidenceSummary is the compact provenance string persisted on the
// transaction: sorted "field:conf" pairs.
func confidenceSummary(res events.ExtractionResult) string {
	keys := make([]string, 0, len(res.Fields))
	for k := range res.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s:%.2f", k, res.Fields[k].Confidence))
	}
	return strings.Join(parts, ",")
}

// provenance maps extraction fields to extracted_fields rows.
func provenance(receiptID uuid.UUID, res events.ExtractionResult) []domain.ExtractedField {
	out := make([]domain.ExtractedField, 0, len(res.Fields))
	for name, f := range res.Fields {
		normalised := ""
		if f.Normalised != nil {
			if s, ok := f.Normalised.(string); ok {
				normalised = s
			} else {
				normalised = fmt.Sprintf("%v", f.Normalised)
			}
		}
		out = append(out, domain.ExtractedField{
			ID:                uuid.New(),
			ReceiptDocumentID: receiptID,
			FieldName:         name,
			RawValue:          f.Raw,
			NormalisedValue:   normalised,
			Confidence:        f.Confidence,
			Source:            "extractor",
			ExtractorName:     res.Extractor.Name,
			ExtractorVersion:  res.Extractor.Version,
		})
	}
	return out
}
