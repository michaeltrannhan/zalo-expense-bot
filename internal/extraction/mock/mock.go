// Package mock is the deterministic MVP extractor (plan §10.4): identical
// bytes always yield the same ExtractionResult, selected from an embedded
// corpus of 10 synthetic receipts by sha256. No network, no OCR, no randomness.
package mock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
)

// MockPrefix marks dev-harness bytes that select a fixture directly by ID,
// e.g. "MOCK-FIXTURE:coopmart-clean". receipt.ValidateAndHash accepts the
// same prefix as text/plain so the simulate harness needs no real images.
const MockPrefix = "MOCK-FIXTURE:"

// Extractor implements extraction.Extractor against the embedded corpus.
type Extractor struct{}

// compile-time contract check.
var _ extraction.Extractor = (*Extractor)(nil)

func New() *Extractor { return &Extractor{} }

func (e *Extractor) Name() string    { return "mock-corpus" }
func (e *Extractor) Version() string { return "v1" }

// Extract selects a corpus entry deterministically: MOCK-FIXTURE:<id> bytes
// select by ID, everything else by sha256 mod len(corpus). The unsupported
// entry returns CodeUnsupported; read failures are CodeTransient.
func (e *Extractor) Extract(ctx context.Context, r io.Reader, _ extraction.Input) (events.ExtractionResult, error) {
	if err := ctx.Err(); err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "extract cancelled", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return events.ExtractionResult{}, domain.E(domain.CodeTransient, "reading receipt bytes", err)
	}

	if id, ok := strings.CutPrefix(string(data), MockPrefix); ok {
		entry, found := byID(strings.TrimSpace(id))
		if !found {
			return events.ExtractionResult{}, domain.Ef(domain.CodeValidation, nil, "unknown mock fixture %q", strings.TrimSpace(id))
		}
		if entry.unsupported {
			return events.ExtractionResult{}, domain.E(domain.CodeUnsupported, "image is not a receipt", nil)
		}
		return entry.result(e.Name(), e.Version()), nil
	}

	entry := corpus[indexFor(data)]
	if entry.unsupported {
		return events.ExtractionResult{}, domain.E(domain.CodeUnsupported, "image is not a receipt", nil)
	}
	return entry.result(e.Name(), e.Version()), nil
}

// indexFor maps arbitrary bytes to a corpus index. Pure function of the bytes.
func indexFor(data []byte) int {
	sum := sha256.Sum256(data)
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(len(corpus)))
}

func byID(id string) (corpusEntry, bool) {
	for _, entry := range corpus {
		if entry.id == id {
			return entry, true
		}
	}
	return corpusEntry{}, false
}

// Fixture exposes a corpus entry for evaluation against ground truth.
type Fixture struct {
	ID          string
	MerchantKey string
	// GroundTruth keys: merchant, total_minor, currency, occurred_at (UTC
	// RFC3339), type, category_key. Mirrors test/fixtures/receipts/ground_truth.json.
	GroundTruth map[string]any
}

// Fixtures returns the corpus in stable order for evaluation tests.
func Fixtures() []Fixture {
	out := make([]Fixture, 0, len(corpus))
	for _, e := range corpus {
		gt := map[string]any{
			"merchant":     e.canonical,
			"total_minor":  e.totalMinor,
			"currency":     e.currency,
			"type":         string(e.txType),
			"category_key": e.categoryKey,
		}
		if e.unsupported {
			gt["type"] = "unsupported"
		}
		if e.occurredAt.IsZero() {
			gt["occurred_at"] = ""
		} else {
			gt["occurred_at"] = e.occurredAt.Format(time.RFC3339)
		}
		out = append(out, Fixture{ID: e.id, MerchantKey: e.merchantKey, GroundTruth: gt})
	}
	return out
}

// BytesForIndex returns synthetic bytes whose indexFor equals i, so tests can
// pick a corpus entry without knowing the hash mapping. It brute-forces a
// nonce from 0 upward — deterministic because the search order is fixed.
// Returns nil for out-of-range i.
func BytesForIndex(i int) []byte {
	if i < 0 || i >= len(corpus) {
		return nil
	}
	for nonce := 0; ; nonce++ {
		h1 := sha256.Sum256([]byte(fmt.Sprintf("mock-fixture-%d-%d", i, nonce)))
		h2 := sha256.Sum256(h1[:])
		data := append(h1[:], h2[:]...) // 64 bytes
		if indexFor(data) == i {
			return data
		}
	}
}
