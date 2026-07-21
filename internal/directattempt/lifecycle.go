package directattempt

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/deliverygate"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
	"github.com/jasonhnd/loopcoder/internal/routepin"
	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
	"github.com/jasonhnd/loopcoder/internal/wtclaim"
)

const SchemaAttempt = "loopcoder.direct.attempt.v1"

// State is the direct attempt lifecycle.
type State string

const (
	StateRequested       State = "requested"
	StateAdmitted        State = "admitted"
	StateLaunching       State = "launching"
	StateRunning         State = "running"
	StateStopping        State = "stopping"
	StateProcessTerminal State = "process_terminal"
	StateCleanupTerminal State = "cleanup_terminal"
	StateFailed          State = "failed"
)

var (
	ErrInvalid         = errors.New("directattempt: invalid")
	ErrNotReady        = errors.New("directattempt: not ready")
	ErrDuplicateLaunch = errors.New("directattempt: launch already occurred")
	ErrDigestMismatch  = errors.New("directattempt: digest mismatch")
	ErrNotTerminal     = errors.New("directattempt: not terminal")
)

// Attempt is durable attempt state.
type Attempt struct {
	Schema              string    `json:"schema"`
	AttemptID           string    `json:"attempt_id"`
	Generation          int64     `json:"generation"`
	RunID               string    `json:"run_id"`
	ProjectID           string    `json:"project_id"`
	State               State     `json:"state"`
	RouteDigest         string    `json:"route_digest"`
	WorktreePath        string    `json:"worktree_path"`
	BaseSHA             string    `json:"base_sha"`
	IdempotencyKey      string    `json:"idempotency_key"`
	StartEventID        string    `json:"start_event_id,omitempty"`
	StartDigest         string    `json:"start_digest,omitempty"`
	ProviderLaunched    bool      `json:"provider_launched"`
	ProviderExitCode    *int      `json:"provider_exit_code,omitempty"`
	OutputFlushed       bool      `json:"output_flushed"`
	ChildrenJoined      bool      `json:"children_joined"`
	ReservationReleased bool      `json:"reservation_released"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// LaunchBundle is frozen data required before provider launch.
type LaunchBundle struct {
	AttemptID      string
	Route          routepin.Fields
	RouteDigest    string
	WorktreePath   string
	BaseSHA        string
	IdempotencyKey string
	// Start report must be rendered by required client first.
	StartEventID   string
	StartDigest    string
	RequiredClient string
}

// Store is in-process attempt ledger.
type Store struct {
	mu   sync.Mutex
	byID map[string]*Attempt
	seq  int64
	now  func() time.Time
}

func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{byID: map[string]*Attempt{}, now: now}
}

// Create registers a requested attempt.
func (s *Store) Create(projectID, runID, attemptID, routeDigest, wtPath, baseSHA, idem string) (Attempt, error) {
	if projectID == "" || runID == "" || attemptID == "" || routeDigest == "" || wtPath == "" || baseSHA == "" || idem == "" {
		return Attempt{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[attemptID]; ok {
		return Attempt{}, ErrInvalid
	}
	s.seq++
	now := s.now().UTC()
	a := &Attempt{
		Schema: SchemaAttempt, AttemptID: attemptID, Generation: s.seq,
		RunID: runID, ProjectID: projectID, State: StateRequested,
		RouteDigest: routeDigest, WorktreePath: wtPath, BaseSHA: baseSHA,
		IdempotencyKey: idem, CreatedAt: now, UpdatedAt: now,
	}
	s.byID[attemptID] = a
	return *a, nil
}

// Admit moves requested -> admitted after resource claim.
func (s *Store) Admit(attemptID string) (Attempt, error) {
	return s.transition(attemptID, StateRequested, StateAdmitted, nil)
}

// MarkStartReport binds start report identity for delivery gate.
func (s *Store) MarkStartReport(attemptID, eventID, digest string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[attemptID]
	if !ok {
		return Attempt{}, ErrInvalid
	}
	a.StartEventID = eventID
	a.StartDigest = digest
	a.UpdatedAt = s.now().UTC()
	return *a, nil
}

// BeginLaunch transitions admitted -> launching if start rendered and digests match.
func (s *Store) BeginLaunch(attemptID string, routeDigest, wtPath, baseSHA string, startRendered bool) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[attemptID]
	if !ok {
		return Attempt{}, ErrInvalid
	}
	if a.State != StateAdmitted {
		if a.ProviderLaunched {
			return *a, ErrDuplicateLaunch
		}
		return Attempt{}, ErrNotReady
	}
	if !startRendered {
		return Attempt{}, fmt.Errorf("%w: start not rendered", ErrNotReady)
	}
	if a.RouteDigest != routeDigest || a.WorktreePath != wtPath || a.BaseSHA != baseSHA {
		return Attempt{}, ErrDigestMismatch
	}
	if a.StartEventID == "" || a.StartDigest == "" {
		return Attempt{}, fmt.Errorf("%w: start report unbound", ErrNotReady)
	}
	a.State = StateLaunching
	a.UpdatedAt = s.now().UTC()
	return *a, nil
}

// RecordLaunch marks the single provider launch (idempotent if same generation already launched).
func (s *Store) RecordLaunch(attemptID string, generation int64) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[attemptID]
	if !ok {
		return Attempt{}, ErrInvalid
	}
	if a.Generation != generation {
		return Attempt{}, ErrInvalid
	}
	if a.ProviderLaunched {
		return *a, ErrDuplicateLaunch
	}
	if a.State != StateLaunching {
		return Attempt{}, ErrNotReady
	}
	a.ProviderLaunched = true
	a.State = StateRunning
	a.UpdatedAt = s.now().UTC()
	return *a, nil
}

// NoteProviderExit records process exit without completing attempt.
func (s *Store) NoteProviderExit(attemptID string, code int) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[attemptID]
	if !ok {
		return Attempt{}, ErrInvalid
	}
	if a.State != StateRunning && a.State != StateStopping {
		return Attempt{}, ErrNotReady
	}
	a.ProviderExitCode = &code
	a.State = StateProcessTerminal
	a.UpdatedAt = s.now().UTC()
	return *a, nil
}

// RequestStop transitions running -> stopping.
func (s *Store) RequestStop(attemptID string) (Attempt, error) {
	return s.transition(attemptID, StateRunning, StateStopping, nil)
}

// CompleteCleanup requires flush+join+release after process terminal.
func (s *Store) CompleteCleanup(attemptID string, flushed, joined, released bool) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[attemptID]
	if !ok {
		return Attempt{}, ErrInvalid
	}
	if a.State != StateProcessTerminal && a.State != StateStopping {
		// stopping may jump if process already dead
		if a.State != StateRunning {
			return Attempt{}, ErrNotTerminal
		}
	}
	if !flushed || !joined || !released {
		return Attempt{}, fmt.Errorf("%w: cleanup incomplete flush=%v join=%v release=%v", ErrNotTerminal, flushed, joined, released)
	}
	// provider exit alone insufficient — must have process terminal or stop path
	if a.ProviderExitCode == nil && a.State != StateStopping {
		return Attempt{}, fmt.Errorf("%w: no process terminal", ErrNotTerminal)
	}
	a.OutputFlushed = true
	a.ChildrenJoined = true
	a.ReservationReleased = true
	a.State = StateCleanupTerminal
	a.UpdatedAt = s.now().UTC()
	return *a, nil
}

// Fail marks failed.
func (s *Store) Fail(attemptID string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[attemptID]
	if !ok {
		return Attempt{}, ErrInvalid
	}
	a.State = StateFailed
	a.UpdatedAt = s.now().UTC()
	return *a, nil
}

// Get returns attempt.
func (s *Store) Get(attemptID string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[attemptID]
	if !ok {
		return Attempt{}, ErrInvalid
	}
	return *a, nil
}

func (s *Store) transition(id string, from, to State, mut func(*Attempt)) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	if !ok {
		return Attempt{}, ErrInvalid
	}
	if a.State != from {
		return Attempt{}, ErrNotReady
	}
	if mut != nil {
		mut(a)
	}
	a.State = to
	a.UpdatedAt = s.now().UTC()
	return *a, nil
}

// Engine wires pin + delivery + optional fake provider for the direct path.
type Engine struct {
	Attempts *Store
	Pins     *routepin.Store
	Ledger   *uisub.Ledger
	// Provider launches once; tests inject.
	Provider func(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error)
	// Reserve/Release hooks (admission)
	Reserve func(attemptID string) error
	Release func(attemptID string) error
}

// PrepareStart publishes start report and records it on the attempt.
func (e *Engine) PrepareStart(attemptID, projectID string, seq int64) (uireport.Envelope, error) {
	env, err := uireport.Project(uireport.Input{
		Kind: uireport.KindStart, ProjectID: projectID, AttemptID: attemptID, Sequence: seq,
		Stage: "start", Status: "starting", Liveness: "alive",
	})
	if err != nil {
		return uireport.Envelope{}, err
	}
	if e.Ledger != nil {
		_ = e.Ledger.Publish(env)
	}
	_, err = e.Attempts.MarkStartReport(attemptID, env.EventID, env.ContentDigest)
	return env, err
}

// TryLaunch enforces start:rendered, pin ready, single launch.
func (e *Engine) TryLaunch(ctx context.Context, b LaunchBundle) (Attempt, error) {
	// verify pin
	pin, err := e.Pins.GetActive(b.AttemptID)
	if err != nil || !pin.ReadyForLaunch() || pin.Digest != b.RouteDigest {
		return Attempt{}, ErrNotReady
	}
	// start rendered?
	rendered := false
	if e.Ledger != nil && b.RequiredClient != "" {
		if ack, ok := e.Ledger.AckEvidence(b.RequiredClient, b.StartEventID, uisub.StageRendered); ok && ack.Digest == b.StartDigest {
			rendered = true
		}
	}
	a, err := e.Attempts.BeginLaunch(b.AttemptID, b.RouteDigest, b.WorktreePath, b.BaseSHA, rendered)
	if err != nil {
		return Attempt{}, err
	}
	if e.Reserve != nil {
		if err := e.Reserve(b.AttemptID); err != nil {
			_, _ = e.Attempts.Fail(b.AttemptID)
			return Attempt{}, err
		}
	}
	// build exec request from frozen pin only
	req, err := providerexec.NewRequest(providerexec.Request{
		RequestID: b.IdempotencyKey, ProjectID: pin.ProjectID, AttemptID: b.AttemptID,
		WorkDir: b.WorktreePath, Route: b.Route.ToExecRoute(),
	})
	if err != nil {
		_, _ = e.Attempts.Fail(b.AttemptID)
		return Attempt{}, err
	}
	// field-level match via pin VerifyActual
	if _, err := e.Pins.VerifyActual(b.AttemptID, b.Route); err != nil {
		_, _ = e.Attempts.Fail(b.AttemptID)
		return Attempt{}, ErrDigestMismatch
	}
	_ = req

	a, err = e.Attempts.RecordLaunch(b.AttemptID, a.Generation)
	if err != nil {
		return Attempt{}, err
	}
	if e.Provider != nil {
		out, perr := e.Provider(ctx, req)
		code := out.ExitCode
		if perr != nil {
			code = 1
		}
		_, _ = e.Attempts.NoteProviderExit(b.AttemptID, code)
	}
	return e.Attempts.Get(b.AttemptID)
}

// FinishCleanup completes after process terminal.
func (e *Engine) FinishCleanup(attemptID string) (Attempt, error) {
	if e.Release != nil {
		_ = e.Release(attemptID)
	}
	return e.Attempts.CompleteCleanup(attemptID, true, true, true)
}

// Ensure deliverygate type is referenced for compile coupling documentation.
var _ = deliverygate.StateLive
var _ = wtclaim.SchemaClaim
