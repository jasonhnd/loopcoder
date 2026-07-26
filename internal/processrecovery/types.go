package processrecovery

import (
	"errors"
	"time"
)

const SchemaVersion = "loopcoder.process_recovery.v1"

// DecisionKind is the recovery classification.
type DecisionKind string

const (
	DecisionAdopt             DecisionKind = "adopt"
	DecisionNeverStarted      DecisionKind = "never_started"
	DecisionExitedUnrecorded  DecisionKind = "exited_unrecorded"
	DecisionPIDReused         DecisionKind = "pid_reused"
	DecisionDescendantsOnly   DecisionKind = "descendants_only"
	DecisionUnknown           DecisionKind = "unknown"
	DecisionTerminalClean     DecisionKind = "terminal_clean"
	DecisionAttentionRequired DecisionKind = "attention_required"
)

// OperatorAction is an explicit next step (never silent launch).
type OperatorAction string

const (
	ActionNone            OperatorAction = "none"
	ActionContinueObserve OperatorAction = "continue_observe"
	ActionNewAttempt      OperatorAction = "new_attempt_required"
	ActionJoinFinalize    OperatorAction = "join_finalize"
	ActionHumanAttention  OperatorAction = "human_attention"
	ActionAlreadyTerminal OperatorAction = "already_terminal"
)

var (
	ErrInvalidEvidence = errors.New("processrecovery: invalid evidence")
	ErrDuplicateApply  = errors.New("processrecovery: decision already applied")
)

// PersistedEvidence is the durable snapshot recovered from project/machine state.
// No argv/secrets.
type PersistedEvidence struct {
	ProjectID            string
	AttemptID            string
	Generation           int64
	RootPID              int
	PGID                 int
	ProcessBirthIdentity string
	ExecutableIdentity   string
	// LaunchRecorded is true when a launch event was persisted.
	LaunchRecorded bool
	// TerminalRecorded is true when a terminal event already exists.
	TerminalRecorded bool
	// ExitCode set when exit was observed but terminal event missing.
	ExitObserved bool
	ExitCode     int
	// ReservationID if an admission claim was held.
	ReservationID         string
	ReservationGeneration int64
	// ReservationReleased already.
	ReservationReleased bool
	// TerminalEventID / DecisionID for idempotency.
	LastDecisionID string
}

// LiveObservation is the OS view at recovery time.
type LiveObservation struct {
	// PIDAlive is true if RootPID is currently alive.
	PIDAlive bool
	// BirthMatches when live birth identity equals persisted.
	BirthMatches bool
	// ExecMatches when live executable identity equals persisted.
	ExecMatches bool
	// PGIDMatches when live process group matches.
	PGIDMatches bool
	// OwnedDescendantsAlive counts matching-group children (no root).
	OwnedDescendantsAlive int
	// ObservationIncomplete when OS probe was partial.
	ObservationIncomplete bool
}

// Decision is the pure recovery outcome.
type Decision struct {
	SchemaVersion  string         `json:"schema_version"`
	DecisionID     string         `json:"decision_id"`
	Kind           DecisionKind   `json:"kind"`
	OperatorAction OperatorAction `json:"operator_action"`
	AttemptID      string         `json:"attempt_id"`
	Generation     int64          `json:"generation"`
	Reasons        []string       `json:"reasons"`
	Adopted        bool           `json:"adopted"`
	// RelaunchAllowed is true only for proven never-started; recovery itself
	// does not execute — a new attempt decision is required.
	RelaunchAllowed bool `json:"relaunch_allowed"`
	// TerminalEventEmitted / ReservationReleased on this apply.
	TerminalEventEmitted bool      `json:"terminal_event_emitted"`
	ReservationReleased  bool      `json:"reservation_released"`
	At                   time.Time `json:"at"`
	// Replay is true when the same DecisionID is reapplied.
	Replay bool `json:"replay,omitempty"`
}

// LiveProber reads OS state for a persisted root.
type LiveProber interface {
	Observe(ev PersistedEvidence) (LiveObservation, error)
}

// EventSink records terminal/recovery events idempotently.
type EventSink interface {
	// EmitTerminal records a terminal event once per attempt+generation.
	EmitTerminal(attemptID string, generation int64, exitCode int, note string) (emitted bool, err error)
	// EmitDecision records the recovery decision once per DecisionID.
	EmitDecision(d Decision) (emitted bool, err error)
}

// ReservationReleaser frees machine admission once.
type ReservationReleaser interface {
	Release(reservationID string, generation int64) (released bool, err error)
}
