// Package delivery implements the v0.8 DeliveryRun durable contract.
package delivery

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrInvalidTransitionCode         ErrorCode = "ErrInvalidTransition"
	ErrTerminalStateCode             ErrorCode = "ErrTerminalState"
	ErrCycleDetectedCode             ErrorCode = "ErrCycleDetected"
	ErrCrossProjectReferenceCode     ErrorCode = "ErrCrossProjectReference"
	ErrMissingReferenceCode          ErrorCode = "ErrMissingReference"
	ErrDuplicateRecordCode           ErrorCode = "ErrDuplicateRecord"
	ErrDuplicateReplayCode           ErrorCode = "ErrDuplicateReplay"
	ErrStaleApprovalCode             ErrorCode = "ErrStaleApproval"
	ErrPolicyDeniedCode              ErrorCode = "ErrPolicyDenied"
	ErrOverrideRequiredCode          ErrorCode = "ErrOverrideRequired"
	ErrApprovalRequiredCode          ErrorCode = "ErrApprovalRequired"
	ErrExpiredApprovalCode           ErrorCode = "ErrExpiredApproval"
	ErrSideEffectClassExceededCode   ErrorCode = "ErrSideEffectClassExceeded"
	ErrClaimRequiredCode             ErrorCode = "ErrClaimRequired"
	ErrClaimConflictCode             ErrorCode = "ErrClaimConflict"
	ErrStaleClaimCode                ErrorCode = "ErrStaleClaim"
	ErrAmbiguousLegacyStateCode      ErrorCode = "ErrAmbiguousLegacyState"
	ErrUnknownRecordVersionCode      ErrorCode = "ErrUnknownRecordVersion"
	ErrInvalidRecordCode             ErrorCode = "ErrInvalidRecord"
	ErrUnsupportedHostCapabilityCode ErrorCode = "ErrUnsupportedHostCapability"
	ErrInvocationInterruptedCode     ErrorCode = "ErrInvocationInterrupted"
)

var (
	ErrInvalidTransition         = &TypedError{Code: ErrInvalidTransitionCode}
	ErrTerminalState             = &TypedError{Code: ErrTerminalStateCode}
	ErrCycleDetected             = &TypedError{Code: ErrCycleDetectedCode}
	ErrCrossProjectReference     = &TypedError{Code: ErrCrossProjectReferenceCode}
	ErrMissingReference          = &TypedError{Code: ErrMissingReferenceCode}
	ErrDuplicateRecord           = &TypedError{Code: ErrDuplicateRecordCode}
	ErrDuplicateReplay           = &TypedError{Code: ErrDuplicateReplayCode}
	ErrStaleApproval             = &TypedError{Code: ErrStaleApprovalCode}
	ErrPolicyDenied              = &TypedError{Code: ErrPolicyDeniedCode}
	ErrOverrideRequired          = &TypedError{Code: ErrOverrideRequiredCode}
	ErrApprovalRequired          = &TypedError{Code: ErrApprovalRequiredCode}
	ErrExpiredApproval           = &TypedError{Code: ErrExpiredApprovalCode}
	ErrSideEffectClassExceeded   = &TypedError{Code: ErrSideEffectClassExceededCode}
	ErrClaimRequired             = &TypedError{Code: ErrClaimRequiredCode}
	ErrClaimConflict             = &TypedError{Code: ErrClaimConflictCode}
	ErrStaleClaim                = &TypedError{Code: ErrStaleClaimCode}
	ErrAmbiguousLegacyState      = &TypedError{Code: ErrAmbiguousLegacyStateCode}
	ErrUnknownRecordVersion      = &TypedError{Code: ErrUnknownRecordVersionCode}
	ErrInvalidRecord             = &TypedError{Code: ErrInvalidRecordCode}
	ErrUnsupportedHostCapability = &TypedError{Code: ErrUnsupportedHostCapabilityCode}
	ErrInvocationInterrupted     = &TypedError{Code: ErrInvocationInterruptedCode}
)

