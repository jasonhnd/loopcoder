package nativechild

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	SchemaPolicy  = "loopcoder.native_child.policy.v1"
	SchemaSession = "loopcoder.native_child.session.v1"
	SchemaAgg     = "loopcoder.native_child.aggregate.v1"
	SchemaEvent   = "loopcoder.native_child.event.v1"
)

// Policy is the owner-approved native-child allowance for one attempt.
type Policy string

const (
	// PolicyForbidden: provider invocation must disable native sub-agents.
	PolicyForbidden Policy = "forbidden"
	// PolicyAllowed: native children permitted under containment rules.
	PolicyAllowed Policy = "allowed"
)

// Session is one observed native child session/process (not a WorkItem).
type Session struct {
	Schema        string `json:"schema"`
	SessionID     string `json:"session_id"`
	ParentAttempt string `json:"parent_attempt_id"`
	// Provider/Model pin inherited from parent (immutable).
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// ProcessID opaque local handle.
	ProcessID string `json:"process_id,omitempty"`
	// ParentPID must be within parent attempt process tree.
	ParentPID string    `json:"parent_pid,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// Active until joined.
	Active bool `json:"active"`
	// Usage samples (synthetic units).
	CPUms    int64 `json:"cpu_ms"`
	RSSkib   int64 `json:"rss_kib"`
	OutputB  int64 `json:"output_bytes"`
	TokenEst int64 `json:"token_est"`
	// Escape true if child left observable tree.
	Escape bool `json:"escape,omitempty"`
}

// Aggregate is parent+children usage against shared budgets.
type Aggregate struct {
	Schema          string `json:"schema"`
	ParentAttemptID string `json:"parent_attempt_id"`
	ChildCount      int    `json:"child_count"`
	ActiveChildren  int    `json:"active_children"`
	TotalCPUms      int64  `json:"total_cpu_ms"`
	TotalRSSkib     int64  `json:"total_rss_kib"`
	TotalOutputB    int64  `json:"total_output_bytes"`
	TotalTokenEst   int64  `json:"total_token_est"`
	// Parent-only baseline (not multiplied per child).
	ParentCPUms    int64 `json:"parent_cpu_ms"`
	ParentRSSkib   int64 `json:"parent_rss_kib"`
	ParentOutputB  int64 `json:"parent_output_bytes"`
	ParentTokenEst int64 `json:"parent_token_est"`
	// Limit breaches
	OverBudget bool     `json:"over_budget"`
	Reasons    []string `json:"reasons,omitempty"`
}

// Budgets are parent/global ceilings (not per-child multiplies).
type Budgets struct {
	MaxCPUms       int64
	MaxRSSkib      int64
	MaxOutputB     int64
	MaxTokenEst    int64
	MaxChildren    int
	MaxActiveChild int
}

// DefaultBudgets returns conservative ceilings.
func DefaultBudgets() Budgets {
	return Budgets{
		MaxCPUms: 600_000, MaxRSSkib: 4 * 1024 * 1024, MaxOutputB: 64 << 20,
		MaxTokenEst: 500_000, MaxChildren: 16, MaxActiveChild: 8,
	}
}

// Attempt is the parent LoopCoder attempt container.
type Attempt struct {
	AttemptID string
	Provider  string
	Model     string
	Policy    Policy
	// ParentProcessID root of the process tree.
	ParentProcessID string
	// Terminal blocked if escape/unjoined children.
	TerminalBlocked bool
	Attention       []string
}

// Status distinguishes native child activity from top-level progress.
type Status struct {
	ParentAttemptID   string `json:"parent_attempt_id"`
	NativeChildActive int    `json:"native_child_active"`
	NativeChildTotal  int    `json:"native_child_total"`
	// TopLevelProgress is never inferred from child prose.
	TopLevelProgress string `json:"top_level_progress"`
	// CompletionInferredFromChildProse always false by design.
	CompletionInferredFromChildProse bool      `json:"completion_inferred_from_child_prose"`
	Aggregate                        Aggregate `json:"aggregate"`
	Policy                           Policy    `json:"policy"`
	// InvocationFlag is the exact flag sent to the provider.
	InvocationFlag string `json:"invocation_flag"`
}

// Event is append-only evidence.
type Event struct {
	Schema    string
	Type      string
	SessionID string
	Detail    string
	At        time.Time
}

// Controller owns native children for one attempt.
type Controller struct {
	mu       sync.Mutex
	attempt  Attempt
	budget   Budgets
	parentU  struct{ cpu, rss, out, tok int64 }
	sessions map[string]*Session
	events   []Event
	seq      int64
	now      func() time.Time
}

// NewController creates a containment controller.
func NewController(att Attempt, budget Budgets, now func() time.Time) (*Controller, error) {
	if strings.TrimSpace(att.AttemptID) == "" || strings.TrimSpace(att.Provider) == "" || strings.TrimSpace(att.Model) == "" {
		return nil, fmt.Errorf("%w: attempt identity", ErrInvalid)
	}
	if att.Policy != PolicyForbidden && att.Policy != PolicyAllowed {
		return nil, fmt.Errorf("%w: policy", ErrInvalid)
	}
	if now == nil {
		now = time.Now
	}
	if budget.MaxChildren <= 0 {
		budget = DefaultBudgets()
	}
	return &Controller{
		attempt: att, budget: budget, sessions: map[string]*Session{}, now: now,
	}, nil
}

var (
	ErrInvalid   = errors.New("nativechild: invalid")
	ErrForbidden = errors.New("nativechild: forbidden by policy")
	ErrLimit     = errors.New("nativechild: budget")
	ErrEscape    = errors.New("nativechild: escape")
)

// InvocationFlag returns the exact provider invocation native-subagent setting.
func (c *Controller) InvocationFlag() string {
	if c.attempt.Policy == PolicyForbidden {
		return "native_subagents=forbidden"
	}
	return "native_subagents=allowed"
}

// ObserveStart records a native child under the parent. Rejected if policy forbids
// or would create independent ownership.
func (c *Controller) ObserveStart(sessionID, processID, parentPID string) (Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempt.Policy == PolicyForbidden {
		c.attempt.Attention = append(c.attempt.Attention, "policy_violation_native_start")
		c.attempt.TerminalBlocked = true
		return Session{}, fmt.Errorf("%w: policy forbids native children", ErrForbidden)
	}
	// parentPID must match parent process tree when both known
	if c.attempt.ParentProcessID != "" && parentPID != "" && parentPID != c.attempt.ParentProcessID {
		// still allow if descendant evidence later; mark for attention if not under parent
		// For containment: require equality or documented descendant prefix "child_of:"
		if !strings.HasPrefix(parentPID, "child_of:"+c.attempt.ParentProcessID) && parentPID != c.attempt.ParentProcessID {
			c.attempt.Attention = append(c.attempt.Attention, "escape:"+sessionID)
			c.attempt.TerminalBlocked = true
			return Session{}, fmt.Errorf("%w: not under parent tree", ErrEscape)
		}
	}
	if len(c.sessions) >= c.budget.MaxChildren {
		return Session{}, fmt.Errorf("%w: max children", ErrLimit)
	}
	active := 0
	for _, s := range c.sessions {
		if s.Active {
			active++
		}
	}
	if active >= c.budget.MaxActiveChild {
		return Session{}, fmt.Errorf("%w: max active children", ErrLimit)
	}
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		c.seq++
		sid = fmt.Sprintf("ncs_%d", c.seq)
	}
	s := &Session{
		Schema: SchemaSession, SessionID: sid, ParentAttempt: c.attempt.AttemptID,
		Provider: c.attempt.Provider, Model: c.attempt.Model,
		ProcessID: processID, ParentPID: parentPID,
		StartedAt: c.now().UTC(), Active: true,
	}
	c.sessions[sid] = s
	c.events = append(c.events, Event{Schema: SchemaEvent, Type: "start", SessionID: sid, At: c.now().UTC()})
	return *s, nil
}

// SampleUsage updates child usage (aggregated into parent budgets).
func (c *Controller) SampleUsage(sessionID string, cpuMs, rssKib, outB, tokens int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: session", ErrInvalid)
	}
	s.CPUms = cpuMs
	s.RSSkib = rssKib
	s.OutputB = outB
	s.TokenEst = tokens
	agg := c.aggregateLocked()
	if agg.OverBudget {
		return fmt.Errorf("%w: %s", ErrLimit, strings.Join(agg.Reasons, ","))
	}
	return nil
}

// SetParentUsage sets parent-only baseline usage (not multiplied).
func (c *Controller) SetParentUsage(cpuMs, rssKib, outB, tokens int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.parentU.cpu, c.parentU.rss, c.parentU.out, c.parentU.tok = cpuMs, rssKib, outB, tokens
}

// Join marks a child finished; parent cancel joins all.
func (c *Controller) Join(sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[sessionID]
	if !ok {
		return fmt.Errorf("%w: session", ErrInvalid)
	}
	s.Active = false
	c.events = append(c.events, Event{Schema: SchemaEvent, Type: "join", SessionID: sessionID, At: c.now().UTC()})
	return nil
}

// CancelParent joins all active children; unjoined/escape blocks clean terminal.
func (c *Controller) CancelParent() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.sessions {
		if s.Active {
			s.Active = false
			c.events = append(c.events, Event{Schema: SchemaEvent, Type: "join_on_cancel", SessionID: id, At: c.now().UTC()})
		}
		if s.Escape {
			c.attempt.TerminalBlocked = true
			c.attempt.Attention = append(c.attempt.Attention, "escape_unresolved:"+id)
		}
	}
	return c.statusLocked("cancelled")
}

// MarkEscape records a child that left the observable tree.
func (c *Controller) MarkEscape(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sessions[sessionID]; ok {
		s.Escape = true
		s.Active = false
		c.attempt.TerminalBlocked = true
		c.attempt.Attention = append(c.attempt.Attention, "escape:"+sessionID)
	}
}

// Status returns parent status without inferring completion from child prose.
func (c *Controller) Status(topLevel string) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked(topLevel)
}

func (c *Controller) statusLocked(topLevel string) Status {
	agg := c.aggregateLocked()
	active := 0
	for _, s := range c.sessions {
		if s.Active {
			active++
		}
	}
	return Status{
		ParentAttemptID:   c.attempt.AttemptID,
		NativeChildActive: active, NativeChildTotal: len(c.sessions),
		TopLevelProgress: topLevel, CompletionInferredFromChildProse: false,
		Aggregate: agg, Policy: c.attempt.Policy, InvocationFlag: c.InvocationFlag(),
	}
}

// CanTerminalClean is false if escape/unjoined/policy violation.
func (c *Controller) CanTerminalClean() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempt.TerminalBlocked {
		return false
	}
	for _, s := range c.sessions {
		if s.Active || s.Escape {
			return false
		}
	}
	return true
}

// DisallowedOwnership documents what native children cannot own.
func DisallowedOwnership() []string {
	return []string{
		"github_issue", "github_pr", "git_branch", "worktree",
		"verification", "merge", "route_change", "loopcoder_terminal_truth", "workitem_claim",
	}
}

func (c *Controller) aggregateLocked() Aggregate {
	agg := Aggregate{
		Schema: SchemaAgg, ParentAttemptID: c.attempt.AttemptID,
		ParentCPUms: c.parentU.cpu, ParentRSSkib: c.parentU.rss,
		ParentOutputB: c.parentU.out, ParentTokenEst: c.parentU.tok,
	}
	for _, s := range c.sessions {
		agg.ChildCount++
		if s.Active {
			agg.ActiveChildren++
		}
		agg.TotalCPUms += s.CPUms
		agg.TotalRSSkib += s.RSSkib
		agg.TotalOutputB += s.OutputB
		agg.TotalTokenEst += s.TokenEst
	}
	// Aggregate = parent + children (shared budget, not N× limit)
	totalCPU := agg.ParentCPUms + agg.TotalCPUms
	totalRSS := agg.ParentRSSkib + agg.TotalRSSkib
	totalOut := agg.ParentOutputB + agg.TotalOutputB
	totalTok := agg.ParentTokenEst + agg.TotalTokenEst
	if c.budget.MaxCPUms > 0 && totalCPU > c.budget.MaxCPUms {
		agg.OverBudget = true
		agg.Reasons = append(agg.Reasons, "cpu")
	}
	if c.budget.MaxRSSkib > 0 && totalRSS > c.budget.MaxRSSkib {
		agg.OverBudget = true
		agg.Reasons = append(agg.Reasons, "rss")
	}
	if c.budget.MaxOutputB > 0 && totalOut > c.budget.MaxOutputB {
		agg.OverBudget = true
		agg.Reasons = append(agg.Reasons, "output")
	}
	if c.budget.MaxTokenEst > 0 && totalTok > c.budget.MaxTokenEst {
		agg.OverBudget = true
		agg.Reasons = append(agg.Reasons, "tokens")
	}
	return agg
}
