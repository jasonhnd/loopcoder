package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

type deliveryMigrationBackup struct {
	BackupID                 string
	SourcePath               string
	SourceHash               string
	BackupPath               string
	CreatedAt                string
	MigrationPlanFingerprint string
}

func prepareDeliveryV10Backup(sourcePath, createdAt string) (*deliveryMigrationBackup, error) {
	sourcePath = filepath.Clean(sourcePath)
	if sourcePath == "." || sourcePath == "" {
		return &deliveryMigrationBackup{
			BackupID:                 "backup_" + hashStringsStorage("schema-v9-empty-source")[:24],
			CreatedAt:                createdAt,
			MigrationPlanFingerprint: "sha256:" + hashStringsStorage("loopcoder.delivery.migration.v1", "9", "10"),
		}, nil
	}
	sourceHash, err := fileSHA256(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("prepare delivery v10 backup hash: %w", err)
	}
	backupPath := filepath.Join(filepath.Dir(sourcePath), "backups", "schema-v9-"+sourceHash[:16]+".db")
	if _, err := os.Stat(backupPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect delivery v10 backup image: %w", err)
		}
		if err := copyFile(sourcePath, backupPath); err != nil {
			return nil, fmt.Errorf("prepare delivery v10 backup image: %w", err)
		}
	}
	return &deliveryMigrationBackup{
		BackupID:                 "backup_" + hashStringsStorage(sourceHash, "schema-v9-to-v10")[:24],
		SourcePath:               sourcePath,
		SourceHash:               sourceHash,
		BackupPath:               backupPath,
		CreatedAt:                createdAt,
		MigrationPlanFingerprint: "sha256:" + hashStringsStorage("loopcoder.delivery.migration.v1", "9", "10"),
	}, nil
}

