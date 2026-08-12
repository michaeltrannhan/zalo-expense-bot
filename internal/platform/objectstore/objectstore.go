// Package objectstore is the local-filesystem object store (S3 replacement).
// Keys are user-scoped paths; objects are never world-accessible because the
// root directory is local and writes are atomic (temp file + rename).
package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Stored describes a persisted object.
type Stored struct {
	Key         string
	SHA256      string
	ByteSize    int64
	ContentType string
}

// Store is the minimal contract the receipt pipeline needs.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, contentType string) (Stored, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// Local stores objects under root on the filesystem.
type Local struct {
	root string
}

func NewLocal(root string) (*Local, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create object store root: %w", err)
	}
	return &Local{root: root}, nil
}

// validateKey rejects traversal and absolute paths.
func validateKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return fmt.Errorf("objectstore: invalid key %q", key)
	}
	return nil
}

func (l *Local) path(key string) string { return filepath.Join(l.root, filepath.FromSlash(key)) }

// Put streams r to disk while hashing, then atomically renames into place.
func (l *Local) Put(_ context.Context, key string, r io.Reader, contentType string) (Stored, error) {
	if err := validateKey(key); err != nil {
		return Stored{}, err
	}
	dst := l.path(key)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return Stored{}, fmt.Errorf("create object dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return Stored{}, fmt.Errorf("create temp object: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		return Stored{}, fmt.Errorf("write object: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Stored{}, fmt.Errorf("close object: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return Stored{}, fmt.Errorf("chmod object: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return Stored{}, fmt.Errorf("commit object: %w", err)
	}
	return Stored{
		Key:         key,
		SHA256:      hex.EncodeToString(h.Sum(nil)),
		ByteSize:    n,
		ContentType: contentType,
	}, nil
}

var ErrNotFound = errors.New("objectstore: object not found")

func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	f, err := os.Open(l.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open object: %w", err)
	}
	return f, nil
}

func (l *Local) Delete(_ context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	err := os.Remove(l.path(key))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}
