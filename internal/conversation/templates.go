package conversation

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/insight"
)

// ---------------------------------------------------------------------------
// Vietnamese message templates (P1-E01). Rendering only — data in, string
// out. Amounts arrive pre-formatted via Money() or domain.FormatMinor.
// ---------------------------------------------------------------------------

// CardData is the view model for the extraction confirmation card. Flags
// marks fields that must render with a ⚠️ (see extraction.FieldNeedsFlag);
// the flag keys are "total", "currency", "date", "merchant", "category".
type CardData struct {
	Merchant string
	Amount   string // formatted, e.g. "325.000 ₫"
	Date     string // formatted dd/mm/yyyy
	Type     string // TypeLabel
	Category string // display name
	Flags    map[string]bool
}

// Money formats minor units for chat (dot thousands, ₫/A$/$ symbol).
func Money(minor int64, currency string) string {
	return domain.FormatMinor(minor, currency)
}

// DateVN renders t in loc as dd/mm/yyyy.
func DateVN(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("02/01/2006")
}

// TypeLabel is the Vietnamese label for a transaction type.
func TypeLabel(t domain.TxType) string {
	switch t {
	case domain.TxExpense:
		return "Chi tiêu"
	case domain.TxIncome:
		return "Thu nhập"
	case domain.TxRefund:
		return "Hoàn tiền"
	case domain.TxTransfer:
		return "Chuyển khoản"
	case domain.TxAdjustment:
		return "Điều chỉnh"
	default:
		return string(t)
	}
}

// ConsentCard is the §11.1 first-contact card. Zalo text has no buttons, so
// the choices are spelled out as reply instructions.
func ConsentCard() string {
	return `Xin chào! Tôi giúp ghi nhận và tổng hợp các khoản chi từ ảnh hóa đơn.

Bạn chỉ cần gửi ảnh hóa đơn, hoặc nhập nhanh một khoản như "an sang 500k".

Dữ liệu có thể bao gồm thông tin mua hàng và được lưu để tạo báo cáo chi tiêu. Bạn có thể xóa dữ liệu bất kỳ lúc nào bằng /delete.

Trả lời ok (hoặc "đồng ý") để bắt đầu, hoặc /privacy để xem cách dữ liệu được sử dụng.`
}

// PrivacyText explains data use in plain words (the §11.1 second choice).
// retentionDays is ORIGINAL_RETENTION_DAYS. If extractor is gemini or
// textract, the copy does not claim that no third parties see the data.
func PrivacyText(retentionDays int, extractor string) string {
	thirdParty := "• Tôi không chia sẻ dữ liệu của bạn cho bên thứ ba.\n"
	switch extractor {
	case "gemini", "textract":
		thirdParty = ""
	}
	return fmt.Sprintf(`Cách tôi dùng dữ liệu của bạn:

• Ảnh hóa đơn chỉ dùng để đọc thông tin giao dịch; ảnh gốc được xóa sau %d ngày.
• Giao dịch đã ghi nhận được lưu để tổng hợp báo cáo chi tiêu cho riêng bạn.
%s
Gửi /export để tải về toàn bộ dữ liệu, /delete để xóa vĩnh viễn.`, retentionDays, thirdParty)
}

// WelcomeText greets the user right after consent is recorded.
func WelcomeText() string {
	return `Cảm ơn bạn! Từ giờ bạn có thể:

• Gửi ảnh hóa đơn để tôi đọc và ghi nhận.
• Nhập nhanh: "an sang 500k", "cafe 45k".
• Xem tổng hợp: /today, /week, /month.
• Xem múi giờ, tiền tệ và lịch tổng kết bằng /settings.

Gõ /help để xem tất cả lệnh.`
}

