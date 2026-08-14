// Package store is the single persistence layer over the 0001+0002 schema.
// It owns every SQL statement in the application: domain types in and out,
// placeholders only, and *domain.Error codes for expected failures
// (NotFound/Conflict/Validation). Nullable columns map to pointer fields.
package store

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/postgres"
)

// Store wraps the connection pool; all methods are safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a Store on an existing pool (see platform/postgres.Connect).
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// WithUserLock serializes multi-step user work with account deletion.
func (s *Store) WithUserLock(ctx context.Context, userID uuid.UUID, fn func(context.Context) error) error {
	return postgres.WithUserLock(ctx, s.pool, userID, fn)
}

func internalErr(op string, err error) error {
	return classifyStoreErr(op, err)
}

// classifyStoreErr maps connectivity, timeout, and serialization failures to
// CodeTransient so callers can retry; everything else stays CodeInternal.
func classifyStoreErr(op string, err error) error {
	if err == nil {
		return nil
	}
	code := domain.CodeInternal
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		code = domain.CodeTransient
	case errors.Is(err, context.Canceled):
		// Caller canceled the request; not a retryable store failure.
		code = domain.CodeInternal
	case isTransientPgError(err),
		isTransientNetError(err),
		errors.Is(err, io.ErrUnexpectedEOF),
		isTransientStoreMessage(err):
		code = domain.CodeTransient
	}
	return domain.E(code, "store."+op, err)
}

func isTransientPgError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "40001", // serialization_failure
		"40P01", // deadlock_detected
		"55P03", // lock_not_available
		"57P01", // admin_shutdown
		"57P02", // crash_shutdown
		"57P03", // cannot_connect_now
		"53300", // too_many_connections
		"08000", // connection_exception
		"08003", // connection_does_not_exist
		"08006": // connection_failure
		return true
	default:
		return false
	}
}

func isTransientNetError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func isTransientStoreMessage(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, frag := range []string{
		"connection refused",
		"i/o timeout",
		"broken pipe",
		"conn closed",
	} {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}

func notFound(op, what string) error {
	return domain.Ef(domain.CodeNotFound, nil, "store.%s: %s not found", op, what)
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
