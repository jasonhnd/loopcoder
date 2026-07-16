package storage

var nestedSchedulerReservationSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS nested_scheduler_resource_reservations (
		reservation_id TEXT PRIMARY KEY,
		run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
		parent_run_id TEXT NOT NULL,
		root_run_id TEXT NOT NULL,
		resource_kind TEXT NOT NULL,
		resource_key TEXT NOT NULL,
		executor_id TEXT NOT NULL,
		claim_generation INTEGER NOT NULL,
		join_identity TEXT NOT NULL,
		state TEXT NOT NULL,
		lease_expires_at TEXT NOT NULL,
		heartbeat_at TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(run_id, resource_kind, resource_key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_nested_scheduler_reservations_resource ON nested_scheduler_resource_reservations(resource_kind, resource_key, state, lease_expires_at)`,
	`CREATE INDEX IF NOT EXISTS idx_nested_scheduler_reservations_run ON nested_scheduler_resource_reservations(run_id, claim_generation, state)`,
}
