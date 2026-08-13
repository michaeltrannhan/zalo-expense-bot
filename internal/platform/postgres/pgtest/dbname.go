package pgtest

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// DatabaseName extracts the PostgreSQL database name from a URL.
func DatabaseName(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	name := strings.TrimPrefix(path.Base(u.Path), "/")
	if name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("empty database name in URL")
	}
	return name, nil
}

// IsDisposableTestDatabase reports whether name ends in _test so helpers that
// truncate tables refuse development/production databases.
func IsDisposableTestDatabase(name string) bool {
	return strings.HasSuffix(name, "_test")
}
