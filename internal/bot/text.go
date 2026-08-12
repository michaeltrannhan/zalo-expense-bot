package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/conversation"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/normalise"
)

// handleText resolves pending chat state first, then fresh intents. Slash
// commands always win over a pending action — asking for /homnay must not
// be swallowed by a stale confirmation card.
func (h *Handler) handleText(ctx context.Context, user *domain.User, ev events.InboundEvent, pm *domain.ProviderMessage) error {
	intent := conversation.Parse(ev.Text)

	if pa, err := h.st.GetPendingAction(ctx, user.ID); err == nil {
		if !isSlashIntent(intent.Kind) {
			if h.resolvePending(ctx, user, ev, pm, pa, intent) {
				return nil
			}
		}
	} else if !domain.IsCode(err, domain.CodeNotFound) {
		return err
	}

	switch intent.Kind {
	case conversation.IntentStart, conversation.IntentHelp:
		return h.reply(ctx, user.ID, ev, conversation.HelpText(), "cmd:"+pm.ID.String())
	case conversation.IntentPrivacy:
		return h.reply(ctx, user.ID, ev, conversation.PrivacyText(), "cmd:"+pm.ID.String())
	case conversation.IntentToday:
		return h.summary(ctx, user, ev, pm, periodToday)
	case conversation.IntentWeek:
		return h.summary(ctx, user, ev, pm, periodWeek)
	case conversation.IntentMonth:
		return h.summary(ctx, user, ev, pm, periodMonth)
	case conversation.IntentLastWeek:
		return h.summary(ctx, user, ev, pm, periodLastWeek)
	case conversation.IntentLastMonth:
		return h.summary(ctx, user, ev, pm, periodLastMonth)
	case conversation.IntentRecent:
		return h.recent(ctx, user, ev, pm)
	case conversation.IntentBudget:
		return h.reply(ctx, user.ID, ev, conversation.BudgetUnsupportedText(), "cmd:"+pm.ID.String())
	case conversation.IntentExport:
		return h.export(ctx, user, ev)
	case conversation.IntentDeleteData:
		return h.requestDelete(ctx, user, ev)
	case conversation.IntentSettings:
		return h.settings(ctx, user, ev, intent)
	case conversation.IntentSummarySchedule:
		return h.summarySchedule(ctx, user, ev, intent)
	case conversation.IntentManualEntry:
		return h.manualEntry(ctx, user, ev, intent)
	case conversation.IntentRecategory:
		return h.recategorise(ctx, user, ev, intent.CategoryText)
	case conversation.IntentDeleteRecent:
		return h.requestDeleteRecent(ctx, user, ev)
	case conversation.IntentConfirm, conversation.IntentDiscard,
		conversation.IntentEditTotal, conversation.IntentEditMerchant,
		conversation.IntentEditDate, conversation.IntentEditCategory,
		conversation.IntentEditType:
		// A confirmation verb with nothing pending.
		return h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "cmd:"+pm.ID.String())
	default:
		return h.reply(ctx, user.ID, ev, conversation.UnknownText(), "cmd:"+pm.ID.String())
	}
}

// isSlashIntent reports whether the intent came from an explicit command,
// which always bypasses pending-action resolution.
func isSlashIntent(k conversation.IntentKind) bool {
	switch k {
	case conversation.IntentStart, conversation.IntentHelp,
		conversation.IntentToday, conversation.IntentWeek, conversation.IntentMonth,
		conversation.IntentLastWeek, conversation.IntentLastMonth,
		conversation.IntentRecent, conversation.IntentBudget,
		conversation.IntentExport, conversation.IntentDeleteData,
		conversation.IntentSettings,
		conversation.IntentSummarySchedule:
		return true
	}
	return false
}

// resolvePending interprets text against the user's open pending action.
// true = handled. The pending slot is replaced or cleared as flows advance.
func (h *Handler) resolvePending(ctx context.Context, user *domain.User, ev events.InboundEvent, pm *domain.ProviderMessage, pa *domain.PendingAction, intent conversation.Intent) bool {
	switch pa.Kind {
	case domain.PendingConfirmExtraction:
		return h.resolveConfirm(ctx, user, ev, pa, intent)
	case domain.PendingEditTotal, domain.PendingEditMerchant, domain.PendingEditDate,
		domain.PendingEditCategory, domain.PendingEditType:
		return h.resolveEdit(ctx, user, ev, pa)
	case domain.PendingDeleteAccount:
		return h.resolveDelete(ctx, user, ev, pm, pa, intent)
	case domain.PendingDeleteRecent:
		return h.resolveDeleteRecent(ctx, user, ev, pa, intent)
	}
	return false
}

