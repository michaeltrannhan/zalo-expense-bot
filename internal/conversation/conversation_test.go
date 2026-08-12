package conversation

import (
	"strings"
	"testing"
	"time"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/insight"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		want     IntentKind
		wantCat  string
		wantDesc string
		wantMin  int64
		wantCur  string
	}{
		// Empty / whitespace.
		{"empty", "", IntentNone, "", "", 0, ""},
		{"whitespace only", "   \n\t ", IntentNone, "", "", 0, ""},

		// Slash commands (folded, diacritics-insensitive).
		{"start", "/batdau", IntentStart, "", "", 0, ""},
		{"today cmd", "/homnay", IntentToday, "", "", 0, ""},
		{"today cmd diacritics", "/hômnay", IntentToday, "", "", 0, ""},
		{"today cmd noisy", " /HomNay! ", IntentToday, "", "", 0, ""},
		{"week cmd", "/tuan", IntentWeek, "", "", 0, ""},
		{"month cmd", "/thang", IntentMonth, "", "", 0, ""},
		{"recent cmd", "/ganday", IntentRecent, "", "", 0, ""},
		{"budget cmd", "/ngansach", IntentBudget, "", "", 0, ""},
		{"export cmd", "/xuatdulieu", IntentExport, "", "", 0, ""},
		{"delete cmd", "/xoadulieu", IntentDeleteData, "", "", 0, ""},
		{"settings cmd", "/caidat", IntentSettings, "", "", 0, ""},
		{"settings cmd diacritics", "/càiđặt", IntentSettings, "", "", 0, ""},
		{"help cmd", "/trogiup", IntentHelp, "", "", 0, ""},
		{"unknown cmd", "/kholai", IntentNone, "", "", 0, ""},

		// Edit phrases (exact folded match).
		{"edit total", "sua so tien", IntentEditTotal, "", "", 0, ""},
		{"edit total diacritics", "sửa số tiền", IntentEditTotal, "", "", 0, ""},
		{"edit total wrong", "sai so tien", IntentEditTotal, "", "", 0, ""},
		{"edit total change", "đổi số tiền", IntentEditTotal, "", "", 0, ""},
		{"edit merchant", "sua cua hang", IntentEditMerchant, "", "", 0, ""},
		{"edit merchant name", "sửa tên cửa hàng", IntentEditMerchant, "", "", 0, ""},
		{"edit merchant wrong name", "sai ten", IntentEditMerchant, "", "", 0, ""},
		{"edit date", "sua ngay", IntentEditDate, "", "", 0, ""},
		{"edit date change", "đổi ngày", IntentEditDate, "", "", 0, ""},
		{"edit category", "sua danh muc", IntentEditCategory, "", "", 0, ""},
		{"edit category change", "đổi danh mục", IntentEditCategory, "", "", 0, ""},
		{"edit type", "sua loai", IntentEditType, "", "", 0, ""},
		{"edit type change", "doi loai", IntentEditType, "", "", 0, ""},

		// Recategory (requires "gần nhất").
		{"recategory", "đổi khoản gần nhất sang đi lại", IntentRecategory, "di lai", "", 0, ""},
		{"recategory plain", "doi khoan gan nhat sang an uong", IntentRecategory, "an uong", "", 0, ""},
		{"recategory chuyen", "chuyển khoản gần nhất sang tiết kiệm", IntentRecategory, "tiet kiem", "", 0, ""},
		{"recategory trailing punct", "Đổi khoản gần nhất sang Ăn uống.", IntentRecategory, "an uong", "", 0, ""},
		{"recategory too loose", "đổi sang ăn uống", IntentNone, "", "", 0, ""},
		{"recategory no tail", "đổi khoản gần nhất sang", IntentNone, "", "", 0, ""},

		// Delete recent (exact phrases only).
		{"delete recent", "xóa khoản gần nhất", IntentDeleteRecent, "", "", 0, ""},
		{"delete recent plain", "xoa khoan gan nhat", IntentDeleteRecent, "", "", 0, ""},
		{"delete recent alt", "xóa khoản vừa rồi", IntentDeleteRecent, "", "", 0, ""},
		{"delete recent tx", "xoa giao dich gan nhat", IntentDeleteRecent, "", "", 0, ""},
		{"delete recent not loose", "xóa khoản gần nhất đi", IntentNone, "", "", 0, ""},

		// Insight phrases.
		{"today phrase", "hôm nay tôi tiêu bao nhiêu?", IntentToday, "", "", 0, ""},
		{"today phrase plain", "hom nay chi bao nhieu", IntentToday, "", "", 0, ""},
		{"today phrase het", "hôm nay hết bao nhiêu rồi", IntentToday, "", "", 0, ""},
		{"today no keyword", "hôm nay trởi đẹp", IntentNone, "", "", 0, ""},
		{"week phrase", "tuần này tiêu gì nhiều không", IntentWeek, "", "", 0, ""},
		{"last week phrase", "tuan truoc the nao", IntentLastWeek, "", "", 0, ""},
		{"last week slash", "/tuantruoc", IntentLastWeek, "", "", 0, ""},
		{"month phrase", "tháng này ăn uống hết bao nhiêu?", IntentMonth, "", "", 0, ""},
		{"last month phrase", "thang truoc tieu gi", IntentLastMonth, "", "", 0, ""},
		{"last month slash", "/thangtruoc", IntentLastMonth, "", "", 0, ""},
		{"recent phrase", "gần đây có gì mới", IntentRecent, "", "", 0, ""},
		{"recent history", "xem lịch sử giao dịch", IntentRecent, "", "", 0, ""},
		{"recent tx history", "giao dich gan day", IntentRecent, "", "", 0, ""},

		// Confirm (exact, short).
		{"confirm xac nhan", "xác nhận", IntentConfirm, "", "", 0, ""},
		{"confirm dong y", "đồng ý", IntentConfirm, "", "", 0, ""},
		{"confirm ok", "Ok", IntentConfirm, "", "", 0, ""},
		{"confirm dung roi", "đúng rồi", IntentConfirm, "", "", 0, ""},
		{"confirm co", "có", IntentConfirm, "", "", 0, ""},
		{"confirm co not bare", "có gì không", IntentNone, "", "", 0, ""},
		{"confirm yes", "yes", IntentConfirm, "", "", 0, ""},

		// Discard (exact).
		{"discard bo qua", "bỏ qua", IntentDiscard, "", "", 0, ""},
		{"discard huy", "huỷ", IntentDiscard, "", "", 0, ""},
		{"discard khong", "không", IntentDiscard, "", "", 0, ""},
		{"discard skip", "skip", IntentDiscard, "", "", 0, ""},
		{"discard thoi", "thôi", IntentDiscard, "", "", 0, ""},
		{"discard khong not bare", "không phải vậy", IntentNone, "", "", 0, ""},

		// Privacy.
		{"privacy cach du lieu", "xem cách dữ liệu được sử dụng", IntentPrivacy, "", "", 0, ""},
		{"privacy chinh sach", "chính sách bảo mật", IntentPrivacy, "", "", 0, ""},
		{"privacy du lieu bao mat", "dữ liệu của tôi có được bảo mật không", IntentPrivacy, "", "", 0, ""},
		{"privacy du lieu xem", "cho xem dữ liệu", IntentPrivacy, "", "", 0, ""},

		// Help.
		{"help giup", "giúp", IntentHelp, "", "", 0, ""},
		{"help huong dan", "hướng dẫn", IntentHelp, "", "", 0, ""},
		{"help tro giup", "tro giup", IntentHelp, "", "", 0, ""},
		{"help prefix", "giúp tôi với", IntentHelp, "", "", 0, ""},
		{"settings natural", "xem cài đặt", IntentSettings, "", "", 0, ""},

		// Manual entry.
		{"manual plain", "150000 ăn trưa", IntentManualEntry, "", "ăn trưa", 150000, "VND"},
		{"manual k", "150k cafe", IntentManualEntry, "", "cafe", 150000, "VND"},
		{"manual tr", "1.2tr tiền nhà", IntentManualEntry, "", "tiền nhà", 1200000, "VND"},
		{"manual dotted", "325.000 Co.opmart", IntentManualEntry, "", "Co.opmart", 325000, "VND"},
		{"manual trailing punct", "150000 ăn trưa.", IntentManualEntry, "", "ăn trưa", 150000, "VND"},
		{"manual usd", "$50 lunch meeting", IntentManualEntry, "", "lunch meeting", 5000, "USD"},
		{"manual no desc", "150000", IntentNone, "", "", 0, ""},
		{"manual bad amount", "abc ăn trưa", IntentNone, "", "", 0, ""},

		// Garbage / precedence.
		{"garbage", "blah blah blah", IntentNone, "", "", 0, ""},
		{"edit not manual", "sua so tien 150000", IntentNone, "", "", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.text)
			if got.Kind != tt.want {
				t.Fatalf("Parse(%q).Kind = %q, want %q", tt.text, got.Kind, tt.want)
			}
			if tt.wantCat != "" && got.CategoryText != tt.wantCat {
				t.Errorf("CategoryText = %q, want %q", got.CategoryText, tt.wantCat)
			}
			if tt.wantDesc != "" && got.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", got.Description, tt.wantDesc)
			}
			if tt.wantMin != 0 && got.AmountMinor != tt.wantMin {
				t.Errorf("AmountMinor = %d, want %d", got.AmountMinor, tt.wantMin)
			}
			if tt.wantCur != "" && got.Currency != tt.wantCur {
				t.Errorf("Currency = %q, want %q", got.Currency, tt.wantCur)
			}
		})
	}
}

