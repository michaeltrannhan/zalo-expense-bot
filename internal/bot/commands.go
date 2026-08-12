package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/contracts/events"
	"zl-expese-bot/internal/account"
	"zl-expese-bot/internal/conversation"
	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/normalise"
	"zl-expese-bot/internal/insight"
	"zl-expese-bot/internal/platform/queue"
	"zl-expese-bot/internal/summaryschedule"
)

// periodKind selects the insight window for summary commands.
type periodKind int

const (
	periodToday periodKind = iota
	periodWeek
	periodMonth
	periodLastWeek
	periodLastMonth
)

// handleImage registers a receipt and queues processing. The worker owns
// download/extraction; the webhook ack path stays under the P95 budget.
func (h *Handler) handleImage(ctx context.Context, user *domain.User, ev events.InboundEvent, pm *domain.ProviderMessage) error {
	if len(ev.Media) == 0 {
		return h.reply(ctx, user.ID, ev, conversation.UnsupportedEventText(), "unsup:"+pm.ID.String())
	}

	receiptID := uuid.New()
	deleteAfter := h.clk.Now().Add(30 * 24 * time.Hour)
	receipt, created, err := h.st.GetOrCreateReceiptForProviderMessage(ctx, &domain.ReceiptDocument{
		ID: receiptID, UserID: user.ID, ProviderMessageID: &pm.ID,
		RetentionPolicy: "originals_30d", DeleteAfter: &deleteAfter,
	})
	if err != nil {
		return err
	}

	limit := h.cfg.PerUserDailyReceiptLimit
	if created {
		day := h.clk.Now().Format("2006-01-02")
		count, err := h.st.IncrementUsage(ctx, "user", user.ID.String(), day, "receipts", 1, limit)
		if err != nil {
			_ = h.st.DeleteReceivedReceipt(ctx, receipt.ID)
			return err
		}
		if limit > 0 && count > limit {
			if err := h.st.TransitionReceipt(ctx, receipt.ID, domain.ReceiptReceived, domain.ReceiptFailedPermanent); err != nil {
				return err
			}
			return h.reply(ctx, user.ID, ev, conversation.DailyQuotaText(limit), "quota:"+pm.ID.String())
		}
	}

	switch receipt.Status {
	case domain.ReceiptReceived:
		if err := h.st.TransitionReceipt(ctx, receipt.ID, domain.ReceiptReceived, domain.ReceiptQueued); err != nil {
			return err
		}
	case domain.ReceiptQueued:
		// A previous attempt committed the row but failed before/while enqueueing.
	default:
		// Processing already owns this receipt; only repair the deterministic ack.
		return h.reply(ctx, user.ID, ev, conversation.ReceiptReceivedText(), "ack:"+pm.ID.String())
	}

	job := events.ReceiptJob{
		SchemaVersion:     events.SchemaV1,
		JobID:             uuid.New(),
		ReceiptID:         receipt.ID,
		UserID:            user.ID,
		Provider:          ev.Provider,
		ProviderChatID:    ev.ProviderChatID,
		ProviderMessageID: ev.ProviderMessageID,
		Attempt:           1,
		EnqueuedAt:        h.clk.Now(),
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return domain.E(domain.CodeInternal, "marshal receipt job", err)
	}
	dedupe := "receipt:" + pm.ID.String()
	if _, err := h.q.Enqueue(ctx, domain.JobReceiptProcess, payload, &dedupe, 5); err != nil && err != queue.ErrDuplicate {
		return err
	}
	return h.reply(ctx, user.ID, ev, conversation.ReceiptReceivedText(), "ack:"+pm.ID.String())
}

// summary answers the period commands (/homnay, /tuan, /thang, /tuantruoc,
// /thangtruoc) with deterministic SQL aggregates, and records the generated
// summary as an evidence-backed insight row (P4-B02). A persistence failure
// is logged but never blocks the user's answer.
func (h *Handler) summary(ctx context.Context, user *domain.User, ev events.InboundEvent, pm *domain.ProviderMessage, kind periodKind) error {
	l := loc(user)
	now := h.clk.Now()
	var p insight.Period
	var label, insightType string
	switch kind {
	case periodToday:
		p, label, insightType = insight.Today(now, l), "Hôm nay", insight.TypeDaily
	case periodWeek:
		p, label, insightType = insight.ThisWeek(now, l), "Tuần này", insight.TypeWeekly
	case periodMonth:
		p, label, insightType = insight.ThisMonth(now, l), "Tháng này", insight.TypeMonthly
	case periodLastWeek:
		p, label, insightType = insight.LastWeek(now, l), "Tuần trước", insight.TypeWeekly
	case periodLastMonth:
		p, label, insightType = insight.LastMonth(now, l), "Tháng trước", insight.TypeMonthly
	}
	sum, err := h.insights.Summarise(ctx, user.ID, p)
	if err != nil {
		return err
	}
	if perr := h.insights.Persist(ctx, user.ID, insightType, p, sum); perr != nil {
		h.log.Warn("persist insight failed", slog.Any("error", perr))
	}
	return h.reply(ctx, user.ID, ev, conversation.SummaryText(label, sum, l), "cmd:"+pm.ID.String())
}

