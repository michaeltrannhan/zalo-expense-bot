package zalo

import (
	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/messaging"
)

func init() {
	messaging.RegisterZalo(func(cfg config.Config) messaging.Provider {
		return New(Config{
			Token:         cfg.ZaloBotToken,
			WebhookSecret: cfg.ZaloWebhookSecret,
			APIBase:       cfg.ZaloAPIBase,
		})
	})
}
