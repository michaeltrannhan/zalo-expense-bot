package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"zl-expese-bot/internal/domain"
)

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

const categoryColumns = `id, system_key, display_name, parent_id, is_system, created_at`

func scanCategory(row pgx.Row) (*domain.Category, error) {
	var c domain.Category
	err := row.Scan(&c.ID, &c.SystemKey, &c.DisplayName, &c.ParentID, &c.IsSystem, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListCategories returns the full taxonomy (system seeds + user customs).
func (s *Store) ListCategories(ctx context.Context) ([]domain.Category, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+categoryColumns+` FROM categories ORDER BY system_key`)
	if err != nil {
		return nil, internalErr("ListCategories", err)
	}
	defer rows.Close()
	var out []domain.Category
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, internalErr("ListCategories", err)
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("ListCategories", err)
	}
	return out, nil
}

// GetCategoryByKey resolves a stable machine key such as "an-uong".
func (s *Store) GetCategoryByKey(ctx context.Context, key string) (*domain.Category, error) {
	c, err := scanCategory(s.pool.QueryRow(ctx,
		`SELECT `+categoryColumns+` FROM categories WHERE system_key = $1`, key))
	if isNoRows(err) {
		return nil, notFound("GetCategoryByKey", "category")
	}
	if err != nil {
		return nil, internalErr("GetCategoryByKey", err)
	}
	return c, nil
}

// GetCategoryByID loads a category by primary key.
func (s *Store) GetCategoryByID(ctx context.Context, id uuid.UUID) (*domain.Category, error) {
	c, err := scanCategory(s.pool.QueryRow(ctx,
		`SELECT `+categoryColumns+` FROM categories WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, notFound("GetCategoryByID", "category")
	}
	if err != nil {
		return nil, internalErr("GetCategoryByID", err)
	}
	return c, nil
}
