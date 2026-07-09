// Package storage provides the internal loopcoder runtime storage boundary.
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

	_ "modernc.org/sqlite"
)

const (
	// CurrentSchemaVersion is the newest SQLite schema version this binary can use.
	CurrentSchemaVersion = 3

	driverName = "sqlite"
)

// Store is the internal storage interface for v0.7 runtime state.
type Store interface {
	Close() error
	Path() string
	Health(context.Context) (Health, error)
	WithTx(context.Context, func(Tx) error) error
}

// Tx is the storage transaction boundary exposed to internal callers.
type Tx interface {
	Exec(context.Context, string, ...any) (sql.Result, error)
	Query(context.Context, string, ...any) (*sql.Rows, error)
	QueryRow(context.Context, string, ...any) *sql.Row
}

// Options controls SQLite store opening.
type Options struct {
	Path string
	Now  func() time.Time
}

// Health reports the local database state without exposing table internals.
type Health struct {
	Path          string
	Exists        bool
	SchemaVersion int
	OK            bool
	Message       string
}

type sqliteStore struct {
	path string
	db   *sql.DB
	now  func() time.Time
}

type sqlTx struct {
	tx *sql.Tx
}

var migrations = []migration{
	{
		version: 1,
		name:    "initial runtime schema",
		statements: []string{
			`CREATE TABLE IF NOT EXISTS migrations (
				version INTEGER PRIMARY KEY,
				name TEXT NOT NULL,
				applied_at TEXT NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS projects (
				id TEXT PRIMARY KEY,
				remote_url TEXT NOT NULL DEFAULT '',
				github_owner TEXT NOT NULL DEFAULT '',
				github_name TEXT NOT NULL DEFAULT '',
				local_path TEXT NOT NULL,
				default_branch TEXT NOT NULL DEFAULT '',
				display_name TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_projects_local_path ON projects(local_path)`,
			`CREATE TABLE IF NOT EXISTS runs (
				id TEXT PRIMARY KEY,
				project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
				parent_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
				issue_number INTEGER,
				status TEXT NOT NULL DEFAULT '',
				started_at TEXT,
				ended_at TEXT,
				updated_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_runs_project_id ON runs(project_id)`,
			`CREATE INDEX IF NOT EXISTS idx_runs_parent_run_id ON runs(parent_run_id)`,
			`CREATE TABLE IF NOT EXISTS run_events (
				id TEXT PRIMARY KEY,
				run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
				sequence INTEGER NOT NULL,
				ts TEXT NOT NULL,
				event_type TEXT NOT NULL,
				payload_json TEXT NOT NULL DEFAULT '{}',
				UNIQUE(run_id, sequence)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_run_events_run_id_sequence ON run_events(run_id, sequence)`,
			`CREATE TABLE IF NOT EXISTS run_edges (
				parent_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
				child_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
				edge_type TEXT NOT NULL DEFAULT '',
				created_at TEXT NOT NULL,
				PRIMARY KEY(parent_run_id, child_run_id)
			)`,
			`CREATE TABLE IF NOT EXISTS reports (
				id TEXT PRIMARY KEY,
				run_id TEXT REFERENCES runs(id) ON DELETE SET NULL,
				role TEXT NOT NULL,
				provider TEXT NOT NULL DEFAULT '',
				model TEXT NOT NULL DEFAULT '',
				started_at TEXT,
				ended_at TEXT,
				payload_json TEXT NOT NULL,
				created_at TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_reports_run_id ON reports(run_id)`,
		},
	},
	{
		version: 2,
		name:    "project identity fields",
		statements: []string{
			`ALTER TABLE projects ADD COLUMN local_path_canonical TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE projects ADD COLUMN git_root TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE projects ADD COLUMN remote_url_normalized TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE projects ADD COLUMN identity_source TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_projects_local_path_canonical ON projects(local_path_canonical)`,
			`CREATE INDEX IF NOT EXISTS idx_projects_remote_url_normalized ON projects(remote_url_normalized)`,
		},
	},
	{
		version: 3,
		name:    "legacy local state import metadata",
		statements: []string{
			`ALTER TABLE reports ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE reports ADD COLUMN source_path TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE reports ADD COLUMN source_hash TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE reports ADD COLUMN source_kind TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_reports_project_id ON reports(project_id)`,
			`CREATE INDEX IF NOT EXISTS idx_reports_source_hash ON reports(source_hash)`,
			`CREATE TABLE IF NOT EXISTS legacy_import_records (
				id TEXT PRIMARY KEY,
				project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
				run_id TEXT,
				record_type TEXT NOT NULL,
				source_path TEXT NOT NULL,
				source_line INTEGER NOT NULL DEFAULT 0,
				source_hash TEXT NOT NULL,
				payload_json TEXT NOT NULL DEFAULT '{}',
				imported_at TEXT NOT NULL
			)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_legacy_import_records_source ON legacy_import_records(project_id, record_type, source_path, source_line, source_hash)`,
			`CREATE INDEX IF NOT EXISTS idx_legacy_import_records_project ON legacy_import_records(project_id)`,
			`CREATE TABLE IF NOT EXISTS legacy_import_status (
				project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
				repo_path TEXT NOT NULL,
				started_at TEXT NOT NULL,
				completed_at TEXT NOT NULL,
				status TEXT NOT NULL,
				scanned_count INTEGER NOT NULL DEFAULT 0,
				imported_count INTEGER NOT NULL DEFAULT 0,
				skipped_count INTEGER NOT NULL DEFAULT 0,
				malformed_count INTEGER NOT NULL DEFAULT 0,
				message TEXT NOT NULL DEFAULT ''
			)`,
		},
	},
}

