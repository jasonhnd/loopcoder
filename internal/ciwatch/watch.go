package ciwatch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SchemaSnapshot   = "loopcoder.ciwatch.snapshot.v1"
	SchemaEvent      = "loopcoder.ciwatch.event.v1"
	SchemaCheckpoint = "loopcoder.ciwatch.checkpoint.v1"
)

// Class is a semantic wait classification.
type Class string

const (
	ClassPending         Class = "pending"
	ClassSuccess         Class = "success"
	ClassFailure         Class = "failure"
	ClassCancelled       Class = "cancelled"
	ClassSkipped         Class = "skipped"
	ClassMissingRequired Class = "missing_required"
	ClassApprovalNeeded  Class = "approval_needed"
	ClassChangedHead     Class = "changed_head"
	ClassRateLimited     Class = "rate_limited"
	ClassUnavailable     Class = "unavailable"
)

// CheckState is one remote check observation.
type CheckState struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"` // success|failure|cancelled|skipped|pending|""
	Required   bool   `json:"required"`
}

// RemoteSnapshot is one poll/notification payload.
type RemoteSnapshot struct {
	PRNumber          int
	HeadOID           string
	Checks            []CheckState
	Approvals         int
	RequiredApprovals int
	RateLimited       bool
	Unavailable       bool
	ObservedAt        time.Time
}

// RequirementPolicy freezes required check names for a PR head.
type RequirementPolicy struct {
	RequiredChecks    []string `json:"required_checks"`
	RequiredApprovals int      `json:"required_approvals"`
	// OptionalEvidence names (e.g. greptile) never hard-block unless listed in RequiredChecks.
	OptionalEvidence []string `json:"optional_evidence"`
}

// WatchState is durable watcher state (restartable).
type WatchState struct {
	Schema        string            `json:"schema"`
	PRNumber      int               `json:"pr_number"`
	HeadOID       string            `json:"head_oid"`
	Policy        RequirementPolicy `json:"policy"`
	Class         Class             `json:"class"`
	LastEventKey  string            `json:"last_event_key"`
	BackoffUntil  time.Time         `json:"backoff_until,omitempty"`
	PollInterval  time.Duration     `json:"poll_interval"`
	ReportDueAt   time.Time         `json:"report_due_at"`
	ProviderCalls int               `json:"provider_calls"` // must stay 0
	EventsEmitted int               `json:"events_emitted"`
	CheckpointAt  time.Time         `json:"checkpoint_at"`
}

// Event is a deduplicated semantic transition.
type Event struct {
	Schema   string    `json:"schema"`
	Key      string    `json:"key"`
	Class    Class     `json:"class"`
	PRNumber int       `json:"pr_number"`
	HeadOID  string    `json:"head_oid"`
	Message  string    `json:"message"`
	At       time.Time `json:"at"`
}

// Store is durable checkpoint storage.
type Store struct {
	mu    sync.Mutex
	state map[int]*WatchState // pr number
	now   func() time.Time
}

func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{state: map[int]*WatchState{}, now: now}
}

// Watcher is the zero-model wait machine.
type Watcher struct {
	Store *Store
	// Clock for tests
	Now func() time.Time
	// Min/Max poll intervals for adaptive backoff
	MinInterval time.Duration
	MaxInterval time.Duration
	// ReportEvery is the mandatory report cadence (e.g. 5m)
	ReportEvery time.Duration
}

// Start initializes watch for a PR with frozen policy and head.
func (w *Watcher) Start(pr int, headOID string, policy RequirementPolicy) (WatchState, error) {
	if pr <= 0 || headOID == "" || len(policy.RequiredChecks) == 0 {
		return WatchState{}, errors.New("ciwatch: invalid start")
	}
	now := w.now()
	minI := w.MinInterval
	if minI <= 0 {
		minI = 15 * time.Second
	}
	rep := w.ReportEvery
	if rep <= 0 {
		rep = 5 * time.Minute
	}
	st := &WatchState{
		Schema: SchemaCheckpoint, PRNumber: pr, HeadOID: headOID, Policy: normalizePolicy(policy),
		Class: ClassPending, PollInterval: minI, ReportDueAt: now.Add(rep),
		CheckpointAt: now, ProviderCalls: 0,
	}
	w.Store.mu.Lock()
	w.Store.state[pr] = st
	w.Store.mu.Unlock()
	return *st, nil
}

