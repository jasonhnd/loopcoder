package storage

import (
	"context"
	"database/sql"
	"errors"
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
	for _, want := range []string{"unsupported storage schema version 999", "supports schema version 1"} {
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

func fixedNow() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}