// HelpText lists commands and natural phrases (§11.5).
func HelpText() string {
	return `Tôi có thể giúp bạn:

• Gửi ảnh hóa đơn — tôi đọc và gợi ý để bạn xác nhận.
• Nhập nhanh một khoản: "an sang 500k", "cafe 45k", "150k cafe".

Lệnh:
/help — xem hướng dẫn này
/today — chi tiêu hôm nay
/week — chi tiêu tuần này
/month — chi tiêu tháng này
/recent — các khoản gần đây
/settings — múi giờ, tiền tệ
/tz — đổi múi giờ, ví dụ /tz Asia/Ho_Chi_Minh
/sched — tổng kết tự động
/export — tải dữ liệu về
/delete — xóa toàn bộ dữ liệu (cần ok lần nữa)
/privacy — cách dữ liệu được sử dụng

ok / y — xác nhận · no / n — bỏ qua · edit — sửa số tiền
Câu tiếng Việt cũ (/homnay, xác nhận, …) vẫn dùng được.`
}

// SettingsText is the discoverable profile-settings surface. It combines
// user preferences with automatic-summary status so /settings is the single
// place a user can inspect every configurable behaviour.
func SettingsText(user domain.User, preferences []domain.SummarySchedule) string {
	var b strings.Builder
	b.WriteString("⚙️ Cài đặt của bạn\n\n")
	fmt.Fprintf(&b, "• Múi giờ: %s\n", user.Timezone)
	fmt.Fprintf(&b, "• Tiền tệ mặc định: %s\n", user.DefaultCurrency)
	fmt.Fprintf(&b, "• Ngôn ngữ: Tiếng Việt (%s)\n", user.Locale)
	b.WriteString("• Tổng kết tự động: ")
	active := 0
	for _, p := range preferences {
		if !p.Enabled {
			continue
		}
		if active > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %02d:%02d", SummaryFrequencyLabel(p.Frequency),
			p.DeliveryMinute/60, p.DeliveryMinute%60)
		active++
	}
	if active == 0 {
		b.WriteString("chưa bật")
	}
	b.WriteString("\n\nThay đổi:\n")
	b.WriteString("/tz Asia/Ho_Chi_Minh\n")
	b.WriteString("/settings currency VND\n")
	b.WriteString("/sched — cài lịch tổng kết")
	return b.String()
}

// SettingsUpdatedText confirms a preference change and returns the full
// refreshed settings, so the user can immediately verify its effect.
func SettingsUpdatedText(label, value string, user domain.User, preferences []domain.SummarySchedule) string {
	return fmt.Sprintf("Đã cập nhật %s: %s.\n\n%s", label, value, SettingsText(user, preferences))
}

// SettingsInvalidText explains every supported settings operation.
func SettingsInvalidText() string {
	return "Cú pháp chưa đúng. Dùng /settings để xem, /tz Asia/Ho_Chi_Minh hoặc /settings currency VND để thay đổi."
}

// InvalidTimezoneText and InvalidCurrencyText are targeted validation errors.
func InvalidTimezoneText() string {
	return "Múi giờ không hợp lệ. Hãy dùng tên IANA, ví dụ Asia/Ho_Chi_Minh, Asia/Bangkok hoặc UTC."
}

func InvalidCurrencyText() string {
	return "Tiền tệ không hợp lệ. Hãy dùng mã ISO 4217 gồm 3 chữ cái, ví dụ VND, USD hoặc AUD."
}

// SummaryScheduleText renders current automatic-summary preferences and
// always includes the exact command syntax for changing them.
func SummaryScheduleText(preferences []domain.SummarySchedule) string {
	var b strings.Builder
	b.WriteString("Tổng kết tự động:\n")
	active := 0
	for _, p := range preferences {
		if !p.Enabled {
			continue
		}
		active++
		fmt.Fprintf(&b, "• %s lúc %02d:%02d\n",
			SummaryFrequencyLabel(p.Frequency), p.DeliveryMinute/60, p.DeliveryMinute%60)
	}
	if active == 0 {
		b.WriteString("• Chưa bật lịch nào.\n")
	}
	b.WriteString("\nCài đặt:\n")
	b.WriteString("/sched daily 20:00\n/sched weekly 08:00\n/sched monthly 09:00\n")
	b.WriteString("/sched off daily|weekly|monthly — tắt một lịch\n/sched off — tắt tất cả")
	return b.String()
}

