package projectschema

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

// SeedProject inserts the singleton project_meta row for this project store.
// It is a test/setup helper, not a full lifecycle API.
func SeedProject(ctx context.Context, ps *authoritystore.ProjectStore, projectID, displayName string, now time.Time) error {
	if projectID == "" {
		return fmt.Errorf("projectschema: project_id required")
	}
	ts := now.UTC().Format(time.RFC3339Nano)
	return ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, `INSERT INTO project_meta(project_id, display_name, created_at, updated_at)
			VALUES(?,?,?,?)
			ON CONFLICT(project_id) DO UPDATE SET display_name=excluded.display_name, updated_at=excluded.updated_at`,
			projectID, displayName, ts, ts,
		)
		return err
	})
}

// InsertEventForTest inserts one event row for schema conformance tests.
// Production append belongs to V090-009.
func InsertEventForTest(ctx context.Context, ps *authoritystore.ProjectStore, row EventRow) error {
	if err := ValidatePayloadBound(row.PayloadJSON); err != nil {
		return err
	}
	if row.EnvelopeVersion == 0 {
		row.EnvelopeVersion = EventEnvelopeVersion
	}
	return ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		_, err := db.ExecContext(ctx, `INSERT INTO events(
			event_id, project_id, aggregate_kind, aggregate_id, kind, envelope_version,
			sequence, recorded_at, idempotency_key, payload_version, payload_json,
			causal_event_id, evidence_ref_id
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			row.EventID, row.ProjectID, row.AggregateKind, row.AggregateID, row.Kind, row.EnvelopeVersion,
			row.Sequence, row.RecordedAt, row.IdempotencyKey, row.PayloadVersion, row.PayloadJSON,
			row.CausalEventID, row.EvidenceRefID,
		)
		return err
	})
}

// EventRow is the durable event envelope.
type EventRow struct {
	EventID         string
	ProjectID       string
	AggregateKind   string
	AggregateID     string
	Kind            string
	EnvelopeVersion int
	Sequence        int64
	RecordedAt      string
	IdempotencyKey  string
	PayloadVersion  int
	PayloadJSON     string
	CausalEventID   string
	EvidenceRefID   string
}

// TryUpdateEvent attempts UPDATE and is expected to fail under immutability triggers.
func TryUpdateEvent(ctx context.Context, ps *authoritystore.ProjectStore, eventID, newPayload string) error {
	return ps.Foundation().WithDB(func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `UPDATE events SET payload_json=? WHERE event_id=?`, newPayload, eventID)
		return err
	})
}

// TryDeleteEvent attempts DELETE and is expected to fail under immutability triggers.
func TryDeleteEvent(ctx context.Context, ps *authoritystore.ProjectStore, eventID string) error {
	return ps.Foundation().WithDB(func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `DELETE FROM events WHERE event_id=?`, eventID)
		return err
	})
}
