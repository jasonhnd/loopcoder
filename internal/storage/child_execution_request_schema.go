package storage

var childExecutionRequestSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS child_execution_requests (
		child_run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
		parent_run_id TEXT NOT NULL,
		plan_id TEXT NOT NULL,
		child_key TEXT NOT NULL,
		schema_version TEXT NOT NULL,
		request_json TEXT NOT NULL,
		contract_fingerprint TEXT NOT NULL,
		repository_identity TEXT NOT NULL,
		checkout_identity TEXT NOT NULL,
		permission TEXT NOT NULL,
		scope_json TEXT NOT NULL,
		claim_generation INTEGER NOT NULL DEFAULT 0,
		lifecycle_status TEXT NOT NULL,
		legacy_ambiguous INTEGER NOT NULL DEFAULT 0 CHECK (legacy_ambiguous IN (0, 1)),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(plan_id, child_key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_child_execution_requests_parent_run_id ON child_execution_requests(parent_run_id)`,
	`CREATE INDEX IF NOT EXISTS idx_child_execution_requests_plan_id ON child_execution_requests(plan_id)`,
	`CREATE INDEX IF NOT EXISTS idx_child_execution_requests_fingerprint ON child_execution_requests(contract_fingerprint)`,
	`INSERT INTO child_execution_requests(
		child_run_id, parent_run_id, plan_id, child_key, schema_version,
		request_json, contract_fingerprint, repository_identity, checkout_identity,
		permission, scope_json, claim_generation, lifecycle_status,
		legacy_ambiguous, created_at, updated_at
	)
	SELECT
		e.child_run_id,
		e.parent_run_id,
		e.plan_id,
		e.child_key,
		'legacy.ambiguous',
		'{}',
		'',
		'',
		'',
		e.permission,
		e.scope_json,
		COALESCE(c.claim_generation, 0),
		'needs-human',
		1,
		e.created_at,
		CASE WHEN TRIM(e.updated_at) <> '' THEN e.updated_at ELSE e.created_at END
	FROM run_edges e
	LEFT JOIN run_claims c ON c.run_id = e.child_run_id
	WHERE TRIM(e.plan_id) <> '' AND TRIM(e.child_key) <> ''
	ON CONFLICT(child_run_id) DO NOTHING`,
	`UPDATE run_edges
	SET status = 'needs-human',
		updated_at = CASE WHEN TRIM(updated_at) <> '' THEN updated_at ELSE created_at END
	WHERE child_run_id IN (SELECT child_run_id FROM child_execution_requests WHERE legacy_ambiguous = 1)
		AND LOWER(TRIM(COALESCE(status, ''))) NOT IN (
			'succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled',
			'timed_out', 'abandoned', 'needs-human', 'skipped', 'hung', 'idle',
			'blocked', 'lost'
		)`,
	`UPDATE runs
	SET status = 'needs-human',
		updated_at = CASE WHEN TRIM(updated_at) <> '' THEN updated_at ELSE created_at END,
		ended_at = COALESCE(NULLIF(ended_at, ''), NULLIF(updated_at, ''), created_at)
	WHERE id IN (SELECT child_run_id FROM child_execution_requests WHERE legacy_ambiguous = 1)
		AND LOWER(TRIM(COALESCE(status, ''))) NOT IN (
			'succeeded', 'succeeded_with_optional_failures', 'failed', 'cancelled',
			'timed_out', 'abandoned', 'needs-human', 'skipped', 'hung', 'idle',
			'blocked', 'lost'
		)`,
}
