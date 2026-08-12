package conversation

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/normalise"
	"zl-expese-bot/internal/insight"
)

// IntentKind classifies one inbound text. Parsing is pure: no store, no
// provider — the bot handler resolves intents against user state and
// pending actions.
type IntentKind string

const (
	IntentNone            IntentKind = ""                 // not understood
	IntentStart           IntentKind = "start"            // /batdau — consent card (pending) or help (active)
	IntentConfirm         IntentKind = "confirm"          // "xác nhận", "đúng", "đồng ý", "ok"
	IntentDiscard         IntentKind = "discard"          // "bỏ qua", "huỷ"
	IntentPrivacy         IntentKind = "privacy"          // "xem cách dữ liệu được sử dụng"
	IntentHelp            IntentKind = "help"             // /trogiup
	IntentToday           IntentKind = "today"            // /homnay, "hôm nay tôi tiêu bao nhiêu?"
	IntentWeek            IntentKind = "week"             // /tuan, "tuần này"
	IntentMonth           IntentKind = "month"            // /thang, "tháng này ăn uống hết bao nhiêu?"
	IntentLastWeek        IntentKind = "last_week"        // /tuantruoc, "tuần trước"
	IntentLastMonth       IntentKind = "last_month"       // /thangtruoc, "tháng trước"
	IntentRecent          IntentKind = "recent"           // /ganday
	IntentBudget          IntentKind = "budget"           // /ngansach — not in MVP, polite unsupported
	IntentExport          IntentKind = "export"           // /xuatdulieu
	IntentDeleteData      IntentKind = "delete"           // /xoadulieu
	IntentSettings        IntentKind = "settings"         // /caidat [muigio|tiente value]
	IntentSummarySchedule IntentKind = "summary_schedule" // /tongket [ngay|tuan|thang HH:MM|tat ...]
	IntentEditTotal       IntentKind = "edit_total"
	IntentEditMerchant    IntentKind = "edit_merchant"
	IntentEditDate        IntentKind = "edit_date"
	IntentEditCategory    IntentKind = "edit_category"
	IntentEditType        IntentKind = "edit_type"
	// IntentRecategory re-categorises the most recent confirmed transaction:
	// "đổi khoản gần nhất sang đi lại". CategoryText carries the raw phrase.
	IntentRecategory IntentKind = "recategory"
	// IntentDeleteRecent asks to delete the newest recorded transaction:
	// "xóa khoản gần nhất". Two-step with an explicit confirm (P4-D02).
	IntentDeleteRecent IntentKind = "delete_recent"
	// IntentManualEntry is "150000 ăn trưa" / "150k cafe" / "1.2tr tiền nhà":
	// a leading amount followed by free-text description.
	IntentManualEntry IntentKind = "manual_entry"
)

// SummaryScheduleAction identifies a /tongket settings operation.
type SummaryScheduleAction string

const (
	SummaryScheduleShow    SummaryScheduleAction = "show"
	SummaryScheduleSet     SummaryScheduleAction = "set"
	SummaryScheduleDisable SummaryScheduleAction = "disable"
	SummaryScheduleInvalid SummaryScheduleAction = "invalid"
)

// SettingsAction identifies a /caidat profile-preference operation.
type SettingsAction string

const (
	SettingsShow        SettingsAction = "show"
	SettingsSetTimezone SettingsAction = "set_timezone"
	SettingsSetCurrency SettingsAction = "set_currency"
	SettingsInvalid     SettingsAction = "invalid"
)

// Intent is the parsed meaning of one text message. Value fields are only
// set for the kinds that need them.
type Intent struct {
	Kind IntentKind
	// Manual entry fields (Kind == IntentManualEntry).
	AmountMinor int64
	Currency    string
	Description string
	AmountText  string
	// Recategory target phrase (Kind == IntentRecategory), e.g. "đi lại".
	CategoryText string
	// Scheduled summary settings (Kind == IntentSummarySchedule).
	ScheduleAction   SummaryScheduleAction
	SummaryFrequency domain.SummaryFrequency
	DeliveryMinute   int
	DisableAll       bool
	// Profile settings (Kind == IntentSettings).
	SettingsAction  SettingsAction
	Timezone        string
	DefaultCurrency string
}

