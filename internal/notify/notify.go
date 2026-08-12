// Package notify is the outbound path: producers enqueue into the
// outbound_messages outbox plus an outbound_send queue job, and the
// notification worker (Sender) is the only code that talks to the messaging
// provider. Outbox idempotency keys make producer retries safe; delivery uses
// an explicit ambiguous state because Zalo cannot dedupe a repeated request.
package notify

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/messaging"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/store"
)

// Enqueuer writes outbound rows and their queue jobs.
type Enqueuer struct {
	st  *store.Store
	q   queue.Queue
	clk clock.Clock
}

// NewEnqueuer builds an Enqueuer.
func NewEnqueuer(st *store.Store, q queue.Queue, clk clock.Clock) *Enqueuer {
	return &Enqueuer{st: st, q: q, clk: clk}
}

// Reply enqueues one chat message for userID/chatID. idempotencyKey must be
// deterministic wherever the producer can be retried (e.g.
// "card:<txID>:v3"); a replayed key is a no-op. The queue job is only
// enqueued for rows actually created. On replay, Reply also repairs the
// queue job if a previous process crashed after writing the outbox row.
func (e *Enqueuer) Reply(ctx context.Context, userID uuid.UUID, provider domain.Provider, chatID, body, idempotencyKey string) error {
	return e.enqueue(ctx, userID, provider, chatID, body, idempotencyKey, false)
}

// DeletionConfirmation enqueues the minimal post-purge receipt. It is
// physically removed with its queue job after a terminal delivery attempt.
func (e *Enqueuer) DeletionConfirmation(ctx context.Context, userID uuid.UUID, provider domain.Provider, chatID, body, idempotencyKey string) error {
	return e.enqueue(ctx, userID, provider, chatID, body, idempotencyKey, true)
}

func (e *Enqueuer) enqueue(ctx context.Context, userID uuid.UUID, provider domain.Provider, chatID, body, idempotencyKey string, ephemeral bool) error {
	rec := &domain.OutboundRecord{
		ID:             uuid.New(),
		UserID:         userID,
		Provider:       provider,
		ProviderChatID: chatID,
		IdempotencyKey: idempotencyKey,
		Body:           body,
		Ephemeral:      ephemeral,
	}
	created, err := e.st.EnqueueOutbound(ctx, rec)
	if err != nil {
		return err
	}
	if !created {
		rec, err = e.st.GetOutboundByIdempotencyKey(ctx, idempotencyKey)
		if err != nil {
			return err
		}
		if rec.Status != domain.OutboundQueued {
			return nil
		}
	}
	job := events.OutboundJob{
		SchemaVersion: events.SchemaV1,
		OutboundID:    rec.ID,
		UserID:        userID,
		EnqueuedAt:    e.clk.Now(),
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return domain.E(domain.CodeInternal, "marshal outbound job", err)
	}
	dedupe := "outbound:" + rec.ID.String()
	if _, err := e.q.Enqueue(ctx, domain.JobOutboundSend, payload, &dedupe, 5); err != nil && err != queue.ErrDuplicate {
		return err
	}
	return nil
}

// Sender drains outbound_send jobs and delivers through the provider. It is
// the only component that calls Provider.Send.
type Sender struct {
	st       *store.Store
	provider messaging.Provider
	clk      clock.Clock
	log      *slog.Logger

	// OutboundEnabled is the emergency outbound kill switch.
	OutboundEnabled bool
	// MonthlyMessageLimit caps provider sends per calendar month (Zalo Basic
	// = 3000). 0 means unlimited (never configured that way in the pilot).
	MonthlyMessageLimit int64
}

// NewSender builds a Sender.
func NewSender(st *store.Store, p messaging.Provider, clk clock.Clock, log *slog.Logger, enabled bool, monthlyLimit int64) *Sender {
	return &Sender{
		st: st, provider: p, clk: clk, log: log,
		OutboundEnabled: enabled, MonthlyMessageLimit: monthlyLimit,
	}
}

