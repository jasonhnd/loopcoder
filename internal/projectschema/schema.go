package projectschema

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

// EventEnvelopeVersion is the event row payload envelope version.
const EventEnvelopeVersion = 1

// MaxPayloadBytes is the hard bound for versioned JSON payloads.
const MaxPayloadBytes = 8192

// Ensure applies project-domain tables and immutability triggers to a project store.
func Ensure(ctx context.Context, ps *authoritystore.ProjectStore) error {
	if ps == nil || ps.Foundation() == nil {
		return fmt.Errorf("projectschema: nil project store")
	}
	return ps.Foundation().WithDB(func(db *sql.DB) error {
		return ensure(ctx, db)
	})
}

func ensure(ctx context.Context, db *sql.DB) error {
	for _, stmt := range ddl {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("projectschema: apply ddl: %w", err)
		}
	}
	for _, stmt := range immutabilityTriggers {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("projectschema: apply immutability trigger: %w", err)
		}
	}
	return nil
}

// ddl is the minimal project authority schema. Sequences and append logic land
// in V090-009; tables are ready for that writer.
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS project_meta (
		project_id TEXT PRIMARY KEY,
		display_name TEXT NOT NULL DEFAULT '',
		github_owner TEXT NOT NULL DEFAULT '',
		github_name TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS work_items (
		work_item_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		external_ref TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL CHECK (state IN (
			'observed','blocked','ready','claimed','active','in_review',
			'needs_human','done','failed','cancelled','superseded'
		)),
		title TEXT NOT NULL DEFAULT '',
		payload_version INTEGER NOT NULL DEFAULT 1,
		payload_json TEXT NOT NULL DEFAULT '{}' CHECK (length(payload_json) <= 8192),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY (project_id) REFERENCES project_meta(project_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_work_items_project_state ON work_items(project_id, state)`,
	`CREATE TABLE IF NOT EXISTS jobs (
		job_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		work_item_id TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN (
			'queued','claimed','running','waiting','blocked','verifying',
			'stopping','succeeded','failed','cancelled','timed_out','needs_human'
		)),
		route_digest TEXT NOT NULL DEFAULT '',
		payload_version INTEGER NOT NULL DEFAULT 1,
		payload_json TEXT NOT NULL DEFAULT '{}' CHECK (length(payload_json) <= 8192),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY (project_id) REFERENCES project_meta(project_id),
		FOREIGN KEY (work_item_id) REFERENCES work_items(work_item_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_jobs_work_item ON jobs(work_item_id, state)`,
	`CREATE TABLE IF NOT EXISTS attempts (
		attempt_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		job_id TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN (
			'queued','running','waiting','stopping','succeeded','failed',
			'cancelled','timed_out','needs_human'
		)),
		provider_key TEXT NOT NULL DEFAULT '',
		model_id TEXT NOT NULL DEFAULT '',
		route_digest TEXT NOT NULL DEFAULT '',
		machine_evidence_digest TEXT NOT NULL DEFAULT '',
		payload_version INTEGER NOT NULL DEFAULT 1,
		payload_json TEXT NOT NULL DEFAULT '{}' CHECK (length(payload_json) <= 8192),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY (project_id) REFERENCES project_meta(project_id),
		FOREIGN KEY (job_id) REFERENCES jobs(job_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_attempts_job ON attempts(job_id, state)`,
	// Append-only event family — lifecycle truth. Corrections append new rows.
	`CREATE TABLE IF NOT EXISTS events (
		event_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		aggregate_kind TEXT NOT NULL,
		aggregate_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		envelope_version INTEGER NOT NULL CHECK (envelope_version >= 1),
		sequence INTEGER NOT NULL,
		recorded_at TEXT NOT NULL,
		idempotency_key TEXT NOT NULL,
		payload_version INTEGER NOT NULL DEFAULT 1,
		payload_json TEXT NOT NULL DEFAULT '{}' CHECK (length(payload_json) <= 8192),
		causal_event_id TEXT NOT NULL DEFAULT '',
		evidence_ref_id TEXT NOT NULL DEFAULT '',
		UNIQUE (project_id, sequence),
		UNIQUE (project_id, idempotency_key),
		FOREIGN KEY (project_id) REFERENCES project_meta(project_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_events_aggregate ON events(project_id, aggregate_kind, aggregate_id, sequence)`,
	`CREATE TABLE IF NOT EXISTS projection_checkpoints (
		project_id TEXT NOT NULL,
		projection_name TEXT NOT NULL,
		last_sequence INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (project_id, projection_name),
		FOREIGN KEY (project_id) REFERENCES project_meta(project_id)
	)`,
	`CREATE TABLE IF NOT EXISTS external_evidence_refs (
		evidence_ref_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		surface TEXT NOT NULL CHECK (surface IN ('github','provider','local','human','ui')),
		external_id TEXT NOT NULL DEFAULT '',
		digest TEXT NOT NULL DEFAULT '',
		recorded_at TEXT NOT NULL,
		payload_version INTEGER NOT NULL DEFAULT 1,
		payload_json TEXT NOT NULL DEFAULT '{}' CHECK (length(payload_json) <= 8192),
		FOREIGN KEY (project_id) REFERENCES project_meta(project_id)
	)`,
	`CREATE TABLE IF NOT EXISTS ui_client_cursors (
		project_id TEXT NOT NULL,
		client_id TEXT NOT NULL,
		cursor_sequence INTEGER NOT NULL DEFAULT 0,
		capabilities_json TEXT NOT NULL DEFAULT '{}' CHECK (length(capabilities_json) <= 4096),
		updated_at TEXT NOT NULL,
		PRIMARY KEY (project_id, client_id),
		FOREIGN KEY (project_id) REFERENCES project_meta(project_id)
	)`,
	`CREATE TABLE IF NOT EXISTS ui_acknowledgements (
		ack_id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		client_id TEXT NOT NULL,
		sequence INTEGER NOT NULL,
		stage TEXT NOT NULL CHECK (stage IN ('persisted','streamed','accepted','rendered','seen')),
		recorded_at TEXT NOT NULL,
		UNIQUE (project_id, client_id, sequence, stage),
		FOREIGN KEY (project_id) REFERENCES project_meta(project_id)
	)`,
}

// immutabilityTriggers make event UPDATE/DELETE fail at the SQL layer.
// Production interfaces also omit update/delete helpers.
var immutabilityTriggers = []string{
	`CREATE TRIGGER IF NOT EXISTS events_no_update
		BEFORE UPDATE ON events
		BEGIN
			SELECT RAISE(ABORT, 'projectschema: events are immutable; append a correction event');
		END`,
	`CREATE TRIGGER IF NOT EXISTS events_no_delete
		BEFORE DELETE ON events
		BEGIN
			SELECT RAISE(ABORT, 'projectschema: events are immutable; append a correction event');
		END`,
}

// AssertNoMachineDomainTables fails if machine-global inventory tables appear.
func AssertNoMachineDomainTables(ctx context.Context, ps *authoritystore.ProjectStore) error {
	forbidden := []string{
		"provider_installations", "model_capabilities", "quota_observations",
		"provider_health", "resource_reservations", "project_registry",
	}
	return ps.Foundation().WithDB(func(db *sql.DB) error {
		for _, name := range forbidden {
			var n int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("projectschema: forbidden machine-domain table %s present", name)
			}
		}
		return nil
	})
}

// TableNames returns the domain tables owned by this package (excluding sqlite internals).
func TableNames() []string {
	return []string{
		"project_meta", "work_items", "jobs", "attempts", "events",
		"projection_checkpoints", "external_evidence_refs",
		"ui_client_cursors", "ui_acknowledgements",
	}
}

// ValidatePayloadBound rejects oversized payloads.
func ValidatePayloadBound(payload string) error {
	if len(payload) > MaxPayloadBytes {
		return fmt.Errorf("projectschema: payload exceeds %d bytes", MaxPayloadBytes)
	}
	if strings.Contains(strings.ToLower(payload), "sk-") ||
		strings.Contains(payload, "ghp_") ||
		strings.Contains(payload, "/Users/") {
		return fmt.Errorf("projectschema: payload contains forbidden content")
	}
	return nil
}
