package successor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/routedecision"
)

const (
	SchemaFailure   = "loopcoder.attempt.failure.v1"
	SchemaSuccessor = "loopcoder.attempt.successor.v1"
	SchemaPolicy    = "loopcoder.attempt.retry_policy.v1"
	PolicyVersion   = "successor-v1"
)

// FailureClass classifies why an attempt stopped.
type FailureClass string

const (
	// FailPreLaunch is definitive failure before provider process starts.
	FailPreLaunch FailureClass = "pre_launch_definitive"
	// FailProviderDeclined is provider refused / not started after admission.
	FailProviderDeclined FailureClass = "provider_declined_not_started"
	// FailTerminal is launched then definitive terminal failure.
	FailTerminal FailureClass = "launched_terminal_failure"
	// FailAmbiguous is launch/execution state unknown — never auto-fallback.
	FailAmbiguous FailureClass = "ambiguous_execution"
	// FailQuotaRate is quota exhausted or rate limited (may retry other route if policy allows).
	FailQuotaRate FailureClass = "quota_or_rate_limit"
	// FailPolicyChange is owner/policy change requiring re-decide (not auto).
	FailPolicyChange FailureClass = "policy_change"
)

// Valid reports known failure class.
func (c FailureClass) Valid() bool {
	switch c {
	case FailPreLaunch, FailProviderDeclined, FailTerminal, FailAmbiguous, FailQuotaRate, FailPolicyChange:
		return true
	}
	return false
}

// Failure is an immutable classification of an attempt stop.
type Failure struct {
	Schema     string       `json:"schema"`
	AttemptID  string       `json:"attempt_id"`
	DecisionID string       `json:"decision_id"`
	Class      FailureClass `json:"class"`
	ReasonCode string       `json:"reason_code"`
	// Provider/Model of the failed route (not a new selection).
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	Evidence  string    `json:"evidence_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RetryPolicy bounds automatic successors.
type RetryPolicy struct {
	Schema string `json:"schema"`
	// MaxAutomaticSuccessors default 1.
	MaxAutomaticSuccessors int `json:"max_automatic_successors"`
	// AllowPreLaunchAuto when true, proven pre-launch failure may auto-succeed.
	AllowPreLaunchAuto bool `json:"allow_pre_launch_auto"`
	// AllowTerminalAuto when true, terminal failure may auto-retry under budget.
	AllowTerminalAuto bool `json:"allow_terminal_auto"`
	// AllowQuotaAuto allows other-route retry on quota/rate-limit.
	AllowQuotaAuto bool `json:"allow_quota_auto"`
	// PinFallbackOrdered is owner-preauthorized ordered provider/model keys
	// ("provider/model") for pin fallback. Empty = no automatic pin fallback.
	PinFallbackOrdered []string `json:"pin_fallback_ordered,omitempty"`
	// ExcludeFailedRoute removes the failed provider/model from successor candidates.
	ExcludeFailedRoute bool `json:"exclude_failed_route"`
}

// DefaultPolicy returns the conservative default (one automatic retry max for
// pre-launch only; no ambiguous/pin auto fallback).
func DefaultPolicy() RetryPolicy {
	return RetryPolicy{
		Schema:                 SchemaPolicy,
		MaxAutomaticSuccessors: 1,
		AllowPreLaunchAuto:     true,
		AllowTerminalAuto:      false,
		AllowQuotaAuto:         false,
		ExcludeFailedRoute:     true,
	}
}

// Attempt is a visible attempt with immutable route decision linkage.
type Attempt struct {
	AttemptID      string `json:"attempt_id"`
	DecisionID     string `json:"decision_id"`
	DecisionDigest string `json:"decision_digest"`
	// PredecessorID is empty for first attempt.
	PredecessorID string `json:"predecessor_id,omitempty"`
	// SuccessorID set when a successor was created.
	SuccessorID string `json:"successor_id,omitempty"`
	// Active means route is frozen for this attempt.
	Active    bool      `json:"active"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// WorktreeRef / LogRef / EventRef preserved evidence pointers (opaque).
	WorktreeRef string `json:"worktree_ref,omitempty"`
	LogRef      string `json:"log_ref,omitempty"`
	EventRef    string `json:"event_ref,omitempty"`
	// AutomaticSuccessorCount lineage depth of auto successors from root.
	AutomaticSuccessorCount int `json:"automatic_successor_count"`
}

