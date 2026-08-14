package gemini

import (
	"context"

	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/extraction"
)

func init() {
	extraction.Register("gemini", func(_ context.Context, cfg config.Config) (extraction.Extractor, error) {
		return New(Config{
			APIKey:  cfg.GeminiAPIKey,
			Model:   cfg.GeminiModel,
			APIBase: cfg.GeminiAPIBase,
		}), nil
	})
}