func TestParseEditPhraseNeverManual(t *testing.T) {
	// "sửa số tiền" starts with letters, but regression-guard that edit
	// phrases are matched before manual entry regardless of content shape.
	if got := Parse("sua so tien"); got.Kind != IntentEditTotal {
		t.Fatalf("Kind = %q, want %q", got.Kind, IntentEditTotal)
	}
}

func TestParseSettings(t *testing.T) {
	tests := []struct {
		text     string
		action   SettingsAction
		timezone string
		currency string
	}{
		{"/caidat", SettingsShow, "", ""},
		{"/càiđặt múi giờ Asia/Ho_Chi_Minh", SettingsSetTimezone, "Asia/Ho_Chi_Minh", ""},
		{"/caidat muigio UTC", SettingsSetTimezone, "UTC", ""},
		{"/caidat tiền tệ usd", SettingsSetCurrency, "", "USD"},
		{"/caidat tiente AUD", SettingsSetCurrency, "", "AUD"},
		{"/caidat muigio", SettingsInvalid, "", ""},
		{"/caidat unknown value", SettingsInvalid, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			got := Parse(tt.text)
			if got.Kind != IntentSettings || got.SettingsAction != tt.action {
				t.Fatalf("Parse(%q) = kind %q action %q", tt.text, got.Kind, got.SettingsAction)
			}
			if got.Timezone != tt.timezone || got.DefaultCurrency != tt.currency {
				t.Fatalf("Parse(%q) values = timezone %q currency %q", tt.text, got.Timezone, got.DefaultCurrency)
			}
		})
	}
}

