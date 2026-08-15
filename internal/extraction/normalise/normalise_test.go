package normalise

import (
	"testing"
	"time"

	"zl-expese-bot/internal/domain"
)

func TestParseAmount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		hint     string
		wantMin  int64
		wantCurr string
	}{
		// VND forms: both separators are thousands separators.
		{"vnd dots", "150.000", "", 150000, "VND"},
		{"vnd commas", "150,000", "", 150000, "VND"},
		{"vnd plain", "150000", "", 150000, "VND"},
		{"vnd k lower", "150k", "", 150000, "VND"},
		{"vnd k upper", "150K", "", 150000, "VND"},
		{"vnd đ suffix", "150.000đ", "", 150000, "VND"},
		{"vnd đồng symbol", "325.000 ₫", "", 325000, "VND"},
		{"vnd đồng word", "325.000 đồng", "", 325000, "VND"},
		{"vnd đồng glued", "325.000đồng", "", 325000, "VND"},
		{"vnd dong word", "85.000 dong", "", 85000, "VND"},
		{"vnd VNĐ word", "500000 VNĐ", "", 500000, "VND"},
		{"vnd tr decimal", "1.2tr", "", 1200000, "VND"},
		{"vnd tr comma decimal", "1,2tr", "", 1200000, "VND"},
		{"vnd triệu", "2 triệu", "", 2000000, "VND"},
		{"vnd k with thousands", "1.500k", "", 1500000, "VND"},
		{"vnd mixed separators", "1.234,567", "", 1234567, "VND"},
		// AUD/USD forms: last "." with exactly 2 digits is the decimal point.
		{"usd thousands+decimal", "1,234.56", "USD", 123456, "USD"},
		{"usd dollar sign", "$12.34", "", 1234, "USD"},
		{"aud a-dollar", "A$12.34", "", 1234, "AUD"},
		{"aud word", "18.90 AUD", "", 1890, "AUD"},
		{"aud hint", "12.34", "AUD", 1234, "AUD"},
		{"aud no decimal scales ×100", "50", "AUD", 5000, "AUD"},
		{"usd integer scales ×100", "50", "USD", 5000, "USD"},
		{"vnd integer no scale", "50", "VND", 50, "VND"},
		{"usd cents from printed decimal", "12.34", "USD", 1234, "USD"},
		{"aud cents from printed decimal", "12.34", "AUD", 1234, "AUD"},
		{"vnd printed with dong ignores usd-style cents", "12.34", "VND", 1234, "VND"},
		// Documented rule: only "." is a decimal point for 2-decimal
		// currencies; a comma with 2 trailing digits stays thousands.
		{"usd comma not decimal", "12,34", "USD", 123400, "USD"},
		// Empty hint with no symbol defaults to VND.
		{"empty hint defaults vnd", "42000", "", 42000, "VND"},
		{"hint uppercase normalised", "42000", "usd", 4200000, "USD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			minor, curr, err := ParseAmount(tt.raw, tt.hint)
			if err != nil {
				t.Fatalf("ParseAmount(%q, %q) error: %v", tt.raw, tt.hint, err)
			}
			if minor != tt.wantMin || curr != tt.wantCurr {
				t.Fatalf("ParseAmount(%q, %q) = (%d, %s), want (%d, %s)",
					tt.raw, tt.hint, minor, curr, tt.wantMin, tt.wantCurr)
			}
		})
	}
}

func TestParseAmountErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		hint string
	}{
		{"empty", "", ""},
		{"negative plain", "-50", ""},
		{"negative with symbol", "-$12.34", ""},
		{"no digits", "₫", ""},
		{"garbage letters", "abc", ""},
		{"overflow 16 digits", "9999999999999999", ""},
		{"overflow via decimal", "999999999999999.99", "USD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			minor, _, err := ParseAmount(tt.raw, tt.hint)
			if err == nil {
				t.Fatalf("ParseAmount(%q, %q) = %d, want error", tt.raw, tt.hint, minor)
			}
			if !domain.IsCode(err, domain.CodeValidation) {
				t.Fatalf("ParseAmount(%q) error code = %s, want validation", tt.raw, domain.CodeOf(err))
			}
		})
	}
}