// SuccessorPlan is the result of evaluating whether a successor is allowed.
type SuccessorPlan struct {
	Schema         string       `json:"schema"`
	Allowed        bool         `json:"allowed"`
	NeedsHuman     bool         `json:"needs_human"`
	Automatic      bool         `json:"automatic"`
	StopReason     string       `json:"stop_reason,omitempty"`
	FailureClass   FailureClass `json:"failure_class"`
	PriorAttemptID string       `json:"prior_attempt_id"`
	// ExcludeKeys routes that must not be selected again.
	ExcludeKeys []string `json:"exclude_keys,omitempty"`
	// AuthorizedPinFallback when pin policy provides next named route.
	AuthorizedPinFallback string `json:"authorized_pin_fallback,omitempty"`
	// NewDecisionKey for the successor decide call.
	NewDecisionKey string `json:"new_decision_key,omitempty"`
	// CausalLink stable id linking prior failure → successor.
	CausalLink string   `json:"causal_link,omitempty"`
	Reasons    []string `json:"reasons"`
	Digest     string   `json:"digest"`
}

// Record is a created successor attempt (never mutates prior).
type Record struct {
	Schema    string        `json:"schema"`
	Successor Attempt       `json:"successor"`
	Prior     Attempt       `json:"prior"` // snapshot of prior after link
	Failure   Failure       `json:"failure"`
	Plan      SuccessorPlan `json:"plan"`
	// Decision is the new route decision for the successor (required when allowed).
	Decision *routedecision.Decision `json:"decision,omitempty"`
}

var (
	ErrInvalid     = errors.New("successor: invalid")
	ErrActiveRoute = errors.New("successor: cannot change route on active attempt")
	ErrNotFound    = errors.New("successor: not found")
	ErrBudget      = errors.New("successor: retry budget exhausted")
	ErrNeedsHuman  = errors.New("successor: needs human")
	ErrNotAllowed  = errors.New("successor: successor not allowed")
)

// Store tracks attempts and successor links (injected clock).
type Store struct {
	mu       sync.Mutex
	attempts map[string]*Attempt
	failures map[string]*Failure // by attempt
	seq      int64
	now      func() time.Time
}

// NewStore creates a store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{attempts: map[string]*Attempt{}, failures: map[string]*Failure{}, now: now}
}

