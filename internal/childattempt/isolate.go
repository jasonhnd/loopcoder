package childattempt

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/routedecision"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const (
	SchemaChild   = "loopcoder.child_attempt.v1"
	SchemaParent  = "loopcoder.parent_workflow.v1"
	PolicyVersion = "child-attempt-v1"
)

// Child is one routed child attempt for a claimed WorkItem.
type Child struct {
	Schema          string `json:"schema"`
	ChildAttemptID  string `json:"child_attempt_id"`
	ParentWorkflow  string `json:"parent_workflow_id"`
	WorkItemID      string `json:"work_item_id"`
	ClaimID         string `json:"claim_id"`
	ClaimGeneration int64  `json:"claim_generation"`
	// RouteDigest from routedecision winner.
	RouteDigest string `json:"route_digest"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	// WorktreeKey exclusive writable checkout (or empty if read-only/no-code).
	WorktreeKey string `json:"worktree_key,omitempty"`
	ReadOnly    bool   `json:"read_only"`
	// CredentialScope redacted scope id (never raw secrets).
	CredentialScope string `json:"credential_scope"`
	// BoundedInputs declared only.
	BoundedInputs []string                `json:"bounded_inputs,omitempty"`
	Terminal      workgraph.TerminalState `json:"terminal,omitempty"`
	// PrivateOutput not exposed to siblings by default.
	PrivateOutput string `json:"-"`
	// PublicSummary optional redacted status for parent aggregate.
	PublicSummary string    `json:"public_summary,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// ParentView is aggregate progress (no child terminal rewrite).
type ParentView struct {
	Schema     string `json:"schema"`
	WorkflowID string `json:"workflow_id"`
	Children   int    `json:"children"`
	Succeeded  int    `json:"succeeded"`
	Failed     int    `json:"failed"`
	Running    int    `json:"running"`
	// DeclaredSuccess only when all required children closed successfully.
	DeclaredSuccess bool              `json:"declared_success"`
	RequiredFailed  []string          `json:"required_failed,omitempty"`
	ByWorkItem      map[string]string `json:"by_work_item"` // id → terminal or running
}

// Registry tracks children and enforces isolation.
type Registry struct {
	mu         sync.Mutex
	children   map[string]*Child // child attempt id
	byWorkItem map[string]string // workitem → child attempt
	worktrees  map[string]string // worktree → child attempt
	credScopes map[string]string // scope → child (exclusive)
	seq        int64
	now        func() time.Time
}

// NewRegistry creates an empty registry.
func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		children: map[string]*Child{}, byWorkItem: map[string]string{},
		worktrees: map[string]string{}, credScopes: map[string]string{}, now: now,
	}
}

var (
	ErrInvalid   = errors.New("childattempt: invalid")
	ErrConflict  = errors.New("childattempt: conflict")
	ErrIsolation = errors.New("childattempt: isolation")
)

// SpawnRequest creates a child from claim + route decision.
type SpawnRequest struct {
	ParentWorkflow string
	Claim          workclaim.Claim
	Decision       routedecision.Decision
	// WorktreeKey required unless ReadOnly.
	WorktreeKey     string
	ReadOnly        bool
	CredentialScope string
	BoundedInputs   []string
}

