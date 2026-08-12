package postgres

import (
	"context"
	"encoding/binary"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type userLockContextKey struct{}

var userLockSemaphores sync.Map // map[*pgxpool.Pool]chan struct{}

// WithUserLock serializes all work that can mutate one user's data. The
// session advisory lock spans calls made through other pooled connections,
// which makes it suitable for multi-step handlers and external API calls.
// The callback context records ownership so nested deletion is reentrant.
func WithUserLock(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, fn func(context.Context) error) error {
	if UserLockHeld(ctx, userID) {
		return fn(ctx)
	}
	sem := userLockSemaphore(pool)
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	key1, key2 := userLockKeys(userID)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, key1, key2); err != nil {
		return err
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1, $2)`, key1, key2)
	}()

	lockedCtx := context.WithValue(ctx, userLockContextKey{}, userID)
	return fn(lockedCtx)
}

// userLockSemaphore leaves at least half of the pool available to callback
// queries. Without this bound, N advisory-lock sessions could consume all N
// connections and deadlock while their callbacks wait for one more.
func userLockSemaphore(pool *pgxpool.Pool) chan struct{} {
	if existing, ok := userLockSemaphores.Load(pool); ok {
		return existing.(chan struct{})
	}
	capacity := int(pool.Config().MaxConns) / 2
	if capacity < 1 {
		capacity = 1
	}
	created := make(chan struct{}, capacity)
	actual, _ := userLockSemaphores.LoadOrStore(pool, created)
	return actual.(chan struct{})
}

// UserLockHeld reports whether ctx came from WithUserLock for this user.
func UserLockHeld(ctx context.Context, userID uuid.UUID) bool {
	locked, ok := ctx.Value(userLockContextKey{}).(uuid.UUID)
	return ok && locked == userID
}

func userLockKeys(userID uuid.UUID) (int32, int32) {
	return int32(binary.BigEndian.Uint32(userID[0:4])), int32(binary.BigEndian.Uint32(userID[4:8]))
}
