// Package normalise turns raw receipt/chat strings into typed values:
// minor-unit amounts, UTC timestamps, merchant keys and transaction type
// hints. Everything here is pure and deterministic: same input, same output.
package normalise

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"zl-expese-bot/internal/domain"
)

// ---------------------------------------------------------------------------
// Amounts
// ---------------------------------------------------------------------------

// multipliers recognised after the digits: 150k → 150.000, 1.2tr → 1.200.000.
var multipliers = []struct {
	suffix string
	factor int64
}{
	{"triệu", 1_000_000},
	{"tr", 1_000_000},
	{"k", 1_000},
}

// currencySymbols are scanned in order; first match wins. "A$" precedes "$".
var currencySymbols = []struct {
	token    string
	currency string
}{
	{"A$", "AUD"},
	{"₫", "VND"},
	{"$", "USD"},
}

// ParseAmount converts a raw amount string into minor units and an ISO
// currency code. currencyHint is used only when the string carries no symbol;
// an empty hint defaults to VND.
//
// Separator rule (kept deliberately small):
//   - 0-decimal currencies (VND): "." and "," are ALWAYS thousands
//     separators; a trailing k/tr/triệu multiplier makes the LAST separator a
//     decimal point iff fewer than 3 digits follow it (1.2tr → 1.200.000).
//   - 2-decimal currencies (AUD/USD): the LAST separator is the decimal point
//     iff it is "." and exactly 2 digits follow it; every other separator is
//     a thousands separator (1,234.56 → 123456 minor).
//
// Negative amounts and values with more than 15 significant digits are
// rejected (15 digits stays well inside int64 after ×100 scaling).
func ParseAmount(raw string, currencyHint string) (minor int64, currency string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, "", domain.E(domain.CodeValidation, "empty amount", nil)
	}
	if strings.ContainsRune(s, '-') {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "negative amount %q", raw)
	}

	currency, s = detectCurrency(s, currencyHint)

	factor := int64(1)
	lower := strings.ToLower(s)
	for _, m := range multipliers {
		if strings.HasSuffix(lower, m.suffix) {
			factor = m.factor
			s = strings.TrimSpace(s[:len(s)-len(m.suffix)])
			break
		}
	}

	// Keep only digits and separators; anything else is garbage.
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '.', r == ',':
			b.WriteRune(r)
		case r == ' ':
		default:
			return 0, "", domain.Ef(domain.CodeValidation, nil, "invalid character %q in amount %q", r, raw)
		}
	}
	s = b.String()
	if s == "" {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "no digits in amount %q", raw)
	}

	if domain.CurrencyDecimalPlaces(currency) == 0 && factor == 1 {
		// VND without multiplier: every separator is a thousands separator.
		return finishAmount(stripSeparators(s), currency, factor, raw)
	}

	// Locate the last separator and decide whether it is a decimal point.
	last := strings.LastIndexAny(s, ".,")
	decimalAt := -1
	if last >= 0 {
		frac := stripSeparators(s[last+1:])
		if domain.CurrencyDecimalPlaces(currency) == 0 {
			// Multiplier path: decimal iff not exactly 3 trailing digits.
			if len(frac) > 0 && len(frac) != 3 {
				decimalAt = last
			}
		} else if s[last] == '.' && len(frac) == 2 {
			decimalAt = last
		}
	}

	if decimalAt < 0 {
		return finishAmount(stripSeparators(s), currency, factor, raw)
	}
	intDigits := stripSeparators(s[:decimalAt])
	fracDigits := stripSeparators(s[decimalAt+1:])
	if intDigits == "" {
		intDigits = "0"
	}
	if len(fracDigits) > 4 {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "too many fraction digits in %q", raw)
	}
	if len(intDigits)+len(fracDigits) > 15 {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
	}
	combined, err := strconv.ParseInt(intDigits+fracDigits, 10, 64)
	if err != nil {
		return 0, "", domain.Ef(domain.CodeValidation, err, "amount %q too large", raw)
	}
	n := len(fracDigits)

	if domain.CurrencyDecimalPlaces(currency) == 0 {
		// Multiplier path (k/tr/triệu): minor = combined * factor / 10^n,
		// exact only when the scaled value divides evenly (1.2tr → 1.200.000).
		scale := int64(1)
		for range n {
			if scale > math.MaxInt64/10 {
				return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
			}
			scale *= 10
		}
		if factor > 1 && combined > math.MaxInt64/factor {
			return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
		}
		value := combined * factor
		if value%scale != 0 {
			return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q is not a whole minor unit", raw)
		}
		return value / scale, currency, nil
	}

	// 2-decimal currencies: minor = combined * 10^(places-n) * factor;
	// more fraction digits than places is not representable in minor units.
	places := domain.CurrencyDecimalPlaces(currency)
	if n > places {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q is not a whole minor unit", raw)
	}
	minor = combined
	for i := n; i < places; i++ {
		if minor > math.MaxInt64/10 {
			return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
		}
		minor *= 10
	}
	if factor > 1 && minor > math.MaxInt64/factor {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
	}
	return minor * factor, currency, nil
}