func TestParseDate(t *testing.T) {
	t.Parallel()
	utc := func(y int, m time.Month, d, hh, mm int) time.Time {
		return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
	}
	tests := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"dd/mm/yyyy", "19/07/2026", utc(2026, time.July, 19, 0, 0)},
		{"dd/mm/yy", "19/07/26", utc(2026, time.July, 19, 0, 0)},
		{"d/m/yyyy", "9/7/2026", utc(2026, time.July, 9, 0, 0)},
		{"dd-mm-yyyy", "19-07-2026", utc(2026, time.July, 19, 0, 0)},
		{"iso", "2026-07-19", utc(2026, time.July, 19, 0, 0)},
		{"space time", "19/07/2026 10:30", utc(2026, time.July, 19, 10, 30)},
		{"comma time", "19/07/2026, 10:30", utc(2026, time.July, 19, 10, 30)},
		{"iso with time", "2026-07-19 08:05", utc(2026, time.July, 19, 8, 5)},
		{"pivot 69 → 2069", "01/01/69", utc(2069, time.January, 1, 0, 0)},
		{"pivot 70 → 1970", "01/01/70", utc(1970, time.January, 1, 0, 0)},
		{"pivot 00 → 2000", "28/02/00", utc(2000, time.February, 28, 0, 0)},
		{"pivot 99 → 1999", "31/12/99", utc(1999, time.December, 31, 0, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDate(tt.raw, time.UTC)
			if err != nil {
				t.Fatalf("ParseDate(%q) error: %v", tt.raw, err)
			}
			if !got.Equal(tt.want) {
				t.Fatalf("ParseDate(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseDateTimezone(t *testing.T) {
	t.Parallel()
	hcm := time.FixedZone("ICT", 7*3600)
	got, err := ParseDate("19/07/2026 10:30", hcm)
	if err != nil {
		t.Fatalf("ParseDate error: %v", err)
	}
	want := time.Date(2026, time.July, 19, 3, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("ParseDate ICT = %v, want %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("result location = %v, want UTC", got.Location())
	}
	// nil tz behaves as UTC.
	gotNil, err := ParseDate("19/07/2026 10:30", nil)
	if err != nil {
		t.Fatalf("ParseDate nil tz error: %v", err)
	}
	if !gotNil.Equal(time.Date(2026, time.July, 19, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("ParseDate nil tz = %v", gotNil)
	}
}

func TestParseDateErrors(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"", "garbage", "31/02/2026", "2026-13-01", "19/07/2026 25:00",
		"19/07/2026 10:60", "19 Jul 2026", "19/07/2026 10:30:11", "19/07",
		"2026/07/19",
	} {
		if got, err := ParseDate(raw, time.UTC); err == nil {
			t.Errorf("ParseDate(%q) = %v, want error", raw, got)
		} else if !domain.IsCode(err, domain.CodeValidation) {
			t.Errorf("ParseDate(%q) code = %s, want validation", raw, domain.CodeOf(err))
		}
	}
}

func TestNormaliseMerchant(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		raw         string
		wantDisplay string
		wantKey     string
	}{
		// The 8 seed merchants from db/migrations/0002.
		{"coopmart", "Co.opmart", "Co.opmart", "coopmart"},
		{"coopmart upper alias", "CO.OPMART", "CO.OPMART", "coopmart"},
		{"coop mart alias", "Co.op Mart", "Co.op Mart", "coop mart"},
		{"circle k", "Circle K", "Circle K", "circle k"},
		{"highlands", "Highlands Coffee", "Highlands Coffee", "highlands coffee"},
		{"highlands upper alias", "HIGHLANDS COFFEE", "HIGHLANDS COFFEE", "highlands coffee"},
		{"grab", "Grab", "Grab", "grab"},
		{"grab upper alias", "GRAB", "GRAB", "grab"},
		{"petrolimex", "Petrolimex", "Petrolimex", "petrolimex"},
		{"guardian", "Guardian", "Guardian", "guardian"},
		{"shopee", "Shopee", "Shopee", "shopee"},
		{"shopee vn alias", "SHOPEE VN", "SHOPEE VN", "shopee vn"},
		{"coffee house", "The Coffee House", "The Coffee House", "the coffee house"},
		// Legal entity prefixes/suffixes.
		{"cty tnhh prefix", "CTY TNHH ABC", "CTY TNHH ABC", "abc"},
		{"cong ty tnhh prefix", "CÔNG TY TNHH CO.OPMART", "CÔNG TY TNHH CO.OPMART", "coopmart"},
		{"cong ty cp prefix", "CÔNG TY CP VINAMILK", "CÔNG TY CP VINAMILK", "vinamilk"},
		{"co ltd suffix", "ABC CO., LTD", "ABC CO., LTD", "abc"},
		{"co ltd no space", "ABC CO.,LTD", "ABC CO.,LTD", "abc"},
		{"jsc suffix", "XYZ JSC", "XYZ JSC", "xyz"},
		{"llc suffix", "ACME LLC", "ACME LLC", "acme"},
		{"both ends", "CÔNG TY TNHH ABC CO., LTD", "CÔNG TY TNHH ABC CO., LTD", "abc"},
		// Diacritics fold to ASCII base letters in the key only.
		{"diacritics key", "Cà Phê Đê", "Cà Phê Đê", "ca phe de"},
		{"đ folds to d", "Đà Nẵng", "Đà Nẵng", "da nang"},
		// Whitespace/punctuation collapse.
		{"whitespace collapse", "  Circle   K  ", "Circle   K", "circle k"},
		{"punctuation collapse", "Lotte-Mart!", "Lotte-Mart!", "lotte mart"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			display, key := NormaliseMerchant(tt.raw)
			if display != tt.wantDisplay || key != tt.wantKey {
				t.Fatalf("NormaliseMerchant(%q) = (%q, %q), want (%q, %q)",
					tt.raw, display, key, tt.wantDisplay, tt.wantKey)
			}
		})
	}
}

func TestTypeHint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text        string
		wantType    domain.TxType
		wantMatched bool
	}{
		{"HOAN TIEN", domain.TxRefund, true},
		{"Hoàn tiền đơn hàng", domain.TxRefund, true},
		{"Refund order 123", domain.TxRefund, true},
		{"trả lại hàng", domain.TxRefund, true},
		{"CHUYEN KHOAN", domain.TxTransfer, true},
		{"chuyển tiền cho mẹ", domain.TxTransfer, true},
		{"bank transfer", domain.TxTransfer, true},
		{"ck momo", domain.TxTransfer, true},
		// Refund wins when both vocabularies appear.
		{"hoàn tiền chuyển khoan", domain.TxRefund, true},
		{"mua cafe sáng", domain.TxExpense, false},
		{"Co.opmart groceries", domain.TxExpense, false},
		{"", domain.TxExpense, false},
	}
	for _, tt := range tests {
		gotType, matched := TypeHint(tt.text)
		if gotType != tt.wantType || matched != tt.wantMatched {
			t.Errorf("TypeHint(%q) = (%s, %v), want (%s, %v)",
				tt.text, gotType, matched, tt.wantType, tt.wantMatched)
		}
	}
}

