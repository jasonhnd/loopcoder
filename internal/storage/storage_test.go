package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesFreshDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "loopcoder.db")

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if got := store.Path(); got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}
	health, err := store.Health(ctx)
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if !health.Exists || !health.OK || health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("health = %#v, want existing healthy schema %d", health, CurrentSchemaVersion)
	}
	for _, table := range []string{"migrations", "projects", "runs", "run_events", "run_edges", "reports"} {
		if !tableExists(t, store, table) {
			t.Fatalf("missing table %s", table)
		}
	}
}

func TestOpenMigratesExistingEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := raw.PingContext(ctx); err != nil {
		t.Fatalf("create empty database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	health, err := store.Health(ctx)
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if !health.OK || health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("health = %#v, want migrated schema %d", health, CurrentSchemaVersion)
	}
}

func TestOpenMigratesProjectIdentityColumns(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	for _, statement := range []string{
		`CREATE TABLE migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`,
		`CREATE TABLE projects (
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
		`CREATE TABLE runs (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id) ON DELETE SET NULL, parent_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL, issue_number INTEGER, status TEXT NOT NULL DEFAULT '', started_at TEXT, ended_at TEXT, updated_at TEXT NOT NULL)`,
		`CREATE TABLE run_events (id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, sequence INTEGER NOT NULL, ts TEXT NOT NULL, event_type TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}', UNIQUE(run_id, sequence))`,
		`CREATE TABLE run_edges (parent_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, child_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, edge_type TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, PRIMARY KEY(parent_run_id, child_run_id))`,
		`CREATE TABLE reports (id TEXT PRIMARY KEY, run_id TEXT REFERENCES runs(id) ON DELETE SET NULL, role TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', started_at TEXT, ended_at TEXT, payload_json TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (1, 'initial runtime schema', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec v1 fixture statement: %v\n%s", err, statement)
		}
	}
	closeRawDB(t, raw)

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	health, err := store.Health(ctx)
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", health.SchemaVersion, CurrentSchemaVersion)
	}
	for _, column := range []string{"local_path_canonical", "git_root", "remote_url_normalized", "identity_source", "detached_at"} {
		if !projectColumnExists(t, store, column) {
			t.Fatalf("missing migrated projects column %s", column)
		}
	}
}

func TestOpenMigratesLegacyImportForeignKeysWithoutCascades(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV3Schema(t, raw)
	for _, statement := range []string{
		`INSERT INTO migrations(version, name, applied_at) VALUES (4, 'scrub project remote urls', '2026-01-01T00:00:00Z')`,
		`INSERT INTO projects(id, local_path, local_path_canonical, display_name, identity_source, created_at, updated_at) VALUES ('project-1', '/repo', '/repo', 'repo', 'local-path', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO legacy_import_records(id, project_id, record_type, source_path, source_hash, imported_at) VALUES ('record-1', 'project-1', 'event', '.loopcoder/runs/r/events.jsonl', 'hash', '2026-01-01T00:00:00Z')`,
		`INSERT INTO legacy_import_status(project_id, repo_path, started_at, completed_at, status) VALUES ('project-1', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'completed')`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec cascade fixture statement: %v\n%s", err, statement)
		}
	}
	closeRawDB(t, raw)

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	var recordCount, statusCount int
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM legacy_import_records WHERE project_id = 'project-1'`).Scan(&recordCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM legacy_import_status WHERE project_id = 'project-1'`).Scan(&statusCount); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = 'project-1'`)
		if err == nil {
			return errors.New("project delete succeeded; legacy import foreign keys still cascade or do not restrict")
		}
		return nil
	}); err != nil {
		t.Fatalf("verify migrated legacy import rows: %v", err)
	}
	if recordCount != 1 || statusCount != 1 {
		t.Fatalf("legacy rows after migration = records:%d status:%d, want 1/1", recordCount, statusCount)
	}
	if tableSQLContains(t, store, "legacy_import_records", "ON DELETE CASCADE") || tableSQLContains(t, store, "legacy_import_status", "ON DELETE CASCADE") {
		t.Fatalf("legacy import schema still contains ON DELETE CASCADE")
	}
}