// SummaryScheduleSetText confirms one enabled preference.
func SummaryScheduleSetText(frequency domain.SummaryFrequency, deliveryMinute int) string {
	return fmt.Sprintf("Đã bật tổng kết %s lúc %02d:%02d theo múi giờ của bạn.",
		SummaryFrequencyLabel(frequency), deliveryMinute/60, deliveryMinute%60)
}

// SummaryScheduleDisabledText confirms an opt-out.
func SummaryScheduleDisabledText(frequency *domain.SummaryFrequency, changed int64) string {
	if changed == 0 {
		return "Không có lịch tổng kết nào đang bật để tắt."
	}
	if frequency == nil {
		return "Đã tắt tất cả tổng kết tự động."
	}
	return fmt.Sprintf("Đã tắt tổng kết %s.", SummaryFrequencyLabel(*frequency))
}

// SummaryScheduleInvalidText explains valid /sched input.
func SummaryScheduleInvalidText() string {
	return "Cú pháp chưa đúng. Ví dụ: /sched daily 20:00, /sched weekly 08:00, hoặc /sched off."
}

// SummaryFrequencyLabel is the Vietnamese cadence label.
func SummaryFrequencyLabel(frequency domain.SummaryFrequency) string {
	switch frequency {
	case domain.SummaryDaily:
		return "hằng ngày"
	case domain.SummaryWeekly:
		return "hằng tuần"
	case domain.SummaryMonthly:
		return "hằng tháng"
	default:
		return string(frequency)
	}
}

// SuspendedText is shown when the account is administratively suspended.
func SuspendedText() string {
	return "Tài khoản của bạn đang tạm dừng. Vui lòng liên hệ quản trị viên để được hỗ trợ."
}

// NotAllowedText rejects users outside the pilot allowlist.
func NotAllowedText() string {
	return "Xin lỗi, tài khoản của bạn chưa được cấp quyền dùng bot trong giai đoạn thử nghiệm."
}

// UnsupportedEventText answers message types the bot cannot handle.
func UnsupportedEventText() string {
	return `Xin lỗi, tôi chưa hỗ trợ loại tin nhắn này. Bạn có thể gửi ảnh hóa đơn hoặc nhập: "an sang 500k".`
}

// UnknownText is the fallback for text Parse cannot classify.
func UnknownText() string {
	return `Tôi chưa hiểu tin nhắn này. Gõ /help để xem cách dùng, hoặc nhập một khoản như "an sang 500k".`
}

// BudgetUnsupportedText answers /ngansach — not part of the MVP.
func BudgetUnsupportedText() string {
	return "Tính năng ngân sách chưa có trong bản hiện tại. Tôi sẽ báo bạn khi sẵn sàng."
}

// ReceiptReceivedText acknowledges an image before processing ("Đã nhận
// ảnh hóa đơn, đang xử lý…").
func ReceiptReceivedText() string {
	return "Đã nhận ảnh hóa đơn, đang xử lý…"
}

// ExtractionCard renders the plan §11.2/§11.3 confirmation card: fields,
// ⚠️ on flagged fields, and the reply hints (xác nhận / sửa … / bỏ qua).
func ExtractionCard(d CardData) string {
	flag := func(key string) string {
		if d.Flags[key] {
			return "  ⚠️"
		}
		return ""
	}
	var b strings.Builder
	// §11.3: an unsure total/currency leads with the low-confidence line.
	if d.Flags["total"] || d.Flags["currency"] {
		b.WriteString("Tôi chưa chắc về tổng tiền.\n\n")
	}
	b.WriteString("Tôi đọc được:\n\n")
	fmt.Fprintf(&b, "Cửa hàng: %s%s\n", d.Merchant, flag("merchant"))
	amountFlag := flag("total")
	if d.Flags["currency"] {
		amountFlag = "  ⚠️"
	}
	fmt.Fprintf(&b, "Số tiền: %s%s\n", d.Amount, amountFlag)
	fmt.Fprintf(&b, "Ngày: %s%s\n", d.Date, flag("date"))
	fmt.Fprintf(&b, "Loại: %s\n", d.Type)
	fmt.Fprintf(&b, "Danh mục: %s%s\n", d.Category, flag("category"))
	b.WriteString("\nTrả lời: ok / y để lưu · edit / fix để sửa số tiền · no / n để hủy")
	return b.String()
}