// FuzzParseAmount: never panics, never accepts a negative minor, and every
// success carries a currency. Run: go test -run FuzzParseAmount -fuzztime 5s.
func FuzzParseAmount(f *testing.F) {
	for _, seed := range []string{
		"150.000", "150,000", "150k", "1.2tr", "325.000 ₫", "$12.34",
		"A$18.90", "-50", "abc", "9999999999999999",
	} {
		f.Add(seed, "VND")
		f.Add(seed, "")
	}
	f.Fuzz(func(t *testing.T, raw, hint string) {
		minor, curr, err := ParseAmount(raw, hint)
		if err != nil {
			return
		}
		if minor < 0 {
			t.Fatalf("ParseAmount(%q, %q) accepted negative minor %d", raw, hint, minor)
		}
		if curr == "" {
			t.Fatalf("ParseAmount(%q, %q) succeeded with empty currency", raw, hint)
		}
	})
}

// FuzzParseDate: never panics and successes are always UTC. Run:
// go test -run FuzzParseDate -fuzztime 5s.
func FuzzParseDate(f *testing.F) {
	for _, seed := range []string{
		"19/07/2026", "19/07/26", "9/7/2026", "2026-07-19", "19/07/2026 10:30",
		"19/07/2026, 10:30", "01/01/70", "31/02/2026", "garbage",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := ParseDate(raw, time.UTC)
		if err != nil {
			return
		}
		if got.Location() != time.UTC {
			t.Fatalf("ParseDate(%q) location = %v, want UTC", raw, got.Location())
		}
		if !got.Equal(got.UTC()) {
			t.Fatalf("ParseDate(%q) = %v not equal to its UTC form", raw, got)
		}
	})
}

func TestFold(t *testing.T) {
	t.Parallel()
	if got, want := Fold("  Tuần  Này  "), "tuan nay"; got != want {
		t.Errorf("Fold whitespace+diacritics = %q, want %q", got, want)
	}
	if got, want := Fold("ĐỔI"), "doi"; got != want {
		t.Errorf("Fold uppercase = %q, want %q", got, want)
	}
}
