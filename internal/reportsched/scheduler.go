package reportsched

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	SchemaVersion   = "loopcoder.report_sched.v1"
	DefaultInterval = 5 * time.Minute
)

// Kind of receipt.
type Kind string

const (
	KindStart       Kind = "start"
	KindInterval    Kind = "interval"
	KindStateChange Kind = "state_change"
	KindBlocker     Kind = "blocker"
	KindTerminal    Kind = "terminal"
	KindNoProgress  Kind = "no_progress"
)

// Action recommended after no-progress.
type Action string

const (
	ActionContinue  Action = "continue"
	ActionStop      Action = "stop"
	ActionDetach    Action = "detach"
	ActionAttention Action = "attention"
)

var (
	ErrNotActive = errors.New("reportsched: attempt not active")
	ErrDuplicate = errors.New("reportsched: duplicate receipt")
)

// Receipt is one status emission (no provider content).
type Receipt struct {
	SchemaVersion        string        `json:"schema_version"`
	Kind                 Kind          `json:"kind"`
	AttemptID            string        `json:"attempt_id"`
	Stage                string        `json:"stage"`
	Elapsed              time.Duration `json:"elapsed"`
	LastConcreteProgress time.Time     `json:"last_concrete_progress"`
	ProcessCount         int           `json:"process_count"`
	ResourceState        string        `json:"resource_state"`
	RemoteGate           string        `json:"remote_gate"`
	Blocker              string        `json:"blocker,omitempty"`
	NextTimeout          time.Time     `json:"next_timeout"`
	NextAction           Action        `json:"next_action"`
	At                   time.Time     `json:"at"`
	// Seq is monotonic per attempt for dedup.
	Seq int64 `json:"seq"`
}

// State is persisted scheduler state for one attempt.
type State struct {
	AttemptID            string
	Active               bool
	StartedAt            time.Time
	NextDue              time.Time
	LastReceiptAt        time.Time
	LastConcreteProgress time.Time
	NoProgressStreak     int
	Seq                  int64
	Stage                string
	ProcessCount         int
	ResourceState        string
	RemoteGate           string
	Blocker              string
}

// Clock is injectable time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// MemoryClock for tests.
type MemoryClock struct {
	T time.Time
}

func (c *MemoryClock) Now() time.Time          { return c.T.UTC() }
func (c *MemoryClock) Advance(d time.Duration) { c.T = c.T.Add(d) }

// Store persists attempt scheduler state.
type Store interface {
	Load(attemptID string) (State, bool, error)
	Save(State) error
}

// MemStore is an in-memory Store.
type MemStore struct {
	mu   sync.Mutex
	data map[string]State
}

func NewMemStore() *MemStore { return &MemStore{data: map[string]State{}} }

func (m *MemStore) Load(id string) (State, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.data[id]
	return s, ok, nil
}

func (m *MemStore) Save(s State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[s.AttemptID] = s
	return nil
}

// Scheduler emits receipts without provider runners.
type Scheduler struct {
	store    Store
	clock    Clock
	interval time.Duration
	// lastReceiptKey prevents duplicate same-kind same-seq.
	emitted map[string]bool
	mu      sync.Mutex
}

