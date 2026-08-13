// Package receipt owns the receipt-processing pipeline worker: download →
// validate → store → duplicate check → extract → normalise → draft
// transaction → confirmation card. Every step is idempotent: a replayed job
// resumes from the receipt's persisted state instead of redoing work.
package receipt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/categorisation"
	"zl-expese-bot/internal/conversation"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction"
	"zl-expese-bot/internal/extraction/normalise"
	"zl-expese-bot/internal/messaging"
	"zl-expese-bot/internal/notify"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/objectstore"
	"zl-expese-bot/internal/store"
)

// Processor handles receipt_process queue jobs.
type Processor struct {
	st        *store.Store
	objects   objectstore.Store
	extractor extraction.Extractor
	provider  messaging.Provider
	cats      *categorisation.Service
	replies   *notify.Enqueuer
	clk       clock.Clock
	log       *slog.Logger

	// ExtractionEnabled is the emergency OCR kill switch.
	ExtractionEnabled bool
	// MonthlyOCRLimit caps paid extraction calls per calendar month.
	MonthlyOCRLimit int64
	// MaxImageBytes bounds the validated image size.
	MaxImageBytes int64
}

// NewProcessor wires the pipeline dependencies.
func NewProcessor(st *store.Store, objects objectstore.Store, ex extraction.Extractor, p messaging.Provider, cats *categorisation.Service, replies *notify.Enqueuer, clk clock.Clock, log *slog.Logger) *Processor {
	return &Processor{
		st: st, objects: objects, extractor: ex, provider: p, cats: cats,
		replies: replies, clk: clk, log: log,
		ExtractionEnabled: true, MonthlyOCRLimit: 80, MaxImageBytes: 10 << 20,
	}
}

// Handle processes one receipt job. Permanent failures are persisted to the
// receipt (failed_permanent) and answered with a user-facing message, then
// return nil so the worker Acks. Transient failures mark the receipt
// failed_transient and return CodeTransient so the worker Nacks with
// backoff; the retry resumes through the state machine.
func (p *Processor) Handle(ctx context.Context, job *domain.QueueJob) error {
	var payload events.ReceiptJob
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return domain.E(domain.CodeValidation, "decode receipt job", err)
	}
	return p.st.WithUserLock(ctx, payload.UserID, func(lockedCtx context.Context) error {
		user, err := p.st.GetUser(lockedCtx, payload.UserID)
		if err != nil {
			if domain.IsCode(err, domain.CodeNotFound) {
				return nil
			}
			return err
		}
		if user.Status == domain.UserDeleted || user.DeletedAt != nil {
			return nil
		}
		err = p.handleLocked(lockedCtx, job, payload)
		return p.ackPolicy(lockedCtx, payload.ReceiptID, err)
	})
}

