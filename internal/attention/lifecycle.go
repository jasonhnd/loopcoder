package attention

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	SchemaAttention = "loopcoder.attention.v1"
	SchemaAction    = "loopcoder.attention.action.v1"
	SchemaIndex     = "loopcoder.attention.index.v1"
	MaxReason       = 512
	MaxInput        = 2 << 10
	MaxEvidenceKeys = 16
)

// State is the attention lifecycle state.
type State string

const (
	StateOpen         State = "open"
	StateAcknowledged State = "acknowledged"
	StateResolved     State = "resolved"
	StateSuperseded   State = "superseded"
)

// Kind classifies attention.
type Kind string

const (
	KindNeedsHuman    Kind = "needs_human"
	KindPermission    Kind = "permission"
	KindRecovery      Kind = "recovery"
	KindDeliveryBlock Kind = "delivery_block"
	KindInputRequired Kind = "input_required"
)

// Severity ranks urgency.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarn     Severity = "warn"
	SeverityCritical Severity = "critical"
)

// ActionType is a bounded operator action.
type ActionType string

const (
	ActionAcknowledge       ActionType = "acknowledge"
	ActionBoundedInput      ActionType = "bounded_input"
	ActionPermissionAllow   ActionType = "permission_approve"
	ActionPermissionDeny    ActionType = "permission_deny"
	ActionCancel            ActionType = "cancel"
	ActionExplicitDetach    ActionType = "explicit_detach"
	ActionProviderFreeRetry ActionType = "provider_free_retry"
	ActionRecoverySelect    ActionType = "recovery_select"
)

// ForbiddenActionType documents rejected control-plane mutations.
type ForbiddenActionType string

const (
	ForbiddenMutateRoute   ForbiddenActionType = "mutate_route_pin"
	ForbiddenForgeComplete ForbiddenActionType = "forge_completion"
	ForbiddenBypassAdmit   ForbiddenActionType = "bypass_admission"
	ForbiddenSignalUnowned ForbiddenActionType = "signal_unowned_process"
)

var (
	ErrNotFound          = errors.New("attention: not found")
	ErrStaleRevision     = errors.New("attention: stale run revision")
	ErrUnauthorized      = errors.New("attention: unauthorized")
	ErrUnsupportedAction = errors.New("attention: unsupported action")
	ErrAlreadyTerminal   = errors.New("attention: already terminal")
	ErrConflict          = errors.New("attention: conflicting action")
	ErrInvalid           = errors.New("attention: invalid request")
	ErrForbidden         = errors.New("attention: forbidden control-plane mutation")
	ErrCrossProject      = errors.New("attention: cross-project")
)

