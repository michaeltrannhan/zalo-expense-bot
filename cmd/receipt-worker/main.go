// Command receipt-worker drains receipt_process queue jobs: download →
// validate → store → duplicate check → extract → draft transaction →
// confirmation card. It is the only process that talks to the extractor,
// which keeps OCR quota accounting and the kill switch in one place.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"zl-expese-bot/internal/categorisation"
	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
	_ "zl-expese-bot/internal/extraction/gemini"
	_ "zl-expese-bot/internal/extraction/mock"
	_ "zl-expese-bot/internal/extraction/textract"
	"zl-expese-bot/internal/logging"
	"zl-expese-bot/internal/messaging"
	_ "zl-expese-bot/internal/messaging/logprovider"
	_ "zl-expese-bot/internal/messaging/zalo"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/postgres"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/receipt"
	"zl-expese-bot/internal/store"
	"zl-expese-bot/internal/worker"
)

// visibility bounds how long one receipt job may run before another lane
// may retry it (crash recovery). Heartbeats renew this lease while OCR runs.
const visibility = 5 * time.Minute

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "receipt-worker:", err)
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
	objects, err := objectstore.NewFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	provider := messaging.NewFromConfig(cfg, log)
	replies := notify.NewEnqueuer(st, q, clk)
	cats := categorisation.NewService(st, clk)

	extractor, err := extraction.NewFromConfig(ctx, cfg)
	if err != nil {
		return err
	}
	processor := receipt.NewProcessor(st, objects, extractor, provider, cats, replies, clk, log)
	processor.ExtractionEnabled = cfg.ExtractionEnabled
	processor.MonthlyOCRLimit = cfg.MonthlyOCRPageLimit
	sweeper := receipt.NewSweeper(st, objects, q, clk, log)

	// Hourly retention sweep: the hour-scoped dedupe key collapses restarts
	// and replicas, so the sweep runs once per hour at most.
	go func() {
		enqueue := func() {
			if err := receipt.EnqueueSweep(ctx, q, clk, clk.Now().Format("2006-01-02T15")); err != nil &&
				!errors.Is(err, queue.ErrDuplicate) {
				log.Error("enqueue retention sweep", slog.String("error", err.Error()))
			}
		}
		enqueue() // catch any backlog on startup
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				enqueue()
			}
		}
	}()

	log.Info("receipt worker starting",
		slog.Int("concurrency", cfg.ReceiptWorkerConcurrency),
		slog.Bool("extraction_enabled", processor.ExtractionEnabled),
		slog.Int64("monthly_ocr_limit", processor.MonthlyOCRLimit))
	worker.Run(ctx, log, q, worker.Config{
		Name:         "receipt",
		Kinds:        []domain.JobKind{domain.JobReceiptProcess, domain.JobRetentionSweep},
		Concurrency:  cfg.ReceiptWorkerConcurrency,
		PollInterval: cfg.QueuePollInterval,
		Visibility:   visibility,
	}, func(ctx context.Context, job *domain.QueueJob) error {
		if job.Kind == domain.JobRetentionSweep {
			return sweeper.Handle(ctx, job)
		}
		return processor.Handle(ctx, job)
	})
	return nil
}
