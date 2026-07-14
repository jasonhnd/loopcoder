package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	for _, table := range []string{"migrations", "projects", "runs", "run_events", "run_edges", "reports", "child_plans", "run_claims", "usage_records", "usage_reconciliations", "budget_policies", "budget_reservations", "budget_aggregates", "quota_budget_events", "role_definitions", "routing_policy_profiles", "routing_policy_inputs", "routing_legacy_model_mappings", "routing_events", "fallback_decisions", "replan_decisions", "verification_decisions", "verification_decision_members", "handoff_transactions", "nested_scheduler_resource_reservations", "progress_delivery_obligations", "progress_delivery_attempts", "progress_delivery_acknowledgments", "progress_delivery_replay_cursors"} {
		if !tableExists(t, store, table) {
			t.Fatalf("missing table %s", table)
		}
	}
	var migrationName string
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT name FROM migrations WHERE version = 28`).Scan(&migrationName)
	}); err != nil {
		t.Fatalf("query migration 28: %v", err)
	}
	if migrationName != "progress delivery outbox" {
		t.Fatalf("migration 28 name = %q", migrationName)
	}
	for _, column := range []string{"next_attempt_at", "ack_policy", "required_ack"} {
		if !tableColumnExists(t, store, "progress_delivery_obligations", column) {
			t.Fatalf("progress_delivery_obligations missing column %s", column)
		}
	}
	if !tableColumnExists(t, store, "progress_delivery_attempts", "next_attempt_at") {
		t.Fatalf("progress_delivery_attempts missing next_attempt_at")
	}
	if tableColumnExists(t, store, "routing_decisions", "alternatives_json") {
		t.Fatalf("routing_decisions includes non-v1 alternatives_json column")
	}
}

func TestOpenFreshDatabaseRecordsNoSourceMigrationMetadataIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "data", "loopcoder.db")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh path stat error = %v, want not exist", err)
	}

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	first := readOnlyDeliveryMigrationBackup(t, ctx, store)
	assertNoSourceDeliveryMigrationBackup(t, first, path)
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM projects`, 0)
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM delivery_runs`, 0)
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM delivery_migration_backups`, 1)
	if err := store.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "backups")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup directory stat error = %v, want no backup directory", err)
	}

	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen returned error: %v", err)
	}
	defer reopened.Close()
	health, err := reopened.Health(ctx)
	if err != nil {
		t.Fatalf("Health returned error: %v", err)
	}
	if !health.OK || health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("health = %#v, want schema %d", health, CurrentSchemaVersion)
	}
	second := readOnlyDeliveryMigrationBackup(t, ctx, reopened)
	if second != first {
		t.Fatalf("backup metadata changed after reopen:\nfirst:  %#v\nsecond: %#v", first, second)
	}
	assertCountInStore(t, ctx, reopened, `SELECT COUNT(*) FROM delivery_migration_backups`, 1)
	assertCountInStore(t, ctx, reopened, `SELECT COUNT(*) FROM projects`, 0)
	assertCountInStore(t, ctx, reopened, `SELECT COUNT(*) FROM delivery_runs`, 0)
}

func TestOpenMigratesV27DatabaseToProgressDeliveryOutboxV28(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		for _, table := range []string{"progress_delivery_replay_cursors", "progress_delivery_acknowledgments", "progress_delivery_attempts", "progress_delivery_obligations"} {
			if _, err := tx.Exec(ctx, `DROP TABLE `+table); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM migrations WHERE version = 28`)
		return err
	}); err != nil {
		t.Fatalf("simulate v27 database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen v27 database: %v", err)
	}
	defer reopened.Close()
	health, err := reopened.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.OK || health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("health = %#v, want schema %d", health, CurrentSchemaVersion)
	}
	for _, table := range []string{"progress_delivery_obligations", "progress_delivery_attempts", "progress_delivery_acknowledgments", "progress_delivery_replay_cursors"} {
		if !tableExists(t, reopened, table) {
			t.Fatalf("missing v28 table %s", table)
		}
	}
}

func TestPrepareDeliveryV10BackupTreatsMissingPathFixturesAsNoSource(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "macos", path: "/Users/example/Library/Application Support/loopcoder/data/loopcoder.db"},
		{name: "linux", path: "/home/example/.loopcoder/data/loopcoder.db"},
		{name: "windows-drive", path: `C:\Users\example\.loopcoder\data\loopcoder.db`},
		{name: "windows-unc", path: `\\server\share\loopcoder\data\loopcoder.db`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var probedPath string
			backup, err := prepareDeliveryV10BackupWithSourceProbe(tt.path, formatTimestamp(fixedNow()), func(path string) (bool, error) {
				probedPath = path
				return false, nil
			})
			if err != nil {
				t.Fatalf("prepareDeliveryV10Backup returned error: %v", err)
			}
			if probedPath != filepath.Clean(tt.path) {
				t.Fatalf("source probe path = %q, want cleaned fixture %q", probedPath, filepath.Clean(tt.path))
			}
			assertNoSourceDeliveryMigrationBackup(t, deliveryMigrationBackupRow{
				BackupID:                 backup.BackupID,
				SourceDBPath:             backup.SourcePath,
				SourceSchemaVersion:      backup.SourceSchemaVersion,
				SourceDBHash:             backup.SourceHash,
				BackupPath:               backup.BackupPath,
				CreatedAt:                backup.CreatedAt,
				LoopcoderVersion:         "0.8.0",
				MigrationPlanFingerprint: backup.MigrationPlanFingerprint,
			}, filepath.Clean(tt.path))
		})
	}
}

func TestPrepareDeliveryV10BackupProbeErrorsFailClosed(t *testing.T) {
	probeErr := errors.New("permission denied")
	_, err := prepareDeliveryV10BackupWithSourceProbe(filepath.Join(t.TempDir(), "loopcoder.db"), formatTimestamp(fixedNow()), func(string) (bool, error) {
		return false, probeErr
	})
	if !errors.Is(err, probeErr) || !strings.Contains(err.Error(), "inspect delivery v10 backup source") {
		t.Fatalf("prepareDeliveryV10Backup error = %v, want visible source probe error", err)
	}
}

func TestDeliveryV10BackupHashReaderUsesBoundedBuffer(t *testing.T) {
	const size = 32 << 20
	const bufferSize = 4 << 10
	reader := &trackingZeroReader{remaining: size}

	got, err := readerSHA256(context.Background(), reader, make([]byte, bufferSize))
	if err != nil {
		t.Fatalf("readerSHA256 returned error: %v", err)
	}
	want := zeroSHA256(size)
	if got != want {
		t.Fatalf("hash = %s, want %s", got, want)
	}
	if reader.maxReadLen > bufferSize {
		t.Fatalf("max read len = %d, want <= %d", reader.maxReadLen, bufferSize)
	}
	if reader.reads <= 1 {
		t.Fatalf("reads = %d, want streaming reads", reader.reads)
	}
}

type trackingZeroReader struct {
	remaining  int64
	maxReadLen int
	reads      int
}

func (r *trackingZeroReader) Read(p []byte) (int, error) {
	if len(p) > r.maxReadLen {
		r.maxReadLen = len(p)
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
	}
	clear(p[:n])
	r.remaining -= int64(n)
	r.reads++
	return n, nil
}

func zeroSHA256(size int64) string {
	hash := sha256.New()
	buffer := make([]byte, 8<<10)
	for size > 0 {
		n := len(buffer)
		if int64(n) > size {
			n = int(size)
		}
		hash.Write(buffer[:n])
		size -= int64(n)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestOpenRerunsRoutingPolicyProfileMigrationIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}
	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen returned error: %v", err)
	}
	defer reopened.Close()
	health, err := reopened.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.OK || health.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("health = %#v, want schema %d", health, CurrentSchemaVersion)
	}
	for _, table := range []string{"routing_policy_profiles", "routing_policy_inputs", "routing_legacy_model_mappings", "fallback_decisions", "replan_decisions", "verification_decisions", "verification_decision_members"} {
		if !tableExists(t, reopened, table) {
			t.Fatalf("missing table %s after reopen", table)
		}
	}
}

func TestBudgetReservationIdempotencyIndexScopedByPrimaryPolicy(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	insert := func(id, policyID string) error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO budget_reservations(
				budget_reservation_id, idempotency_key, request_fingerprint, requester_id,
				quantity_kind, unit, value_scale, requested_value, reserved_value, committed_value,
				released_value, state, generation, lease_expires_at, scope_key, policy_ids_json,
				payload_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				id, "shared-key", "sha256:"+id, "requester",
				"total-tokens", "token", 0, 1, 1, 0,
				0, "active", 1, fixedNow().Add(time.Hour).Format(time.RFC3339Nano), "{}",
				`["`+policyID+`"]`, "{}", fixedNow().Format(time.RFC3339Nano), fixedNow().Format(time.RFC3339Nano))
			return err
		})
	}
	if err := insert("bres_policy_a", "bpol_a"); err != nil {
		t.Fatalf("insert policy a: %v", err)
	}
	if err := insert("bres_policy_b", "bpol_b"); err != nil {
		t.Fatalf("insert policy b with same idempotency key: %v", err)
	}
	if err := insert("bres_policy_a_duplicate", "bpol_a"); err == nil {
		t.Fatal("duplicate same-policy idempotency insert succeeded, want constraint failure")
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
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM delivery_migration_backups`, 0)
}