func (p *Processor) handleLocked(ctx context.Context, job *domain.QueueJob, payload events.ReceiptJob) error {
	r, err := p.st.GetReceipt(ctx, payload.ReceiptID)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			return nil // receipt deleted while queued; nothing to do
		}
		return err
	}
	if r.UserID != payload.UserID {
		return domain.E(domain.CodeValidation, "receipt job user does not own receipt", nil)
	}

	// Idempotent resume: terminal states are done; mid-pipeline stages
	// continue from their checkpoint after a crash or lease reclaim.
	switch r.Status {
	case domain.ReceiptReviewRequired, domain.ReceiptConfirmed, domain.ReceiptDeleted,
		domain.ReceiptFailedPermanent:
		return nil
	case domain.ReceiptFailedTransient:
		if err := p.st.TransitionReceipt(ctx, r.ID, domain.ReceiptFailedTransient, domain.ReceiptQueued); err != nil {
			return p.conflictAsDone(ctx, r.ID, err)
		}
		r.Status = domain.ReceiptQueued
	case domain.ReceiptQueued, domain.ReceiptDownloading, domain.ReceiptStored, domain.ReceiptExtracting:
		// fall through and resume
	default:
		return nil // received or unknown — nothing for this job to do
	}

	// Dequeue already incremented attempts; use that as the attempt number.
	attempt := job.Attempts
	if attempt < 1 {
		attempt = 1
	}
	if err := p.st.RecordAttempt(ctx, r.ID, p.extractor.Name(), p.extractor.Version(), attempt, "started", "", ""); err != nil {
		p.log.Warn("record attempt start failed", slog.String("error", err.Error()))
	}

	data, contentType, err := p.ensureStoredImage(ctx, r)
	if err != nil {
		if domain.IsCode(err, domain.CodeConflict) {
			return p.conflictAsDone(ctx, r.ID, err)
		}
		if domain.IsCode(err, domain.CodeValidation) && strings.Contains(err.Error(), "duplicate receipt") {
			return p.failWith(ctx, r, payload, attempt, "duplicate", conversation.DuplicateReceiptText())
		}
		return p.fail(ctx, r, payload, attempt, err, conversation.UnsupportedImageText())
	}
	// Reload after stage transitions inside ensureStoredImage.
	if latest, lerr := p.st.GetReceipt(ctx, r.ID); lerr == nil {
		r = latest
	}

	if r.Status == domain.ReceiptStored {
		if err := p.st.TransitionReceipt(ctx, r.ID, domain.ReceiptStored, domain.ReceiptExtracting); err != nil {
			return p.conflictAsDone(ctx, r.ID, err)
		}
		r.Status = domain.ReceiptExtracting
	}

	if r.Status != domain.ReceiptExtracting {
		return domain.Ef(domain.CodeInternal, nil, "receipt %s not extracting after store", r.ID)
	}

	// Kill switch and monthly OCR quota, enforced in code (plan §13.1).
	if !p.ExtractionEnabled {
		return p.failWith(ctx, r, payload, attempt, "kill_switch", conversation.OCRDisabledText())
	}
	period := p.clk.Now().Format("2006-01")
	count, err := p.st.IncrementUsage(ctx, "global", "", period, "ocr_pages", 1, p.MonthlyOCRLimit)
	if err != nil {
		return p.fail(ctx, r, payload, attempt, err, "")
	}
	if p.MonthlyOCRLimit > 0 && count > p.MonthlyOCRLimit {
		return p.failWith(ctx, r, payload, attempt, "quota", conversation.MonthlyQuotaText())
	}
	if p.MonthlyOCRLimit > 0 {
		if pct := count * 100 / p.MonthlyOCRLimit; pct == 70 || pct == 85 || pct == 95 {
			p.log.Warn("OCR monthly quota threshold",
				slog.Int64("count", count), slog.Int64("limit", p.MonthlyOCRLimit), slog.Int64("pct", pct))
		}
	}

	result, err := p.extractor.Extract(ctx, bytes.NewReader(data), extraction.Input{
		ReceiptID:   r.ID.String(),
		SHA256:      shaOf(data),
		ContentType: contentType,
	})
	if err != nil {
		if domain.IsCode(err, domain.CodeUnsupported) {
			return p.failWith(ctx, r, payload, attempt, "unsupported", conversation.UnsupportedImageText())
		}
		return p.fail(ctx, r, payload, attempt, err, "")
	}

	tx, sug, err := p.buildAndPersistDraft(ctx, r, result)
	if err != nil {
		if domain.IsCode(err, domain.CodeConflict) {
			return p.conflictAsDone(ctx, r.ID, err)
		}
		return p.fail(ctx, r, payload, attempt, err, "")
	}

	card := p.renderCard(ctx, r.UserID, tx, result, sug)
	if err := p.replies.Reply(ctx, r.UserID, domain.Provider(payload.Provider), payload.ProviderChatID, card,
		fmt.Sprintf("card:%s:v%d", tx.ID, tx.Version)); err != nil {
		return err
	}

	// Soft-duplicate warning (P4-C01): exact-hash duplicates were stopped
	// before storage; near-identical recorded transactions earn a gentle
	// follow-up. Never auto-deletes — the user decides. A lookup failure
	// must not fail the job: the card already landed.
	if err := p.warnPossibleDuplicate(ctx, r.UserID, payload, tx); err != nil {
		p.log.Warn("soft duplicate check failed", slog.Any("error", err))
	}

	if err := p.st.RecordAttempt(ctx, r.ID, p.extractor.Name(), p.extractor.Version(), attempt, "succeeded", "", ""); err != nil {
		p.log.Warn("record attempt success failed", slog.String("error", err.Error()))
	}
	return nil
}

