package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	AgentRegistrationSchema  = "loopcoder.agent_registration.v1"
	AgentScopeGrantSchema    = "loopcoder.agent_scope_grant.v1"
	AgentOwnershipLockSchema = "loopcoder.agent_ownership_lock.v1"
	AgentBudgetBindingSchema = "loopcoder.agent_budget_binding.v1"
	AgentTreeSchema          = "loopcoder.agent_tree.v1"
	AgentPolicyVersion       = "0805.agent_federation.v1"

	AgentStatePlanned    = "planned"
	AgentStateRegistered = "registered"
	AgentStateLaunching  = "launching"
	AgentStateRunning    = "running"
	AgentStateCancelling = "cancelling"
	AgentStateSucceeded  = "succeeded"
	AgentStateFailed     = "failed"
	AgentStateCancelled  = "cancelled"
	AgentStateNeedsHuman = "needs-human"
	AgentStateSuperseded = "superseded"

	AgentActionRegister        = "register"
	AgentActionLaunch          = "launch"
	AgentActionHeartbeat       = "heartbeat"
	AgentActionCancel          = "cancel"
	AgentActionCompleteSuccess = "complete_success"
	AgentActionCompleteFailure = "complete_failure"
	AgentActionRecoverTakeover = "recover_takeover"
	AgentActionSupersede       = "supersede"
	AgentActionInvalidate      = "invalidate"

	PermissionReadOnly    = "read-only"
	PermissionWrite       = "write"
	PermissionOrchestrate = "orchestrate"

	SideEffectNone      = "none"
	SideEffectLocalRead = "local-read"
	SideEffectRepoWrite = "repo-write"

	OwnershipStateRequested = "requested"
	OwnershipStateHeld      = "held"
	OwnershipStateReleasing = "releasing"
	OwnershipStateReleased  = "released"
	OwnershipStateExpired   = "expired"
	OwnershipStateConflict  = "conflict"

	DefaultChildOutputLimit = 64 * 1024
)

type FederationErrorCode string

const (
	ErrAgentRegistrationRequiredCode FederationErrorCode = "ErrAgentRegistrationRequired"
	ErrAgentRegistrationConflictCode FederationErrorCode = "ErrAgentRegistrationConflict"
	ErrUnsupportedNativeSubAgentCode FederationErrorCode = "ErrUnsupportedNativeSubAgent"
	ErrScopeWideningCode             FederationErrorCode = "ErrScopeWidening"
	ErrScopeUnknownCode              FederationErrorCode = "ErrScopeUnknown"
	ErrCredentialScopeDeniedCode     FederationErrorCode = "ErrCredentialScopeDenied"
	ErrOneWriterConflictCode         FederationErrorCode = "ErrOneWriterConflict"
	ErrOwnershipRequiredCode         FederationErrorCode = "ErrOwnershipRequired"
	ErrChildBudgetRequiredCode       FederationErrorCode = "ErrChildBudgetReservationRequired"
	ErrAgentFingerprintMismatchCode  FederationErrorCode = "ErrAgentFingerprintMismatch"
	ErrInvalidTransitionCode         FederationErrorCode = "ErrInvalidTransition"
	ErrTerminalStateCode             FederationErrorCode = "ErrTerminalState"
	ErrCrossProjectReferenceCode     FederationErrorCode = "ErrCrossProjectReference"
	ErrInvalidRecordCode             FederationErrorCode = "ErrInvalidRecord"
	ErrDuplicateReplayCode           FederationErrorCode = "ErrDuplicateReplay"
	ErrStaleClaimCode                FederationErrorCode = "ErrStaleClaim"
)

var (
	ErrAgentRegistrationRequired = &FederationError{Code: ErrAgentRegistrationRequiredCode}
	ErrAgentRegistrationConflict = &FederationError{Code: ErrAgentRegistrationConflictCode}
	ErrUnsupportedNativeSubAgent = &FederationError{Code: ErrUnsupportedNativeSubAgentCode}
	ErrScopeWidening             = &FederationError{Code: ErrScopeWideningCode}
	ErrScopeUnknown              = &FederationError{Code: ErrScopeUnknownCode}
	ErrCredentialScopeDenied     = &FederationError{Code: ErrCredentialScopeDeniedCode}
	ErrOneWriterConflict         = &FederationError{Code: ErrOneWriterConflictCode}
	ErrOwnershipRequired         = &FederationError{Code: ErrOwnershipRequiredCode}
	ErrChildBudgetRequired       = &FederationError{Code: ErrChildBudgetRequiredCode}
	ErrAgentFingerprintMismatch  = &FederationError{Code: ErrAgentFingerprintMismatchCode}
	ErrInvalidTransition         = &FederationError{Code: ErrInvalidTransitionCode}
	ErrTerminalState             = &FederationError{Code: ErrTerminalStateCode}
	ErrCrossProjectReference     = &FederationError{Code: ErrCrossProjectReferenceCode}
	ErrInvalidRecord             = &FederationError{Code: ErrInvalidRecordCode}
	ErrDuplicateReplay           = &FederationError{Code: ErrDuplicateReplayCode}
	ErrStaleClaim                = &FederationError{Code: ErrStaleClaimCode}
)

var agentFederationSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS agent_scope_grants (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		delivery_run_id TEXT NOT NULL,
		child_agent_id TEXT NOT NULL,
		schema_version TEXT NOT NULL,
		record_version INTEGER NOT NULL,
		scope_json TEXT NOT NULL,
		permission TEXT NOT NULL,
		side_effect_class TEXT NOT NULL,
		policy_version TEXT NOT NULL,
		policy_fingerprint TEXT NOT NULL,
		plan_fingerprint TEXT NOT NULL,
		authorization_fingerprint TEXT NOT NULL DEFAULT '',
		agent_federation_fingerprint TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		terminal_error_code TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_scope_grants_child ON agent_scope_grants(child_agent_id)`,
	`CREATE TABLE IF NOT EXISTS agent_registrations (
		id TEXT PRIMARY KEY,
		record_version INTEGER NOT NULL DEFAULT 1,
		project_id TEXT NOT NULL,
		delivery_run_id TEXT NOT NULL,
		root_run_id TEXT NOT NULL,
		parent_run_id TEXT NOT NULL,
		child_run_id TEXT NOT NULL,
		parent_agent_id TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL,
		attempt_id TEXT NOT NULL,
		plan_id TEXT NOT NULL,
		child_key TEXT NOT NULL,
		adapter_id TEXT NOT NULL,
		provider_installation_id TEXT NOT NULL DEFAULT '',
		account_profile_id TEXT NOT NULL DEFAULT '',
		model_capability_id TEXT NOT NULL DEFAULT '',
		routing_decision_id TEXT NOT NULL DEFAULT '',
		provider_session_ref TEXT NOT NULL DEFAULT '',
		scope_grant_id TEXT NOT NULL,
		permission TEXT NOT NULL,
		side_effect_class TEXT NOT NULL,
		budget_binding_ids_json TEXT NOT NULL DEFAULT '[]',
		ownership_lock_ids_json TEXT NOT NULL DEFAULT '[]',
		claim_generation INTEGER NOT NULL,
		executor_id TEXT NOT NULL,
		provider_idempotency_key TEXT NOT NULL,
		provider_receipt TEXT NOT NULL DEFAULT '',
		cancellation_channel TEXT NOT NULL,
		expected_outputs_json TEXT NOT NULL,
		registration_state TEXT NOT NULL,
		depth INTEGER NOT NULL DEFAULT 0,
		policy_version TEXT NOT NULL,
		plan_fingerprint TEXT NOT NULL,
		policy_fingerprint TEXT NOT NULL,
		authorization_fingerprint TEXT NOT NULL DEFAULT '',
		agent_federation_fingerprint TEXT NOT NULL,
		registration_payload_hash TEXT NOT NULL,
		classification TEXT NOT NULL DEFAULT 'local-diagnostic',
		gap_reasons_json TEXT NOT NULL DEFAULT '[]',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		terminal_error_code TEXT NOT NULL DEFAULT '',
		UNIQUE(project_id, delivery_run_id, parent_run_id, child_key),
		UNIQUE(child_run_id),
		FOREIGN KEY(scope_grant_id) REFERENCES agent_scope_grants(id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_registrations_root ON agent_registrations(project_id, root_run_id)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_registrations_parent_agent ON agent_registrations(parent_agent_id)`,
	`CREATE TABLE IF NOT EXISTS agent_ownership_locks (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		delivery_run_id TEXT NOT NULL,
		child_agent_id TEXT NOT NULL,
		run_id TEXT NOT NULL,
		claim_generation INTEGER NOT NULL,
		lock_generation INTEGER NOT NULL,
		resource_kind TEXT NOT NULL,
		resource_key TEXT NOT NULL,
		lock_mode TEXT NOT NULL,
		state TEXT NOT NULL,
		lease_expires_at TEXT NOT NULL,
		heartbeat_at TEXT NOT NULL,
		conflicts_with_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_ownership_active_exact ON agent_ownership_locks(project_id, resource_kind, resource_key)
		WHERE state NOT IN ('released', 'expired', 'conflict', 'needs-human')`,
	`CREATE INDEX IF NOT EXISTS idx_agent_ownership_child ON agent_ownership_locks(child_agent_id)`,
	`CREATE TABLE IF NOT EXISTS agent_budget_bindings (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		delivery_run_id TEXT NOT NULL,
		child_agent_id TEXT NOT NULL,
		budget_policy_id TEXT NOT NULL,
		budget_reservation_id TEXT NOT NULL,
		reservation_scope TEXT NOT NULL,
		reserved_quantities_json TEXT NOT NULL,
		ancestor_budget_refs_json TEXT NOT NULL,
		reservation_state TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(child_agent_id, budget_policy_id, budget_reservation_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_budget_child ON agent_budget_bindings(child_agent_id)`,
	`CREATE TABLE IF NOT EXISTS agent_events (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		delivery_run_id TEXT NOT NULL,
		child_agent_id TEXT NOT NULL,
		event_kind TEXT NOT NULL,
		event_hash TEXT NOT NULL,
		previous_event_hash TEXT NOT NULL DEFAULT '',
		payload_hash TEXT NOT NULL,
		payload_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_agent_events_child_created ON agent_events(child_agent_id, created_at, id)`,
}

type FederationError struct {
	Code    FederationErrorCode `json:"code"`
	Message string              `json:"message,omitempty"`
}

func (e *FederationError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

func (e *FederationError) Is(target error) bool {
	var typed *FederationError
	if !errors.As(target, &typed) {
		return false
	}
	return e.Code == typed.Code
}

func federationError(code FederationErrorCode, format string, args ...any) error {
	msg := ""
	if format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	return &FederationError{Code: code, Message: msg}
}

type AgentScopeGrant struct {
	SchemaVersion              string   `json:"schema_version"`
	RecordVersion              int      `json:"record_version"`
	AgentScopeGrantID          string   `json:"agent_scope_grant_id"`
	ProjectID                  string   `json:"project_id"`
	DeliveryRunID              string   `json:"delivery_run_id"`
	ChildAgentID               string   `json:"child_agent_id"`
	ReadScope                  []string `json:"read_scope"`
	WriteScope                 []string `json:"write_scope"`
	PathScope                  []string `json:"path_scope"`
	RepositoryScope            []string `json:"repository_scope"`
	WorktreeScope              []string `json:"worktree_scope"`
	CommandScope               []string `json:"command_scope"`
	NetworkScope               []string `json:"network_scope"`
	CredentialScope            []string `json:"credential_scope"`
	SideEffectScope            []string `json:"side_effect_scope"`
	ApprovalScope              []string `json:"approval_scope"`
	Permission                 string   `json:"permission"`
	SideEffectClass            string   `json:"side_effect_class"`
	PolicyVersion              string   `json:"policy_version"`
	PolicyFingerprint          string   `json:"policy_fingerprint"`
	PlanFingerprint            string   `json:"plan_fingerprint"`
	AuthorizationFingerprint   string   `json:"authorization_fingerprint,omitempty"`
	AgentFederationFingerprint string   `json:"agent_federation_fingerprint"`
	CreatedAt                  string   `json:"created_at"`
	UpdatedAt                  string   `json:"updated_at"`
	TerminalErrorCode          string   `json:"terminal_error_code,omitempty"`
}

type AgentOwnershipLock struct {
	SchemaVersion        string   `json:"schema_version"`
	AgentOwnershipLockID string   `json:"agent_ownership_lock_id"`
	ProjectID            string   `json:"project_id"`
	DeliveryRunID        string   `json:"delivery_run_id"`
	ChildAgentID         string   `json:"child_agent_id"`
	RunID                string   `json:"run_id"`
	ClaimGeneration      int64    `json:"claim_generation"`
	LockGeneration       int64    `json:"lock_generation"`
	ResourceKind         string   `json:"resource_kind"`
	ResourceKey          string   `json:"resource_key"`
	LockMode             string   `json:"lock_mode"`
	State                string   `json:"state"`
	LeaseExpiresAt       string   `json:"lease_expires_at"`
	HeartbeatAt          string   `json:"heartbeat_at"`
	ConflictsWith        []string `json:"conflicts_with"`
	CreatedAt            string   `json:"created_at"`
	UpdatedAt            string   `json:"updated_at"`
}

type AgentBudgetBinding struct {
	SchemaVersion          string `json:"schema_version"`
	AgentBudgetBindingID   string `json:"agent_budget_binding_id"`
	ProjectID              string `json:"project_id"`
	DeliveryRunID          string `json:"delivery_run_id"`
	ChildAgentID           string `json:"child_agent_id"`
	BudgetPolicyID         string `json:"budget_policy_id"`
	BudgetReservationID    string `json:"budget_reservation_id"`
	ReservationScope       string `json:"reservation_scope"`
	ReservedQuantitiesJSON string `json:"reserved_quantities_json"`
	AncestorBudgetRefsJSON string `json:"ancestor_budget_refs_json"`
	ReservationState       string `json:"reservation_state"`
	CreatedAt              string `json:"created_at"`
	UpdatedAt              string `json:"updated_at"`
}

type AgentRegistration struct {
	SchemaVersion              string   `json:"schema_version"`
	RecordVersion              int      `json:"record_version"`
	ChildAgentID               string   `json:"child_agent_id"`
	ParentAgentID              string   `json:"parent_agent_id,omitempty"`
	ProjectID                  string   `json:"project_id"`
	DeliveryRunID              string   `json:"delivery_run_id"`
	RootRunID                  string   `json:"root_run_id"`
	ParentRunID                string   `json:"parent_run_id"`
	RunID                      string   `json:"run_id"`
	Depth                      int      `json:"depth"`
	TaskID                     string   `json:"task_id"`
	AttemptID                  string   `json:"attempt_id"`
	PlanID                     string   `json:"plan_id"`
	ChildKey                   string   `json:"child_key"`
	AdapterID                  string   `json:"adapter_id"`
	ProviderInstallationID     string   `json:"provider_installation_id,omitempty"`
	AccountProfileID           string   `json:"account_profile_id,omitempty"`
	ModelCapabilityID          string   `json:"model_capability_id,omitempty"`
	RoutingDecisionID          string   `json:"routing_decision_id,omitempty"`
	ProviderSessionRef         string   `json:"provider_session_ref,omitempty"`
	ScopeGrantID               string   `json:"scope_grant_id"`
	Permission                 string   `json:"permission"`
	SideEffectClass            string   `json:"side_effect_class"`
	BudgetBindingIDs           []string `json:"budget_binding_ids"`
	OwnershipLockIDs           []string `json:"ownership_lock_ids"`
	ClaimGeneration            int64    `json:"claim_generation"`
	ExecutorID                 string   `json:"executor_id"`
	ProviderIDempotencyKey     string   `json:"provider_idempotency_key"`
	ProviderReceipt            string   `json:"provider_receipt,omitempty"`
	CancellationChannel        string   `json:"cancellation_channel"`
	ExpectedOutputsJSON        string   `json:"expected_outputs_json"`
	RegistrationState          string   `json:"registration_state"`
	PolicyVersion              string   `json:"policy_version"`
	PlanFingerprint            string   `json:"plan_fingerprint"`
	PolicyFingerprint          string   `json:"policy_fingerprint"`
	AuthorizationFingerprint   string   `json:"authorization_fingerprint,omitempty"`
	AgentFederationFingerprint string   `json:"agent_federation_fingerprint"`
	RegistrationPayloadHash    string   `json:"registration_payload_hash"`
	Classification             string   `json:"classification"`
	GapReasons                 []string `json:"gap_reasons"`
	CreatedAt                  string   `json:"created_at"`
	UpdatedAt                  string   `json:"updated_at"`
	TerminalErrorCode          string   `json:"terminal_error_code,omitempty"`
}

type AgentRegistrationRequest struct {
	ProjectID                string
	DeliveryRunID            string
	RootRunID                string
	ParentRunID              string
	RunID                    string
	Depth                    int
	ParentAgentID            string
	TaskID                   string
	AttemptID                string
	PlanID                   string
	ChildKey                 string
	AdapterID                string
	ProviderInstallationID   string
	AccountProfileID         string
	ModelCapabilityID        string
	RoutingDecisionID        string
	ProviderSessionRef       string
	Scope                    AgentScopeGrant
	ParentScope              *AgentScopeGrant
	Permission               string
	SideEffectClass          string
	BudgetBindings           []AgentBudgetBinding
	OwnershipLocks           []AgentOwnershipLock
	ClaimGeneration          int64
	ExecutorID               string
	ProviderIDempotencyKey   string
	CancellationChannel      string
	ExpectedOutputsJSON      string
	PlanFingerprint          string
	PolicyFingerprint        string
	AuthorizationFingerprint string
	Classification           string
	GapReasons               []string
	CreatedAt                string
	CapabilityEvidence       ProviderCapabilityEvidence
}

type ProviderCapabilityEvidence struct {
	AdapterID            string   `json:"adapter_id"`
	NestedSubagents      bool     `json:"nested_subagents"`
	Cancellation         bool     `json:"cancellation"`
	CapabilityConfidence string   `json:"capability_confidence"`
	FreshnessState       string   `json:"freshness_state"`
	GapReasons           []string `json:"gap_reasons"`
}

type ChildOutputEnvelope struct {
	SchemaVersion  string   `json:"schema_version"`
	Status         string   `json:"status"`
	Classification string   `json:"classification"`
	Accepted       bool     `json:"accepted"`
	Truncated      bool     `json:"truncated"`
	Redacted       bool     `json:"redacted"`
	Output         string   `json:"output"`
	GapReasons     []string `json:"gap_reasons"`
}

type AgentTree struct {
	SchemaVersion              string                  `json:"schema_version"`
	RootRunID                  string                  `json:"root_run_id"`
	AgentFederationFingerprint string                  `json:"agent_federation_fingerprint"`
	Registrations              []AgentTreeRegistration `json:"registrations"`
	Blocked                    []string                `json:"blocked"`
	NeedsHuman                 []string                `json:"needs_human"`
}

type AgentTreeRegistration struct {
	ChildAgentID          string   `json:"child_agent_id"`
	ParentAgentID         *string  `json:"parent_agent_id"`
	ParentRunID           string   `json:"parent_run_id"`
	RunID                 string   `json:"run_id"`
	TaskID                string   `json:"task_id"`
	AttemptID             string   `json:"attempt_id"`
	AdapterID             string   `json:"adapter_id"`
	RegistrationState     string   `json:"registration_state"`
	ScopeGrantID          string   `json:"scope_grant_id"`
	BudgetReservationIDs  []string `json:"budget_reservation_ids"`
	OwnershipLockIDs      []string `json:"ownership_lock_ids"`
	ClaimGeneration       int64    `json:"claim_generation"`
	GapReasons            []string `json:"gap_reasons"`
	FederationFingerprint string   `json:"agent_federation_fingerprint"`
	TerminalErrorCode     string   `json:"terminal_error_code,omitempty"`
}

type federationFingerprintInput struct {
	SchemaVersion            string   `json:"schema_version"`
	ProjectID                string   `json:"project_id"`
	DeliveryRunID            string   `json:"delivery_run_id"`
	RootRunID                string   `json:"root_run_id"`
	ParentRunID              string   `json:"parent_run_id"`
	RunID                    string   `json:"run_id"`
	ParentAgentID            string   `json:"parent_agent_id,omitempty"`
	TaskID                   string   `json:"task_id"`
	AttemptID                string   `json:"attempt_id"`
	PlanID                   string   `json:"plan_id"`
	ChildKey                 string   `json:"child_key"`
	AdapterID                string   `json:"adapter_id"`
	ProviderInstallationID   string   `json:"provider_installation_id,omitempty"`
	AccountProfileID         string   `json:"account_profile_id,omitempty"`
	ModelCapabilityID        string   `json:"model_capability_id,omitempty"`
	RoutingDecisionID        string   `json:"routing_decision_id,omitempty"`
	ProviderSessionRef       string   `json:"provider_session_ref,omitempty"`
	ScopeCanonicalJSON       string   `json:"scope_canonical_json"`
	Permission               string   `json:"permission"`
	SideEffectClass          string   `json:"side_effect_class"`
	BudgetBindingIDs         []string `json:"budget_binding_ids"`
	OwnershipLockIDs         []string `json:"ownership_lock_ids"`
	ClaimGeneration          int64    `json:"claim_generation"`
	ExecutorID               string   `json:"executor_id"`
	ProviderIDempotencyKey   string   `json:"provider_idempotency_key"`
	CancellationChannel      string   `json:"cancellation_channel"`
	ExpectedOutputsHash      string   `json:"expected_outputs_hash"`
	PlanFingerprint          string   `json:"plan_fingerprint"`
	PolicyFingerprint        string   `json:"policy_fingerprint"`
	AuthorizationFingerprint string   `json:"authorization_fingerprint,omitempty"`
}

type registrationCanonicalPayload struct {
	FingerprintInput  federationFingerprintInput `json:"fingerprint_input"`
	RegistrationState string                     `json:"registration_state"`
	Classification    string                     `json:"classification"`
	GapReasons        []string                   `json:"gap_reasons"`
}

type preparedAgentRegistration struct {
	record   AgentRegistration
	scope    AgentScopeGrant
	bindings []AgentBudgetBinding
	locks    []AgentOwnershipLock
}

func ValidateProviderNativeSubagent(evidence ProviderCapabilityEvidence) error {
	adapterID := strings.ToLower(strings.TrimSpace(evidence.AdapterID))
	if adapterID == "" {
		return federationError(ErrUnsupportedNativeSubAgentCode, "adapter_id is required")
	}
	if adapterID == "codex" || adapterID == "gemini" || adapterID == "antigravity" {
		return federationError(ErrUnsupportedNativeSubAgentCode, "%s does not support native sub-agents in the live matrix", adapterID)
	}
	if !evidence.NestedSubagents {
		return federationError(ErrUnsupportedNativeSubAgentCode, "%s lacks nested_subagents evidence", adapterID)
	}
	switch strings.ToLower(strings.TrimSpace(evidence.FreshnessState)) {
	case "", "fresh", "current", "not-applicable":
	default:
		return federationError(ErrUnsupportedNativeSubAgentCode, "%s capability evidence is %s", adapterID, evidence.FreshnessState)
	}
	if strings.EqualFold(evidence.CapabilityConfidence, "unknown") || strings.EqualFold(evidence.CapabilityConfidence, "unavailable") {
		return federationError(ErrUnsupportedNativeSubAgentCode, "%s capability confidence is %s", adapterID, evidence.CapabilityConfidence)
	}
	return nil
}

func RegisterAgent(ctx context.Context, store Store, req AgentRegistrationRequest) (AgentRegistration, error) {
	if store == nil {
		return AgentRegistration{}, federationError(ErrAgentRegistrationRequiredCode, "store is required")
	}
	req = normalizeRegistrationRequest(req)
	prepared, err := prepareAgentRegistration(req)
	if err != nil {
		return AgentRegistration{}, err
	}
	record := prepared.record
	err = withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			inserted, replay, err := insertPreparedAgentRegistrationTx(ctx, tx, prepared, store.Now())
			if err != nil {
				return err
			}
			if replay {
				record = inserted
			}
			return nil
		})
	})
	return record, err
}

func ClaimAndStartNativeChild(ctx context.Context, store Store, parentRunID, childRunID, executorID string, now, leaseUntil time.Time, req AgentRegistrationRequest) (ClaimResult, AgentRegistration, error) {
	if store == nil {
		return ClaimResult{}, AgentRegistration{}, federationError(ErrAgentRegistrationRequiredCode, "store is required")
	}
	parentRunID = strings.TrimSpace(parentRunID)
	childRunID = strings.TrimSpace(childRunID)
	executorID = strings.TrimSpace(executorID)
	now = now.UTC()
	leaseUntil = leaseUntil.UTC()
	if parentRunID == "" || childRunID == "" || executorID == "" || now.IsZero() || leaseUntil.IsZero() || !leaseUntil.After(now) {
		return ClaimResult{}, AgentRegistration{}, federationError(ErrInvalidRecordCode, "valid parent, child, executor, now, and lease_until are required")
	}
	req = normalizeRegistrationRequest(req)
	req.ParentRunID = parentRunID
	req.RunID = childRunID
	req.ExecutorID = executorID
	req.CreatedAt = formatTimestamp(now)
	var claim ClaimResult
	var record AgentRegistration
	err := withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			txClaim, err := claimChildRunExecutionTx(ctx, tx, parentRunID, childRunID, executorID, formatTimestamp(now), formatTimestamp(leaseUntil))
			if err != nil {
				return err
			}
			claim = txClaim
			switch txClaim.Outcome {
			case ClaimOutcomeClaimed, ClaimOutcomeStaleClaim:
			default:
				return nil
			}
			req.ClaimGeneration = txClaim.ClaimGeneration
			req.ExecutorID = txClaim.ExecutorID
			req.ProviderIDempotencyKey = txClaim.ProviderKey
			for i := range req.OwnershipLocks {
				req.OwnershipLocks[i].LeaseExpiresAt = txClaim.LeaseExpiresAt
			}
			prepared, err := prepareAgentRegistration(req)
			if err != nil {
				return err
			}
			inserted, replay, err := insertPreparedAgentRegistrationTx(ctx, tx, prepared, now)
			if err != nil {
				return err
			}
			if replay {
				record = inserted
			} else {
				record = prepared.record
			}
			if record.RegistrationState != AgentStateRegistered {
				return federationError(ErrAgentRegistrationRequiredCode, "registration %s is %s, want registered", record.ChildAgentID, record.RegistrationState)
			}
			if err := validateActiveBudgetBindingsTx(ctx, tx, record, now); err != nil {
				return err
			}
			if err := validateActiveOwnershipLocksTx(ctx, tx, record, now); err != nil {
				return err
			}
			if err := transitionAgentRegistrationTx(ctx, tx, &record, AgentActionLaunch, txClaim.ExecutorID, txClaim.ClaimGeneration, formatTimestamp(now)); err != nil {
				return err
			}
			if err := updateChildRunClaimPhaseTx(ctx, tx, parentRunID, childRunID, txClaim.ExecutorID, txClaim.ClaimGeneration, ClaimPhaseExecuting, formatTimestamp(now), ""); err != nil {
				return err
			}
			if err := transitionAgentRegistrationTx(ctx, tx, &record, AgentActionHeartbeat, txClaim.ExecutorID, txClaim.ClaimGeneration, formatTimestamp(now)); err != nil {
				return err
			}
			claim.ClaimPhase = ClaimPhaseExecuting
			claim.RunStatus = "running"
			claim.EdgeStatus = "running"
			return nil
		})
	})
	return claim, record, err
}

func prepareAgentRegistration(req AgentRegistrationRequest) (preparedAgentRegistration, error) {
	if err := validateRegistrationRequest(req); err != nil {
		return preparedAgentRegistration{}, err
	}
	if err := ValidateProviderNativeSubagent(req.CapabilityEvidence); err != nil {
		return preparedAgentRegistration{}, err
	}
	childAgentID := stableID("agent_", req.ProjectID, req.DeliveryRunID, req.ParentRunID, req.TaskID, req.AttemptID, req.ChildKey, req.PlanFingerprint)
	scope := req.Scope
	scope.SchemaVersion = AgentScopeGrantSchema
	scope.RecordVersion = 1
	scope.ProjectID = req.ProjectID
	scope.DeliveryRunID = req.DeliveryRunID
	scope.ChildAgentID = childAgentID
	scope.Permission = req.Permission
	scope.SideEffectClass = req.SideEffectClass
	scope.PolicyVersion = firstNonEmptyAgent(scope.PolicyVersion, AgentPolicyVersion)
	scope.PolicyFingerprint = req.PolicyFingerprint
	scope.PlanFingerprint = req.PlanFingerprint
	scope.AuthorizationFingerprint = req.AuthorizationFingerprint
	scope.CreatedAt = req.CreatedAt
	scope.UpdatedAt = req.CreatedAt
	if err := canonicalizeScope(&scope); err != nil {
		return preparedAgentRegistration{}, err
	}
	if req.ParentScope != nil {
		parentScope := *req.ParentScope
		if err := canonicalizeScope(&parentScope); err != nil {
			return preparedAgentRegistration{}, err
		}
		if err := ValidateScopeInheritance(parentScope, scope); err != nil {
			return preparedAgentRegistration{}, err
		}
	}
	scopeBytes, err := json.Marshal(scopePayload(scope))
	if err != nil {
		return preparedAgentRegistration{}, federationError(ErrInvalidRecordCode, "marshal scope: %v", err)
	}
	scope.AgentScopeGrantID = stableID("ascope_", childAgentID, string(scopeBytes), req.PolicyFingerprint)
	budgetIDs := make([]string, 0, len(req.BudgetBindings))
	for i := range req.BudgetBindings {
		req.BudgetBindings[i] = normalizeBudgetBinding(req.BudgetBindings[i], req, childAgentID)
		budgetIDs = append(budgetIDs, req.BudgetBindings[i].AgentBudgetBindingID)
	}
	lockIDs := make([]string, 0, len(req.OwnershipLocks))
	for i := range req.OwnershipLocks {
		req.OwnershipLocks[i] = normalizeOwnershipLock(req.OwnershipLocks[i], req, childAgentID)
		lockIDs = append(lockIDs, req.OwnershipLocks[i].AgentOwnershipLockID)
	}
	fingerprintInput := federationFingerprintInput{
		SchemaVersion:            AgentRegistrationSchema,
		ProjectID:                req.ProjectID,
		DeliveryRunID:            req.DeliveryRunID,
		RootRunID:                req.RootRunID,
		ParentRunID:              req.ParentRunID,
		RunID:                    req.RunID,
		ParentAgentID:            req.ParentAgentID,
		TaskID:                   req.TaskID,
		AttemptID:                req.AttemptID,
		PlanID:                   req.PlanID,
		ChildKey:                 req.ChildKey,
		AdapterID:                req.AdapterID,
		ProviderInstallationID:   req.ProviderInstallationID,
		AccountProfileID:         req.AccountProfileID,
		ModelCapabilityID:        req.ModelCapabilityID,
		RoutingDecisionID:        req.RoutingDecisionID,
		ProviderSessionRef:       req.ProviderSessionRef,
		ScopeCanonicalJSON:       string(scopeBytes),
		Permission:               req.Permission,
		SideEffectClass:          req.SideEffectClass,
		BudgetBindingIDs:         sortedCopyAgent(budgetIDs),
		OwnershipLockIDs:         sortedCopyAgent(lockIDs),
		ClaimGeneration:          req.ClaimGeneration,
		ExecutorID:               req.ExecutorID,
		ProviderIDempotencyKey:   req.ProviderIDempotencyKey,
		CancellationChannel:      req.CancellationChannel,
		ExpectedOutputsHash:      hashString(req.ExpectedOutputsJSON),
		PlanFingerprint:          req.PlanFingerprint,
		PolicyFingerprint:        req.PolicyFingerprint,
		AuthorizationFingerprint: req.AuthorizationFingerprint,
	}
	fingerprint := digestJSON(fingerprintInput)
	scope.AgentFederationFingerprint = fingerprint
	payloadHash := digestJSON(registrationCanonicalPayload{
		FingerprintInput:  fingerprintInput,
		RegistrationState: AgentStateRegistered,
		Classification:    req.Classification,
		GapReasons:        sortedCopyAgent(req.GapReasons),
	})
	record := AgentRegistration{
		SchemaVersion:              AgentRegistrationSchema,
		RecordVersion:              1,
		ChildAgentID:               childAgentID,
		ParentAgentID:              req.ParentAgentID,
		ProjectID:                  req.ProjectID,
		DeliveryRunID:              req.DeliveryRunID,
		RootRunID:                  req.RootRunID,
		ParentRunID:                req.ParentRunID,
		RunID:                      req.RunID,
		Depth:                      req.Depth,
		TaskID:                     req.TaskID,
		AttemptID:                  req.AttemptID,
		PlanID:                     req.PlanID,
		ChildKey:                   req.ChildKey,
		AdapterID:                  req.AdapterID,
		ProviderInstallationID:     req.ProviderInstallationID,
		AccountProfileID:           req.AccountProfileID,
		ModelCapabilityID:          req.ModelCapabilityID,
		RoutingDecisionID:          req.RoutingDecisionID,
		ProviderSessionRef:         req.ProviderSessionRef,
		ScopeGrantID:               scope.AgentScopeGrantID,
		Permission:                 req.Permission,
		SideEffectClass:            req.SideEffectClass,
		BudgetBindingIDs:           sortedCopyAgent(budgetIDs),
		OwnershipLockIDs:           sortedCopyAgent(lockIDs),
		ClaimGeneration:            req.ClaimGeneration,
		ExecutorID:                 req.ExecutorID,
		ProviderIDempotencyKey:     req.ProviderIDempotencyKey,
		CancellationChannel:        req.CancellationChannel,
		ExpectedOutputsJSON:        req.ExpectedOutputsJSON,
		RegistrationState:          AgentStateRegistered,
		PolicyVersion:              AgentPolicyVersion,
		PlanFingerprint:            req.PlanFingerprint,
		PolicyFingerprint:          req.PolicyFingerprint,
		AuthorizationFingerprint:   req.AuthorizationFingerprint,
		AgentFederationFingerprint: fingerprint,
		RegistrationPayloadHash:    payloadHash,
		Classification:             req.Classification,
		GapReasons:                 sortedCopyAgent(req.GapReasons),
		CreatedAt:                  req.CreatedAt,
		UpdatedAt:                  req.CreatedAt,
	}
	return preparedAgentRegistration{record: record, scope: scope, bindings: req.BudgetBindings, locks: req.OwnershipLocks}, nil
}

func TransitionAgentRegistration(ctx context.Context, store Store, childAgentID, action, actorExecutorID string, claimGeneration int64, at string) (AgentRegistration, error) {
	if store == nil {
		return AgentRegistration{}, federationError(ErrAgentRegistrationRequiredCode, "store is required")
	}
	childAgentID = strings.TrimSpace(childAgentID)
	action = strings.TrimSpace(action)
	if childAgentID == "" || action == "" {
		return AgentRegistration{}, federationError(ErrInvalidRecordCode, "child_agent_id and action are required")
	}
	if strings.TrimSpace(at) == "" {
		at = formatTimestamp(time.Now().UTC())
	}
	var out AgentRegistration
	err := withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			record, ok, err := loadAgentRegistrationTx(ctx, tx, childAgentID)
			if err != nil {
				return err
			}
			if !ok {
				return federationError(ErrAgentRegistrationRequiredCode, "registration %s is missing", childAgentID)
			}
			if err := transitionAgentRegistrationTx(ctx, tx, &record, action, actorExecutorID, claimGeneration, at); err != nil {
				return err
			}
			out = record
			return nil
		})
	})
	return out, err
}

func ValidateNativeChildLaunch(ctx context.Context, store Store, childRunID, executorID string, claimGeneration int64) (AgentRegistration, error) {
	if store == nil {
		return AgentRegistration{}, federationError(ErrAgentRegistrationRequiredCode, "store is required")
	}
	var record AgentRegistration
	err := store.WithTx(ctx, func(tx Tx) error {
		found, ok, err := loadAgentRegistrationByRunTx(ctx, tx, strings.TrimSpace(childRunID))
		if err != nil {
			return err
		}
		if !ok {
			return federationError(ErrAgentRegistrationRequiredCode, "run %s has no registration", childRunID)
		}
		record = found
		if record.RegistrationState != AgentStateRegistered {
			return federationError(ErrAgentRegistrationRequiredCode, "registration %s is %s, want registered", record.ChildAgentID, record.RegistrationState)
		}
		if len(record.BudgetBindingIDs) == 0 {
			return federationError(ErrChildBudgetRequiredCode, "registration %s has no budget binding", record.ChildAgentID)
		}
		if err := validateActiveBudgetBindingsTx(ctx, tx, record, store.Now()); err != nil {
			return err
		}
		if record.Permission != PermissionReadOnly && len(record.OwnershipLockIDs) == 0 {
			return federationError(ErrOwnershipRequiredCode, "registration %s has no ownership locks", record.ChildAgentID)
		}
		if err := validateRegistrationClaimTx(ctx, tx, record, executorID, claimGeneration); err != nil {
			return err
		}
		if err := validateActiveOwnershipLocksTx(ctx, tx, record, store.Now()); err != nil {
			return err
		}
		return nil
	})
	return record, err
}

func LoadAgentTree(ctx context.Context, store Store, projectID, rootRunID string) (AgentTree, error) {
	tree := AgentTree{SchemaVersion: AgentTreeSchema, RootRunID: strings.TrimSpace(rootRunID), Registrations: []AgentTreeRegistration{}, Blocked: []string{}, NeedsHuman: []string{}}
	if store == nil {
		tree.AgentFederationFingerprint = digestJSON(struct {
			SchemaVersion string   `json:"schema_version"`
			LeafHashes    []string `json:"leaf_hashes"`
		}{AgentTreeSchema, nil})
		return tree, nil
	}
	projectID = strings.TrimSpace(projectID)
	rootRunID = strings.TrimSpace(rootRunID)
	if rootRunID == "" {
		return tree, nil
	}
	err := store.WithTx(ctx, func(tx Tx) error {
		rows, err := tx.Query(ctx, `SELECT
				id, parent_agent_id, parent_run_id, child_run_id, task_id, attempt_id,
				adapter_id, registration_state, scope_grant_id, budget_binding_ids_json,
				ownership_lock_ids_json, claim_generation, gap_reasons_json,
				agent_federation_fingerprint, terminal_error_code
			FROM agent_registrations
			WHERE root_run_id = ? AND (? = '' OR project_id = ?)
			ORDER BY depth, parent_run_id, child_key, id`, rootRunID, projectID, projectID)
		if err != nil {
			return fmt.Errorf("load agent tree: %w", err)
		}
		defer rows.Close()
		leafHashes := []string{}
		for rows.Next() {
			var reg AgentTreeRegistration
			var parentAgentID, budgetsJSON, locksJSON, gapsJSON string
			if err := rows.Scan(
				&reg.ChildAgentID,
				&parentAgentID,
				&reg.ParentRunID,
				&reg.RunID,
				&reg.TaskID,
				&reg.AttemptID,
				&reg.AdapterID,
				&reg.RegistrationState,
				&reg.ScopeGrantID,
				&budgetsJSON,
				&locksJSON,
				&reg.ClaimGeneration,
				&gapsJSON,
				&reg.FederationFingerprint,
				&reg.TerminalErrorCode,
			); err != nil {
				return fmt.Errorf("load agent tree row: %w", err)
			}
			if strings.TrimSpace(parentAgentID) != "" {
				value := strings.TrimSpace(parentAgentID)
				reg.ParentAgentID = &value
			}
			reg.BudgetReservationIDs = decodeStringList(budgetsJSON)
			reg.OwnershipLockIDs = decodeStringList(locksJSON)
			reg.GapReasons = decodeStringList(gapsJSON)
			tree.Registrations = append(tree.Registrations, reg)
			leafHashes = append(leafHashes, reg.FederationFingerprint)
			switch normalizeAgentRegistrationState(reg.RegistrationState) {
			case AgentStateNeedsHuman:
				tree.NeedsHuman = append(tree.NeedsHuman, reg.ChildAgentID)
			case AgentStateFailed, AgentStateCancelled, AgentStateSuperseded:
				tree.Blocked = append(tree.Blocked, reg.ChildAgentID)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("load agent tree rows: %w", err)
		}
		sort.Strings(leafHashes)
		tree.AgentFederationFingerprint = digestJSON(struct {
			SchemaVersion string   `json:"schema_version"`
			LeafHashes    []string `json:"leaf_hashes"`
		}{AgentTreeSchema, leafHashes})
		return nil
	})
	return tree, err
}

func NormalizeChildOutput(raw string, status string, maxBytes int) (ChildOutputEnvelope, error) {
	normalizedStatus, ok := normalizeClosedChildStatus(status)
	if !ok {
		return ChildOutputEnvelope{}, federationError(ErrInvalidRecordCode, "unknown child status %q", status)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultChildOutputLimit
	}
	output, didRedact := redactSecretLike(raw)
	truncated := false
	if len([]byte(output)) > maxBytes {
		output = string([]byte(output)[:maxBytes])
		truncated = true
	}
	return ChildOutputEnvelope{
		SchemaVersion:  "loopcoder.child_output.v1",
		Status:         normalizedStatus,
		Classification: "provider-output-untrusted",
		Accepted:       false,
		Truncated:      truncated,
		Redacted:       didRedact,
		Output:         output,
		GapReasons:     []string{"untrusted-until-accepted"},
	}, nil
}

func ValidateScopeInheritance(parent, child AgentScopeGrant) error {
	if permissionRank(child.Permission) > permissionRank(parent.Permission) {
		return federationError(ErrScopeWideningCode, "permission %s exceeds parent %s", child.Permission, parent.Permission)
	}
	for _, value := range child.CredentialScope {
		if strings.TrimSpace(value) != "" && strings.TrimSpace(value) != "none" {
			return federationError(ErrCredentialScopeDeniedCode, "credential material scope is forbidden")
		}
	}
	dimensions := []struct {
		name   string
		parent []string
		child  []string
	}{
		{"read_scope", parent.ReadScope, child.ReadScope},
		{"write_scope", parent.WriteScope, child.WriteScope},
		{"path_scope", parent.PathScope, child.PathScope},
		{"repository_scope", parent.RepositoryScope, child.RepositoryScope},
		{"worktree_scope", parent.WorktreeScope, child.WorktreeScope},
		{"command_scope", parent.CommandScope, child.CommandScope},
		{"network_scope", parent.NetworkScope, child.NetworkScope},
		{"side_effect_scope", parent.SideEffectScope, child.SideEffectScope},
		{"approval_scope", parent.ApprovalScope, child.ApprovalScope},
	}
	for _, dimension := range dimensions {
		if err := validateScopeSubset(dimension.name, dimension.parent, dimension.child); err != nil {
			return err
		}
	}
	return nil
}

func nextAgentRegistrationState(from, action string) (string, error) {
	from = normalizeAgentRegistrationState(from)
	action = strings.TrimSpace(action)
	if from == "" || action == "" {
		return "", federationError(ErrInvalidTransitionCode, "state and action are required")
	}
	terminal := map[string]bool{
		AgentStateSucceeded:  true,
		AgentStateFailed:     true,
		AgentStateCancelled:  true,
		AgentStateNeedsHuman: true,
		AgentStateSuperseded: true,
	}
	table := map[string]map[string]string{
		AgentStatePlanned: {
			AgentActionRegister:        AgentStateRegistered,
			AgentActionCancel:          AgentStateCancelled,
			AgentActionCompleteFailure: AgentStateFailed,
			AgentActionRecoverTakeover: AgentStatePlanned,
			AgentActionSupersede:       AgentStateSuperseded,
			AgentActionInvalidate:      AgentStateNeedsHuman,
		},
		AgentStateRegistered: {
			AgentActionRegister:        AgentStateRegistered,
			AgentActionLaunch:          AgentStateLaunching,
			AgentActionCancel:          AgentStateCancelled,
			AgentActionCompleteFailure: AgentStateFailed,
			AgentActionRecoverTakeover: AgentStateRegistered,
			AgentActionSupersede:       AgentStateSuperseded,
			AgentActionInvalidate:      AgentStateNeedsHuman,
		},
		AgentStateLaunching: {
			AgentActionLaunch:          AgentStateLaunching,
			AgentActionHeartbeat:       AgentStateRunning,
			AgentActionCancel:          AgentStateCancelling,
			AgentActionCompleteFailure: AgentStateFailed,
			AgentActionRecoverTakeover: AgentStateNeedsHuman,
			AgentActionInvalidate:      AgentStateNeedsHuman,
		},
		AgentStateRunning: {
			AgentActionHeartbeat:       AgentStateRunning,
			AgentActionCancel:          AgentStateCancelling,
			AgentActionCompleteSuccess: AgentStateSucceeded,
			AgentActionCompleteFailure: AgentStateFailed,
			AgentActionRecoverTakeover: AgentStateNeedsHuman,
			AgentActionInvalidate:      AgentStateNeedsHuman,
		},
		AgentStateCancelling: {
			AgentActionHeartbeat:       AgentStateCancelling,
			AgentActionCancel:          AgentStateCancelling,
			AgentActionCompleteFailure: AgentStateCancelled,
			AgentActionRecoverTakeover: AgentStateNeedsHuman,
			AgentActionInvalidate:      AgentStateNeedsHuman,
		},
		AgentStateSucceeded:  {AgentActionCompleteSuccess: AgentStateSucceeded},
		AgentStateFailed:     {AgentActionCompleteFailure: AgentStateFailed},
		AgentStateCancelled:  {AgentActionCancel: AgentStateCancelled},
		AgentStateNeedsHuman: {AgentActionInvalidate: AgentStateNeedsHuman},
		AgentStateSuperseded: {AgentActionSupersede: AgentStateSuperseded},
	}
	if next, ok := table[from][action]; ok {
		return next, nil
	}
	if terminal[from] {
		return "", federationError(ErrTerminalStateCode, "%s cannot handle %s", from, action)
	}
	return "", federationError(ErrInvalidTransitionCode, "%s cannot handle %s", from, action)
}

func normalizeRegistrationRequest(req AgentRegistrationRequest) AgentRegistrationRequest {
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.DeliveryRunID = strings.TrimSpace(req.DeliveryRunID)
	req.RootRunID = strings.TrimSpace(req.RootRunID)
	req.ParentRunID = strings.TrimSpace(req.ParentRunID)
	req.RunID = strings.TrimSpace(req.RunID)
	req.ParentAgentID = strings.TrimSpace(req.ParentAgentID)
	req.TaskID = strings.TrimSpace(req.TaskID)
	req.AttemptID = strings.TrimSpace(req.AttemptID)
	req.PlanID = strings.TrimSpace(req.PlanID)
	req.ChildKey = strings.TrimSpace(req.ChildKey)
	req.AdapterID = strings.ToLower(strings.TrimSpace(req.AdapterID))
	req.ProviderInstallationID = strings.TrimSpace(req.ProviderInstallationID)
	req.AccountProfileID = strings.TrimSpace(req.AccountProfileID)
	req.ModelCapabilityID = strings.TrimSpace(req.ModelCapabilityID)
	req.RoutingDecisionID = strings.TrimSpace(req.RoutingDecisionID)
	req.ProviderSessionRef = strings.TrimSpace(req.ProviderSessionRef)
	req.Permission = normalizePermission(req.Permission)
	req.SideEffectClass = strings.TrimSpace(req.SideEffectClass)
	req.ProviderIDempotencyKey = strings.TrimSpace(req.ProviderIDempotencyKey)
	req.CancellationChannel = strings.TrimSpace(req.CancellationChannel)
	req.ExpectedOutputsJSON = strings.TrimSpace(req.ExpectedOutputsJSON)
	req.PlanFingerprint = strings.TrimSpace(req.PlanFingerprint)
	req.PolicyFingerprint = strings.TrimSpace(req.PolicyFingerprint)
	req.AuthorizationFingerprint = strings.TrimSpace(req.AuthorizationFingerprint)
	req.Classification = strings.TrimSpace(req.Classification)
	if req.Classification == "" {
		req.Classification = "local-diagnostic"
	}
	req.GapReasons = sortedCopyAgent(req.GapReasons)
	if req.CreatedAt == "" {
		req.CreatedAt = formatTimestamp(time.Now().UTC())
	}
	return req
}

func validateRegistrationRequest(req AgentRegistrationRequest) error {
	required := map[string]string{
		"project_id": req.ProjectID, "delivery_run_id": req.DeliveryRunID, "root_run_id": req.RootRunID,
		"parent_run_id": req.ParentRunID, "run_id": req.RunID, "task_id": req.TaskID,
		"attempt_id": req.AttemptID, "plan_id": req.PlanID, "child_key": req.ChildKey,
		"adapter_id": req.AdapterID, "permission": req.Permission, "side_effect_class": req.SideEffectClass,
		"executor_id": req.ExecutorID, "provider_idempotency_key": req.ProviderIDempotencyKey,
		"cancellation_channel": req.CancellationChannel, "expected_outputs_json": req.ExpectedOutputsJSON,
		"plan_fingerprint": req.PlanFingerprint, "policy_fingerprint": req.PolicyFingerprint,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return federationError(ErrInvalidRecordCode, "%s is required", field)
		}
	}
	if !validSideEffectClass(req.SideEffectClass) {
		return federationError(ErrInvalidRecordCode, "side_effect_class %q is invalid", req.SideEffectClass)
	}
	if req.ParentRunID == req.RunID || req.RootRunID == req.RunID {
		return federationError(ErrInvalidRecordCode, "child run cannot equal parent/root run")
	}
	if req.ParentAgentID == stableID("agent_", req.ProjectID, req.DeliveryRunID, req.ParentRunID, req.TaskID, req.AttemptID, req.ChildKey, req.PlanFingerprint) {
		return federationError(ErrInvalidRecordCode, "child agent cannot parent itself")
	}
	if req.ClaimGeneration <= 0 {
		return federationError(ErrInvalidRecordCode, "claim_generation must be positive")
	}
	if !json.Valid([]byte(req.ExpectedOutputsJSON)) {
		return federationError(ErrInvalidRecordCode, "expected_outputs_json is invalid")
	}
	if containsSecretLike(req.ProviderSessionRef) {
		return federationError(ErrCredentialScopeDeniedCode, "provider_session_ref looks credential-shaped")
	}
	if len(req.BudgetBindings) == 0 {
		return federationError(ErrChildBudgetRequiredCode, "budget binding is required")
	}
	if req.Permission != PermissionReadOnly && len(req.OwnershipLocks) == 0 {
		return federationError(ErrOwnershipRequiredCode, "write/orchestrate child requires ownership locks")
	}
	return nil
}

func validateRegistrationReferences(ctx context.Context, tx Tx, record AgentRegistration) error {
	var childProject, parentRun, rootRun string
	err := tx.QueryRow(ctx, `SELECT COALESCE(project_id, ''), COALESCE(parent_run_id, ''), root_run_id FROM runs WHERE id = ?`, record.RunID).Scan(&childProject, &parentRun, &rootRun)
	if err != nil {
		return federationError(ErrAgentRegistrationRequiredCode, "child run %s is missing", record.RunID)
	}
	if childProject != "" && childProject != record.ProjectID {
		return federationError(ErrCrossProjectReferenceCode, "child run belongs to %s", childProject)
	}
	if parentRun != record.ParentRunID || rootRun != record.RootRunID {
		return federationError(ErrAgentRegistrationConflictCode, "child run graph mismatch")
	}
	var edgeRoot string
	if err := tx.QueryRow(ctx, `SELECT root_run_id FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, record.ParentRunID, record.RunID).Scan(&edgeRoot); err != nil {
		return federationError(ErrAgentRegistrationRequiredCode, "edge %s/%s is missing", record.ParentRunID, record.RunID)
	}
	if edgeRoot != record.RootRunID {
		return federationError(ErrAgentRegistrationConflictCode, "edge root mismatch")
	}
	var claimExecutor string
	var claimGeneration int64
	if err := tx.QueryRow(ctx, `SELECT executor_id, claim_generation FROM run_claims WHERE run_id = ?`, record.RunID).Scan(&claimExecutor, &claimGeneration); err != nil {
		return federationError(ErrAgentRegistrationRequiredCode, "claim for %s is missing", record.RunID)
	}
	if claimExecutor != record.ExecutorID || claimGeneration != record.ClaimGeneration {
		return federationError(ErrStaleClaimCode, "claim owner/generation mismatch")
	}
	return nil
}

func validateRegistrationClaimTx(ctx context.Context, tx Tx, record AgentRegistration, executorID string, claimGeneration int64) error {
	executorID = strings.TrimSpace(executorID)
	if executorID == "" || claimGeneration <= 0 {
		return federationError(ErrStaleClaimCode, "executor and claim generation are required")
	}
	var currentExecutor string
	var currentGeneration int64
	if err := tx.QueryRow(ctx, `SELECT executor_id, claim_generation FROM run_claims WHERE run_id = ?`, record.RunID).Scan(&currentExecutor, &currentGeneration); err != nil {
		return federationError(ErrAgentRegistrationRequiredCode, "claim for %s is missing", record.RunID)
	}
	if currentExecutor != executorID || currentGeneration != claimGeneration || record.ExecutorID != executorID || record.ClaimGeneration != claimGeneration {
		return federationError(ErrStaleClaimCode, "registration claim does not match current owner/generation")
	}
	return nil
}

func validateActiveBudgetBindingsTx(ctx context.Context, tx Tx, record AgentRegistration, now time.Time) error {
	if len(record.BudgetBindingIDs) == 0 {
		return federationError(ErrChildBudgetRequiredCode, "registration %s has no budget binding", record.ChildAgentID)
	}
	for _, id := range record.BudgetBindingIDs {
		var childAgentID, policyID, reservationID, scope, quantitiesJSON, state string
		err := tx.QueryRow(ctx, `SELECT child_agent_id, budget_policy_id, budget_reservation_id, reservation_scope, reserved_quantities_json, reservation_state
			FROM agent_budget_bindings WHERE id = ?`, id).Scan(&childAgentID, &policyID, &reservationID, &scope, &quantitiesJSON, &state)
		if err != nil {
			return federationError(ErrChildBudgetRequiredCode, "budget binding %s is missing", id)
		}
		if childAgentID != record.ChildAgentID || strings.TrimSpace(policyID) == "" || strings.TrimSpace(reservationID) == "" {
			return federationError(ErrChildBudgetRequiredCode, "budget binding %s is inconsistent", id)
		}
		if strings.ToLower(strings.TrimSpace(state)) != "active" {
			return federationError(ErrChildBudgetRequiredCode, "budget binding %s is %s, want active", id, state)
		}
		if !hasReservedQuantities(quantitiesJSON) {
			return federationError(ErrChildBudgetRequiredCode, "budget binding %s has no reserved quantities", id)
		}
		if err := validateAuthoritativeReservationTx(ctx, tx, record, policyID, reservationID, scope, quantitiesJSON, now); err != nil {
			return err
		}
	}
	return nil
}

func validateAuthoritativeReservationTx(ctx context.Context, tx Tx, record AgentRegistration, policyID, reservationID, reservationScope, quantitiesJSON string, now time.Time) error {
	policyID = strings.TrimSpace(policyID)
	reservationID = strings.TrimSpace(reservationID)
	if policyID == "" || reservationID == "" {
		return federationError(ErrChildBudgetRequiredCode, "budget reservation authority requires policy and reservation ids")
	}
	rows, err := tx.Query(ctx, `SELECT payload_json FROM usage_records
		WHERE project_id = ?
			AND delivery_run_id = ?
			AND task_id = ?
			AND attempt_id = ?
			AND adapter_id = ?
			AND account_profile_id = ?
			AND model_capability_id = ?
			AND event_kind = 'reservation-created'
			AND json_extract(payload_json, '$.budget_reservation_id') = ?
		ORDER BY event_time DESC, usage_record_id DESC`,
		record.ProjectID, record.DeliveryRunID, record.TaskID, record.AttemptID, record.AdapterID,
		record.AccountProfileID, record.ModelCapabilityID, reservationID)
	if err != nil {
		return federationError(ErrChildBudgetRequiredCode, "query authoritative reservation %s: %v", reservationID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return federationError(ErrChildBudgetRequiredCode, "scan authoritative reservation %s: %v", reservationID, err)
		}
		if authoritativeReservationMatches(raw, record, policyID, reservationID, reservationScope, quantitiesJSON, now) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return federationError(ErrChildBudgetRequiredCode, "read authoritative reservation %s: %v", reservationID, err)
	}
	return federationError(ErrChildBudgetRequiredCode, "missing live authoritative reservation %s for policy %s", reservationID, policyID)
}

func authoritativeReservationMatches(raw string, record AgentRegistration, policyID, reservationID, reservationScope, quantitiesJSON string, now time.Time) bool {
	var payload struct {
		BudgetReservationID string          `json:"budget_reservation_id"`
		State               string          `json:"state"`
		ScopeKey            string          `json:"scope_key"`
		BudgetPolicyIDs     []string        `json:"budget_policy_ids"`
		ReservedValue       int64           `json:"reserved_value"`
		CommittedValue      int64           `json:"committed_value"`
		ReleasedValue       int64           `json:"released_value"`
		ExpiresAt           string          `json:"expires_at"`
		RequesterID         string          `json:"requester_id"`
		PolicyFingerprint   string          `json:"policy_fingerprint"`
		Quantities          json.RawMessage `json:"reserved_quantities_json"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(payload.State))
	if payload.BudgetReservationID != reservationID || (state != "reserved" && state != "partially-committed") {
		return false
	}
	if !stringSliceContains(payload.BudgetPolicyIDs, policyID) {
		return false
	}
	if strings.TrimSpace(payload.ScopeKey) != "" && strings.TrimSpace(payload.ScopeKey) != strings.TrimSpace(reservationScope) {
		return false
	}
	if strings.TrimSpace(payload.RequesterID) != "" && strings.TrimSpace(payload.RequesterID) != record.ChildAgentID && strings.TrimSpace(payload.RequesterID) != record.RunID {
		return false
	}
	if strings.TrimSpace(payload.PolicyFingerprint) != "" && strings.TrimSpace(payload.PolicyFingerprint) != record.PolicyFingerprint {
		return false
	}
	if payload.ReservedValue <= 0 || payload.CommittedValue < 0 || payload.ReleasedValue < 0 || payload.CommittedValue+payload.ReleasedValue > payload.ReservedValue {
		return false
	}
	if strings.TrimSpace(payload.ExpiresAt) != "" {
		expiresAt, err := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
		if err != nil || (!now.IsZero() && !expiresAt.After(now.UTC())) {
			return false
		}
	}
	if len(payload.Quantities) > 0 && string(payload.Quantities) != "null" {
		return canonicalJSONEqual(string(payload.Quantities), quantitiesJSON)
	}
	return true
}

func canonicalJSONEqual(left, right string) bool {
	var l, r any
	if json.Unmarshal([]byte(left), &l) != nil || json.Unmarshal([]byte(right), &r) != nil {
		return strings.TrimSpace(left) == strings.TrimSpace(right)
	}
	return digestJSON(l) == digestJSON(r)
}

func stringSliceContains(values []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func validateActiveOwnershipLocksTx(ctx context.Context, tx Tx, record AgentRegistration, now time.Time) error {
	if record.Permission == PermissionReadOnly {
		return nil
	}
	if len(record.OwnershipLockIDs) == 0 {
		return federationError(ErrOwnershipRequiredCode, "registration %s has no ownership locks", record.ChildAgentID)
	}
	scope, err := loadScopeGrantPayloadTx(ctx, tx, record.ScopeGrantID, record.ChildAgentID)
	if err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	locks := make([]AgentOwnershipLock, 0, len(record.OwnershipLockIDs))
	for _, id := range record.OwnershipLockIDs {
		lock, err := loadOwnershipLockTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if lock.ChildAgentID != record.ChildAgentID || lock.RunID != record.RunID || lock.ClaimGeneration != record.ClaimGeneration {
			return federationError(ErrOwnershipRequiredCode, "ownership lock %s is not fenced to the current child claim", id)
		}
		if lock.State != OwnershipStateHeld {
			return federationError(ErrOwnershipRequiredCode, "ownership lock %s is %s, want held", id, lock.State)
		}
		lease, err := time.Parse(time.RFC3339Nano, lock.LeaseExpiresAt)
		if err != nil || !lease.After(now.UTC()) {
			return federationError(ErrOwnershipRequiredCode, "ownership lock %s is expired", id)
		}
		locks = append(locks, lock)
	}
	for _, writeScope := range scope.WriteScope {
		if strings.TrimSpace(writeScope) == "" {
			continue
		}
		covered := false
		for _, lock := range locks {
			if resourceKeyCovers(lock.ResourceKind, lock.ResourceKey, writeScope) {
				covered = true
				break
			}
		}
		if !covered {
			return federationError(ErrOwnershipRequiredCode, "write scope %s is not covered by an active ownership lock", writeScope)
		}
	}
	return nil
}

func releaseHeldOwnershipLocksTx(ctx context.Context, tx Tx, record AgentRegistration, at string) error {
	if len(record.OwnershipLockIDs) == 0 {
		return nil
	}
	result, err := tx.Exec(ctx, `UPDATE agent_ownership_locks
		SET state = ?, updated_at = ?
		WHERE child_agent_id = ? AND run_id = ? AND claim_generation = ? AND state = ?`,
		OwnershipStateReleased, at, record.ChildAgentID, record.RunID, record.ClaimGeneration, OwnershipStateHeld)
	if err != nil {
		return fmt.Errorf("release ownership locks: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && record.Permission != PermissionReadOnly && affected != int64(len(record.OwnershipLockIDs)) {
		return federationError(ErrOwnershipRequiredCode, "released %d ownership locks, want %d", affected, len(record.OwnershipLockIDs))
	}
	return nil
}

func loadScopeGrantPayloadTx(ctx context.Context, tx Tx, scopeGrantID, childAgentID string) (struct {
	WriteScope []string `json:"write_scope"`
}, error) {
	var raw string
	if err := tx.QueryRow(ctx, `SELECT scope_json FROM agent_scope_grants WHERE id = ? AND child_agent_id = ?`, scopeGrantID, childAgentID).Scan(&raw); err != nil {
		return struct {
			WriteScope []string `json:"write_scope"`
		}{}, federationError(ErrOwnershipRequiredCode, "scope grant %s is missing", scopeGrantID)
	}
	var scope struct {
		WriteScope []string `json:"write_scope"`
	}
	if err := json.Unmarshal([]byte(raw), &scope); err != nil {
		return scope, federationError(ErrInvalidRecordCode, "scope grant %s is invalid", scopeGrantID)
	}
	scope.WriteScope = normalizeScopeList(scope.WriteScope, true)
	return scope, nil
}

func loadOwnershipLockTx(ctx context.Context, tx Tx, id string) (AgentOwnershipLock, error) {
	var lock AgentOwnershipLock
	var conflictsJSON string
	err := tx.QueryRow(ctx, `SELECT id, project_id, delivery_run_id, child_agent_id, run_id, claim_generation,
			lock_generation, resource_kind, resource_key, lock_mode, state, lease_expires_at,
			heartbeat_at, conflicts_with_json, created_at, updated_at
		FROM agent_ownership_locks WHERE id = ?`, id).Scan(
		&lock.AgentOwnershipLockID, &lock.ProjectID, &lock.DeliveryRunID, &lock.ChildAgentID, &lock.RunID, &lock.ClaimGeneration,
		&lock.LockGeneration, &lock.ResourceKind, &lock.ResourceKey, &lock.LockMode, &lock.State, &lock.LeaseExpiresAt,
		&lock.HeartbeatAt, &conflictsJSON, &lock.CreatedAt, &lock.UpdatedAt)
	if err != nil {
		return AgentOwnershipLock{}, federationError(ErrOwnershipRequiredCode, "ownership lock %s is missing", id)
	}
	lock.SchemaVersion = AgentOwnershipLockSchema
	lock.ConflictsWith = decodeStringList(conflictsJSON)
	return lock, nil
}

func hasReservedQuantities(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "[]" || raw == "null" || !json.Valid([]byte(raw)) {
		return false
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return false
	}
	switch value := decoded.(type) {
	case map[string]any:
		return len(value) > 0
	case []any:
		return len(value) > 0
	default:
		return true
	}
}

func isTerminalAgentState(state string) bool {
	switch normalizeAgentRegistrationState(state) {
	case AgentStateSucceeded, AgentStateFailed, AgentStateCancelled:
		return true
	default:
		return false
	}
}

func ensureNoParentAgentCycle(ctx context.Context, tx Tx, childAgentID, parentAgentID, projectID string) error {
	seen := map[string]bool{childAgentID: true}
	current := strings.TrimSpace(parentAgentID)
	for current != "" {
		if seen[current] {
			return federationError(ErrAgentRegistrationConflictCode, "agent parent cycle at %s", current)
		}
		seen[current] = true
		var next, foundProject string
		err := tx.QueryRow(ctx, `SELECT parent_agent_id, project_id FROM agent_registrations WHERE id = ?`, current).Scan(&next, &foundProject)
		if err != nil {
			return federationError(ErrAgentRegistrationRequiredCode, "parent agent %s is missing", current)
		}
		if foundProject != projectID {
			return federationError(ErrCrossProjectReferenceCode, "parent agent %s belongs to %s", current, foundProject)
		}
		current = strings.TrimSpace(next)
	}
	return nil
}

func insertScopeGrantTx(ctx context.Context, tx Tx, scope AgentScopeGrant) error {
	scopeJSON, err := json.Marshal(scopePayload(scope))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO agent_scope_grants(
			id, project_id, delivery_run_id, child_agent_id, schema_version, record_version,
			scope_json, permission, side_effect_class, policy_version, policy_fingerprint,
			plan_fingerprint, authorization_fingerprint, agent_federation_fingerprint,
			created_at, updated_at, terminal_error_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		scope.AgentScopeGrantID, scope.ProjectID, scope.DeliveryRunID, scope.ChildAgentID, scope.SchemaVersion, scope.RecordVersion,
		string(scopeJSON), scope.Permission, scope.SideEffectClass, scope.PolicyVersion, scope.PolicyFingerprint,
		scope.PlanFingerprint, scope.AuthorizationFingerprint, scope.AgentFederationFingerprint,
		scope.CreatedAt, scope.UpdatedAt, scope.TerminalErrorCode)
	if err != nil {
		return fmt.Errorf("insert scope grant: %w", err)
	}
	return nil
}

func insertPreparedAgentRegistrationTx(ctx context.Context, tx Tx, prepared preparedAgentRegistration, now time.Time) (AgentRegistration, bool, error) {
	record := prepared.record
	existing, ok, err := loadAgentRegistrationTx(ctx, tx, record.ChildAgentID)
	if err != nil {
		return AgentRegistration{}, false, err
	}
	if ok {
		if existing.RegistrationPayloadHash == record.RegistrationPayloadHash {
			return existing, true, nil
		}
		return AgentRegistration{}, false, federationError(ErrAgentRegistrationConflictCode, "child_agent_id %s replays with different canonical bytes", record.ChildAgentID)
	}
	if err := validateRegistrationReferences(ctx, tx, record); err != nil {
		return AgentRegistration{}, false, err
	}
	if err := ensureNoParentAgentCycle(ctx, tx, record.ChildAgentID, record.ParentAgentID, record.ProjectID); err != nil {
		return AgentRegistration{}, false, err
	}
	for _, binding := range prepared.bindings {
		if err := validateAuthoritativeReservationTx(ctx, tx, record, binding.BudgetPolicyID, binding.BudgetReservationID, binding.ReservationScope, binding.ReservedQuantitiesJSON, now); err != nil {
			return AgentRegistration{}, false, err
		}
	}
	if err := insertScopeGrantTx(ctx, tx, prepared.scope); err != nil {
		return AgentRegistration{}, false, err
	}
	for _, binding := range prepared.bindings {
		if err := insertBudgetBindingTx(ctx, tx, binding); err != nil {
			return AgentRegistration{}, false, err
		}
	}
	for _, lock := range prepared.locks {
		if err := insertOwnershipLockTx(ctx, tx, lock); err != nil {
			return AgentRegistration{}, false, err
		}
	}
	if err := insertAgentRegistrationTx(ctx, tx, record); err != nil {
		return AgentRegistration{}, false, err
	}
	if err := appendAgentEventTx(ctx, tx, record.ProjectID, record.DeliveryRunID, record.ChildAgentID, "registration.registered", record.CreatedAt, record); err != nil {
		return AgentRegistration{}, false, err
	}
	return record, false, nil
}

func transitionAgentRegistrationTx(ctx context.Context, tx Tx, record *AgentRegistration, action, actorExecutorID string, claimGeneration int64, at string) error {
	if record == nil {
		return federationError(ErrAgentRegistrationRequiredCode, "registration is required")
	}
	if actionRequiresClaim(action) {
		if err := validateRegistrationClaimTx(ctx, tx, *record, actorExecutorID, claimGeneration); err != nil {
			return err
		}
	}
	next, err := nextAgentRegistrationState(record.RegistrationState, action)
	if err != nil {
		return err
	}
	record.RegistrationState = next
	record.UpdatedAt = at
	record.RecordVersion++
	if isTerminalAgentState(next) {
		if err := releaseHeldOwnershipLocksTx(ctx, tx, *record, at); err != nil {
			return err
		}
	}
	result, err := tx.Exec(ctx, `UPDATE agent_registrations
		SET registration_state = ?, updated_at = ?, record_version = ?
		WHERE id = ? AND record_version = ?`,
		record.RegistrationState, record.UpdatedAt, record.RecordVersion, record.ChildAgentID, record.RecordVersion-1)
	if err != nil {
		return fmt.Errorf("transition agent registration: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != 1 {
		return federationError(ErrStaleClaimCode, "registration %s transition updated %d rows", record.ChildAgentID, affected)
	}
	return appendAgentEventTx(ctx, tx, record.ProjectID, record.DeliveryRunID, record.ChildAgentID, "registration."+action, at, *record)
}

func insertBudgetBindingTx(ctx context.Context, tx Tx, binding AgentBudgetBinding) error {
	_, err := tx.Exec(ctx, `INSERT INTO agent_budget_bindings(
			id, project_id, delivery_run_id, child_agent_id, budget_policy_id, budget_reservation_id,
			reservation_scope, reserved_quantities_json, ancestor_budget_refs_json, reservation_state,
			created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(child_agent_id, budget_policy_id, budget_reservation_id) DO NOTHING`,
		binding.AgentBudgetBindingID, binding.ProjectID, binding.DeliveryRunID, binding.ChildAgentID, binding.BudgetPolicyID, binding.BudgetReservationID,
		binding.ReservationScope, binding.ReservedQuantitiesJSON, binding.AncestorBudgetRefsJSON, binding.ReservationState,
		binding.CreatedAt, binding.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert budget binding: %w", err)
	}
	return nil
}

func insertOwnershipLockTx(ctx context.Context, tx Tx, lock AgentOwnershipLock) error {
	if lock.State == OwnershipStateHeld || lock.State == OwnershipStateRequested || lock.State == OwnershipStateReleasing {
		if err := checkOwnershipOverlapTx(ctx, tx, lock); err != nil {
			return err
		}
	}
	conflictsJSON := mustJSONList(lock.ConflictsWith)
	_, err := tx.Exec(ctx, `INSERT INTO agent_ownership_locks(
			id, project_id, delivery_run_id, child_agent_id, run_id, claim_generation,
			lock_generation, resource_kind, resource_key, lock_mode, state, lease_expires_at,
			heartbeat_at, conflicts_with_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lock.AgentOwnershipLockID, lock.ProjectID, lock.DeliveryRunID, lock.ChildAgentID, lock.RunID, lock.ClaimGeneration,
		lock.LockGeneration, lock.ResourceKind, lock.ResourceKey, lock.LockMode, lock.State, lock.LeaseExpiresAt,
		lock.HeartbeatAt, conflictsJSON, lock.CreatedAt, lock.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "idx_agent_ownership_active_exact") || strings.Contains(err.Error(), "constraint failed") {
			return federationError(ErrOneWriterConflictCode, "active lock conflict for %s/%s", lock.ResourceKind, lock.ResourceKey)
		}
		return fmt.Errorf("insert ownership lock: %w", err)
	}
	return nil
}

func checkOwnershipOverlapTx(ctx context.Context, tx Tx, lock AgentOwnershipLock) error {
	rows, err := tx.Query(ctx, `SELECT id, resource_key FROM agent_ownership_locks
		WHERE project_id = ? AND resource_kind = ? AND state NOT IN ('released', 'expired', 'conflict', 'needs-human')`,
		lock.ProjectID, lock.ResourceKind)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, existingKey string
		if err := rows.Scan(&id, &existingKey); err != nil {
			return err
		}
		if resourceKeysConflict(lock.ResourceKind, existingKey, lock.ResourceKey) {
			return federationError(ErrOneWriterConflictCode, "lock %s conflicts with %s", lock.AgentOwnershipLockID, id)
		}
	}
	return rows.Err()
}

func insertAgentRegistrationTx(ctx context.Context, tx Tx, record AgentRegistration) error {
	_, err := tx.Exec(ctx, `INSERT INTO agent_registrations(
			id, record_version, project_id, delivery_run_id, root_run_id, parent_run_id, child_run_id, parent_agent_id,
			task_id, attempt_id, plan_id, child_key, adapter_id, provider_installation_id,
			account_profile_id, model_capability_id, routing_decision_id, provider_session_ref,
			scope_grant_id, permission, side_effect_class, budget_binding_ids_json,
			ownership_lock_ids_json, claim_generation, executor_id, provider_idempotency_key,
			provider_receipt, cancellation_channel, expected_outputs_json, registration_state,
			depth, policy_version, plan_fingerprint, policy_fingerprint, authorization_fingerprint,
			agent_federation_fingerprint, registration_payload_hash, classification,
			gap_reasons_json, created_at, updated_at, terminal_error_code)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ChildAgentID, record.RecordVersion, record.ProjectID, record.DeliveryRunID, record.RootRunID, record.ParentRunID, record.RunID, record.ParentAgentID,
		record.TaskID, record.AttemptID, record.PlanID, record.ChildKey, record.AdapterID, record.ProviderInstallationID,
		record.AccountProfileID, record.ModelCapabilityID, record.RoutingDecisionID, record.ProviderSessionRef,
		record.ScopeGrantID, record.Permission, record.SideEffectClass, mustJSONList(record.BudgetBindingIDs),
		mustJSONList(record.OwnershipLockIDs), record.ClaimGeneration, record.ExecutorID, record.ProviderIDempotencyKey,
		record.ProviderReceipt, record.CancellationChannel, record.ExpectedOutputsJSON, record.RegistrationState,
		record.Depth, record.PolicyVersion, record.PlanFingerprint, record.PolicyFingerprint, record.AuthorizationFingerprint,
		record.AgentFederationFingerprint, record.RegistrationPayloadHash, record.Classification,
		mustJSONList(record.GapReasons), record.CreatedAt, record.UpdatedAt, record.TerminalErrorCode)
	if err != nil {
		return fmt.Errorf("insert agent registration: %w", err)
	}
	return nil
}

func loadAgentRegistrationTx(ctx context.Context, tx Tx, childAgentID string) (AgentRegistration, bool, error) {
	return scanAgentRegistration(tx.QueryRow(ctx, `SELECT
		id, record_version, project_id, delivery_run_id, root_run_id, parent_run_id, child_run_id, parent_agent_id,
		task_id, attempt_id, plan_id, child_key, adapter_id, provider_installation_id,
		account_profile_id, model_capability_id, routing_decision_id, provider_session_ref,
		scope_grant_id, permission, side_effect_class, budget_binding_ids_json,
		ownership_lock_ids_json, claim_generation, executor_id, provider_idempotency_key,
		provider_receipt, cancellation_channel, expected_outputs_json, registration_state,
		depth, policy_version, plan_fingerprint, policy_fingerprint, authorization_fingerprint,
		agent_federation_fingerprint, registration_payload_hash, classification,
		gap_reasons_json, created_at, updated_at, terminal_error_code
		FROM agent_registrations WHERE id = ?`, childAgentID))
}

func loadAgentRegistrationByRunTx(ctx context.Context, tx Tx, runID string) (AgentRegistration, bool, error) {
	return scanAgentRegistration(tx.QueryRow(ctx, `SELECT
		id, record_version, project_id, delivery_run_id, root_run_id, parent_run_id, child_run_id, parent_agent_id,
		task_id, attempt_id, plan_id, child_key, adapter_id, provider_installation_id,
		account_profile_id, model_capability_id, routing_decision_id, provider_session_ref,
		scope_grant_id, permission, side_effect_class, budget_binding_ids_json,
		ownership_lock_ids_json, claim_generation, executor_id, provider_idempotency_key,
		provider_receipt, cancellation_channel, expected_outputs_json, registration_state,
		depth, policy_version, plan_fingerprint, policy_fingerprint, authorization_fingerprint,
		agent_federation_fingerprint, registration_payload_hash, classification,
		gap_reasons_json, created_at, updated_at, terminal_error_code
		FROM agent_registrations WHERE child_run_id = ?`, runID))
}

type scanner interface {
	Scan(dest ...any) error
}

func scanAgentRegistration(row scanner) (AgentRegistration, bool, error) {
	var record AgentRegistration
	var budgetsJSON, locksJSON, gapsJSON string
	err := row.Scan(
		&record.ChildAgentID, &record.RecordVersion, &record.ProjectID, &record.DeliveryRunID, &record.RootRunID, &record.ParentRunID, &record.RunID, &record.ParentAgentID,
		&record.TaskID, &record.AttemptID, &record.PlanID, &record.ChildKey, &record.AdapterID, &record.ProviderInstallationID,
		&record.AccountProfileID, &record.ModelCapabilityID, &record.RoutingDecisionID, &record.ProviderSessionRef,
		&record.ScopeGrantID, &record.Permission, &record.SideEffectClass, &budgetsJSON,
		&locksJSON, &record.ClaimGeneration, &record.ExecutorID, &record.ProviderIDempotencyKey,
		&record.ProviderReceipt, &record.CancellationChannel, &record.ExpectedOutputsJSON, &record.RegistrationState,
		&record.Depth, &record.PolicyVersion, &record.PlanFingerprint, &record.PolicyFingerprint, &record.AuthorizationFingerprint,
		&record.AgentFederationFingerprint, &record.RegistrationPayloadHash, &record.Classification,
		&gapsJSON, &record.CreatedAt, &record.UpdatedAt, &record.TerminalErrorCode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentRegistration{}, false, nil
		}
		return AgentRegistration{}, false, err
	}
	record.SchemaVersion = AgentRegistrationSchema
	record.BudgetBindingIDs = decodeStringList(budgetsJSON)
	record.OwnershipLockIDs = decodeStringList(locksJSON)
	record.GapReasons = decodeStringList(gapsJSON)
	return record, true, nil
}

func appendAgentEventTx(ctx context.Context, tx Tx, projectID, deliveryRunID, childAgentID, kind, at string, payload any) error {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal agent event payload: %w", err)
	}
	payloadHash := hashBytes(payloadBytes)
	previous := ""
	_ = tx.QueryRow(ctx, `SELECT event_hash FROM agent_events WHERE child_agent_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, childAgentID).Scan(&previous)
	eventID := stableID("aevt_", childAgentID, kind, at, payloadHash, previous)
	eventHash := hashString(strings.Join([]string{eventID, childAgentID, kind, previous, payloadHash, string(payloadBytes)}, "\n"))
	_, err = tx.Exec(ctx, `INSERT INTO agent_events(id, project_id, delivery_run_id, child_agent_id, event_kind, event_hash, previous_event_hash, payload_hash, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		eventID, projectID, deliveryRunID, childAgentID, kind, eventHash, previous, payloadHash, string(payloadBytes), at)
	if err != nil {
		return fmt.Errorf("append agent event: %w", err)
	}
	return nil
}

func normalizeBudgetBinding(binding AgentBudgetBinding, req AgentRegistrationRequest, childAgentID string) AgentBudgetBinding {
	binding.SchemaVersion = AgentBudgetBindingSchema
	binding.ProjectID = req.ProjectID
	binding.DeliveryRunID = req.DeliveryRunID
	binding.ChildAgentID = childAgentID
	binding.BudgetPolicyID = strings.TrimSpace(binding.BudgetPolicyID)
	binding.BudgetReservationID = strings.TrimSpace(binding.BudgetReservationID)
	binding.ReservationScope = firstNonEmptyAgent(binding.ReservationScope, "sub-agent")
	binding.ReservedQuantitiesJSON = firstNonEmptyAgent(binding.ReservedQuantitiesJSON, "{}")
	binding.AncestorBudgetRefsJSON = firstNonEmptyAgent(binding.AncestorBudgetRefsJSON, "[]")
	binding.ReservationState = firstNonEmptyAgent(binding.ReservationState, "active")
	binding.CreatedAt = firstNonEmptyAgent(binding.CreatedAt, req.CreatedAt)
	binding.UpdatedAt = firstNonEmptyAgent(binding.UpdatedAt, req.CreatedAt)
	binding.AgentBudgetBindingID = stableID("abudget_", childAgentID, binding.BudgetReservationID, binding.BudgetPolicyID)
	return binding
}

func normalizeOwnershipLock(lock AgentOwnershipLock, req AgentRegistrationRequest, childAgentID string) AgentOwnershipLock {
	lock.SchemaVersion = AgentOwnershipLockSchema
	lock.ProjectID = req.ProjectID
	lock.DeliveryRunID = req.DeliveryRunID
	lock.ChildAgentID = childAgentID
	lock.RunID = req.RunID
	lock.ClaimGeneration = req.ClaimGeneration
	if lock.LockGeneration <= 0 {
		lock.LockGeneration = 1
	}
	lock.ResourceKind = strings.TrimSpace(lock.ResourceKind)
	lock.ResourceKey = canonicalResourceKey(lock.ResourceKind, lock.ResourceKey)
	lock.LockMode = firstNonEmptyAgent(lock.LockMode, "write")
	lock.State = firstNonEmptyAgent(lock.State, OwnershipStateHeld)
	lock.LeaseExpiresAt = firstNonEmptyAgent(lock.LeaseExpiresAt, req.CreatedAt)
	lock.HeartbeatAt = firstNonEmptyAgent(lock.HeartbeatAt, req.CreatedAt)
	lock.ConflictsWith = sortedCopyAgent(lock.ConflictsWith)
	lock.CreatedAt = firstNonEmptyAgent(lock.CreatedAt, req.CreatedAt)
	lock.UpdatedAt = firstNonEmptyAgent(lock.UpdatedAt, req.CreatedAt)
	lock.AgentOwnershipLockID = stableID("alock_", lock.ProjectID, lock.ResourceKind, lock.ResourceKey, childAgentID)
	return lock
}

func canonicalizeScope(scope *AgentScopeGrant) error {
	scope.ReadScope = normalizeScopeList(scope.ReadScope, true)
	scope.WriteScope = normalizeScopeList(scope.WriteScope, true)
	scope.PathScope = normalizeScopeList(scope.PathScope, true)
	scope.RepositoryScope = normalizeScopeList(scope.RepositoryScope, false)
	scope.WorktreeScope = normalizeScopeList(scope.WorktreeScope, false)
	scope.CommandScope = normalizeScopeList(scope.CommandScope, false)
	scope.NetworkScope = normalizeScopeList(scope.NetworkScope, false)
	scope.CredentialScope = normalizeScopeList(scope.CredentialScope, false)
	scope.SideEffectScope = normalizeScopeList(scope.SideEffectScope, false)
	scope.ApprovalScope = normalizeScopeList(scope.ApprovalScope, false)
	scope.Permission = normalizePermission(scope.Permission)
	if !validSideEffectClass(scope.SideEffectClass) {
		return federationError(ErrInvalidRecordCode, "side_effect_class %q is invalid", scope.SideEffectClass)
	}
	for _, value := range scope.SideEffectScope {
		if !validSideEffectClass(value) {
			return federationError(ErrInvalidRecordCode, "side_effect_scope %q is invalid", value)
		}
	}
	for _, value := range append(append([]string{}, scope.ReadScope...), append(scope.WriteScope, scope.PathScope...)...) {
		if _, err := normalizeRepoResource(value); err != nil {
			return err
		}
	}
	for _, value := range scope.CredentialScope {
		if strings.TrimSpace(value) != "" && strings.TrimSpace(value) != "none" {
			return federationError(ErrCredentialScopeDeniedCode, "credential material scope is forbidden")
		}
	}
	return nil
}

func validateScopeSubset(name string, parent, child []string) error {
	parentSet := map[string]bool{}
	for _, value := range parent {
		value = strings.TrimSpace(value)
		if value == "unknown" || value == "stale" || value == "unclassified" {
			return federationError(ErrScopeUnknownCode, "%s parent is %s", name, value)
		}
		parentSet[value] = true
	}
	for _, value := range child {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if value == "unknown" || value == "stale" || value == "unclassified" {
			return federationError(ErrScopeUnknownCode, "%s child is %s", name, value)
		}
		if !parentSet[value] {
			return federationError(ErrScopeWideningCode, "%s %q is not in parent grant", name, value)
		}
	}
	return nil
}

func scopePayload(scope AgentScopeGrant) struct {
	ReadScope       []string `json:"read_scope"`
	WriteScope      []string `json:"write_scope"`
	PathScope       []string `json:"path_scope"`
	RepositoryScope []string `json:"repository_scope"`
	WorktreeScope   []string `json:"worktree_scope"`
	CommandScope    []string `json:"command_scope"`
	NetworkScope    []string `json:"network_scope"`
	CredentialScope []string `json:"credential_scope"`
	SideEffectScope []string `json:"side_effect_scope"`
	ApprovalScope   []string `json:"approval_scope"`
} {
	return struct {
		ReadScope       []string `json:"read_scope"`
		WriteScope      []string `json:"write_scope"`
		PathScope       []string `json:"path_scope"`
		RepositoryScope []string `json:"repository_scope"`
		WorktreeScope   []string `json:"worktree_scope"`
		CommandScope    []string `json:"command_scope"`
		NetworkScope    []string `json:"network_scope"`
		CredentialScope []string `json:"credential_scope"`
		SideEffectScope []string `json:"side_effect_scope"`
		ApprovalScope   []string `json:"approval_scope"`
	}{
		scope.ReadScope, scope.WriteScope, scope.PathScope, scope.RepositoryScope, scope.WorktreeScope,
		scope.CommandScope, scope.NetworkScope, scope.CredentialScope, scope.SideEffectScope, scope.ApprovalScope,
	}
}

func normalizeScopeList(values []string, repoPath bool) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
		if repoPath {
			if normalized, err := normalizeRepoResource(value); err == nil {
				value = normalized
			}
		}
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return dedupeAgent(out)
}

func normalizeRepoResource(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return "", nil
	}
	if strings.HasPrefix(value, "/") || strings.Contains(value, ":") {
		return "", federationError(ErrInvalidRecordCode, "path %q must be project-relative", value)
	}
	clean := path.Clean(value)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", federationError(ErrInvalidRecordCode, "path %q escapes project root", value)
	}
	return clean, nil
}

func resourceKeysConflict(kind, existing, requested string) bool {
	existing = canonicalResourceKey(kind, existing)
	requested = canonicalResourceKey(kind, requested)
	if existing == "" || requested == "" {
		return true
	}
	if existing == requested {
		return true
	}
	if kind == "repo-path" {
		return strings.HasPrefix(existing, requested+"/") || strings.HasPrefix(requested, existing+"/")
	}
	return false
}

func resourceKeyCovers(kind, lockKey, requested string) bool {
	lockKey = canonicalResourceKey(kind, lockKey)
	requested = canonicalResourceKey(kind, requested)
	if lockKey == "" || requested == "" {
		return false
	}
	if lockKey == requested {
		return true
	}
	if kind == "repo-path" {
		return strings.HasPrefix(requested, lockKey+"/")
	}
	return false
}

func canonicalResourceKey(kind, key string) string {
	key = strings.TrimSpace(strings.ReplaceAll(key, "\\", "/"))
	if kind == "repo-path" {
		if normalized, err := normalizeRepoResource(key); err == nil {
			return normalized
		}
	}
	return key
}

func normalizeAgentRegistrationState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case AgentStatePlanned, AgentStateRegistered, AgentStateLaunching, AgentStateRunning, AgentStateCancelling, AgentStateSucceeded, AgentStateFailed, AgentStateCancelled, AgentStateNeedsHuman, AgentStateSuperseded:
		return strings.ToLower(strings.TrimSpace(state))
	case "needs_human", "needs human":
		return AgentStateNeedsHuman
	default:
		return strings.ToLower(strings.TrimSpace(state))
	}
}

func normalizePermission(permission string) string {
	switch strings.ToLower(strings.TrimSpace(permission)) {
	case "", PermissionReadOnly, "readonly", "read_only":
		return PermissionReadOnly
	case PermissionWrite:
		return PermissionWrite
	case PermissionOrchestrate:
		return PermissionOrchestrate
	default:
		return strings.ToLower(strings.TrimSpace(permission))
	}
}

func validSideEffectClass(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case SideEffectNone, SideEffectLocalRead, SideEffectRepoWrite:
		return true
	default:
		return false
	}
}

func permissionRank(permission string) int {
	switch normalizePermission(permission) {
	case PermissionReadOnly:
		return 0
	case PermissionWrite:
		return 1
	case PermissionOrchestrate:
		return 2
	default:
		return 3
	}
}

func actionRequiresClaim(action string) bool {
	switch action {
	case AgentActionLaunch, AgentActionHeartbeat, AgentActionCancel, AgentActionCompleteSuccess, AgentActionCompleteFailure, AgentActionRecoverTakeover:
		return true
	default:
		return false
	}
}

func isUnsupportedNativeAdapter(adapterID string) bool {
	switch strings.ToLower(strings.TrimSpace(adapterID)) {
	case "codex", "gemini", "antigravity":
		return true
	default:
		return false
	}
}

func normalizeClosedChildStatus(status string) (string, bool) {
	switch normalizeDurableStatus(status) {
	case "succeeded", "succeeded_with_optional_failures", "failed", "cancelled", "timed_out", "abandoned", "needs-human", "skipped", "launching", "running", "waiting", "queued", "blocked":
		return normalizeDurableStatus(status), true
	default:
		return "", false
	}
}

func stableID(prefix string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return prefix + strings.ToLower(encoded)
}

func digestJSON(v any) string {
	data, _ := json.Marshal(v)
	return hashBytes(data)
}

func hashString(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func firstNonEmptyAgent(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sortedCopyAgent(values []string) []string {
	out := normalizeScopeList(values, false)
	if out == nil {
		return []string{}
	}
	return out
}

func dedupeAgent(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := values[:0]
	previous := ""
	for _, value := range values {
		if value == "" || value == previous {
			continue
		}
		out = append(out, value)
		previous = value
	}
	return out
}

func mustJSONList(values []string) string {
	data, _ := json.Marshal(sortedCopyAgent(values))
	return string(data)
}

func decodeStringList(raw string) []string {
	var values []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &values); err != nil {
		return []string{}
	}
	return sortedCopyAgent(values)
}

func containsSecretLike(value string) bool {
	_, redacted := redactSecretLike(value)
	return redacted
}

func redactSecretLike(value string) (string, bool) {
	parts := splitSecretBoundaries(value)
	changed := false
	for i, part := range parts {
		if secretTokenLike(part) {
			parts[i] = "[REDACTED]"
			changed = true
		}
	}
	return strings.Join(parts, ""), changed
}

func splitSecretBoundaries(value string) []string {
	if value == "" {
		return nil
	}
	var parts []string
	var current strings.Builder
	currentToken := isSecretTokenRune(rune(value[0]))
	for _, r := range value {
		token := isSecretTokenRune(r)
		if token != currentToken && current.Len() > 0 {
			parts = append(parts, current.String())
			current.Reset()
		}
		current.WriteRune(r)
		currentToken = token
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func isSecretTokenRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
}

func secretTokenLike(token string) bool {
	lower := strings.ToLower(token)
	upper := strings.ToUpper(token)
	if len(token) == 20 && (strings.HasPrefix(upper, "AK"+"IA") || strings.HasPrefix(upper, "AS"+"IA")) && allUpperAlphaNum(upper) {
		return true
	}
	if strings.HasPrefix(lower, "sk-") && len(token) >= 20 {
		return true
	}
	if strings.HasPrefix(lower, "xox") && len(token) >= 20 {
		return true
	}
	if strings.Contains(lower, "token") && len(token) >= 16 {
		return true
	}
	return false
}

func allUpperAlphaNum(value string) bool {
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
