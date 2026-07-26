package projectschema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jasonhnd/loopcoder/internal/authoritystore"
)

// ProjectionCheckpoint is a rebuildable compact projection record.
type ProjectionCheckpoint struct {
	ProjectID      string
	Name           string
	Generation     int64
	ReducerVersion string
	InputSequence  int64
	OutputDigest   string
	PayloadJSON    string
	IsCurrent      bool
	UpdatedAt      time.Time
}

func ensureProjectionSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS projection_state (
			project_id TEXT NOT NULL,
			projection_name TEXT NOT NULL,
			generation INTEGER NOT NULL,
			reducer_version TEXT NOT NULL DEFAULT '1',
			input_sequence INTEGER NOT NULL DEFAULT 0,
			output_digest TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL DEFAULT '{}' CHECK (length(payload_json) <= 8192),
			is_current INTEGER NOT NULL DEFAULT 0 CHECK (is_current IN (0,1)),
			updated_at TEXT NOT NULL,
			PRIMARY KEY (project_id, projection_name, generation)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_projection_state_current
			ON projection_state(project_id, projection_name, is_current)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("projectschema: projection schema: %w", err)
		}
	}
	return nil
}

// GetCurrentCheckpoint returns the current generation for a projection, if any.
func GetCurrentCheckpoint(ctx context.Context, ps *authoritystore.ProjectStore, projectID, name string) (ProjectionCheckpoint, bool, error) {
	var cp ProjectionCheckpoint
	var found bool
	err := ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		if err := ensureProjectionSchema(ctx, db); err != nil {
			return err
		}
		var ts string
		var cur int
		err := db.QueryRowContext(ctx, `
			SELECT project_id, projection_name, generation, reducer_version, input_sequence,
			       output_digest, payload_json, is_current, updated_at
			FROM projection_state
			WHERE project_id=? AND projection_name=? AND is_current=1`,
			projectID, name,
		).Scan(&cp.ProjectID, &cp.Name, &cp.Generation, &cp.ReducerVersion, &cp.InputSequence,
			&cp.OutputDigest, &cp.PayloadJSON, &cur, &ts)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		cp.IsCurrent = cur == 1
		cp.UpdatedAt, _ = time.Parse(time.RFC3339Nano, ts)
		if !json.Valid([]byte(cp.PayloadJSON)) && cp.PayloadJSON != "{}" {
			return fmt.Errorf("%w: corrupt payload", ErrRebuildRequired)
		}
		return nil
	})
	if err != nil {
		return ProjectionCheckpoint{}, false, err
	}
	return cp, found, nil
}

// AdvanceCheckpoint CAS-updates the current projection from expectedInputSequence
// to next. On mismatch returns ErrCheckpointConflict.
func AdvanceCheckpoint(ctx context.Context, ps *authoritystore.ProjectStore, projectID, name, reducerVersion string, expectedInputSequence, nextSequence int64, payloadJSON string, now time.Time) (ProjectionCheckpoint, error) {
	if err := ValidatePayloadBound(payloadJSON); err != nil {
		return ProjectionCheckpoint{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	digest := payloadDigest(payloadJSON)
	var out ProjectionCheckpoint
	err := ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		if err := ensureProjectionSchema(ctx, db); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		var gen int64
		var inputSeq int64
		var ver string
		err = tx.QueryRowContext(ctx, `
			SELECT generation, input_sequence, reducer_version FROM projection_state
			WHERE project_id=? AND projection_name=? AND is_current=1`,
			projectID, name,
		).Scan(&gen, &inputSeq, &ver)
		if err == sql.ErrNoRows {
			// First checkpoint: only allow expected 0
			if expectedInputSequence != 0 {
				return ErrCheckpointConflict
			}
			gen = 1
			ts := now.UTC().Format(time.RFC3339Nano)
			_, err = tx.ExecContext(ctx, `
				INSERT INTO projection_state(
					project_id, projection_name, generation, reducer_version, input_sequence,
					output_digest, payload_json, is_current, updated_at
				) VALUES (?,?,?,?,?,?,?,1,?)`,
				projectID, name, gen, reducerVersion, nextSequence, digest, payloadJSON, ts,
			)
			if err != nil {
				return err
			}
			out = ProjectionCheckpoint{
				ProjectID: projectID, Name: name, Generation: gen, ReducerVersion: reducerVersion,
				InputSequence: nextSequence, OutputDigest: digest, PayloadJSON: payloadJSON,
				IsCurrent: true, UpdatedAt: now.UTC(),
			}
			return tx.Commit()
		}
		if err != nil {
			return err
		}
		if inputSeq != expectedInputSequence {
			return ErrCheckpointConflict
		}
		ts := now.UTC().Format(time.RFC3339Nano)
		res, err := tx.ExecContext(ctx, `
			UPDATE projection_state
			SET input_sequence=?, output_digest=?, payload_json=?, reducer_version=?, updated_at=?
			WHERE project_id=? AND projection_name=? AND generation=? AND input_sequence=?`,
			nextSequence, digest, payloadJSON, reducerVersion, ts,
			projectID, name, gen, expectedInputSequence,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrCheckpointConflict
		}
		out = ProjectionCheckpoint{
			ProjectID: projectID, Name: name, Generation: gen, ReducerVersion: reducerVersion,
			InputSequence: nextSequence, OutputDigest: digest, PayloadJSON: payloadJSON,
			IsCurrent: true, UpdatedAt: now.UTC(),
		}
		return tx.Commit()
	})
	return out, err
}

