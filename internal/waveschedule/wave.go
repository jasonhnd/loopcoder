package waveschedule

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const (
	SchemaWave      = "loopcoder.wave.plan.v1"
	SchemaCandidate = "loopcoder.wave.completion_candidate.v1"
	SchemaSnapshot  = "loopcoder.wave.scheduler_snapshot.v1"
	PolicyVersion   = "wave-schedule-v1"
	DefaultWIP      = 1
)

// ResourceBounds limits concurrent work.
type ResourceBounds struct {
	// MaxActiveWorkers default 1 (serial top-level).
	MaxActiveWorkers int `json:"max_active_workers"`
	// MachineSlots remaining admitted process slots.
	MachineSlots int `json:"machine_slots"`
	// ProviderSlots remaining provider concurrency.
	ProviderSlots int `json:"provider_slots"`
	// WorktreeAvailable false blocks any new wave member.
	WorktreeAvailable bool `json:"worktree_available"`
}

// DefaultBounds returns serial default.
func DefaultBounds() ResourceBounds {
	return ResourceBounds{
		MaxActiveWorkers: DefaultWIP, MachineSlots: 8, ProviderSlots: 4, WorktreeAvailable: true,
	}
}

// Snapshot freezes graph readiness + bounds for pure planning.
type Snapshot struct {
	Graph    workgraph.Graph            `json:"graph"`
	Evidence workgraph.TerminalEvidence `json:"evidence"`
	// ActiveWorkItemIDs already claimed/running (count against WIP).
	ActiveWorkItemIDs []string `json:"active_work_item_ids"`
	// AssignedWorktrees maps workitem → worktree path key (one writer per tree).
	AssignedWorktrees map[string]string `json:"assigned_worktrees,omitempty"`
	Bounds            ResourceBounds    `json:"bounds"`
	// WaveSeq monotonic wave number for this graph version.
	WaveSeq int `json:"wave_seq"`
}

// WaveMember is one planned claim target.
type WaveMember struct {
	WorkItemID       string `json:"work_item_id"`
	IntegrationOrder int    `json:"integration_order"`
	// WorktreeKey reserved exclusive tree (synthetic if not pre-assigned).
	WorktreeKey string   `json:"worktree_key"`
	Reasons     []string `json:"reasons"`
}

// WavePlan is a persisted plan before claims.
type WavePlan struct {
	Schema        string       `json:"schema"`
	PolicyVersion string       `json:"policy_version"`
	GraphID       string       `json:"graph_id"`
	GraphVersion  int          `json:"graph_version"`
	PlanDigest    string       `json:"plan_digest"` // graph plan digest
	WaveSeq       int          `json:"wave_seq"`
	Members       []WaveMember `json:"members"`
	// EmptyReason when no members (blocked / no-ready).
	EmptyReason string    `json:"empty_reason,omitempty"`
	Reasons     []string  `json:"reasons"`
	Digest      string    `json:"digest"`
	CreatedAt   time.Time `json:"created_at"`
}

// CompletionCandidate is immutable finish evidence for V090-100 integration.
// Scheduler does NOT close WorkItems or mutate parent branch.
type CompletionCandidate struct {
	Schema         string                  `json:"schema"`
	WorkItemID     string                  `json:"work_item_id"`
	WaveSeq        int                     `json:"wave_seq"`
	AttemptID      string                  `json:"attempt_id"`
	Terminal       workgraph.TerminalState `json:"terminal"`
	OutputEvidence string                  `json:"output_evidence,omitempty"`
	FinishedAt     time.Time               `json:"finished_at"`
	// IntegrationOrder for ordered emission despite out-of-order finish.
	IntegrationOrder int    `json:"integration_order"`
	Digest           string `json:"digest"`
}

// Store persists waves and candidates (in-memory).
type Store struct {
	mu          sync.Mutex
	plans       map[string]*WavePlan // graphID|version|seq
	activeWave  map[string]string    // graph key → plan digest of current wave
	candidates  []CompletionCandidate
	worktreeOwn map[string]string // worktree → workitem
	now         func() time.Time
}

// NewStore creates a wave store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		plans: map[string]*WavePlan{}, activeWave: map[string]string{},
		worktreeOwn: map[string]string{}, now: now,
	}
}