// slashCommands maps the folded command body (after "/") to its intent.
var slashCommands = map[string]IntentKind{
	"batdau":     IntentStart,
	"homnay":     IntentToday,
	"tuan":       IntentWeek,
	"thang":      IntentMonth,
	"tuantruoc":  IntentLastWeek,
	"thangtruoc": IntentLastMonth,
	"ganday":     IntentRecent,
	"ngansach":   IntentBudget,
	"xuatdulieu": IntentExport,
	"xoadulieu":  IntentDeleteData,
	"trogiup":    IntentHelp,
}

// editPhrases are exact folded messages that start a field-correction flow.
var editPhrases = map[string]IntentKind{
	"sua so tien":      IntentEditTotal,
	"sai so tien":      IntentEditTotal,
	"doi so tien":      IntentEditTotal,
	"sua cua hang":     IntentEditMerchant,
	"sai ten":          IntentEditMerchant,
	"doi cua hang":     IntentEditMerchant,
	"sua ten cua hang": IntentEditMerchant,
	"sua ngay":         IntentEditDate,
	"sai ngay":         IntentEditDate,
	"doi ngay":         IntentEditDate,
	"sua danh muc":     IntentEditCategory,
	"doi danh muc":     IntentEditCategory,
	"sua loai":         IntentEditType,
	"doi loai":         IntentEditType,
}

// recategoryPrefixes need "gan nhat" so a bare "đổi sang ăn uống" stays
// unrecognised instead of silently rewriting the wrong transaction.
var recategoryPrefixes = []string{
	"doi khoan gan nhat sang ",
	"chuyen khoan gan nhat sang ",
}

// deleteRecentPhrases are exact folded messages that start the two-step
// individual-deletion flow (P4-D02). Exact match only: deletion must never
// trigger from a loosely-matching sentence.
var deleteRecentPhrases = map[string]bool{
	"xoa khoan gan nhat":      true,
	"xoa khoan vua roi":       true,
	"xoa giao dich gan nhat":  true,
	"xoa giao dich vua roi":   true,
	"xoa khoan chi gan nhat":  true,
	"xoa khoan tieu gan nhat": true,
}

// confirmWords / discardWords match the whole folded message only — "có" and
// "không" are too common to match inside a sentence.
var confirmWords = map[string]bool{
	"xac nhan": true, "dung": true, "dung roi": true, "dong y": true,
	"ok": true, "okay": true, "yes": true, "co": true, "confirm": true,
}

var discardWords = map[string]bool{
	"bo qua": true, "huy": true, "khong": true, "skip": true, "thoi": true,
}

// helpWords match exactly or as a leading word/phrase ("giúp tôi với").
var helpWords = []string{"giup", "huong dan", "tro giup"}

// hasWord reports whether the folded haystack contains needle as a whole
// phrase bounded by spaces or string edges — keeps "chi" out of "chinh".
func hasWord(folded, needle string) bool {
	return strings.Contains(" "+folded+" ", " "+needle+" ")
}