func TestOpenMigratesExistingV9DatabaseCreatesRealBackupMetadata(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV9NestedClaimLifecycleSchema(t, raw)
	closeRawDB(t, raw)

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	first := readOnlyDeliveryMigrationBackup(t, ctx, store)
	if first.BackupID == "" || !strings.HasPrefix(first.BackupID, "backup_") {
		t.Fatalf("backup_id = %q, want real backup prefix", first.BackupID)
	}
	if first.SourceDBHash == "" {
		t.Fatal("source_db_hash is empty")
	}
	wantBackupPath := filepath.Join(filepath.Dir(path), "backups", "schema-v9-"+first.SourceDBHash[:16]+".db")
	if first.SourceDBPath != filepath.Clean(path) || first.SourceSchemaVersion != 9 || first.BackupPath != wantBackupPath {
		t.Fatalf("backup metadata = %#v, want real v9 backup path %q", first, wantBackupPath)
	}
	backupHash, err := backupPathSHA256(t, ctx, wantBackupPath)
	if err != nil {
		t.Fatalf("hash backup image: %v", err)
	}
	if backupHash != first.SourceDBHash {
		t.Fatalf("backup hash = %s, want recorded source_db_hash %s", backupHash, first.SourceDBHash)
	}
	backupRaw, err := sql.Open(driverName, wantBackupPath)
	if err != nil {
		t.Fatalf("open backup image: %v", err)
	}
	backupVersion, err := schemaVersion(ctx, backupRaw)
	if closeErr := backupRaw.Close(); closeErr != nil {
		t.Fatalf("close backup image: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("query backup schema version: %v", err)
	}
	if backupVersion != 9 {
		t.Fatalf("backup schema version = %d, want 9", backupVersion)
	}
	if err := appendFile(wantBackupPath, []byte("corrupt")); err != nil {
		t.Fatalf("corrupt backup image: %v", err)
	}
	corruptHash, err := backupPathSHA256(t, ctx, wantBackupPath)
	if err != nil {
		t.Fatalf("hash corrupt backup image: %v", err)
	}
	if corruptHash == first.SourceDBHash {
		t.Fatal("corrupt backup hash still matches recorded checksum")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}

	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen returned error: %v", err)
	}
	defer reopened.Close()
	second := readOnlyDeliveryMigrationBackup(t, ctx, reopened)
	if second != first {
		t.Fatalf("backup metadata changed after reopen:\nfirst:  %#v\nsecond: %#v", first, second)
	}
	assertCountInStore(t, ctx, reopened, `SELECT COUNT(*) FROM delivery_migration_backups`, 1)
}

func TestOpenMigratesExistingV9DatabaseBackupCanRestoreAndReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "loopcoder.db")
	raw := createRawDB(t, path)
	createV9NestedClaimLifecycleSchema(t, raw)
	if _, err := raw.ExecContext(ctx, `CREATE TABLE restore_markers(id TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create restore marker table: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO restore_markers(id, value) VALUES ('marker', 'restored')`); err != nil {
		t.Fatalf("insert restore marker: %v", err)
	}
	closeRawDB(t, raw)

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	backup := readOnlyDeliveryMigrationBackup(t, ctx, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	restoredPath := filepath.Join(root, "restored.db")
	copyWithinRoot(t, root, filepath.Join("backups", filepath.Base(backup.BackupPath)), filepath.Base(restoredPath))
	restoredRaw, err := sql.Open(driverName, restoredPath)
	if err != nil {
		t.Fatalf("open restored backup: %v", err)
	}
	version, err := schemaVersion(ctx, restoredRaw)
	if err != nil {
		_ = restoredRaw.Close()
		t.Fatalf("query restored schema version: %v", err)
	}
	var marker string
	if err := restoredRaw.QueryRowContext(ctx, `SELECT value FROM restore_markers WHERE id = 'marker'`).Scan(&marker); err != nil {
		_ = restoredRaw.Close()
		t.Fatalf("query restored marker: %v", err)
	}
	if err := restoredRaw.Close(); err != nil {
		t.Fatalf("close restored backup: %v", err)
	}
	if version != 9 || marker != "restored" {
		t.Fatalf("restored backup = version %d marker %q, want version 9 marker restored", version, marker)
	}
}

