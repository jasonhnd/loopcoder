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
	for _, want := range []string{"unsupported storage schema version 999", "supports schema version 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want containing %q", err.Error(), want)
		}
	}
}

func TestForeignKeysSurviveConnectionRecycling(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	sqlite := store.(*sqliteStore)
	sqlite.db.SetMaxOpenConns(4)
	sqlite.db.SetMaxIdleConns(0)

	err = store.WithTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO runs(id, parent_run_id, status, updated_at, root_run_id, depth, origin)
			VALUES ('child', 'missing-parent', 'planned', '2026-01-01T00:00:00Z', 'missing-parent', 1, 'test')`)
		return err
	})
	if err == nil {
		t.Fatal("insert with missing parent succeeded; want foreign-key failure")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "constraint") && !strings.Contains(strings.ToLower(err.Error()), "foreign") {
		t.Fatalf("error = %q, want foreign-key constraint failure", err.Error())
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

func TestRunLifecycleRejectsInvalidTransitions(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertRun(ctx, RunRecord{ID: "run-test", Origin: "conductor"}); err != nil {
		t.Fatalf("UpsertRun returned error: %v", err)
	}
	_, err = store.TransitionRun(ctx, RunTransition{RunID: "run-test", From: RunStatePlanned, To: RunStateSucceeded})
	if err == nil {
		t.Fatal("TransitionRun returned nil error, want invalid transition")
	}
	if !strings.Contains(err.Error(), `"planned" -> "succeeded"`) {
		t.Fatalf("error = %q, want invalid transition detail", err.Error())
	}
}

func TestRunLifecyclePersistsCurrentStateAfterReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.UpsertRun(ctx, RunRecord{ID: "run-test", Origin: "conductor"}); err != nil {
		t.Fatalf("UpsertRun returned error: %v", err)
	}
	for _, transition := range []RunTransition{
		{RunID: "run-test", From: RunStatePlanned, To: RunStateQueued, At: fixedNow()},
		{RunID: "run-test", From: RunStateQueued, To: RunStateRunning, At: fixedNow().Add(time.Minute)},
		{RunID: "run-test", From: RunStateRunning, To: RunStateSucceeded, At: fixedNow().Add(2 * time.Minute)},
	} {
		if _, err := store.TransitionRun(ctx, transition); err != nil {
			t.Fatalf("TransitionRun(%s -> %s) returned error: %v", transition.From, transition.To, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen returned error: %v", err)
	}
	defer reopened.Close()

	lifecycle, err := reopened.RunLifecycle(ctx, "run-test")
	if err != nil {
		t.Fatalf("RunLifecycle returned error: %v", err)
	}
	if lifecycle.Status != RunStateSucceeded {
		t.Fatalf("Status = %q, want succeeded", lifecycle.Status)
	}
	if len(lifecycle.History) != 3 {
		t.Fatalf("history length = %d, want 3: %#v", len(lifecycle.History), lifecycle.History)
	}
	if lifecycle.History[0].From != RunStatePlanned || lifecycle.History[0].To != RunStateQueued {
		t.Fatalf("first transition = %#v", lifecycle.History[0])
	}
}

func TestRunLifecycleSupportsParentChildRuns(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertRun(ctx, RunRecord{ID: "run-parent", Origin: "conductor"}); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	if err := store.UpsertRun(ctx, RunRecord{ID: "run-child", ParentRunID: "run-parent", Origin: "sub_agent"}); err != nil {
		t.Fatalf("upsert child: %v", err)
	}
	if _, err := store.TransitionRun(ctx, RunTransition{RunID: "run-child", From: RunStatePlanned, To: RunStateRunning}); err != nil {
		t.Fatalf("transition child: %v", err)
	}

	child, err := store.RunLifecycle(ctx, "run-child")
	if err != nil {
		t.Fatalf("RunLifecycle child returned error: %v", err)
	}
	if child.ParentRunID != "run-parent" || child.RootRunID != "run-parent" || child.Depth != 1 || child.Status != RunStateRunning {
		t.Fatalf("child lifecycle = %#v", child)
	}
	var edgeStatus string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`,
			"run-parent", "run-child").Scan(&edgeStatus)
	}); err != nil {
		t.Fatalf("query edge status: %v", err)
	}
	if edgeStatus != string(RunStateRunning) {
		t.Fatalf("edge status = %q, want running", edgeStatus)
	}
}

func TestAggregateRequiredChildStatePrecedence(t *testing.T) {
	got := AggregateRequiredChildState([]RunLifecycle{
		{RunID: "running", Status: RunStateRunning},
		{RunID: "failed", Status: RunStateFailed},
	})
	if got != RunStateNeedsHuman {
		t.Fatalf("AggregateRequiredChildState failed-vs-running = %q, want needs-human", got)
	}

	got = AggregateRequiredChildState([]RunLifecycle{{Status: RunStateSucceeded}, {Status: RunStateSucceeded}})
	if got != RunStateSucceeded {
		t.Fatalf("AggregateRequiredChildState all succeeded = %q, want succeeded", got)
	}
}

func TestImportLegacyRunEventsMapsAndPreservesHistory(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.UpsertRun(ctx, RunRecord{ID: "run-legacy", Origin: "legacy-repo-local"}); err != nil {
		t.Fatalf("UpsertRun returned error: %v", err)
	}
	if err := store.WithTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO run_events(id, run_id, sequence, ts, event_type, payload_json)
			VALUES ('legacy-original', 'run-legacy', 1, '2026-01-01T00:00:00Z', 'worker_event', '{"status":"running"}')`)
		return err
	}); err != nil {
		t.Fatalf("insert original legacy event: %v", err)
	}

	lifecycle, err := store.ImportLegacyRunEvents(ctx, "run-legacy", []LegacyRunEvent{
		{Timestamp: "2026-01-01T00:01:00Z", Status: "running", Phase: "codex_started"},
		{Timestamp: "2026-01-01T00:02:00Z", Status: "succeeded", Phase: "cleanup"},
	})
	if err != nil {
		t.Fatalf("ImportLegacyRunEvents returned error: %v", err)
	}
	if lifecycle.Status != RunStateSucceeded || len(lifecycle.History) != 2 {
		t.Fatalf("lifecycle = %#v, want succeeded with 2 imported events", lifecycle)
	}
	if !lifecycle.History[0].LegacyImport || lifecycle.History[0].EventType != legacyLifecycleEventType {
		t.Fatalf("history[0] = %#v, want legacy import marker", lifecycle.History[0])
	}
	var totalEvents int
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_events WHERE run_id = ?`, "run-legacy").Scan(&totalEvents)
	}); err != nil {
		t.Fatalf("count run events: %v", err)
	}
	if totalEvents != 3 {
		t.Fatalf("run_events count = %d, want original event plus 2 lifecycle imports", totalEvents)
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