// ensureStoredImage resumes download/store checkpoints until the receipt has
// a durable object and status stored (or already extracting with a key).
func (p *Processor) ensureStoredImage(ctx context.Context, r *domain.ReceiptDocument) ([]byte, string, error) {
	if r.Status == domain.ReceiptExtracting && r.StorageKey != "" {
		return p.readStored(ctx, r)
	}
	if r.Status == domain.ReceiptStored && r.StorageKey != "" {
		return p.readStored(ctx, r)
	}

	if r.Status == domain.ReceiptQueued {
		if err := p.st.TransitionReceipt(ctx, r.ID, domain.ReceiptQueued, domain.ReceiptDownloading); err != nil {
			return nil, "", err
		}
		r.Status = domain.ReceiptDownloading
	}

	data, contentType, err := p.fetchAndValidate(ctx, r)
	if err != nil {
		return nil, "", err
	}

	// Duplicate content check before any paid work (plan §13.1).
	if dup, derr := p.st.FindReceiptByHash(ctx, r.UserID, shaOf(data)); derr == nil && dup.ID != r.ID {
		return nil, "", domain.E(domain.CodeValidation, "duplicate receipt image", nil)
	} else if derr != nil && !domain.IsCode(derr, domain.CodeNotFound) {
		return nil, "", derr
	}

	if r.Status == domain.ReceiptDownloading {
		if err := p.st.TransitionReceipt(ctx, r.ID, domain.ReceiptDownloading, domain.ReceiptStored); err != nil {
			return nil, "", err
		}
		r.Status = domain.ReceiptStored
	}

	stored, err := p.objects.Put(ctx, StorageKey(r.UserID, r.ID), bytes.NewReader(data), contentType)
	if err != nil {
		return nil, "", domain.E(domain.CodeTransient, "object store put", err)
	}
	if err := p.st.SetReceiptStored(ctx, r.ID, stored.Key, stored.SHA256, contentType, stored.ByteSize); err != nil {
		return nil, "", domain.E(domain.CodeTransient, "record stored receipt", err)
	}
	r.StorageKey = stored.Key
	r.SHA256 = stored.SHA256
	r.ContentType = contentType
	r.ByteSize = stored.ByteSize
	return data, contentType, nil
}

func (p *Processor) readStored(ctx context.Context, r *domain.ReceiptDocument) ([]byte, string, error) {
	rc, err := p.objects.Open(ctx, r.StorageKey)
	if err != nil {
		return nil, "", domain.E(domain.CodeTransient, "object store open", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, p.MaxImageBytes+1))
	if err != nil {
		return nil, "", domain.E(domain.CodeTransient, "read stored receipt", err)
	}
	if int64(len(data)) > p.MaxImageBytes {
		return nil, "", domain.Ef(domain.CodeValidation, nil, "stored receipt exceeds %d byte cap", p.MaxImageBytes)
	}
	ct := r.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	return data, ct, nil
}

// dupWindow bounds the soft-duplicate search around the transaction time.
const dupWindow = 72 * time.Hour // ±3 days (P4-C01)

