package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const lifecycleEventType = "run_lifecycle_transition"
const legacyLifecycleEventType = "legacy_lifecycle_import"

var errRunNotFound = errors.New("run not found")

// RunState is the durable lifecycle state for a loopcoder run.
type RunState string

const (
	RunStatePlanned    RunState = "planned"
	RunStateQueued     RunState = "queued"
	RunStateRunning    RunState = "running"
	RunStateWaiting    RunState = "waiting"
	RunStateSucceeded  RunState = "succeeded"
	RunStateFailed     RunState = "failed"
	RunStateCancelled  RunState = "cancelled"
	RunStateAbandoned  RunState = "abandoned"
	RunStateNeedsHuman RunState = "needs-human"
)

// RunRecord describes durable run metadata. Child runs set ParentRunID.
type RunRecord struct {
	ID          string
	ProjectID   string
	ParentRunID string
	RootRunID   string
	Depth       int
	Origin      string
	IssueNumber *int
	Status      RunState
	UpdatedAt   time.Time
}

// RunTransition records one explicit lifecycle transition.
type RunTransition struct {
	RunID        string
	From         RunState
	To           RunState
	At           time.Time
	Reason       string
	PayloadJSON  string
	EventType    string
	AllowNoOp    bool
	LegacyImport bool
}

// RunLifecycle is the current state plus inspectable transition history.
type RunLifecycle struct {
	RunID       string
	ParentRunID string
	RootRunID   string
	Depth       int
	Origin      string
	Status      RunState
	UpdatedAt   string
	History     []LifecycleEvent
}

// LifecycleEvent is one persisted lifecycle event for a run.
type LifecycleEvent struct {
	Sequence     int
	Timestamp    string
	EventType    string
	From         RunState
	To           RunState
	Reason       string
	PayloadJSON  string
	LegacyImport bool
}

// LegacyRunEvent is the conservative import shape for v0.6.x events.jsonl rows.
type LegacyRunEvent struct {
	Timestamp   string
	Phase       string
	Status      string
	Event       string
	Outcome     string
	PayloadJSON string
}

type lifecyclePayload struct {
	From          string `json:"from,omitempty"`
	To            string `json:"to"`
	Reason        string `json:"reason,omitempty"`
	LegacyStatus  string `json:"legacy_status,omitempty"`
	LegacyPhase   string `json:"legacy_phase,omitempty"`
	LegacyEvent   string `json:"legacy_event,omitempty"`
	LegacyOutcome string `json:"legacy_outcome,omitempty"`
}

var validTransitions = map[RunState]map[RunState]bool{
	RunStatePlanned: {
		RunStateQueued:    true,
		RunStateRunning:   true,
		RunStateCancelled: true,
		RunStateAbandoned: true,
	},
	RunStateQueued: {
		RunStateRunning:   true,
		RunStateCancelled: true,
		RunStateAbandoned: true,
	},
	RunStateRunning: {
		RunStateWaiting:    true,
		RunStateSucceeded:  true,
		RunStateFailed:     true,
		RunStateCancelled:  true,
		RunStateAbandoned:  true,
		RunStateNeedsHuman: true,
	},
	RunStateWaiting: {
		RunStateQueued:     true,
		RunStateRunning:    true,
		RunStateFailed:     true,
		RunStateCancelled:  true,
		RunStateAbandoned:  true,
		RunStateNeedsHuman: true,
	},
	RunStateNeedsHuman: {
		RunStateQueued:    true,
		RunStateRunning:   true,
		RunStateFailed:    true,
		RunStateCancelled: true,
		RunStateAbandoned: true,
	},
}

// IsRunState reports whether state is one of the durable lifecycle states.
func IsRunState(state RunState) bool {
	switch state {
	case RunStatePlanned, RunStateQueued, RunStateRunning, RunStateWaiting,
		RunStateSucceeded, RunStateFailed, RunStateCancelled, RunStateAbandoned,
		RunStateNeedsHuman:
		return true
	default:
		return false
	}
}

// CanTransitionRun reports whether a run may move directly from one state to another.
func CanTransitionRun(from, to RunState) bool {
	return validTransitions[from][to]
}

// AggregateRequiredChildState returns the deterministic parent aggregate for direct children.
//
// Intervention states take precedence over active states. This resolves the
// failed-vs-running conflict from spec 0646 by returning needs-human when any
// required child is failed, cancelled, abandoned, or needs-human.
func AggregateRequiredChildState(children []RunLifecycle) RunState {
	if len(children) == 0 {
		return RunStatePlanned
	}
	allSucceeded := true
	for _, child := range children {
		switch child.Status {
		case RunStateFailed, RunStateCancelled, RunStateAbandoned, RunStateNeedsHuman:
			return RunStateNeedsHuman
		case RunStateSucceeded:
		default:
			allSucceeded = false
		}
	}
	if allSucceeded {
		return RunStateSucceeded
	}
	return RunStateRunning
}

