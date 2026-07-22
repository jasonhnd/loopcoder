package workclaim

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const (
	SchemaClaim   = "loopcoder.workclaim.v1"
	SchemaClose   = "loopcoder.workclaim.close.v1"
	SchemaEvent   = "loopcoder.workclaim.event.v1"
	PolicyVersion = "workclaim-v1"
)

// ResultCode is the typed claim outcome.
type ResultCode string

const (
	ResultClaimed         ResultCode = "claimed"
	ResultAlreadyRunning  ResultCode = "already_running"
	ResultTerminalReused  ResultCode = "terminal_reused"
	ResultBlocked         ResultCode = "blocked"
	ResultConflict        ResultCode = "conflict"
	ResultNeedsHuman      ResultCode = "needs_human"
	ResultNotReady        ResultCode = "not_ready"
	ResultClosed          ResultCode = "closed"
	ResultIdempotentClose ResultCode = "idempotent_close"
	ResultStaleGeneration ResultCode = "stale_generation"
)

// ClaimState is the durable claim record.
type ClaimState string

const (
	StateClaimed   ClaimState = "claimed"
	StateRunning   ClaimState = "running"
	StateClosed    ClaimState = "closed"
	StateReleased  ClaimState = "released"
	StateExpired   ClaimState = "expired"
	StateAmbiguous ClaimState = "ambiguous" // needs-human; not auto-reclaimable
)

// Claim is one ownership generation for a WorkItem.
type Claim struct {
	Schema       string `json:"schema"`
	ClaimID      string `json:"claim_id"`
	ProjectID    string `json:"project_id"`
	GraphID      string `json:"graph_id"`
	GraphVersion int    `json:"graph_version"`
	PlanDigest   string `json:"plan_digest"`
	WorkItemID   string `json:"work_item_id"`
	AttemptID    string `json:"attempt_id"`
	ExecutorID   string `json:"executor_id"`
	// Generation increments on each successful claim of the same WorkItem key.
	Generation int64      `json:"generation"`
	State      ClaimState `json:"state"`
	ClaimedAt  time.Time  `json:"claimed_at"`
	RenewedAt  time.Time  `json:"renewed_at,omitempty"`
	// LeaseUntil optional soft lease; expiry without proof → ambiguous.
	LeaseUntil time.Time `json:"lease_until,omitempty"`
	// Terminal after close.
	Terminal workgraph.TerminalState `json:"terminal,omitempty"`
	// OutputEvidence required for success close.
	OutputEvidence string    `json:"output_evidence,omitempty"`
	ClosedAt       time.Time `json:"closed_at,omitempty"`
}

// ClaimRequest is the input to Claim.
type ClaimRequest struct {
	ProjectID  string
	Graph      workgraph.Graph
	Evidence   workgraph.TerminalEvidence
	WorkItemID string
	AttemptID  string
	ExecutorID string
	// Lease duration; zero = no lease (manual close only).
	Lease time.Duration
	// NonLaunchProven allows reclaim of expired claim when execution never started.
	NonLaunchProven bool
}

// ClaimResult is the typed atomic claim outcome.
type ClaimResult struct {
	Code   ResultCode `json:"code"`
	Claim  *Claim     `json:"claim,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// CloseRequest closes a claim with generation fence.
type CloseRequest struct {
	ClaimID        string
	Generation     int64
	ExecutorID     string
	AttemptID      string
	Terminal       workgraph.TerminalState
	OutputEvidence string // required for TermSucceeded
}

// Event is an append-only lifecycle record.
type Event struct {
	Schema    string    `json:"schema"`
	EventID   string    `json:"event_id"`
	ClaimID   string    `json:"claim_id"`
	Type      string    `json:"type"`
	Payload   string    `json:"payload,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	ErrInvalid  = errors.New("workclaim: invalid")
	ErrNotFound = errors.New("workclaim: not found")
	ErrStale    = errors.New("workclaim: stale generation")
)

// Store is an in-process atomic claim ledger (simulates one-immediate SQLite tx).
type Store struct {
	mu       sync.Mutex
	byKey    map[string]*Claim // project|graph|version|workitem → active/closed claim
	byID     map[string]*Claim
	events   []Event
	seq      int64
	genByKey map[string]int64
	now      func() time.Time
}

// NewStore creates a claim store with injected clock.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		byKey: map[string]*Claim{}, byID: map[string]*Claim{},
		genByKey: map[string]int64{}, now: now,
	}
}

