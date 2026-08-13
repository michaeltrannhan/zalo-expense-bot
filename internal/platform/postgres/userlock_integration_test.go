//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/internal/platform/postgres"
	"zl-expese-bot/internal/platform/postgres/pgtest"
)

func TestUserLockCancelledCallbackReleasesLock(t *testing.T) {
	pool := pgtest.NewPool(t)
	userID := uuid.New()
	ctx, cancel := context.WithCancel(context.Background())

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- postgres.WithUserLock(ctx, pool, userID, func(locked context.Context) error {
			close(started)
			<-locked.Done()
			return locked.Err()
		})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("lock callback did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected canceled error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock callback did not return after cancel")
	}

	second, cancelSecond := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelSecond()
	if err := postgres.WithUserLock(second, pool, userID, func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatalf("follow-up lock after cancel: %v", err)
	}
}