func TestOpenMigratesCredentialBearingRemoteURLs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	secret := "loopcoder-sentinel-secret-687"
	raw := createRawDB(t, path)
	createV3Schema(t, raw)
	if _, err := raw.ExecContext(ctx, `INSERT INTO projects(id, remote_url, remote_url_normalized, local_path, local_path_canonical, display_name, identity_source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"project-1",
		"https://alice:"+secret+"@github.com/Owner/Repo.git?access_token="+secret+"#token="+secret,
		"https://alice:"+secret+"@github.com/Owner/Repo.git?access_token="+secret,
		"/repo",
		"/repo",
		"Repo",
		"github",
		"2026-01-01T00:00:00Z",
		"2026-01-01T00:00:00Z",
	); err != nil {
		t.Fatalf("insert project fixture: %v", err)
	}
	closeRawDB(t, raw)

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	var remoteURL, normalized string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT remote_url, remote_url_normalized FROM projects WHERE id = ?`, "project-1").Scan(&remoteURL, &normalized)
	}); err != nil {
		t.Fatalf("query migrated project: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}
	if remoteURL != "https://github.com/Owner/Repo" || normalized != "https://github.com/Owner/Repo" {
		t.Fatalf("migrated remote urls = %q %q, want scrubbed", remoteURL, normalized)
	}
	if strings.Contains(remoteURL+normalized, secret) {
		t.Fatalf("migrated remote urls contain secret: %q %q", remoteURL, normalized)
	}

	store, err = Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("second Open returned error: %v", err)
	}
	defer store.Close()
	health, err := store.Health(ctx)
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if !health.OK || health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("health = %#v, want idempotent migrated schema %d", health, CurrentSchemaVersion)
	}
}

func TestOpenFailsForUnsupportedSchemaVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	if _, err := raw.ExecContext(ctx, `CREATE TABLE migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create migrations: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO migrations(version, name, applied_at) VALUES (999, 'future', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert migration: %v", err)
	}
	closeRawDB(t, raw)

	_, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err == nil {
		t.Fatal("Open returned nil error, want unsupported version")
	}
	for _, want := range []string{"unsupported storage schema version 999", fmt.Sprintf("supports schema version %d", CurrentSchemaVersion)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want containing %q", err.Error(), want)
		}
	}
}

func TestOpenFailsWhenVersionedSchemaIsIncomplete(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	if _, err := raw.ExecContext(ctx, `CREATE TABLE migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("create migrations: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO migrations(version, name, applied_at) VALUES (?, ?, ?)`,
		CurrentSchemaVersion, "claimed", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert migration: %v", err)
	}
	closeRawDB(t, raw)

	_, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err == nil {
		t.Fatal("Open returned nil error, want incomplete schema error")
	}
	for _, want := range []string{"migrate storage", "missing required table projects"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want containing %q", err.Error(), want)
		}
	}
}

func TestOpenFailsForCorruptDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o644); err != nil {
		t.Fatalf("write corrupt database: %v", err)
	}

	_, err := Open(context.Background(), Options{Path: path, Now: fixedNow})
	if err == nil {
		t.Fatal("Open returned nil error, want corrupt database error")
	}
	if !strings.Contains(err.Error(), "migrate storage") && !strings.Contains(err.Error(), "file is not a database") {
		t.Fatalf("error = %q, want actionable storage open error", err.Error())
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	wantErr := errors.New("stop")
	err = store.WithTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			"project-1", "/repo", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		if err != nil {
			t.Fatalf("insert project: %v", err)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTx error = %v, want %v", err, wantErr)
	}

	var count int
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM projects`).Scan(&count)
	}); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if count != 0 {
		t.Fatalf("project count = %d, want rollback to leave 0", count)
	}
}

func TestWithTxCommitsOnSuccess(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.WithTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			"project-1", "/repo", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		return err
	}); err != nil {
		t.Fatalf("WithTx insert: %v", err)
	}

	var count int
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM projects`).Scan(&count)
	}); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if count != 1 {
		t.Fatalf("project count = %d, want 1", count)
	}
}

func TestCheckHealthReportsMissingDatabaseWithoutCreatingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loopcoder.db")

	health, err := CheckHealth(context.Background(), path)
	if err != nil {
		t.Fatalf("CheckHealth returned error: %v", err)
	}
	if health.Exists || health.OK || !strings.Contains(health.Message, "not been created") {
		t.Fatalf("health = %#v, want missing non-healthy database", health)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database was created during health check or unexpected stat error: %v", err)
	}
}

func tableExists(t *testing.T, store Store, table string) bool {
	t.Helper()
	var count int
	if err := store.WithTx(context.Background(), func(tx Tx) error {
		return tx.QueryRow(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count)
	}); err != nil {
		t.Fatalf("query table %s: %v", table, err)
	}
	return count == 1
}

