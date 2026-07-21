package attachowner

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/deliverygate"
)

const (
	SchemaOwner    = "loopcoder.attach.owner.v1"
	ModeForeground = "foreground"
	ModeDetached   = "detached"
)

// Phase is ownership lifecycle.
type Phase string

const (
	PhaseAttaching Phase = "attaching"
	PhaseLive      Phase = "live"
	PhaseDetached  Phase = "detached"
	PhaseTerminal  Phase = "terminal"
	PhaseFailed    Phase = "failed"
)

var (
	ErrInvalid          = errors.New("attachowner: invalid")
	ErrNotDetached      = errors.New("attachowner: not detached")
	ErrStaleGeneration  = errors.New("attachowner: stale generation")
	ErrAlreadyTerminal  = errors.New("attachowner: already terminal")
	ErrDetachRequiresUI = errors.New("attachowner: detach requires report client policy")
	ErrNoAutoDetach     = errors.New("attachowner: silent auto-detach forbidden")
)

// Spec freezes ownership for a run.
type Spec struct {
	RunID            string
	ProjectID        string
	AttemptID        string
	Mode             string // foreground | detached
	RequiredClients  []deliverygate.ClientSpec
	AllowedFallbacks []string
	MissedPolicy     deliverygate.MissedPolicy
	// InvokerPID is the invoking UI/core process (foreground owner).
	InvokerPID int
}

// SupervisorIdentity is durable supervisor evidence.
type SupervisorIdentity struct {
	PID        int       `json:"pid"`
	Generation int64     `json:"generation"`
	StartedAt  time.Time `json:"started_at"`
	HostID     string    `json:"host_id,omitempty"`
}

// Ownership is durable run authority.
type Ownership struct {
	Schema         string                `json:"schema"`
	RunID          string                `json:"run_id"`
	ProjectID      string                `json:"project_id"`
	AttemptID      string                `json:"attempt_id"`
	Mode           string                `json:"mode"`
	Phase          Phase                 `json:"phase"`
	Supervisor     *SupervisorIdentity   `json:"supervisor,omitempty"`
	CancelEndpoint string                `json:"cancel_endpoint,omitempty"`
	LeaseExpiresAt time.Time             `json:"lease_expires_at,omitempty"`
	ReportPolicy   deliverygate.Snapshot `json:"report_policy"`
	TerminalJoinOK bool                  `json:"terminal_join_ok"`
	InvokerPID     int                   `json:"invoker_pid"`
	UIDisconnected bool                  `json:"ui_disconnected"`
	CreatedAt      time.Time             `json:"created_at"`
	UpdatedAt      time.Time             `json:"updated_at"`
}

// Store is in-process durable ownership (tests/core projection).
type Store struct {
	mu   sync.Mutex
	runs map[string]*Ownership
	now  func() time.Time
	seq  int64
}

// NewStore creates an empty ownership store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{runs: map[string]*Ownership{}, now: now}
}