type TypedError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message,omitempty"`
}

func (e *TypedError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

func (e *TypedError) Is(target error) bool {
	var typed *TypedError
	if !errors.As(target, &typed) {
		return false
	}
	return e.Code == typed.Code
}

func typed(code ErrorCode, format string, args ...any) error {
	msg := ""
	if format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	return &TypedError{Code: code, Message: msg}
}

const (
	SchemaDeliveryRun     = "loopcoder.delivery_run.v1"
	SchemaTask            = "loopcoder.task.v1"
	SchemaDependencyEdge  = "loopcoder.dependency_edge.v1"
	SchemaAttempt         = "loopcoder.attempt.v1"
	SchemaDecision        = "loopcoder.decision.v1"
	SchemaApproval        = "loopcoder.approval.v1"
	SchemaOverride        = "loopcoder.override.v1"
	SchemaPlanFingerprint = "loopcoder.plan_fingerprint.v1"
	CanonicalJSONVersion  = "loopcoder.canonical_json.v1"
)

type Actor struct {
	ActorKind         string `json:"actor_kind"`
	ActorID           string `json:"actor_id"`
	Display           string `json:"display,omitempty"`
	DecisionAuthority string `json:"decision_authority"`
	Source            string `json:"source"`
}

type Host struct {
	HostKind         string `json:"host_kind"`
	HostID           string `json:"host_id"`
	SessionID        string `json:"session_id"`
	ProcessID        int    `json:"process_id"`
	LoopcoderVersion string `json:"loopcoder_version"`
	Platform         string `json:"platform"`
}

type DeliveryRun struct {
	SchemaVersion            string   `json:"schema_version"`
	RecordVersion            int      `json:"record_version"`
	DeliveryRunID            string   `json:"delivery_run_id"`
	RunID                    string   `json:"run_id"`
	ProjectID                string   `json:"project_id"`
	RootRunID                string   `json:"root_run_id"`
	ParentRunID              string   `json:"parent_run_id,omitempty"`
	State                    string   `json:"state"`
	IntentSummary            string   `json:"intent_summary"`
	InputFingerprint         string   `json:"input_fingerprint,omitempty"`
	PolicyFingerprint        string   `json:"policy_fingerprint,omitempty"`
	PlanFingerprint          string   `json:"plan_fingerprint,omitempty"`
	AuthorizationFingerprint string   `json:"authorization_fingerprint,omitempty"`
	PolicyVersion            string   `json:"policy_version"`
	MaxSideEffectClass       string   `json:"max_side_effect_class"`
	ApprovalStatus           string   `json:"approval_status"`
	OverrideStatus           string   `json:"override_status"`
	CreatedAt                string   `json:"created_at"`
	UpdatedAt                string   `json:"updated_at"`
	PlannedAt                string   `json:"planned_at,omitempty"`
	ApprovedAt               string   `json:"approved_at,omitempty"`
	StartedAt                string   `json:"started_at,omitempty"`
	EndedAt                  string   `json:"ended_at,omitempty"`
	CreatedBy                Actor    `json:"created_by"`
	UpdatedBy                Actor    `json:"updated_by"`
	Host                     Host     `json:"host"`
	TerminalErrorCode        string   `json:"terminal_error_code,omitempty"`
	ErrorMessage             string   `json:"error_message,omitempty"`
	ReportIDs                []string `json:"report_ids,omitempty"`
	LegacyIDSource           string   `json:"legacy_id_source,omitempty"`
}

type Task struct {
	SchemaVersion            string   `json:"schema_version"`
	RecordVersion            int      `json:"record_version"`
	TaskID                   string   `json:"task_id"`
	TaskKey                  string   `json:"task_key"`
	DeliveryRunID            string   `json:"delivery_run_id"`
	ProjectID                string   `json:"project_id"`
	State                    string   `json:"state"`
	Title                    string   `json:"title"`
	RequirementsJSON         string   `json:"requirements_json"`
	ScopeJSON                string   `json:"scope_json"`
	Permission               string   `json:"permission"`
	SideEffectClass          string   `json:"side_effect_class"`
	PolicyVersion            string   `json:"policy_version"`
	PlanFingerprint          string   `json:"plan_fingerprint"`
	AuthorizationFingerprint string   `json:"authorization_fingerprint,omitempty"`
	AttemptCount             int      `json:"attempt_count"`
	ActiveAttemptID          string   `json:"active_attempt_id,omitempty"`
	DependsOn                []string `json:"depends_on"`
	CreatedAt                string   `json:"created_at"`
	UpdatedAt                string   `json:"updated_at"`
	ReadyAt                  string   `json:"ready_at,omitempty"`
	StartedAt                string   `json:"started_at,omitempty"`
	EndedAt                  string   `json:"ended_at,omitempty"`
	CreatedBy                Actor    `json:"created_by"`
	UpdatedBy                Actor    `json:"updated_by"`
	Host                     Host     `json:"host"`
	TerminalErrorCode        string   `json:"terminal_error_code,omitempty"`
	ErrorMessage             string   `json:"error_message,omitempty"`
}

type DependencyEdge struct {
	SchemaVersion   string `json:"schema_version"`
	RecordVersion   int    `json:"record_version"`
	EdgeID          string `json:"edge_id"`
	DeliveryRunID   string `json:"delivery_run_id"`
	ProjectID       string `json:"project_id"`
	FromTaskID      string `json:"from_task_id"`
	ToTaskID        string `json:"to_task_id"`
	EdgeKind        string `json:"edge_kind"`
	Ordinal         int    `json:"ordinal"`
	PlanFingerprint string `json:"plan_fingerprint"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	CreatedBy       Actor  `json:"created_by"`
	Host            Host   `json:"host"`
}

type Attempt struct {
	SchemaVersion          string `json:"schema_version"`
	RecordVersion          int    `json:"record_version"`
	AttemptID              string `json:"attempt_id"`
	TaskID                 string `json:"task_id"`
	DeliveryRunID          string `json:"delivery_run_id"`
	ProjectID              string `json:"project_id"`
	AttemptOrdinal         int    `json:"attempt_ordinal"`
	State                  string `json:"state"`
	ClaimGeneration        int64  `json:"claim_generation,omitempty"`
	ExecutorID             string `json:"executor_id,omitempty"`
	ProviderIdempotencyKey string `json:"provider_idempotency_key,omitempty"`
	ProviderReceipt        string `json:"provider_receipt,omitempty"`
	SideEffectClass        string `json:"side_effect_class"`
	StartedAt              string `json:"started_at,omitempty"`
	EndedAt                string `json:"ended_at,omitempty"`
	CreatedAt              string `json:"created_at"`
	UpdatedAt              string `json:"updated_at"`
	CreatedBy              Actor  `json:"created_by"`
	UpdatedBy              Actor  `json:"updated_by"`
	Host                   Host   `json:"host"`
	ReportID               string `json:"report_id,omitempty"`
	TerminalErrorCode      string `json:"terminal_error_code,omitempty"`
	ErrorMessage           string `json:"error_message,omitempty"`
}

type Decision struct {
	SchemaVersion     string `json:"schema_version"`
	RecordVersion     int    `json:"record_version"`
	DecisionID        string `json:"decision_id"`
	DecisionKey       string `json:"decision_key"`
	DecisionKind      string `json:"decision_kind"`
	DeliveryRunID     string `json:"delivery_run_id"`
	TaskID            string `json:"task_id,omitempty"`
	ProjectID         string `json:"project_id"`
	DecidedBy         Actor  `json:"decided_by"`
	InputsFingerprint string `json:"inputs_fingerprint"`
	OutputJSON        string `json:"output_json"`
	AlternativesJSON  string `json:"alternatives_json,omitempty"`
	Heuristic         bool   `json:"heuristic"`
	HeuristicReason   string `json:"heuristic_reason,omitempty"`
	PolicyVersion     string `json:"policy_version"`
	SideEffectClass   string `json:"side_effect_class"`
	CreatedAt         string `json:"created_at"`
	CreatedBy         Actor  `json:"created_by"`
	Host              Host   `json:"host"`
}

type Approval struct {
	SchemaVersion            string `json:"schema_version"`
	RecordVersion            int    `json:"record_version"`
	ApprovalID               string `json:"approval_id"`
	DeliveryRunID            string `json:"delivery_run_id"`
	ProjectID                string `json:"project_id"`
	ApprovalKind             string `json:"approval_kind"`
	AuthorizationFingerprint string `json:"authorization_fingerprint"`
	InputFingerprint         string `json:"input_fingerprint"`
	PolicyFingerprint        string `json:"policy_fingerprint"`
	PlanFingerprint          string `json:"plan_fingerprint"`
	ApprovedSideEffectClass  string `json:"approved_side_effect_class"`
	ApprovedScopeJSON        string `json:"approved_scope_json"`
	ApprovedBy               Actor  `json:"approved_by"`
	Status                   string `json:"status"`
	ApprovedAt               string `json:"approved_at,omitempty"`
	ExpiresAt                string `json:"expires_at,omitempty"`
	CreatedAt                string `json:"created_at"`
	UpdatedAt                string `json:"updated_at"`
	CreatedBy                Actor  `json:"created_by"`
	UpdatedBy                Actor  `json:"updated_by"`
	Host                     Host   `json:"host"`
}

type Override struct {
	SchemaVersion            string `json:"schema_version"`
	RecordVersion            int    `json:"record_version"`
	OverrideID               string `json:"override_id"`
	DeliveryRunID            string `json:"delivery_run_id"`
	ProjectID                string `json:"project_id"`
	OverrideKind             string `json:"override_kind"`
	AuthorizationFingerprint string `json:"authorization_fingerprint"`
	InputFingerprint         string `json:"input_fingerprint"`
	PolicyFingerprint        string `json:"policy_fingerprint"`
	PlanFingerprint          string `json:"plan_fingerprint"`
	PolicyDecisionID         string `json:"policy_decision_id"`
	OverriddenBy             Actor  `json:"overridden_by"`
	Justification            string `json:"justification"`
	LimitsJSON               string `json:"limits_json"`
	Status                   string `json:"status"`
	CreatedAt                string `json:"created_at"`
	UpdatedAt                string `json:"updated_at"`
	ExpiresAt                string `json:"expires_at,omitempty"`
	CreatedBy                Actor  `json:"created_by"`
	UpdatedBy                Actor  `json:"updated_by"`
	Host                     Host   `json:"host"`
}

type PlanFingerprint struct {
	SchemaVersion            string `json:"schema_version"`
	RecordVersion            int    `json:"record_version"`
	FingerprintID            string `json:"fingerprint_id"`
	DeliveryRunID            string `json:"delivery_run_id"`
	ProjectID                string `json:"project_id"`
	InputFingerprint         string `json:"input_fingerprint"`
	PolicyFingerprint        string `json:"policy_fingerprint"`
	PlanFingerprint          string `json:"plan_fingerprint"`
	AuthorizationFingerprint string `json:"authorization_fingerprint"`
	CanonicalizationVersion  string `json:"canonicalization_version"`
	Algorithm                string `json:"algorithm"`
	CanonicalInputsJSON      string `json:"canonical_inputs_json,omitempty"`
	CreatedAt                string `json:"created_at"`
	CreatedBy                Actor  `json:"created_by"`
	Host                     Host   `json:"host"`
}