func migrateDeliveryRunContracts(ctx context.Context, tx *sql.Tx, backup *deliveryMigrationBackup, createdAt string) error {
	for _, statement := range deliveryContractSchemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create delivery contract schema: %w", err)
		}
	}

	if backup != nil {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO delivery_migration_backups(
			backup_id,
			source_db_path,
			source_schema_version,
			source_db_hash,
			backup_path,
			created_at,
			loopcoder_version,
			migration_plan_fingerprint
		) VALUES (?, ?, 9, ?, ?, ?, ?, ?)`,
			backup.BackupID,
			backup.SourcePath,
			backup.SourceHash,
			backup.BackupPath,
			backup.CreatedAt,
			"0.8.0",
			backup.MigrationPlanFingerprint,
		); err != nil {
			return fmt.Errorf("record delivery v10 backup metadata: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO delivery_runs(
			delivery_run_id,
			run_id,
			schema_version,
			record_version,
			project_id,
			root_run_id,
			parent_run_id,
			state,
			intent_summary,
			policy_version,
			max_side_effect_class,
			approval_status,
			override_status,
			created_at,
			updated_at,
			started_at,
			ended_at,
			created_by_json,
			updated_by_json,
			host_json,
			terminal_error_code,
			legacy_id_source
		)
		SELECT
			r.id,
			r.id,
			'loopcoder.delivery_run.v1',
			1,
			r.project_id,
			CASE WHEN r.root_run_id <> '' THEN r.root_run_id ELSE r.id END,
			NULLIF(r.parent_run_id, ''),
			CASE
				WHEN EXISTS (
					SELECT 1 FROM run_claims c
					WHERE c.run_id = r.id
						AND LOWER(TRIM(c.phase)) IN ('launching', 'executing')
						AND COALESCE(c.provider_receipt, '') = ''
						AND LOWER(TRIM(COALESCE(r.status, ''))) NOT IN ('succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human', 'skipped', 'hung', 'idle', 'blocked')
				) THEN 'needs-human'
				ELSE CASE LOWER(TRIM(COALESCE(r.status, '')))
					WHEN 'queued' THEN 'queued'
					WHEN 'running' THEN 'running'
					WHEN 'launching' THEN 'running'
					WHEN 'waiting' THEN 'paused'
					WHEN 'succeeded' THEN 'succeeded'
					WHEN 'succeeded_with_optional_failures' THEN 'succeeded'
					WHEN 'failed' THEN 'failed'
					WHEN 'cancelled' THEN 'cancelled'
					WHEN 'canceled' THEN 'cancelled'
					WHEN 'abandoned' THEN 'abandoned'
					WHEN 'needs-human' THEN 'needs-human'
					WHEN 'needs_human' THEN 'needs-human'
					ELSE 'needs-human'
				END
			END,
			'legacy v0.7 run imported without v0.8 plan authority',
			'legacy-v0.7-import',
			'local-write',
			'not-required',
			'none',
			COALESCE(NULLIF(r.created_at, ''), NULLIF(r.started_at, ''), r.updated_at),
			r.updated_at,
			NULLIF(r.started_at, ''),
			NULLIF(r.ended_at, ''),
			'{"actor_kind":"migration","actor_id":"schema-v10","display":"v0.8 schema migration","decision_authority":"migration","source":"storage"}',
			'{"actor_kind":"migration","actor_id":"schema-v10","display":"v0.8 schema migration","decision_authority":"migration","source":"storage"}',
			'{"host_kind":"migration","host_id":"local-storage","session_id":"schema-v10","process_id":0,"loopcoder_version":"0.8.0","platform":""}',
			CASE
				WHEN EXISTS (
					SELECT 1 FROM run_claims c
					WHERE c.run_id = r.id
						AND LOWER(TRIM(c.phase)) IN ('launching', 'executing')
						AND COALESCE(c.provider_receipt, '') = ''
						AND LOWER(TRIM(COALESCE(r.status, ''))) NOT IN ('succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled', 'timed_out', 'abandoned', 'needs-human', 'skipped', 'hung', 'idle', 'blocked')
				) THEN 'ErrAmbiguousLegacyState'
				ELSE ''
			END,
			'runs.id'
		FROM runs r
		WHERE COALESCE(r.project_id, '') <> ''`); err != nil {
		return fmt.Errorf("migrate legacy runs to delivery_runs: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO delivery_legacy_imports(
			legacy_import_id,
			project_id,
			delivery_run_id,
			legacy_table,
			legacy_record_id,
			migration_status,
			error_code,
			diagnostic,
			created_at
		)
		SELECT
			'legacy_' || lower(hex(CAST(r.project_id || char(0) || r.id || char(0) || 'runs' AS BLOB))),
			r.project_id,
			r.id,
			'runs',
			r.id,
			CASE
				WHEN d.terminal_error_code = 'ErrAmbiguousLegacyState' THEN 'needs-human'
				ELSE 'imported'
			END,
			d.terminal_error_code,
			CASE
				WHEN d.terminal_error_code = 'ErrAmbiguousLegacyState' THEN 'legacy non-terminal launch or execution claim cannot prove provider side-effect state'
				ELSE 'legacy run imported conservatively without v0.8 plan authority'
			END,
			?
		FROM runs r
		JOIN delivery_runs d ON d.delivery_run_id = r.id
		WHERE COALESCE(r.project_id, '') <> ''`, createdAt); err != nil {
		return fmt.Errorf("record delivery legacy import diagnostics: %w", err)
	}

	return nil
}

var deliveryContractSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS delivery_runs (
		delivery_run_id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL UNIQUE,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL,
		project_id TEXT NOT NULL REFERENCES projects(id),
		root_run_id TEXT NOT NULL,
		parent_run_id TEXT,
		state TEXT NOT NULL,
		intent_summary TEXT NOT NULL,
		input_fingerprint TEXT NOT NULL DEFAULT '',
		policy_fingerprint TEXT NOT NULL DEFAULT '',
		plan_fingerprint TEXT NOT NULL DEFAULT '',
		authorization_fingerprint TEXT NOT NULL DEFAULT '',
		policy_version TEXT NOT NULL,
		max_side_effect_class TEXT NOT NULL,
		approval_status TEXT NOT NULL,
		override_status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		planned_at TEXT,
		approved_at TEXT,
		started_at TEXT,
		ended_at TEXT,
		created_by_json TEXT NOT NULL,
		updated_by_json TEXT NOT NULL,
		host_json TEXT NOT NULL,
		terminal_error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		report_ids_json TEXT NOT NULL DEFAULT '[]',
		legacy_id_source TEXT NOT NULL DEFAULT '',
		CHECK(json_valid(created_by_json)),
		CHECK(json_valid(updated_by_json)),
		CHECK(json_valid(host_json)),
		CHECK(json_valid(report_ids_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_runs_project_state ON delivery_runs(project_id, state)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_runs_authorization_fingerprint ON delivery_runs(authorization_fingerprint)`,
	`CREATE TABLE IF NOT EXISTS delivery_plan_fingerprints (
		fingerprint_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL DEFAULT 1,
		project_id TEXT NOT NULL REFERENCES projects(id),
		delivery_run_id TEXT NOT NULL REFERENCES delivery_runs(delivery_run_id) ON DELETE CASCADE,
		input_fingerprint TEXT NOT NULL,
		policy_fingerprint TEXT NOT NULL,
		plan_fingerprint TEXT NOT NULL,
		authorization_fingerprint TEXT NOT NULL,
		canonicalization_version TEXT NOT NULL,
		algorithm TEXT NOT NULL,
		canonical_inputs_json TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		created_by_json TEXT NOT NULL,
		host_json TEXT NOT NULL,
		CHECK(json_valid(canonical_inputs_json)),
		CHECK(json_valid(created_by_json)),
		CHECK(json_valid(host_json)),
		UNIQUE(project_id, delivery_run_id, authorization_fingerprint)
	)`,
	`CREATE TABLE IF NOT EXISTS delivery_tasks (
		task_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL,
		project_id TEXT NOT NULL REFERENCES projects(id),
		delivery_run_id TEXT NOT NULL REFERENCES delivery_runs(delivery_run_id) ON DELETE CASCADE,
		task_key TEXT NOT NULL,
		state TEXT NOT NULL,
		title TEXT NOT NULL,
		requirements_json TEXT NOT NULL,
		scope_json TEXT NOT NULL,
		permission TEXT NOT NULL,
		side_effect_class TEXT NOT NULL,
		policy_version TEXT NOT NULL,
		plan_fingerprint TEXT NOT NULL,
		authorization_fingerprint TEXT NOT NULL DEFAULT '',
		attempt_count INTEGER NOT NULL DEFAULT 0,
		active_attempt_id TEXT,
		depends_on_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		ready_at TEXT,
		started_at TEXT,
		ended_at TEXT,
		created_by_json TEXT NOT NULL,
		updated_by_json TEXT NOT NULL,
		host_json TEXT NOT NULL,
		terminal_error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		CHECK(json_valid(requirements_json)),
		CHECK(json_valid(scope_json)),
		CHECK(json_valid(depends_on_json)),
		CHECK(json_valid(created_by_json)),
		CHECK(json_valid(updated_by_json)),
		CHECK(json_valid(host_json)),
		UNIQUE(project_id, delivery_run_id, task_key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_tasks_run_state ON delivery_tasks(delivery_run_id, state)`,
	`CREATE TABLE IF NOT EXISTS delivery_dependency_edges (
		edge_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL DEFAULT 1,
		project_id TEXT NOT NULL REFERENCES projects(id),
		delivery_run_id TEXT NOT NULL REFERENCES delivery_runs(delivery_run_id) ON DELETE CASCADE,
		from_task_id TEXT NOT NULL REFERENCES delivery_tasks(task_id) ON DELETE CASCADE,
		to_task_id TEXT NOT NULL REFERENCES delivery_tasks(task_id) ON DELETE CASCADE,
		edge_kind TEXT NOT NULL,
		ordinal INTEGER NOT NULL,
		plan_fingerprint TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		created_by_json TEXT NOT NULL,
		host_json TEXT NOT NULL,
		CHECK(json_valid(created_by_json)),
		CHECK(json_valid(host_json)),
		UNIQUE(delivery_run_id, from_task_id, to_task_id, edge_kind)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_dependency_edges_to ON delivery_dependency_edges(to_task_id)`,
	`CREATE TABLE IF NOT EXISTS delivery_attempts (
		attempt_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL,
		project_id TEXT NOT NULL REFERENCES projects(id),
		delivery_run_id TEXT NOT NULL REFERENCES delivery_runs(delivery_run_id) ON DELETE CASCADE,
		task_id TEXT NOT NULL REFERENCES delivery_tasks(task_id) ON DELETE CASCADE,
		attempt_ordinal INTEGER NOT NULL,
		state TEXT NOT NULL,
		claim_generation INTEGER,
		executor_id TEXT NOT NULL DEFAULT '',
		provider_idempotency_key TEXT NOT NULL DEFAULT '',
		provider_receipt TEXT NOT NULL DEFAULT '',
		side_effect_class TEXT NOT NULL,
		started_at TEXT,
		ended_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		created_by_json TEXT NOT NULL,
		updated_by_json TEXT NOT NULL,
		host_json TEXT NOT NULL,
		report_id TEXT NOT NULL DEFAULT '',
		terminal_error_code TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		CHECK(json_valid(created_by_json)),
		CHECK(json_valid(updated_by_json)),
		CHECK(json_valid(host_json)),
		UNIQUE(task_id, attempt_ordinal)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_attempts_task_state ON delivery_attempts(task_id, state)`,
	`CREATE TABLE IF NOT EXISTS delivery_decisions (
		decision_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL DEFAULT 1,
		project_id TEXT NOT NULL REFERENCES projects(id),
		delivery_run_id TEXT NOT NULL REFERENCES delivery_runs(delivery_run_id) ON DELETE CASCADE,
		task_id TEXT REFERENCES delivery_tasks(task_id) ON DELETE CASCADE,
		decision_key TEXT NOT NULL,
		decision_kind TEXT NOT NULL,
		decided_by_json TEXT NOT NULL,
		inputs_fingerprint TEXT NOT NULL,
		output_json TEXT NOT NULL,
		alternatives_json TEXT NOT NULL DEFAULT 'null',
		heuristic INTEGER NOT NULL,
		heuristic_reason TEXT NOT NULL DEFAULT '',
		policy_version TEXT NOT NULL,
		side_effect_class TEXT NOT NULL,
		created_at TEXT NOT NULL,
		created_by_json TEXT NOT NULL,
		host_json TEXT NOT NULL,
		CHECK(json_valid(decided_by_json)),
		CHECK(json_valid(output_json)),
		CHECK(json_valid(alternatives_json)),
		CHECK(json_valid(created_by_json)),
		CHECK(json_valid(host_json)),
		UNIQUE(project_id, delivery_run_id, decision_key)
	)`,
	`CREATE TABLE IF NOT EXISTS delivery_approvals (
		approval_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL,
		project_id TEXT NOT NULL REFERENCES projects(id),
		delivery_run_id TEXT NOT NULL REFERENCES delivery_runs(delivery_run_id) ON DELETE CASCADE,
		approval_kind TEXT NOT NULL,
		authorization_fingerprint TEXT NOT NULL,
		input_fingerprint TEXT NOT NULL,
		policy_fingerprint TEXT NOT NULL,
		plan_fingerprint TEXT NOT NULL,
		approved_side_effect_class TEXT NOT NULL,
		approved_scope_json TEXT NOT NULL,
		approved_by_json TEXT NOT NULL,
		status TEXT NOT NULL,
		approved_at TEXT,
		expires_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		created_by_json TEXT NOT NULL,
		updated_by_json TEXT NOT NULL,
		host_json TEXT NOT NULL,
		CHECK(json_valid(approved_scope_json)),
		CHECK(json_valid(approved_by_json)),
		CHECK(json_valid(created_by_json)),
		CHECK(json_valid(updated_by_json)),
		CHECK(json_valid(host_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_approvals_authorization ON delivery_approvals(project_id, delivery_run_id, authorization_fingerprint, status)`,
	`CREATE TABLE IF NOT EXISTS delivery_overrides (
		override_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL,
		project_id TEXT NOT NULL REFERENCES projects(id),
		delivery_run_id TEXT NOT NULL REFERENCES delivery_runs(delivery_run_id) ON DELETE CASCADE,
		override_kind TEXT NOT NULL,
		authorization_fingerprint TEXT NOT NULL,
		input_fingerprint TEXT NOT NULL,
		policy_fingerprint TEXT NOT NULL,
		plan_fingerprint TEXT NOT NULL,
		policy_decision_id TEXT NOT NULL REFERENCES delivery_decisions(decision_id),
		overridden_by_json TEXT NOT NULL,
		justification TEXT NOT NULL,
		limits_json TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		expires_at TEXT,
		created_by_json TEXT NOT NULL,
		updated_by_json TEXT NOT NULL,
		host_json TEXT NOT NULL,
		CHECK(json_valid(overridden_by_json)),
		CHECK(json_valid(limits_json)),
		CHECK(json_valid(created_by_json)),
		CHECK(json_valid(updated_by_json)),
		CHECK(json_valid(host_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_overrides_authorization ON delivery_overrides(project_id, delivery_run_id, authorization_fingerprint, status)`,
	`CREATE TABLE IF NOT EXISTS delivery_idempotency (
		idempotency_key TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		delivery_run_id TEXT NOT NULL,
		operation TEXT NOT NULL,
		request_hash TEXT NOT NULL,
		request_json TEXT NOT NULL,
		result_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		CHECK(json_valid(request_json)),
		CHECK(json_valid(result_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_delivery_idempotency_run ON delivery_idempotency(project_id, delivery_run_id)`,
	`CREATE TABLE IF NOT EXISTS delivery_migration_backups (
		backup_id TEXT PRIMARY KEY,
		source_db_path TEXT NOT NULL,
		source_schema_version INTEGER NOT NULL,
		source_db_hash TEXT NOT NULL,
		backup_path TEXT NOT NULL,
		created_at TEXT NOT NULL,
		loopcoder_version TEXT NOT NULL,
		migration_plan_fingerprint TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS delivery_legacy_imports (
		legacy_import_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES projects(id),
		delivery_run_id TEXT REFERENCES delivery_runs(delivery_run_id) ON DELETE CASCADE,
		legacy_table TEXT NOT NULL,
		legacy_record_id TEXT NOT NULL,
		migration_status TEXT NOT NULL,
		error_code TEXT NOT NULL DEFAULT '',
		diagnostic TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}

func hashStringsStorage(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}
