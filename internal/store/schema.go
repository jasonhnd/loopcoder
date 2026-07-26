package store

// foundationSchemaStatements is the durable compact-store foundation DDL.
// Later R1 slices may add domain tables through explicit migrations; this
// slice only establishes store metadata and the migration ledger.
var foundationSchemaStatements = []string{
	`CREATE TABLE store_metadata (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		store_id TEXT NOT NULL,
		format_identity TEXT NOT NULL,
		schema_version INTEGER NOT NULL,
		compatibility_floor INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		last_successful_migration INTEGER NOT NULL,
		last_migration_at TEXT NOT NULL
	)`,
	`CREATE TABLE migration_ledger (
		version INTEGER PRIMARY KEY,
		migration_id TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL,
		source_version INTEGER NOT NULL,
		target_version INTEGER NOT NULL,
		backup_manifest_pointer TEXT NOT NULL DEFAULT '',
		verification_result TEXT NOT NULL
	)`,
}