var (
	ErrInvalid  = errors.New("waveschedule: invalid")
	ErrLimit    = errors.New("waveschedule: limit")
	ErrConflict = errors.New("waveschedule: conflict")
)

// PlanWave is pure: same snapshot → identical members, order, reasons, digest.
func PlanWave(snap Snapshot) (WavePlan, error) {
	if snap.Bounds.MaxActiveWorkers <= 0 {
		snap.Bounds.MaxActiveWorkers = DefaultWIP
	}
	ready := workgraph.EvaluateReady(snap.Graph, snap.Evidence)
	p := WavePlan{
		Schema: SchemaWave, PolicyVersion: PolicyVersion,
		GraphID: snap.Graph.GraphID, GraphVersion: snap.Graph.Version,
		PlanDigest: ready.PlanDigest, WaveSeq: snap.WaveSeq,
		Members: []WaveMember{}, Reasons: []string{},
	}
	if !ready.Valid {
		p.EmptyReason = "invalid_graph"
		p.Reasons = append(p.Reasons, ready.Errors...)
		p.Digest = digestPlan(p)
		return p, nil
	}
	if len(ready.Ready) == 0 {
		p.EmptyReason = "no_ready"
		p.Reasons = append(p.Reasons, "blocked_or_complete")
		p.Digest = digestPlan(p)
		return p, nil
	}
	if !snap.Bounds.WorktreeAvailable {
		p.EmptyReason = "no_worktree"
		p.Reasons = append(p.Reasons, "worktree_unavailable")
		p.Digest = digestPlan(p)
		return p, nil
	}

	active := map[string]struct{}{}
	for _, id := range snap.ActiveWorkItemIDs {
		active[id] = struct{}{}
	}
	// Capacity remaining
	slots := snap.Bounds.MaxActiveWorkers - len(active)
	if snap.Bounds.MachineSlots < slots {
		slots = snap.Bounds.MachineSlots
	}
	if snap.Bounds.ProviderSlots < slots {
		slots = snap.Bounds.ProviderSlots
	}
	if slots <= 0 {
		p.EmptyReason = "wip_full"
		p.Reasons = append(p.Reasons, "active_workers_at_limit")
		p.Digest = digestPlan(p)
		return p, nil
	}

	// order map
	order := map[string]int{}
	for _, it := range snap.Graph.Items {
		order[it.ID] = it.IntegrationOrder
	}
	// ready already ordered by EvaluateReady
	usedTrees := map[string]string{}
	if snap.AssignedWorktrees != nil {
		for wi, wt := range snap.AssignedWorktrees {
			usedTrees[wt] = wi
		}
	}

	for _, id := range ready.Ready {
		if len(p.Members) >= slots {
			p.Reasons = append(p.Reasons, "capacity_reached")
			break
		}
		if _, ok := active[id]; ok {
			continue
		}
		wt := "wt:" + id
		if snap.AssignedWorktrees != nil {
			if pre, ok := snap.AssignedWorktrees[id]; ok && pre != "" {
				wt = pre
			}
		}
		// never two writers one worktree
		if owner, ok := usedTrees[wt]; ok && owner != id {
			p.Reasons = append(p.Reasons, "worktree_busy:"+wt)
			continue
		}
		usedTrees[wt] = id
		p.Members = append(p.Members, WaveMember{
			WorkItemID: id, IntegrationOrder: order[id], WorktreeKey: wt,
			Reasons: []string{"ready", "admitted"},
		})
	}
	if len(p.Members) == 0 && p.EmptyReason == "" {
		p.EmptyReason = "no_admissible_ready"
	}
	p.Reasons = append(p.Reasons, fmt.Sprintf("members=%d", len(p.Members)))
	p.Digest = digestPlan(p)
	return p, nil
}

