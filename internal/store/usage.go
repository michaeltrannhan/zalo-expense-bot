package store

import (
	"context"
)

// ---------------------------------------------------------------------------
// Usage counters (quota enforcement)
// ---------------------------------------------------------------------------

// IncrementUsage atomically adds delta to a counter, creating it on first
// use, and returns the new count for limit checks.
func (s *Store) IncrementUsage(ctx context.Context, scope, scopeID, period, metric string, delta, limit int64) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO usage_counters (scope, scope_id, period, metric, count, limit_value)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (scope, scope_id, period, metric) DO UPDATE SET
			count = usage_counters.count + EXCLUDED.count,
			limit_value = EXCLUDED.limit_value,
			updated_at = now()
		 RETURNING count`,
		scope, scopeID, period, metric, delta, limit).Scan(&count)
	if err != nil {
		return 0, internalErr("IncrementUsage", err)
	}
	return count, nil
}

// GetUsage reads a counter; a missing counter is 0, not an error.
func (s *Store) GetUsage(ctx context.Context, scope, scopeID, period, metric string) (int64, error) {
	var count int64
	err := s.pool.QueryRow(ctx,
		`SELECT count FROM usage_counters
		 WHERE scope = $1 AND scope_id = $2 AND period = $3 AND metric = $4`,
		scope, scopeID, period, metric).Scan(&count)
	if isNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, internalErr("GetUsage", err)
	}
	return count, nil
}