// Checkpoint returns restartable state.
func (w *Watcher) Checkpoint(pr int) (WatchState, error) {
	w.Store.mu.Lock()
	defer w.Store.mu.Unlock()
	st, ok := w.Store.state[pr]
	if !ok {
		return WatchState{}, errors.New("ciwatch: not found")
	}
	return *st, nil
}

// Restore loads a checkpoint (restart without losing state).
func (w *Watcher) Restore(st WatchState) {
	w.Store.mu.Lock()
	cp := st
	w.Store.state[st.PRNumber] = &cp
	w.Store.mu.Unlock()
}

// Observe ingests one remote snapshot; returns at most one new semantic event.
// Never invokes a provider/model.
func (w *Watcher) Observe(ctx context.Context, snap RemoteSnapshot) (Event, bool, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, false, err
	}
	// structural: no provider path exists on this type
	w.Store.mu.Lock()
	defer w.Store.mu.Unlock()
	st, ok := w.Store.state[snap.PRNumber]
	if !ok {
		return Event{}, false, errors.New("ciwatch: not started")
	}
	// provider calls must remain zero
	if st.ProviderCalls != 0 {
		return Event{}, false, errors.New("ciwatch: provider dependency violated")
	}
	now := w.now()
	if !st.BackoffUntil.IsZero() && now.Before(st.BackoffUntil) {
		// do not busy poll — ignore observation until backoff ends
		return Event{}, false, nil
	}

	class, msg := classify(st, snap)
	// head/policy change invalidates readiness
	if snap.HeadOID != "" && snap.HeadOID != st.HeadOID {
		class = ClassChangedHead
		msg = "pr head changed; prior readiness invalid"
		st.HeadOID = snap.HeadOID
	}

	eventKey := fmt.Sprintf("%d|%s|%s", snap.PRNumber, st.HeadOID, class)
	if eventKey == st.LastEventKey {
		// duplicate remote response — no flood
		w.adaptBackoff(st, snap, false)
		return Event{}, false, nil
	}

	// only emit on class change or first
	prev := st.Class
	st.Class = class
	if class == ClassRateLimited || class == ClassUnavailable {
		w.adaptBackoff(st, snap, true)
	} else {
		w.adaptBackoff(st, snap, false)
	}

	if class == prev && st.LastEventKey != "" && !strings.HasPrefix(string(class), "changed") {
		// same class after first — still dedupe by key
		if eventKey == st.LastEventKey {
			return Event{}, false, nil
		}
	}

	st.LastEventKey = eventKey
	st.EventsEmitted++
	st.CheckpointAt = now
	ev := Event{
		Schema: SchemaEvent, Key: eventKey, Class: class,
		PRNumber: snap.PRNumber, HeadOID: st.HeadOID, Message: msg, At: now,
	}
	return ev, true, nil
}

// DueReport returns true when a timed report should emit (no model).
func (w *Watcher) DueReport(pr int) bool {
	w.Store.mu.Lock()
	defer w.Store.mu.Unlock()
	st, ok := w.Store.state[pr]
	if !ok {
		return false
	}
	return !w.now().Before(st.ReportDueAt)
}

// MarkReported schedules next report.
func (w *Watcher) MarkReported(pr int) {
	w.Store.mu.Lock()
	defer w.Store.mu.Unlock()
	st, ok := w.Store.state[pr]
	if !ok {
		return
	}
	rep := w.ReportEvery
	if rep <= 0 {
		rep = 5 * time.Minute
	}
	st.ReportDueAt = w.now().Add(rep)
}