func TestManualEntryPreservesRawAmount(t *testing.T) {
	got := Parse("$50 lunch")
	if got.Kind != IntentManualEntry || got.AmountText != "$50" {
		t.Fatalf("Parse manual amount = kind %q raw %q", got.Kind, got.AmountText)
	}
}

func TestParseSummarySchedule(t *testing.T) {
	tests := []struct {
		text       string
		action     SummaryScheduleAction
		frequency  domain.SummaryFrequency
		minute     int
		disableAll bool
	}{
		{"/tongket", SummaryScheduleShow, "", 0, false},
		{"/tongket ngày 20:05", SummaryScheduleSet, domain.SummaryDaily, 20*60 + 5, false},
		{"bật tổng kết tuần 08:30", SummaryScheduleSet, domain.SummaryWeekly, 8*60 + 30, false},
		{"/tongket thang 09:00", SummaryScheduleSet, domain.SummaryMonthly, 9 * 60, false},
		{"tắt tổng kết ngày", SummaryScheduleDisable, domain.SummaryDaily, 0, false},
		{"/tongket tat ca", SummaryScheduleDisable, "", 0, true},
		{"/tongket ngay 25:00", SummaryScheduleInvalid, "", 0, false},
		{"/tongket ngay", SummaryScheduleInvalid, "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			got := Parse(tt.text)
			if got.Kind != IntentSummarySchedule || got.ScheduleAction != tt.action {
				t.Fatalf("Parse(%q) = kind %q action %q", tt.text, got.Kind, got.ScheduleAction)
			}
			if got.SummaryFrequency != tt.frequency || got.DeliveryMinute != tt.minute || got.DisableAll != tt.disableAll {
				t.Fatalf("Parse(%q) schedule fields = %q/%d/all=%v", tt.text,
					got.SummaryFrequency, got.DeliveryMinute, got.DisableAll)
			}
		})
	}
}

