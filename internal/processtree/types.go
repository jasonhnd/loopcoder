package processtree

import (
	"errors"
	"time"
)

// DefaultMaxNodes bounds a single tree snapshot (fixture + production).
const DefaultMaxNodes = 64

// Liveness is the process-tree execution authority state.
type Liveness string

const (
	LivenessNotStarted Liveness = "not_started"
	LivenessStarting   Liveness = "starting"
	LivenessAlive      Liveness = "alive"
	LivenessExited     Liveness = "exited"
	LivenessUnknown    Liveness = "unknown"
)

// Confidence grades observation quality.
type Confidence string

const (
	ConfidenceFull    Confidence = "full"
	ConfidencePartial Confidence = "partial"
	ConfidenceNone    Confidence = "none"
)

var (
	// ErrPIDReuse means the PID is alive but birth identity does not match launch.
	ErrPIDReuse = errors.New("processtree: pid reuse detected")
	// ErrAttentionRequired means escaped/unobservable descendants need human review.
	ErrAttentionRequired = errors.New("processtree: attention required")
	// ErrNotStarted means no root was recorded.
	ErrNotStarted = errors.New("processtree: not started")
)

// LaunchEvidence is the durable root identity recorded at process start.
// Wrapper PIDs alone are never durable worker identity without birth evidence.
type LaunchEvidence struct {
	RootPID              int
	PGID                 int
	ProcessBirthIdentity string
	ExecutableIdentity   string // redacted/comm only when persisted
	SessionID            int    // optional; 0 if unknown
	RecordedAt           time.Time
	// AttemptID is caller correlation only.
	AttemptID string
}

// Node is one process in a bounded snapshot. Argv/env are never stored.
type Node struct {
	PID                  int
	PPID                 int
	PGID                 int
	ProcessBirthIdentity string
	// Comm is a short command name (no full argv secrets).
	Comm string
	// Owned is true when this node is considered part of the launch tree.
	Owned bool
	// Escaped is true when a former owned descendant is outside the group.
	Escaped bool
	// Zombie is true when the process is a zombie (status Z).
	Zombie bool
}

// Snapshot is an ordered, bounded view of the owned tree.
type Snapshot struct {
	Root       LaunchEvidence
	ObservedAt time.Time
	// Nodes is ordered by PID ascending (stable).
	Nodes []Node
	// Truncated is true when MaxNodes cut the walk.
	Truncated bool
	// ObservationError is a redacted note when OS observation failed partially.
	ObservationError string
}

// Assessment is the liveness decision for one observation.
type Assessment struct {
	Liveness   Liveness
	Confidence Confidence
	// AttentionRequired is true for escape, unobservable descendants, or ambiguity.
	AttentionRequired bool
	// Reasons are short machine-stable tokens (no secrets/paths).
	Reasons []string
	// Snapshot is the observation used (may be empty for not_started).
	Snapshot Snapshot
	// Terminal is true only when the entire owned tree has exited (no owned live nodes).
	// Wrapper exit with live descendants is NOT terminal.
	Terminal bool
}
