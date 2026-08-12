// Package worker runs bounded-concurrency consumers over the queue. One
// runner per process role (receipt, notification); graceful shutdown lets
// in-flight jobs finish before returning.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/queue"
)

// Handler processes one claimed job. Returning a *domain.Error with
// CodeTransient Nacks the job (exponential backoff, dead-letter after
// max attempts); any nil or permanent error Acks it — permanent failures
// must be persisted by the handler itself before returning nil.
type Handler func(ctx context.Context, job *domain.QueueJob) error

// Config tunes one runner.
type Config struct {
	Name         string
	Kinds        []domain.JobKind
	Concurrency  int
	PollInterval time.Duration
	Visibility   time.Duration
}

// Run blocks until ctx is cancelled and all in-flight jobs settle.
func Run(ctx context.Context, log *slog.Logger, q queue.Queue, cfg Config, handle Handler) {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.Visibility <= 0 {
		cfg.Visibility = 30 * time.Second
	}

	var wg sync.WaitGroup
	for i := range cfg.Concurrency {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			loop(ctx, log.With(slog.String("worker", cfg.Name), slog.Int("lane", id)), q, cfg, handle)
		}(i)
	}
	wg.Wait()
}

func loop(ctx context.Context, log *slog.Logger, q queue.Queue, cfg Config, handle Handler) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := q.Dequeue(ctx, cfg.Kinds, cfg.Visibility)
		switch {
		case errors.Is(err, queue.ErrEmpty):
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.PollInterval):
			}
			continue
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			log.Error("dequeue failed", slog.String("error", err.Error()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.PollInterval):
			}
			continue
		}

		handleErr := handle(ctx, job)
		switch {
		case handleErr == nil:
			if err := q.Ack(ctx, job.ID); err != nil {
				log.Error("ack failed", slog.String("job_id", job.ID.String()), slog.String("error", err.Error()))
			}
		case domain.Retryable(handleErr):
			log.Warn("job failed transiently",
				slog.String("job_id", job.ID.String()),
				slog.String("kind", string(job.Kind)),
				slog.String("error_class", string(domain.CodeOf(handleErr))))
			if err := q.Nack(ctx, job.ID, handleErr); err != nil {
				log.Error("nack failed", slog.String("job_id", job.ID.String()), slog.String("error", err.Error()))
			}
		default:
			// Permanent error without persisted outcome: Nack would retry
			// forever, so Ack and rely on the handler's own records.
			log.Error("job failed permanently",
				slog.String("job_id", job.ID.String()),
				slog.String("kind", string(job.Kind)),
				slog.String("error", handleErr.Error()))
			if err := q.Ack(ctx, job.ID); err != nil {
				log.Error("ack failed", slog.String("job_id", job.ID.String()), slog.String("error", err.Error()))
			}
		}
	}
}