// warnPossibleDuplicate sends the "có thể trùng" follow-up when a
// confirmed/amended transaction matches the new draft on amount, currency,
// merchant and a ±3-day window.
func (p *Processor) warnPossibleDuplicate(ctx context.Context, userID uuid.UUID, payload events.ReceiptJob, tx *domain.Transaction) error {
	dups, err := p.st.FindPotentialDuplicates(ctx, userID, tx.AmountMinor, tx.Currency, tx.OccurredAt, dupWindow, tx.MerchantID, tx.MerchantName)
	if err != nil {
		return err
	}
	if len(dups) == 0 {
		return nil
	}
	loc := time.UTC
	if user, err := p.st.GetUser(ctx, userID); err == nil {
		if l, lerr := time.LoadLocation(user.Timezone); lerr == nil {
			loc = l
		}
	}
	lines := make([]conversation.DupLine, 0, len(dups))
	for _, d := range dups {
		lines = append(lines, conversation.DupLine{
			Amount:   conversation.Money(d.AmountMinor, d.Currency),
			Merchant: d.MerchantName,
			Date:     conversation.DateVN(d.OccurredAt, loc),
		})
	}
	return p.replies.Reply(ctx, userID, domain.Provider(payload.Provider), payload.ProviderChatID,
		conversation.PossibleDuplicateText(lines), fmt.Sprintf("dupnote:%s:v%d", tx.ID, tx.Version))
}

// fetchAndValidate recovers the media reference from the retained provider
// payload, downloads with provider guards, and validates image bytes.
func (p *Processor) fetchAndValidate(ctx context.Context, r *domain.ReceiptDocument) ([]byte, string, error) {
	if r.ProviderMessageID == nil {
		return nil, "", domain.E(domain.CodeValidation, "receipt has no provider message", nil)
	}
	pm, err := p.st.GetProviderMessage(ctx, *r.ProviderMessageID)
	if err != nil {
		return nil, "", err
	}
	evs, err := p.provider.ParseWebhook(ctx, pm.RawPayload)
	if err != nil {
		return nil, "", domain.E(domain.CodeValidation, "re-parse provider payload", err)
	}
	var ref *events.MediaReference
	for i := range evs {
		if len(evs[i].Media) > 0 {
			ref = &evs[i].Media[0]
			break
		}
	}
	if ref == nil {
		return nil, "", domain.E(domain.CodeValidation, "provider payload has no media", nil)
	}
	rc, _, err := p.provider.DownloadMedia(ctx, *ref)
	if err != nil {
		return nil, "", err // adapter already classifies transient/permanent
	}
	defer rc.Close()
	data, _, contentType, err := ValidateAndHash(rc, p.MaxImageBytes)
	if err != nil {
		return nil, "", err
	}
	return data, contentType, nil
}