// UnsupportedImageText is plan §11.4 (not a receipt + manual-entry hint).
func UnsupportedImageText() string {
	return `Tôi chưa nhận ra đây là hóa đơn hoặc ảnh giao dịch.

Bạn có thể:
• gửi ảnh rõ hơn;
• cắt bớt phần không liên quan; hoặc
• nhập: "an sang 500k".`
}

// DuplicateReceiptText warns the image was already processed before.
func DuplicateReceiptText() string {
	return `Ảnh này tôi đã xử lý trước đó rồi. Nếu muốn ghi lại, bạn nhập tay nhé, ví dụ: "an sang 500k".`
}

// DailyQuotaText tells the user the per-user daily receipt limit is hit.
func DailyQuotaText(limit int64) string {
	return fmt.Sprintf(`Bạn đã gửi %d ảnh hóa đơn hôm nay — đạt giới hạn rồi. Bạn vẫn có thể nhập tay, ví dụ: "an sang 500k", hoặc gửi ảnh lại vào ngày mai.`, limit)
}

// MonthlyQuotaText tells the user the system-wide monthly image-processing
// allowance is exhausted and suggests manual entry.
func MonthlyQuotaText() string {
	return `Hệ thống đã đạt hạn mức xử lý ảnh trong tháng này. Bạn vẫn có thể nhập tay, ví dụ: "an sang 500k".`
}

// OCRDisabledText is the kill-switch notice.
func OCRDisabledText() string {
	return `Tính năng đọc ảnh hóa đơn đang tạm tắt để bảo trì. Bạn vẫn có thể nhập tay, ví dụ: "an sang 500k".`
}

// ExtractionFailedText reports a permanent processing failure.
func ExtractionFailedText() string {
	return `Tôi không đọc được ảnh này. Bạn có thể gửi ảnh rõ hơn, hoặc nhập tay: "an sang 500k".`
}

// ConfirmedText acknowledges a confirmed transaction, e.g.
// "Đã ghi nhận: 325.000 ₫ tại Co.opmart (Thực phẩm)."
func ConfirmedText(amount, merchant, category string) string {
	return fmt.Sprintf("Đã ghi nhận: %s tại %s (%s).", amount, merchant, category)
}

// DiscardedText acknowledges discarding a suggestion.
func DiscardedText() string {
	return "Đã bỏ qua, không ghi nhận khoản này."
}

// EditPrompt asks for the replacement value of one field, e.g.
// "Nhập số tiền đúng (ví dụ: 325000):".
func EditPrompt(kind domain.PendingKind) string {
	switch kind {
	case domain.PendingEditTotal:
		return "Nhập số tiền đúng (ví dụ: 325000):"
	case domain.PendingEditMerchant:
		return "Nhập tên cửa hàng đúng:"
	case domain.PendingEditDate:
		return "Nhập ngày đúng (ví dụ: 19/07/2026):"
	case domain.PendingEditCategory:
		return "Chọn danh mục đúng từ danh sách — trả lời số hoặc tên danh mục:"
	case domain.PendingEditType:
		return "Chọn loại giao dịch đúng từ danh sách — trả lời số hoặc tên loại:"
	case domain.PendingDeleteAccount:
		return DeleteConfirmText()
	default:
		// confirm_extraction re-renders the card instead of prompting.
		return ""
	}
}

// EditInvalidText rejects an unparseable edit value and re-asks.
func EditInvalidText(kind domain.PendingKind) string {
	switch kind {
	case domain.PendingConfirmExtraction:
		return "Tôi chưa hiểu. Trả lời ok / y để lưu, hoặc no / n để hủy."
	case domain.PendingDeleteAccount:
		return "Trả lời ok để xóa, hoặc bất kỳ nội dung nào khác để hủy."
	default:
		return "Giá trị chưa hợp lệ. " + EditPrompt(kind)
	}
}

// CorrectedText prefixes a re-rendered card after an edit: "Đã cập nhật."
func CorrectedText(card string) string {
	return "Đã cập nhật.\n\n" + card
}

