package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
}

func TestOpenCreatesFoundationSchemaAndPermissions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "loopcoder-store.db")

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if got := store.Path(); got != path {
		t.Fatalf("Path() = %q, want %q", got, path)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", version, CurrentSchemaVersion)
	}
	meta, err := store.Metadata(ctx)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if meta.FormatIdentity != FormatIdentity {
		t.Fatalf("FormatIdentity = %q, want %q", meta.FormatIdentity, FormatIdentity)
	}
	if meta.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("metadata schema = %d, want %d", meta.SchemaVersion, CurrentSchemaVersion)
	}
	if meta.CompatibilityFloor != CompatibilityFloor {
		t.Fatalf("compatibility floor = %d, want %d", meta.CompatibilityFloor, CompatibilityFloor)
	}
	if meta.StoreID == "" {
		t.Fatal("expected non-empty store id")
	}
	if !meta.CreatedAt.Equal(fixedNow()) {
		t.Fatalf("created_at = %v, want %v", meta.CreatedAt, fixedNow())
	}
	if meta.LastSuccessfulMigration != CurrentSchemaVersion {
		t.Fatalf("last successful migration = %d, want %d", meta.LastSuccessfulMigration, CurrentSchemaVersion)
	}
	if err := store.CheckIntegrity(ctx); err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	}

	assertTableExists(t, store, "store_metadata")
	assertTableExists(t, store, "migration_ledger")
	for _, domainTable := range []string{
		"projects",
		"work_items",
		"dependencies",
		"jobs",
		"attempts",
		"events",
		"provider_snapshots",
		"route_decisions",
		"verifications",
		"runs",
		"run_events",
	} {
		if tableExists(t, store, domainTable) {
			t.Fatalf("domain table %s must not be created by foundation open", domainTable)
		}
	}

	var migrationID, checksum, verification string
	var sourceVersion, targetVersion int
	if err := store.db.QueryRowContext(ctx, `SELECT migration_id, checksum, source_version, target_version, verification_result
		FROM migration_ledger WHERE version = ?`, CurrentSchemaVersion).Scan(
		&migrationID, &checksum, &sourceVersion, &targetVersion, &verification,
	); err != nil {
		t.Fatalf("query migration ledger: %v", err)
	}
	if migrationID != foundationMigrationID {
		t.Fatalf("migration id = %q, want %q", migrationID, foundationMigrationID)
	}
	if checksum != foundationChecksum() {
		t.Fatalf("checksum = %q, want %q", checksum, foundationChecksum())
	}
	if sourceVersion != 0 || targetVersion != CurrentSchemaVersion || verification != "verified" {
		t.Fatalf("ledger row = source=%d target=%d verification=%q", sourceVersion, targetVersion, verification)
	}
}

func TestOpenIsIdempotentForCurrentSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")

	first, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	firstMeta, err := first.Metadata(ctx)
	if err != nil {
		t.Fatalf("first Metadata: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	second, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer second.Close()
	secondMeta, err := second.Metadata(ctx)
	if err != nil {
		t.Fatalf("second Metadata: %v", err)
	}
	if firstMeta.StoreID != secondMeta.StoreID {
		t.Fatalf("store id changed across reopen: %q -> %q", firstMeta.StoreID, secondMeta.StoreID)
	}
	if secondMeta.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("reopened schema = %d, want %d", secondMeta.SchemaVersion, CurrentSchemaVersion)
	}

	var ledgerCount int
	if err := second.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM migration_ledger`).Scan(&ledgerCount); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledgerCount != 1 {
		t.Fatalf("migration ledger count = %d, want 1 (no duplicate foundation rows)", ledgerCount)
	}
}

func TestCloseIsIdempotentAndReleasesHandle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
	if _, err := store.SchemaVersion(ctx); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("SchemaVersion after close error = %v, want closed", err)
	}
	if err := store.CheckIntegrity(ctx); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("CheckIntegrity after close error = %v, want closed", err)
	}
	// Reopen proves the previous handle did not leave the file locked forever.
	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("reopened Close: %v", err)
	}
	if err := (*Store)(nil).Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestOpenRejectsForeignSchemaDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "foreign.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE projects (id TEXT PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatalf("create foreign table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close foreign db: %v", err)
	}

	_, err = Open(ctx, Options{Path: path, Now: fixedNow})
	if err == nil || !strings.Contains(err.Error(), "foreign schema") {
		t.Fatalf("Open error = %v, want foreign schema refusal", err)
	}
}

func TestOpenFailsClosedOnUnsupportedNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE store_metadata SET schema_version = ?, last_successful_migration = ? WHERE id = 1`, CurrentSchemaVersion+1, CurrentSchemaVersion+1); err != nil {
		_ = store.Close()
		t.Fatalf("bump metadata: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO migration_ledger(
		version, migration_id, name, checksum, applied_at, source_version, target_version, backup_manifest_pointer, verification_result
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		CurrentSchemaVersion+1,
		"future-migration",
		"future",
		"sha256:deadbeef",
		formatTimestamp(fixedNow()),
		CurrentSchemaVersion,
		CurrentSchemaVersion+1,
		"",
		"verified",
	); err != nil {
		_ = store.Close()
		t.Fatalf("insert future ledger: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = Open(ctx, Options{Path: path, Now: fixedNow})
	if err == nil || !strings.Contains(err.Error(), "unsupported store schema version") {
		t.Fatalf("Open error = %v, want unsupported newer schema", err)
	}
}

func TestIntegrityFailsClosedOnCorruption(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Overwrite the database with non-SQLite bytes while keeping a regular file.
	if err := os.WriteFile(path, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("corrupt database file: %v", err)
	}
	_, err = Open(ctx, Options{Path: path, Now: fixedNow})
	if err == nil {
		t.Fatal("Open succeeded on corrupt database; want fail-closed error")
	}
}

func TestIntegrityFailsClosedOnLedgerGap(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM migration_ledger WHERE version = 1`); err != nil {
		_ = store.Close()
		t.Fatalf("delete ledger row: %v", err)
	}
	err = store.CheckIntegrity(ctx)
	_ = store.Close()
	if err == nil || !strings.Contains(err.Error(), "migration ledger") {
		t.Fatalf("CheckIntegrity error = %v, want migration ledger failure", err)
	}
}

func TestOpenRequiresPath(t *testing.T) {
	_, err := Open(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("Open error = %v, want path required", err)
	}
}

func assertTableExists(t *testing.T, store *Store, name string) {
	t.Helper()
	if !tableExists(t, store, name) {
		t.Fatalf("missing table %s", name)
	}
}

func tableExists(t *testing.T, store *Store, name string) bool {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("inspect table %s: %v", name, err)
	}
	return count == 1
}
