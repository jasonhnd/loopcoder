package storage

import (
	"context"
	"database/sql"
	"fmt"
)

var providerExecutionAuthoritySchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS provider_execution_authorities (
		authority_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL DEFAULT 1,
		project_id TEXT NOT NULL REFERENCES projects(id),
		run_id TEXT NOT NULL,
		attempt_id TEXT NOT NULL,
		provider_pid INTEGER NOT NULL,
		provider_pgid INTEGER NOT NULL DEFAULT 0,
		process_birth_identity TEXT NOT NULL DEFAULT '',
		executable_identity TEXT NOT NULL DEFAULT '',
		owner_id TEXT NOT NULL,
		claim_generation INTEGER NOT NULL,
		started_at TEXT NOT NULL,
		heartbeat_at TEXT NOT NULL,
		worktree_path TEXT NOT NULL,
		log_path TEXT NOT NULL,
		identity_ambiguous INTEGER NOT NULL DEFAULT 1,
		ambiguity_reason TEXT NOT NULL DEFAULT '',
		completed_at TEXT,
		terminal_state TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		CHECK(provider_pid > 0),
		CHECK(provider_pgid >= 0),
		CHECK(claim_generation > 0),
		CHECK(identity_ambiguous IN (0, 1)),
		UNIQUE(project_id, run_id, attempt_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_provider_execution_authorities_owner
		ON provider_execution_authorities(project_id, run_id, owner_id, claim_generation)`,
	`CREATE INDEX IF NOT EXISTS idx_provider_execution_authorities_process
		ON provider_execution_authorities(provider_pid, provider_pgid, process_birth_identity)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_execution_authorities_birth_unique
		ON provider_execution_authorities(provider_pid, provider_pgid, process_birth_identity)
		WHERE process_birth_identity <> ''`,
}

// migrateProviderExecutionSpawnPhase (v33): typed spawn phase on the authority
// row itself — same durable store as Persist, not a later event.
// DB default for migrated/pre-v33 rows is legacy_unknown (never auto-recoverable).
// New Persist must explicitly write authority_persisted in the INSERT.
// Idempotent: re-applying after a partial rewind of migrations must not fail.
func migrateProviderExecutionSpawnPhase(ctx context.Context, tx *sql.Tx) error {
	cols, err := tableColumns(ctx, tx, "provider_execution_authorities")
	if err != nil {
		return err
	}
	if cols["spawn_phase"] {
		return nil
	}
	// legacy_unknown: pre-v33 rows must NOT become authority_persisted by default.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE provider_execution_authorities ADD COLUMN spawn_phase TEXT NOT NULL DEFAULT 'legacy_unknown'`); err != nil {
		return fmt.Errorf("add provider_execution_authorities.spawn_phase: %w", err)
	}
	return nil
}