// SummaryText renders an insight.Summary for one period label ("Hôm nay",
// "Tuần này", "Tháng này"): per-currency expense totals, refund totals when
// present, top categories, largest transactions, no-spend days. It MUST use
// "chi tiêu đã ghi nhận" wording (recorded spending, plan §1). loc formats
// LargestByCurrency.OccurredAt.
func SummaryText(periodLabel string, s insight.Summary, loc *time.Location) string {
	if s.TxCount == 0 {
		return EmptySummaryText(periodLabel)
	}
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — chi tiêu đã ghi nhận:\n\n", periodLabel)

	expenseCurs := sortedKeys(s.CurrencyTotals)
	for _, c := range expenseCurs {
		if len(expenseCurs) == 1 {
			fmt.Fprintf(&b, "Tổng: %s\n", Money(s.CurrencyTotals[c], c))
		} else {
			fmt.Fprintf(&b, "Tổng (%s): %s\n", c, Money(s.CurrencyTotals[c], c))
		}
	}

	refundCurs := sortedKeys(s.RefundTotals)
	for _, c := range refundCurs {
		if len(refundCurs) == 1 {
			fmt.Fprintf(&b, "Hoàn tiền: %s\n", Money(s.RefundTotals[c], c))
		} else {
			fmt.Fprintf(&b, "Hoàn tiền (%s): %s\n", c, Money(s.RefundTotals[c], c))
		}
	}

	if len(s.ByCategory) > 0 {
		b.WriteString("\nTheo danh mục:\n")
		categoryCounts := map[string]int{}
		for _, ct := range s.ByCategory {
			if categoryCounts[ct.Currency] >= 5 {
				continue
			}
			categoryCounts[ct.Currency]++
			label := ct.DisplayName
			if len(s.CurrencyTotals) > 1 {
				label += " (" + ct.Currency + ")"
			}
			fmt.Fprintf(&b, "• %s: %s\n", label, Money(ct.Minor, ct.Currency))
		}
	}

	for i, largest := range s.LargestByCurrency {
		if i == 0 {
			b.WriteString("\n")
		}
		label := "Khoản lớn nhất"
		if len(s.LargestByCurrency) > 1 {
			label += " (" + largest.Currency + ")"
		}
		fmt.Fprintf(&b, "%s: %s · %s · %s\n",
			label,
			Money(largest.Minor, largest.Currency),
			largest.Merchant,
			largest.OccurredAt.In(loc).Format("02/01"))
	}

	if s.NoSpendDays > 0 {
		fmt.Fprintf(&b, "\nSố ngày không chi: %d\n", s.NoSpendDays)
	}

	return strings.TrimRight(b.String(), "\n")
}

// sortedKeys gives deterministic output for the per-currency maps.
func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EmptySummaryText is the zero-transaction period message.
func EmptySummaryText(periodLabel string) string {
	return fmt.Sprintf(`%s: chưa có khoản chi tiêu nào được ghi nhận. Gửi ảnh hóa đơn hoặc nhập: "an sang 500k".`, periodLabel)
}