// summarySchedule handles P4-B03 opt-in settings. A command always refreshes
// the destination chat so future summaries follow the user's current
// private conversation.
func (h *Handler) summarySchedule(ctx context.Context, user *domain.User, ev events.InboundEvent, intent conversation.Intent) error {
	switch intent.ScheduleAction {
	case conversation.SummaryScheduleShow:
		preferences, err := h.st.ListSummarySchedules(ctx, user.ID)
		if err != nil {
			return err
		}
		return h.reply(ctx, user.ID, ev, conversation.SummaryScheduleText(preferences),
			"schedule-show:"+ev.ProviderMessageID)
	case conversation.SummaryScheduleSet:
		next, err := summaryschedule.NextDelivery(h.clk.Now(), loc(user), intent.SummaryFrequency, intent.DeliveryMinute)
		if err != nil {
			return h.reply(ctx, user.ID, ev, conversation.SummaryScheduleInvalidText(),
				"schedule-invalid:"+ev.ProviderMessageID)
		}
		if _, err := h.st.UpsertSummarySchedule(ctx, &domain.SummarySchedule{
			ID:             uuid.New(),
			UserID:         user.ID,
			Frequency:      intent.SummaryFrequency,
			DeliveryMinute: intent.DeliveryMinute,
			Provider:       domain.Provider(ev.Provider),
			ProviderChatID: ev.ProviderChatID,
			Enabled:        true,
			NextDeliveryAt: next,
		}); err != nil {
			return err
		}
		return h.reply(ctx, user.ID, ev,
			conversation.SummaryScheduleSetText(intent.SummaryFrequency, intent.DeliveryMinute),
			"schedule-set:"+ev.ProviderMessageID)
	case conversation.SummaryScheduleDisable:
		var frequency *domain.SummaryFrequency
		if !intent.DisableAll {
			f := intent.SummaryFrequency
			frequency = &f
		}
		changed, err := h.st.DisableSummarySchedules(ctx, user.ID, frequency)
		if err != nil {
			return err
		}
		return h.reply(ctx, user.ID, ev,
			conversation.SummaryScheduleDisabledText(frequency, changed),
			"schedule-disable:"+ev.ProviderMessageID)
	default:
		return h.reply(ctx, user.ID, ev, conversation.SummaryScheduleInvalidText(),
			"schedule-invalid:"+ev.ProviderMessageID)
	}
}

// settings exposes and updates the preferences already stored on each user.
// Changing timezone also recalculates enabled scheduled-summary delivery
// instants so the displayed local time and the actual next run stay aligned.
func (h *Handler) settings(ctx context.Context, user *domain.User, ev events.InboundEvent, intent conversation.Intent) error {
	preferences, err := h.st.ListSummarySchedules(ctx, user.ID)
	if err != nil {
		return err
	}
	current := *user
	switch intent.SettingsAction {
	case conversation.SettingsShow:
		return h.reply(ctx, user.ID, ev, conversation.SettingsText(current, preferences),
			"settings-show:"+ev.ProviderMessageID)
	case conversation.SettingsSetTimezone:
		if intent.Timezone == "Local" {
			return h.reply(ctx, user.ID, ev, conversation.InvalidTimezoneText(),
				"settings-timezone-invalid:"+ev.ProviderMessageID)
		}
		location, loadErr := time.LoadLocation(intent.Timezone)
		if loadErr != nil {
			return h.reply(ctx, user.ID, ev, conversation.InvalidTimezoneText(),
				"settings-timezone-invalid:"+ev.ProviderMessageID)
		}
		current.Timezone = location.String()
		if err := h.st.UpdateUserPreferences(ctx, user.ID, current.Timezone, current.DefaultCurrency); err != nil {
			return err
		}
		for i := range preferences {
			if !preferences[i].Enabled {
				continue
			}
			next, nextErr := summaryschedule.NextDelivery(h.clk.Now(), location,
				preferences[i].Frequency, preferences[i].DeliveryMinute)
			if nextErr != nil {
				return nextErr
			}
			preferences[i].NextDeliveryAt = next
			preferences[i].Provider = domain.Provider(ev.Provider)
			preferences[i].ProviderChatID = ev.ProviderChatID
			saved, saveErr := h.st.UpsertSummarySchedule(ctx, &preferences[i])
			if saveErr != nil {
				return saveErr
			}
			preferences[i] = *saved
		}
		return h.reply(ctx, user.ID, ev,
			conversation.SettingsUpdatedText("múi giờ", current.Timezone, current, preferences),
			"settings-timezone:"+ev.ProviderMessageID)
	case conversation.SettingsSetCurrency:
		currency := strings.ToUpper(strings.TrimSpace(intent.DefaultCurrency))
		if !validCurrencyCode(currency) {
			return h.reply(ctx, user.ID, ev, conversation.InvalidCurrencyText(),
				"settings-currency-invalid:"+ev.ProviderMessageID)
		}
		current.DefaultCurrency = currency
		if err := h.st.UpdateUserPreferences(ctx, user.ID, current.Timezone, current.DefaultCurrency); err != nil {
			return err
		}
		return h.reply(ctx, user.ID, ev,
			conversation.SettingsUpdatedText("tiền tệ mặc định", currency, current, preferences),
			"settings-currency:"+ev.ProviderMessageID)
	default:
		return h.reply(ctx, user.ID, ev, conversation.SettingsInvalidText(),
			"settings-invalid:"+ev.ProviderMessageID)
	}
}