// NextPollAfter returns wait duration until next allowed poll (no busy-poll).
func (w *Watcher) NextPollAfter(pr int) time.Duration {
	w.Store.mu.Lock()
	defer w.Store.mu.Unlock()
	st, ok := w.Store.state[pr]
	if !ok {
		return w.MinInterval
	}
	now := w.now()
	if !st.BackoffUntil.IsZero() && now.Before(st.BackoffUntil) {
		return st.BackoffUntil.Sub(now)
	}
	if st.PollInterval <= 0 {
		return 15 * time.Second
	}
	return st.PollInterval
}

// Ready is true only on success classification for current head.
func (w *Watcher) Ready(pr int) bool {
	w.Store.mu.Lock()
	defer w.Store.mu.Unlock()
	st, ok := w.Store.state[pr]
	return ok && st.Class == ClassSuccess
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return w.Store.now().UTC()
}

func (w *Watcher) adaptBackoff(st *WatchState, snap RemoteSnapshot, escalate bool) {
	minI := w.MinInterval
	if minI <= 0 {
		minI = 15 * time.Second
	}
	maxI := w.MaxInterval
	if maxI <= 0 {
		maxI = 5 * time.Minute
	}
	if escalate || snap.RateLimited || snap.Unavailable {
		next := st.PollInterval * 2
		if next < minI {
			next = minI
		}
		if next > maxI {
			next = maxI
		}
		st.PollInterval = next
		st.BackoffUntil = w.now().Add(next)
		return
	}
	// decay toward min on healthy observations
	if st.PollInterval > minI {
		st.PollInterval = st.PollInterval / 2
		if st.PollInterval < minI {
			st.PollInterval = minI
		}
	}
	st.BackoffUntil = time.Time{}
}

func classify(st *WatchState, snap RemoteSnapshot) (Class, string) {
	if snap.Unavailable {
		return ClassUnavailable, "github unavailable"
	}
	if snap.RateLimited {
		return ClassRateLimited, "rate limited"
	}
	required := map[string]bool{}
	for _, n := range st.Policy.RequiredChecks {
		required[strings.ToLower(n)] = true
	}
	optional := map[string]bool{}
	for _, n := range st.Policy.OptionalEvidence {
		optional[strings.ToLower(n)] = true
	}

	seenRequired := map[string]CheckState{}
	pending := false
	for _, c := range snap.Checks {
		name := strings.ToLower(c.Name)
		if required[name] {
			seenRequired[name] = c
			switch strings.ToLower(c.Conclusion) {
			case "failure":
				return ClassFailure, "required check failed: " + c.Name
			case "cancelled":
				return ClassCancelled, "required check cancelled: " + c.Name
			case "skipped":
				// skipped required still missing success
				return ClassSkipped, "required check skipped: " + c.Name
			case "success":
				// ok
			default:
				pending = true
			}
		}
		// optional evidence never blocks unless also required
		_ = optional
	}
	for name := range required {
		if _, ok := seenRequired[name]; !ok {
			return ClassMissingRequired, "missing required check: " + name
		}
	}
	if pending {
		return ClassPending, "required checks pending"
	}
	if snap.RequiredApprovals > 0 && snap.Approvals < snap.RequiredApprovals {
		// use policy required approvals
	}
	need := st.Policy.RequiredApprovals
	if need > 0 && snap.Approvals < need {
		return ClassApprovalNeeded, "approval needed"
	}
	return ClassSuccess, "required checks green"
}

func normalizePolicy(p RequirementPolicy) RequirementPolicy {
	req := append([]string(nil), p.RequiredChecks...)
	opt := append([]string(nil), p.OptionalEvidence...)
	sort.Strings(req)
	sort.Strings(opt)
	// remove optionals that are also required — required wins
	reqSet := map[string]bool{}
	for _, r := range req {
		reqSet[strings.ToLower(r)] = true
	}
	var opt2 []string
	for _, o := range opt {
		if !reqSet[strings.ToLower(o)] {
			opt2 = append(opt2, o)
		}
	}
	p.RequiredChecks = req
	p.OptionalEvidence = opt2
	return p
}

// AssertNoProviderDependency is a structural marker for tests.
func AssertNoProviderDependency() {
	// Watcher has no provider field; Observe never calls models.
}
