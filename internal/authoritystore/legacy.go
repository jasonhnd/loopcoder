package authoritystore

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

// LegacyReadOnly is a read-only compatibility port over v0.8 storage.
// It never opens the v0.9 compact foundation and never allows writes.
type LegacyReadOnly struct {
	inner storage.Store
	path  string
}

// LegacyOpenOptions controls legacy read-only opens.
type LegacyOpenOptions struct {
	Path string
	Now  func() time.Time
}

// OpenLegacyReadOnly opens an existing v0.8 storage database for read-only access.
func OpenLegacyReadOnly(ctx context.Context, opts LegacyOpenOptions) (*LegacyReadOnly, error) {
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, fmt.Errorf("open legacy storage: path is required")
	}
	path = filepath.Clean(path)
	inner, err := storage.OpenReadOnly(ctx, storage.Options{
		Path: path,
		Now:  opts.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("open legacy storage read-only: %w", err)
	}
	return &LegacyReadOnly{inner: inner, path: path}, nil
}

// Path returns the legacy database path.
func (l *LegacyReadOnly) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close closes the read-only handle.
func (l *LegacyReadOnly) Close() error {
	if l == nil || l.inner == nil {
		return nil
	}
	return l.inner.Close()
}

// Storage exposes the underlying read-only storage.Store for migration exporters.
// Writers must not be used; OpenReadOnly already rejects writes.
func (l *LegacyReadOnly) Storage() storage.Store {
	if l == nil {
		return nil
	}
	return l.inner
}

// Writable reports false; the compatibility port is read-only by construction.
func (l *LegacyReadOnly) Writable() bool { return false }
