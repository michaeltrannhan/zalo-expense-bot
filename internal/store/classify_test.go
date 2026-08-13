package store

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"zl-expese-bot/internal/domain"
)

func TestClassifyStoreErrTransient(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want domain.Code
	}{
		{"deadline", context.DeadlineExceeded, domain.CodeTransient},
		{"canceled", context.Canceled, domain.CodeInternal},
		{"serialization", &pgconn.PgError{Code: "40001"}, domain.CodeTransient},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, domain.CodeTransient},
		{"conn failure", &pgconn.PgError{Code: "08006"}, domain.CodeTransient},
		{"unique", &pgconn.PgError{Code: "23505"}, domain.CodeInternal},
		{"net op", &net.OpError{Op: "read", Err: errors.New("reset")}, domain.CodeTransient},
		{"reset", syscall.ECONNRESET, domain.CodeTransient},
		{"unexpected eof", io.ErrUnexpectedEOF, domain.CodeTransient},
		{"refused text", errors.New("connection refused"), domain.CodeTransient},
		{"other", errors.New("syntax error"), domain.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStoreErr("Op", tc.err)
			if domain.CodeOf(got) != tc.want {
				t.Fatalf("classifyStoreErr(%v) = %s, want %s", tc.err, domain.CodeOf(got), tc.want)
			}
		})
	}
}
