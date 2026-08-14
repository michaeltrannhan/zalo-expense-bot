package messaging

import (
	"log/slog"

	"zl-expese-bot/internal/config"
)

// Adapter packages register constructors via init. NewFromConfig lives in
// this package so cmd mains stay thin; it cannot import zalo/logprovider
// itself (those packages import messaging).
var (
	newZalo func(cfg config.Config) Provider
	newLog  func(log *slog.Logger) Provider
)

// RegisterZalo is called from messaging/zalo init.
func RegisterZalo(fn func(cfg config.Config) Provider) { newZalo = fn }

// RegisterLog is called from messaging/logprovider init.
func RegisterLog(fn func(log *slog.Logger) Provider) { newLog = fn }

// NewFromConfig picks the messaging adapter: the real Zalo Bot API when a
// token is configured, otherwise the log provider that never dials out.
func NewFromConfig(cfg config.Config, log *slog.Logger) Provider {
	if cfg.MessagingMode() == "zalo" && newZalo != nil {
		return newZalo(cfg)
	}
	if log != nil {
		log.Warn("ZALO_BOT_TOKEN not set: outbound messages are logged, not sent")
	}
	if newLog == nil {
		panic("messaging: log provider not registered (import internal/messaging/logprovider)")
	}
	return newLog(log)
}