// resolveConfirm handles replies to the extraction card.
func (h *Handler) resolveConfirm(ctx context.Context, user *domain.User, ev events.InboundEvent, pa *domain.PendingAction, intent conversation.Intent) bool {
	txID := pa.TransactionID
	if txID == nil {
		_ = h.st.ClearPendingAction(ctx, user.ID)
		return false
	}
	switch intent.Kind {
	case conversation.IntentConfirm:
		h.must(h.confirmTx(ctx, user, ev, *txID))
		return true
	case conversation.IntentDiscard:
		h.must(h.discardTx(ctx, user, ev, *txID))
		return true
	case conversation.IntentEditTotal, conversation.IntentEditMerchant,
		conversation.IntentEditDate, conversation.IntentEditCategory,
		conversation.IntentEditType:
		kind := domain.PendingKind(intent.Kind)
		prompt := conversation.EditPrompt(kind)
		if kind == domain.PendingEditCategory {
			if cats, err := h.st.ListCategories(ctx); err == nil {
				prompt = conversation.CategoryListText(cats)
			}
		} else if kind == domain.PendingEditType {
			prompt = conversation.TypeListText()
		}
		if err := h.st.SetPendingAction(ctx, &domain.PendingAction{
			UserID: user.ID, Kind: kind, TransactionID: txID,
			ExpiresAt: h.clk.Now().Add(pendingTTL),
		}); err != nil {
			h.log.Warn("set pending edit failed", slog.Any("error", err))
		}
		h.must(h.reply(ctx, user.ID, ev, prompt, "editp:"+ev.ProviderMessageID))
		return true
	case conversation.IntentNone:
		// Unrecognised reply to a card: re-show it rather than guessing.
		h.must(h.reshowCard(ctx, user, ev, *txID))
		return true
	}
	return false
}

// resolveEdit applies one field correction, records it, and returns to the
// confirmation card. The transaction must still be awaiting confirmation —
// a confirmed transaction can only change through recategorise.
func (h *Handler) resolveEdit(ctx context.Context, user *domain.User, ev events.InboundEvent, pa *domain.PendingAction) bool {
	txID := pa.TransactionID
	if txID == nil {
		_ = h.st.ClearPendingAction(ctx, user.ID)
		return false
	}
	tx, err := h.st.GetTransactionForUser(ctx, *txID, user.ID)
	if err != nil || tx.Status != domain.TxAwaitingConfirmation {
		_ = h.st.ClearPendingAction(ctx, user.ID)
		h.must(h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "stale:"+ev.ProviderMessageID))
		return true
	}

	text := strings.TrimSpace(ev.Text)
	invalid := func() bool {
		h.must(h.reply(ctx, user.ID, ev, conversation.EditInvalidText(pa.Kind), "editinv:"+ev.ProviderMessageID))
		return true
	}

	var field, predicted, corrected string
	switch pa.Kind {
	case domain.PendingEditTotal:
		minor, currency, err := normalise.ParseAmount(text, tx.Currency)
		if err != nil {
			return invalid()
		}
		field, predicted = "total_minor", fmt.Sprintf("%d %s", tx.AmountMinor, tx.Currency)
		tx.AmountMinor, tx.Currency = minor, currency
		corrected = fmt.Sprintf("%d %s", minor, currency)
	case domain.PendingEditMerchant:
		if text == "" {
			return invalid()
		}
		merchant, _, err := h.cats.ResolveMerchant(ctx, text)
		if err != nil {
			h.must(err)
			return true
		}
		field, predicted = "merchant", tx.MerchantName
		if merchant != nil {
			tx.MerchantID = &merchant.ID
			tx.MerchantName = merchant.CanonicalName
		} else {
			tx.MerchantID = nil
			tx.MerchantName = text
		}
		corrected = tx.MerchantName
	case domain.PendingEditDate:
		when, err := normalise.ParseDate(text, loc(user))
		if err != nil {
			return invalid()
		}
		field, predicted = "occurred_at", tx.OccurredAt.Format("2006-01-02 15:04")
		tx.OccurredAt = when
		corrected = when.Format("2006-01-02 15:04")
	case domain.PendingEditCategory:
		cat, err := h.matchCategory(ctx, text)
		if err != nil {
			return invalid()
		}
		field, predicted = "category", ptrKey(tx.CategoryID)
		tx.CategoryID = &cat.ID
		corrected = cat.SystemKey
	case domain.PendingEditType:
		t, ok := matchType(text)
		if !ok {
			return invalid()
		}
		field, predicted = "type", string(tx.Type)
		tx.Type = t
		corrected = string(t)
	}

	if err := h.st.UpdateTransaction(ctx, tx, tx.Version); err != nil {
		h.must(err)
		return true
	}
	_ = h.st.InsertCorrection(ctx, &domain.Correction{
		TransactionID: tx.ID, FieldName: field,
		PredictedValue: predicted, CorrectedValue: corrected, Source: "chat_edit",
	})
	if err := h.st.SetPendingAction(ctx, &domain.PendingAction{
		UserID: user.ID, Kind: domain.PendingConfirmExtraction, TransactionID: txID,
		ExpiresAt: h.clk.Now().Add(pendingTTL),
	}); err != nil {
		h.log.Warn("restore confirm pending failed", slog.Any("error", err))
	}
	h.must(h.reshowCard(ctx, user, ev, *txID))
	return true
}

