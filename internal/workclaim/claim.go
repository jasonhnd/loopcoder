package workclaim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Claim is one ownership generation for a logical WorkItem under a durable
// AttemptID. Closed attempts are immutable: the attempt-scoped key is never
// reopened or replaced. A generation-safe alternate (e.g. model_unavailable)
// claims a distinct AttemptID and records SupersedesAttemptID as an explicit
// relation only — never by mutating the prior claim.
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
	// Generation increments on each successful claim of the same logical WorkItem.
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
	// SupersedesAttemptID links this claim to a prior closed attempt of the same
	// logical WorkItem (explicit relation only; prior attempt stays immutable).
	SupersedesAttemptID string `json:"supersedes_attempt_id,omitempty"`
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
	// SupersedesAttemptID optional explicit prior closed attempt of the same
	// logical WorkItem. Does not reopen or replace that attempt's terminal state.
	SupersedesAttemptID string
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
	Schema    string          `json:"schema"`
	EventID   string          `json:"event_id"`
	ClaimID   string          `json:"claim_id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

var (
	ErrInvalid  = errors.New("workclaim: invalid")
	ErrNotFound = errors.New("workclaim: not found")
	ErrStale    = errors.New("workclaim: stale generation")
)

// Store is an in-process atomic claim ledger (simulates one-immediate SQLite tx).
//
// Indexing:
//   - byAttempt: project|graph|version|workitem|attemptID → claim (closed = immutable)
//   - liveByItem: project|graph|version|workitem → claimID of current live claim only
//
// A closed attempt is never reopened or replaced in byAttempt. Generation-safe
// alternates claim a distinct AttemptID and may set SupersedesAttemptID.
type Store struct {
	mu         sync.Mutex
	byAttempt  map[string]*Claim // attempt-scoped durable identity
	liveByItem map[string]string // logical item → live claimID
	byID       map[string]*Claim
	events     []Event
	seq        int64
	genByItem  map[string]int64 // logical item generation counter
	now        func() time.Time
}

// NewStore creates a claim store with injected clock.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		byAttempt:  map[string]*Claim{},
		liveByItem: map[string]string{},
		byID:       map[string]*Claim{},
		genByItem:  map[string]int64{},
		now:        now,
	}
}

// claimLogicalKey is the logical WorkItem ownership namespace (not claimable alone).
func claimLogicalKey(project, graph string, version int, item string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "|" +
		strings.TrimSpace(graph) + "|" +
		fmt.Sprintf("%d", version) + "|" +
		strings.TrimSpace(item)
}

// claimAttemptKey is the durable attempt identity. Closed keys are immutable.
func claimAttemptKey(project, graph string, version int, item, attemptID string) string {
	return claimLogicalKey(project, graph, version, item) + "|" + strings.TrimSpace(attemptID)
}

// Claim atomically verifies readiness and creates ownership for one durable attempt.
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
					return ClaimResult{Code: ResultTerminalReused, Reason: "already_terminal:" + string(it.Terminal), Claim: nil}, nil
				}
				if it.Life == workgraph.LifeBlocked {
					return ClaimResult{Code: ResultBlocked, Reason: strings.Join(it.Reasons, ",")}, nil
				}
			}
		}
		return ClaimResult{Code: ResultNotReady, Reason: "not_in_ready_set"}, nil
	}

	logical := claimLogicalKey(req.ProjectID, req.Graph.GraphID, req.Graph.Version, req.WorkItemID)
	attemptKey := claimAttemptKey(req.ProjectID, req.Graph.GraphID, req.Graph.Version, req.WorkItemID, req.AttemptID)

	// Logical success is final and immutable — never claim another attempt.
	if prevOK, prevClaim := s.logicalSucceededLocked(req.ProjectID, req.Graph.GraphID, req.Graph.Version, req.WorkItemID); prevOK {
		return ClaimResult{Code: ResultTerminalReused, Reason: "closed:succeeded", Claim: prevClaim}, nil
	}

	// Same AttemptID is immutable once recorded (closed or live).
	if prev, ok := s.byAttempt[attemptKey]; ok {
		switch prev.State {
		case StateClaimed, StateRunning:
			if !prev.LeaseUntil.IsZero() && now.After(prev.LeaseUntil) {
				if req.NonLaunchProven && prev.State == StateClaimed {
					prev.State = StateReleased
					s.clearLiveLocked(logical, prev.ClaimID)
					s.appendEventLocked(prev.ClaimID, "released_non_launch", nil, now)
					// Fall through only for a *different* attempt after release —
					// same attemptID remains released, not reopened.
					return ClaimResult{Code: ResultTerminalReused, Reason: "released_same_attempt_immutable:" + prev.AttemptID, Claim: clone(prev)}, nil
				}
				prev.State = StateAmbiguous
				s.clearLiveLocked(logical, prev.ClaimID)
				s.appendEventLocked(prev.ClaimID, "ambiguous_expired", nil, now)
				return ClaimResult{Code: ResultNeedsHuman, Reason: "expired_live_ambiguous", Claim: clone(prev)}, nil
			}
			return ClaimResult{Code: ResultAlreadyRunning, Reason: "owned_by_" + prev.ClaimID, Claim: clone(prev)}, nil
		case StateAmbiguous:
			return ClaimResult{Code: ResultNeedsHuman, Reason: "ambiguous_needs_human", Claim: clone(prev)}, nil
		case StateClosed:
			// Immutable closed attempt — never reopen or replace terminal state.
			return ClaimResult{Code: ResultTerminalReused, Reason: "closed:" + string(prev.Terminal), Claim: clone(prev)}, nil
		case StateReleased, StateExpired:
			// Same attempt identity already consumed; require a new AttemptID.
			return ClaimResult{Code: ResultTerminalReused, Reason: string(prev.State) + "_same_attempt_immutable", Claim: clone(prev)}, nil
		}
	}

	// Logical live lock: at most one live claim per WorkItem (any attempt).
	if liveID, ok := s.liveByItem[logical]; ok {
		live := s.byID[liveID]
		if live != nil {
			switch live.State {
			case StateClaimed, StateRunning:
				if !live.LeaseUntil.IsZero() && now.After(live.LeaseUntil) {
					if req.NonLaunchProven && live.State == StateClaimed {
						live.State = StateReleased
						s.clearLiveLocked(logical, live.ClaimID)
						s.appendEventLocked(live.ClaimID, "released_non_launch", nil, now)
						// continue — new AttemptID may claim
					} else {
						live.State = StateAmbiguous
						s.clearLiveLocked(logical, live.ClaimID)
						s.appendEventLocked(live.ClaimID, "ambiguous_expired", nil, now)
						return ClaimResult{Code: ResultNeedsHuman, Reason: "expired_live_ambiguous", Claim: clone(live)}, nil
					}
				} else {
					return ClaimResult{Code: ResultAlreadyRunning, Reason: "owned_by_" + live.ClaimID, Claim: clone(live)}, nil
				}
			case StateAmbiguous:
				return ClaimResult{Code: ResultNeedsHuman, Reason: "ambiguous_needs_human", Claim: clone(live)}, nil
			default:
				// Stale live pointer (closed/released) — clear and continue.
				s.clearLiveLocked(logical, liveID)
			}
		} else {
			delete(s.liveByItem, logical)
		}
	}

	// Explicit supersession: prior attempt must exist, same logical item, closed, immutable.
	supersedes := strings.TrimSpace(req.SupersedesAttemptID)
	if supersedes != "" {
		priorKey := claimAttemptKey(req.ProjectID, req.Graph.GraphID, req.Graph.Version, req.WorkItemID, supersedes)
		prior, ok := s.byAttempt[priorKey]
		if !ok || prior == nil {
			return ClaimResult{Code: ResultConflict, Reason: "supersedes_unknown_attempt"}, nil
		}
		if prior.State != StateClosed {
			return ClaimResult{Code: ResultConflict, Reason: "supersedes_not_closed:" + string(prior.State)}, nil
		}
		if !strings.EqualFold(prior.WorkItemID, req.WorkItemID) {
			return ClaimResult{Code: ResultConflict, Reason: "supersedes_work_item_mismatch"}, nil
		}
		// Prior remains immutable under priorKey; relation is recorded on the new claim only.
	}

	s.seq++
	s.genByItem[logical]++
	gen := s.genByItem[logical]
	id := fmt.Sprintf("wcl_%d", s.seq)
	c := &Claim{
		Schema: SchemaClaim, ClaimID: id,
		ProjectID: req.ProjectID, GraphID: req.Graph.GraphID, GraphVersion: req.Graph.Version,
		PlanDigest: ready.PlanDigest, WorkItemID: req.WorkItemID,
		AttemptID: req.AttemptID, ExecutorID: req.ExecutorID,
		Generation: gen, State: StateClaimed, ClaimedAt: now, RenewedAt: now,
		SupersedesAttemptID: supersedes,
	}
	if req.Lease > 0 {
		c.LeaseUntil = now.Add(req.Lease)
	}
	s.byAttempt[attemptKey] = c
	s.byID[id] = c
	s.liveByItem[logical] = id
	if supersedes != "" {
		// Type is a closed token; relation lives in Payload (stable attempt IDs).
		s.appendEventLocked(id, "claimed_superseding", map[string]string{
			"supersedes_attempt_id": supersedes,
			"attempt_id":            req.AttemptID,
		}, now)
		// Explicit relation on prior claim events only — does not mutate prior state/terminal.
		priorKey := claimAttemptKey(req.ProjectID, req.Graph.GraphID, req.Graph.Version, req.WorkItemID, supersedes)
		if prior, ok := s.byAttempt[priorKey]; ok && prior != nil {
			s.appendEventLocked(prior.ClaimID, "superseded", map[string]string{
				"by_attempt_id": req.AttemptID,
				"attempt_id":    supersedes,
			}, now)
		}
	} else {
		s.appendEventLocked(id, "claimed", nil, now)
	}
	return ClaimResult{Code: ResultClaimed, Claim: clone(c)}, nil
}

func (s *Store) logicalSucceededLocked(projectID, graphID string, version int, workItemID string) (bool, *Claim) {
	for _, c := range s.byAttempt {
		if c == nil {
			continue
		}
		if c.ProjectID != projectID || c.GraphID != graphID || c.GraphVersion != version {
			continue
		}
		if !strings.EqualFold(c.WorkItemID, workItemID) {
			continue
		}
		if c.State == StateClosed && c.Terminal == workgraph.TermSucceeded {
			return true, clone(c)
		}
	}
	return false, nil
}

func (s *Store) clearLiveLocked(logical, claimID string) {
	if id, ok := s.liveByItem[logical]; ok && (claimID == "" || id == claimID) {
		delete(s.liveByItem, logical)
	}
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
	s.appendEventLocked(c.ClaimID, "renewed", nil, now)
	return ClaimResult{Code: ResultClaimed, Claim: clone(c)}, nil
}

// Close terminals a claim with generation fence. Idempotent for same terminal.
// Closed attempt identity remains immutable under its attempt key.
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
	logical := claimLogicalKey(c.ProjectID, c.GraphID, c.GraphVersion, c.WorkItemID)
	s.clearLiveLocked(logical, c.ClaimID)
	s.appendEventLocked(c.ClaimID, "closed_"+string(req.Terminal), nil, now)
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

// GetByAttempt returns the immutable claim for a durable attempt identity.
func (s *Store) GetByAttempt(projectID, graphID string, version int, workItemID, attemptID string) (Claim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := claimAttemptKey(projectID, graphID, version, workItemID, attemptID)
	c, ok := s.byAttempt[key]
	if !ok || c == nil {
		return Claim{}, ErrNotFound
	}
	return *clone(c), nil
}

// AcceptedTerminals builds TerminalEvidence from closed claims for a graph version.
// Per logical WorkItem, the closed attempt with the highest Generation wins
// (deterministic; closed attempts remain immutable under their attempt keys).
// Map iteration order must not affect the result.
func (s *Store) AcceptedTerminals(projectID, graphID string, version int) workgraph.TerminalEvidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	type pick struct {
		term workgraph.TerminalState
		gen  int64
		aid  string // tie-break: lexicographic attempt id
	}
	best := map[string]pick{}
	for _, c := range s.byAttempt {
		if c == nil {
			continue
		}
		if c.ProjectID != projectID || c.GraphID != graphID || c.GraphVersion != version {
			continue
		}
		if c.State != StateClosed || c.Terminal == workgraph.TermNone {
			continue
		}
		prev, ok := best[c.WorkItemID]
		if !ok || c.Generation > prev.gen ||
			(c.Generation == prev.gen && c.AttemptID > prev.aid) {
			best[c.WorkItemID] = pick{term: c.Terminal, gen: c.Generation, aid: c.AttemptID}
		}
	}
	ev := workgraph.TerminalEvidence{}
	for id, p := range best {
		ev[id] = p.term
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

func (s *Store) appendEventLocked(claimID, typ string, payload any, now time.Time) {
	s.seq++
	var raw json.RawMessage
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil {
			raw = b
		}
	}
	s.events = append(s.events, Event{
		Schema: SchemaEvent, EventID: fmt.Sprintf("wce_%d", s.seq),
		ClaimID: claimID, Type: typ, Payload: raw, CreatedAt: now,
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
