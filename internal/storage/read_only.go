package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrReadOnlyStore is returned when a caller attempts to write through a
// store opened with OpenReadOnly.
var ErrReadOnlyStore = errors.New("storage is read-only")

// readOnlyStore is a deliberately separate implementation from sqliteStore.
// Keeping the write-capable fields and retry machinery out of this type makes
// the read-only boundary explicit at both the Store and Tx layers.
type readOnlyStore struct {
	path string
	db   *sql.DB
	now  func() time.Time
}

type readOnlyTx struct {
	tx *sql.Tx
}

// OpenReadOnly opens an existing, current-schema SQLite store without
// creating paths, repairing permissions, applying migrations, or hardening
// sidecars. The SQLite connection itself is opened with mode=ro and
// query_only, and the returned Store rejects every write transaction.
func OpenReadOnly(ctx context.Context, opts Options) (Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, errors.New("open storage read-only: path is required")
	}
	path = filepath.Clean(path)

	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("open storage read-only: database does not exist: %s", path)
		}
		return nil, fmt.Errorf("open storage read-only: inspect database %s: %w", path, err)
	}
	permissions, err := CheckPermissions(path)
	if err != nil {
		return nil, fmt.Errorf("open storage read-only: inspect permissions for %s: %w", path, err)
	}
	if unsafe := firstUnsafePermissionItem(permissions); unsafe != nil {
		return nil, fmt.Errorf("open storage read-only: unsafe storage path %s: %s", unsafe.Path, unsafe.Message)
	}
	if permissions.Supported && !permissions.Secure {
		for _, item := range permissions.Items {
			if item.Exists && !item.Secure {
				return nil, fmt.Errorf("open storage read-only: insecure %s %s: %s", item.Kind, item.Path, item.Message)
			}
		}
		return nil, fmt.Errorf("open storage read-only: storage permissions for %s are not secure", path)
	}

	db, err := openReadOnlySQLite(ctx, path)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (Store, error) {
		_ = db.Close()
		return nil, err
	}
	version, err := schemaVersion(ctx, db)
	if err != nil {
		return closeOnError(fmt.Errorf("open storage read-only: inspect schema version: %w", err))
	}
	switch {
	case version > CurrentSchemaVersion:
		return closeOnError(fmt.Errorf("open storage read-only: %w", unsupportedVersionError(version)))
	case version < CurrentSchemaVersion:
		return closeOnError(fmt.Errorf("open storage read-only: schema version %d is older than required version %d; run storage migration first", version, CurrentSchemaVersion))
	}
	if err := validateSchemaMigrationHistory(ctx, db, version); err != nil {
		return closeOnError(fmt.Errorf("open storage read-only: %w", err))
	}
	if err := checkRequiredTables(ctx, db); err != nil {
		return closeOnError(fmt.Errorf("open storage read-only: %w", err))
	}

	return &readOnlyStore{path: path, db: db, now: normalizeNow(opts.Now)}, nil
}

func (s *readOnlyStore) Close() error {
	return s.db.Close()
}

func (s *readOnlyStore) Path() string {
	return s.path
}

func (s *readOnlyStore) Now() time.Time {
	return s.now().UTC()
}

func (s *readOnlyStore) Health(ctx context.Context) (Health, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	health := Health{Path: s.path, Exists: true}
	permissions, err := CheckPermissions(s.path)
	if err != nil {
		return health, fmt.Errorf("storage health: inspect permissions for %s: %w", s.path, err)
	}
	if unsafe := firstUnsafePermissionItem(permissions); unsafe != nil {
		return health, fmt.Errorf("storage health: unsafe storage path %s: %s", unsafe.Path, unsafe.Message)
	}
	version, err := schemaVersion(ctx, s.db)
	if err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	health.SchemaVersion = version
	if version > CurrentSchemaVersion {
		return health, unsupportedVersionError(version)
	}
	if version == 0 {
		return health, errors.New("storage health: migrations table is missing or empty; run a newer loopcoder storage migration")
	}
	if err := checkRequiredTables(ctx, s.db); err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	if err := integrityCheck(ctx, s.db); err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	if err := checkDurableRunGraph(ctx, s.db); err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	health.OK = true
	health.Message = "storage database is healthy"
	return health, nil
}

func (s *readOnlyStore) WithTx(ctx context.Context, fn func(Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return errors.New("storage read-only transaction: callback is required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("storage read-only transaction: begin: %w", err)
	}
	if err := fn(readOnlyTx{tx: tx}); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return fmt.Errorf("storage read-only transaction: rollback after %v: %w", err, rollbackErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage read-only transaction: commit: %w", err)
	}
	return nil
}

func (s *readOnlyStore) WithWriteTx(context.Context, func(Tx) error) error {
	return fmt.Errorf("storage write transaction: %w", ErrReadOnlyStore)
}

func (tx readOnlyTx) Exec(context.Context, string, ...any) (sql.Result, error) {
	return nil, fmt.Errorf("storage transaction exec: %w", ErrReadOnlyStore)
}

func (tx readOnlyTx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.tx.QueryContext(ctx, query, args...)
}

func (tx readOnlyTx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.tx.QueryRowContext(ctx, query, args...)
}
