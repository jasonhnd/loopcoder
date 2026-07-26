package runtimefacade

import (
	"context"
	"errors"
	"time"
)

// Typed failure categories. Use errors.Is.
var (
	ErrInvalidLaunch   = errors.New("runtimefacade: invalid launch request")
	ErrLaunchFailed    = errors.New("runtimefacade: launch failed")
	ErrNotRunning      = errors.New("runtimefacade: attempt not running")
	ErrObserveFailed   = errors.New("runtimefacade: observe failed")
	ErrSignalFailed    = errors.New("runtimefacade: signal failed")
	ErrJoinFailed      = errors.New("runtimefacade: join failed")
	ErrJoinIncomplete  = errors.New("runtimefacade: join incomplete; attention required")
	ErrAlreadyTerminal = errors.New("runtimefacade: attempt already terminal")
)

// ProcessState is OS-backed liveness for an attempt handle.
type ProcessState string

const (
	StateNotStarted ProcessState = "not_started"
	StateStarting   ProcessState = "starting"
	StateAlive      ProcessState = "alive"
	StateExited     ProcessState = "exited"
	StateUnknown    ProcessState = "unknown"
)

// SignalKind is a portable signal request.
type SignalKind string

const (
	SignalTerm SignalKind = "term"
	SignalKill SignalKind = "kill"
	SignalInt  SignalKind = "interrupt"
)

// OutcomeClass is how the process ended from the runtime's perspective.
type OutcomeClass string

const (
	OutcomeCompleted OutcomeClass = "completed"
	OutcomeSignalled OutcomeClass = "signalled"
	OutcomeDeadline  OutcomeClass = "deadline"
	OutcomeUnknown   OutcomeClass = "unknown"
)

// LaunchRequest is an immutable launch contract. Runtime.Launch must not
// mutate the caller's request after return.
type LaunchRequest struct {
	// AttemptID is the caller's durable attempt key (required).
	AttemptID string
	// Argv is the executable and arguments (required, non-empty).
	Argv []string
	// WorkDir is the child working directory (optional).
	WorkDir string
	// Env, when non-nil, is the full child environment. When nil, a cleaned
	// process environment is used (no inherited provider/Git secrets).
	Env []string
	// HardCap bounds wall time from launch to join; zero means no hard cap
	// at the facade (adapters may still apply their own defaults).
	HardCap time.Duration
	// Role is an optional kill-group / diagnostic label.
	Role string
}

// Clone returns a deep copy so Launch can retain an immutable snapshot.
func (r LaunchRequest) Clone() LaunchRequest {
	out := r
	if r.Argv != nil {
		out.Argv = append([]string(nil), r.Argv...)
	}
	if r.Env != nil {
		out.Env = append([]string(nil), r.Env...)
	}
	return out
}

// Identity is the started attempt identity. Success paths require a real PID.
type Identity struct {
	AttemptID string
	PID       int
	StartedAt time.Time
	// Request is the immutable snapshot retained at launch.
	Request LaunchRequest
}

// Observation is a point-in-time OS-backed view. Provider output is not used.
type Observation struct {
	State      ProcessState
	PID        int
	ExitCode   *int
	ObservedAt time.Time
	// EvidenceNote is a short non-secret note (e.g. "waited", "signal pending").
	EvidenceNote string
}

// TerminalEvidence is required before any successful Join result.
type TerminalEvidence struct {
	Exited   bool
	ExitCode int
	Killed   bool
	Elapsed  time.Duration
	Outcome  OutcomeClass
	// Note is redacted diagnostic text only.
	Note string
}

// JoinResult is returned only when join has process evidence.
// Success() is true only when the process exited (any exit code) and was joined.
type JoinResult struct {
	Terminal TerminalEvidence
}

// Success reports whether process terminal evidence is complete.
// Exit code is not interpreted as pass/fail here.
func (r JoinResult) Success() bool {
	return r.Terminal.Exited
}

// Runtime is the provider-neutral launch port.
type Runtime interface {
	// Launch starts one attempt. On failure no process is owned.
	Launch(ctx context.Context, req LaunchRequest) (Handle, error)
}

// Handle is one live or terminal attempt.
type Handle interface {
	Identity() Identity
	Observe(ctx context.Context) (Observation, error)
	Signal(ctx context.Context, kind SignalKind) error
	// Join waits for terminal process evidence. Provider prose cannot complete
	// a join. Incomplete joins return ErrJoinIncomplete with strongest evidence.
	Join(ctx context.Context) (JoinResult, error)
}

// Clock abstracts time for tests.
type Clock interface {
	Now() time.Time
}

// SystemClock uses time.Now.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// OutputSink receives bounded child stdout/stderr if adapters capture it.
// Adapters may ignore the sink; it is never authority for completion.
type OutputSink interface {
	WriteStdout([]byte)
	WriteStderr([]byte)
}

// NopSink discards output.
type NopSink struct{}

func (NopSink) WriteStdout([]byte) {}
func (NopSink) WriteStderr([]byte) {}