func validCurrencyCode(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, char := range currency {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

// recent answers /ganday with the newest transactions.
func (h *Handler) recent(ctx context.Context, user *domain.User, ev events.InboundEvent, pm *domain.ProviderMessage) error {
	txs, err := h.st.ListRecentTransactions(ctx, user.ID, 10)
	if err != nil {
		return err
	}
	if len(txs) == 0 {
		return h.reply(ctx, user.ID, ev, conversation.NoRecentText(), "cmd:"+pm.ID.String())
	}
	l := loc(user)
	lines := make([]conversation.RecentLine, 0, len(txs))
	for _, t := range txs {
		label := ""
		if t.Type != domain.TxExpense {
			label = conversation.TypeLabel(t.Type)
		}
		merchant := t.MerchantName
		if merchant == "" {
			merchant = t.Description
		}
		lines = append(lines, conversation.RecentLine{
			Date:     conversation.DateVN(t.OccurredAt, l),
			Amount:   conversation.Money(t.AmountMinor, t.Currency),
			Merchant: merchant,
			Category: h.categoryName(ctx, t.CategoryID),
			Type:     label,
		})
	}
	return h.reply(ctx, user.ID, ev, conversation.RecentText(lines), "cmd:"+pm.ID.String())
}

// export writes the user's CSV/JSON exports under DATA_DIR/exports and
// replies with the paths (local pilot: no file upload over Zalo yet).
func (h *Handler) export(ctx context.Context, user *domain.User, ev events.InboundEvent) error {
	dir := filepath.Join(h.cfg.DataDir, "exports", user.ID.String())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return domain.E(domain.CodeInternal, "create export dir", err)
	}
	stamp := h.clk.Now().Format("20060102-150405")
	csvPath := filepath.Join(dir, "transactions-"+stamp+".csv")
	jsonPath := filepath.Join(dir, "metadata-"+stamp+".json")

	csvFile, err := os.OpenFile(csvPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return domain.E(domain.CodeInternal, "create csv export", err)
	}
	n, csvErr := account.ExportTransactionsCSV(ctx, h.pool, user.ID, csvFile)
	if cerr := csvFile.Close(); cerr != nil && csvErr == nil {
		csvErr = cerr
	}
	if csvErr != nil {
		return csvErr
	}

	jsonFile, err := os.OpenFile(jsonPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return domain.E(domain.CodeInternal, "create json export", err)
	}
	jsonErr := account.ExportMetadataJSON(ctx, h.pool, user.ID, jsonFile)
	if cerr := jsonFile.Close(); cerr != nil && jsonErr == nil {
		jsonErr = cerr
	}
	if jsonErr != nil {
		return jsonErr
	}

	return h.reply(ctx, user.ID, ev, conversation.ExportReadyText(csvPath, jsonPath, n), "export:"+ev.ProviderMessageID)
}

// requestDelete arms the two-step account deletion.
func (h *Handler) requestDelete(ctx context.Context, user *domain.User, ev events.InboundEvent) error {
	if err := h.st.SetPendingAction(ctx, &domain.PendingAction{
		UserID: user.ID, Kind: domain.PendingDeleteAccount,
		ExpiresAt: h.clk.Now().Add(15 * time.Minute),
	}); err != nil {
		return err
	}
	return h.reply(ctx, user.ID, ev, conversation.DeleteConfirmText(), "delask:"+ev.ProviderMessageID)
}

// deleteAccount runs the full deletion flow through internal/account.
func (h *Handler) deleteAccount(ctx context.Context, userID, providerMessageID uuid.UUID) (account.Report, error) {
	return account.DeleteAccount(ctx, h.pool, h.objects, h.cfg.DataDir, userID, providerMessageID)
}

// requestDeleteRecent arms the two-step individual deletion (P4-D02): the
// user first sees exactly which transaction will be removed, and only an
// explicit confirm deletes it.
func (h *Handler) requestDeleteRecent(ctx context.Context, user *domain.User, ev events.InboundEvent) error {
	tx, err := h.st.LatestConfirmedTransaction(ctx, user.ID)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			return h.reply(ctx, user.ID, ev, conversation.NoRecentText(), "cmd:"+ev.ProviderMessageID)
		}
		return err
	}
	if err := h.st.SetPendingAction(ctx, &domain.PendingAction{
		UserID: user.ID, Kind: domain.PendingDeleteRecent, TransactionID: &tx.ID,
		ExpiresAt: h.clk.Now().Add(15 * time.Minute),
	}); err != nil {
		return err
	}
	return h.reply(ctx, user.ID, ev, conversation.DeleteRecentConfirmText(
		conversation.Money(tx.AmountMinor, tx.Currency), tx.MerchantName,
		conversation.DateVN(tx.OccurredAt, loc(user)), h.categoryName(ctx, tx.CategoryID)),
		"delrecentask:"+ev.ProviderMessageID)
}