func (s *sqliteStore) UpsertRun(ctx context.Context, record RunRecord) error {
	if ctx == nil {
		ctx = context.Background()
	}
	record.ID = strings.TrimSpace(record.ID)
	if record.ID == "" {
		return errors.New("upsert run: id is required")
	}
	if record.Status == "" {
		record.Status = RunStatePlanned
	}
	if !IsRunState(record.Status) {
		return fmt.Errorf("upsert run %s: invalid lifecycle state %q", record.ID, record.Status)
	}
	if record.Origin == "" {
		record.Origin = "unknown"
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = s.now()
	}
	if record.ParentRunID == "" {
		if record.RootRunID == "" {
			record.RootRunID = record.ID
		}
		record.Depth = 0
	} else if err := s.fillChildRunMetadata(ctx, &record); err != nil {
		return err
	}

	return s.WithTx(ctx, func(tx Tx) error {
		issueNumber := any(nil)
		if record.IssueNumber != nil {
			issueNumber = *record.IssueNumber
		}
		_, err := tx.Exec(ctx, `INSERT INTO runs(id, project_id, parent_run_id, issue_number, status, updated_at, root_run_id, depth, origin)
			VALUES (?, nullif(?, ''), nullif(?, ''), ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				project_id = excluded.project_id,
				parent_run_id = excluded.parent_run_id,
				issue_number = excluded.issue_number,
				status = excluded.status,
				updated_at = excluded.updated_at,
				root_run_id = excluded.root_run_id,
				depth = excluded.depth,
				origin = excluded.origin`,
			record.ID, record.ProjectID, record.ParentRunID, issueNumber, string(record.Status),
			formatTimestamp(record.UpdatedAt), record.RootRunID, record.Depth, record.Origin)
		if err != nil {
			return fmt.Errorf("upsert run %s: %w", record.ID, err)
		}
		if record.ParentRunID == "" {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO run_edges(parent_run_id, child_run_id, edge_type, created_at, root_run_id, depth, status, updated_at)
			VALUES (?, ?, 'child', ?, ?, ?, ?, ?)
			ON CONFLICT(parent_run_id, child_run_id) DO UPDATE SET
				root_run_id = excluded.root_run_id,
				depth = excluded.depth,
				status = excluded.status,
				updated_at = excluded.updated_at`,
			record.ParentRunID, record.ID, formatTimestamp(record.UpdatedAt), record.RootRunID, record.Depth,
			string(record.Status), formatTimestamp(record.UpdatedAt))
		if err != nil {
			return fmt.Errorf("upsert run edge %s -> %s: %w", record.ParentRunID, record.ID, err)
		}
		return nil
	})
}

func (s *sqliteStore) TransitionRun(ctx context.Context, transition RunTransition) (RunLifecycle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	transition.RunID = strings.TrimSpace(transition.RunID)
	if transition.RunID == "" {
		return RunLifecycle{}, errors.New("transition run: run id is required")
	}
	if transition.To == "" || !IsRunState(transition.To) {
		return RunLifecycle{}, fmt.Errorf("transition run %s: invalid target lifecycle state %q", transition.RunID, transition.To)
	}
	if transition.At.IsZero() {
		transition.At = s.now()
	}
	if transition.EventType == "" {
		transition.EventType = lifecycleEventType
	}

	if err := s.WithTx(ctx, func(tx Tx) error {
		current, err := currentRunState(ctx, tx, transition.RunID)
		if err != nil {
			return err
		}
		if transition.From != "" && transition.From != current {
			return fmt.Errorf("transition run %s: current state is %q, not %q", transition.RunID, current, transition.From)
		}
		if current == transition.To && !transition.AllowNoOp {
			return fmt.Errorf("transition run %s: already in lifecycle state %q", transition.RunID, current)
		}
		if current != transition.To && !CanTransitionRun(current, transition.To) {
			return fmt.Errorf("transition run %s: invalid lifecycle transition %q -> %q", transition.RunID, current, transition.To)
		}
		return insertLifecycleTransition(ctx, tx, transition, current)
	}); err != nil {
		return RunLifecycle{}, err
	}
	return s.RunLifecycle(ctx, transition.RunID)
}

func (s *sqliteStore) RunLifecycle(ctx context.Context, runID string) (RunLifecycle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return RunLifecycle{}, errors.New("run lifecycle: run id is required")
	}
	var lifecycle RunLifecycle
	if err := s.WithTx(ctx, func(tx Tx) error {
		var status string
		err := tx.QueryRow(ctx, `SELECT id, coalesce(parent_run_id, ''), root_run_id, depth, origin, status, updated_at
			FROM runs WHERE id = ?`, runID).Scan(&lifecycle.RunID, &lifecycle.ParentRunID, &lifecycle.RootRunID,
			&lifecycle.Depth, &lifecycle.Origin, &status, &lifecycle.UpdatedAt)
		if err != nil {
			if errors.Is(err, errRunNotFound) {
				return fmt.Errorf("run lifecycle: run %q not found", runID)
			}
			return fmt.Errorf("run lifecycle %s: %w", runID, err)
		}
		lifecycle.Status = RunState(status)
		history, err := queryLifecycleHistory(ctx, tx, runID)
		if err != nil {
			return err
		}
		lifecycle.History = history
		return nil
	}); err != nil {
		return RunLifecycle{}, err
	}
	return lifecycle, nil
}

func (s *sqliteStore) ImportLegacyRunEvents(ctx context.Context, runID string, events []LegacyRunEvent) (RunLifecycle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return RunLifecycle{}, errors.New("import legacy run events: run id is required")
	}
	if err := s.WithTx(ctx, func(tx Tx) error {
		current, err := currentRunState(ctx, tx, runID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				now := s.now()
				_, err = tx.Exec(ctx, `INSERT INTO runs(id, status, updated_at, root_run_id, depth, origin)
					VALUES (?, ?, ?, ?, 0, ?)`, runID, string(RunStatePlanned), formatTimestamp(now), runID, "legacy-repo-local")
				if err != nil {
					return fmt.Errorf("import legacy run events %s: create run: %w", runID, err)
				}
				current = RunStatePlanned
			} else {
				return err
			}
		}
		for _, event := range events {
			mapped, ok := MapLegacyRunState(event)
			if !ok {
				continue
			}
			at := s.now()
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(event.Timestamp)); err == nil {
				at = parsed
			}
			payload, err := legacyPayloadJSON(current, mapped, event)
			if err != nil {
				return err
			}
			if err := insertLifecycleTransition(ctx, tx, RunTransition{
				RunID:        runID,
				To:           mapped,
				At:           at,
				Reason:       "legacy event import",
				PayloadJSON:  payload,
				EventType:    legacyLifecycleEventType,
				AllowNoOp:    true,
				LegacyImport: true,
			}, current); err != nil {
				return err
			}
			current = mapped
		}
		return nil
	}); err != nil {
		return RunLifecycle{}, err
	}
	return s.RunLifecycle(ctx, runID)
}

// MapLegacyRunState maps v0.6.x event status/phase fields to the v0.7 lifecycle.
func MapLegacyRunState(event LegacyRunEvent) (RunState, bool) {
	for _, raw := range []string{event.Status, event.Outcome, event.Event, event.Phase} {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "planned":
			return RunStatePlanned, true
		case "queued", "pending":
			return RunStateQueued, true
		case "running", "started", "codex_started", "dispatch_started", "review_started":
			return RunStateRunning, true
		case "waiting":
			return RunStateWaiting, true
		case "blocked", "stale", "hung", "idle":
			return RunStateNeedsHuman, true
		case "succeeded", "success", "passed", "pass", "done", "merged", "promoted":
			return RunStateSucceeded, true
		case "failed", "failure", "fail", "error":
			return RunStateFailed, true
		case "cancelled", "canceled":
			return RunStateCancelled, true
		case "abandoned":
			return RunStateAbandoned, true
		case "needs-human", "needs_human", "human", "manual":
			return RunStateNeedsHuman, true
		}
	}
	return "", false
}

func (s *sqliteStore) fillChildRunMetadata(ctx context.Context, record *RunRecord) error {
	var parentRoot string
	var parentDepth int
	err := s.db.QueryRowContext(ctx, `SELECT root_run_id, depth FROM runs WHERE id = ?`, record.ParentRunID).Scan(&parentRoot, &parentDepth)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("upsert run %s: parent run %q not found", record.ID, record.ParentRunID)
		}
		return fmt.Errorf("upsert run %s: inspect parent run %q: %w", record.ID, record.ParentRunID, err)
	}
	if record.RootRunID == "" {
		record.RootRunID = parentRoot
	}
	record.Depth = parentDepth + 1
	return nil
}

func currentRunState(ctx context.Context, tx Tx, runID string) (RunState, error) {
	var status string
	err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = ?`, runID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: %q", errRunNotFound, runID)
		}
		return "", fmt.Errorf("transition run %s: read current state: %w", runID, err)
	}
	state := RunState(status)
	if !IsRunState(state) {
		return "", fmt.Errorf("transition run %s: stored lifecycle state %q is invalid", runID, status)
	}
	return state, nil
}

func insertLifecycleTransition(ctx context.Context, tx Tx, transition RunTransition, from RunState) error {
	payload := strings.TrimSpace(transition.PayloadJSON)
	if payload == "" {
		data, err := json.Marshal(lifecyclePayload{
			From:   string(from),
			To:     string(transition.To),
			Reason: transition.Reason,
		})
		if err != nil {
			return fmt.Errorf("transition run %s: marshal lifecycle payload: %w", transition.RunID, err)
		}
		payload = string(data)
	}
	sequence, err := nextRunEventSequence(ctx, tx, transition.RunID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_events(id, run_id, sequence, ts, event_type, payload_json)
		VALUES (?, ?, ?, ?, ?, ?)`, lifecycleEventID(transition.RunID, sequence), transition.RunID, sequence,
		formatTimestamp(transition.At), transition.EventType, payload); err != nil {
		return fmt.Errorf("transition run %s: insert lifecycle event: %w", transition.RunID, err)
	}
	endedAt := any(nil)
	if isTerminalRunState(transition.To) {
		endedAt = formatTimestamp(transition.At)
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = ?, updated_at = ?,
		started_at = CASE WHEN ? = ? AND started_at IS NULL THEN ? ELSE started_at END,
		ended_at = CASE WHEN ? IS NULL THEN ended_at ELSE ? END
		WHERE id = ?`, string(transition.To), formatTimestamp(transition.At),
		string(transition.To), string(RunStateRunning), formatTimestamp(transition.At),
		endedAt, endedAt, transition.RunID); err != nil {
		return fmt.Errorf("transition run %s: update current lifecycle state: %w", transition.RunID, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE run_edges SET status = ?, updated_at = ? WHERE child_run_id = ?`,
		string(transition.To), formatTimestamp(transition.At), transition.RunID); err != nil {
		return fmt.Errorf("transition run %s: update child edge lifecycle state: %w", transition.RunID, err)
	}
	return nil
}

func queryLifecycleHistory(ctx context.Context, tx Tx, runID string) ([]LifecycleEvent, error) {
	rows, err := tx.Query(ctx, `SELECT sequence, ts, event_type, payload_json
		FROM run_events
		WHERE run_id = ? AND event_type IN (?, ?)
		ORDER BY sequence`, runID, lifecycleEventType, legacyLifecycleEventType)
	if err != nil {
		return nil, fmt.Errorf("run lifecycle %s: query history: %w", runID, err)
	}
	defer rows.Close()
	var history []LifecycleEvent
	for rows.Next() {
		var event LifecycleEvent
		if err := rows.Scan(&event.Sequence, &event.Timestamp, &event.EventType, &event.PayloadJSON); err != nil {
			return nil, fmt.Errorf("run lifecycle %s: scan history: %w", runID, err)
		}
		var payload lifecyclePayload
		if err := json.Unmarshal([]byte(event.PayloadJSON), &payload); err == nil {
			event.From = RunState(payload.From)
			event.To = RunState(payload.To)
			event.Reason = payload.Reason
		}
		event.LegacyImport = event.EventType == legacyLifecycleEventType
		history = append(history, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("run lifecycle %s: read history: %w", runID, err)
	}
	return history, nil
}

func nextRunEventSequence(ctx context.Context, tx Tx, runID string) (int, error) {
	var sequence int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM run_events WHERE run_id = ?`, runID).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("run %s: allocate lifecycle event sequence: %w", runID, err)
	}
	return sequence, nil
}

func lifecycleEventID(runID string, sequence int) string {
	return fmt.Sprintf("%s:lifecycle:%06d", runID, sequence)
}

func legacyPayloadJSON(from, to RunState, event LegacyRunEvent) (string, error) {
	if strings.TrimSpace(event.PayloadJSON) != "" {
		return event.PayloadJSON, nil
	}
	data, err := json.Marshal(lifecyclePayload{
		From:          string(from),
		To:            string(to),
		Reason:        "legacy event import",
		LegacyStatus:  event.Status,
		LegacyPhase:   event.Phase,
		LegacyEvent:   event.Event,
		LegacyOutcome: event.Outcome,
	})
	if err != nil {
		return "", fmt.Errorf("import legacy lifecycle event: marshal payload: %w", err)
	}
	return string(data), nil
}

func isTerminalRunState(state RunState) bool {
	switch state {
	case RunStateSucceeded, RunStateFailed, RunStateCancelled, RunStateAbandoned:
		return true
	default:
		return false
	}
}
