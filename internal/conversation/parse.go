package conversation

import (
	"strconv"
	"strings"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/normalise"
)

// IntentKind classifies one inbound text. Parsing is pure: no store, no
// provider — the bot handler resolves intents against user state and
// pending actions.
type IntentKind string

const (
	IntentNone            IntentKind = ""                 // not understood
	IntentStart           IntentKind = "start"            // /start — consent card (pending) or help (active)
	IntentConfirm         IntentKind = "confirm"          // ok, y, "xác nhận"
	IntentDiscard         IntentKind = "discard"          // no, n, "bỏ qua"
	IntentPrivacy         IntentKind = "privacy"          // /privacy
	IntentHelp            IntentKind = "help"             // /help
	IntentToday           IntentKind = "today"            // /today
	IntentWeek            IntentKind = "week"             // /week
	IntentMonth           IntentKind = "month"            // /month
	IntentLastWeek        IntentKind = "last_week"        // /lastweek
	IntentLastMonth       IntentKind = "last_month"       // /lastmonth
	IntentRecent          IntentKind = "recent"           // /recent
	IntentBudget          IntentKind = "budget"           // /budget — not in MVP, polite unsupported
	IntentExport          IntentKind = "export"           // /export
	IntentDeleteData      IntentKind = "delete"           // /delete — two-step account wipe
	IntentSettings        IntentKind = "settings"         // /settings [tz|currency value]
	IntentSummarySchedule IntentKind = "summary_schedule" // /sched [daily|weekly|monthly HH:MM|off ...]
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
	// IntentManualEntry is "150000 ăn trưa" / "an sang 500k" / "cafe 45k":
	// a parseable amount plus leftover description, either order.
	IntentManualEntry IntentKind = "manual_entry"
)

// SummaryScheduleAction identifies a /sched settings operation.
type SummaryScheduleAction string

const (
	SummaryScheduleShow    SummaryScheduleAction = "show"
	SummaryScheduleSet     SummaryScheduleAction = "set"
	SummaryScheduleDisable SummaryScheduleAction = "disable"
	SummaryScheduleInvalid SummaryScheduleAction = "invalid"
)

// SettingsAction identifies a /settings profile-preference operation.
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

// BotCommand is one entry in the Zalo slash-command picker. Command has no
// leading slash: Zalo's Bot API follows the Telegram setMyCommands shape
// (1–32 lowercase letters, digits, underscores).
type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SlashMenu is the short English/abbrev list registered with Zalo so the
// in-chat "/" picker shows real bot commands instead of the platform
// default /xinchao. Vietnamese phrases stay typed aliases and are not
// all listed here.
func SlashMenu() []BotCommand {
	return []BotCommand{
		{Command: "help", Description: "Hướng dẫn"},
		{Command: "today", Description: "Chi tiêu hôm nay"},
		{Command: "week", Description: "Chi tiêu tuần này"},
		{Command: "month", Description: "Chi tiêu tháng này"},
		{Command: "recent", Description: "Các khoản gần đây"},
		{Command: "settings", Description: "Múi giờ, tiền tệ"},
		{Command: "sched", Description: "Tổng kết tự động"},
		{Command: "export", Description: "Tải dữ liệu"},
		{Command: "delete", Description: "Xóa toàn bộ dữ liệu"},
		{Command: "privacy", Description: "Cách dùng dữ liệu"},
	}
}

// slashCommands maps the folded command body (after "/") to its intent.
// English/abbrev forms are primary; Vietnamese names stay as aliases.
var slashCommands = map[string]IntentKind{
	"start":      IntentStart,
	"batdau":     IntentStart,
	"xinchao":    IntentStart, // leftover Zalo platform default
	"help":       IntentHelp,
	"trogiup":    IntentHelp,
	"today":      IntentToday,
	"homnay":     IntentToday,
	"week":       IntentWeek,
	"tuan":       IntentWeek,
	"month":      IntentMonth,
	"thang":      IntentMonth,
	"lastweek":   IntentLastWeek,
	"tuantruoc":  IntentLastWeek,
	"lastmonth":  IntentLastMonth,
	"thangtruoc": IntentLastMonth,
	"recent":     IntentRecent,
	"ganday":     IntentRecent,
	"budget":     IntentBudget,
	"ngansach":   IntentBudget,
	"export":     IntentExport,
	"xuatdulieu": IntentExport,
	"delete":     IntentDeleteData,
	"wipe":       IntentDeleteData,
	"xoadulieu":  IntentDeleteData,
	"privacy":    IntentPrivacy,
	"consent":    IntentPrivacy,
	"confirm":    IntentConfirm,
	"ok":         IntentConfirm,
	"y":          IntentConfirm,
	"discard":    IntentDiscard,
	"no":         IntentDiscard,
	"n":          IntentDiscard,
	"edit":       IntentEditTotal,
	"fix":        IntentEditTotal,
}