// Attention is durable product attention state.
type Attention struct {
	Schema         string            `json:"schema"`
	ID             string            `json:"id"`
	ProjectID      string            `json:"project_id"`
	RunID          string            `json:"run_id,omitempty"`
	AttemptID      string            `json:"attempt_id,omitempty"`
	RunRevision    int64             `json:"run_revision"`
	Kind           Kind              `json:"kind"`
	Severity       Severity          `json:"severity"`
	Reason         string            `json:"reason"`
	AllowedActions []ActionType      `json:"allowed_actions"`
	Deadline       time.Time         `json:"deadline,omitempty"`
	Evidence       map[string]string `json:"evidence"`
	State          State             `json:"state"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	// PrivateBody is project-scoped only; never projected to machine index.
	PrivateBody string `json:"-"`
}

// RedactedSummary is machine-wide safe index row.
type RedactedSummary struct {
	Schema      string    `json:"schema"`
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Kind        Kind      `json:"kind"`
	Severity    Severity  `json:"severity"`
	State       State     `json:"state"`
	AgeMS       int64     `json:"age_ms"`
	Remediation string    `json:"remediation"`
	Deadline    time.Time `json:"deadline,omitempty"`
}

// ActionRequest is a client-submitted authorized action.
type ActionRequest struct {
	Schema           string     `json:"schema"`
	AttentionID      string     `json:"attention_id"`
	ProjectID        string     `json:"project_id"`
	ClientID         string     `json:"client_id"`
	SessionID        string     `json:"session_id"`
	ExpectedRevision int64      `json:"expected_run_revision"`
	IdempotencyKey   string     `json:"idempotency_key"`
	Action           ActionType `json:"action"`
	// Authorization token/scope for action (e.g. permission name).
	Authorization string            `json:"authorization,omitempty"`
	Input         string            `json:"input,omitempty"`
	Recovery      string            `json:"recovery,omitempty"`
	Extra         map[string]string `json:"extra,omitempty"`
}

// ActionResult is the typed outcome of Submit.
type ActionResult struct {
	Schema         string     `json:"schema"`
	AttentionID    string     `json:"attention_id"`
	Action         ActionType `json:"action"`
	IdempotencyKey string     `json:"idempotency_key"`
	State          State      `json:"state"`
	// Effect is the mapped runtime/delivery transition name (not a UI side channel).
	Effect     string    `json:"effect"`
	EvidenceID string    `json:"evidence_id"`
	Duplicate  bool      `json:"duplicate,omitempty"`
	AppliedAt  time.Time `json:"applied_at"`
}

// EvidenceRecord is append-only action evidence.
type EvidenceRecord struct {
	ID             string     `json:"id"`
	AttentionID    string     `json:"attention_id"`
	ProjectID      string     `json:"project_id"`
	Action         ActionType `json:"action"`
	ClientID       string     `json:"client_id"`
	SessionID      string     `json:"session_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	BeforeState    State      `json:"before_state"`
	AfterState     State      `json:"after_state"`
	Effect         string     `json:"effect"`
	At             time.Time  `json:"at"`
	Digest         string     `json:"digest"`
}

// Store is the in-process durable attention authority (project + machine index).
type Store struct {
	mu        sync.Mutex
	items     map[string]*Attention // id -> attention
	byProject map[string][]string   // project -> ids
	evidence  []EvidenceRecord
	// idempotency: key client|idem -> result
	idem map[string]ActionResult
	now  func() time.Time
	seq  int64
}

// NewStore creates an empty store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		items:     map[string]*Attention{},
		byProject: map[string][]string{},
		idem:      map[string]ActionResult{},
		now:       now,
	}
}

// OpenInput creates a new open attention.
type OpenInput struct {
	ID             string
	ProjectID      string
	RunID          string
	AttemptID      string
	RunRevision    int64
	Kind           Kind
	Severity       Severity
	Reason         string
	AllowedActions []ActionType
	Deadline       time.Time
	Evidence       map[string]string
	PrivateBody    string
}