// finishAmount parses a pure digit string as minor units, applying factor and
// (for 2-decimal currencies) the implied ×100 when no decimal point was seen.
func finishAmount(digits string, currency string, factor int64, raw string) (int64, string, error) {
	if digits == "" {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "no digits in amount %q", raw)
	}
	if len(digits) > 15 {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, "", domain.Ef(domain.CodeValidation, err, "amount %q too large", raw)
	}
	if v < 0 {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
	}
	if domain.CurrencyDecimalPlaces(currency) == 2 {
		if v > math.MaxInt64/100 {
			return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
		}
		v *= 100
	}
	if factor > 1 && v > math.MaxInt64/factor {
		return 0, "", domain.Ef(domain.CodeValidation, nil, "amount %q too large", raw)
	}
	return v * factor, currency, nil
}

func stripSeparators(s string) string {
	return strings.NewReplacer(".", "", ",", "").Replace(s)
}

// detectCurrency extracts a currency symbol/word from s, falling back to the
// hint and finally to VND.
func detectCurrency(s, hint string) (currency, rest string) {
	upper := strings.ToUpper(s)
	switch {
	case strings.Contains(upper, "VNĐ"), strings.Contains(upper, "VND"),
		strings.Contains(s, "đ"), strings.Contains(s, "Đ"), strings.Contains(s, "₫"):
		currency = "VND"
	case strings.Contains(upper, "AUD"):
		currency = "AUD"
	default:
		for _, sym := range currencySymbols {
			if strings.Contains(s, sym.token) {
				currency = sym.currency
				break
			}
		}
	}
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(hint))
	}
	if currency == "" {
		currency = "VND"
	}

	// Remove currency tokens so only digits/separators/multiplier remain.
	rest = s
	for _, tok := range []string{"VNĐ", "VND", "vnđ", "vnd", "AUD", "aud", "A$", "a$", "$", "₫", "đ", "Đ"} {
		rest = strings.ReplaceAll(rest, tok, "")
	}
	return currency, strings.TrimSpace(rest)
}

// ---------------------------------------------------------------------------
// Dates
// ---------------------------------------------------------------------------

var (
	isoDateRe = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
	dmyDateRe = regexp.MustCompile(`^(\d{1,2})[/-](\d{1,2})[/-](\d{2}|\d{4})$`)
	timeRe    = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
)

// ParseDate parses receipt dates in the forms dd/mm/yyyy, dd/mm/yy, d/m/yyyy,
// dd-mm-yyyy and yyyy-mm-dd, with an optional " hh:mm" or ", hh:mm" suffix.
// Two-digit years pivot at 69/70: 00–69 → 2000s, 70–99 → 1900s. The result is
// interpreted in tz (UTC when nil) and returned in UTC.
func ParseDate(raw string, tz *time.Location) (time.Time, error) {
	if tz == nil {
		tz = time.UTC
	}
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, ", ", " ") // "19/07/2026, 10:30" → space-separated
	parts := strings.Fields(s)
	if len(parts) == 0 || len(parts) > 2 {
		return time.Time{}, domain.Ef(domain.CodeValidation, nil, "unrecognised date %q", raw)
	}

	var year, month, day int
	if m := isoDateRe.FindStringSubmatch(parts[0]); m != nil {
		year, _ = strconv.Atoi(m[1])
		month, _ = strconv.Atoi(m[2])
		day, _ = strconv.Atoi(m[3])
	} else if m := dmyDateRe.FindStringSubmatch(parts[0]); m != nil {
		day, _ = strconv.Atoi(m[1])
		month, _ = strconv.Atoi(m[2])
		year, _ = strconv.Atoi(m[3])
		if year < 100 {
			if year <= 69 {
				year += 2000
			} else {
				year += 1900
			}
		}
	} else {
		return time.Time{}, domain.Ef(domain.CodeValidation, nil, "unrecognised date %q", raw)
	}

	hour, min := 0, 0
	if len(parts) == 2 {
		m := timeRe.FindStringSubmatch(parts[1])
		if m == nil {
			return time.Time{}, domain.Ef(domain.CodeValidation, nil, "unrecognised time in %q", raw)
		}
		hour, _ = strconv.Atoi(m[1])
		min, _ = strconv.Atoi(m[2])
		if hour > 23 || min > 59 {
			return time.Time{}, domain.Ef(domain.CodeValidation, nil, "invalid time in %q", raw)
		}
	}

	t := time.Date(year, time.Month(month), day, hour, min, 0, 0, tz)
	// time.Date normalises overflow (32/01 → 01/02); reject silently-wrapped dates.
	if t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, domain.Ef(domain.CodeValidation, nil, "invalid calendar date %q", raw)
	}
	return t.UTC(), nil
}

