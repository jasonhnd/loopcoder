package termination

import (
	"context"
	"errors"
	"time"
)

const SchemaVersion = "loopcoder.termination.v1"

// Default deadlines.
const (
	DefaultGrace          = 2 * time.Second
	DefaultHardAfterGrace = 3 * time.Second
	DefaultCleanupBound   = 10 * time.Second
)

// Reason classifies why stop was requested.
type Reason string

const (
	ReasonCancel   Reason = "cancel"
	ReasonTimeout  Reason = "timeout"
	ReasonPolicy   Reason = "policy"
	ReasonOperator Reason = "operator"
	ReasonSuccess  Reason = "success_cleanup"
	ReasonFailure  Reason = "failure_cleanup"
)

// State is the lifecycle state machine.
type State string

const (
	StateIdle              State = "idle"
	StateStopping          State = "stopping"
	StateEscalating        State = "escalating"
	StateJoining           State = "joining"
	StateTerminalClean     State = "terminal_clean"
	StateAttentionRequired State = "attention_required"
)

// SignalKind is the escalation step.
type SignalKind string

const (
	SignalTerm SignalKind = "term"
	SignalKill SignalKind = "kill"
)

var (
	ErrGenerationMismatch = errors.New("termination: generation mismatch")
	ErrAlreadyTerminal    = errors.New("termination: already terminal")
	ErrAttentionRequired  = errors.New("termination: attention required")
	ErrInvalidTarget      = errors.New("termination: invalid target")
	ErrNotOwned           = errors.New("termination: process not owned by generation")
)

// Target is a generation-fenced process authority handle.
type Target struct {
	AttemptID             string
	Generation            int64
	RootPID               int
	PGID                  int
	ProcessBirthIdentity  string
	ReservationID         string
	ReservationGeneration int64
}

// Policy bounds grace/hard escalation and independent cleanup.
type Policy struct {
	Grace          time.Duration
	HardAfterGrace time.Duration
	CleanupBound   time.Duration
}

// DefaultPolicy returns issue defaults.
func DefaultPolicy() Policy {
	return Policy{
		Grace:          DefaultGrace,
		HardAfterGrace: DefaultHardAfterGrace,
		CleanupBound:   DefaultCleanupBound,
	}
}

// Transition is one persisted lifecycle step (no secrets/argv).
type Transition struct {
	SchemaVersion string
	AttemptID     string
	Generation    int64
	From          State
	To            State
	Reason        Reason
	Signal        SignalKind
	Note          string
	At            time.Time
}

// JoinEvidence records what was observed at join time.
type JoinEvidence struct {
	RootExited       bool
	OwnedJoined      int
	EscapedChildren  int
	UnknownChildren  bool
	OutputFlushed    bool
	ReservationFreed bool
	AttentionReason  string
}

// Result is the outcome of Stop.
type Result struct {
	State       State
	Evidence    JoinEvidence
	Transitions []Transition
	// TerminalClean is true only when output flushed, no owned descendants,
	// and reservation released.
	TerminalClean bool
}

// EventWriter records meaningful transitions.
type EventWriter interface {
	Write(t Transition) error
}

// NopEvents discards transitions.
type NopEvents struct{}

func (NopEvents) Write(Transition) error { return nil }

// Controller signals and waits for a generation-fenced root.
type Controller interface {
	// Signal sends kind only if the live process still matches target generation identity.
	Signal(target Target, kind SignalKind) error
	// Alive reports whether the generation-matched root is still running.
	Alive(target Target) (bool, error)
	// Wait waits until the root exits or ctx ends.
	Wait(ctx context.Context, target Target) error
}

// TreeView observes owned descendants (no argv).
type TreeView interface {
	// Snapshot returns count of still-alive owned children, escaped count, and
	// whether observation was incomplete/unknown.
	Snapshot(target Target) (ownedAlive int, escaped int, unknown bool, err error)
}

// OutputFlusher drains bounded output before terminal-clean.
type OutputFlusher interface {
	Flush(ctx context.Context, attemptID string) error
}

// ReservationReleaser releases the machine admission claim.
type ReservationReleaser interface {
	Release(ctx context.Context, reservationID string, generation int64) error
}

// NopFlush / NopRelease helpers for tests.
type NopFlush struct{}

func (NopFlush) Flush(context.Context, string) error { return nil }

type NopRelease struct{}

func (NopRelease) Release(context.Context, string, int64) error { return nil }
