//go:build integration

package categorisation

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/store"
)

// Fixed seed IDs from db/migrations/0002_seed_categories.up.sql.
const (
	coopmartID = "22222222-2222-2222-2222-222222222201"
	catAnUong  = "11111111-1111-1111-1111-111111111101"
	catMuaSam  = "11111111-1111-1111-1111-111111111105"
	catKhac    = "11111111-1111-1111-1111-111111111113"
)

func newService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	pool := pgtest.NewPool(t)
	st := store.New(pool)
	return NewService(st, clock.Real{}), st
}

func TestResolveMerchantIntegration(t *testing.T) {
	ctx := context.Background()

	t.Run("blank input", func(t *testing.T) {
		svc, _ := newService(t)
		m, kind, err := svc.ResolveMerchant(ctx, "   ")
		if err != nil || m != nil || kind != MatchNone {
			t.Errorf("got (%v, %q, %v), want (nil, none, nil)", m, kind, err)
		}
		m, kind, err = svc.ResolveMerchant(ctx, "!!!")
		if err != nil || m != nil || kind != MatchNone {
			t.Errorf("all-punctuation: got (%v, %q, %v), want (nil, none, nil)", m, kind, err)
		}
	})

	t.Run("seed alias hit", func(t *testing.T) {
		svc, _ := newService(t)
		m, kind, err := svc.ResolveMerchant(ctx, "CO.OPMART")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if kind != MatchAlias {
			t.Errorf("kind = %q, want alias", kind)
		}
		if m.CanonicalName != "Co.opmart" || m.ID.String() != coopmartID {
			t.Errorf("merchant = %+v, want seeded Co.opmart", m)
		}
	})

	t.Run("canonical hit", func(t *testing.T) {
		svc, _ := newService(t)
		// "Circle K" has a seeded merchant but no seeded alias, so the
		// alias step misses and the canonical name decides.
		m, kind, err := svc.ResolveMerchant(ctx, "circle  k")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if kind != MatchCanonical || m.CanonicalName != "Circle K" {
			t.Errorf("got (%q, %q), want (Circle K, canonical)", m.CanonicalName, kind)
		}
	})

	t.Run("fuzzy hit", func(t *testing.T) {
		svc, _ := newService(t)
		// "highland" is a substring of the seeded "highlands coffee" but
		// matches no alias or canonical name exactly.
		m, kind, err := svc.ResolveMerchant(ctx, "Highland")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if kind != MatchFuzzy || m.CanonicalName != "Highlands Coffee" {
			t.Errorf("got (%q, %q), want (Highlands Coffee, fuzzy)", m.CanonicalName, kind)
		}
	})

	t.Run("create then alias", func(t *testing.T) {
		svc, _ := newService(t)
		// Merchants survive pgtest truncation, so the name must be unique
		// per run to still exercise the create path.
		raw := "Tạp Hóa " + strings.ReplaceAll(uuid.NewString()[:8], "-", "a")
		m, kind, err := svc.ResolveMerchant(ctx, raw)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if kind != MatchCreated || m.CanonicalName != raw {
			t.Errorf("got (%q, %q), want (%q, created)", m.CanonicalName, kind, raw)
		}
		again, kind, err := svc.ResolveMerchant(ctx, raw)
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		if kind != MatchAlias || again.ID != m.ID {
			t.Errorf("second call got (%v, %q), want (same merchant, alias)", again.ID, kind)
		}
	})
}

