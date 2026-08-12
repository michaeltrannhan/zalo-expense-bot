package mock

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
	"zl-expese-bot/internal/extraction/normalise"
)

// groundTruthEntry mirrors one row of
// test/fixtures/receipts/ground_truth.json (plan P1-D02).
type groundTruthEntry struct {
	ID          string `json:"id"`
	Merchant    string `json:"merchant"`
	TotalMinor  int64  `json:"total_minor"`
	Currency    string `json:"currency"`
	OccurredAt  string `json:"occurred_at"`
	Type        string `json:"type"`
	CategoryKey string `json:"category_key"`
}

func loadGroundTruth(t *testing.T) []groundTruthEntry {
	t.Helper()
	path := filepath.Join("..", "..", "..", "test", "fixtures", "receipts", "ground_truth.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ground truth: %v", err)
	}
	var entries []groundTruthEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("decode ground truth: %v", err)
	}
	return entries
}

// TestCorpusMatchesGroundTruth keeps the embedded corpus and the repository
// ground-truth fixture in lockstep: every corpus entry is mirrored exactly,
// so evaluation results stay meaningful when either side changes (P1-D02).
func TestCorpusMatchesGroundTruth(t *testing.T) {
	entries := loadGroundTruth(t)
	fixtures := Fixtures()
	if len(entries) != len(fixtures) {
		t.Fatalf("ground truth has %d entries, corpus has %d", len(entries), len(fixtures))
	}
	byID := make(map[string]groundTruthEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, f := range fixtures {
		e, ok := byID[f.ID]
		if !ok {
			t.Errorf("ground truth missing corpus entry %q", f.ID)
			continue
		}
		gt := f.GroundTruth
		checks := []struct {
			key  string
			want any
			got  any
		}{
			{"merchant", gt["merchant"], e.Merchant},
			{"total_minor", gt["total_minor"], e.TotalMinor},
			{"currency", gt["currency"], e.Currency},
			{"occurred_at", gt["occurred_at"], e.OccurredAt},
			{"type", gt["type"], e.Type},
			{"category_key", gt["category_key"], e.CategoryKey},
		}
		for _, c := range checks {
			if c.want != c.got {
				t.Errorf("%s: corpus %s = %v, ground truth = %v", f.ID, c.key, c.want, c.got)
			}
		}
	}
}

// TestExtractorEvaluation is the P3-Q01 harness: it scores the extractor
// against ground truth per field and enforces the Gate-3 accuracy floors
// (§19). The mock corpus is deterministic, so every field must be exact;
// the floors are the same thresholds the real OCR adapter will be judged
// against on a larger synthetic/redacted corpus.
func TestExtractorEvaluation(t *testing.T) {
	ex := New()
	ctx := context.Background()

	var supported, merchantOK, totalOK, currencyOK, dateOK, typeOK, categoryOK, unsupportedOK int
	for _, e := range loadGroundTruth(t) {
		res, err := ex.Extract(ctx, strings.NewReader(MockPrefix+e.ID), extraction.Input{ReceiptID: e.ID})
		if e.Type == "unsupported" {
			if domain.IsCode(err, domain.CodeUnsupported) {
				unsupportedOK++
			} else {
				t.Errorf("%s: want CodeUnsupported, got %v", e.ID, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: extract: %v", e.ID, err)
			continue
		}
		supported++

		_, wantKey := normalise.NormaliseMerchant(e.Merchant)
		if res.Fields["merchant"].Normalised == wantKey {
			merchantOK++
		}
		if res.Fields["total_minor"].Normalised == e.TotalMinor {
			totalOK++
		}
		if res.Fields["currency"].Normalised == e.Currency {
			currencyOK++
		}
		if res.Fields["occurred_at"].Normalised == e.OccurredAt {
			dateOK++
		}
		if res.Fields["category"].Normalised == e.CategoryKey {
			categoryOK++
		}

		gotType := "expense"
		if hint, ok := res.Fields["type_hint"]; ok {
			gotType, _ = hint.Normalised.(string)
		}
		if gotType == e.Type {
			typeOK++
		}
		// Product rule: refund/transfer must never be silently acceptable.
		if e.Type == "refund" || e.Type == "transfer" {
			if !extraction.TypeNeedsConfirmation(domain.TxType(gotType)) {
				t.Errorf("%s: type %s must require explicit confirmation", e.ID, gotType)
			}
		}
	}

	acc := func(n int) float64 { return float64(n) / float64(supported) }
	t.Logf("P3-Q01 evaluation over %d supported fixtures: merchant=%.2f total=%.2f currency=%.2f date=%.2f type=%.2f category=%.2f; unsupported handled=%d",
		supported, acc(merchantOK), acc(totalOK), acc(currencyOK), acc(dateOK), acc(typeOK), acc(categoryOK), unsupportedOK)

	floors := map[string]struct {
		got  float64
		want float64
	}{
		"total_minor": {acc(totalOK), 0.95},
		"currency":    {acc(currencyOK), 0.95},
		"occurred_at": {acc(dateOK), 0.90},
		"merchant":    {acc(merchantOK), 0.80},
		"category":    {acc(categoryOK), 0.80},
		"type":        {acc(typeOK), 1.00}, // dangerous types are never approximate
	}
	for field, f := range floors {
		if f.got < f.want {
			t.Errorf("%s accuracy %.2f below Gate-3 floor %.2f", field, f.got, f.want)
		}
	}
	if unsupportedOK != 1 {
		t.Errorf("unsupported fixtures handled = %d, want 1", unsupportedOK)
	}
}