// buildAndPersistDraft turns extraction into a transactional review draft.
func (p *Processor) buildAndPersistDraft(ctx context.Context, r *domain.ReceiptDocument, result events.ExtractionResult) (*domain.Transaction, categorisation.Suggestion, error) {
	user, err := p.st.GetUser(ctx, r.UserID)
	if err != nil {
		return nil, categorisation.Suggestion{}, err
	}
	loc, lerr := time.LoadLocation(user.Timezone)
	if lerr != nil {
		loc = time.UTC
	}

	merchantRaw := fieldRaw(result, "merchant")
	var merchant *domain.Merchant
	var matchKind categorisation.MatchKind
	if merchantRaw != "" {
		merchant, matchKind, err = p.cats.ResolveMerchant(ctx, merchantRaw)
		if err != nil {
			return nil, categorisation.Suggestion{}, err
		}
	}

	total, err := fieldInt(result, "total_minor")
	if err != nil {
		return nil, categorisation.Suggestion{}, err
	}
	currency := fieldString(result, "currency")
	if currency == "" {
		currency = user.DefaultCurrency
	}
	occurred, err := fieldTime(result, "occurred_at", loc)
	if err != nil {
		occurred = p.clk.Now() // unknown date: now is safer than skipping the receipt
	}

	hint, hintMatched := normalise.TypeHint(merchantRaw + " " + fieldRaw(result, "type_hint"))
	txType, typeConf, typeSource := p.cats.SuggestType(ctx, r.UserID, merchant, hint, hintMatched)
	sug, err := p.cats.SuggestCategory(ctx, r.UserID, merchant, fieldString(result, "category"), fieldConf(result, "category"))
	if err != nil {
		return nil, categorisation.Suggestion{}, err
	}

	merchantDisplay := ""
	var merchantID *uuid.UUID
	if merchant != nil {
		merchantDisplay = merchant.CanonicalName
		merchantID = &merchant.ID
	} else if d, _ := normalise.NormaliseMerchant(merchantRaw); d != "" {
		merchantDisplay = d
	}

	now := p.clk.Now()
	tx := &domain.Transaction{
		ID: uuid.New(), UserID: r.UserID, ReceiptDocumentID: &r.ID,
		Type: txType, MerchantID: merchantID, MerchantName: merchantDisplay,
		AmountMinor: total, Currency: currency, OccurredAt: occurred,
		CategoryID: sug.CategoryID,
		Status:     domain.TxAwaitingConfirmation, Source: domain.TxSourceReceipt,
		ConfidenceSummary: confidenceSummary(result),
		Version:           1,
		CreatedAt:         now, UpdatedAt: now,
	}
	preds := []domain.Prediction{
		{ID: uuid.New(), TransactionID: tx.ID, PredictionType: "category",
			PredictedValue: sug.CategoryKey, Confidence: sug.Confidence,
			ModelName: sug.Source, ModelVersion: "v1", CreatedAt: now},
		{ID: uuid.New(), TransactionID: tx.ID, PredictionType: "type",
			PredictedValue: string(txType), Confidence: typeConf,
			ModelName: typeSource, ModelVersion: "v1", CreatedAt: now},
	}
	if merchant != nil {
		preds = append(preds, domain.Prediction{ID: uuid.New(), TransactionID: tx.ID,
			PredictionType: "merchant", PredictedValue: merchant.NormalisedName,
			Confidence: fieldConf(result, "merchant"), ModelName: "merchant-resolution",
			ModelVersion: string(matchKind), CreatedAt: now})
	}
	expires := now.Add(24 * time.Hour)
	if err := p.st.FinalizeReceiptReview(ctx, store.ReceiptReviewDraft{
		FromStatus:  domain.ReceiptExtracting,
		Transaction: tx,
		Fields:      provenance(r.ID, result),
		Predictions: preds,
		Pending: &domain.PendingAction{
			UserID: r.UserID, Kind: domain.PendingConfirmExtraction,
			TransactionID: &tx.ID, ExpiresAt: expires,
		},
	}); err != nil {
		return nil, categorisation.Suggestion{}, err
	}
	r.Status = domain.ReceiptReviewRequired
	return tx, sug, nil
}

// fail records a transient failure and asks the worker for a retry. For
// permanent causes it delegates to failWith; an empty msg becomes the
// generic failure notice so the user is never left silent.
func (p *Processor) fail(ctx context.Context, r *domain.ReceiptDocument, payload events.ReceiptJob, attempt int, cause error, msg string) error {
	if domain.Retryable(cause) {
		_ = p.st.TransitionReceipt(ctx, r.ID, r.Status, domain.ReceiptFailedTransient)
		_ = p.st.RecordAttempt(ctx, r.ID, p.extractor.Name(), p.extractor.Version(), attempt, "failed", "transient", string(domain.CodeOf(cause)))
		return domain.E(domain.CodeTransient, "receipt pipeline", cause)
	}
	if msg == "" {
		msg = conversation.ExtractionFailedText()
	}
	return p.failWith(ctx, r, payload, attempt, string(domain.CodeOf(cause)), msg)
}