func TestOpenMigratesLargeV9DatabaseWithBoundedBackupHashBuffer(t *testing.T) {
	ctx := context.Background()
	const largePayloadSize = 8 << 20
	const hashBufferSize = 4 << 10
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	createV9NestedClaimLifecycleSchema(t, raw)
	if _, err := raw.ExecContext(ctx, `CREATE TABLE backup_large_payloads(id TEXT PRIMARY KEY, payload BLOB NOT NULL)`); err != nil {
		t.Fatalf("create large payload table: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO backup_large_payloads(id, payload) VALUES ('large', zeroblob(?))`, largePayloadSize); err != nil {
		t.Fatalf("insert large payload: %v", err)
	}
	closeRawDB(t, raw)

	bufferCalls := 0
	maxBufferLen := 0
	store, err := Open(ctx, Options{
		Path: path,
		Now:  fixedNow,
		deliveryV10BackupBufferFactoryForTest: func() []byte {
			bufferCalls++
			buffer := make([]byte, hashBufferSize)
			if len(buffer) > maxBufferLen {
				maxBufferLen = len(buffer)
			}
			return buffer
		},
	})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	backup := readOnlyDeliveryMigrationBackup(t, ctx, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}
	if bufferCalls == 0 {
		t.Fatal("backup hash buffer factory was not used")
	}
	if maxBufferLen > hashBufferSize {
		t.Fatalf("max backup hash buffer = %d, want <= %d", maxBufferLen, hashBufferSize)
	}
	info, err := os.Stat(backup.BackupPath)
	if err != nil {
		t.Fatalf("stat backup image: %v", err)
	}
	if info.Size() <= int64(largePayloadSize) {
		t.Fatalf("backup size = %d, want larger than large fixture payload %d", info.Size(), largePayloadSize)
	}
	backupHash, err := backupPathSHA256(t, ctx, backup.BackupPath)
	if err != nil {
		t.Fatalf("hash backup image: %v", err)
	}
	if backupHash != backup.SourceDBHash {
		t.Fatalf("backup hash = %s, want recorded source_db_hash %s", backupHash, backup.SourceDBHash)
	}
	backupRaw, err := sql.Open(driverName, backup.BackupPath)
	if err != nil {
		t.Fatalf("open backup image: %v", err)
	}
	var payloadLen int
	if err := backupRaw.QueryRowContext(ctx, `SELECT length(payload) FROM backup_large_payloads WHERE id = 'large'`).Scan(&payloadLen); err != nil {
		t.Fatalf("query backup large payload: %v", err)
	}
	if err := backupRaw.Close(); err != nil {
		t.Fatalf("close backup image: %v", err)
	}
	if payloadLen != largePayloadSize {
		t.Fatalf("backup payload length = %d, want %d", payloadLen, largePayloadSize)
	}
}

func TestOpenMigratesV9WALDatabaseBackupIncludesCommittedContent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw := createRawDB(t, path)
	if _, err := raw.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		t.Fatalf("enable WAL mode: %v", err)
	}
	createV9NestedClaimLifecycleSchema(t, raw)
	if _, err := raw.ExecContext(ctx, `CREATE TABLE wal_backup_markers(id TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create WAL marker table: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO wal_backup_markers(id, value) VALUES ('marker', 'committed-through-wal')`); err != nil {
		t.Fatalf("insert WAL marker: %v", err)
	}

	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	backup := readOnlyDeliveryMigrationBackup(t, ctx, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}
	closeRawDB(t, raw)

	backupRaw, err := sql.Open(driverName, backup.BackupPath)
	if err != nil {
		t.Fatalf("open backup image: %v", err)
	}
	var value string
	if err := backupRaw.QueryRowContext(ctx, `SELECT value FROM wal_backup_markers WHERE id = 'marker'`).Scan(&value); err != nil {
		t.Fatalf("query WAL marker from backup: %v", err)
	}
	if err := backupRaw.Close(); err != nil {
		t.Fatalf("close backup image: %v", err)
	}
	if value != "committed-through-wal" {
		t.Fatalf("backup WAL marker = %q, want committed-through-wal", value)
	}
}

func TestOpenV9MigrationCancellationCleansPartialBackup(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []deliveryV10BackupPhase{
		deliveryV10BackupPhaseBeforeVacuum,
		deliveryV10BackupPhaseAfterVacuum,
		deliveryV10BackupPhaseAfterHash,
		deliveryV10BackupPhaseBeforeRename,
	} {
		t.Run(string(phase), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "loopcoder.db")
			raw := createRawDB(t, path)
			createV9NestedClaimLifecycleSchema(t, raw)
			closeRawDB(t, raw)

			_, err := Open(ctx, Options{
				Path: path,
				Now:  fixedNow,
				deliveryV10BackupHookForTest: func(_ context.Context, got deliveryV10BackupPhase, _ string) error {
					if got == phase {
						return context.Canceled
					}
					return nil
				},
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Open error = %v, want context.Canceled", err)
			}
			assertBackupDirEmpty(t, path)
			raw = createRawDB(t, path)
			version, err := schemaVersion(ctx, raw)
			closeRawDB(t, raw)
			if err != nil {
				t.Fatalf("query source schema after cancelled migration: %v", err)
			}
			if version != 9 {
				t.Fatalf("source schema after cancelled migration = %d, want 9", version)
			}
		})
	}
}

func TestOpenV9MigrationCancellationAfterVacuumAndHashReleasesBackupFiles(t *testing.T) {
	ctx := context.Background()
	for _, phase := range []deliveryV10BackupPhase{
		deliveryV10BackupPhaseAfterVacuum,
		deliveryV10BackupPhaseAfterHash,
	} {
		t.Run(string(phase), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "loopcoder.db")
			raw := createRawDB(t, path)
			createV9NestedClaimLifecycleSchema(t, raw)
			closeRawDB(t, raw)

			_, err := Open(ctx, Options{
				Path: path,
				Now:  fixedNow,
				deliveryV10BackupHookForTest: func(_ context.Context, got deliveryV10BackupPhase, _ string) error {
					if got == phase {
						return context.Canceled
					}
					return nil
				},
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Open error = %v, want context.Canceled", err)
			}
			assertBackupDirEmpty(t, path)
			if err := os.RemoveAll(root); err != nil {
				t.Fatalf("remove cancelled migration tree: %v", err)
			}
		})
	}
}

func TestVacuumIntoBindsQuotedDestinationPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sourcePath := filepath.Join(root, "loopcoder.db")
	raw := createRawDB(t, sourcePath)
	createV9NestedClaimLifecycleSchema(t, raw)
	if _, err := raw.ExecContext(ctx, `CREATE TABLE quoted_backup_markers(id TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create quoted marker table: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO quoted_backup_markers(id, value) VALUES ('marker', 'quoted path')`); err != nil {
		t.Fatalf("insert quoted marker: %v", err)
	}
	defer closeRawDB(t, raw)

	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatalf("mkdir backup dir: %v", err)
	}
	backupPath := filepath.Join(backupDir, "schema-v9-'quoted'.tmp")
	if err := vacuumInto(ctx, raw, backupPath); err != nil {
		t.Fatalf("vacuum into quoted destination: %v", err)
	}
	backupRaw, err := sql.Open(driverName, backupPath)
	if err != nil {
		t.Fatalf("open quoted backup: %v", err)
	}
	var value string
	if err := backupRaw.QueryRowContext(ctx, `SELECT value FROM quoted_backup_markers WHERE id = 'marker'`).Scan(&value); err != nil {
		_ = backupRaw.Close()
		t.Fatalf("query quoted backup marker: %v", err)
	}
	if err := backupRaw.Close(); err != nil {
		t.Fatalf("close quoted backup: %v", err)
	}
	if value != "quoted path" {
		t.Fatalf("quoted backup marker = %q, want quoted path", value)
	}
}

