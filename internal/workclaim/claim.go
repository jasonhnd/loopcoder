package workclaim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const (
	SchemaClaim   = "loopcoder.workclaim.v1"
	SchemaClose   = "loopcoder.workclaim.close.v1"
	SchemaEvent   = "loopcoder.workclaim.event.v1"
	SchemaFile    = "loopcoder.workclaim.store.v1"
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
	// PlanDigest is the canonical ExecutionPlanDigest (workflowdef.Normalize digest).
	// Never a workgraph.DigestGraph value.
	PlanDigest string `json:"plan_digest"`
	// GraphDigest is the separate workgraph.DigestGraph identity (not used for
	// attempt/capacity keys). Empty only on pre-2A-1 records.
	GraphDigest string `json:"graph_digest,omitempty"`
	// TaskClass is the expected classified capability floor (canonical).
	TaskClass string `json:"task_class,omitempty"`
	// ChildContractDigest is the expected pre-claim assignment digest.
	ChildContractDigest string `json:"child_contract_digest,omitempty"`
	WorkItemID          string `json:"work_item_id"`
	AttemptID           string `json:"attempt_id"`
	ExecutorID          string `json:"executor_id"`
	// Generation increments on each successful claim of the same logical WorkItem.
	// Positive (≥1) for live claims created by Claim.
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
	// PlanDigest is the required nonempty canonical ExecutionPlanDigest
	// (workflowdef.Normalize). No fallback to Graph.PlanDigest / ready.PlanDigest.
	PlanDigest string
	// TaskClass is the required expected classified floor (canonical token).
	TaskClass string
	// ChildContractDigest is the required expected pre-claim assignment digest.
	ChildContractDigest string
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

// Store is an atomic claim ledger. When path is set (OpenPath), mutations
// persist to disk so process restart retains closed/open claim truth.
//
// Indexing:
//   - byAttempt: project|graph|version|workitem|attemptID → claim (closed = immutable)
//   - liveByItem: project|graph|version|workitem → claimID of current live claim only
//
// A closed attempt is never reopened or replaced in byAttempt. Generation-safe
// alternates claim a distinct AttemptID and may set SupersedesAttemptID.
type Store struct {
	mu         sync.Mutex
	path       string            // optional durable JSON path
	byAttempt  map[string]*Claim // attempt-scoped durable identity
	liveByItem map[string]string // logical item → live claimID
	byID       map[string]*Claim
	events     []Event
	seq        int64
	genByItem  map[string]int64 // logical item generation counter
	now        func() time.Time
	// TestFailSave when non-nil forces saveLocked to fail (tests only).
	TestFailSave error
}

type fileDoc struct {
	Schema     string            `json:"schema"`
	SavedAt    time.Time         `json:"saved_at"`
	Seq        int64             `json:"seq"`
	GenByItem  map[string]int64  `json:"gen_by_item,omitempty"`
	LiveByItem map[string]string `json:"live_by_item,omitempty"`
	Claims     []Claim           `json:"claims"`
	Events     []Event           `json:"events,omitempty"`
}

// NewStore creates an in-memory claim store with injected clock (tests).
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

// OpenPath opens or creates a durable claim store. Fail closed on corrupt JSON,
// schema mismatch, or duplicate claim identity.
func OpenPath(path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("%w: empty claim store path", ErrInvalid)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{
		path:       path,
		byAttempt:  map[string]*Claim{},
		liveByItem: map[string]string{},
		byID:       map[string]*Claim{},
		genByItem:  map[string]int64{},
		now:        now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	if s == nil || s.path == "" {
		return nil
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var doc fileDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("%w: corrupt claim store JSON: %v", ErrInvalid, err)
	}
	// Schema required and exact — no silent upgrade of empty/wrong schema.
	if strings.TrimSpace(doc.Schema) == "" {
		return fmt.Errorf("%w: claim store missing schema", ErrInvalid)
	}
	if doc.Schema != SchemaFile {
		return fmt.Errorf("%w: claim store schema %q", ErrInvalid, doc.Schema)
	}
	seenID := map[string]bool{}
	seenAttempt := map[string]bool{}
	seenEvent := map[string]bool{}
	maxSeq := doc.Seq
	for i := range doc.Claims {
		c := doc.Claims[i]
		if strings.TrimSpace(c.Schema) == "" || strings.TrimSpace(c.Schema) != SchemaClaim {
			return fmt.Errorf("%w: claim schema want %q got %q", ErrInvalid, SchemaClaim, c.Schema)
		}
		if strings.TrimSpace(c.ClaimID) == "" || strings.TrimSpace(c.AttemptID) == "" {
			return fmt.Errorf("%w: claim missing claim_id/attempt_id", ErrInvalid)
		}
		if !strings.HasPrefix(c.ClaimID, "wcl_") || parseWclSeq(c.ClaimID) <= 0 {
			return fmt.Errorf("%w: claim_id must be wcl_N got %q", ErrInvalid, c.ClaimID)
		}
		if c.Generation <= 0 {
			return fmt.Errorf("%w: claim %q generation must be positive", ErrInvalid, c.ClaimID)
		}
		if strings.TrimSpace(c.ProjectID) == "" || strings.TrimSpace(c.GraphID) == "" ||
			strings.TrimSpace(c.WorkItemID) == "" || strings.TrimSpace(string(c.State)) == "" {
			return fmt.Errorf("%w: claim %q missing ProjectID/GraphID/WorkItemID/State", ErrInvalid, c.ClaimID)
		}
		if c.Generation < 0 {
			return fmt.Errorf("%w: claim %q malformed generation", ErrInvalid, c.ClaimID)
		}
		if seenID[c.ClaimID] {
			return fmt.Errorf("%w: duplicate claim_id %q", ErrInvalid, c.ClaimID)
		}
		seenID[c.ClaimID] = true
		akey := claimAttemptKey(c.ProjectID, c.GraphID, c.GraphVersion, c.WorkItemID, c.AttemptID)
		if seenAttempt[akey] {
			return fmt.Errorf("%w: duplicate attempt key %q", ErrInvalid, akey)
		}
		seenAttempt[akey] = true
		if n := parseWclSeq(c.ClaimID); n > maxSeq {
			maxSeq = n
		}
		cp := c
		s.byID[c.ClaimID] = &cp
		s.byAttempt[akey] = &cp
	}
	if doc.LiveByItem != nil {
		for logical, cid := range doc.LiveByItem {
			c, ok := s.byID[cid]
			if !ok || c == nil {
				return fmt.Errorf("%w: live pointer %q -> missing claim %q", ErrInvalid, logical, cid)
			}
			// Live pointers must be claimed/running only — never ambiguous/closed/released/expired.
			if c.State != StateClaimed && c.State != StateRunning {
				return fmt.Errorf("%w: live pointer %q -> non-live claim state %s (claimed|running only)", ErrInvalid, logical, c.State)
			}
			// Logical key must match claim identity.
			wantLogical := claimLogicalKey(c.ProjectID, c.GraphID, c.GraphVersion, c.WorkItemID)
			if logical != wantLogical {
				return fmt.Errorf("%w: live pointer key %q != claim logical %q", ErrInvalid, logical, wantLogical)
			}
			s.liveByItem[logical] = cid
		}
	}
	// GenByItem: recompute high-water from claims; reject unknown keys and missing high-water.
	recomputedGen := map[string]int64{}
	for _, c := range s.byID {
		lk := claimLogicalKey(c.ProjectID, c.GraphID, c.GraphVersion, c.WorkItemID)
		if c.Generation > recomputedGen[lk] {
			recomputedGen[lk] = c.Generation
		}
	}
	if doc.GenByItem != nil {
		for k, v := range doc.GenByItem {
			if v <= 0 {
				return fmt.Errorf("%w: gen_by_item %q non-positive %d", ErrInvalid, k, v)
			}
			want, ok := recomputedGen[k]
			if !ok {
				return fmt.Errorf("%w: gen_by_item unknown key %q (no claims)", ErrInvalid, k)
			}
			if v != want {
				return fmt.Errorf("%w: gen_by_item %q high-water %d != recomputed %d", ErrInvalid, k, v, want)
			}
		}
		// Every claim logical key must appear in gen_by_item when map is present.
		for k, want := range recomputedGen {
			if got, ok := doc.GenByItem[k]; !ok || got != want {
				return fmt.Errorf("%w: gen_by_item missing or wrong key %q want %d", ErrInvalid, k, want)
			}
		}
		s.genByItem = map[string]int64{}
		for k, v := range recomputedGen {
			s.genByItem[k] = v
		}
	} else if len(recomputedGen) > 0 {
		// Missing gen_by_item with claims: recompute high-water from claims (fail closed on
		// unknown keys only when the map is present and inconsistent).
		s.genByItem = map[string]int64{}
		for k, v := range recomputedGen {
			s.genByItem[k] = v
		}
	}
	// Validate claim states and terminal consistency.
	validState := map[ClaimState]bool{
		StateClaimed: true, StateRunning: true, StateClosed: true,
		StateReleased: true, StateExpired: true, StateAmbiguous: true,
	}
	liveCount := map[string]int{}
	for _, c := range s.byID {
		if !validState[c.State] {
			return fmt.Errorf("%w: claim %q invalid state %q", ErrInvalid, c.ClaimID, c.State)
		}
		if c.State == StateClosed {
			if c.Terminal == workgraph.TermNone {
				return fmt.Errorf("%w: closed claim %q missing terminal", ErrInvalid, c.ClaimID)
			}
			if c.Terminal == workgraph.TermSucceeded && strings.TrimSpace(c.OutputEvidence) == "" {
				return fmt.Errorf("%w: succeeded claim %q missing evidence", ErrInvalid, c.ClaimID)
			}
		} else if c.Terminal != workgraph.TermNone && c.Terminal != "" {
			return fmt.Errorf("%w: non-closed claim %q has terminal", ErrInvalid, c.ClaimID)
		}
		if strings.TrimSpace(c.ExecutorID) == "" {
			return fmt.Errorf("%w: claim %q missing executor", ErrInvalid, c.ClaimID)
		}
		if c.State == StateClaimed || c.State == StateRunning {
			lk := claimLogicalKey(c.ProjectID, c.GraphID, c.GraphVersion, c.WorkItemID)
			liveCount[lk]++
		}
	}
	for lk, n := range liveCount {
		if n > 1 {
			return fmt.Errorf("%w: multiple live claims for %q", ErrInvalid, lk)
		}
	}
	// Every live claim must appear exactly once in live_by_item.
	for lk, n := range liveCount {
		if n != 1 {
			continue
		}
		if doc.LiveByItem == nil {
			return fmt.Errorf("%w: live claim missing live_by_item map for %q", ErrInvalid, lk)
		}
		if _, ok := doc.LiveByItem[lk]; !ok {
			return fmt.Errorf("%w: live claim missing from live_by_item for %q", ErrInvalid, lk)
		}
	}
	if len(doc.Events) > 0 {
		for _, ev := range doc.Events {
			if strings.TrimSpace(ev.Schema) == "" || strings.TrimSpace(ev.Schema) != SchemaEvent {
				return fmt.Errorf("%w: claim event schema required %q", ErrInvalid, SchemaEvent)
			}
			if strings.TrimSpace(ev.EventID) == "" {
				return fmt.Errorf("%w: claim event missing event_id", ErrInvalid)
			}
			if !strings.HasPrefix(ev.EventID, "wce_") || parseWceSeq(ev.EventID) <= 0 {
				return fmt.Errorf("%w: claim event_id must be wce_N", ErrInvalid)
			}
			if seenEvent[ev.EventID] {
				return fmt.Errorf("%w: duplicate claim event_id %q", ErrInvalid, ev.EventID)
			}
			seenEvent[ev.EventID] = true
			if strings.TrimSpace(ev.ClaimID) == "" {
				return fmt.Errorf("%w: claim event missing claim_id", ErrInvalid)
			}
			if _, ok := s.byID[ev.ClaimID]; !ok {
				return fmt.Errorf("%w: claim event claim_id %q unknown", ErrInvalid, ev.ClaimID)
			}
			if strings.TrimSpace(ev.Type) == "" {
				return fmt.Errorf("%w: claim event missing type", ErrInvalid)
			}
			if ev.CreatedAt.IsZero() {
				return fmt.Errorf("%w: claim event missing created_at", ErrInvalid)
			}
			if n := parseWceSeq(ev.EventID); n > maxSeq {
				maxSeq = n
			}
		}
		s.events = append(s.events, doc.Events...)
	}
	// Seed sequence from high-water mark of claim/event IDs — never recycle wcl_N.
	if maxSeq > s.seq {
		s.seq = maxSeq
	}
	return nil
}

func parseWclSeq(id string) int64 {
	const p = "wcl_"
	if !strings.HasPrefix(id, p) {
		return 0
	}
	var n int64
	for _, r := range id[len(p):] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

func parseWceSeq(id string) int64 {
	const p = "wce_"
	if !strings.HasPrefix(id, p) {
		return 0
	}
	var n int64
	for _, r := range id[len(p):] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

// snapshotLocked captures mutable store state for rollback on save failure.
type storeSnap struct {
	byAttempt  map[string]*Claim
	liveByItem map[string]string
	byID       map[string]*Claim
	events     []Event
	seq        int64
	genByItem  map[string]int64
}

func (s *Store) snapshotLocked() storeSnap {
	snap := storeSnap{
		byAttempt:  map[string]*Claim{},
		liveByItem: map[string]string{},
		byID:       map[string]*Claim{},
		genByItem:  map[string]int64{},
		seq:        s.seq,
	}
	// Clone each claim once and index by both maps so restore keeps identity.
	byPtr := map[*Claim]*Claim{}
	for _, v := range s.byID {
		if v == nil {
			continue
		}
		if _, ok := byPtr[v]; ok {
			continue
		}
		cp := *v
		byPtr[v] = &cp
	}
	for k, v := range s.byID {
		if v != nil {
			snap.byID[k] = byPtr[v]
		}
	}
	for k, v := range s.byAttempt {
		if v != nil {
			snap.byAttempt[k] = byPtr[v]
		}
	}
	for k, v := range s.liveByItem {
		snap.liveByItem[k] = v
	}
	for k, v := range s.genByItem {
		snap.genByItem[k] = v
	}
	if len(s.events) > 0 {
		snap.events = append([]Event(nil), s.events...)
	}
	return snap
}

func (s *Store) restoreLocked(snap storeSnap) {
	s.byAttempt = snap.byAttempt
	s.byID = snap.byID
	s.liveByItem = snap.liveByItem
	s.genByItem = snap.genByItem
	s.events = snap.events
	s.seq = snap.seq
}

// commitLocked persists or rolls back to snap on failure.
func (s *Store) commitLocked(snap storeSnap) error {
	if err := s.saveLocked(); err != nil {
		s.restoreLocked(snap)
		return err
	}
	return nil
}

func (s *Store) saveLocked() error {
	if s == nil || s.path == "" {
		return nil
	}
	if s.TestFailSave != nil {
		return s.TestFailSave
	}
	doc := fileDoc{
		Schema:     SchemaFile,
		SavedAt:    s.now().UTC(),
		Seq:        s.seq,
		GenByItem:  map[string]int64{},
		LiveByItem: map[string]string{},
	}
	for k, v := range s.genByItem {
		doc.GenByItem[k] = v
	}
	for k, v := range s.liveByItem {
		doc.LiveByItem[k] = v
	}
	for _, c := range s.byID {
		if c != nil {
			doc.Claims = append(doc.Claims, *c)
		}
	}
	if len(s.events) > 0 {
		doc.Events = append(doc.Events, s.events...)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Path returns the durable store path (empty for in-memory).
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// AllClaims returns a snapshot of every claim (for reconciliation/tests).
func (s *Store) AllClaims() []Claim {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Claim, 0, len(s.byID))
	for _, c := range s.byID {
		if c != nil {
			out = append(out, *clone(c))
		}
	}
	return out
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
	// Fail closed before any mutation: explicit ExecutionPlanDigest + contract required.
	// Never synthesize from Graph.PlanDigest / EvaluateReady (that recreated the identity split).
	planDigest := strings.TrimSpace(req.PlanDigest)
	if planDigest == "" {
		return ClaimResult{}, fmt.Errorf("%w: plan_digest (execution plan digest) required; no silent ready/graph fallback", ErrInvalid)
	}
	taskClass := strings.TrimSpace(req.TaskClass)
	if taskClass == "" {
		return ClaimResult{}, fmt.Errorf("%w: task_class required on claim (no empty/default)", ErrInvalid)
	}
	ccd := strings.TrimSpace(req.ChildContractDigest)
	if ccd == "" {
		return ClaimResult{}, fmt.Errorf("%w: child_contract_digest required on claim (no post-exec synthesis)", ErrInvalid)
	}
	// GraphDigest is canonical workgraph.DigestGraph — compute and verify; never
	// blindly store empty or inconsistent req.Graph.PlanDigest.
	graphDigest := workgraph.DigestGraph(req.Graph)
	if graphDigest == "" {
		return ClaimResult{}, fmt.Errorf("%w: graph_digest empty after DigestGraph", ErrInvalid)
	}
	if stored := strings.TrimSpace(req.Graph.PlanDigest); stored != "" && stored != graphDigest {
		return ClaimResult{}, fmt.Errorf("%w: graph plan_digest inconsistent with DigestGraph (stored=%q computed=%q)", ErrInvalid, stored, graphDigest)
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
					snap := s.snapshotLocked()
					prev.State = StateReleased
					s.clearLiveLocked(logical, prev.ClaimID)
					s.appendEventLocked(prev.ClaimID, "released_non_launch", nil, now)
					if err := s.commitLocked(snap); err != nil {
						return ClaimResult{}, fmt.Errorf("%w: persist release: %v", ErrInvalid, err)
					}
					return ClaimResult{Code: ResultTerminalReused, Reason: "released_same_attempt_immutable:" + prev.AttemptID, Claim: clone(prev)}, nil
				}
				snap := s.snapshotLocked()
				prev.State = StateAmbiguous
				s.clearLiveLocked(logical, prev.ClaimID)
				s.appendEventLocked(prev.ClaimID, "ambiguous_expired", nil, now)
				if err := s.commitLocked(snap); err != nil {
					return ClaimResult{}, fmt.Errorf("%w: persist ambiguous: %v", ErrInvalid, err)
				}
				return ClaimResult{Code: ResultNeedsHuman, Reason: "expired_live_ambiguous", Claim: clone(prev)}, nil
			}
			return ClaimResult{Code: ResultAlreadyRunning, Reason: "owned_by_" + prev.ClaimID, Claim: clone(prev)}, nil
		case StateAmbiguous:
			return ClaimResult{Code: ResultNeedsHuman, Reason: "ambiguous_needs_human", Claim: clone(prev)}, nil
		case StateClosed:
			return ClaimResult{Code: ResultTerminalReused, Reason: "closed:" + string(prev.Terminal), Claim: clone(prev)}, nil
		case StateReleased, StateExpired:
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
						snap := s.snapshotLocked()
						live.State = StateReleased
						s.clearLiveLocked(logical, live.ClaimID)
						s.appendEventLocked(live.ClaimID, "released_non_launch", nil, now)
						if err := s.commitLocked(snap); err != nil {
							return ClaimResult{}, fmt.Errorf("%w: persist release: %v", ErrInvalid, err)
						}
						// continue — new AttemptID may claim
					} else {
						snap := s.snapshotLocked()
						live.State = StateAmbiguous
						s.clearLiveLocked(logical, live.ClaimID)
						s.appendEventLocked(live.ClaimID, "ambiguous_expired", nil, now)
						if err := s.commitLocked(snap); err != nil {
							return ClaimResult{}, fmt.Errorf("%w: persist ambiguous: %v", ErrInvalid, err)
						}
						return ClaimResult{Code: ResultNeedsHuman, Reason: "expired_live_ambiguous", Claim: clone(live)}, nil
					}
				} else {
					return ClaimResult{Code: ResultAlreadyRunning, Reason: "owned_by_" + live.ClaimID, Claim: clone(live)}, nil
				}
			case StateAmbiguous:
				return ClaimResult{Code: ResultNeedsHuman, Reason: "ambiguous_needs_human", Claim: clone(live)}, nil
			default:
				// Stale live pointer — clear and persist.
				snap := s.snapshotLocked()
				s.clearLiveLocked(logical, liveID)
				if err := s.commitLocked(snap); err != nil {
					return ClaimResult{}, fmt.Errorf("%w: persist clear live: %v", ErrInvalid, err)
				}
			}
		} else {
			snap := s.snapshotLocked()
			delete(s.liveByItem, logical)
			if err := s.commitLocked(snap); err != nil {
				return ClaimResult{}, fmt.Errorf("%w: persist clear live: %v", ErrInvalid, err)
			}
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

	snap := s.snapshotLocked()
	s.seq++
	s.genByItem[logical]++
	gen := s.genByItem[logical]
	id := fmt.Sprintf("wcl_%d", s.seq)
	c := &Claim{
		Schema: SchemaClaim, ClaimID: id,
		ProjectID: req.ProjectID, GraphID: req.Graph.GraphID, GraphVersion: req.Graph.Version,
		PlanDigest:          planDigest,
		GraphDigest:         graphDigest,
		TaskClass:           taskClass,
		ChildContractDigest: ccd,
		WorkItemID:          req.WorkItemID,
		AttemptID:           req.AttemptID, ExecutorID: req.ExecutorID,
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
		s.appendEventLocked(id, "claimed_superseding", map[string]string{
			"supersedes_attempt_id": supersedes,
			"attempt_id":            req.AttemptID,
		}, now)
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
	if err := s.commitLocked(snap); err != nil {
		return ClaimResult{}, fmt.Errorf("%w: persist claim: %v", ErrInvalid, err)
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
	snap := s.snapshotLocked()
	now := s.now().UTC()
	c.State = StateRunning
	c.RenewedAt = now
	if lease > 0 {
		c.LeaseUntil = now.Add(lease)
	}
	s.appendEventLocked(c.ClaimID, "renewed", nil, now)
	if err := s.commitLocked(snap); err != nil {
		return ClaimResult{}, fmt.Errorf("%w: persist renew: %v", ErrInvalid, err)
	}
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
	snap := s.snapshotLocked()
	now := s.now().UTC()
	c.State = StateClosed
	c.Terminal = req.Terminal
	c.OutputEvidence = req.OutputEvidence
	c.ClosedAt = now
	logical := claimLogicalKey(c.ProjectID, c.GraphID, c.GraphVersion, c.WorkItemID)
	s.clearLiveLocked(logical, c.ClaimID)
	s.appendEventLocked(c.ClaimID, "closed_"+string(req.Terminal), nil, now)
	if err := s.commitLocked(snap); err != nil {
		return ClaimResult{}, fmt.Errorf("%w: persist close: %v", ErrInvalid, err)
	}
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
