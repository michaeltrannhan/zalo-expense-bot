package mock

import (
	"context"

	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/extraction"
)

func init() {
	extraction.Register("mock", func(context.Context, config.Config) (extraction.Extractor, error) {
		return New(), nil
	})
}
