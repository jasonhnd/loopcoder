package storage

var quotaTelemetrySchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS quota_telemetry_sources (
		quota_source_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL DEFAULT 1,
		adapter_id TEXT NOT NULL,
		source_kind TEXT NOT NULL,
		source_key TEXT NOT NULL,
		source_schema_version TEXT NOT NULL,
		network_declared INTEGER NOT NULL,
		unsupported_reason TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '{}',
		CHECK(json_valid(payload_json))
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_quota_telemetry_sources_adapter_key ON quota_telemetry_sources(adapter_id, source_key, source_schema_version)`,
	`CREATE INDEX IF NOT EXISTS idx_quota_telemetry_sources_kind ON quota_telemetry_sources(source_kind)`,
	`CREATE TABLE IF NOT EXISTS quota_snapshots (
		quota_snapshot_id TEXT PRIMARY KEY,
		quota_source_id TEXT NOT NULL REFERENCES quota_telemetry_sources(quota_source_id) ON DELETE RESTRICT,
		source_kind TEXT NOT NULL,
		adapter_id TEXT NOT NULL DEFAULT '',
		scope_key TEXT NOT NULL,
		quantity_kind TEXT NOT NULL,
		unit TEXT NOT NULL,
		window_kind TEXT NOT NULL,
		confidence TEXT NOT NULL,
		freshness_state TEXT NOT NULL,
		captured_at TEXT NOT NULL,
		stale_after TEXT NOT NULL DEFAULT '',
		terminal_error_code TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '{}',
		CHECK(json_valid(payload_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_quota_snapshots_adapter_captured ON quota_snapshots(adapter_id, captured_at)`,
	`CREATE INDEX IF NOT EXISTS idx_quota_snapshots_scope_window ON quota_snapshots(adapter_id, scope_key, quantity_kind, window_kind)`,
	`CREATE INDEX IF NOT EXISTS idx_quota_snapshots_confidence ON quota_snapshots(confidence, freshness_state)`,
}