// ---------------------------------------------------------------------------
// Merchants
// ---------------------------------------------------------------------------

// foldDiacritics maps Vietnamese letters to their ASCII base so keys compare
// regardless of diacritics. Applied after lowercasing.
var foldDiacritics = strings.NewReplacer(
	"à", "a", "á", "a", "ạ", "a", "ả", "a", "ã", "a",
	"â", "a", "ầ", "a", "ấ", "a", "ậ", "a", "ẩ", "a", "ẫ", "a",
	"ă", "a", "ằ", "a", "ắ", "a", "ặ", "a", "ẳ", "a", "ẵ", "a",
	"è", "e", "é", "e", "ẹ", "e", "ẻ", "e", "ẽ", "e",
	"ê", "e", "ề", "e", "ế", "e", "ệ", "e", "ể", "e", "ễ", "e",
	"ì", "i", "í", "i", "ị", "i", "ỉ", "i", "ĩ", "i",
	"ò", "o", "ó", "o", "ọ", "o", "ỏ", "o", "õ", "o",
	"ô", "o", "ồ", "o", "ố", "o", "ộ", "o", "ổ", "o", "ỗ", "o",
	"ơ", "o", "ờ", "o", "ớ", "o", "ợ", "o", "ở", "o", "ỡ", "o",
	"ù", "u", "ú", "u", "ụ", "u", "ủ", "u", "ũ", "u",
	"ư", "u", "ừ", "u", "ứ", "u", "ự", "u", "ử", "u", "ữ", "u",
	"ỳ", "y", "ý", "y", "ỵ", "y", "ỷ", "y", "ỹ", "y",
	"đ", "d",
)

// legalForms are stripped from the folded key as whole leading/trailing
// tokens (punctuation has already been collapsed to spaces at that point, so
// "CO., LTD" arrives as "co ltd").
var legalForms = []string{
	"cty tnhh", "cong ty tnhh", "cong ty cp", "co ltd", "jsc", "llc",
}

// NormaliseMerchant returns the trimmed original-casing display form plus a
// matching key: lowercased, diacritics-folded, punctuation collapsed to
// single spaces, with legal entity prefixes/suffixes removed.
func NormaliseMerchant(raw string) (display string, key string) {
	display = strings.TrimSpace(raw)

	key = foldDiacritics.Replace(strings.ToLower(display))
	// Dots are dropped (Co.opmart → coopmart); every other non-alphanumeric
	// character becomes a space, then whitespace collapses.
	key = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		if r == '.' {
			return -1
		}
		return ' '
	}, key)
	key = strings.Join(strings.Fields(key), " ")

	// Strip legal forms repeatedly ("CÔNG TY TNHH ABC CO., LTD").
	for stripped := true; stripped && key != ""; {
		stripped = false
		for _, form := range legalForms {
			if rest, ok := strings.CutPrefix(key, form+" "); ok {
				key, stripped = rest, true
			}
			if rest, ok := strings.CutSuffix(key, " "+form); ok {
				key, stripped = rest, true
			}
			if key == form {
				key, stripped = "", true
			}
		}
	}
	return display, key
}

// ---------------------------------------------------------------------------
// Type hints
// ---------------------------------------------------------------------------

var (
	refundHints   = []string{"hoan tien", "hoan", "refund", "tra lai"}
	transferHints = []string{"chuyen khoan", "chuyen tien", "transfer", "ck"}
)

// TypeHint scans free text (folded, case-insensitive) for refund/transfer
// vocabulary. Refund is checked first: a refund note that mentions a bank
// transfer is still a refund. The bool reports whether any hint matched;
// (TxExpense, false) is the neutral default.
func TypeHint(text string) (domain.TxType, bool) {
	// Word-boundary matching: fold punctuation to spaces, pad, and match
	// " hint " so "chuyen khoan" never trips the refund hint "hoan".
	lower := foldDiacritics.Replace(strings.ToLower(text))
	words := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return ' '
	}, lower)
	folded := " " + strings.Join(strings.Fields(words), " ") + " "
	for _, h := range refundHints {
		if strings.Contains(folded, " "+h+" ") {
			return domain.TxRefund, true
		}
	}
	for _, h := range transferHints {
		if strings.Contains(folded, " "+h+" ") {
			return domain.TxTransfer, true
		}
	}
	return domain.TxExpense, false
}
