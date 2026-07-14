// Package simulation implements the deterministic v0.8 orchestration
// simulation fixture and result contract.
package simulation

import (
	"encoding/json"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	ScenarioSchemaV1      = "loopcoder.evaluation.simulation_scenario.v1"
	ResultSchemaV1        = "loopcoder.evaluation.simulation_result.v1"
	ReplayJournalSchemaV1 = "loopcoder.evaluation.simulation_replay_journal.v1"

	ErrInvalidFixture       = "ErrInvalidFixture"
	ErrUnknownSchemaVersion = "ErrUnknownSchemaVersion"
	ErrMissingReference     = "ErrMissingReference"
	ErrBoundExceeded        = "ErrBoundExceeded"
	ErrQuotaUnknown         = "ErrQuotaUnknown"
	ErrQuotaInsufficient    = "ErrQuotaInsufficient"
	ErrBudgetDenied         = "ErrBudgetDenied"
	ErrInvariantFailed      = "ErrInvariantFailed"
	ErrReplayMismatch       = "ErrReplayMismatch"
	ErrCrashInjected        = "ErrCrashInjected"
	ErrProviderFailure      = "ErrProviderFailure"
)

type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type TypedError struct {
	Code        string       `json:"code"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func (e *TypedError) Error() string {
	if e == nil {
		return ""
	}
	if len(e.Diagnostics) == 0 || e.Diagnostics[0].Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Diagnostics[0].Message
}

type Scenario struct {
	SchemaVersion     string                     `json:"schema_version"`
	ScenarioID        string                     `json:"scenario_id"`
	Seed              int64                      `json:"seed"`
	Clock             ClockPlan                  `json:"clock"`
	Host              HostRuntime                `json:"host,omitempty"`
	Limits            Limits                     `json:"limits"`
	DurableSourceIDs  DurableSourceIDs           `json:"durable_source_ids"`
	PolicyProvenance  PolicyProvenance           `json:"policy_provenance"`
	StartingState     DurableState               `json:"starting_state,omitempty"`
	Inventory         Inventory                  `json:"inventory"`
	BudgetAuthorities []BudgetAuthority          `json:"budget_authorities"`
	Tasks             []Task                     `json:"tasks"`
	InjectedEvents    []InjectedEvent            `json:"injected_events"`
	ConcurrencyScript []ConcurrencyOrder         `json:"concurrency_script,omitempty"`
	Invariants        []Invariant                `json:"invariants"`
	Extensions        map[string]json.RawMessage `json:"-"`
}

type ClockPlan struct {
	Origin string `json:"origin"`
	StepMS int64  `json:"step_ms"`
}

type HostRuntime struct {
	ConductorHostID string           `json:"conductor_host_id,omitempty"`
	AdapterID       string           `json:"adapter_id,omitempty"`
	Capabilities    HostCapabilities `json:"capabilities,omitempty"`
}

type HostCapabilities struct {
	SupportsPush   bool `json:"supports_push"`
	SupportsWake   bool `json:"supports_wake"`
	SupportsFollow bool `json:"supports_follow"`
	SupportsPoll   bool `json:"supports_poll"`
}

type Limits struct {
	MaxEvents  int `json:"max_events"`
	MaxSteps   int `json:"max_steps"`
	MaxDepth   int `json:"max_depth"`
	MaxBreadth int `json:"max_breadth"`
}

type DurableSourceIDs struct {
	ProjectID         string `json:"project_id"`
	DeliveryRunID     string `json:"delivery_run_id"`
	InventorySourceID string `json:"inventory_source_id"`
	BudgetSourceID    string `json:"budget_source_id"`
	RoutingSourceID   string `json:"routing_source_id"`
	AgentTreeSourceID string `json:"agent_tree_source_id"`
	HandoffSourceID   string `json:"handoff_source_id"`
	DurableStateID    string `json:"durable_state_id"`
}

type PolicyProvenance struct {
	PolicyVersion            string `json:"policy_version"`
	PolicyFingerprint        string `json:"policy_fingerprint"`
	PlanFingerprint          string `json:"plan_fingerprint"`
	AuthorizationFingerprint string `json:"authorization_fingerprint"`
	RoutingPolicyProfileID   string `json:"routing_policy_profile_id"`
}

type Inventory struct {
	Providers []Provider           `json:"providers"`
	Accounts  []Account            `json:"accounts"`
	Models    []Model              `json:"models"`
	Quotas    []QuotaWindow        `json:"quotas"`
	Latencies []LatencyExpectation `json:"latencies,omitempty"`
	Failures  []ProviderFailure    `json:"failures,omitempty"`
}

type Provider struct {
	ProviderInstallationID string                              `json:"provider_installation_id"`
	AdapterID              string                              `json:"adapter_id"`
	State                  providerinventory.InstallationState `json:"state"`
	DiscoverySource        providerinventory.DiscoverySource   `json:"discovery_source"`
}

type Account struct {
	AccountProfileID       string                           `json:"account_profile_id"`
	AdapterID              string                           `json:"adapter_id"`
	ProviderInstallationID string                           `json:"provider_installation_id"`
	Readiness              providerinventory.ReadinessState `json:"readiness"`
}

type Model struct {
	ModelCapabilityID string                              `json:"model_capability_id"`
	AdapterID         string                              `json:"adapter_id"`
	AccountProfileID  string                              `json:"account_profile_id"`
	CanonicalModelID  string                              `json:"canonical_model_id"`
	Availability      providerinventory.AvailabilityState `json:"availability"`
	Roles             []providerinventory.CatalogRole     `json:"roles"`
	CostMicrounits    int64                               `json:"cost_microunits"`
}

type QuotaWindow struct {
	QuotaSnapshotID   string                         `json:"quota_snapshot_id"`
	AdapterID         string                         `json:"adapter_id"`
	AccountProfileID  string                         `json:"account_profile_id"`
	ModelCapabilityID string                         `json:"model_capability_id"`
	QuantityKind      providerinventory.QuantityKind `json:"quantity_kind"`
	WindowKind        providerinventory.WindowKind   `json:"window_kind"`
	Confidence        providerinventory.Confidence   `json:"confidence"`
	LimitValue        *int64                         `json:"limit_value,omitempty"`
	RemainingValue    *int64                         `json:"remaining_value,omitempty"`
	WindowStart       string                         `json:"window_start,omitempty"`
	WindowEnd         string                         `json:"window_end,omitempty"`
}

type LatencyExpectation struct {
	ModelCapabilityID string `json:"model_capability_id"`
	LatencyMS         int64  `json:"latency_ms"`
	SourceID          string `json:"source_id"`
}

type ProviderFailure struct {
	ModelCapabilityID string `json:"model_capability_id"`
	FailureCode       string `json:"failure_code"`
	AtCallOrdinal     int    `json:"at_call_ordinal"`
	Retryable         bool   `json:"retryable"`
	CostMicrounits    int64  `json:"cost_microunits"`
}

type BudgetAuthority struct {
	BudgetAuthorityID string                         `json:"budget_authority_id"`
	QuantityKind      providerinventory.QuantityKind `json:"quantity_kind"`
	Unit              string                         `json:"unit"`
	LimitValue        int64                          `json:"limit_value"`
	RemainingValue    int64                          `json:"remaining_value"`
	Confidence        providerinventory.Confidence   `json:"confidence"`
	PolicyVersion     string                         `json:"policy_version"`
}

type Task struct {
	TaskID              string                         `json:"task_id"`
	TaskKey             string                         `json:"task_key"`
	Role                providerinventory.CatalogRole  `json:"role"`
	UserPinnedModelID   string                         `json:"user_pinned_model_id,omitempty"`
	UserOverrideModelID string                         `json:"user_override_model_id,omitempty"`
	RequiredQuantity    int64                          `json:"required_quantity"`
	QuantityKind        providerinventory.QuantityKind `json:"quantity_kind"`
	BudgetAuthorityID   string                         `json:"budget_authority_id"`
	CandidateModelIDs   []string                       `json:"candidate_model_ids"`
	HandoffTargetTaskID string                         `json:"handoff_target_task_id,omitempty"`
	AgentDepth          int                            `json:"agent_depth"`
	OwnerResourceKey    string                         `json:"owner_resource_key"`
}

type InjectedEvent struct {
	EventID          string `json:"event_id"`
	Kind             string `json:"kind"`
	TaskID           string `json:"task_id,omitempty"`
	ConcurrencyGroup string `json:"concurrency_group,omitempty"`
	PayloadRef       string `json:"payload_ref,omitempty"`
}

type ConcurrencyOrder struct {
	Group    string   `json:"group"`
	EventIDs []string `json:"event_ids"`
}

type Invariant struct {
	InvariantID string `json:"invariant_id"`
	Kind        string `json:"kind"`
	TaskID      string `json:"task_id,omitempty"`
	Expected    string `json:"expected,omitempty"`
}

type Options struct {
	ReplayJournal *ReplayJournal
	CrashAfter    int
}

type Result struct {
	SchemaVersion    string                     `json:"schema_version"`
	ScenarioID       string                     `json:"scenario_id"`
	Seed             int64                      `json:"seed"`
	Clock            ClockPlan                  `json:"clock"`
	StartedAt        string                     `json:"started_at"`
	EndedAt          string                     `json:"ended_at"`
	PolicyProvenance PolicyProvenance           `json:"policy_provenance"`
	DurableSourceIDs DurableSourceIDs           `json:"durable_source_ids"`
	EventLog         []EventRecord              `json:"event_log"`
	Decisions        []DecisionRecord           `json:"decisions"`
	InvariantResults []InvariantResult          `json:"invariant_results"`
	Diff             StableDiff                 `json:"diff"`
	DurableState     DurableState               `json:"durable_state"`
	ReplayJournal    ReplayJournal              `json:"replay_journal"`
	Diagnostics      []Diagnostic               `json:"diagnostics,omitempty"`
	Truncated        bool                       `json:"truncated"`
	TruncationReason string                     `json:"truncation_reason,omitempty"`
	Extensions       map[string]json.RawMessage `json:"extensions,omitempty"`
}

type EventRecord struct {
	Sequence       int               `json:"sequence"`
	EventID        string            `json:"event_id"`
	EventKind      string            `json:"event_kind"`
	TaskID         string            `json:"task_id,omitempty"`
	At             string            `json:"at"`
	DecisionID     string            `json:"decision_id,omitempty"`
	Status         string            `json:"status"`
	ReceiptID      string            `json:"receipt_id,omitempty"`
	IdempotencyKey string            `json:"idempotency_key"`
	Details        map[string]string `json:"details,omitempty"`
}

type DecisionRecord struct {
	DecisionID               string      `json:"decision_id"`
	EventID                  string      `json:"event_id"`
	TaskID                   string      `json:"task_id"`
	DecisionKind             string      `json:"decision_kind"`
	Accepted                 bool        `json:"accepted"`
	ChosenCandidateID        string      `json:"chosen_candidate_id,omitempty"`
	UserPinnedModelID        string      `json:"user_pinned_model_id,omitempty"`
	UserOverrideModelID      string      `json:"user_override_model_id,omitempty"`
	RejectedCandidates       []Rejection `json:"rejected_candidates"`
	QuotaSnapshotIDs         []string    `json:"quota_snapshot_ids"`
	BudgetAuthorityID        string      `json:"budget_authority_id,omitempty"`
	PolicyFingerprint        string      `json:"policy_fingerprint"`
	PlanFingerprint          string      `json:"plan_fingerprint"`
	AuthorizationFingerprint string      `json:"authorization_fingerprint"`
	Reason                   string      `json:"reason"`
}

type Rejection struct {
	CandidateID string `json:"candidate_id"`
	Code        string `json:"code"`
	Reason      string `json:"reason"`
}

type InvariantResult struct {
	InvariantID string `json:"invariant_id"`
	Kind        string `json:"kind"`
	Passed      bool   `json:"passed"`
	Diagnostic  string `json:"diagnostic,omitempty"`
}

type StableDiff struct {
	BeforeHash       string   `json:"before_hash"`
	AfterHash        string   `json:"after_hash"`
	Changed          bool     `json:"changed"`
	AddedEventIDs    []string `json:"added_event_ids"`
	AddedDecisionIDs []string `json:"added_decision_ids"`
}

type DurableState struct {
	AppliedEventIDs   []string           `json:"applied_event_ids,omitempty"`
	ProviderReceipts  []ProviderReceipt  `json:"provider_receipts,omitempty"`
	HostDeliveries    []HostDelivery     `json:"host_deliveries,omitempty"`
	PartialResults    []PartialResult    `json:"partial_results,omitempty"`
	OwnerConflicts    []OwnerConflict    `json:"owner_conflicts,omitempty"`
	BudgetCommitments []BudgetCommitment `json:"budget_commitments,omitempty"`
	Handoffs          []HandoffRecord    `json:"handoffs,omitempty"`
	AgentOwners       []AgentOwner       `json:"agent_owners,omitempty"`
	CompletedTaskIDs  []string           `json:"completed_task_ids,omitempty"`
}

type ProviderReceipt struct {
	ReceiptID         string `json:"receipt_id"`
	EventID           string `json:"event_id"`
	TaskID            string `json:"task_id"`
	ModelCapabilityID string `json:"model_capability_id"`
	Status            string `json:"status"`
	FailureCode       string `json:"failure_code,omitempty"`
	LatencyMS         int64  `json:"latency_ms"`
	CostMicrounits    int64  `json:"cost_microunits"`
}

type HostDelivery struct {
	DeliveryID        string `json:"delivery_id"`
	EventID           string `json:"event_id"`
	TaskID            string `json:"task_id"`
	HostID            string `json:"host_id"`
	PayloadRef        string `json:"payload_ref"`
	Transport         string `json:"transport"`
	Durable           bool   `json:"durable"`
	FollowScheduled   bool   `json:"follow_scheduled"`
	PollScheduled     bool   `json:"poll_scheduled"`
	PushAttempted     bool   `json:"push_attempted"`
	WakeAttempted     bool   `json:"wake_attempted"`
	VisibilityClaimed bool   `json:"visibility_claimed"`
	Acknowledged      bool   `json:"acknowledged"`
	ExternalCall      bool   `json:"external_call"`
}

type PartialResult struct {
	PartialResultID string `json:"partial_result_id"`
	EventID         string `json:"event_id"`
	TaskID          string `json:"task_id"`
	ParentTaskID    string `json:"parent_task_id"`
	Status          string `json:"status"`
	Durable         bool   `json:"durable"`
	ExternalCall    bool   `json:"external_call"`
}

type OwnerConflict struct {
	ConflictID        string `json:"conflict_id"`
	EventID           string `json:"event_id"`
	TaskID            string `json:"task_id"`
	ResourceKey       string `json:"resource_key"`
	ExistingOwnerTask string `json:"existing_owner_task"`
	ConflictState     string `json:"conflict_state"`
	ExternalCall      bool   `json:"external_call"`
}

type BudgetCommitment struct {
	CommitmentID      string `json:"commitment_id"`
	EventID           string `json:"event_id"`
	TaskID            string `json:"task_id"`
	BudgetAuthorityID string `json:"budget_authority_id"`
	QuantityKind      string `json:"quantity_kind"`
	CommittedValue    int64  `json:"committed_value"`
}

type HandoffRecord struct {
	HandoffID        string `json:"handoff_id"`
	EventID          string `json:"event_id"`
	SourceTaskID     string `json:"source_task_id"`
	TargetTaskID     string `json:"target_task_id"`
	AuthorizationRef string `json:"authorization_ref"`
}

type AgentOwner struct {
	OwnershipID     string `json:"ownership_id"`
	EventID         string `json:"event_id"`
	TaskID          string `json:"task_id"`
	ResourceKey     string `json:"resource_key"`
	OwnerState      string `json:"owner_state"`
	Permission      string `json:"permission"`
	SideEffectClass string `json:"side_effect_class"`
}

type ReplayJournal struct {
	SchemaVersion string           `json:"schema_version"`
	ScenarioID    string           `json:"scenario_id"`
	Seed          int64            `json:"seed"`
	Events        []EventRecord    `json:"events"`
	Decisions     []DecisionRecord `json:"decisions"`
}

func defaultLimits(l Limits) Limits {
	if l.MaxEvents == 0 {
		l.MaxEvents = 1000
	}
	if l.MaxSteps == 0 {
		l.MaxSteps = 1000
	}
	if l.MaxDepth == 0 {
		l.MaxDepth = 8
	}
	if l.MaxBreadth == 0 {
		l.MaxBreadth = 64
	}
	return l
}

func permissionForTask(_ Task) string {
	return storage.PermissionOrchestrate
}

func sideEffectForTask(_ Task) string {
	return storage.SideEffectProviderLaunch
}