func TestTemplateSmoke(t *testing.T) {
	funcs := map[string]string{
		"ConsentCard": ConsentCard(),
		"PrivacyText": PrivacyText(),
		"WelcomeText": WelcomeText(),
		"HelpText":    HelpText(),
		"SettingsText": SettingsText(domain.User{
			Timezone: "Asia/Ho_Chi_Minh", DefaultCurrency: "VND", Locale: "vi-VN",
		}, nil),
		"SettingsInvalidText":        SettingsInvalidText(),
		"InvalidTimezoneText":        InvalidTimezoneText(),
		"InvalidCurrencyText":        InvalidCurrencyText(),
		"SuspendedText":              SuspendedText(),
		"NotAllowedText":             NotAllowedText(),
		"UnsupportedEventText":       UnsupportedEventText(),
		"UnknownText":                UnknownText(),
		"BudgetUnsupportedText":      BudgetUnsupportedText(),
		"SummaryScheduleText":        SummaryScheduleText(nil),
		"SummaryScheduleInvalidText": SummaryScheduleInvalidText(),
		"ReceiptReceivedText":        ReceiptReceivedText(),
		"UnsupportedImageText":       UnsupportedImageText(),
		"DuplicateReceiptText":       DuplicateReceiptText(),
		"DailyQuotaText":             DailyQuotaText(15),
		"MonthlyQuotaText":           MonthlyQuotaText(),
		"OCRDisabledText":            OCRDisabledText(),
		"ExtractionFailedText":       ExtractionFailedText(),
		"ConfirmedText":              ConfirmedText("325.000 ₫", "Co.opmart", "Thực phẩm"),
		"DiscardedText":              DiscardedText(),
		"EditInvalidText":            EditInvalidText(domain.PendingEditTotal),
		"CorrectedText":              CorrectedText("card"),
		"EmptySummaryText":           EmptySummaryText("Hôm nay"),
		"DeleteConfirmText":          DeleteConfirmText(),
		"DeletedText":                DeletedText(),
		"NoRecentText":               NoRecentText(),
		"PendingExpiredText":         PendingExpiredText(),
		"ExportReadyText":            ExportReadyText("/tmp/a.csv", "/tmp/a.json", 42),
		"TypeListText":               TypeListText(),
		"RecategorisedText":          RecategorisedText("Co.opmart", "Ăn uống"),
		"CategoryListText": CategoryListText([]domain.Category{
			{DisplayName: "Ăn uống"}, {DisplayName: "Đi lại"},
		}),
	}
	for name, got := range funcs {
		if strings.TrimSpace(got) == "" {
			t.Errorf("%s returned empty", name)
		}
	}

	// EditPrompt: non-empty for every prompting kind, empty for confirm.
	for _, k := range []domain.PendingKind{
		domain.PendingEditTotal, domain.PendingEditMerchant, domain.PendingEditDate,
		domain.PendingEditCategory, domain.PendingEditType, domain.PendingDeleteAccount,
	} {
		if EditPrompt(k) == "" {
			t.Errorf("EditPrompt(%q) empty", k)
		}
	}
	if got := EditPrompt(domain.PendingConfirmExtraction); got != "" {
		t.Errorf("EditPrompt(confirm_extraction) = %q, want empty", got)
	}
}

func TestEditPromptExact(t *testing.T) {
	if got := EditPrompt(domain.PendingEditTotal); got != "Nhập số tiền đúng (ví dụ: 325000):" {
		t.Errorf("EditPrompt(edit_total) = %q", got)
	}
	if got := EditPrompt(domain.PendingEditMerchant); got != "Nhập tên cửa hàng đúng:" {
		t.Errorf("EditPrompt(edit_merchant) = %q", got)
	}
	if got := EditPrompt(domain.PendingEditDate); got != "Nhập ngày đúng (ví dụ: 19/07/2026):" {
		t.Errorf("EditPrompt(edit_date) = %q", got)
	}
	if got := EditPrompt(domain.PendingDeleteAccount); got != DeleteConfirmText() {
		t.Errorf("EditPrompt(delete_account) should delegate to DeleteConfirmText")
	}
}