// RecentText renders the /recent transaction list (newest first), one line
// per transaction: date, amount, merchant, category.
func RecentText(lines []RecentLine) string {
	if len(lines) == 0 {
		return NoRecentText()
	}
	var b strings.Builder
	b.WriteString("Các khoản gần đây:\n")
	for _, l := range lines {
		prefix := ""
		// Only non-expense rows carry a type tag.
		if l.Type != "" && l.Type != TypeLabel(domain.TxExpense) && l.Type != string(domain.TxExpense) {
			prefix = "[" + l.Type + "] "
		}
		fmt.Fprintf(&b, "%s%s · %s · %s · %s\n", prefix, l.Date, l.Amount, l.Merchant, l.Category)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RecentLine is one row of RecentText.
type RecentLine struct {
	Date     string
	Amount   string
	Merchant string
	Category string
	Type     string
}

// ExportReadyText points the user at their export files.
func ExportReadyText(csvPath string, jsonPath string, txCount int) string {
	return fmt.Sprintf("Đã xuất %d giao dịch đã ghi nhận:\n• CSV: %s\n• JSON: %s", txCount, csvPath, jsonPath)
}

// DeleteConfirmText asks for explicit confirmation before data deletion.
func DeleteConfirmText() string {
	return `Bạn sắp xóa VĨNH VIỄN toàn bộ dữ liệu của mình, gồm:

• Tất cả giao dịch đã ghi nhận
• Ảnh hóa đơn đã gửi
• Các quy tắc đã học từ bạn

Thao tác này không thể hoàn tác.
Trả lời ok để xóa, hoặc bất kỳ nội dung nào khác để hủy.`
}

// DeletedText confirms account data was deleted.
func DeletedText() string {
	return "Đã xóa toàn bộ dữ liệu của bạn. Cảm ơn bạn đã đồng hành."
}

// CategoryListText lists selectable categories for the edit_category flow.
func CategoryListText(cats []domain.Category) string {
	var b strings.Builder
	for i, c := range cats {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c.DisplayName)
	}
	b.WriteString("\nTrả lời số hoặc tên danh mục.")
	return b.String()
}

// TypeListText lists selectable transaction types for the edit_type flow.
func TypeListText() string {
	types := []domain.TxType{
		domain.TxExpense, domain.TxIncome, domain.TxRefund,
		domain.TxTransfer, domain.TxAdjustment,
	}
	var b strings.Builder
	for i, t := range types {
		fmt.Fprintf(&b, "%d. %s\n", i+1, TypeLabel(t))
	}
	b.WriteString("\nTrả lời số hoặc tên loại giao dịch.")
	return b.String()
}

// RecategorisedText confirms the latest transaction changed category.
func RecategorisedText(merchant, category string) string {
	if merchant == "" {
		return fmt.Sprintf("Đã đổi khoản gần nhất sang danh mục %s.", category)
	}
	return fmt.Sprintf("Đã đổi khoản gần nhất tại %s sang danh mục %s.", merchant, category)
}

// NoRecentText is the empty /recent or recategory-with-nothing message.
func NoRecentText() string {
	return `Chưa có giao dịch nào được ghi nhận. Gửi ảnh hóa đơn hoặc nhập: "an sang 500k".`
}

// PendingExpiredText reports a stale confirmation (pending action expired).
func PendingExpiredText() string {
	return "Yêu cầu trước đó đã hết hạn. Bạn gửi lại ảnh hoặc nhập lại nhé."
}

// DupLine is one already-recorded transaction that may duplicate the new
// receipt: formatted amount, merchant and date.
type DupLine struct {
	Amount   string
	Merchant string
	Date     string
}

// PossibleDuplicateText warns — and only warns — when a new receipt looks
// like an already-recorded transaction (P4-C01). The product never
// auto-deletes: the user decides with the usual confirm/discard replies.
func PossibleDuplicateText(lines []DupLine) string {
	var b strings.Builder
	b.WriteString("⚠️ Khoản này có thể trùng với khoản đã ghi trước đó:")
	for _, l := range lines {
		fmt.Fprintf(&b, "\n• %s · %s · %s", l.Amount, l.Merchant, l.Date)
	}
	b.WriteString("\n\nNếu đây là hai khoản khác nhau, cứ ok như thường. Nếu bị trùng, trả lời no.")
	return b.String()
}

// DeleteRecentConfirmText asks before deleting the newest recorded
// transaction (P4-D02): the user sees exactly what will be removed.
func DeleteRecentConfirmText(amount, merchant, date, category string) string {
	return fmt.Sprintf(`Bạn muốn xóa khoản này?

%s · %s · %s · %s

Trả lời ok để xóa, hoặc gửi gì cũng được để giữ lại.`, amount, merchant, date, category)
}

// DeletedRecentText confirms one transaction was deleted.
func DeletedRecentText(amount, merchant string) string {
	return fmt.Sprintf("Đã xóa: %s tại %s.", amount, merchant)
}

// DeleteRecentCancelText answers anything that is not a confirm.
func DeleteRecentCancelText() string {
	return "Đã giữ lại khoản đó, không xóa nhé."
}
