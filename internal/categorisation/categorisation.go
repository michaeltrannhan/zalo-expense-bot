// Package categorisation resolves merchants and suggests categories/types
// for draft transactions (plan P4-A01/P4-A02). Rules are count-based and
// deterministic — no ML model in the MVP. Resolution order for merchants:
// exact alias → canonical normalised match → fuzzy candidate → create new.
// Category suggestion order: user merchant rule → extraction hint → "khac".
package categorisation

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/extraction/normalise"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/store"
)

// MatchKind records how ResolveMerchant resolved a raw name, for
// prediction/evidence records.
type MatchKind string

const (
	MatchAlias     MatchKind = "alias"     // exact normalised alias hit
	MatchCanonical MatchKind = "canonical" // exact normalised merchant-name hit
	MatchFuzzy     MatchKind = "fuzzy"     // substring candidate
	MatchCreated   MatchKind = "created"   // no match; new merchant learned
	MatchNone      MatchKind = "none"      // empty raw name
)

// Suggestion is a category/type proposal for one draft transaction.
type Suggestion struct {
	CategoryID  *uuid.UUID // nil only when even "khac" is missing (broken seed)
	CategoryKey string
	DisplayName string
	Confidence  float64
	Source      string // user_rule|extraction|default — persisted on predictions
	Reason      string // Vietnamese explanation, "" when not user-facing
}

// Confidence floor for applying a learned user rule (plan P4-A02: cautious
// after one correction, confident after repeats).
const UserRuleApplyConfidence = 0.7

// ruleConfidence maps a sample count to rule confidence:
// n=1 → 0.6, then +0.1 per additional sample, capped at 0.95.
func ruleConfidence(n int) float64 {
	c := 0.6 + 0.1*float64(n-1)
	if c > 0.95 {
		c = 0.95
	}
	if c < 0 {
		c = 0
	}
	return c
}

// Service resolves merchants and suggests categories.
type Service struct {
	st  *store.Store
	clk clock.Clock
}

// NewService builds a Service. clk timestamps learning writes.
func NewService(st *store.Store, clk clock.Clock) *Service {
	return &Service{st: st, clk: clk}
}

// ResolveMerchant applies the plan P4-A01 order to rawName (an extraction
// merchant string or manual-entry text): alias → canonical → fuzzy →
// create. A created merchant is learned with the extraction source and its
// raw spelling registered as an alias. MatchNone returns (nil, MatchNone,
// nil) for blank input.
func (s *Service) ResolveMerchant(ctx context.Context, rawName string) (*domain.Merchant, MatchKind, error) {
	display, key := normalise.NormaliseMerchant(rawName)
	// An empty key covers both blank input and all-punctuation names: there
	// is nothing stable to match or learn from.
	if key == "" {
		return nil, MatchNone, nil
	}

	m, err := s.st.FindMerchantByAlias(ctx, key)
	if err == nil {
		return m, MatchAlias, nil
	}
	if !domain.IsCode(err, domain.CodeNotFound) {
		return nil, "", err
	}

	m, err = s.st.FindMerchantByNormalisedName(ctx, key)
	if err == nil {
		return m, MatchCanonical, nil
	}
	if !domain.IsCode(err, domain.CodeNotFound) {
		return nil, "", err
	}

	m, err = s.st.FindMerchantFuzzy(ctx, key)
	if err == nil {
		return m, MatchFuzzy, nil
	}
	if !domain.IsCode(err, domain.CodeNotFound) {
		return nil, "", err
	}

	// Unknown merchant: learn it, with the raw spelling as an extraction
	// alias so the next sighting resolves as MatchAlias.
	m, err = s.st.CreateMerchant(ctx, display, key, "")
	if err != nil {
		return nil, "", err
	}
	if err := s.st.AddMerchantAlias(ctx, m.ID, display, key, "extraction"); err != nil {
		return nil, "", err
	}
	return m, MatchCreated, nil
}

