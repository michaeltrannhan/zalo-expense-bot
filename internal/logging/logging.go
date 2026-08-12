// Package logging configures slog with correlation IDs and redaction.
// Receipt content, full sender IDs, tokens and secrets must never be logged
// (plan §21.2); use HashID for identifiers.
package logging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"strings"
)

type ctxKey string

const (
	ctxRequestID ctxKey = "request_id"
	ctxJobID     ctxKey = "job_id"
	ctxUserHash  ctxKey = "user_id_hash"
)

// New builds the root logger.
func New(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(strings.ToLower(level))); err != nil {
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}

// WithRequestID attaches a request correlation ID to the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// WithJobID attaches a queue job ID to the context.
func WithJobID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxJobID, id)
}

// WithUser attaches the hashed user ID to the context.
func WithUser(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxUserHash, HashID(userID))
}

// FromContext returns a logger carrying whatever correlation fields are set.
func FromContext(ctx context.Context, base *slog.Logger) *slog.Logger {
	l := base
	if v, ok := ctx.Value(ctxRequestID).(string); ok && v != "" {
		l = l.With("request_id", v)
	}
	if v, ok := ctx.Value(ctxJobID).(string); ok && v != "" {
		l = l.With("job_id", v)
	}
	if v, ok := ctx.Value(ctxUserHash).(string); ok && v != "" {
		l = l.With("user_id_hash", v)
	}
	return l
}

// HashID one-way truncates an identifier for log lines.
func HashID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:12]
}
