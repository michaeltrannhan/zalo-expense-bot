package main

import (
	"bytes"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zl-expese-bot/contracts/events"
)

func syntheticExtractionResult() events.ExtractionResult {
	return events.ExtractionResult{Fields: map[string]events.FieldValue{
		"merchant":    {Normalised: syntheticMerchant},
		"total_minor": {Normalised: syntheticTotalMinor},
		"currency":    {Normalised: syntheticCurrency},
		"occurred_at": {Normalised: syntheticOccurredAt},
	}}
}

func TestValidateSyntheticResultRequiresExactKnownValues(t *testing.T) {
	if err := validateSyntheticResult(syntheticExtractionResult()); err != nil {
		t.Fatalf("known fixture rejected: %v", err)
	}

	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "merchant", field: "merchant", value: "some other cafe"},
		{name: "total", field: "total_minor", value: int64(123457)},
		{name: "currency", field: "currency", value: "USD"},
		{name: "date", field: "occurred_at", value: "2026-08-13T00:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := syntheticExtractionResult()
			result.Fields[tt.field] = events.FieldValue{Normalised: tt.value}
			if err := validateSyntheticResult(result); err == nil {
				t.Fatalf("mismatched %s was accepted", tt.field)
			}
		})
	}
}

func TestValidateE2EDatabaseURLUsesEffectivePGXTarget(t *testing.T) {
	for _, valid := range []string{
		"postgres://postgres:secret@localhost:5432/zl_expense_e2e?sslmode=disable",
		"postgresql://postgres:secret@127.0.0.1:5432/zl_expense_e2e?sslmode=disable",
		"postgres://postgres:secret@[::1]:5432/zl_expense_e2e?sslmode=disable",
	} {
		if err := validateE2EDatabaseURL(valid); err != nil {
			t.Errorf("valid local E2E URL rejected: %v", err)
		}
	}

	tests := []struct {
		name string
		url  string
	}{
		{name: "empty", url: ""},
		{name: "different database", url: "postgres://localhost/zl_expense"},
		{name: "remote host", url: "postgres://db.example.com/zl_expense_e2e"},
		{name: "database query override", url: "postgres://localhost/zl_expense_e2e?database=zl_expense"},
		{name: "host query override", url: "postgres://localhost/zl_expense_e2e?host=db.example.com"},
		{name: "remote fallback host", url: "postgres://localhost,db.example.com/zl_expense_e2e?sslmode=disable"},
		{name: "service file override", url: "postgres://localhost/zl_expense_e2e?servicefile=/tmp/override.conf"},
		{name: "password file input", url: "postgres://localhost/zl_expense_e2e?passfile=/tmp/passwords"},
		{name: "malformed with secret", url: "postgres://user:super-secret@[bad/zl_expense_e2e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateE2EDatabaseURL(tt.url)
			if err == nil {
				t.Fatal("unsafe E2E database URL was accepted")
			}
			if strings.Contains(err.Error(), "super-secret") {
				t.Fatal("parse error exposed database credentials")
			}
		})
	}
}

func TestSyntheticReceiptPNG(t *testing.T) {
	body, err := syntheticReceiptPNG()
	if err != nil {
		t.Fatalf("syntheticReceiptPNG: %v", err)
	}
	image, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode PNG: %v", err)
	}
	if got := image.Bounds().Size(); got.X != 900 || got.Y != 600 {
		t.Fatalf("size = %v", got)
	}
	nonWhite := 0
	for y := image.Bounds().Min.Y; y < image.Bounds().Max.Y; y += 5 {
		for x := image.Bounds().Min.X; x < image.Bounds().Max.X; x += 5 {
			r, g, b, a := image.At(x, y).RGBA()
			wr, wg, wb, wa := color.White.RGBA()
			if r != wr || g != wg || b != wb || a != wa {
				nonWhite++
			}
		}
	}
	if nonWhite < 500 {
		t.Fatalf("fixture has too little rendered ink: %d samples", nonWhite)
	}
}

func TestRunFixtureWritesPrivatePNGAndRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipt.png")
	if err := runFixture([]string{"-out", path}); err != nil {
		t.Fatalf("runFixture: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(body)); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if err := runFixture([]string{"-out", path}); err == nil {
		t.Fatal("fixture overwrote an existing file")
	}
}

func TestProviderIDPattern(t *testing.T) {
	for _, valid := range []string{"6ede9afa66b88fe6d6a9", "123456789", "chat.id-1"} {
		if !providerIDPattern.MatchString(valid) {
			t.Errorf("valid ID rejected: %q", valid)
		}
	}
	for _, invalid := range []string{"", "space id", "line\nbreak"} {
		if providerIDPattern.MatchString(invalid) {
			t.Errorf("invalid ID accepted: %q", invalid)
		}
	}
}

func TestIdentityFileRoundTripIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	want := discoveredIdentity{ProviderUserID: "user-123", ProviderChatID: "chat-456"}
	if err := writeIdentity(path, want); err != nil {
		t.Fatalf("writeIdentity: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	got, err := readIdentity(path)
	if err != nil {
		t.Fatalf("readIdentity: %v", err)
	}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}