// Parse maps one user text to an Intent. Matching is diacritics-insensitive
// (fold) but manual-entry amounts use normalise.ParseAmount rules. Matching
// order is significant and MUST stay: slash commands → edit/recategory
// phrases → insight phrases → confirm/discard → privacy/help → manual entry
// (leading amount) → none.
func Parse(text string) Intent {
	f := fold(text)
	if f == "" {
		return Intent{Kind: IntentNone}
	}

	// Slash commands: exact folded body after the "/". Settings preserve
	// the original value casing because IANA timezone names are case-sensitive.
	if strings.HasPrefix(f, "/") {
		body := strings.TrimPrefix(f, "/")
		if body == "caidat" {
			return parseSettings("")
		}
		if strings.HasPrefix(body, "caidat ") {
			rawBody := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/"))
			if _, args, ok := strings.Cut(rawBody, " "); ok {
				return parseSettings(args)
			}
			return parseSettings("")
		}
		if body == "tongket" || strings.HasPrefix(body, "tongket ") {
			return parseSummarySchedule(strings.TrimSpace(strings.TrimPrefix(body, "tongket")))
		}
		if kind, ok := slashCommands[body]; ok {
			return Intent{Kind: kind}
		}
		return Intent{Kind: IntentNone}
	}

	// Natural discovery phrase for the settings surface.
	if f == "cai dat" || f == "xem cai dat" {
		return parseSettings("")
	}

	// Natural equivalents for the scheduled-summary settings command.
	switch {
	case f == "tong ket tu dong":
		return parseSummarySchedule("")
	case strings.HasPrefix(f, "bat tong ket "):
		return parseSummarySchedule(strings.TrimPrefix(f, "bat tong ket "))
	case strings.HasPrefix(f, "tat tong ket"):
		tail := strings.TrimSpace(strings.TrimPrefix(f, "tat tong ket"))
		return parseSummarySchedule(strings.TrimSpace("tat " + tail))
	}

	// Edit phrases: exact match so "sửa số tiền" never falls into manual entry.
	if kind, ok := editPhrases[f]; ok {
		return Intent{Kind: kind}
	}
	// Recategory: folded prefix + folded category tail.
	for _, p := range recategoryPrefixes {
		if strings.HasPrefix(f, p) {
			if tail := strings.TrimSpace(strings.TrimPrefix(f, p)); tail != "" {
				return Intent{Kind: IntentRecategory, CategoryText: tail}
			}
		}
	}
	// Delete recent: exact phrases only (see deleteRecentPhrases).
	if deleteRecentPhrases[f] {
		return Intent{Kind: IntentDeleteRecent}
	}

	// Insight phrases: whole-word contains.
	switch {
	case hasWord(f, "hom nay") && (hasWord(f, "tieu") || hasWord(f, "chi") ||
		hasWord(f, "het") || hasWord(f, "bao nhieu")):
		return Intent{Kind: IntentToday}
	case hasWord(f, "tuan truoc"):
		return Intent{Kind: IntentLastWeek}
	case hasWord(f, "tuan nay"):
		return Intent{Kind: IntentWeek}
	case hasWord(f, "thang truoc"):
		return Intent{Kind: IntentLastMonth}
	case hasWord(f, "thang nay"):
		return Intent{Kind: IntentMonth}
	case hasWord(f, "gan day"), hasWord(f, "lich su"):
		return Intent{Kind: IntentRecent}
	}

	// Confirm / discard: whole message only.
	if confirmWords[f] {
		return Intent{Kind: IntentConfirm}
	}
	if discardWords[f] {
		return Intent{Kind: IntentDiscard}
	}

	// Privacy: data-use questions.
	if strings.Contains(f, "cach du lieu") || strings.Contains(f, "chinh sach") ||
		(strings.Contains(f, "du lieu") &&
			(hasWord(f, "xem") || hasWord(f, "su dung") || hasWord(f, "bao mat"))) {
		return Intent{Kind: IntentPrivacy}
	}

	// Help: exact or leading phrase.
	for _, w := range helpWords {
		if f == w || strings.HasPrefix(f, w+" ") {
			return Intent{Kind: IntentHelp}
		}
	}

	// Manual entry: leading amount token + free-text description. The
	// description keeps the user's original casing (only trimmed).
	fields := strings.Fields(text)
	if len(fields) >= 2 {
		if minor, currency, err := normalise.ParseAmount(fields[0], ""); err == nil {
			desc := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
			desc = strings.Trim(desc, " .,!?…;:")
			if desc != "" {
				return Intent{
					Kind:        IntentManualEntry,
					AmountMinor: minor,
					Currency:    currency,
					Description: desc,
					AmountText:  fields[0],
				}
			}
		}
	}

	return Intent{Kind: IntentNone}
}