// PersistPlan stores a wave plan; rejects silent membership change on restart
// if same wave_seq already exists with different digest.
func (s *Store) PersistPlan(p WavePlan) (WavePlan, error) {
	if p.Digest == "" {
		p.Digest = digestPlan(p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = s.now().UTC()
	}
	key := planKey(p.GraphID, p.GraphVersion, p.WaveSeq)
	if prev, ok := s.plans[key]; ok {
		if prev.Digest != p.Digest {
			return WavePlan{}, fmt.Errorf("%w: wave membership changed for seq %d", ErrConflict, p.WaveSeq)
		}
		return *prev, nil // idempotent resume
	}
	// reserve worktrees
	for _, m := range p.Members {
		if owner, ok := s.worktreeOwn[m.WorktreeKey]; ok && owner != m.WorkItemID {
			return WavePlan{}, fmt.Errorf("%w: worktree %s owned by %s", ErrLimit, m.WorktreeKey, owner)
		}
		s.worktreeOwn[m.WorktreeKey] = m.WorkItemID
	}
	cp := p
	s.plans[key] = &cp
	s.activeWave[graphKey(p.GraphID, p.GraphVersion)] = p.Digest
	return cp, nil
}

// GetPlan loads a persisted wave.
func (s *Store) GetPlan(graphID string, version, seq int) (WavePlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[planKey(graphID, version, seq)]
	if !ok {
		return WavePlan{}, fmt.Errorf("%w: not found", ErrInvalid)
	}
	return *p, nil
}

// Complete records an immutable completion candidate (no WorkItem close).
func (s *Store) Complete(graphID string, version, waveSeq int, workItemID, attemptID string, term workgraph.TerminalState, evidence string, integrationOrder int) (CompletionCandidate, error) {
	if term == workgraph.TermNone {
		return CompletionCandidate{}, fmt.Errorf("%w: terminal", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	c := CompletionCandidate{
		Schema: SchemaCandidate, WorkItemID: workItemID, WaveSeq: waveSeq,
		AttemptID: attemptID, Terminal: term, OutputEvidence: evidence,
		FinishedAt: now, IntegrationOrder: integrationOrder,
	}
	c.Digest = digestCandidate(c)
	s.candidates = append(s.candidates, c)
	// release worktree for this item
	for wt, wi := range s.worktreeOwn {
		if wi == workItemID {
			delete(s.worktreeOwn, wt)
		}
	}
	return c, nil
}

// IntegrationCandidates returns completion candidates sorted by integration order
// then finish time — for V090-100 consumption only.
func (s *Store) IntegrationCandidates() []CompletionCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]CompletionCandidate{}, s.candidates...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IntegrationOrder != out[j].IntegrationOrder {
			return out[i].IntegrationOrder < out[j].IntegrationOrder
		}
		return out[i].FinishedAt.Before(out[j].FinishedAt)
	})
	return out
}

func planKey(graphID string, version, seq int) string {
	return fmt.Sprintf("%s|%d|%d", graphID, version, seq)
}
func graphKey(graphID string, version int) string {
	return fmt.Sprintf("%s|%d", graphID, version)
}

func digestPlan(p WavePlan) string {
	type wire struct {
		Schema        string       `json:"schema"`
		PolicyVersion string       `json:"policy_version"`
		GraphID       string       `json:"graph_id"`
		GraphVersion  int          `json:"graph_version"`
		PlanDigest    string       `json:"plan_digest"`
		WaveSeq       int          `json:"wave_seq"`
		Members       []WaveMember `json:"members"`
		EmptyReason   string       `json:"empty_reason,omitempty"`
		Reasons       []string     `json:"reasons"`
	}
	w := wire{
		Schema: p.Schema, PolicyVersion: p.PolicyVersion,
		GraphID: p.GraphID, GraphVersion: p.GraphVersion, PlanDigest: p.PlanDigest,
		WaveSeq: p.WaveSeq, Members: p.Members, EmptyReason: p.EmptyReason, Reasons: p.Reasons,
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

func digestCandidate(c CompletionCandidate) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%s|%s", c.WorkItemID, c.WaveSeq, c.AttemptID, c.Terminal, c.OutputEvidence)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// ExplainEmpty returns a human explanation for wait state.
func ExplainEmpty(p WavePlan) string {
	if len(p.Members) > 0 {
		return fmt.Sprintf("wave %d has %d members", p.WaveSeq, len(p.Members))
	}
	return fmt.Sprintf("wave %d waiting: %s; reasons=%s; provider_calls=0",
		p.WaveSeq, p.EmptyReason, strings.Join(p.Reasons, ","))
}
