package logprovider

import (
	"log/slog"

	"zl-expese-bot/internal/messaging"
)

func init() {
	messaging.RegisterLog(func(log *slog.Logger) messaging.Provider {
		return New(log)
	})
}