// Spawn creates a child attempt; fails if isolation would break.
func (r *Registry) Spawn(req SpawnRequest) (Child, error) {
	if r == nil {
		return Child{}, fmt.Errorf("%w: nil", ErrInvalid)
	}
	if req.Claim.ClaimID == "" || req.Claim.WorkItemID == "" {
		return Child{}, fmt.Errorf("%w: claim required", ErrInvalid)
	}
	if req.Decision.Winner == nil || req.Decision.Digest == "" {
		return Child{}, fmt.Errorf("%w: route decision winner required", ErrInvalid)
	}
	if !req.ReadOnly && strings.TrimSpace(req.WorktreeKey) == "" {
		return Child{}, fmt.Errorf("%w: worktree required for writable child", ErrInvalid)
	}
	if strings.TrimSpace(req.CredentialScope) == "" {
		req.CredentialScope = "cred:" + req.Claim.WorkItemID
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if prev, ok := r.byWorkItem[req.Claim.WorkItemID]; ok {
		return Child{}, fmt.Errorf("%w: workitem already has child %s", ErrConflict, prev)
	}
	if !req.ReadOnly {
		if owner, ok := r.worktrees[req.WorktreeKey]; ok {
			return Child{}, fmt.Errorf("%w: worktree %s owned by %s", ErrIsolation, req.WorktreeKey, owner)
		}
	}
	if owner, ok := r.credScopes[req.CredentialScope]; ok {
		return Child{}, fmt.Errorf("%w: credentials shared with %s", ErrIsolation, owner)
	}

	r.seq++
	id := fmt.Sprintf("catt_%d", r.seq)
	c := &Child{
		Schema: SchemaChild, ChildAttemptID: id, ParentWorkflow: req.ParentWorkflow,
		WorkItemID: req.Claim.WorkItemID, ClaimID: req.Claim.ClaimID,
		ClaimGeneration: req.Claim.Generation,
		RouteDigest:     req.Decision.Digest,
		Provider:        req.Decision.Winner.Provider, Model: req.Decision.Winner.Model,
		WorktreeKey: req.WorktreeKey, ReadOnly: req.ReadOnly,
		CredentialScope: req.CredentialScope,
		BoundedInputs:   append([]string{}, req.BoundedInputs...),
		CreatedAt:       r.now().UTC(),
	}
	r.children[id] = c
	r.byWorkItem[c.WorkItemID] = id
	if !c.ReadOnly {
		r.worktrees[c.WorktreeKey] = id
	}
	r.credScopes[c.CredentialScope] = id
	return *c, nil
}

// SetPrivateOutput stores private sibling-hidden output.
func (r *Registry) SetPrivateOutput(childID, out string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.children[childID]
	if !ok {
		return fmt.Errorf("%w: not found", ErrInvalid)
	}
	c.PrivateOutput = out
	return nil
}

// SiblingPrivateOutput returns empty by default (isolation).
func (r *Registry) SiblingPrivateOutput(requesterChildID, targetChildID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if requesterChildID == targetChildID {
		if c, ok := r.children[requesterChildID]; ok {
			return c.PrivateOutput, true
		}
	}
	// default deny sibling access
	return "", false
}

// CloseChild sets terminal; parent cannot rewrite later.
func (r *Registry) CloseChild(childID string, term workgraph.TerminalState, publicSummary string) error {
	if term == workgraph.TermNone {
		return fmt.Errorf("%w: terminal", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.children[childID]
	if !ok {
		return fmt.Errorf("%w: not found", ErrInvalid)
	}
	if c.Terminal != workgraph.TermNone {
		if c.Terminal == term {
			return nil // idempotent
		}
		return fmt.Errorf("%w: terminal immutable", ErrConflict)
	}
	c.Terminal = term
	c.PublicSummary = publicSummary
	// release worktree on close
	if !c.ReadOnly && c.WorktreeKey != "" {
		delete(r.worktrees, c.WorktreeKey)
	}
	return nil
}

// ParentRewriteTerminal is always rejected.
func (r *Registry) ParentRewriteTerminal(childID string, term workgraph.TerminalState) error {
	return fmt.Errorf("%w: parent cannot rewrite child terminal", ErrIsolation)
}

// Aggregate builds parent view without declaring success early.
func (r *Registry) Aggregate(workflowID string, required []string) ParentView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := ParentView{
		Schema: SchemaParent, WorkflowID: workflowID,
		ByWorkItem: map[string]string{},
	}
	reqSet := map[string]bool{}
	for _, id := range required {
		reqSet[id] = true
	}
	for _, c := range r.children {
		if c.ParentWorkflow != workflowID {
			continue
		}
		v.Children++
		switch c.Terminal {
		case workgraph.TermSucceeded:
			v.Succeeded++
			v.ByWorkItem[c.WorkItemID] = string(c.Terminal)
		case workgraph.TermFailed, workgraph.TermCancelled:
			v.Failed++
			v.ByWorkItem[c.WorkItemID] = string(c.Terminal)
			if reqSet[c.WorkItemID] {
				v.RequiredFailed = append(v.RequiredFailed, c.WorkItemID)
			}
		default:
			v.Running++
			v.ByWorkItem[c.WorkItemID] = "running"
		}
	}
	// success only when every required item succeeded
	if len(required) > 0 && len(v.RequiredFailed) == 0 && v.Running == 0 {
		all := true
		for _, id := range required {
			if v.ByWorkItem[id] != string(workgraph.TermSucceeded) {
				all = false
				break
			}
		}
		v.DeclaredSuccess = all
	}
	return v
}

// MutateSiblingRoute is rejected.
func (r *Registry) MutateSiblingRoute(childID, newProvider string) error {
	return fmt.Errorf("%w: cannot mutate sibling/parent routes from child", ErrIsolation)
}