// RegisterFirst records the first attempt for a decision (route frozen while active).
func (s *Store) RegisterFirst(decision routedecision.Decision, worktree, log, event string) (Attempt, error) {
	if s == nil {
		return Attempt{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if decision.DecisionID == "" || decision.Digest == "" {
		return Attempt{}, fmt.Errorf("%w: decision required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := fmt.Sprintf("att_%d", s.seq)
	a := &Attempt{
		AttemptID: id, DecisionID: decision.DecisionID, DecisionDigest: decision.Digest,
		Active: true, CreatedAt: s.now().UTC(),
		WorktreeRef: worktree, LogRef: log, EventRef: event,
	}
	if decision.Winner != nil {
		a.Provider = decision.Winner.Provider
		a.Model = decision.Winner.Model
	}
	s.attempts[id] = a
	return *a, nil
}

// Get returns an attempt.
func (s *Store) Get(id string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[strings.TrimSpace(id)]
	if !ok {
		return Attempt{}, ErrNotFound
	}
	return *a, nil
}

// MarkInactive freezes the attempt as no longer running (terminal). Does not
// change provider/model.
func (s *Store) MarkInactive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[strings.TrimSpace(id)]
	if !ok {
		return ErrNotFound
	}
	a.Active = false
	return nil
}

// RecordFailure classifies a stop. Fails if caller tries to mutate route fields.
func (s *Store) RecordFailure(f Failure) (Failure, error) {
	if !f.Class.Valid() {
		return Failure{}, fmt.Errorf("%w: class", ErrInvalid)
	}
	if strings.TrimSpace(f.AttemptID) == "" {
		return Failure{}, fmt.Errorf("%w: attempt_id", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[f.AttemptID]
	if !ok {
		return Failure{}, ErrNotFound
	}
	// Cannot change route identity on the attempt.
	if f.Provider != "" && a.Provider != "" && f.Provider != a.Provider {
		return Failure{}, fmt.Errorf("%w: provider", ErrActiveRoute)
	}
	if f.Model != "" && a.Model != "" && f.Model != a.Model {
		return Failure{}, fmt.Errorf("%w: model", ErrActiveRoute)
	}
	f.Schema = SchemaFailure
	if f.CreatedAt.IsZero() {
		f.CreatedAt = s.now().UTC()
	}
	if f.Provider == "" {
		f.Provider = a.Provider
	}
	if f.Model == "" {
		f.Model = a.Model
	}
	if f.DecisionID == "" {
		f.DecisionID = a.DecisionID
	}
	cp := f
	s.failures[f.AttemptID] = &cp
	a.Active = false // terminal classification ends active run
	return cp, nil
}

// PlanSuccessor evaluates whether a successor may be created (pure policy).
func PlanSuccessor(prior Attempt, fail Failure, pol RetryPolicy, autoCount int) SuccessorPlan {
	if pol.Schema == "" {
		pol.Schema = SchemaPolicy
	}
	if pol.MaxAutomaticSuccessors <= 0 {
		pol.MaxAutomaticSuccessors = 1
	}
	p := SuccessorPlan{
		Schema:         SchemaSuccessor,
		FailureClass:   fail.Class,
		PriorAttemptID: prior.AttemptID,
		Reasons:        []string{},
	}
	add := func(r string) { p.Reasons = append(p.Reasons, r) }

	// Ambiguous never auto-fallback
	if fail.Class == FailAmbiguous {
		p.NeedsHuman = true
		p.StopReason = "ambiguous_execution_needs_human"
		add("ambiguous.no_auto_fallback")
		p.Digest = digestPlan(p)
		return p
	}
	if fail.Class == FailPolicyChange {
		p.NeedsHuman = true
		p.StopReason = "policy_change_requires_owner"
		add("policy.change_needs_human")
		p.Digest = digestPlan(p)
		return p
	}

	// Budget
	if autoCount >= pol.MaxAutomaticSuccessors {
		p.StopReason = "retry_budget_exhausted"
		add("budget.exhausted")
		p.Digest = digestPlan(p)
		return p
	}

	// Class gates
	switch fail.Class {
	case FailPreLaunch, FailProviderDeclined:
		if !pol.AllowPreLaunchAuto {
			p.NeedsHuman = true
			p.StopReason = "pre_launch_auto_disabled"
			add("policy.pre_launch_auto_disabled")
			p.Digest = digestPlan(p)
			return p
		}
		p.Automatic = true
		add("pre_launch.auto_allowed")
	case FailTerminal:
		if !pol.AllowTerminalAuto {
			p.NeedsHuman = true
			p.StopReason = "terminal_auto_disabled"
			add("policy.terminal_auto_disabled")
			p.Digest = digestPlan(p)
			return p
		}
		p.Automatic = true
		add("terminal.auto_allowed")
	case FailQuotaRate:
		if !pol.AllowQuotaAuto {
			p.NeedsHuman = true
			p.StopReason = "quota_auto_disabled"
			add("policy.quota_auto_disabled")
			p.Digest = digestPlan(p)
			return p
		}
		p.Automatic = true
		add("quota.auto_allowed")
	default:
		p.NeedsHuman = true
		p.StopReason = "unknown_class"
		add("class.unknown")
		p.Digest = digestPlan(p)
		return p
	}

	if pol.ExcludeFailedRoute && (fail.Provider != "" || prior.Provider != "") {
		prov := fail.Provider
		if prov == "" {
			prov = prior.Provider
		}
		mod := fail.Model
		if mod == "" {
			mod = prior.Model
		}
		p.ExcludeKeys = append(p.ExcludeKeys, strings.ToLower(prov)+"/"+mod)
		add("exclude.failed_route")
	}

	// Pin fallback only if ordered policy lists a next route
	if len(pol.PinFallbackOrdered) > 0 {
		cur := strings.ToLower(prior.Provider) + "/" + prior.Model
		for i, k := range pol.PinFallbackOrdered {
			if strings.ToLower(strings.TrimSpace(k)) == cur && i+1 < len(pol.PinFallbackOrdered) {
				p.AuthorizedPinFallback = strings.TrimSpace(pol.PinFallbackOrdered[i+1])
				add("pin.authorized_ordered_fallback")
				break
			}
		}
		if p.AuthorizedPinFallback == "" {
			// pin without authorized next → needs human for pin path
			add("pin.no_authorized_next")
		}
	}

	p.Allowed = true
	p.NewDecisionKey = prior.AttemptID + ":successor:" + string(fail.Class)
	p.CausalLink = causalLink(prior.AttemptID, fail)
	add("successor.allowed")
	p.Digest = digestPlan(p)
	return p
}

// CreateSuccessor materializes a successor attempt with a new decision.
// Prior attempt fields are never overwritten except SuccessorID link.
func (s *Store) CreateSuccessor(priorID string, fail Failure, pol RetryPolicy, newDecision routedecision.Decision) (Record, error) {
	if s == nil {
		return Record{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, ok := s.attempts[strings.TrimSpace(priorID)]
	if !ok {
		return Record{}, ErrNotFound
	}
	// Capture prior snapshot for record (immutable copy of current state).
	priorSnap := *prior
	plan := PlanSuccessor(priorSnap, fail, pol, prior.AutomaticSuccessorCount)
	if !plan.Allowed {
		if plan.NeedsHuman {
			return Record{Plan: plan, Failure: fail, Prior: priorSnap}, ErrNeedsHuman
		}
		return Record{Plan: plan, Failure: fail, Prior: priorSnap}, ErrNotAllowed
	}
	if newDecision.DecisionID == "" || newDecision.Digest == "" {
		return Record{}, fmt.Errorf("%w: new decision required", ErrInvalid)
	}
	if newDecision.Digest == prior.DecisionDigest {
		return Record{}, fmt.Errorf("%w: successor must have new decision digest", ErrInvalid)
	}
	// Exclude failed route from winner if policy says so
	if len(plan.ExcludeKeys) > 0 && newDecision.Winner != nil {
		wkey := strings.ToLower(newDecision.Winner.Provider) + "/" + newDecision.Winner.Model
		for _, ex := range plan.ExcludeKeys {
			if wkey == ex {
				return Record{}, fmt.Errorf("%w: winner is excluded failed route", ErrNotAllowed)
			}
		}
	}

	s.seq++
	sid := fmt.Sprintf("att_%d", s.seq)
	autoCount := prior.AutomaticSuccessorCount
	if plan.Automatic {
		autoCount++
	}
	succ := &Attempt{
		AttemptID: sid, DecisionID: newDecision.DecisionID, DecisionDigest: newDecision.Digest,
		PredecessorID: prior.AttemptID, Active: true,
		CreatedAt: s.now().UTC(),
		// Preserve evidence pointers from prior (do not overwrite prior storage).
		WorktreeRef: prior.WorktreeRef, LogRef: prior.LogRef, EventRef: prior.EventRef,
		AutomaticSuccessorCount: autoCount,
	}
	if newDecision.Winner != nil {
		succ.Provider = newDecision.Winner.Provider
		succ.Model = newDecision.Winner.Model
	}
	// Link prior → successor without clearing prior evidence.
	prior.SuccessorID = sid
	prior.Active = false
	s.attempts[sid] = succ
	// Ensure failure stored
	if _, ok := s.failures[prior.AttemptID]; !ok {
		f := fail
		f.Schema = SchemaFailure
		if f.CreatedAt.IsZero() {
			f.CreatedAt = s.now().UTC()
		}
		s.failures[prior.AttemptID] = &f
	}
	priorOut := *prior
	return Record{
		Schema: SchemaSuccessor, Successor: *succ, Prior: priorOut,
		Failure: fail, Plan: plan, Decision: &newDecision,
	}, nil
}

// StatusExplain returns visible retry/fallback budget and stop reasons.
func (s *Store) StatusExplain(attemptID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[strings.TrimSpace(attemptID)]
	if !ok {
		return "", ErrNotFound
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Attempt %s active=%v decision=%s digest=%s\n", a.AttemptID, a.Active, a.DecisionID, a.DecisionDigest)
	fmt.Fprintf(&b, "Route: %s/%s\n", a.Provider, a.Model)
	if a.PredecessorID != "" {
		fmt.Fprintf(&b, "Predecessor: %s\n", a.PredecessorID)
	}
	if a.SuccessorID != "" {
		fmt.Fprintf(&b, "Successor: %s\n", a.SuccessorID)
	}
	fmt.Fprintf(&b, "AutomaticSuccessorCount: %d\n", a.AutomaticSuccessorCount)
	fmt.Fprintf(&b, "Evidence worktree=%s log=%s event=%s\n", a.WorktreeRef, a.LogRef, a.EventRef)
	if f, ok := s.failures[a.AttemptID]; ok {
		fmt.Fprintf(&b, "Failure class=%s reason=%s\n", f.Class, f.ReasonCode)
	}
	return b.String(), nil
}

func causalLink(priorID string, f Failure) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s", priorID, f.Class, f.ReasonCode, f.Evidence)
	return "caus_" + hex.EncodeToString(h.Sum(nil))[:16]
}

func digestPlan(p SuccessorPlan) string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}
