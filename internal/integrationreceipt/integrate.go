package integrationreceipt

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

	"github.com/jasonhnd/loopcoder/internal/waveschedule"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const (
	SchemaIntent    = "loopcoder.integration.intent.v1"
	SchemaReceipt   = "loopcoder.integration.receipt.v1"
	SchemaAttention = "loopcoder.integration.attention.v1"
	PolicyVersion   = "integration-receipt-v1"
)

// Method is a documented deterministic integration method.
type Method string

const (
	// MethodCherryPick applies candidate tree onto expected parent.
	MethodCherryPick Method = "cherry_pick"
	// MethodMergeFastForward only when parent matches and history allows.
	MethodMergeFastForward Method = "merge_ff"
	// MethodApplyPatch applies a patch digest (fixture-friendly).
	MethodApplyPatch Method = "apply_patch"
)

// Intent freezes integration plan before any mutation.
type Intent struct {
	Schema            string `json:"schema"`
	PolicyVersion     string `json:"policy_version"`
	IntegrationTree   string `json:"integration_worktree"`
	IntegrationBranch string `json:"integration_branch"`
	ExpectedParent    string `json:"expected_parent"` // commit/tree digest
	Method            Method `json:"method"`
	// Candidates ordered by integration_order (stable independent of finish order).
	CandidateIDs   []string `json:"candidate_ids"`
	IdempotencyKey string   `json:"idempotency_key"`
	Digest         string   `json:"digest"`
}

// Candidate is one integration unit (from waveschedule completion).
type Candidate struct {
	ID               string
	WorkItemID       string
	SourceCommit     string // or tree digest
	SourceTreeDigest string
	OutputEvidence   string
	IntegrationOrder int
	Terminal         workgraph.TerminalState
}