// BeginRebuild starts a non-current generation for offline rebuild from sequence 0.
// Event append is not blocked; rebuild writes a new generation then SwapCurrent.
func BeginRebuild(ctx context.Context, ps *authoritystore.ProjectStore, projectID, name, reducerVersion string, now time.Time) (ProjectionCheckpoint, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var out ProjectionCheckpoint
	err := ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensure(ctx, db); err != nil {
			return err
		}
		if err := ensureProjectionSchema(ctx, db); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var maxGen sql.NullInt64
		if err := tx.QueryRowContext(ctx, `
			SELECT MAX(generation) FROM projection_state WHERE project_id=? AND projection_name=?`,
			projectID, name,
		).Scan(&maxGen); err != nil {
			return err
		}
		gen := int64(1)
		if maxGen.Valid {
			gen = maxGen.Int64 + 1
		}
		ts := now.UTC().Format(time.RFC3339Nano)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO projection_state(
				project_id, projection_name, generation, reducer_version, input_sequence,
				output_digest, payload_json, is_current, updated_at
			) VALUES (?,?,?,?,0,'','{}',0,?)`,
			projectID, name, gen, reducerVersion, ts,
		)
		if err != nil {
			return err
		}
		out = ProjectionCheckpoint{
			ProjectID: projectID, Name: name, Generation: gen, ReducerVersion: reducerVersion,
			InputSequence: 0, IsCurrent: false, UpdatedAt: now.UTC(), PayloadJSON: "{}",
		}
		return tx.Commit()
	})
	return out, err
}

// WriteRebuildCheckpoint updates a non-current rebuild generation.
func WriteRebuildCheckpoint(ctx context.Context, ps *authoritystore.ProjectStore, projectID, name string, generation int64, reducerVersion string, inputSequence int64, payloadJSON string, now time.Time) error {
	if err := ValidatePayloadBound(payloadJSON); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	digest := payloadDigest(payloadJSON)
	return ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensureProjectionSchema(ctx, db); err != nil {
			return err
		}
		ts := now.UTC().Format(time.RFC3339Nano)
		res, err := db.ExecContext(ctx, `
			UPDATE projection_state
			SET reducer_version=?, input_sequence=?, output_digest=?, payload_json=?, updated_at=?
			WHERE project_id=? AND projection_name=? AND generation=? AND is_current=0`,
			reducerVersion, inputSequence, digest, payloadJSON, ts,
			projectID, name, generation,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("%w: rebuild generation not found", ErrRebuildRequired)
		}
		return nil
	})
}

// SwapCurrent atomically makes generation current and demotes previous current.
func SwapCurrent(ctx context.Context, ps *authoritystore.ProjectStore, projectID, name string, generation int64, now time.Time) error {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return ps.Foundation().WithDB(func(db *sql.DB) error {
		if err := ensureProjectionSchema(ctx, db); err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		ts := now.UTC().Format(time.RFC3339Nano)
		// Ensure target exists and is not already current incorrectly.
		var isCur int
		err = tx.QueryRowContext(ctx, `
			SELECT is_current FROM projection_state
			WHERE project_id=? AND projection_name=? AND generation=?`,
			projectID, name, generation,
		).Scan(&isCur)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: generation missing", ErrRebuildRequired)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE projection_state SET is_current=0, updated_at=?
			WHERE project_id=? AND projection_name=? AND is_current=1`,
			ts, projectID, name,
		); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE projection_state SET is_current=1, updated_at=?
			WHERE project_id=? AND projection_name=? AND generation=?`,
			ts, projectID, name, generation,
		)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("%w: swap failed", ErrRebuildRequired)
		}
		return tx.Commit()
	})
}

// RebuildProjection runs reducer from sequence zero into a new generation and swaps.
// Append is not blocked during rebuild (separate generation rows).
func RebuildProjection(ctx context.Context, ps *authoritystore.ProjectStore, projectID, name, reducerVersion string, reduce func(events []EventRow) (string, error), now time.Time) (ProjectionCheckpoint, error) {
	staging, err := BeginRebuild(ctx, ps, projectID, name, reducerVersion, now)
	if err != nil {
		return ProjectionCheckpoint{}, err
	}
	events, _, err := ReplayAll(ctx, ps, projectID, ZeroCursor(projectID), 200)
	if err != nil {
		return ProjectionCheckpoint{}, err
	}
	payload := "{}"
	if reduce != nil {
		payload, err = reduce(events)
		if err != nil {
			return ProjectionCheckpoint{}, err
		}
	}
	var lastSeq int64
	if len(events) > 0 {
		lastSeq = events[len(events)-1].Sequence
	}
	if err := WriteRebuildCheckpoint(ctx, ps, projectID, name, staging.Generation, reducerVersion, lastSeq, payload, now); err != nil {
		return ProjectionCheckpoint{}, err
	}
	if err := SwapCurrent(ctx, ps, projectID, name, staging.Generation, now); err != nil {
		return ProjectionCheckpoint{}, err
	}
	cp, ok, err := GetCurrentCheckpoint(ctx, ps, projectID, name)
	if err != nil {
		return ProjectionCheckpoint{}, err
	}
	if !ok {
		return ProjectionCheckpoint{}, fmt.Errorf("%w: missing after swap", ErrRebuildRequired)
	}
	return cp, nil
}

func payloadDigest(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}
