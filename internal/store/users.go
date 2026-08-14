package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/postgres"
)

// ---------------------------------------------------------------------------
// Users and identities
// ---------------------------------------------------------------------------

const userColumns = `id, status, timezone, default_currency, locale,
	consent_version, consented_at, created_at, updated_at, deleted_at`

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Status, &u.Timezone, &u.DefaultCurrency, &u.Locale,
		&u.ConsentVersion, &u.ConsentedAt, &u.CreatedAt, &u.UpdatedAt, &u.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateUser inserts a pending user; timezone/currency/locale defaults come
// from the schema (Asia/Ho_Chi_Minh, VND, vi-VN).
func (s *Store) CreateUser(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO users (id) VALUES ($1)`, id)
	if err != nil {
		return internalErr("CreateUser", err)
	}
	return nil
}

// GetUser loads a user by primary key.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, notFound("GetUser", "user")
	}
	if err != nil {
		return nil, internalErr("GetUser", err)
	}
	return u, nil
}

// GetUserByIdentity resolves a provider subject to its local user.
func (s *Store) GetUserByIdentity(ctx context.Context, provider domain.Provider, subject, scope string) (*domain.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT u.id, u.status, u.timezone, u.default_currency, u.locale,
		 u.consent_version, u.consented_at, u.created_at, u.updated_at, u.deleted_at
		 FROM users u
		 JOIN user_identities i ON i.user_id = u.id
		 WHERE i.provider = $1 AND i.provider_subject = $2 AND i.provider_scope = $3`,
		string(provider), subject, scope))
	if isNoRows(err) {
		return nil, notFound("GetUserByIdentity", "identity")
	}
	if err != nil {
		return nil, internalErr("GetUserByIdentity", err)
	}
	return u, nil
}

// UpdateUserPreferences stores the user-facing timezone and default
// currency together. Callers validate IANA/ISO semantics before writing;
// the store still rejects empty values so corrupted settings cannot enter
// through another call site.
func (s *Store) UpdateUserPreferences(ctx context.Context, userID uuid.UUID, timezone, currency string) error {
	timezone = strings.TrimSpace(timezone)
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if timezone == "" || currency == "" {
		return domain.E(domain.CodeValidation, "store.UpdateUserPreferences: timezone and currency are required", nil)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE users
		SET timezone = $2, default_currency = $3, updated_at = now()
		WHERE id = $1 AND status <> 'deleted'`,
		userID, timezone, currency)
	if err != nil {
		return internalErr("UpdateUserPreferences", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("UpdateUserPreferences", "active user")
	}
	return nil
}

// CreateIdentity links a provider subject to a user. The UNIQUE constraint
// on (provider, subject, scope) is the guard against double registration.
func (s *Store) CreateIdentity(ctx context.Context, userID uuid.UUID, provider domain.Provider, subject, scope string) (*domain.UserIdentity, error) {
	id := uuid.New()
	var ident domain.UserIdentity
	err := s.pool.QueryRow(ctx,
		`INSERT INTO user_identities (id, user_id, provider, provider_subject, provider_scope)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING id, user_id, provider, provider_subject, provider_scope, created_at, last_seen_at`,
		id, userID, string(provider), subject, scope).
		Scan(&ident.ID, &ident.UserID, &ident.Provider, &ident.ProviderSubject, &ident.ProviderScope,
			&ident.CreatedAt, &ident.LastSeenAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, domain.E(domain.CodeConflict, "store.CreateIdentity: identity exists", nil)
		}
		return nil, internalErr("CreateIdentity", err)
	}
	return &ident, nil
}

// CreateUserWithIdentity inserts a pending user and its provider identity in
// one transaction. A unique conflict on the identity rolls back the user row
// so concurrent first contact cannot leave orphans.
func (s *Store) CreateUserWithIdentity(ctx context.Context, userID uuid.UUID, provider domain.Provider, subject, scope string) (*domain.User, error) {
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO users (id) VALUES ($1)`, userID); err != nil {
			return internalErr("CreateUserWithIdentity", err)
		}
		id := uuid.New()
		_, err := tx.Exec(ctx,
			`INSERT INTO user_identities (id, user_id, provider, provider_subject, provider_scope)
			 VALUES ($1, $2, $3, $4, $5)`,
			id, userID, string(provider), subject, scope)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.E(domain.CodeConflict, "store.CreateUserWithIdentity: identity exists", nil)
			}
			return internalErr("CreateUserWithIdentity", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, userID)
}

// TouchIdentityLastSeen refreshes last_seen_at on every verified webhook.
func (s *Store) TouchIdentityLastSeen(ctx context.Context, provider domain.Provider, subject, scope string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE user_identities SET last_seen_at = now()
		 WHERE provider = $1 AND provider_subject = $2 AND provider_scope = $3`,
		string(provider), subject, scope)
	if err != nil {
		return internalErr("TouchIdentityLastSeen", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("TouchIdentityLastSeen", "identity")
	}
	return nil
}

// SetConsent records PDPD consent and activates the user. Only a pending
// user can consent; anything else is a state conflict.
func (s *Store) SetConsent(ctx context.Context, userID uuid.UUID, version string, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET status = 'active', consent_version = $2, consented_at = $3, updated_at = now()
		 WHERE id = $1 AND status = 'pending'`,
		userID, version, at.UTC())
	if err != nil {
		return internalErr("SetConsent", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.E(domain.CodeConflict, "store.SetConsent: user not pending", nil)
	}
	return nil
}

// SetUserStatus moves a user through the lifecycle; deletion stamps deleted_at.
func (s *Store) SetUserStatus(ctx context.Context, userID uuid.UUID, status domain.UserStatus) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE users SET status = $2, updated_at = now(),
			deleted_at = CASE WHEN $2 = 'deleted' THEN now() ELSE deleted_at END
		 WHERE id = $1`,
		userID, string(status))
	if err != nil {
		return internalErr("SetUserStatus", err)
	}
	if tag.RowsAffected() == 0 {
		return notFound("SetUserStatus", "user")
	}
	return nil
}