func TestSyncFileUsesClosableWriteCapableHandle(t *testing.T) {
	rootPath := t.TempDir()
	backupRoot, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatalf("open backup root: %v", err)
	}
	defer backupRoot.Close()

	file, err := backupRoot.OpenFile("snapshot.db", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if _, err := file.WriteString("snapshot"); err != nil {
		_ = file.Close()
		t.Fatalf("write snapshot: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close snapshot before sync: %v", err)
	}
	if err := syncFile(backupRoot, "snapshot.db"); err != nil {
		t.Fatalf("sync snapshot: %v", err)
	}
	if err := backupRoot.Rename("snapshot.db", "snapshot-renamed.db"); err != nil {
		t.Fatalf("rename synced snapshot: %v", err)
	}
	if err := backupRoot.Remove("snapshot-renamed.db"); err != nil {
		t.Fatalf("remove synced snapshot: %v", err)
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

func TestClaimChildRunExecutionWithReservationsRejectsZeroBudgetAndRollsBack(t *testing.T) {
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
	req := nestedSchedulerStorageBudgetRequest(children[0].RunID)
	req.BudgetRequestedValue = 0
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	_, err = ClaimChildRunExecutionWithReservations(ctx, store, parent.RunID, children[0].RunID, "executor-budget", now, now.Add(time.Minute), req)
	if err == nil || !strings.Contains(err.Error(), string(ErrChildBudgetRequiredCode)) {
		t.Fatalf("ClaimChildRunExecutionWithReservations error = %v, want child budget required", err)
	}
	var claims, resources, reservations int
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_claims WHERE run_id = ?`, children[0].RunID).Scan(&claims); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM nested_scheduler_resource_reservations WHERE run_id = ?`, children[0].RunID).Scan(&resources); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM budget_reservations WHERE sub_agent_id = ?`, children[0].RunID).Scan(&reservations)
	}); err != nil {
		t.Fatalf("query rollback counts: %v", err)
	}
	if claims != 0 || resources != 0 || reservations != 0 {
		t.Fatalf("partial rows claims/resources/budgets = %d/%d/%d, want 0/0/0", claims, resources, reservations)
	}
}

func TestClaimChildRunExecutionWithReservationsRequiredAuthorityEmptyRollsBack(t *testing.T) {
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
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	_, err = ClaimChildRunExecutionWithReservations(ctx, store, parent.RunID, children[0].RunID, "executor-budget", now, now.Add(time.Minute), SchedulerResourceReservationRequest{
		RequireBudgetAuthority: true,
	})
	if !errors.Is(err, ErrChildBudgetRequired) {
		t.Fatalf("ClaimChildRunExecutionWithReservations error = %v, want ErrChildBudgetRequired", err)
	}
	var claims, resources, reservations int
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_claims WHERE run_id = ?`, children[0].RunID).Scan(&claims); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM nested_scheduler_resource_reservations WHERE run_id = ?`, children[0].RunID).Scan(&resources); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM budget_reservations WHERE sub_agent_id = ?`, children[0].RunID).Scan(&reservations)
	}); err != nil {
		t.Fatalf("query rollback counts: %v", err)
	}
	if claims != 0 || resources != 0 || reservations != 0 {
		t.Fatalf("partial rows claims/resources/budgets = %d/%d/%d, want 0/0/0", claims, resources, reservations)
	}
}

func TestClaimChildRunExecutionWithReservationsRejectsMismatchedChosenCandidate(t *testing.T) {
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
	req := nestedSchedulerStorageBudgetRequest(children[0].RunID)
	if err := seedNestedSchedulerStorageBudgetAuthority(ctx, store, req, "claude", true); err != nil {
		t.Fatalf("seed budget authority: %v", err)
	}
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	_, err = ClaimChildRunExecutionWithReservations(ctx, store, parent.RunID, children[0].RunID, "executor-budget", now, now.Add(time.Minute), req)
	if err == nil || !strings.Contains(err.Error(), "does not match requested provider route") {
		t.Fatalf("ClaimChildRunExecutionWithReservations error = %v, want chosen-candidate route mismatch", err)
	}
}

func TestClaimChildRunExecutionWithReservationsMissingAggregateRollsBack(t *testing.T) {
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
	req := nestedSchedulerStorageBudgetRequest(children[0].RunID)
	if err := seedNestedSchedulerStorageBudgetAuthority(ctx, store, req, req.AdapterID, false); err != nil {
		t.Fatalf("seed budget authority: %v", err)
	}
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	_, err = ClaimChildRunExecutionWithReservations(ctx, store, parent.RunID, children[0].RunID, "executor-budget", now, now.Add(time.Minute), req)
	if err == nil || !strings.Contains(err.Error(), "budget aggregate") {
		t.Fatalf("ClaimChildRunExecutionWithReservations error = %v, want missing aggregate", err)
	}
	var claims, reservations int
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_claims WHERE run_id = ?`, children[0].RunID).Scan(&claims); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM budget_reservations WHERE sub_agent_id = ?`, children[0].RunID).Scan(&reservations)
	}); err != nil {
		t.Fatalf("query rollback counts: %v", err)
	}
	if claims != 0 || reservations != 0 {
		t.Fatalf("partial rows claims/budgets = %d/%d, want 0/0", claims, reservations)
	}
}

