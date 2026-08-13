//go:build integration

package pgtest

import (
	"os"
	"testing"
)

// testDatabaseURL returns TEST_DATABASE_URL after verifying the database
// name ends in "_test". Integration helpers truncate tables; refusing
// development/production names prevents accidental data loss.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	name, err := DatabaseName(raw)
	if err != nil {
		t.Fatalf("pgtest: parse TEST_DATABASE_URL: %v", err)
	}
	if !IsDisposableTestDatabase(name) {
		t.Fatalf("pgtest: refusing database %q; name must end in _test (got URL from TEST_DATABASE_URL)", name)
	}
	return raw
}