func parseSettings(args string) Intent {
	intent := Intent{Kind: IntentSettings, SettingsAction: SettingsInvalid}
	rawFields := strings.Fields(strings.TrimSpace(args))
	foldedFields := strings.Fields(fold(args))
	if len(foldedFields) == 0 {
		intent.SettingsAction = SettingsShow
		return intent
	}

	valueAt := -1
	switch {
	case foldedFields[0] == "muigio" && len(foldedFields) == 2:
		intent.SettingsAction, valueAt = SettingsSetTimezone, 1
	case len(foldedFields) == 3 && foldedFields[0] == "mui" && foldedFields[1] == "gio":
		intent.SettingsAction, valueAt = SettingsSetTimezone, 2
	case foldedFields[0] == "tiente" && len(foldedFields) == 2:
		intent.SettingsAction, valueAt = SettingsSetCurrency, 1
	case len(foldedFields) == 3 && foldedFields[0] == "tien" && foldedFields[1] == "te":
		intent.SettingsAction, valueAt = SettingsSetCurrency, 2
	default:
		return intent
	}
	if valueAt >= len(rawFields) {
		intent.SettingsAction = SettingsInvalid
		return intent
	}
	if intent.SettingsAction == SettingsSetTimezone {
		intent.Timezone = rawFields[valueAt]
	} else {
		intent.DefaultCurrency = strings.ToUpper(rawFields[valueAt])
	}
	return intent
}

func parseSummarySchedule(args string) Intent {
	intent := Intent{Kind: IntentSummarySchedule, ScheduleAction: SummaryScheduleInvalid}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		intent.ScheduleAction = SummaryScheduleShow
		return intent
	}
	if fields[0] == "tat" {
		intent.ScheduleAction = SummaryScheduleDisable
		if len(fields) == 1 || (len(fields) == 2 && fields[1] == "ca") {
			intent.DisableAll = true
			return intent
		}
		if len(fields) != 2 {
			intent.ScheduleAction = SummaryScheduleInvalid
			return intent
		}
		frequency, ok := parseSummaryFrequency(fields[1])
		if !ok {
			intent.ScheduleAction = SummaryScheduleInvalid
			return intent
		}
		intent.SummaryFrequency = frequency
		return intent
	}
	if len(fields) != 2 {
		return intent
	}
	frequency, ok := parseSummaryFrequency(fields[0])
	if !ok {
		return intent
	}
	parts := strings.Split(fields[1], ":")
	if len(parts) != 2 {
		return intent
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return intent
	}
	intent.ScheduleAction = SummaryScheduleSet
	intent.SummaryFrequency = frequency
	intent.DeliveryMinute = hour*60 + minute
	return intent
}

func parseSummaryFrequency(text string) (domain.SummaryFrequency, bool) {
	switch text {
	case "ngay":
		return domain.SummaryDaily, true
	case "tuan":
		return domain.SummaryWeekly, true
	case "thang":
		return domain.SummaryMonthly, true
	default:
		return "", false
	}
}

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

Bạn chỉ cần gửi ảnh hóa đơn, hoặc nhập nhanh một khoản như "150000 ăn trưa".

Dữ liệu có thể bao gồm thông tin mua hàng và được lưu để tạo báo cáo chi tiêu. Bạn có thể xóa dữ liệu bất kỳ lúc nào bằng /xoadulieu.

Trả lời "đồng ý" để bắt đầu, hoặc "chính sách" để xem cách dữ liệu được sử dụng.`
}

// PrivacyText explains data use in plain words (the §11.1 second choice).
func PrivacyText() string {
	return `Cách tôi dùng dữ liệu của bạn:

• Ảnh hóa đơn chỉ dùng để đọc thông tin giao dịch; ảnh gốc được xóa sau 30 ngày.
• Giao dịch đã ghi nhận được lưu để tổng hợp báo cáo chi tiêu cho riêng bạn.
• Tôi không chia sẻ dữ liệu của bạn cho bên thứ ba.