func claimKey(project, graph string, version int, item string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "|" +
		strings.TrimSpace(graph) + "|" +
		fmt.Sprintf("%d", version) + "|" +
		strings.TrimSpace(item)
}

// Claim atomically verifies readiness and creates ownership for one WorkItem.
func (s *Store) Claim(req ClaimRequest) (ClaimResult, error) {
	if s == nil {
		return ClaimResult{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if strings.TrimSpace(req.ProjectID) == "" || strings.TrimSpace(req.WorkItemID) == "" ||
		strings.TrimSpace(req.AttemptID) == "" || strings.TrimSpace(req.ExecutorID) == "" {
		return ClaimResult{}, fmt.Errorf("%w: project/workitem/attempt/executor required", ErrInvalid)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()

	// Validate graph + readiness in the same critical section (atomic with claim).
	ready := workgraph.EvaluateReady(req.Graph, req.Evidence)
	if !ready.Valid {
		return ClaimResult{Code: ResultBlocked, Reason: "invalid_graph"}, nil
	}
	if !workgraph.ReadyContains(ready, req.WorkItemID) {
		// check if terminal
		for _, it := range ready.Items {
			if it.ID == req.WorkItemID {
				if it.Life == workgraph.LifeTerminal {
					return ClaimResult{Code: ResultTerminalReused, Reason: "already_terminal:" + string(it.Terminal)}, nil
				}
				if it.Life == workgraph.LifeBlocked {
					return ClaimResult{Code: ResultBlocked, Reason: strings.Join(it.Reasons, ",")}, nil
				}
			}
		}
		return ClaimResult{Code: ResultNotReady, Reason: "not_in_ready_set"}, nil
	}

	key := claimKey(req.ProjectID, req.Graph.GraphID, req.Graph.Version, req.WorkItemID)
	if prev, ok := s.byKey[key]; ok {
		switch prev.State {
		case StateClaimed, StateRunning:
			// live owner
			if !prev.LeaseUntil.IsZero() && now.After(prev.LeaseUntil) {
				// expired lease
				if req.NonLaunchProven && prev.State == StateClaimed {
					// reclaim allowed
					prev.State = StateReleased
					s.appendEventLocked(prev.ClaimID, "released_non_launch", now)
				} else {
					prev.State = StateAmbiguous
					s.appendEventLocked(prev.ClaimID, "ambiguous_expired", now)
					return ClaimResult{Code: ResultNeedsHuman, Reason: "expired_live_ambiguous", Claim: clone(prev)}, nil
				}
			} else {
				return ClaimResult{Code: ResultAlreadyRunning, Reason: "owned_by_" + prev.ClaimID, Claim: clone(prev)}, nil
			}
		case StateAmbiguous:
			return ClaimResult{Code: ResultNeedsHuman, Reason: "ambiguous_needs_human", Claim: clone(prev)}, nil
		case StateClosed:
			return ClaimResult{Code: ResultTerminalReused, Reason: "closed:" + string(prev.Terminal), Claim: clone(prev)}, nil
		case StateReleased, StateExpired:
			// may claim again
		}
	}

	s.seq++
	s.genByKey[key]++
	gen := s.genByKey[key]
	id := fmt.Sprintf("wcl_%d", s.seq)
	c := &Claim{
		Schema: SchemaClaim, ClaimID: id,
		ProjectID: req.ProjectID, GraphID: req.Graph.GraphID, GraphVersion: req.Graph.Version,
		PlanDigest: ready.PlanDigest, WorkItemID: req.WorkItemID,
		AttemptID: req.AttemptID, ExecutorID: req.ExecutorID,
		Generation: gen, State: StateClaimed, ClaimedAt: now, RenewedAt: now,
	}
	if req.Lease > 0 {
		c.LeaseUntil = now.Add(req.Lease)
	}
	s.byKey[key] = c
	s.byID[id] = c
	s.appendEventLocked(id, "claimed", now)
	return ClaimResult{Code: ResultClaimed, Claim: clone(c)}, nil
}

// Renew extends a lease if generation/executor/attempt match.
func (s *Store) Renew(claimID string, generation int64, executorID, attemptID string, lease time.Duration) (ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[strings.TrimSpace(claimID)]
	if !ok {
		return ClaimResult{}, ErrNotFound
	}
	if c.Generation != generation || c.ExecutorID != executorID || c.AttemptID != attemptID {
		return ClaimResult{Code: ResultStaleGeneration, Reason: "fence_mismatch"}, ErrStale
	}
	if c.State != StateClaimed && c.State != StateRunning {
		return ClaimResult{Code: ResultConflict, Reason: "not_renewable:" + string(c.State)}, nil
	}
	now := s.now().UTC()
	c.State = StateRunning
	c.RenewedAt = now
	if lease > 0 {
		c.LeaseUntil = now.Add(lease)
	}
	s.appendEventLocked(c.ClaimID, "renewed", now)
	return ClaimResult{Code: ResultClaimed, Claim: clone(c)}, nil
}

// Close terminals a claim with generation fence. Idempotent for same terminal.
func (s *Store) Close(req CloseRequest) (ClaimResult, error) {
	if req.Terminal == workgraph.TermNone {
		return ClaimResult{}, fmt.Errorf("%w: terminal required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[strings.TrimSpace(req.ClaimID)]
	if !ok {
		return ClaimResult{}, ErrNotFound
	}
	if c.Generation != req.Generation || c.ExecutorID != req.ExecutorID || c.AttemptID != req.AttemptID {
		return ClaimResult{Code: ResultStaleGeneration, Reason: "fence_mismatch"}, ErrStale
	}
	if c.State == StateClosed {
		if c.Terminal == req.Terminal {
			return ClaimResult{Code: ResultIdempotentClose, Claim: clone(c)}, nil
		}
		return ClaimResult{Code: ResultConflict, Reason: "already_closed_different_terminal"}, nil
	}
	if c.State == StateAmbiguous {
		return ClaimResult{Code: ResultNeedsHuman, Reason: "ambiguous_cannot_close"}, nil
	}
	if req.Terminal == workgraph.TermSucceeded && strings.TrimSpace(req.OutputEvidence) == "" {
		return ClaimResult{Code: ResultConflict, Reason: "success_requires_output_evidence"}, nil
	}
	now := s.now().UTC()
	c.State = StateClosed
	c.Terminal = req.Terminal
	c.OutputEvidence = req.OutputEvidence
	c.ClosedAt = now
	s.appendEventLocked(c.ClaimID, "closed_"+string(req.Terminal), now)
	return ClaimResult{Code: ResultClosed, Claim: clone(c)}, nil
}

// Get returns a claim by ID.
func (s *Store) Get(claimID string) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[strings.TrimSpace(claimID)]
	if !ok {
		return Claim{}, ErrNotFound
	}
	return *clone(c), nil
}

// AcceptedTerminals builds TerminalEvidence from closed claims for a graph version.
func (s *Store) AcceptedTerminals(projectID, graphID string, version int) workgraph.TerminalEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := workgraph.TerminalEvidence{}
	prefix := claimKey(projectID, graphID, version, "")
	// scan byKey
	for k, c := range s.byKey {
		if !strings.HasPrefix(k, strings.TrimSuffix(prefix, "|")) && !strings.Contains(k, fmt.Sprintf("|%s|%d|", graphID, version)) {
			continue
		}
		if c.ProjectID != projectID || c.GraphID != graphID || c.GraphVersion != version {
			continue
		}
		if c.State == StateClosed && c.Terminal != workgraph.TermNone {
			ev[c.WorkItemID] = c.Terminal
		}
	}
	return ev
}

// Events returns a copy of claim events.
func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *Store) appendEventLocked(claimID, typ string, now time.Time) {
	s.seq++
	s.events = append(s.events, Event{
		Schema: SchemaEvent, EventID: fmt.Sprintf("wce_%d", s.seq),
		ClaimID: claimID, Type: typ, CreatedAt: now,
	})
}

func clone(c *Claim) *Claim {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// ConcurrentClaimBarrier runs two claim attempts under a barrier for tests.
func ConcurrentClaimBarrier(s *Store, a, b ClaimRequest) (ClaimResult, ClaimResult) {
	var r1, r2 ClaimResult
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		r1, _ = s.Claim(a)
	}()
	go func() {
		defer wg.Done()
		<-start
		r2, _ = s.Claim(b)
	}()
	close(start)
	wg.Wait()
	return r1, r2
}

// DigestClaim is a stable content fingerprint for tests.
func DigestClaim(c Claim) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%d|%s", c.ClaimID, c.WorkItemID, c.AttemptID, c.Generation, c.GraphVersion, c.State)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}
