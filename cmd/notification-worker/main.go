// Command notification-worker drains outbound_send queue jobs and delivers
// them through the messaging provider. It is the only process that calls
// Provider.Send, which keeps the monthly message quota and the outbound
// kill switch in one place.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/insight"
	"zl-expese-bot/internal/logging"
	"zl-expese-bot/internal/messaging"
	"zl-expese-bot/internal/messaging/logprovider"
	"zl-expese-bot/internal/messaging/zalo"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/postgres"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/store"
	"zl-expese-bot/internal/summaryschedule"
	"zl-expese-bot/internal/worker"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "notification-worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	st := store.New(pool)
	clk := clock.Real{}
	q := queue.NewPG(pool, clk)
	provider := messagingProvider(cfg, log)
	replies := notify.NewEnqueuer(st, q, clk)
	sender := notify.NewSender(st, provider, clk, log, cfg.OutboundEnabled, cfg.ZaloMonthlyMessageLimit)
	summaryRunner := summaryschedule.NewRunner(st, insight.NewService(pool, clk), replies, clk, log)

	log.Info("notification worker starting",
		slog.Int("concurrency", cfg.NotificationWorkerConcurrency),
		slog.Bool("outbound_enabled", cfg.OutboundEnabled),
		slog.Int64("monthly_message_limit", cfg.ZaloMonthlyMessageLimit))
	go summaryRunner.Run(ctx, cfg.SummarySchedulePollInterval)
	worker.Run(ctx, log, q, worker.Config{
		Name:         "notification",
		Kinds:        []domain.JobKind{domain.JobOutboundSend},
		Concurrency:  cfg.NotificationWorkerConcurrency,
		PollInterval: cfg.QueuePollInterval,
	}, sender.Handle)
	return nil
}

// messagingProvider picks the messaging adapter: the real Zalo Bot API when
// a token is configured, otherwise the log provider that never dials out.
func messagingProvider(cfg config.Config, log *slog.Logger) messaging.Provider {
	if cfg.MessagingMode() == "zalo" {
		return zalo.New(zalo.Config{
			Token:         cfg.ZaloBotToken,
			WebhookSecret: cfg.ZaloWebhookSecret,
			APIBase:       cfg.ZaloAPIBase,
		})
	}
	return logprovider.New(log)
}
