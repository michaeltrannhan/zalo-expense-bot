package domain

import (
	"fmt"
	"strings"
)

// Money is stored as integer minor units. For VND the minor unit is 1 đồng;
// for AUD it is 1 cent. Floating-point money is forbidden everywhere.

// CurrencyDecimalPlaces maps the currencies we normalise to their minor-unit
// exponent. Unknown currencies default to 2 and are never auto-confirmed.
func CurrencyDecimalPlaces(currency string) int {
	switch strings.ToUpper(currency) {
	case "VND", "JPY", "KRW":
		return 0
	default:
		return 2
	}
}

// FormatMinor renders an amount for chat: 325000 VND → "325.000 ₫",
// 1234 AUD → "A$12.34". The locale separator conventions follow vi-VN
// (dot thousands, comma decimal) since the pilot audience is Vietnamese.
func FormatMinor(amount int64, currency string) string {
	cur := strings.ToUpper(currency)
	neg := amount < 0
	if neg {
		amount = -amount
	}
	places := CurrencyDecimalPlaces(cur)

	var intPart, fracPart string
	if places == 0 {
		intPart = fmt.Sprintf("%d", amount)
	} else {
		scale := int64(1)
		for range places {
			scale *= 10
		}
		intPart = fmt.Sprintf("%d", amount/scale)
		fracPart = fmt.Sprintf(",%0*d", places, amount%scale)
	}

	// Dot-group the integer part: 1234567 → 1.234.567.
	digits := []byte(intPart)
	var grouped []byte
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped = append(grouped, '.')
		}
		grouped = append(grouped, d)
	}

	sign := ""
	if neg {
		sign = "-"
	}
	symbol := currencySymbol(cur)
	if symbol != "" {
		return fmt.Sprintf("%s%s%s %s", sign, grouped, fracPart, symbol)
	}
	return fmt.Sprintf("%s%s%s %s", sign, grouped, fracPart, cur)
}

func currencySymbol(cur string) string {
	switch cur {
	case "VND":
		return "₫"
	case "AUD":
		return "A$"
	case "USD":
		return "$"
	}
	return ""
}