var settingsSlash = []string{"settings", "tz", "caidat"}
var scheduleSlash = []string{"sched", "tongket"}

// exactCommands are bare (no slash) English/abbrev messages. Vietnamese
// natural phrases are matched separately below.
var exactCommands = map[string]IntentKind{
	"help":      IntentHelp,
	"today":     IntentToday,
	"week":      IntentWeek,
	"month":     IntentMonth,
	"lastweek":  IntentLastWeek,
	"lastmonth": IntentLastMonth,
	"recent":    IntentRecent,
	"budget":    IntentBudget,
	"export":    IntentExport,
	"delete":    IntentDeleteData,
	"wipe":      IntentDeleteData,
	"privacy":   IntentPrivacy,
	"consent":   IntentPrivacy,
	"start":     IntentStart,
}

// editPhrases are exact folded messages that start a field-correction flow.
var editPhrases = map[string]IntentKind{
	"edit":             IntentEditTotal,
	"fix":              IntentEditTotal,
	"edit amount":      IntentEditTotal,
	"edit total":       IntentEditTotal,
	"sua so tien":      IntentEditTotal,
	"sai so tien":      IntentEditTotal,
	"doi so tien":      IntentEditTotal,
	"edit merchant":    IntentEditMerchant,
	"sua cua hang":     IntentEditMerchant,
	"sai ten":          IntentEditMerchant,
	"doi cua hang":     IntentEditMerchant,
	"sua ten cua hang": IntentEditMerchant,
	"edit date":        IntentEditDate,
	"sua ngay":         IntentEditDate,
	"sai ngay":         IntentEditDate,
	"doi ngay":         IntentEditDate,
	"edit category":    IntentEditCategory,
	"sua danh muc":     IntentEditCategory,
	"doi danh muc":     IntentEditCategory,
	"edit type":        IntentEditType,
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
	"ok": true, "okay": true, "yes": true, "y": true, "co": true, "confirm": true,
}

var discardWords = map[string]bool{
	"bo qua": true, "huy": true, "khong": true, "skip": true, "thoi": true,
	"no": true, "n": true, "discard": true,
}

// helpWords match exactly or as a leading word/phrase ("giúp tôi với").
var helpWords = []string{"help", "giup", "huong dan", "tro giup"}

// hasWord reports whether the folded haystack contains needle as a whole
// phrase bounded by spaces or string edges — keeps "chi" out of "chinh".
func hasWord(folded, needle string) bool {
	return strings.Contains(" "+folded+" ", " "+needle+" ")
}

// Parse maps one user text to an Intent. Matching is diacritics-insensitive
// (fold) but manual-entry amounts use normalise.ParseAmount rules. Matching
// order is significant and MUST stay: slash commands → edit/recategory
// phrases → insight phrases → confirm/discard → privacy/help → manual entry
// (amount + description, either order) → none.
func Parse(text string) Intent {
	f := fold(text)
	if f == "" {
		return Intent{Kind: IntentNone}
	}

	// Slash commands: exact folded body after the "/". Settings preserve
	// the original value casing because IANA timezone names are case-sensitive.
	if strings.HasPrefix(f, "/") {
		body := strings.TrimPrefix(f, "/")
		if name, _, ok := matchNamedSlash(body, settingsSlash); ok {
			return parseSettings(rawSlashArgs(text, name))
		}
		if name, foldedArgs, ok := matchNamedSlash(body, scheduleSlash); ok {
			if name == "sched" {
				return parseSummarySchedule(foldedArgs)
			}
			return parseSummarySchedule(strings.TrimSpace(strings.TrimPrefix(body, name)))
		}
		if kind, ok := slashCommands[body]; ok {
			return Intent{Kind: kind}
		}
		return Intent{Kind: IntentNone}
	}

	// Natural discovery phrases for the settings surface.
	if f == "cai dat" || f == "xem cai dat" || f == "settings" {
		return parseSettings("")
	}

	// Natural equivalents for the scheduled-summary settings command.
	switch {
	case f == "tong ket tu dong" || f == "sched":
		return parseSummarySchedule("")
	case strings.HasPrefix(f, "bat tong ket "):
		return parseSummarySchedule(strings.TrimPrefix(f, "bat tong ket "))
	case strings.HasPrefix(f, "tat tong ket"):
		tail := strings.TrimSpace(strings.TrimPrefix(f, "tat tong ket"))
		return parseSummarySchedule(strings.TrimSpace("tat " + tail))
	}

	// Bare English/abbrev commands (no slash).
	if kind, ok := exactCommands[f]; ok {
		return Intent{Kind: kind}
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

	if intent, ok := parseManualEntry(text); ok {
		return intent
	}

	return Intent{Kind: IntentNone}
}

// parseManualEntry accepts a leading amount ("150k cafe") or a trailing
// amount ("an sang 500k", "cafe 45k", "com 80.000"). Description keeps the
// user's original casing. A lone amount with no leftover text is not an
// entry — callers should keep the unknown/help path.
func parseManualEntry(text string) (Intent, bool) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return Intent{}, false
	}
	if intent, ok := manualFromAmount(fields[0], strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))); ok {
		if !commandLikeDescription(intent.Description) {
			return intent, true
		}
	}
	last := strings.Trim(fields[len(fields)-1], ".,!?…;:")
	desc := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), fields[len(fields)-1]))
	desc = strings.Trim(desc, " .,!?…;:")
	if commandLikeDescription(desc) {
		return Intent{}, false
	}
	if intent, ok := manualFromAmount(last, desc); ok {
		return intent, true
	}
	return Intent{}, false
}

