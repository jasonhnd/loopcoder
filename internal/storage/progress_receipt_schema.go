package storage

// progressReceiptSchemaStatements creates the v25 append-only progress receipt
// store. Rollback is intentionally fail-closed: older binaries that only know
// schema v24 reject a database whose migrations table records v25. Operators
// that must downgrade should restore a pre-v25 database copy or export local
// runtime state before opening it with the older binary; this migration does
// not rewrite existing rows and is committed atomically with the migration row.
var progressReceiptSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS progress_receipts (
		storage_order INTEGER PRIMARY KEY AUTOINCREMENT,
		progress_receipt_id TEXT NOT NULL UNIQUE,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL DEFAULT 1,
		project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
		delivery_run_id TEXT NOT NULL,
		run_id TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL DEFAULT '',
		attempt_id TEXT NOT NULL DEFAULT '',
		attempt_ordinal INTEGER NOT NULL DEFAULT 0,
		correlation_id TEXT NOT NULL,
		correlation_sequence INTEGER NOT NULL,
		semantic_fingerprint TEXT NOT NULL,
		phase TEXT NOT NULL,
		status TEXT NOT NULL,
		provider_id TEXT NOT NULL,
		model_id TEXT NOT NULL,
		heartbeat_age_millis INTEGER NOT NULL DEFAULT -1,
		progress_age_millis INTEGER NOT NULL DEFAULT -1,
		occurred_at TEXT NOT NULL,
		persisted_at TEXT NOT NULL,
		task_counts_json TEXT NOT NULL DEFAULT '{}',
		provider_json TEXT NOT NULL DEFAULT '{}',
		heartbeat_json TEXT NOT NULL DEFAULT '{}',
		progress_json TEXT NOT NULL DEFAULT '{}',
		evidence_json TEXT NOT NULL DEFAULT '[]',
		quota_budget_json TEXT NOT NULL DEFAULT '{}',
		blocker_json TEXT NOT NULL DEFAULT '{}',
		next_action_json TEXT NOT NULL DEFAULT '{}',
		redaction_json TEXT NOT NULL DEFAULT '{}',
		gap_reasons_json TEXT NOT NULL DEFAULT '[]',
		payload_json TEXT NOT NULL,
		CHECK(record_version > 0),
		CHECK(correlation_sequence >= 0),
		CHECK(attempt_ordinal >= 0),
		CHECK(heartbeat_age_millis >= -1),
		CHECK(progress_age_millis >= -1),
		CHECK(json_valid(task_counts_json)),
		CHECK(json_valid(provider_json)),
		CHECK(json_valid(heartbeat_json)),
		CHECK(json_valid(progress_json)),
		CHECK(json_valid(evidence_json)),
		CHECK(json_valid(quota_budget_json)),
		CHECK(json_valid(blocker_json)),
		CHECK(json_valid(next_action_json)),
		CHECK(json_valid(redaction_json)),
		CHECK(json_valid(gap_reasons_json)),
		CHECK(json_valid(payload_json)),
		UNIQUE(project_id, delivery_run_id, correlation_id, semantic_fingerprint)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_progress_receipts_run_replay
		ON progress_receipts(project_id, delivery_run_id, occurred_at, correlation_id, correlation_sequence, storage_order)`,
	`CREATE INDEX IF NOT EXISTS idx_progress_receipts_correlation
		ON progress_receipts(project_id, delivery_run_id, correlation_id, correlation_sequence, storage_order)`,
	`CREATE INDEX IF NOT EXISTS idx_progress_receipts_status
		ON progress_receipts(project_id, delivery_run_id, phase, status)`,
}
