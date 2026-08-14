package summaryschedule

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"zl-expese-bot/internal/conversation"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/insight"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/store"
)

const dueBatchSize = 100

// Runner finds due preferences, builds evidence-backed summaries, and
// enqueues them through the same idempotent outbox as interactive replies.
type Runner struct {
	store    *store.Store
	insights *insight.Service
	replies  *notify.Enqueuer
	clock    clock.Clock
	log      *slog.Logger
}

// NewRunner builds a scheduled-summary runner.
func NewRunner(st *store.Store, insights *insight.Service, replies *notify.Enqueuer, clk clock.Clock, log *slog.Logger) *Runner {
	return &Runner{store: st, insights: insights, replies: replies, clock: clk, log: log}
}

// Run polls until ctx is cancelled. A failed preference does not block other
// users; it remains due and is retried on the next tick.
func (r *Runner) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	run := func() {
		processed, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Error("scheduled summary pass failed", slog.String("error", err.Error()))
		}
		if processed > 0 {
			r.log.Info("scheduled summaries enqueued", slog.Int("count", processed))
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// RunOnce processes one bounded due batch and returns the number advanced.
func (r *Runner) RunOnce(ctx context.Context) (int, error) {
	now := r.clock.Now()
	preferences, err := r.store.ListDueSummarySchedules(ctx, now, dueBatchSize)
	if err != nil {
		return 0, err
	}
	processed := 0
	var errs []error
	for i := range preferences {
		if err := r.process(ctx, &preferences[i], now); err != nil {
			errs = append(errs, err)
			r.log.Warn("scheduled summary failed",
				slog.String("frequency", string(preferences[i].Frequency)),
				slog.String("error_class", string(domain.CodeOf(err))))
			continue
		}
		processed++
	}
	return processed, errors.Join(errs...)
}

func (r *Runner) process(ctx context.Context, preference *domain.SummarySchedule, now time.Time) error {
	return r.store.WithUserLock(ctx, preference.UserID, func(lockedCtx context.Context) error {
		return r.processLocked(lockedCtx, preference, now)
	})
}

func (r *Runner) processLocked(ctx context.Context, preference *domain.SummarySchedule, now time.Time) error {
	user, err := r.store.GetUser(ctx, preference.UserID)
	if err != nil {
		return err
	}
	if user.Status != domain.UserActive || user.DeletedAt != nil {
		return nil
	}
	loc := user.Location()
	scheduledFor, err := LatestDelivery(now, loc, preference.Frequency, preference.DeliveryMinute)
	if err != nil {
		return domain.E(domain.CodeValidation, "calculate latest summary delivery", err)
	}
	next, err := NextDelivery(now, loc, preference.Frequency, preference.DeliveryMinute)
	if err != nil {
		return domain.E(domain.CodeValidation, "calculate next summary delivery", err)
	}

	var period insight.Period
	var label, insightType string
	switch preference.Frequency {
	case domain.SummaryDaily:
		period, label, insightType = insight.Yesterday(scheduledFor, loc), "Hôm qua", insight.TypeDaily
	case domain.SummaryWeekly:
		period, label, insightType = insight.LastWeek(scheduledFor, loc), "Tuần trước", insight.TypeWeekly
	case domain.SummaryMonthly:
		period, label, insightType = insight.LastMonth(scheduledFor, loc), "Tháng trước", insight.TypeMonthly
	default:
		return domain.Ef(domain.CodeValidation, nil,
			"unsupported scheduled summary frequency %q", preference.Frequency)
	}

	sum, err := r.insights.Summarise(ctx, user.ID, period)
	if err != nil {
		return err
	}
	if err := r.insights.Persist(ctx, user.ID, insightType, period, sum); err != nil {
		return err
	}
	idempotencyKey := fmt.Sprintf("scheduled-summary:%s:%s:%s",
		user.ID, preference.Frequency, period.Start.Format("20060102T150405Z"))
	if err := r.replies.Reply(ctx, user.ID, preference.Provider, preference.ProviderChatID,
		conversation.SummaryText(label, sum, loc), idempotencyKey); err != nil {
		return err
	}
	_, err = r.store.AdvanceSummarySchedule(ctx, preference.ID, preference.NextDeliveryAt, next, now)
	return err
}
