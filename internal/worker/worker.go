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
// max attempts). Returning nil Acks it. Permanent errors also Ack — the
// handler must persist a durable terminal outcome or return CodeTransient.
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

		handleErr := runWithHeartbeat(ctx, q, job, cfg.Visibility, handle)
		switch {
		case handleErr == nil:
			if err := q.Ack(ctx, job.ID, job.ClaimToken); err != nil {
				if !errors.Is(err, queue.ErrStaleClaim) {
					log.Error("ack failed", slog.String("job_id", job.ID.String()), slog.String("error", err.Error()))
				}
			}
		case domain.Retryable(handleErr):
			log.Warn("job failed transiently",
				slog.String("job_id", job.ID.String()),
				slog.String("kind", string(job.Kind)),
				slog.String("error_class", string(domain.CodeOf(handleErr))),
				slog.String("error", handleErr.Error()))
			if err := q.Nack(ctx, job.ID, job.ClaimToken, handleErr); err != nil {
				if !errors.Is(err, queue.ErrStaleClaim) {
					log.Error("nack failed", slog.String("job_id", job.ID.String()), slog.String("error", err.Error()))
				}
			}
		default:
			// Permanent error with a persisted outcome: Ack. Receipt jobs
			// convert non-durable permanent failures to CodeTransient first.
			log.Error("job failed permanently",
				slog.String("job_id", job.ID.String()),
				slog.String("kind", string(job.Kind)),
				slog.String("error", handleErr.Error()))
			if err := q.Ack(ctx, job.ID, job.ClaimToken); err != nil {
				if !errors.Is(err, queue.ErrStaleClaim) {
					log.Error("ack failed", slog.String("job_id", job.ID.String()), slog.String("error", err.Error()))
				}
			}
		}
	}
}

// runWithHeartbeat renews the claim lease while handle runs so long OCR or
// network work cannot expire visibility and let a second worker steal the job.
func runWithHeartbeat(ctx context.Context, q queue.Queue, job *domain.QueueJob, visibility time.Duration, handle Handler) error {
	hbCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	interval := visibility / 3
	if interval < time.Second {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if err := q.Heartbeat(hbCtx, job.ID, job.ClaimToken, visibility); err != nil {
					return
				}
			}
		}
	}()

	return handle(ctx, job)
}