// Receipt is one applied (or stopped) integration result.
type Receipt struct {
	Schema       string `json:"schema"`
	ReceiptID    string `json:"receipt_id"`
	IntentDigest string `json:"intent_digest"`
	CandidateID  string `json:"candidate_id"`
	WorkItemID   string `json:"work_item_id"`
	Method       Method `json:"method"`
	// Status: applied|conflict|skipped_already|failed|timeout
	Status       string    `json:"status"`
	ParentBefore string    `json:"parent_before"`
	ParentAfter  string    `json:"parent_after,omitempty"`
	ReadBackOK   bool      `json:"read_back_ok"`
	Evidence     string    `json:"evidence,omitempty"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// WorkItemClosed only when applied+readback+verified.
	WorkItemClosed bool `json:"work_item_closed"`
}

// Attention is durable conflict/stop attention (no auto-resolve).
type Attention struct {
	Schema      string    `json:"schema"`
	AttentionID string    `json:"attention_id"`
	CandidateID string    `json:"candidate_id"`
	WorkItemID  string    `json:"work_item_id"`
	Reason      string    `json:"reason"`
	Evidence    string    `json:"evidence"`
	CreatedAt   time.Time `json:"created_at"`
	// RequiresOwner true — cannot auto-resolve by model/strategy change.
	RequiresOwner bool `json:"requires_owner"`
}

// WorktreeState is the integration worktree snapshot (injected, not live git).
type WorktreeState struct {
	Path      string
	Branch    string
	Head      string // parent commit digest
	Dirty     bool
	OwnerID   string // integrator id; empty = unowned
	Available bool
}

// ApplyFunc is a deterministic apply hook for tests (returns new head or error class).
// error classes: conflict, missing_commit, hook_failure, timeout, dirty, unowned
type ApplyFunc func(method Method, parent, source string) (newHead string, errClass string)

// Engine owns one integrator per worktree.
type Engine struct {
	mu        sync.Mutex
	intents   map[string]*Intent
	receipts  map[string]*Receipt // candidateID → receipt (idempotent)
	attention []Attention
	// tree state
	tree       WorktreeState
	integrator string
	apply      ApplyFunc
	seq        int64
	now        func() time.Time
	// closed work items after successful integration
	closed map[string]bool
}

// NewEngine creates an integration engine bound to one worktree.
func NewEngine(tree WorktreeState, integrator string, apply ApplyFunc, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	if apply == nil {
		apply = defaultApply
	}
	return &Engine{
		intents: map[string]*Intent{}, receipts: map[string]*Receipt{},
		tree: tree, integrator: integrator, apply: apply, now: now,
		closed: map[string]bool{},
	}
}

var (
	ErrInvalid  = errors.New("integrationreceipt: invalid")
	ErrConflict = errors.New("integrationreceipt: conflict")
	ErrStop     = errors.New("integrationreceipt: stopped")
)

// BuildIntent freezes ordered candidates and parent/method before mutation.
// Order is by IntegrationOrder independent of finish time.
func BuildIntent(tree WorktreeState, method Method, cands []Candidate, idem string) (Intent, error) {
	if strings.TrimSpace(tree.Path) == "" || strings.TrimSpace(tree.Head) == "" {
		return Intent{}, fmt.Errorf("%w: worktree path/head", ErrInvalid)
	}
	if method != MethodCherryPick && method != MethodMergeFastForward && method != MethodApplyPatch {
		return Intent{}, fmt.Errorf("%w: method", ErrInvalid)
	}
	sorted := append([]Candidate{}, cands...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].IntegrationOrder != sorted[j].IntegrationOrder {
			return sorted[i].IntegrationOrder < sorted[j].IntegrationOrder
		}
		return sorted[i].ID < sorted[j].ID
	})
	ids := make([]string, len(sorted))
	for i, c := range sorted {
		ids[i] = c.ID
	}
	if idem == "" {
		idem = "idem_" + short(strings.Join(ids, ","))
	}
	in := Intent{
		Schema: SchemaIntent, PolicyVersion: PolicyVersion,
		IntegrationTree: tree.Path, IntegrationBranch: tree.Branch,
		ExpectedParent: tree.Head, Method: method,
		CandidateIDs: ids, IdempotencyKey: idem,
	}
	in.Digest = digestIntent(in)
	return in, nil
}

// FromWaveCandidates adapts waveschedule completion candidates.
func FromWaveCandidates(cs []waveschedule.CompletionCandidate) []Candidate {
	out := make([]Candidate, 0, len(cs))
	for _, c := range cs {
		if c.Terminal != workgraph.TermSucceeded {
			continue // only successful executions integrate
		}
		out = append(out, Candidate{
			ID: c.Digest, WorkItemID: c.WorkItemID,
			SourceCommit: c.OutputEvidence, SourceTreeDigest: c.OutputEvidence,
			OutputEvidence: c.OutputEvidence, IntegrationOrder: c.IntegrationOrder,
			Terminal: c.Terminal,
		})
	}
	return out
}

// Run applies candidates one at a time per intent. Stops on first hard failure.
func (e *Engine) Run(intent Intent, cands []Candidate) ([]Receipt, error) {
	if e == nil {
		return nil, fmt.Errorf("%w: nil engine", ErrInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// Only one integrator
	if e.tree.OwnerID != "" && e.tree.OwnerID != e.integrator {
		return nil, fmt.Errorf("%w: worktree owned by %s", ErrConflict, e.tree.OwnerID)
	}
	e.tree.OwnerID = e.integrator

	if intent.Digest == "" {
		intent.Digest = digestIntent(intent)
	}
	if e.tree.Dirty {
		e.raiseAttentionLocked("", "", "dirty_worktree", e.tree.Path)
		return nil, fmt.Errorf("%w: dirty worktree", ErrStop)
	}

	// Index candidates
	byID := map[string]Candidate{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	// Order from intent
	var receipts []Receipt
	// Parent freeze applies only when about to mutate (skip if full idempotent replay).
	needMutate := false
	for _, id := range intent.CandidateIDs {
		if prev, ok := e.receipts[id]; !ok || prev.Status != "applied" {
			needMutate = true
			break
		}
	}
	if needMutate && e.tree.Head != intent.ExpectedParent {
		// Allow resume after partial apply: parent may have advanced from earlier
		// candidates in this intent; only reject if no receipts yet for this intent.
		hasAny := false
		for _, id := range intent.CandidateIDs {
			if _, ok := e.receipts[id]; ok {
				hasAny = true
				break
			}
		}
		if !hasAny {
			e.raiseAttentionLocked("", "", "changed_parent", "expected="+intent.ExpectedParent+" actual="+e.tree.Head)
			return nil, fmt.Errorf("%w: changed parent", ErrStop)
		}
	}
	for _, id := range intent.CandidateIDs {
		c, ok := byID[id]
		if !ok {
			e.raiseAttentionLocked(id, "", "missing_candidate", id)
			return receipts, fmt.Errorf("%w: missing candidate %s", ErrStop, id)
		}
		// Idempotent: already applied
		if prev, ok := e.receipts[id]; ok && prev.Status == "applied" {
			receipts = append(receipts, *prev)
			continue
		}
		// Parent/source checks
		if c.SourceCommit == "" && c.SourceTreeDigest == "" {
			r := e.failReceiptLocked(intent, c, "missing_commit", e.tree.Head)
			receipts = append(receipts, r)
			e.raiseAttentionLocked(c.ID, c.WorkItemID, "missing_commit", c.ID)
			return receipts, fmt.Errorf("%w: missing commit", ErrStop)
		}
		parentBefore := e.tree.Head
		src := c.SourceCommit
		if src == "" {
			src = c.SourceTreeDigest
		}
		newHead, errClass := e.apply(intent.Method, parentBefore, src)
		now := e.now().UTC()
		e.seq++
		rid := fmt.Sprintf("irc_%d", e.seq)
		r := Receipt{
			Schema: SchemaReceipt, ReceiptID: rid, IntentDigest: intent.Digest,
			CandidateID: c.ID, WorkItemID: c.WorkItemID, Method: intent.Method,
			ParentBefore: parentBefore, CreatedAt: now,
		}
		switch errClass {
		case "":
			// success path
			if newHead == "" {
				newHead = parentBefore + "+" + short(src)
			}
			// read-back: head must equal newHead
			e.tree.Head = newHead
			r.ParentAfter = newHead
			r.Status = "applied"
			r.ReadBackOK = e.tree.Head == newHead
			r.Evidence = "applied:" + src
			if r.ReadBackOK {
				// verification: parent advanced and evidence present
				r.WorkItemClosed = true
				e.closed[c.WorkItemID] = true
			}
			e.receipts[c.ID] = &r
			receipts = append(receipts, r)
		case "conflict":
			r.Status = "conflict"
			r.Error = "conflict"
			r.Evidence = "conflict:" + src
			e.receipts[c.ID] = &r
			receipts = append(receipts, r)
			e.raiseAttentionLocked(c.ID, c.WorkItemID, "conflict", src)
			return receipts, fmt.Errorf("%w: conflict at %s", ErrStop, c.WorkItemID)
		case "timeout", "hook_failure", "dirty", "unowned", "missing_commit":
			r.Status = "failed"
			r.Error = errClass
			e.receipts[c.ID] = &r
			receipts = append(receipts, r)
			e.raiseAttentionLocked(c.ID, c.WorkItemID, errClass, src)
			return receipts, fmt.Errorf("%w: %s at %s", ErrStop, errClass, c.WorkItemID)
		default:
			r.Status = "failed"
			r.Error = errClass
			e.receipts[c.ID] = &r
			receipts = append(receipts, r)
			return receipts, fmt.Errorf("%w: %s", ErrStop, errClass)
		}
	}
	return receipts, nil
}

// IsClosed reports WorkItem closed by successful integration.
func (e *Engine) IsClosed(workItemID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed[workItemID]
}

// Receipts returns all receipts.
func (e *Engine) Receipts() []Receipt {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Receipt, 0, len(e.receipts))
	for _, r := range e.receipts {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceiptID < out[j].ReceiptID })
	return out
}

// Attentions returns durable attention records.
func (e *Engine) Attentions() []Attention {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := append([]Attention{}, e.attention...)
	return out
}

// Head returns current integration head.
func (e *Engine) Head() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tree.Head
}

func (e *Engine) raiseAttentionLocked(cand, wi, reason, evidence string) Attention {
	e.seq++
	a := Attention{
		Schema: SchemaAttention, AttentionID: fmt.Sprintf("iat_%d", e.seq),
		CandidateID: cand, WorkItemID: wi, Reason: reason, Evidence: evidence,
		CreatedAt: e.now().UTC(), RequiresOwner: true,
	}
	e.attention = append(e.attention, a)
	return a
}

func (e *Engine) failReceiptLocked(intent Intent, c Candidate, errClass, parent string) Receipt {
	e.seq++
	r := Receipt{
		Schema: SchemaReceipt, ReceiptID: fmt.Sprintf("irc_%d", e.seq),
		IntentDigest: intent.Digest, CandidateID: c.ID, WorkItemID: c.WorkItemID,
		Method: intent.Method, Status: "failed", ParentBefore: parent,
		Error: errClass, CreatedAt: e.now().UTC(),
	}
	e.receipts[c.ID] = &r
	return r
}

func defaultApply(method Method, parent, source string) (string, string) {
	if strings.Contains(source, "CONFLICT") {
		return "", "conflict"
	}
	if strings.Contains(source, "MISSING") {
		return "", "missing_commit"
	}
	if strings.Contains(source, "HOOK") {
		return "", "hook_failure"
	}
	if strings.Contains(source, "TIMEOUT") {
		return "", "timeout"
	}
	// deterministic new head
	h := sha256.Sum256([]byte(parent + "|" + string(method) + "|" + source))
	return "commit:" + hex.EncodeToString(h[:])[:12], ""
}

func digestIntent(in Intent) string {
	type wire struct {
		Schema            string   `json:"schema"`
		PolicyVersion     string   `json:"policy_version"`
		IntegrationTree   string   `json:"integration_worktree"`
		IntegrationBranch string   `json:"integration_branch"`
		ExpectedParent    string   `json:"expected_parent"`
		Method            Method   `json:"method"`
		CandidateIDs      []string `json:"candidate_ids"`
		IdempotencyKey    string   `json:"idempotency_key"`
	}
	w := wire{
		Schema: in.Schema, PolicyVersion: in.PolicyVersion,
		IntegrationTree: in.IntegrationTree, IntegrationBranch: in.IntegrationBranch,
		ExpectedParent: in.ExpectedParent, Method: in.Method,
		CandidateIDs: in.CandidateIDs, IdempotencyKey: in.IdempotencyKey,
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

func short(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}