func TestRenewChildRunClaimFailsAtomicallyWhenNestedAuthorityMissing(t *testing.T) {
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
	req := nestedSchedulerStorageBudgetRequest(children[0].RunID)
	if err := seedNestedSchedulerStorageBudgetAuthority(ctx, store, req, req.AdapterID, true); err != nil {
		t.Fatalf("seed budget authority: %v", err)
	}
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	claim, err := ClaimChildRunExecutionWithReservations(ctx, store, parent.RunID, children[0].RunID, "executor-budget", now, now.Add(2*time.Minute), req)
	if err != nil {
		t.Fatalf("ClaimChildRunExecutionWithReservations: %v", err)
	}
	var oldHeartbeat, oldClaimLease, oldBudgetLease string
	var oldBudgetGeneration int64
	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT heartbeat_at, lease_expires_at FROM run_claims WHERE run_id = ?`, children[0].RunID).Scan(&oldHeartbeat, &oldClaimLease); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT generation, lease_expires_at FROM budget_reservations WHERE sub_agent_id = ?`, children[0].RunID).Scan(&oldBudgetGeneration, &oldBudgetLease); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM nested_scheduler_resource_reservations WHERE run_id = ? AND resource_kind = 'provider'`, children[0].RunID)
		return err
	}); err != nil {
		t.Fatalf("delete resource reservation: %v", err)
	}
	err = RenewChildRunClaim(ctx, store, children[0].RunID, claim.ExecutorID, claim.ClaimGeneration, now.Add(30*time.Second), now.Add(3*time.Minute))
	if err == nil || !strings.Contains(err.Error(), string(ErrChildBudgetRequiredCode)) {
		t.Fatalf("RenewChildRunClaim error = %v, want child budget required", err)
	}
	var heartbeat, claimLease, budgetLease string
	var budgetGeneration int64
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT heartbeat_at, lease_expires_at FROM run_claims WHERE run_id = ?`, children[0].RunID).Scan(&heartbeat, &claimLease); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT generation, lease_expires_at FROM budget_reservations WHERE sub_agent_id = ?`, children[0].RunID).Scan(&budgetGeneration, &budgetLease)
	}); err != nil {
		t.Fatalf("query renewal state: %v", err)
	}
	if heartbeat != oldHeartbeat || claimLease != oldClaimLease || budgetGeneration != oldBudgetGeneration || budgetLease != oldBudgetLease {
		t.Fatalf("renew mutated state heartbeat=%q/%q claimLease=%q/%q budget=%d/%d budgetLease=%q/%q",
			heartbeat, oldHeartbeat, claimLease, oldClaimLease, budgetGeneration, oldBudgetGeneration, budgetLease, oldBudgetLease)
	}
}

func TestCompleteClaimedChildRunReconcilesNestedBudgetOnce(t *testing.T) {
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
	req := nestedSchedulerStorageBudgetRequest(children[0].RunID)
	if err := seedNestedSchedulerStorageBudgetAuthority(ctx, store, req, req.AdapterID, true); err != nil {
		t.Fatalf("seed budget authority: %v", err)
	}
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	claim, err := ClaimChildRunExecutionWithReservations(ctx, store, parent.RunID, children[0].RunID, "executor-budget", now, now.Add(2*time.Minute), req)
	if err != nil {
		t.Fatalf("ClaimChildRunExecutionWithReservations: %v", err)
	}
	if err := CompleteClaimedChildRun(ctx, store, parent.RunID, children[0].RunID, claim.ExecutorID, claim.ClaimGeneration, "succeeded", formatTimestamp(now.Add(time.Minute)), "done", ""); err != nil {
		t.Fatalf("CompleteClaimedChildRun first: %v", err)
	}
	var committedBefore int64
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT committed_value FROM budget_aggregates WHERE budget_policy_id = 'bpol-storage-project'`).Scan(&committedBefore)
	}); err != nil {
		t.Fatalf("query committed before replay: %v", err)
	}
	err = CompleteClaimedChildRun(ctx, store, parent.RunID, children[0].RunID, claim.ExecutorID, claim.ClaimGeneration, "succeeded", formatTimestamp(now.Add(90*time.Second)), "done again", "")
	if err == nil || !strings.Contains(err.Error(), string(ErrChildBudgetRequiredCode)) {
		t.Fatalf("CompleteClaimedChildRun replay error = %v, want child budget required", err)
	}
	var committedAfter int64
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT committed_value FROM budget_aggregates WHERE budget_policy_id = 'bpol-storage-project'`).Scan(&committedAfter)
	}); err != nil {
		t.Fatalf("query committed after replay: %v", err)
	}
	if committedBefore != req.BudgetRequestedValue || committedAfter != committedBefore {
		t.Fatalf("committed before/after = %d/%d, want exactly one commit of %d", committedBefore, committedAfter, req.BudgetRequestedValue)
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

func TestPropagateRunTreeTerminalClassifiesLaunchedChildLostAndReleasesAuthority(t *testing.T) {
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
	req := nestedSchedulerStorageBudgetRequest(childRunID)
	if err := seedNestedSchedulerStorageBudgetAuthority(ctx, store, req, "codex", true); err != nil {
		t.Fatalf("seed budget authority: %v", err)
	}
	now := time.Date(2026, 7, 10, 0, 0, 1, 0, time.UTC)
	claim, err := ClaimChildRunExecutionWithReservations(ctx, store, parent.RunID, childRunID, "executor-cancel", now, now.Add(time.Hour), req)
	if err != nil {
		t.Fatalf("ClaimChildRunExecutionWithReservations: %v", err)
	}
	if err := UpdateChildRunClaimPhase(ctx, store, parent.RunID, childRunID, claim.ExecutorID, claim.ClaimGeneration, ClaimPhaseExecuting, formatTimestamp(now.Add(time.Second)), ""); err != nil {
		t.Fatalf("UpdateChildRunClaimPhase executing: %v", err)
	}

	results, err := PropagateRunTreeTerminal(ctx, store, RunTreeTerminalRequest{
		RunID:     parent.RunID,
		Status:    "cancelled",
		UpdatedAt: formatTimestamp(now.Add(2 * time.Second)),
		Reason:    "parent cancellation",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("PropagateRunTreeTerminal: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v, want parent and child", results)
	}

	var childStatus, edgeStatus, resourceState, budgetState string
	var activeResources, eventCount int
	var aggregateReserved int64
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = ?`, childRunID).Scan(&childStatus); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, parent.RunID, childRunID).Scan(&edgeStatus); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM nested_scheduler_resource_reservations WHERE run_id = ? AND state = 'active'`, childRunID).Scan(&activeResources); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state FROM nested_scheduler_resource_reservations WHERE run_id = ? LIMIT 1`, childRunID).Scan(&resourceState); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state FROM budget_reservations WHERE sub_agent_id = ? LIMIT 1`, childRunID).Scan(&budgetState); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT reserved_value FROM budget_aggregates WHERE budget_policy_id = 'bpol-storage-project'`).Scan(&aggregateReserved); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_events WHERE run_id = ?`, childRunID).Scan(&eventCount)
	}); err != nil {
		t.Fatalf("query propagated state: %v", err)
	}
	if childStatus != "lost" || edgeStatus != "lost" || activeResources != 0 || resourceState != schedulerReservationStateReleased || budgetState != "cancelled" || aggregateReserved != 0 {
		t.Fatalf("propagated state child=%q edge=%q active_resources=%d resource=%q budget=%q reserved=%d, want lost/lost released/cancelled/0",
			childStatus, edgeStatus, activeResources, resourceState, budgetState, aggregateReserved)
	}

	if _, err := PropagateRunTreeTerminal(ctx, store, RunTreeTerminalRequest{RunID: parent.RunID, Status: "cancelled", UpdatedAt: formatTimestamp(now.Add(3 * time.Second)), Reason: "replay", Source: "test"}); err != nil {
		t.Fatalf("PropagateRunTreeTerminal replay: %v", err)
	}
	var replayEventCount int
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_events WHERE run_id = ?`, childRunID).Scan(&replayEventCount)
	}); err != nil {
		t.Fatalf("query replay events: %v", err)
	}
	if replayEventCount != eventCount {
		t.Fatalf("replay event count = %d, want unchanged %d", replayEventCount, eventCount)
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

func TestWithWriteTxFirstAttemptDoesNotAddRetryDeadline(t *testing.T) {
	ctx := context.Background()
	shortRetry := WriteTxRetryOptions{
		MaxAttempts: 2,
		MaxElapsed:  time.Nanosecond,
		Backoff:     func(int) time.Duration { return 0 },
	}
	policy := normalizeWriteTxRetryPolicy(shortRetry)
	if !policy.useAttemptDeadline {
		t.Fatal("normalized real-clock retry policy did not enable attempt deadlines")
	}
	var retryLoopSawDeadline bool
	if err := retryWriteTx(ctx, policy, func(attemptCtx context.Context) error {
		_, retryLoopSawDeadline = attemptCtx.Deadline()
		return nil
	}); err != nil {
		t.Fatalf("retryWriteTx returned error: %v", err)
	}
	if retryLoopSawDeadline {
		t.Fatal("first successful retryWriteTx attempt received an internal deadline")
	}

	var commitHookCalls int
	store, err := Open(ctx, Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  fixedNow,
		WriteTxCommitHookForTest: func(hookCtx context.Context, tx Tx, commit func(context.Context) error) error {
			commitHookCalls++
			if deadline, ok := hookCtx.Deadline(); ok {
				return fmt.Errorf("commit hook context has internal deadline %s", deadline.Format(time.RFC3339Nano))
			}
			return commit(hookCtx)
		},
		WriteTxRetry: shortRetry,
	})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('first-attempt-no-deadline', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		return err
	}); err != nil {
		t.Fatalf("WithWriteTx returned error: %v", err)
	}
	if commitHookCalls != 1 {
		t.Fatalf("commit hook calls = %d, want 1", commitHookCalls)
	}
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM projects WHERE id = 'first-attempt-no-deadline'`, 1)
}

func TestRetryWriteTxNormalAttemptCanOutliveRetryMaxElapsed(t *testing.T) {
	ctx := context.Background()
	clock := newManualWriteTxRetryClock(fixedNow())
	policy := normalizeWriteTxRetryPolicy(WriteTxRetryOptions{
		MaxAttempts: 2,
		MaxElapsed:  time.Millisecond,
		Backoff:     func(int) time.Duration { return time.Millisecond },
		Clock:       clock,
	})
	policy.useAttemptDeadline = true

	attempts := 0
	err := retryWriteTx(ctx, policy, func(attemptCtx context.Context) error {
		attempts++
		if deadline, ok := attemptCtx.Deadline(); ok {
			return fmt.Errorf("normal attempt has internal deadline %s", deadline.Format(time.RFC3339Nano))
		}
		clock.now = clock.now.Add(time.Hour)
		return nil
	})
	if err != nil {
		t.Fatalf("retryWriteTx returned error: %v", err)
	}
	if attempts != 1 || clock.sleeps != 0 {
		t.Fatalf("attempts/sleeps = %d/%d, want 1/0", attempts, clock.sleeps)
	}
	if !clock.now.After(fixedNow().Add(policy.maxElapsed)) {
		t.Fatalf("clock now = %s, want beyond retry max elapsed", clock.now.Format(time.RFC3339Nano))
	}
}

func TestRetryWriteTxBusinessDeadlineAfterBusyIsNotRetriedOrRewritten(t *testing.T) {
	ctx := context.Background()
	busyErr := storageSQLiteBusyError(t)
	clock := newManualWriteTxRetryClock(time.Now().Add(time.Hour))
	policy := normalizeWriteTxRetryPolicy(WriteTxRetryOptions{
		MaxAttempts: 3,
		MaxElapsed:  time.Minute,
		Backoff:     func(int) time.Duration { return 0 },
		Clock:       clock,
	})
	policy.useAttemptDeadline = true

	attempts := 0
	err := retryWriteTx(ctx, policy, func(attemptCtx context.Context) error {
		attempts++
		switch attempts {
		case 1:
			return busyErr
		case 2:
			if err := attemptCtx.Err(); err != nil {
				t.Fatalf("second attempt internal context error before business failure = %v, want nil", err)
			}
			if _, ok := attemptCtx.Deadline(); !ok {
				t.Fatal("second attempt did not receive retry internal deadline")
			}
			return context.DeadlineExceeded
		default:
			t.Fatalf("unexpected retry attempt %d after non-busy deadline", attempts)
			return nil
		}
	})
	if err != context.DeadlineExceeded {
		t.Fatalf("retryWriteTx error = %T %[1]v, want exact context.DeadlineExceeded", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if clock.sleeps != 0 {
		t.Fatalf("retry sleeps = %d, want 0 with zero backoff", clock.sleeps)
	}
}

func TestRetryWriteTxExpiredInternalDeadlineReturnsTypedBusy(t *testing.T) {
	ctx := context.Background()
	busyErr := storageSQLiteBusyError(t)
	clock := newManualWriteTxRetryClock(time.Now().Add(-time.Hour))
	policy := normalizeWriteTxRetryPolicy(WriteTxRetryOptions{
		MaxAttempts: 3,
		MaxElapsed:  time.Minute,
		Backoff:     func(int) time.Duration { return 0 },
		Clock:       clock,
	})
	policy.useAttemptDeadline = true

	attempts := 0
	err := retryWriteTx(ctx, policy, func(attemptCtx context.Context) error {
		attempts++
		switch attempts {
		case 1:
			return busyErr
		case 2:
			if err := attemptCtx.Err(); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("second attempt internal context error = %v, want context.DeadlineExceeded", err)
			}
			return attemptCtx.Err()
		default:
			t.Fatalf("unexpected retry attempt %d after internal deadline", attempts)
			return nil
		}
	})
	if err != busyErr {
		t.Fatalf("retryWriteTx error = %T %[1]v, want original typed busy %T %[2]v", err, busyErr)
	}
	if !IsBusy(err) {
		t.Fatalf("retryWriteTx error = %T %[1]v, want storage.IsBusy", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestWithWriteTxRetriesBusyBeginAfterRollbackBoundary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	clock := newManualWriteTxRetryClock(fixedNow())
	store, err := Open(ctx, Options{
		Path: path,
		Now:  fixedNow,
		WriteTxRetry: WriteTxRetryOptions{
			MaxAttempts: 3,
			MaxElapsed:  time.Minute,
			Backoff:     func(int) time.Duration { return time.Millisecond },
			Clock:       clock,
		},
	})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	setStoreBusyTimeout(t, ctx, store, 1)

	lockDB, lockConn := beginImmediateLock(t, ctx, path)
	defer lockDB.Close()
	lockReleased := false
	clock.onSleep = func() {
		if lockReleased {
			return
		}
		lockReleased = true
		if _, err := lockConn.ExecContext(ctx, `ROLLBACK`); err != nil {
			t.Fatalf("release lock: %v", err)
		}
		lockConn.Close()
	}

	closureCalls := 0
	err = store.WithWriteTx(ctx, func(tx Tx) error {
		closureCalls++
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('retry-success', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		return err
	})
	if err != nil {
		t.Fatalf("WithWriteTx returned error: %v", err)
	}
	if closureCalls != 1 {
		t.Fatalf("closure calls = %d, want only successful transaction attempt", closureCalls)
	}
	if clock.sleeps != 1 {
		t.Fatalf("retry sleeps = %d, want 1", clock.sleeps)
	}
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM projects WHERE id = 'retry-success'`, 1)
}

