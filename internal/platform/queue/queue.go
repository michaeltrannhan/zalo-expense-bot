// Package queue is the local PostgreSQL-backed job queue. It mirrors SQS
// semantics (visibility timeout, max receives, dead-letter state) so a real
// SQS adapter can replace it without touching producers or consumers.
package queue

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/platform/clock"
)

// ErrEmpty is returned by Dequeue when no job is ready.
var ErrEmpty = errors.New("queue: no job available")

// ErrDuplicate is returned by Enqueue when the dedupe key already exists.
var ErrDuplicate = errors.New("queue: duplicate dedupe key")

// ErrStaleClaim is returned when Ack/Nack/Heartbeat uses a claim token that
// no longer owns the job (another worker reclaimed it after lease expiry).
var ErrStaleClaim = errors.New("queue: stale claim token")

// Queue is the producer/consumer contract used by the API and workers.
type Queue interface {
	Enqueue(ctx context.Context, kind domain.JobKind, payload []byte, dedupeKey *string, maxAttempts int) (uuid.UUID, error)
	Dequeue(ctx context.Context, kinds []domain.JobKind, visibility time.Duration) (*domain.QueueJob, error)
	Heartbeat(ctx context.Context, id, claimToken uuid.UUID, visibility time.Duration) error
	Ack(ctx context.Context, id, claimToken uuid.UUID) error
	Nack(ctx context.Context, id, claimToken uuid.UUID, cause error) error
	Depth(ctx context.Context) (map[domain.JobKind]int64, error)
}

// PG implements Queue on the queue_jobs table.
type PG struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

func NewPG(pool *pgxpool.Pool, clk clock.Clock) *PG {
	return &PG{pool: pool, clock: clk}
}

func (q *PG) Enqueue(ctx context.Context, kind domain.JobKind, payload []byte, dedupeKey *string, maxAttempts int) (uuid.UUID, error) {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	id := uuid.New()
	_, err := q.pool.Exec(ctx, `
		INSERT INTO queue_jobs (id, kind, payload_json, dedupe_key, max_attempts)
		VALUES ($1, $2, $3, $4, $5)`,
		id, string(kind), payload, dedupeKey, maxAttempts)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return uuid.Nil, ErrDuplicate
		}
		return uuid.Nil, fmt.Errorf("enqueue %s: %w", kind, err)
	}
	return id, nil
}

// Dequeue atomically claims one ready job: queued, due, or a running job
// whose visibility timeout expired (crash recovery). Each claim receives a
// fresh claim_token; only that token may Ack/Nack/Heartbeat the job.
func (q *PG) Dequeue(ctx context.Context, kinds []domain.JobKind, visibility time.Duration) (*domain.QueueJob, error) {
	now := q.clock.Now()
	claim := uuid.New()
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	row := q.pool.QueryRow(ctx, `
		UPDATE queue_jobs
		SET status = 'running', attempts = attempts + 1,
		    claim_token = $2, visible_at = $3, updated_at = $3
		WHERE id = (
			SELECT id FROM queue_jobs
			WHERE kind = ANY($4)
			  AND ((status = 'queued' AND run_after <= $1)
			    OR (status = 'running' AND visible_at <= $1))
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, kind, payload_json, dedupe_key, status, attempts, max_attempts,
		          run_after, visible_at, last_error, created_at, updated_at, claim_token`,
		now, claim, now.Add(visibility), names)

	var j domain.QueueJob
	var kind string
	var token *uuid.UUID
	if err := row.Scan(&j.ID, &kind, &j.Payload, &j.DedupeKey, &j.Status,
		&j.Attempts, &j.MaxAttempts, &j.RunAfter, &j.VisibleAt, &j.LastError,
		&j.CreatedAt, &j.UpdatedAt, &token); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrEmpty
		}
		return nil, fmt.Errorf("dequeue: %w", err)
	}
	j.Kind = domain.JobKind(kind)
	if token != nil {
		j.ClaimToken = *token
	}
	return &j, nil
}

// Heartbeat extends the visibility deadline for the owning claim.
func (q *PG) Heartbeat(ctx context.Context, id, claimToken uuid.UUID, visibility time.Duration) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE queue_jobs
		SET visible_at = $3, updated_at = $3
		WHERE id = $1 AND claim_token = $2 AND status = 'running'`,
		id, claimToken, q.clock.Now().Add(visibility))
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

func (q *PG) Ack(ctx context.Context, id, claimToken uuid.UUID) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE queue_jobs
		SET status = 'done', claim_token = NULL, updated_at = $3
		WHERE id = $1 AND claim_token = $2 AND status = 'running'`,
		id, claimToken, q.clock.Now())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

// Nack returns the job to the queue with exponential backoff, or marks it
// dead once attempts are exhausted. Backoff: 2^attempts seconds, capped 5m.
func (q *PG) Nack(ctx context.Context, id, claimToken uuid.UUID, cause error) error {
	now := q.clock.Now()
	var attempts, maxAttempts int
	err := q.pool.QueryRow(ctx, `
		SELECT attempts, max_attempts FROM queue_jobs
		WHERE id = $1 AND claim_token = $2 AND status = 'running'`,
		id, claimToken).Scan(&attempts, &maxAttempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleClaim
		}
		return fmt.Errorf("nack read attempts: %w", err)
	}
	// Compute the deadline in Go. Postgres infers `$3 + make_interval(...)`
	// as interval when $3 is an untyped parameter, which cannot be assigned
	// to timestamptz run_after (SQLSTATE 42804).
	secs := int64(math.Min(math.Pow(2, float64(attempts)), 300))
	runAfter := now.Add(time.Duration(secs) * time.Second)
	tag, err := q.pool.Exec(ctx, `
		UPDATE queue_jobs
		SET status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
		    run_after = $4,
		    last_error = $5, claim_token = NULL, updated_at = $3
		WHERE id = $1 AND claim_token = $2 AND status = 'running'`,
		id, claimToken, now, runAfter, truncateErr(cause))
	if err != nil {
		return fmt.Errorf("nack: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

func (q *PG) Depth(ctx context.Context) (map[domain.JobKind]int64, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT kind, count(*) FROM queue_jobs
		WHERE status IN ('queued','running') GROUP BY kind`)
	if err != nil {
		return nil, fmt.Errorf("depth: %w", err)
	}
	defer rows.Close()
	out := map[domain.JobKind]int64{}
	for rows.Next() {
		var kind string
		var n int64
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[domain.JobKind(kind)] = n
	}
	return out, rows.Err()
}

func truncateErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	const max = 500
	if len(s) > max {
		s = s[:max]
	}
	return s
}
