package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPlanSchemaMigrationFreshPathHasNoSideEffects(t *testing.T) {
	root := t.TempDir()
	databaseDir := filepath.Join(root, "not-created")
	path := filepath.Join(databaseDir, "loopcoder.db")

	plan, err := PlanSchemaMigration(context.Background(), path)
	if err != nil {
		t.Fatalf("PlanSchemaMigration returned error: %v", err)
	}
	if plan.SchemaVersion != SchemaMigrationContract || plan.DatabasePath != path {
		t.Fatalf("plan identity = %#v", plan)
	}
	if plan.SourceExists || plan.SourceSchemaVersion != 0 || plan.Status != schemaMigrationStatusFresh {
		t.Fatalf("fresh plan source = %#v", plan)
	}
	if plan.TargetSchemaVersion != CurrentSchemaVersion || len(plan.Steps) != CurrentSchemaVersion {
		t.Fatalf("fresh plan target/steps = %d/%d", plan.TargetSchemaVersion, len(plan.Steps))
	}
	if plan.BackupRequired || plan.Rollback.Applicable || plan.PlanFingerprint == "" {
		t.Fatalf("fresh plan recovery metadata = %#v", plan)
	}
	if _, err := os.Stat(databaseDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan created database directory: %v", err)
	}
}

func TestPlanSchemaMigrationV07IsReadOnlyAndRequiresVerifiedBackup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV9NestedClaimLifecycleSchema(t, raw)
	closeRawDB(t, raw)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat source before plan: %v", err)
	}

	plan, err := PlanSchemaMigration(ctx, path)
	if err != nil {
		t.Fatalf("PlanSchemaMigration returned error: %v", err)
	}
	if !plan.SourceExists || plan.SourceSchemaVersion != 9 || plan.Status != schemaMigrationStatusUpgradeRequired {
		t.Fatalf("v0.7 plan source = %#v", plan)
	}
	if len(plan.Steps) != CurrentSchemaVersion-9 || plan.Steps[0].Version != 10 || plan.Steps[len(plan.Steps)-1].Version != CurrentSchemaVersion {
		t.Fatalf("v0.7 plan steps = %#v", plan.Steps)
	}
	if !plan.BackupRequired || plan.BackupDirectory != filepath.Join(filepath.Dir(path), "backups") {
		t.Fatalf("v0.7 backup plan = %#v", plan)
	}
	if !plan.Rollback.Applicable || !plan.Rollback.Supported || !plan.Rollback.RequiresOffline || !plan.Rollback.BackupRequired {
		t.Fatalf("v0.7 rollback plan = %#v", plan.Rollback)
	}
	if len(plan.Rollback.Limitations) != 4 || !strings.HasPrefix(plan.PlanFingerprint, "sha256:") {
		t.Fatalf("v0.7 machine-readable metadata = %#v", plan)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat source after plan: %v", err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("plan mutated source metadata: before=%#v after=%#v", before, after)
	}
	if _, err := os.Stat(plan.BackupDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan created backup directory: %v", err)
	}
	raw = createRawDB(t, path)
	version, err := schemaVersion(ctx, raw)
	closeRawDB(t, raw)
	if err != nil || version != 9 {
		t.Fatalf("source after plan = version %d error %v, want version 9", version, err)
	}
}

