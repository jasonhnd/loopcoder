package storage

// v0.9.0 Work Graph persistence (V090-057). Compact project-scoped tables for
// immutable graph versions, WorkItems, typed dependencies, and lifecycle refs.
// Explicitly separate from v0.8 nested/federation schemas.
var workGraphSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS workgraph_versions (
		graph_version_row_id TEXT PRIMARY KEY,
		schema_version TEXT NOT NULL,
		project_id TEXT NOT NULL REFERENCES projects(id),
		graph_id TEXT NOT NULL,
		version INTEGER NOT NULL,
		plan_digest TEXT NOT NULL,
		source TEXT NOT NULL,
		explicit_opt_in INTEGER NOT NULL DEFAULT 0,
		approved_by TEXT NOT NULL DEFAULT '',
		direct_run_equivalent INTEGER NOT NULL DEFAULT 0,
		execution_started INTEGER NOT NULL DEFAULT 0,
		limits_json TEXT NOT NULL,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		obsolete INTEGER NOT NULL DEFAULT 0,
		CHECK(json_valid(limits_json)),
		CHECK(json_valid(payload_json)),
		CHECK(version >= 1),
		UNIQUE(project_id, graph_id, version),
		UNIQUE(project_id, graph_id, plan_digest)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_workgraph_versions_project ON workgraph_versions(project_id, graph_id, version)`,
	`CREATE TABLE IF NOT EXISTS workgraph_items (
		workitem_row_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES projects(id),
		graph_id TEXT NOT NULL,
		graph_version INTEGER NOT NULL,
		stable_key TEXT NOT NULL,
		intent TEXT NOT NULL,
		status TEXT NOT NULL,
		owner TEXT NOT NULL,
		ownership TEXT NOT NULL,
		route_requirement TEXT NOT NULL DEFAULT '',
		output_contract TEXT NOT NULL DEFAULT '',
		integration_order INTEGER NOT NULL,
		terminal TEXT NOT NULL DEFAULT '',
		attempt_id TEXT NOT NULL DEFAULT '',
		provider_child_ref TEXT NOT NULL DEFAULT '',
		github_ref TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '{}',
		CHECK(json_valid(payload_json)),
		CHECK(integration_order >= 1),
		CHECK(ownership = 'loopcoder_workitem'),
		UNIQUE(project_id, graph_id, graph_version, stable_key),
		FOREIGN KEY(project_id, graph_id, graph_version)
			REFERENCES workgraph_versions(project_id, graph_id, version)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_workgraph_items_version ON workgraph_items(project_id, graph_id, graph_version)`,
	`CREATE TABLE IF NOT EXISTS workgraph_dependencies (
		dep_row_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES projects(id),
		graph_id TEXT NOT NULL,
		graph_version INTEGER NOT NULL,
		from_key TEXT NOT NULL,
		to_key TEXT NOT NULL,
		kind TEXT NOT NULL,
		CHECK(from_key <> to_key),
		CHECK(kind IN ('finish_to_start', 'output', 'soft_order')),
		UNIQUE(project_id, graph_id, graph_version, from_key, to_key, kind),
		FOREIGN KEY(project_id, graph_id, graph_version)
			REFERENCES workgraph_versions(project_id, graph_id, version)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_workgraph_deps_version ON workgraph_dependencies(project_id, graph_id, graph_version)`,
	`CREATE TABLE IF NOT EXISTS workgraph_events (
		event_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES projects(id),
		graph_id TEXT NOT NULL,
		graph_version INTEGER NOT NULL,
		event_type TEXT NOT NULL,
		stable_key TEXT NOT NULL DEFAULT '',
		payload_json TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL,
		CHECK(json_valid(payload_json))
	)`,
	`CREATE INDEX IF NOT EXISTS idx_workgraph_events_graph ON workgraph_events(project_id, graph_id, created_at)`,
}
