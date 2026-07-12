package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	for _, table := range []string{"migrations", "projects", "runs", "run_events", "run_edges", "reports", "child_plans", "run_claims", "usage_records", "usage_reconciliations", "budget_policies", "budget_reservations", "budget_aggregates", "quota_budget_events"} {
		if !tableExists(t, store, table) {
			t.Fatalf("missing table %s", table)
		}
	}
}

func TestOpenMigratesNestedGraphSchemaFromV6(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV3Schema(t, raw)
	for _, statement := range []string{
		`INSERT INTO migrations(version, name, applied_at) VALUES (4, 'scrub project remote urls', '2026-01-01T00:00:00Z')`,
		`ALTER TABLE projects ADD COLUMN detached_at TEXT NOT NULL DEFAULT ''`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (5, 'preserve project history on registry removal', '2026-01-01T00:00:00Z')`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (6, 'reconcile physical project identities', '2026-01-01T00:00:00Z')`,
		`INSERT INTO runs(id, status, started_at, updated_at) VALUES ('run-20260709T000000Z-wave', 'running', '2026-07-09T00:00:00Z', '2026-07-09T00:00:00Z')`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec v6 fixture statement: %v\n%s", err, statement)
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
	for _, column := range []string{"root_run_id", "depth", "origin", "created_at"} {
		if !tableColumnExists(t, store, "runs", column) {
			t.Fatalf("missing migrated runs column %s", column)
		}
	}
	for _, column := range []string{"root_run_id", "plan_id", "child_key", "ordinal", "scope_json", "aggregation_json", "status", "updated_at"} {
		if !tableColumnExists(t, store, "run_edges", column) {
			t.Fatalf("missing migrated run_edges column %s", column)
		}
	}
	if !tableExists(t, store, "child_plans") {
		t.Fatalf("missing child_plans table")
	}
	if !tableExists(t, store, "run_claims") {
		t.Fatalf("missing run_claims table")
	}
	for _, column := range []string{"phase", "provider_idempotency_key", "provider_receipt"} {
		if !tableColumnExists(t, store, "run_claims", column) {
			t.Fatalf("missing migrated run_claims column %s", column)
		}
	}
	var rootRunID string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT root_run_id FROM runs WHERE id = 'run-20260709T000000Z-wave'`).Scan(&rootRunID)
	}); err != nil {
		t.Fatalf("query migrated run: %v", err)
	}
	if rootRunID != "run-20260709T000000Z-wave" {
		t.Fatalf("migrated root_run_id = %q, want self root", rootRunID)
	}
}

func TestOpenMigratesRunClaimsLifecycleColumnsFromV8(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV8NestedClaimsSchema(t, raw)
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
	for _, column := range []string{"phase", "provider_idempotency_key", "provider_receipt"} {
		if !tableColumnExists(t, store, "run_claims", column) {
			t.Fatalf("missing migrated run_claims column %s", column)
		}
	}
	var phase, key, receipt string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT phase, provider_idempotency_key, provider_receipt FROM run_claims WHERE run_id = 'run-child'`).Scan(&phase, &key, &receipt)
	}); err != nil {
		t.Fatalf("query migrated claim: %v", err)
	}
	if phase != ClaimPhaseExecuting || key != "" || receipt != "" {
		t.Fatalf("migrated claim lifecycle fields = %q/%q/%q, want executing/empty/empty", phase, key, receipt)
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

func TestOpenMigratesDuplicatePhysicalProjectIdentities(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliable in unprivileged Windows test runs")
	}
	ctx := context.Background()
	root := t.TempDir()
	physical := filepath.Join(root, "physical", "repo")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatalf("mkdir physical: %v", err)
	}
	aliasRoot := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "physical"), aliasRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	aliasRepo := filepath.Join(aliasRoot, "repo")

	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV3Schema(t, raw)
	for _, statement := range []string{
		`INSERT INTO migrations(version, name, applied_at) VALUES (4, 'scrub project remote urls', '2026-01-01T00:00:00Z')`,
		`ALTER TABLE projects ADD COLUMN detached_at TEXT NOT NULL DEFAULT ''`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (5, 'preserve project history on registry removal', '2026-01-01T00:00:00Z')`,
		`INSERT INTO projects(id, local_path, local_path_canonical, display_name, identity_source, created_at, updated_at, detached_at) VALUES ('proj_alias', '` + aliasRepo + `', '` + aliasRepo + `', 'repo', 'local-path', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '')`,
		`INSERT INTO projects(id, local_path, local_path_canonical, display_name, identity_source, created_at, updated_at, detached_at) VALUES ('proj_physical', '` + physical + `', '` + physical + `', 'repo', 'local-path', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', '')`,
		`INSERT INTO runs(id, project_id, status, updated_at) VALUES ('run-alias', 'proj_alias', 'done', '2026-01-01T00:00:00Z')`,
		`INSERT INTO runs(id, project_id, status, updated_at) VALUES ('run-physical', 'proj_physical', 'done', '2026-01-01T00:00:00Z')`,
		`INSERT INTO reports(id, project_id, run_id, role, provider, model, payload_json, created_at) VALUES ('report-alias', 'proj_alias', 'run-alias', 'worker', 'codex', 'gpt-test', '{}', '2026-01-01T00:00:00Z')`,
		`INSERT INTO reports(id, project_id, run_id, role, provider, model, payload_json, created_at) VALUES ('report-physical', 'proj_physical', 'run-physical', 'worker', 'codex', 'gpt-test', '{}', '2026-01-01T00:00:00Z')`,
		`INSERT INTO legacy_import_records(id, project_id, record_type, source_path, source_hash, imported_at) VALUES ('legacy-alias', 'proj_alias', 'event', '.loopcoder/runs/a/events.jsonl', 'hash-a', '2026-01-01T00:00:00Z')`,
		`INSERT INTO legacy_import_records(id, project_id, record_type, source_path, source_hash, imported_at) VALUES ('legacy-physical', 'proj_physical', 'event', '.loopcoder/runs/b/events.jsonl', 'hash-b', '2026-01-01T00:00:00Z')`,
		`INSERT INTO legacy_import_status(project_id, repo_path, started_at, completed_at, status, scanned_count, imported_count) VALUES ('proj_alias', '` + aliasRepo + `', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'completed', 1, 1)`,
		`INSERT INTO legacy_import_status(project_id, repo_path, started_at, completed_at, status, scanned_count, imported_count) VALUES ('proj_physical', '` + physical + `', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'completed', 1, 1)`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec duplicate fixture statement: %v\n%s", err, statement)
		}
	}
	closeRawDB(t, raw)

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	assertCount := func(query string, args ...any) int {
		t.Helper()
		var count int
		if err := store.WithTx(ctx, func(tx Tx) error {
			return tx.QueryRow(ctx, query, args...).Scan(&count)
		}); err != nil {
			t.Fatalf("count query %q: %v", query, err)
		}
		return count
	}
	if got := assertCount(`SELECT COUNT(*) FROM projects`); got != 1 {
		t.Fatalf("project count = %d, want 1", got)
	}
	if got := assertCount(`SELECT COUNT(*) FROM runs WHERE project_id = 'proj_alias'`); got != 2 {
		t.Fatalf("merged run count = %d, want 2", got)
	}
	if got := assertCount(`SELECT COUNT(*) FROM reports WHERE project_id = 'proj_alias'`); got != 2 {
		t.Fatalf("merged report count = %d, want 2", got)
	}
	if got := assertCount(`SELECT COUNT(*) FROM legacy_import_records WHERE project_id = 'proj_alias'`); got != 2 {
		t.Fatalf("merged legacy record count = %d, want 2", got)
	}
	if got := assertCount(`SELECT scanned_count FROM legacy_import_status WHERE project_id = 'proj_alias'`); got != 2 {
		t.Fatalf("merged import status scanned_count = %d, want 2", got)
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

func TestPersistChildPlanGraphRejectsCyclesAndInconsistentRunGraph(t *testing.T) {
	tests := []struct {
		name string
		seed func(context.Context, Store)
		edit func(*RunNode, []RunNode, *ChildPlanRecord, []RunEdgeRecord)
		want string
	}{
		{
			name: "parent_child_self_cycle",
			edit: func(parent *RunNode, children []RunNode, plan *ChildPlanRecord, edges []RunEdgeRecord) {
				children[0].RunID = parent.RunID
				children[0].ParentRunID = parent.RunID
				edges[0].ChildRunID = parent.RunID
			},
			want: "cannot reuse parent run id",
		},
		{
			name: "child_reuses_root_ancestor",
			seed: func(ctx context.Context, store Store) {
				seedRunGraphRows(t, ctx, store, []RunNode{
					{RunID: "run-root", RootRunID: "run-root", Depth: 0, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
					{RunID: "run-parent", ParentRunID: "run-root", RootRunID: "run-root", Depth: 1, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
				}, []RunEdgeRecord{
					{ParentRunID: "run-root", ChildRunID: "run-parent", RootRunID: "run-root", Depth: 1, PlanID: "seed-plan", ChildKey: "parent", Ordinal: 0, ScopeJSON: "{}", AggregationJSON: "{}", Permission: "write", Status: "succeeded", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
				})
			},
			edit: func(parent *RunNode, children []RunNode, plan *ChildPlanRecord, edges []RunEdgeRecord) {
				parent.RunID = "run-parent"
				parent.ParentRunID = "run-root"
				parent.RootRunID = "run-root"
				parent.Depth = 1
				plan.ParentRunID = "run-parent"
				plan.RootRunID = "run-root"
				children[0].RunID = "run-root"
				children[0].ParentRunID = "run-parent"
				children[0].RootRunID = "run-root"
				children[0].Depth = 2
				edges[0].ParentRunID = "run-parent"
				edges[0].ChildRunID = "run-root"
				edges[0].RootRunID = "run-root"
				edges[0].Depth = 2
			},
			want: "cannot reuse root run id",
		},
		{
			name: "multi_level_cycle",
			seed: func(ctx context.Context, store Store) {
				seedRunGraphRows(t, ctx, store, []RunNode{
					{RunID: "run-root", RootRunID: "run-root", Depth: 0, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
					{RunID: "run-a", ParentRunID: "run-root", RootRunID: "run-root", Depth: 1, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
					{RunID: "run-b", ParentRunID: "run-a", RootRunID: "run-root", Depth: 2, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
				}, []RunEdgeRecord{
					{ParentRunID: "run-root", ChildRunID: "run-a", RootRunID: "run-root", Depth: 1, PlanID: "seed-plan-a", ChildKey: "a", Ordinal: 0, ScopeJSON: "{}", AggregationJSON: "{}", Permission: "write", Status: "succeeded", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
					{ParentRunID: "run-a", ChildRunID: "run-b", RootRunID: "run-root", Depth: 2, PlanID: "seed-plan-b", ChildKey: "b", Ordinal: 0, ScopeJSON: "{}", AggregationJSON: "{}", Permission: "write", Status: "succeeded", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
				})
			},
			edit: func(parent *RunNode, children []RunNode, plan *ChildPlanRecord, edges []RunEdgeRecord) {
				parent.RunID = "run-b"
				parent.ParentRunID = "run-a"
				parent.RootRunID = "run-root"
				parent.Depth = 2
				plan.ParentRunID = "run-b"
				plan.RootRunID = "run-root"
				children[0].RunID = "run-a"
				children[0].ParentRunID = "run-b"
				children[0].RootRunID = "run-root"
				children[0].Depth = 3
				edges[0].ParentRunID = "run-b"
				edges[0].ChildRunID = "run-a"
				edges[0].RootRunID = "run-root"
				edges[0].Depth = 3
			},
			want: "cannot reuse ancestor run id",
		},
		{
			name: "root_mismatch",
			seed: func(ctx context.Context, store Store) {
				seedRunGraphRows(t, ctx, store, []RunNode{
					{RunID: "run-parent", RootRunID: "run-parent", Depth: 0, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
				}, nil)
			},
			edit: func(parent *RunNode, children []RunNode, plan *ChildPlanRecord, edges []RunEdgeRecord) {
				parent.RootRunID = "run-other-root"
				plan.RootRunID = "run-other-root"
				children[0].RootRunID = "run-other-root"
				edges[0].RootRunID = "run-other-root"
			},
			want: "root mismatch",
		},
		{
			name: "depth_mismatch",
			seed: func(ctx context.Context, store Store) {
				seedRunGraphRows(t, ctx, store, []RunNode{
					{RunID: "run-parent", RootRunID: "run-parent", Depth: 1, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
				}, nil)
			},
			want: "depth mismatch",
		},
		{
			name: "existing_child_belongs_to_another_parent",
			seed: func(ctx context.Context, store Store) {
				seedRunGraphRows(t, ctx, store, []RunNode{
					{RunID: "run-other-parent", RootRunID: "run-other-parent", Depth: 0, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
					{RunID: "run-child", ParentRunID: "run-other-parent", RootRunID: "run-other-parent", Depth: 1, Status: "queued", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
				}, []RunEdgeRecord{
					{ParentRunID: "run-other-parent", ChildRunID: "run-child", RootRunID: "run-other-parent", Depth: 1, PlanID: "seed-plan", ChildKey: "seed-child", Ordinal: 0, ScopeJSON: "{}", AggregationJSON: "{}", Permission: "write", Status: "queued", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
				})
			},
			want: "parent mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
			if err != nil {
				t.Fatalf("Open returned error: %v", err)
			}
			defer store.Close()
			if tt.seed != nil {
				tt.seed(ctx, store)
			}
			parent, children, plan, edges := validChildPlanGraphFixture()
			if tt.edit != nil {
				tt.edit(&parent, children, &plan, edges)
			}
			err = PersistChildPlanGraph(ctx, store, parent, children, plan, edges)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("PersistChildPlanGraph error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestPersistChildPlanGraphRejectsInvalidGraphWithoutPartialRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	parent, children, plan, edges := validChildPlanGraphFixture()
	children[0].RunID = parent.RunID
	edges[0].ChildRunID = parent.RunID
	err = PersistChildPlanGraph(ctx, store, parent, children, plan, edges)
	if err == nil {
		t.Fatal("PersistChildPlanGraph returned nil error, want self-cycle rejection")
	}
	var planCount, edgeCount, runCount int
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM child_plans`).Scan(&planCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_edges`).Scan(&edgeCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runCount)
	}); err != nil {
		t.Fatalf("query counts: %v", err)
	}
	if planCount != 0 || edgeCount != 0 || runCount != 0 {
		t.Fatalf("partial rows plans/edges/runs = %d/%d/%d, want 0/0/0", planCount, edgeCount, runCount)
	}
}

func TestTransitionRunStatusValidatesAndRecordsHistory(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	parent, children, plan, edges := validChildPlanGraphFixture()
	if err := PersistChildPlanGraph(ctx, store, parent, children, plan, edges); err != nil {
		t.Fatalf("PersistChildPlanGraph: %v", err)
	}

	if err := TransitionChildRunStatus(ctx, store, parent.RunID, children[0].RunID, "running", "2026-07-10T00:00:01Z", "launch"); err != nil {
		t.Fatalf("TransitionChildRunStatus running: %v", err)
	}
	if err := TransitionChildRunStatus(ctx, store, parent.RunID, children[0].RunID, "succeeded", "2026-07-10T00:00:02Z", "finished"); err != nil {
		t.Fatalf("TransitionChildRunStatus succeeded: %v", err)
	}
	if err := TransitionParentRunStatus(ctx, store, parent.RunID, "succeeded_with_optional_failures", "2026-07-10T00:00:03Z", "aggregate"); err != nil {
		t.Fatalf("TransitionParentRunStatus optional failures: %v", err)
	}
	err = TransitionRunStatus(ctx, store, RunStatusTransition{RunID: parent.RunID, Status: "running", UpdatedAt: "2026-07-10T00:00:04Z"})
	if err == nil || !strings.Contains(err.Error(), "succeeded_with_optional_failures -> running") {
		t.Fatalf("terminal transition error = %v, want invalid optional-failures -> running", err)
	}

	var childStatus, edgeStatus, parentStatus, parentEndedAt string
	var eventCount int
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = ?`, children[0].RunID).Scan(&childStatus); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, parent.RunID, children[0].RunID).Scan(&edgeStatus); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status, COALESCE(ended_at, '') FROM runs WHERE id = ?`, parent.RunID).Scan(&parentStatus, &parentEndedAt); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_events WHERE run_id IN (?, ?)`, parent.RunID, children[0].RunID).Scan(&eventCount)
	}); err != nil {
		t.Fatalf("query transition state: %v", err)
	}
	if childStatus != "succeeded" || edgeStatus != "succeeded" {
		t.Fatalf("child/edge status = %q/%q, want succeeded/succeeded", childStatus, edgeStatus)
	}
	if parentStatus != "succeeded_with_optional_failures" || parentEndedAt == "" {
		t.Fatalf("parent status/ended_at = %q/%q, want optional-failures with ended_at", parentStatus, parentEndedAt)
	}
	if eventCount != 3 {
		t.Fatalf("run_events count = %d, want 3", eventCount)
	}
}

func TestClaimChildRunExecutionClaimsObservesAndFencesCompletion(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	parent, children, plan, edges := validChildPlanGraphFixture()
	if err := PersistChildPlanGraph(ctx, store, parent, children, plan, edges); err != nil {
		t.Fatalf("PersistChildPlanGraph: %v", err)
	}
	childRunID := children[0].RunID
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	claim, err := ClaimChildRunExecution(ctx, store, parent.RunID, childRunID, "executor-a", now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution winner: %v", err)
	}
	if claim.Outcome != ClaimOutcomeClaimed || claim.ClaimGeneration != 1 || claim.ExecutorID != "executor-a" {
		t.Fatalf("claim = %#v, want claimed generation 1 by executor-a", claim)
	}
	loser, err := ClaimChildRunExecution(ctx, store, parent.RunID, childRunID, "executor-b", now.Add(time.Second), now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution loser: %v", err)
	}
	if loser.Outcome != ClaimOutcomeAlreadyRunning || loser.ExecutorID != "executor-a" || loser.ClaimGeneration != 1 {
		t.Fatalf("loser claim = %#v, want already-running owner executor-a generation 1", loser)
	}
	stale, err := ClaimChildRunExecution(ctx, store, parent.RunID, childRunID, "executor-c", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution stale takeover: %v", err)
	}
	if stale.Outcome != ClaimOutcomeStaleClaim || stale.ClaimGeneration != 2 || stale.PreviousOwner != "executor-a" || stale.ExecutorID != "executor-c" {
		t.Fatalf("stale claim = %#v, want takeover generation 2 from executor-a to executor-c", stale)
	}
	if stale.ProviderKey == "" || stale.ProviderKey != claim.ProviderKey {
		t.Fatalf("stale provider key = %q, first key = %q, want stable logical child key across generations", stale.ProviderKey, claim.ProviderKey)
	}
	err = CompleteClaimedChildRun(ctx, store, parent.RunID, childRunID, "executor-a", 1, "succeeded", "2026-07-10T00:03:01Z", "stale completion", "")
	if err == nil || !strings.Contains(err.Error(), "stale claim") {
		t.Fatalf("stale completion error = %v, want stale claim rejection", err)
	}
	if err := CompleteClaimedChildRun(ctx, store, parent.RunID, childRunID, "executor-c", 2, "succeeded", "2026-07-10T00:03:02Z", "winner completion", "provider-receipt-1"); err != nil {
		t.Fatalf("CompleteClaimedChildRun winner: %v", err)
	}
	var receipt string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_receipt FROM run_claims WHERE run_id = ?`, childRunID).Scan(&receipt)
	}); err != nil {
		t.Fatalf("query provider receipt: %v", err)
	}
	if receipt != "provider-receipt-1" {
		t.Fatalf("provider_receipt = %q, want real receipt", receipt)
	}
	reused, err := ClaimChildRunExecution(ctx, store, parent.RunID, childRunID, "executor-d", now.Add(4*time.Minute), now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution terminal: %v", err)
	}
	if reused.Outcome != ClaimOutcomeTerminalReused {
		t.Fatalf("terminal claim = %#v, want terminal-reused", reused)
	}
}

func TestRenewChildRunClaimExtendsLeaseAndFencesOwner(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	parent, children, plan, edges := validChildPlanGraphFixture()
	if err := PersistChildPlanGraph(ctx, store, parent, children, plan, edges); err != nil {
		t.Fatalf("PersistChildPlanGraph: %v", err)
	}
	childRunID := children[0].RunID
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	claim, err := ClaimChildRunExecution(ctx, store, parent.RunID, childRunID, "executor-a", now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution: %v", err)
	}
	renewedUntil := now.Add(4 * time.Minute)
	if err := RenewChildRunClaim(ctx, store, childRunID, claim.ExecutorID, claim.ClaimGeneration, now.Add(30*time.Second), renewedUntil); err != nil {
		t.Fatalf("RenewChildRunClaim: %v", err)
	}
	loser, err := ClaimChildRunExecution(ctx, store, parent.RunID, childRunID, "executor-b", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution loser: %v", err)
	}
	if loser.Outcome != ClaimOutcomeAlreadyRunning || loser.LeaseExpiresAt != formatTimestamp(renewedUntil) {
		t.Fatalf("loser claim = %#v, want active renewed owner through %s", loser, formatTimestamp(renewedUntil))
	}
	err = RenewChildRunClaim(ctx, store, childRunID, "executor-b", claim.ClaimGeneration, now.Add(time.Minute), now.Add(5*time.Minute))
	if !IsStaleChildRunClaim(err) {
		t.Fatalf("stale renew error = %v, want ErrStaleChildRunClaim", err)
	}
}

func TestClaimChildRunExecutionExpiredExecutingClaimNeedsHuman(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	parent, children, plan, edges := validChildPlanGraphFixture()
	if err := PersistChildPlanGraph(ctx, store, parent, children, plan, edges); err != nil {
		t.Fatalf("PersistChildPlanGraph: %v", err)
	}
	childRunID := children[0].RunID
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	claim, err := ClaimChildRunExecution(ctx, store, parent.RunID, childRunID, "executor-a", now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution: %v", err)
	}
	if err := UpdateChildRunClaimPhase(ctx, store, parent.RunID, childRunID, claim.ExecutorID, claim.ClaimGeneration, ClaimPhaseExecuting, formatTimestamp(now.Add(time.Second)), ""); err != nil {
		t.Fatalf("UpdateChildRunClaimPhase executing: %v", err)
	}
	blocked, err := ClaimChildRunExecution(ctx, store, parent.RunID, childRunID, "executor-b", now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution expired executing: %v", err)
	}
	if blocked.Outcome != ClaimOutcomeBlocked || blocked.RunStatus != "needs-human" || blocked.PreviousOwner != "executor-a" {
		t.Fatalf("blocked claim = %#v, want needs-human for expired executing owner", blocked)
	}
}

func TestWithWriteTxRollbackFailureDiscardsConnection(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	wantErr := errors.New("force rollback")
	rollbackErr := errors.New("forced rollback failure")
	rollbackConnTxHookForTest = func(*sql.Conn) error {
		return rollbackErr
	}
	defer func() { rollbackConnTxHookForTest = nil }()
	err = store.WithWriteTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('project-rollback', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("insert project before forced rollback: %v", err)
		}
		return wantErr
	})
	if err == nil || !strings.Contains(err.Error(), "rollback after") || !errors.Is(err, rollbackErr) {
		t.Fatalf("WithWriteTx error = %v, want rollback failure", err)
	}
	rollbackConnTxHookForTest = nil

	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('project-after-rollback', '/repo2', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		return err
	}); err != nil {
		t.Fatalf("write after rollback failure: %v", err)
	}
}

func TestCheckHealthRejectsCorruptDurableRunGraph(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	seedRunGraphRows(t, ctx, store, []RunNode{
		{RunID: "run-a", RootRunID: "run-a", Depth: 0, Status: "running", CreatedAt: "2026-07-10T00:00:00Z", UpdatedAt: "2026-07-10T00:00:00Z"},
	}, nil)
	if err := store.WithTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO run_edges(parent_run_id, child_run_id, edge_type, created_at, root_run_id, plan_id, child_key, depth, ordinal, scope_json, permission, aggregation_json, status, updated_at)
			VALUES ('run-a', 'run-a', 'child', '2026-07-10T00:00:00Z', 'run-a', 'corrupt-plan', 'self', 0, 0, '{}', 'write', '{}', 'queued', '2026-07-10T00:00:00Z')`)
		return err
	}); err != nil {
		t.Fatalf("seed corrupt self-edge: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	health, err := CheckHealth(ctx, path)
	if err == nil {
		t.Fatalf("CheckHealth returned nil error, health=%#v", health)
	}
	if !strings.Contains(err.Error(), "self-edge") {
		t.Fatalf("CheckHealth error = %v, want self-edge diagnostic", err)
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
	return tableColumnExists(t, store, "projects", column)
}

func tableColumnExists(t *testing.T, store Store, table, column string) bool {
	t.Helper()
	found := false
	if err := store.WithTx(context.Background(), func(tx Tx) error {
		rows, err := tx.Query(context.Background(), `PRAGMA table_info(`+table+`)`)
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
		t.Fatalf("query %s columns: %v", table, err)
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

func createV8NestedClaimsSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	createV3Schema(t, db)
	for _, statement := range []string{
		`INSERT INTO migrations(version, name, applied_at) VALUES (4, 'scrub project remote urls', '2026-01-01T00:00:00Z')`,
		`ALTER TABLE projects ADD COLUMN detached_at TEXT NOT NULL DEFAULT ''`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (5, 'preserve project history on registry removal', '2026-01-01T00:00:00Z')`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (6, 'reconcile physical project identities', '2026-01-01T00:00:00Z')`,
		`ALTER TABLE runs ADD COLUMN root_run_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN depth INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN origin TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN created_at TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_runs_root_run_id ON runs(root_run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_depth ON runs(depth)`,
		`CREATE TABLE child_plans (
			plan_id TEXT PRIMARY KEY,
			parent_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
			root_run_id TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			max_depth INTEGER NOT NULL,
			max_concurrency INTEGER NOT NULL,
			plan_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_child_plans_parent_run_id ON child_plans(parent_run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_child_plans_root_run_id ON child_plans(root_run_id)`,
		`ALTER TABLE run_edges ADD COLUMN root_run_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE run_edges ADD COLUMN plan_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE run_edges ADD COLUMN child_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE run_edges ADD COLUMN depth INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE run_edges ADD COLUMN ordinal INTEGER NOT NULL DEFAULT -1`,
		`ALTER TABLE run_edges ADD COLUMN scope_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE run_edges ADD COLUMN permission TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE run_edges ADD COLUMN aggregation_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE run_edges ADD COLUMN status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE run_edges ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_run_edges_root_run_id ON run_edges(root_run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_run_edges_plan_id ON run_edges(plan_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_run_edges_plan_child_key ON run_edges(plan_id, child_key) WHERE plan_id <> '' AND child_key <> ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_run_edges_parent_ordinal ON run_edges(parent_run_id, ordinal) WHERE ordinal >= 0`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (7, 'nested child plans and durable run graph', '2026-01-01T00:00:00Z')`,
		`CREATE TABLE run_claims (
			run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
			executor_id TEXT NOT NULL,
			claim_generation INTEGER NOT NULL,
			claimed_at TEXT NOT NULL,
			lease_expires_at TEXT NOT NULL,
			heartbeat_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_run_claims_executor_id ON run_claims(executor_id)`,
		`CREATE INDEX IF NOT EXISTS idx_run_claims_lease_expires_at ON run_claims(lease_expires_at)`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (8, 'nested child execution claims', '2026-01-01T00:00:00Z')`,
		`INSERT INTO runs(id, status, started_at, updated_at, root_run_id, depth, origin, created_at) VALUES ('run-parent', 'running', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'run-parent', 0, 'nested_parent', '2026-01-01T00:00:00Z')`,
		`INSERT INTO runs(id, parent_run_id, status, started_at, updated_at, root_run_id, depth, origin, created_at) VALUES ('run-child', 'run-parent', 'running', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'run-parent', 1, 'sub_agent', '2026-01-01T00:00:00Z')`,
		`INSERT INTO run_edges(parent_run_id, child_run_id, edge_type, created_at, root_run_id, plan_id, child_key, depth, ordinal, scope_json, permission, aggregation_json, status, updated_at) VALUES ('run-parent', 'run-child', 'child', '2026-01-01T00:00:00Z', 'run-parent', 'plan-parent', 'child', 1, 0, '{}', 'write', '{}', 'running', '2026-01-01T00:00:00Z')`,
		`INSERT INTO run_claims(run_id, executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at) VALUES ('run-child', 'executor-a', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:30:00Z', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec v8 fixture statement: %v\n%s", err, statement)
		}
	}
}

func validChildPlanGraphFixture() (RunNode, []RunNode, ChildPlanRecord, []RunEdgeRecord) {
	at := "2026-07-10T00:00:00Z"
	parent := RunNode{
		RunID:     "run-parent",
		RootRunID: "run-parent",
		Depth:     0,
		Origin:    "nested_parent",
		Status:    "running",
		CreatedAt: at,
		UpdatedAt: at,
	}
	children := []RunNode{{
		RunID:       "run-child",
		ParentRunID: "run-parent",
		RootRunID:   "run-parent",
		Depth:       1,
		Origin:      "sub_agent",
		Status:      "queued",
		CreatedAt:   at,
		UpdatedAt:   at,
	}}
	plan := ChildPlanRecord{
		PlanID:         "plan-run-parent",
		ParentRunID:    "run-parent",
		RootRunID:      "run-parent",
		SchemaVersion:  "loopcoder.child_plan.v1",
		MaxDepth:       2,
		MaxConcurrency: 1,
		PlanJSON:       `{"schema_version":"loopcoder.child_plan.v1"}`,
		CreatedAt:      at,
	}
	edges := []RunEdgeRecord{{
		ParentRunID:     "run-parent",
		ChildRunID:      "run-child",
		RootRunID:       "run-parent",
		PlanID:          "plan-run-parent",
		ChildKey:        "child",
		Depth:           1,
		Ordinal:         0,
		ScopeJSON:       "{}",
		Permission:      "write",
		AggregationJSON: "{}",
		Status:          "queued",
		CreatedAt:       at,
		UpdatedAt:       at,
	}}
	return parent, children, plan, edges
}

func seedRunGraphRows(t *testing.T, ctx context.Context, store Store, runs []RunNode, edges []RunEdgeRecord) {
	t.Helper()
	if err := store.WithTx(ctx, func(tx Tx) error {
		for _, run := range runs {
			if err := upsertRunNode(ctx, tx, run); err != nil {
				return err
			}
		}
		for _, edge := range edges {
			if err := upsertRunEdge(ctx, tx, edge); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed run graph rows: %v", err)
	}
}

func fixedNow() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}
