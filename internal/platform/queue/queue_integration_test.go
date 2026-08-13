//go:build integration

package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/clock"
	"zl-expese-bot/internal/platform/postgres/pgtest"
	"zl-expese-bot/internal/platform/queue"
)

func TestStaleAckFromSecondWorkerIsRejected(t *testing.T) {
	pool := pgtest.NewPool(t)
	clk := clock.Real{}
	q := queue.NewPG(pool, clk)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, domain.JobReceiptProcess, []byte(`{"v":1}`), nil, 5)
	if err != nil {
		t.Fatal(err)
	}

	first, err := q.Dequeue(ctx, []domain.JobKind{domain.JobReceiptProcess}, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != id {
		t.Fatalf("first job = %s, want %s", first.ID, id)
	}

	time.Sleep(80 * time.Millisecond)
	second, err := q.Dequeue(ctx, []domain.JobKind{domain.JobReceiptProcess}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.ClaimToken == first.ClaimToken {
		t.Fatal("second claim reused the first token")
	}

	if err := q.Ack(ctx, first.ID, first.ClaimToken); !errors.Is(err, queue.ErrStaleClaim) {
		t.Fatalf("stale ack = %v, want ErrStaleClaim", err)
	}
	if err := q.Nack(ctx, first.ID, first.ClaimToken, errors.New("stale")); !errors.Is(err, queue.ErrStaleClaim) {
		t.Fatalf("stale nack = %v, want ErrStaleClaim", err)
	}
	if err := q.Ack(ctx, second.ID, second.ClaimToken); err != nil {
		t.Fatalf("owner ack: %v", err)
	}
}

func TestHeartbeatExtendsLease(t *testing.T) {
	pool := pgtest.NewPool(t)
	clk := clock.Real{}
	q := queue.NewPG(pool, clk)
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, domain.JobReceiptProcess, []byte(`{"v":1}`), nil, 5); err != nil {
		t.Fatal(err)
	}
	job, err := q.Dequeue(ctx, []domain.JobKind{domain.JobReceiptProcess}, 80*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := q.Heartbeat(ctx, job.ID, job.ClaimToken, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	_, err = q.Dequeue(ctx, []domain.JobKind{domain.JobReceiptProcess}, time.Minute)
	if !errors.Is(err, queue.ErrEmpty) {
		t.Fatalf("dequeue during heartbeat = %v, want empty", err)
	}
	if err := q.Ack(ctx, job.ID, job.ClaimToken); err != nil {
		t.Fatal(err)
	}
}