func TestSuggestCategoryIntegration(t *testing.T) {
	ctx := context.Background()

	t.Run("extraction fallback", func(t *testing.T) {
		svc, _ := newService(t)
		sug, err := svc.SuggestCategory(ctx, uuid.New(), nil, "an-uong", 0.8)
		if err != nil {
			t.Fatalf("suggest: %v", err)
		}
		if sug.Source != "extraction" || sug.CategoryKey != "an-uong" ||
			sug.Confidence != 0.8 || sug.CategoryID == nil ||
			*sug.CategoryID != uuid.MustParse(catAnUong) {
			t.Errorf("suggestion = %+v, want extraction/an-uong/0.8", sug)
		}
	})

	t.Run("khac default", func(t *testing.T) {
		svc, _ := newService(t)
		sug, err := svc.SuggestCategory(ctx, uuid.New(), nil, "", 0.8)
		if err != nil {
			t.Fatalf("suggest: %v", err)
		}
		if sug.Source != "default" || sug.CategoryKey != "khac" ||
			sug.Confidence != 0.3 || *sug.CategoryID != uuid.MustParse(catKhac) {
			t.Errorf("suggestion = %+v, want default/khac/0.3", sug)
		}
		// An unknown extraction key also lands on "khac".
		sug, err = svc.SuggestCategory(ctx, uuid.New(), nil, "khong-ton-tai", 0.8)
		if err != nil {
			t.Fatalf("suggest: %v", err)
		}
		if sug.Source != "default" || sug.CategoryKey != "khac" {
			t.Errorf("unknown key: suggestion = %+v, want default/khac", sug)
		}
	})

	t.Run("learning loop", func(t *testing.T) {
		svc, st := newService(t)
		userID := uuid.New()
		if err := st.CreateUser(ctx, userID); err != nil {
			t.Fatalf("create user: %v", err)
		}
		m, _, err := svc.ResolveMerchant(ctx, "CO.OPMART")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}

		tx := &domain.Transaction{
			Type:       domain.TxExpense,
			MerchantID: &m.ID,
			CategoryID: ptrUUID(catAnUong),
		}
		// Two confirmations: 0.6 then 0.7, crossing the apply floor.
		for i := range 2 {
			if err := svc.LearnFromConfirmation(ctx, userID, tx); err != nil {
				t.Fatalf("learn %d: %v", i, err)
			}
		}
		rule, err := st.GetUserMerchantRule(ctx, userID, m.ID)
		if err != nil {
			t.Fatalf("load rule: %v", err)
		}
		if rule.SampleCount != 2 || rule.Confidence != 0.7 {
			t.Errorf("rule = count %d conf %v, want 2 / 0.7", rule.SampleCount, rule.Confidence)
		}

		// The learned rule overrides a conflicting extraction key.
		sug, err := svc.SuggestCategory(ctx, userID, m, "mua-sam", 0.9)
		if err != nil {
			t.Fatalf("suggest: %v", err)
		}
		if sug.Source != "user_rule" || *sug.CategoryID != uuid.MustParse(catAnUong) ||
			sug.Confidence != 0.7 || sug.CategoryKey != "an-uong" {
			t.Errorf("suggestion = %+v, want user_rule/an-uong/0.7", sug)
		}
		want := "Bạn thường xếp Co.opmart vào Ăn uống"
		if sug.Reason != want {
			t.Errorf("reason = %q, want %q", sug.Reason, want)
		}

		// Type suggestion follows the rule too.
		txType, conf, src := svc.SuggestType(ctx, userID, m, domain.TxExpense, false)
		if txType != domain.TxExpense || conf != 0.7 || src != "user_rule" {
			t.Errorf("type = (%q, %v, %q), want (expense, 0.7, user_rule)", txType, conf, src)
		}
	})

	t.Run("single confirmation stays below floor", func(t *testing.T) {
		svc, st := newService(t)
		userID := uuid.New()
		if err := st.CreateUser(ctx, userID); err != nil {
			t.Fatalf("create user: %v", err)
		}
		m, _, err := svc.ResolveMerchant(ctx, "GRAB")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		tx := &domain.Transaction{
			Type:       domain.TxExpense,
			MerchantID: &m.ID,
			CategoryID: ptrUUID(catAnUong),
		}
		if err := svc.LearnFromConfirmation(ctx, userID, tx); err != nil {
			t.Fatalf("learn: %v", err)
		}
		// 0.6 < 0.7: extraction hint still wins.
		sug, err := svc.SuggestCategory(ctx, userID, m, "di-lai", 0.9)
		if err != nil {
			t.Fatalf("suggest: %v", err)
		}
		if sug.Source != "extraction" || sug.CategoryKey != "di-lai" {
			t.Errorf("suggestion = %+v, want extraction/di-lai", sug)
		}
	})

	t.Run("nil merchant learns nothing", func(t *testing.T) {
		svc, st := newService(t)
		userID := uuid.New()
		if err := st.CreateUser(ctx, userID); err != nil {
			t.Fatalf("create user: %v", err)
		}
		tx := &domain.Transaction{Type: domain.TxExpense, CategoryID: ptrUUID(catAnUong)}
		if err := svc.LearnFromConfirmation(ctx, userID, tx); err != nil {
			t.Fatalf("learn: %v", err)
		}
		m, _, err := svc.ResolveMerchant(ctx, "GRAB")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if _, err := st.GetUserMerchantRule(ctx, userID, m.ID); !domain.IsCode(err, domain.CodeNotFound) {
			t.Errorf("rule err = %v, want CodeNotFound", err)
		}
	})
}

func TestSuggestTypeIntegration(t *testing.T) {
	svc, st := newService(t)
	ctx := context.Background()
	userID := uuid.New()
	if err := st.CreateUser(ctx, userID); err != nil {
		t.Fatalf("create user: %v", err)
	}
	m, _, err := svc.ResolveMerchant(ctx, "SHOPEE")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// A matched hint is carried through as-is; downgrading is the caller's
	// never-silent-confirm policy, not this function's.
	txType, conf, src := svc.SuggestType(ctx, userID, m, domain.TxRefund, true)
	if txType != domain.TxRefund || conf != 0.9 || src != "extraction_hint" {
		t.Errorf("hint: got (%q, %v, %q), want (refund, 0.9, extraction_hint)", txType, conf, src)
	}

	// No hint, no rule: expense default.
	txType, conf, src = svc.SuggestType(ctx, userID, m, domain.TxExpense, false)
	if txType != domain.TxExpense || conf != 0.5 || src != "default" {
		t.Errorf("default: got (%q, %v, %q), want (expense, 0.5, default)", txType, conf, src)
	}

	// Learn a refund preference, then it applies without a hint.
	tx := &domain.Transaction{Type: domain.TxRefund, MerchantID: &m.ID, CategoryID: ptrUUID(catMuaSam)}
	for i := 0; i < 2; i++ {
		if err := svc.LearnFromConfirmation(ctx, userID, tx); err != nil {
			t.Fatalf("learn %d: %v", i, err)
		}
	}
	txType, conf, src = svc.SuggestType(ctx, userID, m, domain.TxExpense, false)
	if txType != domain.TxRefund || conf != 0.7 || src != "user_rule" {
		t.Errorf("rule: got (%q, %v, %q), want (refund, 0.7, user_rule)", txType, conf, src)
	}
}

func ptrUUID(s string) *uuid.UUID {
	id := uuid.MustParse(s)
	return &id
}