func TestWithWriteTxBusyExhaustionReturnsTypedOriginal(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	clock := newManualWriteTxRetryClock(fixedNow())
	store, err := Open(ctx, Options{
		Path: path,
		Now:  fixedNow,
		WriteTxRetry: WriteTxRetryOptions{
			MaxAttempts: 2,
			MaxElapsed:  time.Minute,
			Backoff:     func(int) time.Duration { return time.Millisecond },
			Clock:       clock,
		},
	})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	setStoreBusyTimeout(t, ctx, store, 1)
	lockDB, lockConn := beginImmediateLock(t, ctx, path)
	defer lockDB.Close()
	defer lockConn.Close()
	defer lockConn.ExecContext(ctx, `ROLLBACK`)

	closureCalls := 0
	err = store.WithWriteTx(ctx, func(tx Tx) error {
		closureCalls++
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('retry-exhausted', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		return err
	})
	if err == nil || !IsBusy(err) || !strings.Contains(err.Error(), "storage write transaction: begin immediate") {
		t.Fatalf("WithWriteTx error = %T %[1]v, want typed busy begin error", err)
	}
	if closureCalls != 0 {
		t.Fatalf("closure calls = %d, want 0 when BEGIN IMMEDIATE never succeeds", closureCalls)
	}
	if clock.sleeps != 1 {
		t.Fatalf("retry sleeps = %d, want 1", clock.sleeps)
	}
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM projects WHERE id = 'retry-exhausted'`, 0)
}

func TestWithWriteTxCancellationStopsBusyRetryBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	clock := newManualWriteTxRetryClock(fixedNow())
	store, err := Open(ctx, Options{
		Path: path,
		Now:  fixedNow,
		WriteTxRetry: WriteTxRetryOptions{
			MaxAttempts: 4,
			MaxElapsed:  time.Minute,
			Backoff:     func(int) time.Duration { return time.Second },
			Clock:       clock,
		},
	})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()
	setStoreBusyTimeout(t, ctx, store, 1)
	lockDB, lockConn := beginImmediateLock(t, context.Background(), path)
	defer lockDB.Close()
	defer lockConn.Close()
	defer lockConn.ExecContext(context.Background(), `ROLLBACK`)
	clock.onSleep = cancel

	err = store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('retry-cancelled', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WithWriteTx error = %v, want context.Canceled", err)
	}
	if clock.sleeps != 1 {
		t.Fatalf("retry sleeps = %d, want cancellation during first backoff", clock.sleeps)
	}
}

func TestWithWriteTxNonBusyErrorsAreNotRetriedAndRollback(t *testing.T) {
	ctx := context.Background()
	clock := newManualWriteTxRetryClock(fixedNow())
	store, err := Open(ctx, Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  fixedNow,
		WriteTxRetry: WriteTxRetryOptions{
			MaxAttempts: 4,
			MaxElapsed:  time.Minute,
			Backoff:     func(int) time.Duration { return time.Millisecond },
			Clock:       clock,
		},
	})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	wantErr := errors.New("validation failed")
	closureCalls := 0
	err = store.WithWriteTx(ctx, func(tx Tx) error {
		closureCalls++
		if _, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('non-busy-rollback', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithWriteTx error = %v, want %v", err, wantErr)
	}
	if closureCalls != 1 || clock.sleeps != 0 {
		t.Fatalf("closure calls/sleeps = %d/%d, want 1/0", closureCalls, clock.sleeps)
	}
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM projects WHERE id = 'non-busy-rollback'`, 0)
}