// Open registers a new open attention.
func (s *Store) Open(in OpenInput) (Attention, error) {
	if in.ProjectID == "" || in.Kind == "" || in.Reason == "" {
		return Attention{}, fmt.Errorf("%w: missing identity/reason", ErrInvalid)
	}
	if len(in.Reason) > MaxReason {
		return Attention{}, fmt.Errorf("%w: reason too long", ErrInvalid)
	}
	if in.Severity == "" {
		in.Severity = SeverityWarn
	}
	if len(in.AllowedActions) == 0 {
		in.AllowedActions = defaultActions(in.Kind)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := in.ID
	if id == "" {
		id = fmt.Sprintf("att_%d", s.seq)
	}
	if _, ok := s.items[id]; ok {
		return Attention{}, fmt.Errorf("%w: id exists", ErrConflict)
	}
	now := s.now().UTC()
	a := &Attention{
		Schema:         SchemaAttention,
		ID:             id,
		ProjectID:      in.ProjectID,
		RunID:          in.RunID,
		AttemptID:      in.AttemptID,
		RunRevision:    in.RunRevision,
		Kind:           in.Kind,
		Severity:       in.Severity,
		Reason:         in.Reason,
		AllowedActions: append([]ActionType(nil), in.AllowedActions...),
		Deadline:       in.Deadline,
		Evidence:       copyMap(in.Evidence),
		State:          StateOpen,
		CreatedAt:      now,
		UpdatedAt:      now,
		PrivateBody:    in.PrivateBody,
	}
	s.items[id] = a
	s.byProject[in.ProjectID] = append(s.byProject[in.ProjectID], id)
	return *a, nil
}

// Get returns a full project-scoped attention (includes private body for project callers).
func (s *Store) Get(projectID, id string) (Attention, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.items[id]
	if !ok {
		return Attention{}, ErrNotFound
	}
	if a.ProjectID != projectID {
		return Attention{}, ErrCrossProject
	}
	out := *a
	out.Evidence = copyMap(a.Evidence)
	out.AllowedActions = append([]ActionType(nil), a.AllowedActions...)
	return out, nil
}

// ListOpen returns open/acknowledged attentions for a project.
func (s *Store) ListOpen(projectID string) []Attention {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Attention
	for _, id := range s.byProject[projectID] {
		a := s.items[id]
		if a.State == StateOpen || a.State == StateAcknowledged {
			cp := *a
			cp.Evidence = copyMap(a.Evidence)
			cp.AllowedActions = append([]ActionType(nil), a.AllowedActions...)
			// strip private for list default
			cp.PrivateBody = ""
			out = append(out, cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// MachineIndex returns redacted summaries across projects (no private bodies).
func (s *Store) MachineIndex() []RedactedSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	var out []RedactedSummary
	for _, a := range s.items {
		if a.State != StateOpen && a.State != StateAcknowledged {
			continue
		}
		out = append(out, RedactedSummary{
			Schema:      SchemaIndex,
			ID:          a.ID,
			ProjectID:   a.ProjectID,
			Kind:        a.Kind,
			Severity:    a.Severity,
			State:       a.State,
			AgeMS:       now.Sub(a.CreatedAt).Milliseconds(),
			Remediation: safeRemediation(a),
			Deadline:    a.Deadline,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Evidence returns append-only evidence for an attention (project scoped).
func (s *Store) Evidence(projectID, attentionID string) ([]EvidenceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.items[attentionID]
	if !ok {
		return nil, ErrNotFound
	}
	if a.ProjectID != projectID {
		return nil, ErrCrossProject
	}
	var out []EvidenceRecord
	for _, e := range s.evidence {
		if e.AttentionID == attentionID {
			out = append(out, e)
		}
	}
	return out, nil
}

// Submit applies an authorized action with evidence-before-effect.
func (s *Store) Submit(req ActionRequest) (ActionResult, error) {
	if err := validateRequest(req); err != nil {
		return ActionResult{}, err
	}
	if forbidden := detectForbidden(req); forbidden != "" {
		return ActionResult{}, fmt.Errorf("%w: %s", ErrForbidden, forbidden)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	idemKey := req.ClientID + "|" + req.IdempotencyKey
	if prev, ok := s.idem[idemKey]; ok {
		prev.Duplicate = true
		return prev, nil
	}

	a, ok := s.items[req.AttentionID]
	if !ok {
		return ActionResult{}, ErrNotFound
	}
	if a.ProjectID != req.ProjectID {
		return ActionResult{}, ErrCrossProject
	}
	if a.State == StateResolved || a.State == StateSuperseded {
		return ActionResult{}, ErrAlreadyTerminal
	}
	if req.ExpectedRevision != a.RunRevision {
		return ActionResult{}, fmt.Errorf("%w: expected %d have %d", ErrStaleRevision, req.ExpectedRevision, a.RunRevision)
	}
	if !actionAllowed(a.AllowedActions, req.Action) {
		return ActionResult{}, fmt.Errorf("%w: %s", ErrUnsupportedAction, req.Action)
	}
	if err := authorizeAction(a, req); err != nil {
		return ActionResult{}, err
	}

	before := a.State
	after, effect, err := transition(a, req)
	if err != nil {
		return ActionResult{}, err
	}

	// Evidence before effect (state write).
	s.seq++
	evID := fmt.Sprintf("ev_%d", s.seq)
	now := s.now().UTC()
	ev := EvidenceRecord{
		ID:             evID,
		AttentionID:    a.ID,
		ProjectID:      a.ProjectID,
		Action:         req.Action,
		ClientID:       req.ClientID,
		SessionID:      req.SessionID,
		IdempotencyKey: req.IdempotencyKey,
		BeforeState:    before,
		AfterState:     after,
		Effect:         effect,
		At:             now,
	}
	ev.Digest = digestEvidence(ev)
	s.evidence = append(s.evidence, ev)

	// Effect: mutate attention state + evidence map.
	a.State = after
	a.UpdatedAt = now
	if a.Evidence == nil {
		a.Evidence = map[string]string{}
	}
	a.Evidence["last_action"] = string(req.Action)
	a.Evidence["last_evidence_id"] = evID
	if req.Input != "" {
		// store redacted length only in public evidence
		a.Evidence["input_len"] = fmt.Sprintf("%d", len(req.Input))
	}
	if req.Recovery != "" {
		a.Evidence["recovery"] = sanitize(req.Recovery)
	}

	res := ActionResult{
		Schema:         SchemaAction,
		AttentionID:    a.ID,
		Action:         req.Action,
		IdempotencyKey: req.IdempotencyKey,
		State:          after,
		Effect:         effect,
		EvidenceID:     evID,
		AppliedAt:      now,
	}
	s.idem[idemKey] = res
	return res, nil
}

// Supersede marks open attention superseded (e.g. newer attention replaces it).
func (s *Store) Supersede(projectID, id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.items[id]
	if !ok {
		return ErrNotFound
	}
	if a.ProjectID != projectID {
		return ErrCrossProject
	}
	if a.State == StateResolved || a.State == StateSuperseded {
		return ErrAlreadyTerminal
	}
	a.State = StateSuperseded
	a.UpdatedAt = s.now().UTC()
	if a.Evidence == nil {
		a.Evidence = map[string]string{}
	}
	a.Evidence["supersede_reason"] = sanitize(reason)
	return nil
}

// RebuildIndex is a no-op projection rebuild (in-memory items are source of truth).
func (s *Store) RebuildIndex() []RedactedSummary {
	return s.MachineIndex()
}

func validateRequest(req ActionRequest) error {
	if req.AttentionID == "" || req.ProjectID == "" || req.ClientID == "" || req.SessionID == "" {
		return fmt.Errorf("%w: missing client/attention identity", ErrInvalid)
	}
	if req.IdempotencyKey == "" || req.Action == "" {
		return fmt.Errorf("%w: missing action/idempotency", ErrInvalid)
	}
	if len(req.Input) > MaxInput {
		return fmt.Errorf("%w: input too large", ErrInvalid)
	}
	return nil
}

func detectForbidden(req ActionRequest) ForbiddenActionType {
	// Explicit forbidden control-plane keys in extra/input.
	blob := strings.ToLower(req.Input + " " + req.Authorization + " " + req.Recovery)
	for k, v := range req.Extra {
		blob += " " + strings.ToLower(k+"="+v)
	}
	switch {
	case strings.Contains(blob, "mutate_route") || strings.Contains(blob, "route_pin") || req.Action == ActionType("set_route"):
		return ForbiddenMutateRoute
	case strings.Contains(blob, "forge_completion") || req.Action == ActionType("force_complete"):
		return ForbiddenForgeComplete
	case strings.Contains(blob, "bypass_admission") || req.Action == ActionType("bypass_admission"):
		return ForbiddenBypassAdmit
	case strings.Contains(blob, "signal_unowned") || req.Action == ActionType("kill_unowned"):
		return ForbiddenSignalUnowned
	}
	return ""
}

func authorizeAction(a *Attention, req ActionRequest) error {
	switch req.Action {
	case ActionPermissionAllow, ActionPermissionDeny:
		if req.Authorization == "" {
			return fmt.Errorf("%w: permission actions require authorization scope", ErrUnauthorized)
		}
		// scope must match attention evidence permission name when present
		if want, ok := a.Evidence["permission"]; ok && want != "" && req.Authorization != want {
			return fmt.Errorf("%w: authorization scope mismatch", ErrUnauthorized)
		}
	case ActionBoundedInput:
		if strings.TrimSpace(req.Input) == "" {
			return fmt.Errorf("%w: bounded_input requires input", ErrInvalid)
		}
	case ActionRecoverySelect:
		if strings.TrimSpace(req.Recovery) == "" {
			return fmt.Errorf("%w: recovery_select requires recovery", ErrInvalid)
		}
	}
	return nil
}

func transition(a *Attention, req ActionRequest) (State, string, error) {
	switch req.Action {
	case ActionAcknowledge:
		if a.State == StateAcknowledged {
			return StateAcknowledged, "runtime.attention.ack_noop", nil
		}
		return StateAcknowledged, "runtime.attention.acknowledged", nil
	case ActionBoundedInput:
		return StateResolved, "delivery.operator_input_accepted", nil
	case ActionPermissionAllow:
		return StateResolved, "runtime.permission.approved", nil
	case ActionPermissionDeny:
		return StateResolved, "runtime.permission.denied", nil
	case ActionCancel:
		return StateResolved, "runtime.attempt.cancel_requested", nil
	case ActionExplicitDetach:
		return StateResolved, "runtime.supervisor.explicit_detach", nil
	case ActionProviderFreeRetry:
		return StateResolved, "delivery.retry.provider_free", nil
	case ActionRecoverySelect:
		return StateResolved, "runtime.recovery.selected", nil
	default:
		return "", "", fmt.Errorf("%w: %s", ErrUnsupportedAction, req.Action)
	}
}

func actionAllowed(allowed []ActionType, act ActionType) bool {
	for _, a := range allowed {
		if a == act {
			return true
		}
	}
	return false
}

func defaultActions(k Kind) []ActionType {
	switch k {
	case KindPermission:
		return []ActionType{ActionAcknowledge, ActionPermissionAllow, ActionPermissionDeny, ActionCancel}
	case KindRecovery:
		return []ActionType{ActionAcknowledge, ActionRecoverySelect, ActionCancel, ActionProviderFreeRetry}
	case KindInputRequired:
		return []ActionType{ActionAcknowledge, ActionBoundedInput, ActionCancel}
	default:
		return []ActionType{
			ActionAcknowledge, ActionCancel, ActionExplicitDetach,
			ActionProviderFreeRetry, ActionBoundedInput,
		}
	}
}

func safeRemediation(a *Attention) string {
	if len(a.AllowedActions) == 0 {
		return "review"
	}
	return string(a.AllowedActions[0])
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if len(out) >= MaxEvidenceKeys {
			break
		}
		out[k] = sanitize(v)
	}
	return out
}

func sanitize(s string) string {
	lower := strings.ToLower(s)
	for _, bad := range []string{"sk-", "ghp_", "password", "api_key", "-----begin", "bearer "} {
		if strings.Contains(lower, bad) {
			return "[redacted]"
		}
	}
	if strings.HasPrefix(s, "/") && strings.Count(s, "/") >= 2 {
		return "[path]"
	}
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

func digestEvidence(e EvidenceRecord) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|%s",
		e.AttentionID, e.Action, e.ClientID, e.IdempotencyKey, e.BeforeState, e.AfterState, e.Effect)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:24]
}