func projectColumnExists(t *testing.T, store Store, column string) bool {
	t.Helper()
	found := false
	if err := store.WithTx(context.Background(), func(tx Tx) error {
		rows, err := tx.Query(context.Background(), `PRAGMA table_info(projects)`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cid int
			var name string
			var typ string
			var notNull int
			var defaultValue any
			var pk int
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				return err
			}
			if name == column {
				found = true
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("query projects columns: %v", err)
	}
	return found
}

func tableSQLContains(t *testing.T, store Store, table, fragment string) bool {
	t.Helper()
	var sqlText string
	if err := store.WithTx(context.Background(), func(tx Tx) error {
		return tx.QueryRow(context.Background(), `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&sqlText)
	}); err != nil {
		t.Fatalf("query table SQL for %s: %v", table, err)
	}
	return strings.Contains(sqlText, fragment)
}

func createRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	raw, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := raw.PingContext(context.Background()); err != nil {
		t.Fatalf("raw ping: %v", err)
	}
	return raw
}

func closeRawDB(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}
}

func createV3Schema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`CREATE TABLE migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`,
		`CREATE TABLE projects (
			id TEXT PRIMARY KEY,
			remote_url TEXT NOT NULL DEFAULT '',
			github_owner TEXT NOT NULL DEFAULT '',
			github_name TEXT NOT NULL DEFAULT '',
			local_path TEXT NOT NULL,
			default_branch TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			local_path_canonical TEXT NOT NULL DEFAULT '',
			git_root TEXT NOT NULL DEFAULT '',
			remote_url_normalized TEXT NOT NULL DEFAULT '',
			identity_source TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX idx_projects_local_path ON projects(local_path)`,
		`CREATE INDEX idx_projects_local_path_canonical ON projects(local_path_canonical)`,
		`CREATE INDEX idx_projects_remote_url_normalized ON projects(remote_url_normalized)`,
		`CREATE TABLE runs (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id) ON DELETE SET NULL, parent_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL, issue_number INTEGER, status TEXT NOT NULL DEFAULT '', started_at TEXT, ended_at TEXT, updated_at TEXT NOT NULL)`,
		`CREATE INDEX idx_runs_project_id ON runs(project_id)`,
		`CREATE INDEX idx_runs_parent_run_id ON runs(parent_run_id)`,
		`CREATE TABLE run_events (id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, sequence INTEGER NOT NULL, ts TEXT NOT NULL, event_type TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}', UNIQUE(run_id, sequence))`,
		`CREATE INDEX idx_run_events_run_id_sequence ON run_events(run_id, sequence)`,
		`CREATE TABLE run_edges (parent_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, child_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, edge_type TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, PRIMARY KEY(parent_run_id, child_run_id))`,
		`CREATE TABLE reports (id TEXT PRIMARY KEY, run_id TEXT REFERENCES runs(id) ON DELETE SET NULL, role TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', started_at TEXT, ended_at TEXT, payload_json TEXT NOT NULL, created_at TEXT NOT NULL, project_id TEXT NOT NULL DEFAULT '', source_path TEXT NOT NULL DEFAULT '', source_hash TEXT NOT NULL DEFAULT '', source_kind TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX idx_reports_run_id ON reports(run_id)`,
		`CREATE INDEX idx_reports_project_id ON reports(project_id)`,
		`CREATE INDEX idx_reports_source_hash ON reports(source_hash)`,
		`CREATE TABLE legacy_import_records (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, run_id TEXT, record_type TEXT NOT NULL, source_path TEXT NOT NULL, source_line INTEGER NOT NULL DEFAULT 0, source_hash TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}', imported_at TEXT NOT NULL)`,
		`CREATE UNIQUE INDEX idx_legacy_import_records_source ON legacy_import_records(project_id, record_type, source_path, source_line, source_hash)`,
		`CREATE INDEX idx_legacy_import_records_project ON legacy_import_records(project_id)`,
		`CREATE TABLE legacy_import_status (project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE, repo_path TEXT NOT NULL, started_at TEXT NOT NULL, completed_at TEXT NOT NULL, status TEXT NOT NULL, scanned_count INTEGER NOT NULL DEFAULT 0, imported_count INTEGER NOT NULL DEFAULT 0, skipped_count INTEGER NOT NULL DEFAULT 0, malformed_count INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (1, 'initial runtime schema', '2026-01-01T00:00:00Z')`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (2, 'project identity fields', '2026-01-01T00:00:00Z')`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (3, 'legacy local state import metadata', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec v3 fixture statement: %v\n%s", err, statement)
		}
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}