// New builds a Scheduler. store required.
func New(store Store, clock Clock, interval time.Duration) (*Scheduler, error) {
	if store == nil {
		return nil, fmt.Errorf("reportsched: store required")
	}
	if clock == nil {
		clock = realClock{}
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Scheduler{store: store, clock: clock, interval: interval, emitted: map[string]bool{}}, nil
}

// Start begins tracking and emits a start receipt.
func (s *Scheduler) Start(attemptID, stage string) (Receipt, error) {
	if attemptID == "" {
		return Receipt{}, fmt.Errorf("reportsched: attempt id required")
	}
	now := s.clock.Now()
	st := State{
		AttemptID:            attemptID,
		Active:               true,
		StartedAt:            now,
		NextDue:              now.Add(s.interval),
		LastReceiptAt:        now,
		LastConcreteProgress: now,
		Stage:                stage,
		ResourceState:        "ok",
		RemoteGate:           "unknown",
		Seq:                  1,
	}
	if err := s.store.Save(st); err != nil {
		return Receipt{}, err
	}
	r := s.receipt(st, KindStart, now, ActionContinue)
	return s.dedupEmit(r)
}

// NoteProgress records concrete progress and resets no-progress streak.
func (s *Scheduler) NoteProgress(attemptID string) error {
	st, ok, err := s.store.Load(attemptID)
	if err != nil {
		return err
	}
	if !ok || !st.Active {
		return ErrNotActive
	}
	st.LastConcreteProgress = s.clock.Now()
	st.NoProgressStreak = 0
	return s.store.Save(st)
}

// NoteStateChange emits an immediate state-change receipt and resets interval clock.
func (s *Scheduler) NoteStateChange(attemptID, stage string) (Receipt, error) {
	return s.immediate(attemptID, KindStateChange, stage, "", ActionContinue, true)
}

// NoteBlocker emits an immediate blocker receipt.
func (s *Scheduler) NoteBlocker(attemptID, blocker string) (Receipt, error) {
	return s.immediate(attemptID, KindBlocker, "", blocker, ActionAttention, false)
}

// NoteTerminal emits a terminal receipt and deactivates.
func (s *Scheduler) NoteTerminal(attemptID, stage string) (Receipt, error) {
	r, err := s.immediate(attemptID, KindTerminal, stage, "", ActionStop, false)
	if err != nil {
		return r, err
	}
	st, ok, err := s.store.Load(attemptID)
	if err != nil || !ok {
		return r, err
	}
	st.Active = false
	_ = s.store.Save(st)
	return r, nil
}

// Tick evaluates whether an interval receipt is due. Returns ok=false if not due.
// After two consecutive no-progress intervals, emits KindNoProgress once.
func (s *Scheduler) Tick(attemptID string) (Receipt, bool, error) {
	st, ok, err := s.store.Load(attemptID)
	if err != nil {
		return Receipt{}, false, err
	}
	if !ok || !st.Active {
		return Receipt{}, false, ErrNotActive
	}
	now := s.clock.Now()
	if now.Before(st.NextDue) {
		return Receipt{}, false, nil
	}
	// Interval due.
	st.Seq++
	st.LastReceiptAt = now
	st.NextDue = now.Add(s.interval)
	// Count no-progress when last concrete progress is outside this interval window.
	windowStart := now.Add(-s.interval)
	if !st.LastConcreteProgress.After(windowStart) {
		st.NoProgressStreak++
	} else {
		st.NoProgressStreak = 0
	}
	kind := KindInterval
	action := ActionContinue
	if st.NoProgressStreak >= 2 {
		kind = KindNoProgress
		action = ActionAttention
	}
	if err := s.store.Save(st); err != nil {
		return Receipt{}, false, err
	}
	r := s.receipt(st, kind, now, action)
	r, err = s.dedupEmit(r)
	if err != nil {
		return Receipt{}, false, err
	}
	return r, true, nil
}

// Snapshot returns persisted state (for restart tests).
func (s *Scheduler) Snapshot(attemptID string) (State, bool, error) {
	return s.store.Load(attemptID)
}

// Restore reloads state after restart without emitting.
func (s *Scheduler) Restore(st State) error {
	return s.store.Save(st)
}

func (s *Scheduler) immediate(attemptID string, kind Kind, stage, blocker string, action Action, resetClock bool) (Receipt, error) {
	st, ok, err := s.store.Load(attemptID)
	if err != nil {
		return Receipt{}, err
	}
	if !ok || !st.Active {
		return Receipt{}, ErrNotActive
	}
	now := s.clock.Now()
	st.Seq++
	st.LastReceiptAt = now
	if stage != "" {
		st.Stage = stage
	}
	if blocker != "" {
		st.Blocker = blocker
	}
	if resetClock {
		st.NextDue = now.Add(s.interval)
	}
	if err := s.store.Save(st); err != nil {
		return Receipt{}, err
	}
	r := s.receipt(st, kind, now, action)
	if blocker != "" {
		r.Blocker = blocker
	}
	return s.dedupEmit(r)
}

func (s *Scheduler) receipt(st State, kind Kind, now time.Time, action Action) Receipt {
	return Receipt{
		SchemaVersion:        SchemaVersion,
		Kind:                 kind,
		AttemptID:            st.AttemptID,
		Stage:                st.Stage,
		Elapsed:              now.Sub(st.StartedAt),
		LastConcreteProgress: st.LastConcreteProgress,
		ProcessCount:         st.ProcessCount,
		ResourceState:        st.ResourceState,
		RemoteGate:           st.RemoteGate,
		Blocker:              st.Blocker,
		NextTimeout:          st.NextDue,
		NextAction:           action,
		At:                   now,
		Seq:                  st.Seq,
	}
}

func (s *Scheduler) dedupEmit(r Receipt) (Receipt, error) {
	key := fmt.Sprintf("%s|%s|%d", r.AttemptID, r.Kind, r.Seq)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.emitted[key] {
		return Receipt{}, ErrDuplicate
	}
	s.emitted[key] = true
	return r, nil
}

// HasProviderRunner is always false — structural guarantee.
func (s *Scheduler) HasProviderRunner() bool { return false }
