// Package bot orchestrates inbound events: dedupe → identity → consent →
// command routing → pending-action resolution. It never runs extraction;
// images become queue jobs so webhook acknowledgements stay fast.
package bot

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/categorisation"
	"zl-expese-bot/internal/config"
	"zl-expese-bot/internal/conversation"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/insight"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/store"
)

// zaloScope is the provider_scope for identity rows. A single bot serves the
// pilot; multi-bot deployments must scope by bot ID instead.
const zaloScope = string(domain.ProviderZaloBot)

// consentVersion identifies the consent text shown at onboarding; bump when
// the copy changes so re-consent can be enforced.
const consentVersion = "consent-v1"

// pendingTTL bounds how long a chat state survives unanswered.
const pendingTTL = 24 * time.Hour

// inboundClaimLease bounds an inline webhook attempt. A retry before the
// lease expires receives an error so the provider keeps retrying; after the
// lease it can recover a process crash.
const inboundClaimLease = 2 * time.Minute

// redactedPayload is stored for allowlist rejects and pre-consent messages so
// text/media URLs never persist before authorization.
var redactedPayload = []byte(`{"redacted":true}`)

// Handler routes normalised inbound events.
type Handler struct {
	st       *store.Store
	pool     *pgxpool.Pool
	q        queue.Queue
	objects  objectstore.Store
	replies  *notify.Enqueuer
	cats     *categorisation.Service
	insights *insight.Service
	clk      clock.Clock
	log      *slog.Logger
	cfg      config.Config
}

// NewHandler wires the handler. cfg supplies the allowlist, data directory
// and per-user receipt limit.
func NewHandler(st *store.Store, pool *pgxpool.Pool, q queue.Queue, objects objectstore.Store, replies *notify.Enqueuer, cats *categorisation.Service, insights *insight.Service, clk clock.Clock, log *slog.Logger, cfg config.Config) *Handler {
	return &Handler{
		st: st, pool: pool, q: q, objects: objects, replies: replies,
		cats: cats, insights: insights, clk: clk, log: log, cfg: cfg,
	}
}

// HandleEvent processes one inbound event end to end. Completed provider
// duplicates are absorbed; failed or abandoned attempts are safely reclaimed.
func (h *Handler) HandleEvent(ctx context.Context, ev events.InboundEvent) error {
	raw := ev.RawPayload
	opts := store.ClaimProviderMessageOpts{}
	if !h.cfg.Allowlisted(ev.ProviderUserID) {
		// Never persist text/media for rejected senders; keep the original
		// hash so retries remain idempotent.
		raw = redactedPayload
	} else {
		existing, err := h.st.GetUserByIdentity(ctx, domain.ProviderZaloBot, ev.ProviderUserID, zaloScope)
		switch {
		case err == nil && existing.Status == domain.UserPending && !isConsentRelated(ev):
			opts.Redact = true
		case domain.IsCode(err, domain.CodeNotFound) && !isConsentRelated(ev):
			// First contact creates a pending user; redact in the same claim TX.
			opts.Redact = true
		case err != nil && !domain.IsCode(err, domain.CodeNotFound):
			return err
		}
	}
	pm := &domain.ProviderMessage{
		ID:                uuid.New(),
		Provider:          domain.Provider(ev.Provider),
		ProviderChatID:    ev.ProviderChatID,
		ProviderMessageID: ev.ProviderMessageID,
		EventType:         ev.EventType,
		PayloadHash:       ev.RawPayloadHash,
		RawPayload:        raw,
		ReceivedAt:        ev.ReceivedAt,
	}
	claimed, err := h.st.ClaimProviderMessageOpts(ctx, pm, inboundClaimLease, opts)
	if err != nil {
		return err
	}
	if !claimed {
		h.log.Info("duplicate provider message absorbed",
			slog.String("provider", ev.Provider),
			slog.String("event_type", ev.EventType))
		return nil
	}

	err = h.route(ctx, ev, pm)
	status := domain.MessageProcessed
	if err != nil {
		status = domain.MessageFailed
	}
	if merr := h.st.MarkProviderMessage(ctx, pm.ID, status); merr != nil {
		// Full account deletion purges the very webhook row that requested it.
		// The deleted tombstone proves this is intentional, not a lost write.
		if err == nil && domain.IsCode(merr, domain.CodeNotFound) && pm.UserID != nil {
			if user, uerr := h.st.GetUser(ctx, *pm.UserID); uerr == nil &&
				(user.Status == domain.UserDeleted || user.DeletedAt != nil) {
				return nil
			}
		}
		if err == nil {
			return merr
		}
		h.log.Warn("mark failed provider message failed", slog.String("error", merr.Error()))
	}
	return err
}