Gửi /xuatdulieu để tải về toàn bộ dữ liệu, /xoadulieu để xóa vĩnh viễn.`
}

// WelcomeText greets the user right after consent is recorded.
func WelcomeText() string {
	return `Cảm ơn bạn! Từ giờ bạn có thể:

• Gửi ảnh hóa đơn để tôi đọc và ghi nhận.
• Nhập nhanh: "150000 ăn trưa", "150k cafe".
• Xem tổng hợp: "hôm nay tôi tiêu bao nhiêu?", "tuần này", "tháng này".
• Xem múi giờ, tiền tệ và lịch tổng kết bằng /caidat.

Gõ /trogiup để xem tất cả lệnh.`
}

// HelpText lists commands and natural phrases (§11.5).
func HelpText() string {
	return `Tôi có thể giúp bạn:

• Gửi ảnh hóa đơn — tôi đọc và gợi ý để bạn xác nhận.
• Nhập nhanh một khoản: "150000 ăn trưa", "150k cafe", "1.2tr tiền nhà".

Lệnh:
/homnay — chi tiêu hôm nay
/tuan — chi tiêu tuần này
/thang — chi tiêu tháng này
/tuantruoc — chi tiêu tuần trước
/thangtruoc — chi tiêu tháng trước
/ganday — các khoản gần đây
/caidat — múi giờ, tiền tệ và lịch tổng kết
/tongket — cài tổng kết tự động
/xuatdulieu — tải dữ liệu về
/xoadulieu — xóa toàn bộ dữ liệu
/trogiup — xem hướng dẫn này

Bạn cũng có thể hỏi tự nhiên: "hôm nay tôi tiêu bao nhiêu?", "đổi khoản gần nhất sang đi lại", hoặc "xóa khoản gần nhất".`
}

// SettingsText is the discoverable profile-settings surface. It combines
// user preferences with automatic-summary status so /caidat is the single
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
	b.WriteString("/caidat muigio Asia/Ho_Chi_Minh\n")
	b.WriteString("/caidat tiente VND\n")
	b.WriteString("/tongket — cài lịch tổng kết")
	return b.String()
}

// SettingsUpdatedText confirms a preference change and returns the full
// refreshed settings, so the user can immediately verify its effect.
func SettingsUpdatedText(label, value string, user domain.User, preferences []domain.SummarySchedule) string {
	return fmt.Sprintf("Đã cập nhật %s: %s.\n\n%s", label, value, SettingsText(user, preferences))
}

// SettingsInvalidText explains every supported settings operation.
func SettingsInvalidText() string {
	return "Cú pháp chưa đúng. Dùng /caidat để xem, /caidat muigio Asia/Ho_Chi_Minh hoặc /caidat tiente VND để thay đổi."
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
	b.WriteString("/tongket ngay 20:00\n/tongket tuan 08:00\n/tongket thang 09:00\n")
	b.WriteString("/tongket tat ngay|tuan|thang — tắt một lịch\n/tongket tat ca — tắt tất cả")
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

// SummaryScheduleInvalidText explains valid /tongket input.
func SummaryScheduleInvalidText() string {
	return "Cú pháp chưa đúng. Ví dụ: /tongket ngay 20:00, /tongket tuan 08:00, hoặc /tongket tat ca."
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
	return `Xin lỗi, tôi chưa hỗ trợ loại tin nhắn này. Bạn có thể gửi ảnh hóa đơn hoặc nhập: "150000 ăn trưa".`
}

// UnknownText is the fallback for text Parse cannot classify.
func UnknownText() string {
	return `Tôi chưa hiểu tin nhắn này. Gõ /trogiup để xem cách dùng, hoặc nhập một khoản như "150000 ăn trưa".`
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
	b.WriteString("\nTrả lời: 'xác nhận' để lưu · 'sửa số tiền'/'sửa cửa hàng'/'sửa ngày'/'sửa danh mục'/'sửa loại' để chỉnh · 'bỏ qua' để hủy")
	return b.String()
}

// UnsupportedImageText is plan §11.4 (not a receipt + manual-entry hint).
func UnsupportedImageText() string {
	return `Tôi chưa nhận ra đây là hóa đơn hoặc ảnh giao dịch.