func TestWithWriteTxNonBusyCommitErrorIsNotRetriedAndRollback(t *testing.T) {
	ctx := context.Background()
	clock := newManualWriteTxRetryClock(fixedNow())
	wantErr := errors.New("commit hook failed")
	store, err := Open(ctx, Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  fixedNow,
		WriteTxCommitHookForTest: func(context.Context, Tx, func(context.Context) error) error {
			return wantErr
		},
		WriteTxRetry: WriteTxRetryOptions{
			MaxAttempts: 4,
			MaxElapsed:  time.Minute,
			Backoff:     func(int) time.Duration { return time.Millisecond },
			Clock:       clock,
		},
	})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer store.Close()

	closureCalls := 0
	err = store.WithWriteTx(ctx, func(tx Tx) error {
		closureCalls++
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('commit-non-busy-rollback', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
		return err
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithWriteTx error = %v, want %v", err, wantErr)
	}
	if closureCalls != 1 || clock.sleeps != 0 {
		t.Fatalf("closure calls/sleeps = %d/%d, want 1/0", closureCalls, clock.sleeps)
	}
	assertCountInStore(t, ctx, store, `SELECT COUNT(*) FROM projects WHERE id = 'commit-non-busy-rollback'`, 0)
}

func TestIsBusyRecognizesSQLiteLockedCode(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open(driverName, filepath.Join(t.TempDir(), "locked.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE t(id INTEGER PRIMARY KEY)`,
		`INSERT INTO t(id) VALUES (1), (2)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed locked fixture: %v", err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	rows, err := conn.QueryContext(ctx, `SELECT id FROM t`)
	if err != nil {
		t.Fatalf("open rows: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("expected first row")
	}
	_, err = conn.ExecContext(ctx, `DROP TABLE t`)
	if err == nil || !IsBusy(err) {
		t.Fatalf("DROP TABLE error = %T %[1]v, want SQLITE_LOCKED classified as busy", err)
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

type deliveryMigrationBackupRow struct {
	BackupID                 string
	SourceDBPath             string
	SourceSchemaVersion      int
	SourceDBHash             string
	BackupPath               string
	CreatedAt                string
	LoopcoderVersion         string
	MigrationPlanFingerprint string
}

func readOnlyDeliveryMigrationBackup(t *testing.T, ctx context.Context, store Store) deliveryMigrationBackupRow {
	t.Helper()
	var row deliveryMigrationBackupRow
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT backup_id, source_db_path, source_schema_version, source_db_hash, backup_path, created_at, loopcoder_version, migration_plan_fingerprint FROM delivery_migration_backups`).Scan(
			&row.BackupID,
			&row.SourceDBPath,
			&row.SourceSchemaVersion,
			&row.SourceDBHash,
			&row.BackupPath,
			&row.CreatedAt,
			&row.LoopcoderVersion,
			&row.MigrationPlanFingerprint,
		)
	}); err != nil {
		t.Fatalf("query delivery migration backup metadata: %v", err)
	}
	return row
}

func assertNoSourceDeliveryMigrationBackup(t *testing.T, row deliveryMigrationBackupRow, wantPath string) {
	t.Helper()
	wantPath = filepath.Clean(wantPath)
	if wantPath == "." {
		wantPath = ""
	}
	if !strings.HasPrefix(row.BackupID, "no_source_") {
		t.Fatalf("backup_id = %q, want no_source_ prefix", row.BackupID)
	}
	if row.SourceDBPath != wantPath {
		t.Fatalf("source_db_path = %q, want %q", row.SourceDBPath, wantPath)
	}
	if row.SourceSchemaVersion != 0 {
		t.Fatalf("source_schema_version = %d, want 0 for no source database", row.SourceSchemaVersion)
	}
	if row.SourceDBHash != "" {
		t.Fatalf("source_db_hash = %q, want empty for no source database", row.SourceDBHash)
	}
	if row.BackupPath != "" {
		t.Fatalf("backup_path = %q, want empty for no source database", row.BackupPath)
	}
	if row.CreatedAt != formatTimestamp(fixedNow()) {
		t.Fatalf("created_at = %q, want fixed time", row.CreatedAt)
	}
	if row.LoopcoderVersion != "0.8.0" {
		t.Fatalf("loopcoder_version = %q, want 0.8.0", row.LoopcoderVersion)
	}
	if row.MigrationPlanFingerprint == "" {
		t.Fatal("migration_plan_fingerprint is empty")
	}
}

func assertCountInStore(t *testing.T, ctx context.Context, store Store, query string, want int) {
	t.Helper()
	var got int
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, query).Scan(&got)
	}); err != nil {
		t.Fatalf("query count %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("%s = %d, want %d", query, got, want)
	}
}

func appendFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func backupPathSHA256(t *testing.T, ctx context.Context, path string) (string, error) {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open backup root: %v", err)
	}
	defer root.Close()
	return fileSHA256(ctx, root, filepath.Base(path), nil)
}

func copyWithinRoot(t *testing.T, rootPath, sourceName, targetName string) {
	t.Helper()
	if !filepath.IsLocal(sourceName) || sourceName == "." {
		t.Fatalf("source path %q is not local to root", sourceName)
	}
	if !filepath.IsLocal(targetName) || targetName == "." {
		t.Fatalf("target path %q is not local to root", targetName)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatalf("open copy root: %v", err)
	}
	defer root.Close()

	source, err := root.Open(sourceName)
	if err != nil {
		t.Fatalf("open copy source: %v", err)
	}
	defer source.Close()
	target, err := root.OpenFile(targetName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open copy target: %v", err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		t.Fatalf("copy backup: %v", err)
	}
	if err := target.Close(); err != nil {
		t.Fatalf("close copy target: %v", err)
	}
}

