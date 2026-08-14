package extraction

import (
	"context"
	"fmt"

	"zl-expese-bot/internal/config"
)

// Factory builds an Extractor from process config. Adapter packages register
// via init so this package does not import them (they import extraction).
type Factory func(ctx context.Context, cfg config.Config) (Extractor, error)

var factories = map[string]Factory{}

// Register is called from extraction adapter init functions.
func Register(name string, fn Factory) { factories[name] = fn }

// NewFromConfig builds the receipt extractor: the deterministic mock by
// default, Amazon Textract AnalyzeExpense (EXTRACTOR=textract), or a vision
// LLM (EXTRACTOR=gemini). Callers must import the adapter packages so they
// register (typically cmd/receipt-worker).
func NewFromConfig(ctx context.Context, cfg config.Config) (Extractor, error) {
	name := cfg.ExtractionBackend
	if name == "" {
		name = "mock"
	}
	fn := factories[name]
	if fn == nil {
		return nil, fmt.Errorf("extractor %q is not registered", name)
	}
	return fn(ctx, cfg)
}