Bạn có thể:
• gửi ảnh rõ hơn;
• cắt bớt phần không liên quan; hoặc
• nhập: "150000 ăn trưa".`
}

// DuplicateReceiptText warns the image was already processed before.
func DuplicateReceiptText() string {
	return `Ảnh này tôi đã xử lý trước đó rồi. Nếu muốn ghi lại, bạn nhập tay nhé, ví dụ: "150000 ăn trưa".`
}

// DailyQuotaText tells the user the per-user daily receipt limit is hit.
func DailyQuotaText(limit int64) string {
	return fmt.Sprintf(`Bạn đã gửi %d ảnh hóa đơn hôm nay — đạt giới hạn rồi. Bạn vẫn có thể nhập tay, ví dụ: "150000 ăn trưa", hoặc gửi ảnh lại vào ngày mai.`, limit)
}

// MonthlyQuotaText tells the user the system-wide monthly image-processing
// allowance is exhausted and suggests manual entry.
func MonthlyQuotaText() string {
	return `Hệ thống đã đạt hạn mức xử lý ảnh trong tháng này. Bạn vẫn có thể nhập tay, ví dụ: "150000 ăn trưa".`
}

// OCRDisabledText is the kill-switch notice.
func OCRDisabledText() string {
	return `Tính năng đọc ảnh hóa đơn đang tạm tắt để bảo trì. Bạn vẫn có thể nhập tay, ví dụ: "150000 ăn trưa".`
}

// ExtractionFailedText reports a permanent processing failure.
func ExtractionFailedText() string {
	return `Tôi không đọc được ảnh này. Bạn có thể gửi ảnh rõ hơn, hoặc nhập tay: "150000 ăn trưa".`
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
		return "Tôi chưa hiểu. Trả lời 'xác nhận' để lưu, hoặc 'bỏ qua' để hủy."
	case domain.PendingDeleteAccount:
		return "Trả lời 'xác nhận' để xóa, hoặc bất kỳ nội dung nào khác để hủy."
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
	return fmt.Sprintf(`%s: chưa có khoản chi tiêu nào được ghi nhận. Gửi ảnh hóa đơn hoặc nhập: "150000 ăn trưa".`, periodLabel)
}

// RecentText renders the /ganday transaction list (newest first), one line
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
Trả lời "xác nhận" để xóa, hoặc bất kỳ nội dung nào khác để hủy.`
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

// NoRecentText is the empty /ganday or recategory-with-nothing message.
func NoRecentText() string {
	return `Chưa có giao dịch nào được ghi nhận. Gửi ảnh hóa đơn hoặc nhập: "150000 ăn trưa".`
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
	b.WriteString("\n\nNếu đây là hai khoản khác nhau, cứ 'xác nhận' như thường. Nếu bị trùng, trả lời 'bỏ qua'.")
	return b.String()
}

// DeleteRecentConfirmText asks before deleting the newest recorded
// transaction (P4-D02): the user sees exactly what will be removed.
func DeleteRecentConfirmText(amount, merchant, date, category string) string {
	return fmt.Sprintf(`Bạn muốn xóa khoản này?

%s · %s · %s · %s

Trả lời 'xác nhận' để xóa, hoặc gửi gì cũng được để giữ lại.`, amount, merchant, date, category)
}

// DeletedRecentText confirms one transaction was deleted.
func DeletedRecentText(amount, merchant string) string {
	return fmt.Sprintf("Đã xóa: %s tại %s.", amount, merchant)
}

// DeleteRecentCancelText answers anything that is not a confirm.
func DeleteRecentCancelText() string {
	return "Đã giữ lại khoản đó, không xóa nhé."
}