func assertBackupDirEmpty(t *testing.T, dbPath string) {
	t.Helper()
	backupDir := filepath.Join(filepath.Dir(dbPath), "backups")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("read backup dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("backup dir contains partial files: %v", names)
	}
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

func createV9NestedClaimLifecycleSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	createV8NestedClaimsSchema(t, db)
	for _, statement := range []string{
		`ALTER TABLE run_claims ADD COLUMN phase TEXT NOT NULL DEFAULT 'claimed'`,
		`ALTER TABLE run_claims ADD COLUMN provider_idempotency_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE run_claims ADD COLUMN provider_receipt TEXT NOT NULL DEFAULT ''`,
		`UPDATE run_claims
			SET phase = CASE
				WHEN EXISTS (
					SELECT 1 FROM runs r
					WHERE r.id = run_claims.run_id
						AND LOWER(TRIM(COALESCE(r.status, ''))) IN ('succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human', 'skipped', 'hung', 'idle', 'blocked')
				) THEN 'completed'
				ELSE 'executing'
			END
			WHERE phase = '' OR phase = 'claimed'`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (9, 'nested child claim lifecycle phase', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec v9 fixture statement: %v\n%s", err, statement)
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

type manualWriteTxRetryClock struct {
	now     time.Time
	sleeps  int
	onSleep func()
}

func newManualWriteTxRetryClock(now time.Time) *manualWriteTxRetryClock {
	return &manualWriteTxRetryClock{now: now}
}

func (c *manualWriteTxRetryClock) Now() time.Time {
	return c.now
}

func (c *manualWriteTxRetryClock) Sleep(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sleeps++
	c.now = c.now.Add(delay)
	if c.onSleep != nil {
		c.onSleep()
	}
	return ctx.Err()
}

func setStoreBusyTimeout(t *testing.T, ctx context.Context, store Store, millis int) {
	t.Helper()
	sqliteStore, ok := store.(*sqliteStore)
	if !ok {
		t.Fatalf("store type = %T, want *sqliteStore", store)
	}
	if _, err := sqliteStore.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, millis)); err != nil {
		t.Fatalf("set busy_timeout: %v", err)
	}
}

func beginImmediateLock(t *testing.T, ctx context.Context, path string) (*sql.DB, *sql.Conn) {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("open lock db: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 1`); err != nil {
		_ = db.Close()
		t.Fatalf("set lock busy_timeout: %v", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		t.Fatalf("lock conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		_ = db.Close()
		t.Fatalf("begin immediate lock: %v", err)
	}
	return db, conn
}

func storageSQLiteBusyError(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	db1, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("open busy db1: %v", err)
	}
	defer db1.Close()
	db2, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("open busy db2: %v", err)
	}
	defer db2.Close()
	for _, db := range []*sql.DB{db1, db2} {
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 1`); err != nil {
			t.Fatalf("set busy timeout: %v", err)
		}
	}
	conn1, err := db1.Conn(ctx)
	if err != nil {
		t.Fatalf("busy conn1: %v", err)
	}
	defer conn1.Close()
	if _, err := conn1.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin busy lock: %v", err)
	}
	defer conn1.ExecContext(ctx, `ROLLBACK`)
	_, err = db2.ExecContext(ctx, `BEGIN IMMEDIATE`)
	if err == nil || !IsBusy(err) {
		t.Fatalf("generated busy error = %T %[1]v, want typed SQLITE_BUSY", err)
	}
	return err
}

func fixedNow() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

func nestedSchedulerStorageBudgetRequest(childRunID string) SchedulerResourceReservationRequest {
	return SchedulerResourceReservationRequest{
		RootRunID:                "run-parent",
		ProviderKey:              "route-storage:codex:acct-storage:mcap-storage",
		RootMaxConcurrency:       3,
		ParentMaxConcurrency:     3,
		ProviderMaxConcurrency:   3,
		ProjectID:                "proj-storage-budget",
		DeliveryRunID:            "drun-storage-budget",
		TaskID:                   "task-storage-budget",
		SubAgentID:               childRunID,
		AdapterID:                "codex",
		AccountProfileID:         "acct-storage",
		ModelCapabilityID:        "mcap-storage",
		RoutingDecisionID:        "route-storage-budget",
		RoutingFingerprint:       "sha256:route-storage-budget",
		PlanFingerprint:          "sha256:plan-storage-budget",
		PolicyFingerprint:        "sha256:policy-storage-budget",
		AuthorizationFingerprint: "sha256:auth-storage-budget",
		BudgetRequestedValue:     25,
		BudgetQuantityKind:       "local-policy",
		BudgetUnit:               "local-policy-unit",
		BudgetWindowKind:         "unbounded",
	}
}

func seedNestedSchedulerStorageBudgetAuthority(ctx context.Context, store Store, req SchedulerResourceReservationRequest, candidateAdapter string, withAggregates bool) error {
	at := formatTimestamp(fixedNow())
	projectScope, err := nestedSchedulerBudgetScopeKey(nestedBudgetScope{ScopeKind: "project", ProjectID: req.ProjectID})
	if err != nil {
		return err
	}
	providerScope, err := nestedSchedulerBudgetScopeKey(nestedBudgetScope{
		ScopeKind:         "provider-scope",
		ProjectID:         req.ProjectID,
		AdapterID:         req.AdapterID,
		AccountProfileID:  req.AccountProfileID,
		ModelCapabilityID: req.ModelCapabilityID,
	})
	if err != nil {
		return err
	}
	candidates, err := json.Marshal([]map[string]any{{
		"routing_candidate_id":   "candidate-storage-budget",
		"task_id":                req.TaskID,
		"adapter_id":             candidateAdapter,
		"account_profile_id":     req.AccountProfileID,
		"model_capability_id":    req.ModelCapabilityID,
		"candidate_fingerprint":  "sha256:candidate-storage-budget",
		"invocation_profile_key": "default",
		"budget_policy_ids":      []string{"bpol-storage-project", "bpol-storage-provider"},
	}})
	if err != nil {
		return err
	}
	return store.WithWriteTx(ctx, func(tx Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
			req.ProjectID, "/tmp/"+req.ProjectID, at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_runs(
			delivery_run_id, run_id, schema_version, record_version, project_id, root_run_id, parent_run_id,
			state, intent_summary, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint,
			policy_version, max_side_effect_class, approval_status, override_status, created_at, updated_at,
			created_by_json, updated_by_json, host_json)
			VALUES (?, ?, 'loopcoder.delivery_run.v1', 1, ?, 'run-parent', '', 'approved', 'storage budget test',
				'sha256:input-storage-budget', ?, ?, ?, '0805.agent_federation.v1', 'repo-write', 'approved', 'none',
				?, ?, '{}', '{}', '{}')`,
			req.DeliveryRunID, "run-storage-delivery", req.ProjectID, req.PolicyFingerprint, req.PlanFingerprint, req.AuthorizationFingerprint, at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO routing_decisions(
			routing_decision_id, schema_version, record_version, project_id, delivery_run_id, task_id, task_requirement_id,
			decision_key, decision_kind, routing_policy_profile_id, role_definition_id, plan_fingerprint, policy_fingerprint,
			routing_fingerprint, candidate_generation_status, decision_status, chosen_candidate_id, terminal_error_code,
			input_record_refs_json, eligible_candidates_json, rejected_candidates_json, scored_candidates_json,
			rejected_summary_json, optimization_policy_json, payload_json, created_at, updated_at, decided_by_json, host_json)
			VALUES (?, 'loopcoder.routing_decision.v1', 1, ?, ?, ?, 'treq-storage-budget', 'route-storage-budget', 'routing',
				'rprofile-storage-budget', '', ?, ?, ?, 'full', 'selected', 'candidate-storage-budget', '',
				'[]', ?, '[]', '[]', '{}', '{}', '{}', ?, ?, '{}', '{}')`,
			req.RoutingDecisionID, req.ProjectID, req.DeliveryRunID, req.TaskID, req.PlanFingerprint, req.PolicyFingerprint, req.RoutingFingerprint, string(candidates), at, at); err != nil {
			return err
		}
		for _, policy := range []struct {
			id    string
			scope string
		}{
			{id: "bpol-storage-project", scope: projectScope},
			{id: "bpol-storage-provider", scope: providerScope},
		} {
			if _, err := tx.Exec(ctx, `INSERT INTO budget_policies(
				budget_policy_id, project_id, delivery_run_id, task_id, sub_agent_id, adapter_id, account_profile_id,
				model_capability_id, scope_kind, scope_key, quantity_kind, unit, window_kind, policy_mode,
				ceiling_value, active, policy_version, payload_json)
				VALUES (?, ?, '', '', '', ?, ?, ?, '', ?, 'local-policy', 'local-policy-unit', 'unbounded', 'hard',
					100, 1, '0805.agent_federation.v1', '{}')`,
				policy.id, req.ProjectID, req.AdapterID, req.AccountProfileID, req.ModelCapabilityID, policy.scope); err != nil {
				return err
			}
			if withAggregates {
				if _, err := tx.Exec(ctx, `INSERT INTO budget_aggregates(budget_policy_id, reserved_value, committed_value, updated_at) VALUES (?, 0, 0, ?)`,
					policy.id, at); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