func TestExtractionCard(t *testing.T) {
	d := CardData{
		Merchant: "Co.opmart",
		Amount:   "325.000 ₫",
		Date:     "19/07/2026",
		Type:     "Chi tiêu",
		Category: "Thực phẩm",
		Flags:    map[string]bool{"total": true, "merchant": true},
	}
	card := ExtractionCard(d)
	for _, want := range []string{
		"Tôi chưa chắc về tổng tiền.",
		"Tôi đọc được:",
		"Cửa hàng: Co.opmart  ⚠️",
		"Số tiền: 325.000 ₫  ⚠️",
		"Ngày: 19/07/2026",
		"Loại: Chi tiêu",
		"Danh mục: Thực phẩm",
		"'xác nhận' để lưu",
		"'bỏ qua' để hủy",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("ExtractionCard missing %q:\n%s", want, card)
		}
	}

	// No flags → no lead-in, no ⚠️.
	d.Flags = nil
	clean := ExtractionCard(d)
	if strings.Contains(clean, "⚠️") || strings.Contains(clean, "Tôi chưa chắc") {
		t.Errorf("unflagged card should have no ⚠️/lead-in:\n%s", clean)
	}

	// Currency flag alone also flags the amount line.
	d.Flags = map[string]bool{"currency": true}
	cur := ExtractionCard(d)
	if !strings.Contains(cur, "Số tiền: 325.000 ₫  ⚠️") {
		t.Errorf("currency flag should flag amount line:\n%s", cur)
	}
}

func TestSummaryText(t *testing.T) {
	loc := time.FixedZone("ICT", 7*3600)
	s := insight.Summary{
		CurrencyTotals: map[string]int64{"VND": 650000},
		RefundTotals:   map[string]int64{"VND": 50000},
		ByCategory: []insight.CategoryTotal{
			{DisplayName: "Thực phẩm", Currency: "VND", Minor: 325000},
			{DisplayName: "Ăn uống", Currency: "VND", Minor: 325000},
		},
		TxCount: 3,
		LargestByCurrency: []insight.LargestTx{{
			Merchant:   "Co.opmart",
			Minor:      325000,
			Currency:   "VND",
			OccurredAt: time.Date(2026, 7, 19, 10, 0, 0, 0, loc),
		}},
		NoSpendDays: 2,
	}
	got := SummaryText("Tuần này", s, loc)
	for _, want := range []string{
		"Tuần này", "chi tiêu đã ghi nhận", "325.000 ₫", "650.000 ₫",
		"Hoàn tiền: 50.000 ₫", "Theo danh mục:", "Khoản lớn nhất",
		"Co.opmart", "19/07", "Số ngày không chi: 2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SummaryText missing %q:\n%s", want, got)
		}
	}
}

func TestSummaryTextTop5Categories(t *testing.T) {
	s := insight.Summary{
		CurrencyTotals: map[string]int64{"VND": 6000},
		TxCount:        6,
	}
	for i, name := range []string{"A", "B", "C", "D", "E", "F"} {
		s.ByCategory = append(s.ByCategory, insight.CategoryTotal{
			DisplayName: name, Currency: "VND", Minor: int64(6000 - i*1000),
		})
	}
	got := SummaryText("Tháng này", s, time.UTC)
	if strings.Contains(got, "• F:") {
		t.Errorf("SummaryText should cap categories at 5:\n%s", got)
	}
	if !strings.Contains(got, "• E:") {
		t.Errorf("SummaryText should include 5th category:\n%s", got)
	}
}

func TestSummaryTextKeepsCurrenciesAttached(t *testing.T) {
	s := insight.Summary{
		CurrencyTotals: map[string]int64{"AUD": 1234, "VND": 250000},
		ByCategory: []insight.CategoryTotal{
			{DisplayName: "Ăn uống", Currency: "AUD", Minor: 1234},
			{DisplayName: "Thực phẩm", Currency: "VND", Minor: 250000},
		},
		TxCount: 2,
		LargestByCurrency: []insight.LargestTx{
			{Merchant: "Cafe Oz", Minor: 1234, Currency: "AUD", OccurredAt: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)},
			{Merchant: "Co.opmart", Minor: 250000, Currency: "VND", OccurredAt: time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)},
		},
	}
	got := SummaryText("Tuần này", s, time.UTC)
	for _, want := range []string{
		"Ăn uống (AUD): 12,34 A$",
		"Thực phẩm (VND): 250.000 ₫",
		"Khoản lớn nhất (AUD): 12,34 A$",
		"Khoản lớn nhất (VND): 250.000 ₫",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SummaryText missing %q:\n%s", want, got)
		}
	}
}

