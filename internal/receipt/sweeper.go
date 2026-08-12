package receipt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/store"
)

// Sweeper deletes receipt originals whose retention deadline has passed
// (plan §12: originals_30d). Only the image object and the receipt's live
// status go away; extracted fields and transactions are kept, so history
// and insights survive the purge. Every step is idempotent: a replayed
// sweep re-queries and finds nothing to do.
type Sweeper struct {
	st      *store.Store
	objects objectstore.Store
	q       queue.Queue
	clk     clock.Clock
	log     *slog.Logger

	// BatchSize caps deletions per job; a full batch chains one follow-up
	// job so large backlogs drain over several short claims instead of one
	// long one.
	BatchSize int
}

// NewSweeper wires the retention sweep dependencies.
func NewSweeper(st *store.Store, objects objectstore.Store, q queue.Queue, clk clock.Clock, log *slog.Logger) *Sweeper {
	return &Sweeper{st: st, objects: objects, q: q, clk: clk, log: log, BatchSize: 200}
}

// Handle processes one retention_sweep job. Object-removal or transition
// failures on individual receipts are skipped (they retry on the next
// hourly sweep); only a failure to list or to chain the backlog job is
// returned for the worker's retry classification.
func (s *Sweeper) Handle(ctx context.Context, job *domain.QueueJob) error {
	var payload events.RetentionSweepJob
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return domain.E(domain.CodeValidation, "decode retention job", err)
	}
	tombstonesDeleted, err := s.st.DeleteExpiredProviderMessageTombstones(ctx, s.clk.Now())
	if err != nil {
		return domain.E(domain.CodeTransient, "delete expired provider message tombstones", err)
	}

	pending, err := s.st.ListReceiptsPendingDeletion(ctx, s.clk.Now())
	if err != nil {
		return domain.E(domain.CodeTransient, "list receipts pending deletion", err)
	}
	backlog := len(pending) > s.BatchSize
	if backlog {
		pending = pending[:s.BatchSize]
	}

	deleted := 0
	for _, r := range pending {
		// In-flight receipts cannot transition to deleted (state machine);
		// the next sweep picks them up once processing settles.
		if r.Status == domain.ReceiptDownloading || r.Status == domain.ReceiptExtracting {
			continue
		}
		if r.StorageKey != "" {
			if err := s.objects.Delete(ctx, r.StorageKey); err != nil {
				s.log.Warn("retention: receipt object delete failed",
					slog.String("receipt_id", r.ID.String()), slog.String("error", err.Error()))
				continue
			}
		}
		if err := s.st.TransitionReceipt(ctx, r.ID, r.Status, domain.ReceiptDeleted); err != nil {
			// Conflict means the receipt moved on meanwhile (user action);
			// nothing to undo — the object delete is retry-safe.
			s.log.Warn("retention: receipt transition failed",
				slog.String("receipt_id", r.ID.String()),
				slog.String("from", string(r.Status)),
				slog.String("error", err.Error()))
			continue
		}
		deleted++
	}
	s.log.Info("retention sweep done",
		slog.Int("receipts_deleted", deleted),
		slog.Int64("provider_tombstones_deleted", tombstonesDeleted),
		slog.Int("scanned", len(pending)), slog.Bool("backlog", backlog))

	if backlog {
		if err := EnqueueSweep(ctx, s.q, s.clk, ""); err != nil && !errors.Is(err, queue.ErrDuplicate) {
			return domain.E(domain.CodeTransient, "chain retention sweep", err)
		}
	}
	return nil
}

// EnqueueSweep queues one retention sweep. dedupeSuffix scopes the dedupe
// key: the scheduler passes the current hour ("2006-01-02T15"), so hourly
// runs collapse across replicas; the backlog chain passes "" for a unique
// key per call.
func EnqueueSweep(ctx context.Context, q queue.Queue, clk clock.Clock, dedupeSuffix string) error {
	now := clk.Now()
	payload, err := json.Marshal(events.RetentionSweepJob{
		SchemaVersion: events.SchemaV1,
		EnqueuedAt:    now,
	})
	if err != nil {
		return domain.E(domain.CodeInternal, "marshal retention job", err)
	}
	dedupe := "retention:" + dedupeSuffix
	if dedupeSuffix == "" {
		dedupe = "retention:chain:" + now.UTC().Format("20060102150405.000000000")
	}
	_, err = q.Enqueue(ctx, domain.JobRetentionSweep, payload, &dedupe, 3)
	return err
}