// resolveDelete executes or cancels account deletion.
func (h *Handler) resolveDelete(ctx context.Context, user *domain.User, ev events.InboundEvent, pm *domain.ProviderMessage, pa *domain.PendingAction, intent conversation.Intent) bool {
	if intent.Kind != conversation.IntentConfirm {
		_ = h.st.ClearPendingAction(ctx, user.ID)
		h.must(h.reply(ctx, user.ID, ev, conversation.DiscardedText(), "delcancel:"+ev.ProviderMessageID))
		return true
	}
	report, err := h.deleteAccount(ctx, user.ID, pm.ID)
	if err != nil {
		h.must(err)
		return true
	}
	h.must(h.replies.DeletionConfirmation(ctx, user.ID, domain.Provider(ev.Provider), ev.ProviderChatID,
		conversation.DeletedText()+fmt.Sprintf("\n(%d giao dịch, %d ảnh đã xóa)", report.TransactionsDeleted, report.FilesRemoved),
		"deleted:"+ev.ProviderMessageID))
	return true
}

// resolveDeleteRecent executes or cancels individual transaction deletion.
// Only the exact transaction shown in the prompt is ever deleted; anything
// but an explicit confirm keeps it.
func (h *Handler) resolveDeleteRecent(ctx context.Context, user *domain.User, ev events.InboundEvent, pa *domain.PendingAction, intent conversation.Intent) bool {
	if intent.Kind != conversation.IntentConfirm {
		_ = h.st.ClearPendingAction(ctx, user.ID)
		h.must(h.reply(ctx, user.ID, ev, conversation.DeleteRecentCancelText(), "delrecentcancel:"+ev.ProviderMessageID))
		return true
	}
	if pa.TransactionID == nil {
		_ = h.st.ClearPendingAction(ctx, user.ID)
		h.must(h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "stale:"+ev.ProviderMessageID))
		return true
	}
	txID := *pa.TransactionID
	tx, err := h.st.GetTransactionForUser(ctx, txID, user.ID)
	if err != nil {
		_ = h.st.ClearPendingAction(ctx, user.ID)
		h.must(h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "stale:"+ev.ProviderMessageID))
		return true
	}
	if err := h.st.SoftDeleteTransaction(ctx, txID, user.ID); err != nil {
		h.must(err)
		return true
	}
	_ = h.st.InsertCorrection(ctx, &domain.Correction{TransactionID: txID, Source: "chat_delete"})
	_ = h.st.ClearPendingAction(ctx, user.ID)
	h.must(h.reply(ctx, user.ID, ev, conversation.DeletedRecentText(
		conversation.Money(tx.AmountMinor, tx.Currency), tx.MerchantName), "delrecent:"+txID.String()))
	return true
}

// confirmTx finalises a suggested transaction and feeds the learning rule.
func (h *Handler) confirmTx(ctx context.Context, user *domain.User, ev events.InboundEvent, txID uuid.UUID) error {
	tx, err := h.st.GetTransactionForUser(ctx, txID, user.ID)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			_ = h.st.ClearPendingAction(ctx, user.ID)
			return h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "stale:"+ev.ProviderMessageID)
		}
		return err
	}
	if tx.Status != domain.TxAwaitingConfirmation {
		_ = h.st.ClearPendingAction(ctx, user.ID)
		return h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "stale:"+ev.ProviderMessageID)
	}

	now := h.clk.Now()
	tx.Status = domain.TxConfirmed
	tx.ConfirmedAt = &now
	if err := h.st.UpdateTransaction(ctx, tx, tx.Version); err != nil {
		if domain.IsCode(err, domain.CodeConflict) {
			// A racing confirm already won; the outcome the user asked for
			// exists, so acknowledge rather than error.
			_ = h.st.ClearPendingAction(ctx, user.ID)
			return nil
		}
		return err
	}
	if tx.ReceiptDocumentID != nil {
		_ = h.st.TransitionReceipt(ctx, *tx.ReceiptDocumentID, domain.ReceiptReviewRequired, domain.ReceiptConfirmed)
	}
	_ = h.st.InsertCorrection(ctx, &domain.Correction{
		TransactionID: tx.ID, Source: "chat_confirm",
	})
	if err := h.cats.LearnFromConfirmation(ctx, user.ID, tx); err != nil {
		h.log.Warn("learn from confirmation failed", slog.Any("error", err))
	}
	_ = h.st.ClearPendingAction(ctx, user.ID)

	catName := h.categoryName(ctx, tx.CategoryID)
	return h.reply(ctx, user.ID, ev,
		conversation.ConfirmedText(conversation.Money(tx.AmountMinor, tx.Currency), tx.MerchantName, catName),
		"confirm:"+tx.ID.String())
}