// manualEntry persists "150000 ăn trưa"-style entries as suggestions going
// through the same confirmation card as receipts.
func (h *Handler) manualEntry(ctx context.Context, user *domain.User, ev events.InboundEvent, intent conversation.Intent) error {
	amountMinor, currency, err := normalise.ParseAmount(intent.AmountText, user.DefaultCurrency)
	if err != nil {
		return err
	}
	var merchant *domain.Merchant
	if intent.Description != "" {
		m, _, err := h.cats.ResolveMerchant(ctx, intent.Description)
		if err != nil {
			return err
		}
		merchant = m
	}
	sug, err := h.cats.SuggestCategory(ctx, user.ID, merchant, guessCategoryKey(intent.Description), 0.6)
	if err != nil {
		return err
	}
	hint, hintMatched := normalise.TypeHint(intent.Description)
	txType, typeConf, typeSource := h.cats.SuggestType(ctx, user.ID, merchant, hint, hintMatched)

	merchantName, merchantID := intent.Description, (*uuid.UUID)(nil)
	if merchant != nil {
		merchantName, merchantID = merchant.CanonicalName, &merchant.ID
	}

	now := h.clk.Now()
	tx := &domain.Transaction{
		ID: uuid.New(), UserID: user.ID,
		Type: txType, MerchantID: merchantID, MerchantName: merchantName,
		Description: intent.Description,
		AmountMinor: amountMinor, Currency: currency,
		OccurredAt:        now,
		CategoryID:        sug.CategoryID,
		Status:            domain.TxAwaitingConfirmation,
		Source:            domain.TxSourceManual,
		ConfidenceSummary: "manual",
		Version:           1,
		CreatedAt:         now, UpdatedAt: now,
	}
	if err := h.st.CreateTransaction(ctx, tx); err != nil {
		return err
	}
	_ = h.st.InsertPredictions(ctx, []domain.Prediction{
		{ID: uuid.New(), TransactionID: tx.ID, PredictionType: "category",
			PredictedValue: sug.CategoryKey, Confidence: sug.Confidence,
			ModelName: sug.Source, ModelVersion: "v1", CreatedAt: now},
		{ID: uuid.New(), TransactionID: tx.ID, PredictionType: "type",
			PredictedValue: string(txType), Confidence: typeConf,
			ModelName: typeSource, ModelVersion: "v1", CreatedAt: now},
	})
	if err := h.st.SetPendingAction(ctx, &domain.PendingAction{
		UserID: user.ID, Kind: domain.PendingConfirmExtraction, TransactionID: &tx.ID,
		ExpiresAt: now.Add(pendingTTL),
	}); err != nil {
		return err
	}
	card := conversation.ExtractionCard(conversation.CardData{
		Merchant: merchantName,
		Amount:   conversation.Money(tx.AmountMinor, tx.Currency),
		Date:     conversation.DateVN(tx.OccurredAt, loc(user)),
		Type:     conversation.TypeLabel(tx.Type),
		Category: sug.DisplayName,
		Flags:    map[string]bool{},
	})
	return h.reply(ctx, user.ID, ev, card, fmt.Sprintf("card:%s:v%d", tx.ID, tx.Version))
}