func TestSummaryTextEmptyDelegates(t *testing.T) {
	got := SummaryText("Hôm nay", insight.Summary{}, time.UTC)
	if got != EmptySummaryText("Hôm nay") {
		t.Errorf("SummaryText(TxCount==0) = %q, want EmptySummaryText", got)
	}
}

func TestRecentText(t *testing.T) {
	lines := []RecentLine{
		{Date: "19/07", Amount: "325.000 ₫", Merchant: "Co.opmart", Category: "Thực phẩm", Type: "Chi tiêu"},
		{Date: "18/07", Amount: "50.000 ₫", Merchant: "Grab", Category: "Đi lại", Type: "Hoàn tiền"},
	}
	got := RecentText(lines)
	if !strings.HasPrefix(got, "Các khoản gần đây:") {
		t.Errorf("RecentText missing header:\n%s", got)
	}
	if !strings.Contains(got, "19/07 · 325.000 ₫ · Co.opmart · Thực phẩm") {
		t.Errorf("RecentText missing expense line:\n%s", got)
	}
	if !strings.Contains(got, "[Hoàn tiền] 18/07 · 50.000 ₫ · Grab · Đi lại") {
		t.Errorf("RecentText missing refund-tagged line:\n%s", got)
	}
	if got := RecentText(nil); got != NoRecentText() {
		t.Errorf("RecentText(nil) = %q, want NoRecentText", got)
	}
}

func TestDateVN(t *testing.T) {
	loc := time.FixedZone("ICT", 7*3600)
	// 23:30 UTC is already the next day in +7.
	in := time.Date(2026, 7, 19, 23, 30, 0, 0, time.UTC)
	if got := DateVN(in, loc); got != "20/07/2026" {
		t.Errorf("DateVN = %q, want %q", got, "20/07/2026")
	}
	if got := DateVN(in, time.UTC); got != "19/07/2026" {
		t.Errorf("DateVN = %q, want %q", got, "19/07/2026")
	}
}

func TestTypeLabel(t *testing.T) {
	want := map[domain.TxType]string{
		domain.TxExpense:    "Chi tiêu",
		domain.TxIncome:     "Thu nhập",
		domain.TxRefund:     "Hoàn tiền",
		domain.TxTransfer:   "Chuyển khoản",
		domain.TxAdjustment: "Điều chỉnh",
	}
	for tx, label := range want {
		if got := TypeLabel(tx); got != label {
			t.Errorf("TypeLabel(%q) = %q, want %q", tx, got, label)
		}
	}
}

func TestCategoryListText(t *testing.T) {
	got := CategoryListText([]domain.Category{
		{DisplayName: "Ăn uống"}, {DisplayName: "Đi lại"},
	})
	if !strings.Contains(got, "1. Ăn uống") || !strings.Contains(got, "2. Đi lại") {
		t.Errorf("CategoryListText missing numbered lines:\n%s", got)
	}
	if !strings.Contains(got, "Trả lời số hoặc tên danh mục.") {
		t.Errorf("CategoryListText missing instruction:\n%s", got)
	}
}

func TestTypeListText(t *testing.T) {
	got := TypeListText()
	for _, want := range []string{
		"1. Chi tiêu", "2. Thu nhập", "3. Hoàn tiền", "4. Chuyển khoản", "5. Điều chỉnh",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("TypeListText missing %q:\n%s", want, got)
		}
	}
}

func TestDeleteConfirmText(t *testing.T) {
	got := DeleteConfirmText()
	for _, want := range []string{"giao dịch", "Ảnh hóa đơn", "quy tắc", "xác nhận"} {
		if !strings.Contains(got, want) {
			t.Errorf("DeleteConfirmText missing %q:\n%s", want, got)
		}
	}
}

func TestPossibleDuplicateText(t *testing.T) {
	got := PossibleDuplicateText([]DupLine{
		{Amount: "325.000 ₫", Merchant: "Co.opmart", Date: "15/07/2026"},
		{Amount: "325.000 ₫", Merchant: "CO.OPMART NGUYỄN TRÃI", Date: "12/07/2026"},
	})
	for _, want := range []string{
		"có thể trùng", "325.000 ₫ · Co.opmart · 15/07/2026",
		"CO.OPMART NGUYỄN TRÃI", "xác nhận", "bỏ qua",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("PossibleDuplicateText missing %q:\n%s", want, got)
		}
	}
}