// discardTx drops a suggestion: transaction and receipt are deleted.
func (h *Handler) discardTx(ctx context.Context, user *domain.User, ev events.InboundEvent, txID uuid.UUID) error {
	tx, err := h.st.GetTransactionForUser(ctx, txID, user.ID)
	if err == nil && tx.ReceiptDocumentID != nil {
		_ = h.st.TransitionReceipt(ctx, *tx.ReceiptDocumentID, domain.ReceiptReviewRequired, domain.ReceiptDeleted)
	}
	if err := h.st.SoftDeleteTransaction(ctx, txID, user.ID); err != nil {
		if domain.IsCode(err, domain.CodeValidation) || domain.IsCode(err, domain.CodeNotFound) {
			_ = h.st.ClearPendingAction(ctx, user.ID)
			return h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "stale:"+ev.ProviderMessageID)
		}
		return err
	}
	_ = h.st.InsertCorrection(ctx, &domain.Correction{
		TransactionID: txID, Source: "chat_discard",
	})
	_ = h.st.ClearPendingAction(ctx, user.ID)
	return h.reply(ctx, user.ID, ev, conversation.DiscardedText(), "discard:"+txID.String())
}

// reshowCard re-renders the confirmation card from the live transaction.
func (h *Handler) reshowCard(ctx context.Context, user *domain.User, ev events.InboundEvent, txID uuid.UUID) error {
	tx, err := h.st.GetTransactionForUser(ctx, txID, user.ID)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			_ = h.st.ClearPendingAction(ctx, user.ID)
			return h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "stale:"+ev.ProviderMessageID)
		}
		return err
	}
	card := conversation.ExtractionCard(conversation.CardData{
		Merchant: tx.MerchantName,
		Amount:   conversation.Money(tx.AmountMinor, tx.Currency),
		Date:     conversation.DateVN(tx.OccurredAt, loc(user)),
		Type:     conversation.TypeLabel(tx.Type),
		Category: h.categoryName(ctx, tx.CategoryID),
		Flags:    map[string]bool{},
	})
	return h.reply(ctx, user.ID, ev, conversation.CorrectedText(card),
		fmt.Sprintf("card:%s:v%d", tx.ID, tx.Version))
}

// categoryName resolves a category ID to its display name.
func (h *Handler) categoryName(ctx context.Context, id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	cats, err := h.st.ListCategories(ctx)
	if err != nil {
		return ""
	}
	for _, c := range cats {
		if c.ID == *id {
			return c.DisplayName
		}
	}
	return ""
}

// matchCategory resolves free text against the taxonomy: a 1-based number
// from CategoryListText, a system key, or a display name (folded).
func (h *Handler) matchCategory(ctx context.Context, text string) (*domain.Category, error) {
	cats, err := h.st.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	t := strings.TrimSpace(text)
	var n int
	if _, err := fmt.Sscanf(t, "%d", &n); err == nil && n >= 1 && n <= len(cats) {
		return &cats[n-1], nil
	}
	_, key := normalise.NormaliseMerchant(t)
	for i := range cats {
		if cats[i].SystemKey == key {
			return &cats[i], nil
		}
		_, dkey := normalise.NormaliseMerchant(cats[i].DisplayName)
		if dkey == key && key != "" {
			return &cats[i], nil
		}
	}
	return nil, domain.Ef(domain.CodeNotFound, nil, "no category matches %q", text)
}

// matchType resolves a type label or 1-based number from TypeListText.
func matchType(text string) (domain.TxType, bool) {
	_, key := normalise.NormaliseMerchant(strings.TrimSpace(text))
	switch key {
	case "1", "chi tieu":
		return domain.TxExpense, true
	case "2", "thu nhap":
		return domain.TxIncome, true
	case "3", "hoan tien":
		return domain.TxRefund, true
	case "4", "chuyen khoan":
		return domain.TxTransfer, true
	case "5", "dieu chinh":
		return domain.TxAdjustment, true
	}
	return "", false
}

// ptrKey renders a category pointer for correction records.
func ptrKey(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// must logs unexpected errors without aborting the chat flow — the user
// already got an answer or the error is recorded for the operator.
func (h *Handler) must(err error) {
	if err != nil {
		h.log.Error("bot flow step failed", slog.Any("error", err))
	}
}