// route resolves identity and dispatches by user status and event type.
func (h *Handler) route(ctx context.Context, ev events.InboundEvent, pm *domain.ProviderMessage) error {
	user, err := h.resolveUser(ctx, ev)
	if err != nil {
		return err
	}
	pm.UserID = &user.ID
	if err := h.st.SetProviderMessageUser(ctx, pm.ID, user.ID); err != nil {
		return err
	}
	_ = h.st.TouchIdentityLastSeen(ctx, domain.ProviderZaloBot, ev.ProviderUserID, zaloScope)

	return h.st.WithUserLock(ctx, user.ID, func(lockedCtx context.Context) error {
		// Deletion may have completed after identity resolution but before the
		// lock. Never recreate user data from an in-flight webhook.
		lockedUser, err := h.st.GetUser(lockedCtx, user.ID)
		if err != nil {
			return err
		}
		if lockedUser.Status == domain.UserDeleted || lockedUser.Status == domain.UserDeleting || lockedUser.DeletedAt != nil {
			return nil
		}

		if !h.cfg.Allowlisted(ev.ProviderUserID) {
			if rerr := h.st.RedactProviderMessageRaw(lockedCtx, pm.ID); rerr != nil {
				h.log.Warn("redact rejected-sender payload failed", slog.Any("error", rerr))
			}
			return h.reply(lockedCtx, lockedUser.ID, ev, conversation.NotAllowedText(), "allow:"+pm.ID.String())
		}

		switch lockedUser.Status {
		case domain.UserSuspended:
			return h.reply(lockedCtx, lockedUser.ID, ev, conversation.SuspendedText(), "susp:"+pm.ID.String())
		case domain.UserPending:
			return h.consentFlow(lockedCtx, lockedUser, ev)
		}

		switch ev.EventType {
		case domain.EventImageReceived:
			return h.handleImage(lockedCtx, lockedUser, ev, pm)
		case domain.EventTextReceived:
			return h.handleText(lockedCtx, lockedUser, ev, pm)
		default:
			return h.reply(lockedCtx, lockedUser.ID, ev, conversation.UnsupportedEventText(), "unsup:"+pm.ID.String())
		}
	})
}

// resolveUser loads or creates the user behind a provider subject. User and
// identity are inserted in one transaction so a concurrent first contact
// cannot leave an orphan users row.
func (h *Handler) resolveUser(ctx context.Context, ev events.InboundEvent) (*domain.User, error) {
	user, err := h.st.GetUserByIdentity(ctx, domain.ProviderZaloBot, ev.ProviderUserID, zaloScope)
	if err == nil {
		return user, nil
	}
	if !domain.IsCode(err, domain.CodeNotFound) {
		return nil, err
	}
	userID := uuid.New()
	user, err = h.st.CreateUserWithIdentity(ctx, userID, domain.ProviderZaloBot, ev.ProviderUserID, zaloScope)
	if err == nil {
		return user, nil
	}
	if domain.IsCode(err, domain.CodeConflict) {
		return h.st.GetUserByIdentity(ctx, domain.ProviderZaloBot, ev.ProviderUserID, zaloScope)
	}
	return nil, err
}

// isConsentRelated reports intents that pending users may send without
// redacting the stored payload (consent confirm, start, privacy).
func isConsentRelated(ev events.InboundEvent) bool {
	if ev.EventType != domain.EventTextReceived {
		return false
	}
	switch conversation.Parse(ev.Text).Kind {
	case conversation.IntentConfirm, conversation.IntentStart, conversation.IntentPrivacy:
		return true
	}
	return false
}

// consentFlow gates pending users: only consent and privacy queries work
// until consent_version is recorded (plan §11.1).
func (h *Handler) consentFlow(ctx context.Context, user *domain.User, ev events.InboundEvent) error {
	if ev.EventType == domain.EventTextReceived {
		switch conversation.Parse(ev.Text).Kind {
		case conversation.IntentConfirm, conversation.IntentStart:
			if err := h.st.SetConsent(ctx, user.ID, consentVersion, h.clk.Now()); err != nil &&
				!domain.IsCode(err, domain.CodeConflict) {
				return err
			}
			return h.reply(ctx, user.ID, ev, conversation.WelcomeText(), "welcome:"+ev.ProviderMessageID)
		case conversation.IntentPrivacy:
			return h.reply(ctx, user.ID, ev, conversation.PrivacyText(), "privacy:"+ev.ProviderMessageID)
		}
	}
	return h.reply(ctx, user.ID, ev, conversation.ConsentCard(), "consent:"+ev.ProviderMessageID)
}

// reply enqueues one outbound message; the notification worker sends it.
func (h *Handler) reply(ctx context.Context, userID uuid.UUID, ev events.InboundEvent, body, idem string) error {
	return h.replies.Reply(ctx, userID, domain.Provider(ev.Provider), ev.ProviderChatID, body, idem)
}

// loc returns the user's timezone, UTC on misconfiguration.
func loc(user *domain.User) *time.Location {
	if l, err := time.LoadLocation(user.Timezone); err == nil {
		return l
	}
	return time.UTC
}