// recategorise applies "đổi khoản gần nhất sang đi lại": the newest
// confirmed transaction moves to the named category, and the rule learns.
func (h *Handler) recategorise(ctx context.Context, user *domain.User, ev events.InboundEvent, categoryText string) error {
	tx, err := h.st.LatestConfirmedTransaction(ctx, user.ID)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			return h.reply(ctx, user.ID, ev, conversation.NoRecentText(), "cmd:"+ev.ProviderMessageID)
		}
		return err
	}
	cat, err := h.matchCategory(ctx, categoryText)
	if err != nil {
		return h.reply(ctx, user.ID, ev, conversation.EditInvalidText(domain.PendingEditCategory), "cmd:"+ev.ProviderMessageID)
	}

	predicted := ptrKey(tx.CategoryID)
	tx.CategoryID = &cat.ID
	if tx.Status == domain.TxConfirmed {
		tx.Status = domain.TxAmended
	}
	if err := h.st.UpdateTransaction(ctx, tx, tx.Version); err != nil {
		if domain.IsCode(err, domain.CodeConflict) {
			return h.reply(ctx, user.ID, ev, conversation.PendingExpiredText(), "cmd:"+ev.ProviderMessageID)
		}
		return err
	}
	_ = h.st.InsertCorrection(ctx, &domain.Correction{
		TransactionID: tx.ID, FieldName: "category",
		PredictedValue: predicted, CorrectedValue: cat.SystemKey, Source: "chat_edit",
	})
	if err := h.cats.LearnFromConfirmation(ctx, user.ID, tx); err != nil {
		h.log.Warn("learn from recategorise failed", slog.Any("error", err))
	}
	return h.reply(ctx, user.ID, ev, conversation.RecategorisedText(tx.MerchantName, cat.DisplayName), "cmd:"+ev.ProviderMessageID)
}

// guessCategoryKey is the deterministic keyword fallback for manual entries;
// user rules and corrections override it after one confirmation.
func guessCategoryKey(desc string) string {
	_, key := normalise.NormaliseMerchant(desc)
	if key == "" {
		return ""
	}
	padded := " " + key + " "
	type rule struct{ substr, category string }
	rules := []rule{
		{"ca phe", "an-uong"}, {"cafe", "an-uong"}, {"tra sua", "an-uong"},
		{"an sang", "an-uong"}, {"an trua", "an-uong"}, {"an toi", "an-uong"},
		{" an ", "an-uong"}, {"com ", "an-uong"}, {"pho ", "an-uong"},
		{"bun ", "an-uong"}, {"do an", "an-uong"}, {"nuoc uong", "an-uong"},
		{" cho ", "thuc-pham"}, {"sieu thi", "thuc-pham"}, {"tap hoa", "thuc-pham"},
		{"rau ", "thuc-pham"}, {"thit ", "thuc-pham"},
		{"grab", "di-lai"}, {"taxi", "di-lai"}, {"xang", "di-lai"},
		{"xe buyt", "di-lai"}, {"gui xe", "di-lai"}, {"ve xe", "di-lai"},
		{"di lai", "di-lai"}, {"tau ", "di-lai"}, {"may bay", "di-lai"},
		{"hoa don", "hoa-don"}, {"tien dien", "hoa-don"}, {"tien nuoc", "hoa-don"},
		{"internet", "hoa-don"}, {"wifi", "hoa-don"}, {"dien thoai", "hoa-don"},
		{"shopee", "mua-sam"}, {"lazada", "mua-sam"}, {"mua sam", "mua-sam"},
		{"quanao", "mua-sam"}, {" ao ", "mua-sam"}, {"quan ", "mua-sam"},
		{"thuoc", "suc-khoe"}, {"benh vien", "suc-khoe"}, {"kham ", "suc-khoe"},
		{"phim", "giai-tri"}, {"game", "giai-tri"}, {"karaoke", "giai-tri"},
		{"sach", "giao-duc"}, {"hoc phi", "giao-duc"}, {"khoa hoc", "giao-duc"},
		{"tien nha", "nha-o"}, {"thue nha", "nha-o"},
		{"luong", "thu-nhap"}, {"thuong", "thu-nhap"},
	}
	for _, r := range rules {
		if strings.Contains(padded, r.substr) {
			return r.category
		}
	}
	return ""
}
