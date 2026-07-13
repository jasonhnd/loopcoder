package storage

import (
	"context"
	"database/sql"
	"fmt"
)

var availabilitySchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS availability_observations (
		availability_observation_id TEXT PRIMARY KEY,
		observation_kind TEXT NOT NULL,
		scope_key TEXT NOT NULL,
		adapter_id TEXT NOT NULL DEFAULT '',
		provider_installation_id TEXT NOT NULL DEFAULT '',
		account_profile_id TEXT NOT NULL DEFAULT '',
		model_capability_id TEXT NOT NULL DEFAULT '',
		failure_class TEXT NOT NULL DEFAULT '',
		observed_at TEXT NOT NULL,
		confidence TEXT NOT NULL,
		source_record_ids_json TEXT NOT NULL DEFAULT '[]',
		payload_json TEXT NOT NULL DEFAULT '{}',
		CHECK(json_valid(source_record_ids_json)),
		CHECK(json_valid(payload_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_availability_observations_scope_time ON availability_observations(scope_key, observed_at)`,
	`CREATE INDEX IF NOT EXISTS idx_availability_observations_adapter_kind ON availability_observations(adapter_id, observation_kind, observed_at)`,
	`CREATE TABLE IF NOT EXISTS availability_scores (
		availability_score_id TEXT PRIMARY KEY,
		scope_key TEXT NOT NULL,
		adapter_id TEXT NOT NULL DEFAULT '',
		provider_installation_id TEXT NOT NULL DEFAULT '',
		account_profile_id TEXT NOT NULL DEFAULT '',
		model_capability_id TEXT NOT NULL DEFAULT '',
		score INTEGER NOT NULL,
		eligible INTEGER NOT NULL,
		score_confidence TEXT NOT NULL,
		hard_ineligible_reasons_json TEXT NOT NULL DEFAULT '[]',
		evidence_record_ids_json TEXT NOT NULL DEFAULT '[]',
		captured_at TEXT NOT NULL,
		policy_version TEXT NOT NULL,
		payload_json TEXT NOT NULL DEFAULT '{}',
		CHECK(score >= 0 AND score <= 100),
		CHECK(json_valid(hard_ineligible_reasons_json)),
		CHECK(json_valid(evidence_record_ids_json)),
		CHECK(json_valid(payload_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_availability_scores_scope_time ON availability_scores(scope_key, captured_at)`,
	`CREATE INDEX IF NOT EXISTS idx_availability_scores_adapter ON availability_scores(adapter_id, score, eligible)`,
	`CREATE TABLE IF NOT EXISTS circuit_breakers (
		circuit_breaker_id TEXT PRIMARY KEY,
		breaker_kind TEXT NOT NULL,
		state TEXT NOT NULL,
		scope_key TEXT NOT NULL,
		adapter_id TEXT NOT NULL DEFAULT '',
		model_capability_id TEXT NOT NULL DEFAULT '',
		open_until TEXT NOT NULL DEFAULT '',
		last_observation_id TEXT NOT NULL DEFAULT '',
		state_reason TEXT NOT NULL,
		policy_version TEXT NOT NULL,
		record_version INTEGER NOT NULL DEFAULT 1,
		payload_json TEXT NOT NULL DEFAULT '{}',
		CHECK(record_version > 0),
		CHECK(json_valid(payload_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_circuit_breakers_scope_state ON circuit_breakers(scope_key, state)`,
	`CREATE INDEX IF NOT EXISTS idx_circuit_breakers_adapter_kind ON circuit_breakers(adapter_id, breaker_kind, state)`,
}

var circuitBreakerRecoveryColumns = []struct {
	name string
	def  string
}{
	{name: "opened_at", def: "TEXT NOT NULL DEFAULT ''"},
	{name: "half_open_probe_budget", def: "INTEGER NOT NULL DEFAULT 0"},
	{name: "half_open_probe_count", def: "INTEGER NOT NULL DEFAULT 0"},
	{name: "failure_count", def: "INTEGER NOT NULL DEFAULT 0"},
	{name: "success_count", def: "INTEGER NOT NULL DEFAULT 0"},
	{name: "last_observation_at", def: "TEXT NOT NULL DEFAULT ''"},
	{name: "probe_lease_owner", def: "TEXT NOT NULL DEFAULT ''"},
	{name: "probe_lease_expires_at", def: "TEXT NOT NULL DEFAULT ''"},
	{name: "probe_lease_generation", def: "INTEGER NOT NULL DEFAULT 0"},
}

func migrateCircuitBreakerRecovery(ctx context.Context, tx *sql.Tx) error {
	cols, err := tableColumns(ctx, tx, "circuit_breakers")
	if err != nil {
		return err
	}
	for _, col := range circuitBreakerRecoveryColumns {
		if cols[col.name] {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE circuit_breakers ADD COLUMN %s %s`, col.name, col.def)); err != nil {
			return fmt.Errorf("add circuit_breakers.%s: %w", col.name, err)
		}
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_circuit_breakers_open_until ON circuit_breakers(state, open_until)`,
		`CREATE INDEX IF NOT EXISTS idx_circuit_breakers_probe_lease ON circuit_breakers(probe_lease_expires_at, probe_lease_owner)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}
