package projectschema

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

// ReplayPage is one bounded page of events after a cursor.
type ReplayPage struct {
	Events     []EventRow
	NextCursor Cursor
	// Exhausted is true when no more events exist after NextCursor.
	Exhausted bool
}

// Replay reads committed events with sequence > cursor.Sequence, in stable
// ascending order, limited to pageSize (default 100, max 500).
func Replay(ctx context.Context, ps *authoritystore.ProjectStore, projectID string, cursor Cursor, pageSize int) (ReplayPage, error) {
	if ps == nil || ps.Foundation() == nil {
		return ReplayPage{}, fmt.Errorf("projectschema: nil project store")
	}
	if err := ValidateCursorForProject(cursor, projectID); err != nil {
		return ReplayPage{}, err
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	if pageSize > 500 {
		pageSize = 500
	}

	var page ReplayPage
	err := ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		if err := ensureProjectionSchema(ctx, db); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `
			SELECT event_id, project_id, aggregate_kind, aggregate_id, kind, envelope_version,
			       sequence, recorded_at, idempotency_key, payload_version, payload_json,
			       causal_event_id, evidence_ref_id
			FROM events
			WHERE project_id = ? AND sequence > ?
			ORDER BY sequence ASC
			LIMIT ?`, projectID, cursor.Sequence, pageSize)
		if err != nil {
			return err
		}
		defer rows.Close()
		var events []EventRow
		for rows.Next() {
			var e EventRow
			if err := rows.Scan(
				&e.EventID, &e.ProjectID, &e.AggregateKind, &e.AggregateID, &e.Kind, &e.EnvelopeVersion,
				&e.Sequence, &e.RecordedAt, &e.IdempotencyKey, &e.PayloadVersion, &e.PayloadJSON,
				&e.CausalEventID, &e.EvidenceRefID,
			); err != nil {
				return err
			}
			events = append(events, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		page.Events = events
		next := cursor
		if len(events) > 0 {
			next.Sequence = events[len(events)-1].Sequence
		}
		page.NextCursor = next
		// Empty tail or short page => exhausted for this query.
		page.Exhausted = len(events) < pageSize
		return nil
	})
	return page, err
}

// ReplayAll collects all events after cursor by paging (test helper).
func ReplayAll(ctx context.Context, ps *authoritystore.ProjectStore, projectID string, cursor Cursor, pageSize int) ([]EventRow, Cursor, error) {
	var all []EventRow
	cur := cursor
	for {
		page, err := Replay(ctx, ps, projectID, cur, pageSize)
		if err != nil {
			return nil, cur, err
		}
		all = append(all, page.Events...)
		cur = page.NextCursor
		if page.Exhausted {
			return all, cur, nil
		}
		if len(page.Events) == 0 {
			return all, cur, nil
		}
	}
}