// SuggestCategory proposes the category for a (user, merchant) pair:
//
//  1. The user's learned rule when its confidence ≥ UserRuleApplyConfidence
//     (Source "user_rule", Reason "Bạn thường xếp <merchant> vào <category>").
//  2. The extractor's category key when it resolves to a seeded category
//     (Source "extraction", Confidence = extractionConf).
//  3. The "khac" catch-all (Source "default", Confidence 0.3).
//
// m may be nil (unresolved merchant) — rule lookup is then skipped.
func (s *Service) SuggestCategory(ctx context.Context, userID uuid.UUID, m *domain.Merchant, extractionKey string, extractionConf float64) (Suggestion, error) {
	if m != nil {
		rule, err := s.st.GetUserMerchantRule(ctx, userID, m.ID)
		switch {
		case err == nil:
			if rule.Confidence >= UserRuleApplyConfidence && rule.PreferredCategoryID != nil {
				cat, err := s.findCategory(ctx, *rule.PreferredCategoryID)
				if err != nil {
					return Suggestion{}, err
				}
				if cat != nil {
					return Suggestion{
						CategoryID:  &cat.ID,
						CategoryKey: cat.SystemKey,
						DisplayName: cat.DisplayName,
						Confidence:  rule.Confidence,
						Source:      "user_rule",
						Reason:      fmt.Sprintf("Bạn thường xếp %s vào %s", m.CanonicalName, cat.DisplayName),
					}, nil
				}
				// Rule references a category that no longer exists; fall
				// through to the extraction hint rather than fail the draft.
			}
		case domain.IsCode(err, domain.CodeNotFound):
			// Never confirmed this merchant — normal.
		default:
			return Suggestion{}, err
		}
	}

	if extractionKey != "" {
		cat, err := s.st.GetCategoryByKey(ctx, extractionKey)
		if err == nil {
			return Suggestion{
				CategoryID:  &cat.ID,
				CategoryKey: cat.SystemKey,
				DisplayName: cat.DisplayName,
				Confidence:  extractionConf,
				Source:      "extraction",
			}, nil
		}
		if !domain.IsCode(err, domain.CodeNotFound) {
			return Suggestion{}, err
		}
	}

	cat, err := s.st.GetCategoryByKey(ctx, "khac")
	if err != nil {
		// A missing "khac" seed is a deployment bug, not a draft error:
		// return the zero suggestion so the flow can still ask the user.
		if domain.IsCode(err, domain.CodeNotFound) {
			return Suggestion{}, nil
		}
		return Suggestion{}, err
	}
	return Suggestion{
		CategoryID:  &cat.ID,
		CategoryKey: cat.SystemKey,
		DisplayName: cat.DisplayName,
		Confidence:  0.3,
		Source:      "default",
	}, nil
}

// findCategory resolves a category by ID via the full taxonomy; the store
// has no by-ID lookup. Returns (nil, nil) when the ID is unknown.
func (s *Service) findCategory(ctx context.Context, id uuid.UUID) (*domain.Category, error) {
	cats, err := s.st.ListCategories(ctx)
	if err != nil {
		return nil, err
	}
	for i := range cats {
		if cats[i].ID == id {
			return &cats[i], nil
		}
	}
	return nil, nil
}

// SuggestType proposes the transaction type: refund/transfer hints from the
// extractor are carried through but NEVER silently confirmed (the caller
// marks them for explicit confirmation); the user rule's preferred type
// applies only for expense/income when the rule is strong enough.
func (s *Service) SuggestType(ctx context.Context, userID uuid.UUID, m *domain.Merchant, hint domain.TxType, hintMatched bool) (domain.TxType, float64, string) {
	if hintMatched {
		return hint, 0.9, "extraction_hint"
	}
	if m != nil {
		// No error return on this signature: a failed rule read degrades
		// to the default instead of blocking the draft.
		if rule, err := s.st.GetUserMerchantRule(ctx, userID, m.ID); err == nil &&
			rule.Confidence >= UserRuleApplyConfidence && rule.PreferredTransactionType != "" {
			return rule.PreferredTransactionType, rule.Confidence, "user_rule"
		}
	}
	return domain.TxExpense, 0.5, "default"
}

// LearnFromConfirmation updates the user's merchant rule after a confirmed
// or corrected transaction: sample_count grows by one, confidence follows
// ruleConfidence, and the preferred category/type become the transaction's
// final values. Transactions without a merchant teach nothing (no-op).
func (s *Service) LearnFromConfirmation(ctx context.Context, userID uuid.UUID, tx *domain.Transaction) error {
	if tx.MerchantID == nil {
		return nil
	}
	count := 0
	rule, err := s.st.GetUserMerchantRule(ctx, userID, *tx.MerchantID)
	if err == nil {
		count = rule.SampleCount
	} else if !domain.IsCode(err, domain.CodeNotFound) {
		return err
	}
	// Upsert adds SampleCount as a delta, so confidence must be computed
	// from the count AFTER this confirmation lands.
	return s.st.UpsertUserMerchantRule(ctx, &domain.UserMerchantRule{
		UserID:                   userID,
		MerchantID:               *tx.MerchantID,
		PreferredCategoryID:      tx.CategoryID,
		PreferredTransactionType: tx.Type,
		SampleCount:              1,
		Confidence:               ruleConfidence(count + 1),
		LastConfirmedAt:          s.clk.Now(),
	})
}