// commandLikeDescription keeps trailing-amount parsing from stealing edit
// phrases and confirm/discard/summary commands ("sua so tien 150000").
func commandLikeDescription(desc string) bool {
	f := fold(desc)
	if f == "" {
		return false
	}
	if _, ok := editPhrases[f]; ok {
		return true
	}
	if confirmWords[f] || discardWords[f] || deleteRecentPhrases[f] {
		return true
	}
	if _, ok := exactCommands[f]; ok {
		return true
	}
	if f == "settings" || f == "sched" || f == "cai dat" || f == "xem cai dat" {
		return true
	}
	return false
}

func manualFromAmount(amountToken, desc string) (Intent, bool) {
	desc = strings.Trim(strings.TrimSpace(desc), " .,!?…;:")
	if desc == "" {
		return Intent{}, false
	}
	minor, currency, err := normalise.ParseAmount(amountToken, "")
	if err != nil {
		return Intent{}, false
	}
	return Intent{
		Kind:        IntentManualEntry,
		AmountMinor: minor,
		Currency:    currency,
		Description: desc,
		AmountText:  amountToken,
	}, true
}

func matchNamedSlash(body string, names []string) (name, args string, ok bool) {
	for _, n := range names {
		if body == n {
			return n, "", true
		}
		if strings.HasPrefix(body, n+" ") {
			return n, strings.TrimSpace(body[len(n):]), true
		}
	}
	return "", "", false
}

func rawSlashArgs(text, foldedName string) string {
	raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/"))
	_, args, found := strings.Cut(raw, " ")
	if !found {
		return ""
	}
	_ = foldedName
	return strings.TrimSpace(args)
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
	case (foldedFields[0] == "muigio" || foldedFields[0] == "tz" || foldedFields[0] == "timezone") && len(foldedFields) == 2:
		intent.SettingsAction, valueAt = SettingsSetTimezone, 1
	case len(foldedFields) == 3 && foldedFields[0] == "mui" && foldedFields[1] == "gio":
		intent.SettingsAction, valueAt = SettingsSetTimezone, 2
	case (foldedFields[0] == "tiente" || foldedFields[0] == "currency") && len(foldedFields) == 2:
		intent.SettingsAction, valueAt = SettingsSetCurrency, 1
	case len(foldedFields) == 3 && foldedFields[0] == "tien" && foldedFields[1] == "te":
		intent.SettingsAction, valueAt = SettingsSetCurrency, 2
	case len(foldedFields) == 1 && (strings.Contains(rawFields[0], "/") || strings.EqualFold(rawFields[0], "UTC")):
		intent.SettingsAction, valueAt = SettingsSetTimezone, 0
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
	if fields[0] == "tat" || fields[0] == "off" {
		intent.ScheduleAction = SummaryScheduleDisable
		if len(fields) == 1 || (len(fields) == 2 && (fields[1] == "ca" || fields[1] == "all")) {
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
	case "ngay", "daily":
		return domain.SummaryDaily, true
	case "tuan", "weekly":
		return domain.SummaryWeekly, true
	case "thang", "monthly":
		return domain.SummaryMonthly, true
	default:
		return "", false
	}
}

func fold(s string) string {
	return strings.Trim(normalise.Fold(s), ".,!?…;:")
}