// failWith records a permanent failure and notifies the user. It returns
// nil only after the receipt is in a terminal status so the worker can ACK.
func (p *Processor) failWith(ctx context.Context, r *domain.ReceiptDocument, payload events.ReceiptJob, attempt int, class, msg string) error {
	if err := p.st.TransitionReceipt(ctx, r.ID, r.Status, domain.ReceiptFailedPermanent); err != nil {
		if !domain.IsCode(err, domain.CodeConflict) && !domain.IsCode(err, domain.CodeValidation) {
			return domain.E(domain.CodeTransient, "persist permanent receipt failure", err)
		}
		latest, lerr := p.st.GetReceipt(ctx, r.ID)
		if lerr != nil || !receiptTerminal(latest.Status) {
			return domain.E(domain.CodeTransient, "permanent receipt failure not durable", err)
		}
	} else {
		r.Status = domain.ReceiptFailedPermanent
	}
	_ = p.st.RecordAttempt(ctx, r.ID, p.extractor.Name(), p.extractor.Version(), attempt, "failed", "permanent", class)
	if msg != "" {
		if err := p.replies.Reply(ctx, r.UserID, domain.Provider(payload.Provider), payload.ProviderChatID, msg,
			fmt.Sprintf("fail:%s:%s", r.ID, class)); err != nil {
			return err
		}
	}
	return nil
}

func receiptTerminal(status domain.ReceiptStatus) bool {
	switch status {
	case domain.ReceiptReviewRequired, domain.ReceiptConfirmed, domain.ReceiptDeleted,
		domain.ReceiptFailedPermanent:
		return true
	default:
		return false
	}
}

// ackPolicy converts a permanent handler error into a retry when the receipt
// is not yet in a terminal status, so the worker NACKs instead of ACKing.
func (p *Processor) ackPolicy(ctx context.Context, receiptID uuid.UUID, err error) error {
	if err == nil || domain.Retryable(err) {
		return err
	}
	r, gerr := p.st.GetReceipt(ctx, receiptID)
	if gerr != nil || r == nil || !receiptTerminal(r.Status) {
		return domain.E(domain.CodeTransient, "permanent failure not yet durable", err)
	}
	return nil
}

// conflictAsDone treats a state conflict as someone else's progress: the
// job is a duplicate of work another lane is doing.
func (p *Processor) conflictAsDone(ctx context.Context, receiptID uuid.UUID, err error) error {
	if domain.IsCode(err, domain.CodeConflict) || domain.IsCode(err, domain.CodeValidation) {
		p.log.Info("receipt moved on; dropping duplicate job",
			slog.String("receipt_id", receiptID.String()))
		return nil
	}
	return err
}

// renderCard builds the confirmation card with per-field ⚠️ flags.
func (p *Processor) renderCard(ctx context.Context, userID uuid.UUID, tx *domain.Transaction, result events.ExtractionResult, sug categorisation.Suggestion) string {
	loc := time.UTC
	if user, err := p.st.GetUser(ctx, userID); err == nil {
		if l, lerr := time.LoadLocation(user.Timezone); lerr == nil {
			loc = l
		}
	}
	merchant := tx.MerchantName
	if merchant == "" {
		merchant = fieldRaw(result, "merchant")
	}
	return conversation.ExtractionCard(conversation.CardData{
		Merchant: merchant,
		Amount:   conversation.Money(tx.AmountMinor, tx.Currency),
		Date:     conversation.DateVN(tx.OccurredAt, loc),
		Type:     conversation.TypeLabel(tx.Type),
		Category: sug.DisplayName,
		Flags: map[string]bool{
			"total":    extraction.FieldNeedsFlag("total_minor", fieldConf(result, "total_minor")),
			"currency": extraction.FieldNeedsFlag("currency", fieldConf(result, "currency")),
			"date":     extraction.FieldNeedsFlag("occurred_at", fieldConf(result, "occurred_at")),
			"merchant": extraction.FieldNeedsFlag("merchant", fieldConf(result, "merchant")),
			"category": extraction.FieldNeedsFlag("category", sug.Confidence),
		},
	})
}