var requiredTables = []string{"migrations", "projects", "runs", "run_events", "run_edges", "reports", "legacy_import_records", "legacy_import_status"}

type migration struct {
	version    int
	name       string
	statements []string
}

// Open opens or creates the SQLite-backed store and applies supported migrations.
func Open(ctx context.Context, opts Options) (Store, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := strings.TrimSpace(opts.Path)
	if path == "" {
		return nil, errors.New("open storage: path is required")
	}
	path = filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("open storage: create data directory: %w", err)
	}

	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("open storage %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	store := &sqliteStore{path: path, db: db, now: normalizeNow(opts.Now)}
	if err := store.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// CheckHealth inspects an existing database path without creating or migrating it.
func CheckHealth(ctx context.Context, path string) (Health, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path = filepath.Clean(strings.TrimSpace(path))
	health := Health{Path: path}
	if path == "." || path == "" {
		return health, errors.New("storage health: path is required")
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			health.Message = "database has not been created"
			return health, nil
		}
		return health, fmt.Errorf("storage health: inspect %s: %w", path, err)
	}
	health.Exists = true

	db, err := sql.Open(driverName, path)
	if err != nil {
		return health, fmt.Errorf("storage health: open %s: %w", path, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return health, fmt.Errorf("storage health: enable foreign keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA query_only = ON`); err != nil {
		return health, fmt.Errorf("storage health: enable read-only mode: %w", err)
	}
	version, err := schemaVersion(ctx, db)
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
	if err := checkRequiredTables(ctx, db); err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	if err := integrityCheck(ctx, db); err != nil {
		return health, fmt.Errorf("storage health: %w", err)
	}
	health.OK = true
	health.Message = "storage database is healthy"
	return health, nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

func (s *sqliteStore) Path() string {
	return s.path
}

func (s *sqliteStore) Health(ctx context.Context) (Health, error) {
	return CheckHealth(ctx, s.path)
}

func (s *sqliteStore) WithTx(ctx context.Context, fn func(Tx) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return errors.New("storage transaction: callback is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage transaction: begin: %w", err)
	}
	wrapped := sqlTx{tx: tx}
	if err := fn(wrapped); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return fmt.Errorf("storage transaction: rollback after %v: %w", err, rollbackErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage transaction: commit: %w", err)
	}
	return nil
}

func (tx sqlTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, query, args...)
}

func (tx sqlTx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.tx.QueryContext(ctx, query, args...)
}

func (tx sqlTx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.tx.QueryRowContext(ctx, query, args...)
}

func (s *sqliteStore) configure(ctx context.Context) error {
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("open storage %s: configure sqlite: %w", s.path, err)
		}
	}
	return nil
}

func (s *sqliteStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate storage: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrate storage: initialize migrations table: %w", err)
	}
	version, err := txSchemaVersion(ctx, tx)
	if err != nil {
		return fmt.Errorf("migrate storage: %w", err)
	}
	if version > CurrentSchemaVersion {
		return unsupportedVersionError(version)
	}

	for _, migration := range migrations {
		if migration.version <= version {
			continue
		}
		for _, statement := range migration.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate storage to version %d: %w", migration.version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO migrations(version, name, applied_at) VALUES (?, ?, ?)`,
			migration.version, migration.name, formatTimestamp(s.now())); err != nil {
			return fmt.Errorf("record storage migration %d: %w", migration.version, err)
		}
	}
	if err := checkRequiredTables(ctx, tx); err != nil {
		return fmt.Errorf("migrate storage: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate storage: commit: %w", err)
	}
	return nil
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'migrations'`).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}
	return querySchemaVersion(ctx, db)
}

func txSchemaVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	return querySchemaVersion(ctx, tx)
}

type schemaVersionQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func querySchemaVersion(ctx context.Context, q schemaVersionQuerier) (int, error) {
	var version int
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

func checkRequiredTables(ctx context.Context, q schemaVersionQuerier) error {
	for _, table := range requiredTables {
		var count int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			return fmt.Errorf("inspect required table %s: %w", table, err)
		}
		if count != 1 {
			return fmt.Errorf("schema version %d is missing required table %s", CurrentSchemaVersion, table)
		}
	}
	return nil
}

func integrityCheck(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check failed: %s", result)
	}
	return nil
}

func unsupportedVersionError(version int) error {
	return fmt.Errorf("unsupported storage schema version %d; selected loopcoder supports schema version %d", version, CurrentSchemaVersion)
}

func normalizeNow(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