func TestRunSchemaMigrationFreshThenNoOp(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "loopcoder.db")

	created, err := RunSchemaMigration(ctx, SchemaMigrationOptions{Path: path, Apply: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("RunSchemaMigration fresh returned error: %v", err)
	}
	if created.Status != "created" || !created.Applied || created.DryRun || created.Health == nil || !created.Health.OK {
		t.Fatalf("fresh result = %#v", created)
	}
	if created.Backup != nil || created.Rollback.Applicable {
		t.Fatalf("fresh recovery result = %#v", created)
	}

	repeated, err := RunSchemaMigration(ctx, SchemaMigrationOptions{Path: path, Apply: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("RunSchemaMigration repeated returned error: %v", err)
	}
	if repeated.Status != "no-op" || !repeated.Applied || repeated.Plan.Status != schemaMigrationStatusCurrent || len(repeated.Plan.Steps) != 0 {
		t.Fatalf("repeated result = %#v", repeated)
	}
}

func TestRunSchemaMigrationV07PreservesProjectsAndHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV9NestedClaimLifecycleSchema(t, raw)
	for _, statement := range []string{
		`INSERT INTO projects(id, remote_url, github_owner, github_name, local_path, default_branch, display_name, created_at, updated_at, local_path_canonical, git_root, remote_url_normalized, identity_source, detached_at)
			VALUES ('proj-a', 'git@github.com:owner/a.git', 'owner', 'a', '/work/a', 'main', 'A', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '/work/a', '/work/a', 'github.com/owner/a', 'remote', '')`,
		`INSERT INTO projects(id, remote_url, github_owner, github_name, local_path, default_branch, display_name, created_at, updated_at, local_path_canonical, git_root, remote_url_normalized, identity_source, detached_at)
			VALUES ('proj-b', 'git@github.com:owner/b.git', 'owner', 'b', '/work/b', 'main', 'B', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '/work/b', '/work/b', 'github.com/owner/b', 'remote', '')`,
		`UPDATE runs SET project_id = 'proj-a' WHERE id = 'run-parent'`,
		`UPDATE runs SET project_id = 'proj-b' WHERE id = 'run-child'`,
		`INSERT INTO reports(id, run_id, role, provider, model, payload_json, created_at, project_id, source_path, source_hash, source_kind)
			VALUES ('report-history', 'run-child', 'worker', 'codex', 'gpt-test', '{}', '2026-01-01T00:00:00Z', 'proj-b', 'legacy/report.json', 'sha256:test', 'legacy')`,
	} {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed v0.7 identity fixture: %v\n%s", err, statement)
		}
	}
	closeRawDB(t, raw)

	result, err := RunSchemaMigration(ctx, SchemaMigrationOptions{Path: path, Apply: true, Now: fixedNow})
	if err != nil {
		t.Fatalf("RunSchemaMigration returned error: %v", err)
	}
	if result.Status != "migrated" || result.Health == nil || result.Health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("migration result = %#v", result)
	}
	if result.Backup == nil || !result.Backup.Verified || result.Backup.SourceSchemaVersion != 9 {
		t.Fatalf("verified backup = %#v", result.Backup)
	}
	if result.Rollback.BackupPath != result.Backup.Path || result.Rollback.BackupSHA256 != result.Backup.SHA256 {
		t.Fatalf("rollback/backup mismatch: rollback=%#v backup=%#v", result.Rollback, result.Backup)
	}
	backupInfo, err := os.Stat(result.Backup.Path)
	if err != nil {
		t.Fatalf("stat verified backup: %v", err)
	}
	if backupInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("backup mode = %04o, want owner-only", backupInfo.Mode().Perm())
	}

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	defer store.Close()
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM projects WHERE id IN ('proj-a', 'proj-b')`:                                                                2,
		`SELECT COUNT(*) FROM runs WHERE (id = 'run-parent' AND project_id = 'proj-a') OR (id = 'run-child' AND project_id = 'proj-b')`: 2,
		`SELECT COUNT(*) FROM reports WHERE id = 'report-history' AND project_id = 'proj-b'`:                                            1,
		`SELECT COUNT(*) FROM delivery_migration_backups WHERE source_schema_version = 9`:                                               1,
	} {
		var got int
		if err := store.WithTx(ctx, func(tx Tx) error { return tx.QueryRow(ctx, query).Scan(&got) }); err != nil {
			t.Fatalf("query preserved migration state: %v", err)
		}
		if got != want {
			t.Fatalf("query %q count = %d, want %d", query, got, want)
		}
	}
}

func TestPlanSchemaMigrationRejectsCorruptInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write corrupt database: %v", err)
	}
	if _, err := PlanSchemaMigration(context.Background(), path); err == nil {
		t.Fatal("PlanSchemaMigration corrupt input returned nil error")
	}
}

func TestOpenV9MigrationDiskFullLeavesSourceAtV07(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV9NestedClaimLifecycleSchema(t, raw)
	closeRawDB(t, raw)

	_, err := Open(ctx, Options{
		Path: path,
		Now:  fixedNow,
		deliveryV10BackupHookForTest: func(_ context.Context, phase deliveryV10BackupPhase, _ string) error {
			if phase == deliveryV10BackupPhaseAfterVacuum {
				return syscall.ENOSPC
			}
			return nil
		},
	})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Open disk-full error = %v, want ENOSPC", err)
	}
	assertBackupDirEmpty(t, path)
	raw = createRawDB(t, path)
	version, versionErr := schemaVersion(ctx, raw)
	closeRawDB(t, raw)
	if versionErr != nil || version != 9 {
		t.Fatalf("source after disk-full = version %d error %v, want version 9", version, versionErr)
	}
}