// Handle processes one outbound_send queue job. Because Zalo sendMessage has
// no provider-side idempotency key, an interrupted/uncertain attempt becomes
// explicit "ambiguous" state and is never automatically sent a second time.
func (s *Sender) Handle(ctx context.Context, job *domain.QueueJob) error {
	var payload events.OutboundJob
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return domain.E(domain.CodeValidation, "decode outbound job", err)
	}
	return s.st.WithUserLock(ctx, payload.UserID, func(lockedCtx context.Context) error {
		return s.handleLocked(lockedCtx, job, payload)
	})
}

func (s *Sender) handleLocked(ctx context.Context, job *domain.QueueJob, payload events.OutboundJob) error {
	rec, err := s.st.GetOutbound(ctx, payload.OutboundID)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			return s.st.DeleteQueueJob(ctx, job.ID)
		}
		return err
	}
	switch rec.Status {
	case domain.OutboundQueued:
	case domain.OutboundSending:
		if rec.Ephemeral {
			return s.st.PurgeOutboundAndJob(ctx, rec.ID, job.ID)
		}
		return s.st.MarkOutboundStatus(ctx, rec.ID, domain.OutboundAmbiguous,
			"worker recovered an interrupted provider attempt; delivery outcome unknown", nil)
	default:
		return nil
	}
	user, err := s.st.GetUser(ctx, payload.UserID)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			return s.st.PurgeOutboundAndJob(ctx, rec.ID, job.ID)
		}
		return err
	}
	if (user.Status == domain.UserDeleted || user.DeletedAt != nil) && !rec.Ephemeral {
		return s.st.PurgeOutboundAndJob(ctx, rec.ID, job.ID)
	}

	if !s.OutboundEnabled {
		if rec.Ephemeral {
			return s.st.PurgeOutboundAndJob(ctx, rec.ID, job.ID)
		}
		return s.st.MarkOutboundStatus(ctx, rec.ID, domain.OutboundSuppressed, "outbound kill switch", nil)
	}

	period := s.clk.Now().Format("2006-01")
	rec, allowed, attemptCount, err := s.st.BeginOutboundAttempt(ctx, rec.ID, period, s.MonthlyMessageLimit)
	if err != nil {
		return err
	}
	if !allowed {
		if rec.Ephemeral {
			return s.st.PurgeOutboundAndJob(ctx, rec.ID, job.ID)
		}
		return nil
	}
	if s.MonthlyMessageLimit > 0 {
		if pct := attemptCount * 100 / s.MonthlyMessageLimit; pct == 70 || pct == 85 || pct == 95 {
			s.log.Warn("zalo monthly message attempt threshold",
				slog.Int64("count", attemptCount), slog.Int64("limit", s.MonthlyMessageLimit), slog.Int64("pct", pct))
		}
	}

	ref, sendErr := s.provider.Send(ctx, messaging.OutboundMessage{
		ProviderChatID: rec.ProviderChatID,
		Text:           rec.Body,
		IdempotencyKey: rec.IdempotencyKey,
	})
	if sendErr != nil {
		if domain.Retryable(sendErr) {
			if rec.Ephemeral {
				return s.st.PurgeOutboundAndJob(ctx, rec.ID, job.ID)
			}
			return s.st.MarkOutboundStatus(ctx, rec.ID, domain.OutboundAmbiguous,
				string(domain.CodeOf(sendErr))+": "+sendErr.Error(), nil)
		}
		if rec.Ephemeral {
			return s.st.PurgeOutboundAndJob(ctx, rec.ID, job.ID)
		}
		return s.st.MarkOutboundStatus(ctx, rec.ID, domain.OutboundFailed, string(domain.CodeOf(sendErr))+": "+sendErr.Error(), nil)
	}
	_, err = s.st.CompleteOutboundSend(ctx, rec.ID, job.ID, period,
		s.MonthlyMessageLimit, ref.ProviderMessageID)
	return err
}