// Start begins a run in foreground or explicit detach mode.
func (s *Store) Start(spec Spec) (Ownership, error) {
	if spec.RunID == "" || spec.ProjectID == "" || spec.AttemptID == "" {
		return Ownership{}, fmt.Errorf("%w: missing identity", ErrInvalid)
	}
	if spec.Mode == "" {
		spec.Mode = ModeForeground
	}
	if spec.Mode != ModeForeground && spec.Mode != ModeDetached {
		return Ownership{}, fmt.Errorf("%w: mode", ErrInvalid)
	}
	if spec.Mode == ModeDetached {
		if len(spec.RequiredClients) == 0 {
			return Ownership{}, ErrDetachRequiresUI
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.runs[spec.RunID]; ok {
		return Ownership{}, fmt.Errorf("%w: run exists", ErrInvalid)
	}
	now := s.now().UTC()
	pol := deliverygate.Snapshot{
		Schema:    deliverygate.SchemaSnapshot,
		ProjectID: spec.ProjectID, RunID: spec.RunID, AttemptID: spec.AttemptID,
		RequiredClients:    append([]deliverygate.ClientSpec(nil), spec.RequiredClients...),
		AllowedFallbacks:   append([]string(nil), spec.AllowedFallbacks...),
		MissedReportPolicy: spec.MissedPolicy,
	}
	if pol.MissedReportPolicy == "" {
		pol.MissedReportPolicy = deliverygate.MissedStop
	}
	o := &Ownership{
		Schema:       SchemaOwner,
		RunID:        spec.RunID,
		ProjectID:    spec.ProjectID,
		AttemptID:    spec.AttemptID,
		Mode:         spec.Mode,
		Phase:        PhaseAttaching,
		ReportPolicy: pol,
		InvokerPID:   spec.InvokerPID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if spec.Mode == ModeDetached {
		s.seq++
		o.Supervisor = &SupervisorIdentity{
			PID:        0, // filled by MarkSupervisor
			Generation: s.seq,
			StartedAt:  now,
		}
		o.Phase = PhaseDetached
		o.CancelEndpoint = "run://" + spec.RunID + "/cancel"
		o.LeaseExpiresAt = now.Add(2 * time.Minute)
	} else {
		o.Phase = PhaseLive
	}
	s.runs[spec.RunID] = o
	return clone(o), nil
}

// MarkSupervisor records durable supervisor identity for explicit detach.
// Returns stable run ID only when identity+cancel+policy are set.
func (s *Store) MarkSupervisor(runID string, pid int, hostID string) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.runs[runID]
	if !ok {
		return Ownership{}, ErrInvalid
	}
	if o.Mode != ModeDetached {
		return Ownership{}, ErrNotDetached
	}
	if o.Phase == PhaseTerminal || o.Phase == PhaseFailed {
		return Ownership{}, ErrAlreadyTerminal
	}
	if pid <= 0 {
		return Ownership{}, fmt.Errorf("%w: pid", ErrInvalid)
	}
	now := s.now().UTC()
	if o.Supervisor == nil {
		s.seq++
		o.Supervisor = &SupervisorIdentity{Generation: s.seq, StartedAt: now}
	}
	o.Supervisor.PID = pid
	o.Supervisor.HostID = hostID
	if o.CancelEndpoint == "" {
		o.CancelEndpoint = "run://" + runID + "/cancel"
	}
	o.LeaseExpiresAt = now.Add(2 * time.Minute)
	o.UpdatedAt = now
	return clone(o), nil
}

// DetachReady is true when detach evidence is durable enough to return run ID.
func (o Ownership) DetachReady() bool {
	if o.Mode != ModeDetached {
		return false
	}
	if o.Supervisor == nil || o.Supervisor.PID <= 0 || o.Supervisor.Generation <= 0 {
		return false
	}
	if o.CancelEndpoint == "" {
		return false
	}
	if len(o.ReportPolicy.RequiredClients) == 0 {
		return false
	}
	return true
}

// NoteUIDisconnect records UI gone without killing work.
func (s *Store) NoteUIDisconnect(runID string) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.runs[runID]
	if !ok {
		return Ownership{}, ErrInvalid
	}
	// Explicit forbid silent auto-detach transition from foreground.
	if o.Mode == ModeForeground && o.Phase == PhaseLive {
		o.UIDisconnected = true
		o.UpdatedAt = s.now().UTC()
		// remain live; delivery policy decides stop/detach — no auto mode flip
		return clone(o), nil
	}
	o.UIDisconnected = true
	o.UpdatedAt = s.now().UTC()
	return clone(o), nil
}

// ForbidAutoDetach documents that disconnect must not flip to detached.
func (s *Store) TryAutoDetach(runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.runs[runID]
	if !ok {
		return ErrInvalid
	}
	if o.Mode == ModeForeground {
		return ErrNoAutoDetach
	}
	return nil
}

// Heartbeat renews lease for matching generation only.
func (s *Store) Heartbeat(runID string, generation int64, pid int) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.runs[runID]
	if !ok {
		return Ownership{}, ErrInvalid
	}
	if o.Supervisor == nil || o.Supervisor.Generation != generation {
		return Ownership{}, ErrStaleGeneration
	}
	if o.Supervisor.PID != pid {
		return Ownership{}, ErrStaleGeneration
	}
	if o.Phase == PhaseTerminal || o.Phase == PhaseFailed {
		return Ownership{}, ErrAlreadyTerminal
	}
	now := s.now().UTC()
	o.LeaseExpiresAt = now.Add(2 * time.Minute)
	o.UpdatedAt = now
	return clone(o), nil
}

// SignalCancel is generation-fenced cancel.
func (s *Store) SignalCancel(runID string, generation int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.runs[runID]
	if !ok {
		return ErrInvalid
	}
	if o.Supervisor != nil && generation != 0 && o.Supervisor.Generation != generation {
		return ErrStaleGeneration
	}
	// cancel is allowed through durable run authority even if UI is gone
	o.UpdatedAt = s.now().UTC()
	return nil
}

// Complete marks terminal with join evidence.
func (s *Store) Complete(runID string, generation int64, joinOK bool) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.runs[runID]
	if !ok {
		return Ownership{}, ErrInvalid
	}
	if o.Supervisor != nil && generation != 0 && o.Supervisor.Generation != generation {
		return Ownership{}, ErrStaleGeneration
	}
	if o.Phase == PhaseTerminal {
		return clone(o), nil
	}
	o.Phase = PhaseTerminal
	o.TerminalJoinOK = joinOK
	o.UpdatedAt = s.now().UTC()
	return clone(o), nil
}

// Fail marks failure; generation-fenced when supervisor present.
func (s *Store) Fail(runID string, generation int64) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.runs[runID]
	if !ok {
		return Ownership{}, ErrInvalid
	}
	if o.Supervisor != nil && generation != 0 && o.Supervisor.Generation != generation {
		return Ownership{}, ErrStaleGeneration
	}
	o.Phase = PhaseFailed
	o.UpdatedAt = s.now().UTC()
	return clone(o), nil
}

// Get returns ownership by run ID (no original UI required).
func (s *Store) Get(runID string) (Ownership, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.runs[runID]
	if !ok {
		return Ownership{}, ErrInvalid
	}
	return clone(o), nil
}

// ForegroundReturnOK is true when foreground may return (terminal/failed only;
// detach is a separate mode from start).
func (o Ownership) ForegroundReturnOK() bool {
	if o.Mode != ModeForeground {
		return false
	}
	return o.Phase == PhaseTerminal || o.Phase == PhaseFailed
}

// ResourcesCleared is true when terminal join succeeded (no unowned child claim).
func (o Ownership) ResourcesCleared() bool {
	return o.Phase == PhaseTerminal && o.TerminalJoinOK
}

func clone(o *Ownership) Ownership {
	cp := *o
	if o.Supervisor != nil {
		s := *o.Supervisor
		cp.Supervisor = &s
	}
	cp.ReportPolicy.RequiredClients = append([]deliverygate.ClientSpec(nil), o.ReportPolicy.RequiredClients...)
	cp.ReportPolicy.AllowedFallbacks = append([]string(nil), o.ReportPolicy.AllowedFallbacks...)
	return cp
}
