package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zl-expese-bot/internal/domain"
)

// ---------------------------------------------------------------------------
// Merchants and aliases
// ---------------------------------------------------------------------------

const merchantColumns = `id, canonical_name, normalised_name, merchant_family, created_at`

func scanMerchant(row pgx.Row) (*domain.Merchant, error) {
	var m domain.Merchant
	err := row.Scan(&m.ID, &m.CanonicalName, &m.NormalisedName, &m.MerchantFamily, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// FindMerchantByNormalisedName matches on the canonical normalised name.
func (s *Store) FindMerchantByNormalisedName(ctx context.Context, key string) (*domain.Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`SELECT `+merchantColumns+` FROM merchants WHERE normalised_name = $1`, key))
	if isNoRows(err) {
		return nil, notFound("FindMerchantByNormalisedName", "merchant")
	}
	if err != nil {
		return nil, internalErr("FindMerchantByNormalisedName", err)
	}
	return m, nil
}

// FindMerchantByAlias matches on any known alias spelling.
func (s *Store) FindMerchantByAlias(ctx context.Context, key string) (*domain.Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`SELECT m.id, m.canonical_name, m.normalised_name, m.merchant_family, m.created_at
		 FROM merchants m
		 JOIN merchant_aliases a ON a.merchant_id = m.id
		 WHERE a.normalised_alias = $1`, key))
	if isNoRows(err) {
		return nil, notFound("FindMerchantByAlias", "merchant")
	}
	if err != nil {
		return nil, internalErr("FindMerchantByAlias", err)
	}
	return m, nil
}

// FindMerchantFuzzy does an escaped ILIKE substring match on normalised
// names and returns the longest match (shortest edit distance proxy).
// CodeNotFound when nothing contains the key.
func (s *Store) FindMerchantFuzzy(ctx context.Context, key string) (*domain.Merchant, error) {
	pattern := "%" + escapeLike(key) + "%"
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`SELECT `+merchantColumns+` FROM merchants
		 WHERE normalised_name ILIKE $1 ESCAPE '\'
		 ORDER BY length(normalised_name) DESC LIMIT 1`,
		pattern))
	if isNoRows(err) {
		return nil, notFound("FindMerchantFuzzy", "merchant")
	}
	if err != nil {
		return nil, internalErr("FindMerchantFuzzy", err)
	}
	return m, nil
}

// escapeLike escapes ILIKE metacharacters; the pattern itself always travels
// as a placeholder, never concatenated into SQL.
func escapeLike(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '%', '_':
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// CreateMerchant inserts a merchant learned on the fly. Concurrent creation
// of the same normalised name returns the existing row.
func (s *Store) CreateMerchant(ctx context.Context, canonical, key, family string) (*domain.Merchant, error) {
	m, err := scanMerchant(s.pool.QueryRow(ctx,
		`INSERT INTO merchants (id, canonical_name, normalised_name, merchant_family)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (normalised_name) DO UPDATE SET canonical_name = merchants.canonical_name
		 RETURNING `+merchantColumns,
		uuid.New(), canonical, key, family))
	if err != nil {
		return nil, internalErr("CreateMerchant", err)
	}
	return m, nil
}

// AddMerchantAlias attaches a new spelling; duplicates are a no-op.
func (s *Store) AddMerchantAlias(ctx context.Context, merchantID uuid.UUID, alias, key, source string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO merchant_aliases (id, merchant_id, alias, normalised_alias, source)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (normalised_alias) DO NOTHING`,
		uuid.New(), merchantID, alias, key, source)
	if err != nil {
		return internalErr("AddMerchantAlias", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// User merchant rules (learned preferences)
// ---------------------------------------------------------------------------

const ruleColumns = `user_id, merchant_id, preferred_category_id,
	preferred_transaction_type, sample_count, confidence, last_confirmed_at`

func scanRule(row pgx.Row) (*domain.UserMerchantRule, error) {
	var r domain.UserMerchantRule
	err := row.Scan(&r.UserID, &r.MerchantID, &r.PreferredCategoryID,
		&r.PreferredTransactionType, &r.SampleCount, &r.Confidence, &r.LastConfirmedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetUserMerchantRule loads the learned preference; CodeNotFound means the
// user has never confirmed this merchant, which is normal.
func (s *Store) GetUserMerchantRule(ctx context.Context, userID, merchantID uuid.UUID) (*domain.UserMerchantRule, error) {
	r, err := scanRule(s.pool.QueryRow(ctx,
		`SELECT `+ruleColumns+` FROM user_merchant_rules WHERE user_id = $1 AND merchant_id = $2`,
		userID, merchantID))
	if isNoRows(err) {
		return nil, notFound("GetUserMerchantRule", "rule")
	}
	if err != nil {
		return nil, internalErr("GetUserMerchantRule", err)
	}
	return r, nil
}

// UpsertUserMerchantRule inserts or grows the learned rule. SampleCount on
// the argument is a delta: the stored count increases by it, while the
// caller-supplied confidence (computed from the expected new count) and the
// preferred values are replaced.
func (s *Store) UpsertUserMerchantRule(ctx context.Context, rule *domain.UserMerchantRule) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO user_merchant_rules
			(user_id, merchant_id, preferred_category_id, preferred_transaction_type,
			 sample_count, confidence, last_confirmed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (user_id, merchant_id) DO UPDATE SET
			preferred_category_id = EXCLUDED.preferred_category_id,
			preferred_transaction_type = EXCLUDED.preferred_transaction_type,
			sample_count = user_merchant_rules.sample_count + EXCLUDED.sample_count,
			confidence = EXCLUDED.confidence,
			last_confirmed_at = EXCLUDED.last_confirmed_at`,
		rule.UserID, rule.MerchantID, rule.PreferredCategoryID,
		string(rule.PreferredTransactionType), rule.SampleCount, rule.Confidence,
		rule.LastConfirmedAt.UTC())
	if err != nil {
		return internalErr("UpsertUserMerchantRule", err)
	}
	return nil
}
