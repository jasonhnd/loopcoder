package routing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/availability"
	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/sanitize"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	DecisionSchema                = "loopcoder.routing_decision.v1"
	DecisionKindRouting           = "routing"
	DefaultProfileID              = "rprof_balanced_v1"
	DefaultProfileVersion         = "balanced-v1"
	DefaultTieBreakSeed           = "0804-routing-balanced-v1"
	CandidateGenerationFull       = "complete"
	CandidateGenerationNeedsHuman = "needs-human"
	CandidateGenerationZeroReason = "candidate generation produced zero candidates from scoped provider inventory; run loopcoder providers refresh --repo . --format json and ensure a matching provider installation and model capability are configured"
	DecisionStatusSelected        = "selected"
	DecisionStatusNoEligible      = "no-eligible-candidate"

	StrategyQualityFirst     = "quality-first"
	StrategyBalanced         = "balanced"
	StrategyBurnBeforeReset  = "burn-before-reset"
	StrategyVersion          = "reset-aware-v1"
	DefaultTargetUtilization = int64(9200)
)

type ComponentName string

const (
	ComponentQualityFit     ComponentName = "quality_fit"
	ComponentQuotaHeadroom  ComponentName = "quota_headroom"
	ComponentExpiryUrgency  ComponentName = "expiry_urgency"
	ComponentTaskHeadroom   ComponentName = "task_equivalent_headroom"
	ComponentCapacityTrust  ComponentName = "capacity_trust"
	ComponentAvailability   ComponentName = "availability"
	ComponentHealth         ComponentName = "health"
	ComponentCost           ComponentName = "cost"
	ComponentLatency        ComponentName = "latency"
	ComponentDiversity      ComponentName = "diversity"
	ComponentLocality       ComponentName = "locality"
	ComponentUserPreference ComponentName = "user_preference"
)

var supportedComponents = map[ComponentName]bool{
	ComponentAvailability:   true,
	ComponentQuotaHeadroom:  true,
	ComponentExpiryUrgency:  true,
	ComponentTaskHeadroom:   true,
	ComponentCapacityTrust:  true,
	ComponentQualityFit:     true,
	ComponentLatency:        true,
	ComponentCost:           true,
	ComponentDiversity:      true,
	ComponentHealth:         true,
	ComponentLocality:       true,
	ComponentUserPreference: true,
}

var scoringComponentOrder = []ComponentName{
	ComponentExpiryUrgency,
	ComponentTaskHeadroom,
	ComponentCapacityTrust,
	ComponentAvailability,
	ComponentQuotaHeadroom,
	ComponentQualityFit,
	ComponentLatency,
	ComponentCost,
	ComponentDiversity,
	ComponentHealth,
	ComponentLocality,
	ComponentUserPreference,
}

type OptimizationPolicy struct {
	SchemaVersion          string                `json:"schema_version"`
	RoutingPolicyProfileID string                `json:"routing_policy_profile_id"`
	ProfileKey             string                `json:"profile_key"`
	ProfileVersion         string                `json:"profile_version"`
	PolicyVersion          string                `json:"policy_version"`
	StrategyKey            string                `json:"strategy_key"`
	StrategyVersion        string                `json:"strategy_version"`
	TargetUtilizationBP    int64                 `json:"target_utilization_basis_points"`
	CompletionReserveBP    int64                 `json:"completion_reserve_basis_points"`
	VerificationReserveBP  int64                 `json:"verification_reserve_basis_points"`
	AllowPaidOverage       bool                  `json:"allow_paid_overage"`
	ResetBands             []ResetBand           `json:"reset_bands"`
	Weights                map[ComponentName]int `json:"weights"`
	TieBreakSeed           string                `json:"tie_break_seed"`
	LocalAdapterIDs        []string              `json:"local_adapter_ids"`
	PreferredCandidateIDs  []string              `json:"preferred_candidate_ids"`
	DiversityHistory       []SelectedRouteRef    `json:"diversity_history"`
	PolicyFingerprint      string                `json:"policy_fingerprint"`
}

type ResetBand struct {
	Name                 string `json:"name"`
	MinResetSeconds      int64  `json:"min_reset_seconds"`
	MaxResetSeconds      int64  `json:"max_reset_seconds,omitempty"`
	MaxTaskClass         string `json:"max_task_class"`
	ExpiryUrgencyScore   int    `json:"expiry_urgency_score"`
	ExpectedWasteAvoided int64  `json:"expected_waste_avoided,omitempty"`
}

type SelectedRouteRef struct {
	RoutingDecisionID  string `json:"routing_decision_id,omitempty"`
	RoutingCandidateID string `json:"routing_candidate_id,omitempty"`
	AdapterID          string `json:"adapter_id,omitempty"`
	AccountProfileID   string `json:"account_profile_id,omitempty"`
	ModelCapabilityID  string `json:"model_capability_id,omitempty"`
}

type DecisionInput struct {
	ProjectID                string
	DeliveryRunID            string
	DecisionKey              string
	TaskRequirementID        string
	RoleDefinitionID         string
	PlanFingerprint          string
	PolicyFingerprint        string
	AuthorizationFingerprint string
	PriorRoutingDecisionID   string
	PriorRoutingFingerprint  string
	RoutingPolicyProfileID   string
	RoutingPolicyProfile     RoutingPolicyProfile
	PolicyInputRecords       []PolicyInputRecord
	OverrideProvenance       []OverrideProvenance
	Inputs                   Inputs
	OptimizationPolicy       OptimizationPolicy
	DecidedBy                delivery.Actor
	Host                     delivery.Host
	Now                      time.Time
}

type InputRecordRef struct {
	RecordKind  string `json:"record_kind"`
	RecordID    string `json:"record_id"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type ComponentScore struct {
	Name              ComponentName                    `json:"name"`
	Score             int                              `json:"score"`
	Weight            int                              `json:"weight"`
	WeightedScore     string                           `json:"weighted_score"`
	EvidenceValue     *int64                           `json:"evidence_value,omitempty"`
	ResetAt           string                           `json:"reset_at,omitempty"`
	ResetWindow       string                           `json:"reset_window,omitempty"`
	TaskClass         string                           `json:"task_class,omitempty"`
	BudgetClass       BudgetClass                      `json:"budget_class,omitempty"`
	DeadlineClass     DeadlineClass                    `json:"deadline_class,omitempty"`
	ReserveBasisBP    int64                            `json:"reserve_basis_points,omitempty"`
	ExpectedWaste     *int64                           `json:"expected_waste_avoided,omitempty"`
	Confidence        providerinventory.Confidence     `json:"confidence"`
	Heuristic         bool                             `json:"heuristic"`
	HeuristicReason   string                           `json:"heuristic_reason,omitempty"`
	EvidenceRecordIDs []string                         `json:"evidence_record_ids"`
	SnapshotIDs       []string                         `json:"snapshot_ids"`
	FreshnessState    providerinventory.FreshnessState `json:"freshness_state,omitempty"`
}

type ScoredCandidate struct {
	RoutingCandidateID string                       `json:"routing_candidate_id"`
	Candidate          Candidate                    `json:"candidate"`
	Components         []ComponentScore             `json:"components"`
	TotalScore         string                       `json:"total_score"`
	TotalBasisPoints   int                          `json:"total_basis_points"`
	TieBreakValue      string                       `json:"tie_break_value"`
	Rank               int                          `json:"rank"`
	Confidence         providerinventory.Confidence `json:"confidence"`
	Heuristic          bool                         `json:"heuristic"`
	WhyNotSelected     string                       `json:"why_not_selected,omitempty"`
}

type RoutingDecision struct {
	SchemaVersion             string                     `json:"schema_version"`
	RecordVersion             int                        `json:"record_version"`
	RoutingDecisionID         string                     `json:"routing_decision_id"`
	DecisionKey               string                     `json:"decision_key"`
	DecisionKind              string                     `json:"decision_kind"`
	ProjectID                 string                     `json:"project_id"`
	DeliveryRunID             string                     `json:"delivery_run_id"`
	TaskID                    string                     `json:"task_id"`
	TaskRequirementID         string                     `json:"task_requirement_id"`
	RoutingPolicyProfileID    string                     `json:"routing_policy_profile_id"`
	RoleDefinitionID          string                     `json:"role_definition_id"`
	PlanFingerprint           string                     `json:"plan_fingerprint"`
	PolicyFingerprint         string                     `json:"policy_fingerprint"`
	AuthorizationFingerprint  string                     `json:"authorization_fingerprint,omitempty"`
	RuntimeHostName           string                     `json:"runtime_host_name,omitempty"`
	BudgetClass               BudgetClass                `json:"budget_class"`
	DeadlineClass             DeadlineClass              `json:"deadline_class"`
	RoutingFingerprint        string                     `json:"routing_fingerprint"`
	InputRecordRefs           []InputRecordRef           `json:"input_record_refs"`
	CandidateGenerationStatus string                     `json:"candidate_generation_status"`
	EligibleCandidates        []Candidate                `json:"eligible_candidates"`
	RejectedCandidates        []RejectedCandidate        `json:"rejected_candidates"`
	ScoredCandidates          []ScoredCandidate          `json:"scored_candidates"`
	ChosenCandidateID         string                     `json:"chosen_candidate_id,omitempty"`
	ChosenReason              string                     `json:"chosen_reason,omitempty"`
	UserPinRefs               []string                   `json:"user_pin_refs"`
	FallbackChain             []string                   `json:"fallback_chain"`
	BreakerGateRefs           []string                   `json:"breaker_gate_refs"`
	RoutingPolicyProfile      *RoutingPolicyProfile      `json:"routing_policy_profile,omitempty"`
	PolicyInputRecords        []PolicyInputRecord        `json:"policy_input_records,omitempty"`
	OverrideProvenance        []OverrideProvenance       `json:"override_provenance,omitempty"`
	OptimizationPolicy        OptimizationPolicy         `json:"optimization_policy"`
	HeuristicComponents       []ComponentScore           `json:"heuristic_components"`
	DecisionStatus            string                     `json:"decision_status"`
	RejectedSummary           map[RejectionCode]int      `json:"rejected_summary"`
	CreatedAt                 string                     `json:"created_at"`
	UpdatedAt                 string                     `json:"updated_at"`
	DecidedBy                 delivery.Actor             `json:"decided_by"`
	Host                      delivery.Host              `json:"host"`
	TerminalErrorCode         taskrequirements.ErrorCode `json:"terminal_error_code,omitempty"`
}

type DryRunExplain struct {
	Decision RoutingDecision `json:"decision"`
	Human    string          `json:"human"`
	Stable   json.RawMessage `json:"stable_json"`
}

type ReevaluateRouteTrigger string

const (
	ReevaluateAtTaskBoundary       ReevaluateRouteTrigger = "task-boundary"
	ReevaluateAtFreshCapacityEvent ReevaluateRouteTrigger = "fresh-capacity-event"
)

type ReevaluateRouteInput struct {
	DecisionInput DecisionInput
	Trigger       ReevaluateRouteTrigger
	DryRun        bool
}

type ReevaluateRouteResult struct {
	Decision RoutingDecision        `json:"decision"`
	Trigger  ReevaluateRouteTrigger `json:"trigger"`
	DryRun   bool                   `json:"dry_run"`
	Changed  bool                   `json:"changed"`
}

func ReevaluateRoute(ctx context.Context, store storage.Store, input ReevaluateRouteInput) (ReevaluateRouteResult, error) {
	if input.Trigger != ReevaluateAtTaskBoundary && input.Trigger != ReevaluateAtFreshCapacityEvent {
		return ReevaluateRouteResult{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing re-evaluation trigger must be task-boundary or fresh-capacity-event"}
	}
	if store == nil {
		return ReevaluateRouteResult{}, errors.New("routing re-evaluation: storage store is required")
	}
	decisionInput := input.DecisionInput
	decisionInput.Now = store.Now()
	prepared, err := prepareDecisionInputFromStore(ctx, store, decisionInput)
	if err != nil {
		return ReevaluateRouteResult{}, err
	}
	decision, err := BuildRoutingDecision(prepared)
	if err != nil {
		return ReevaluateRouteResult{}, err
	}
	changed, err := routingDecisionFingerprintChanged(ctx, store, decision.ProjectID, decision.DeliveryRunID, decision.DecisionKey, decision.RoutingFingerprint)
	if err != nil {
		return ReevaluateRouteResult{}, err
	}
	if !input.DryRun {
		if err := PersistRoutingDecision(ctx, store, decision); err != nil {
			return ReevaluateRouteResult{}, err
		}
	}
	if decision.TerminalErrorCode != "" {
		return ReevaluateRouteResult{Decision: decision, Trigger: input.Trigger, DryRun: input.DryRun, Changed: changed}, routingTerminalError(decision.TerminalErrorCode)
	}
	return ReevaluateRouteResult{Decision: decision, Trigger: input.Trigger, DryRun: input.DryRun, Changed: changed}, nil
}

func routingDecisionFingerprintChanged(ctx context.Context, store storage.Store, projectID, deliveryRunID, decisionKey, routingFingerprint string) (bool, error) {
	count := 0
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM routing_decisions WHERE project_id = ? AND delivery_run_id = ? AND decision_key = ? AND routing_fingerprint = ?`,
			strings.TrimSpace(projectID), strings.TrimSpace(deliveryRunID), strings.TrimSpace(decisionKey), strings.TrimSpace(routingFingerprint)).Scan(&count)
	})
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

func DryRunExplainRoute(input DecisionInput) (DryRunExplain, error) {
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		return DryRunExplain{}, err
	}
	stable, err := ExplainJSON(decision)
	if err != nil {
		return DryRunExplain{}, err
	}
	return DryRunExplain{
		Decision: decision,
		Human:    ExplainHuman(decision),
		Stable:   json.RawMessage(stable),
	}, nil
}

func DecideAndPersistRoute(ctx context.Context, store storage.Store, input DecisionInput) (RoutingDecision, error) {
	if store == nil {
		return RoutingDecision{}, errors.New("routing decision: storage store is required")
	}
	input.Now = store.Now()
	prepared, err := prepareDecisionInputFromStore(ctx, store, input)
	if err != nil {
		return RoutingDecision{}, err
	}
	input = prepared
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		return RoutingDecision{}, err
	}
	if err := PersistRoutingDecision(ctx, store, decision); err != nil {
		return RoutingDecision{}, err
	}
	stored, err := LoadRoutingDecision(ctx, store, decision.RoutingDecisionID)
	if err != nil {
		return RoutingDecision{}, err
	}
	if stored.TerminalErrorCode != "" {
		return stored, routingTerminalError(stored.TerminalErrorCode)
	}
	return stored, nil
}

func BuildRoutingDecision(input DecisionInput) (RoutingDecision, error) {
	if strings.TrimSpace(input.ProjectID) == "" || strings.TrimSpace(input.DeliveryRunID) == "" || strings.TrimSpace(input.DecisionKey) == "" {
		return RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "project_id, delivery_run_id, and decision_key are required"}
	}
	if hasRoutingPolicyProfile(input.RoutingPolicyProfile) {
		profile := normalizeRoutingPolicyProfile(input.RoutingPolicyProfile)
		if err := ValidateRoutingPolicyProfile(profile); err != nil {
			return RoutingDecision{}, err
		}
		input.RoutingPolicyProfile = profile
		if strings.TrimSpace(input.RoutingPolicyProfileID) == "" {
			input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
		}
		if isZeroOptimizationPolicy(input.OptimizationPolicy) {
			input.OptimizationPolicy = profile.OptimizationPolicy
		}
		if isZeroHardPolicy(input.Inputs.Policy) {
			input.Inputs.Policy = profile.EligibilityPolicy
		}
	}
	policy, err := normalizeOptimizationPolicy(input.OptimizationPolicy, input.RoutingPolicyProfileID)
	if err != nil {
		return RoutingDecision{}, err
	}
	expectedPolicyFingerprint := policy.PolicyFingerprint
	if hasRoutingPolicyProfile(input.RoutingPolicyProfile) {
		expectedPolicyFingerprint = input.RoutingPolicyProfile.PolicyFingerprint
	}
	if strings.TrimSpace(input.PolicyFingerprint) == "" {
		input.PolicyFingerprint = expectedPolicyFingerprint
	} else if strings.TrimSpace(input.PolicyFingerprint) != expectedPolicyFingerprint {
		return RoutingDecision{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "policy fingerprint does not match routing policy profile"}
	}
	if diagnostics := ValidateOverrideProvenance(input.OverrideProvenance, input.Now, expectedPolicyFingerprint, input.AuthorizationFingerprint); len(diagnostics) > 0 {
		return RoutingDecision{}, &taskrequirements.TypedError{Code: diagnostics[0].Code, Message: diagnostics[0].Message}
	}
	if err := validateOverrideDecisionScope(input.OverrideProvenance, input.DeliveryRunID, input.Inputs.Requirement.TaskID); err != nil {
		return RoutingDecision{}, err
	}
	budgetClass, deadlineClass, err := resolveTaskFitClasses(input.Inputs.Requirement, input.Inputs.BudgetClass, input.Inputs.DeadlineClass)
	if err != nil {
		return RoutingDecision{}, err
	}
	input.Inputs.BudgetClass = budgetClass
	input.Inputs.DeadlineClass = deadlineClass
	if err := validateDecisionInput(input, policy); err != nil {
		return RoutingDecision{}, err
	}
	input.Inputs.OptimizationPolicy = policy
	input.Inputs.Now = input.Now
	input.Inputs = applyManualRoutingOverrides(input.Inputs, input.OverrideProvenance, input.DeliveryRunID, input.Now)
	input.OverrideProvenance = redactOverrideProvenance(input.OverrideProvenance)
	eligibility := FilterHardEligibility(input.Inputs)
	refs := inputRefs(input, eligibility)
	scored := scoreCandidates(eligibility.Eligible, input.Inputs, policy, input.Now)
	generationStatus := CandidateGenerationFull
	generationReason := ""
	if len(eligibility.Eligible) == 0 && len(eligibility.Rejected) == 0 {
		generationStatus = CandidateGenerationNeedsHuman
		generationReason = CandidateGenerationZeroReason
	}
	routingFingerprint, err := routingFingerprint(input, policy, eligibility, scored, refs, generationStatus, generationReason)
	if err != nil {
		return RoutingDecision{}, err
	}
	taskID := strings.TrimSpace(input.Inputs.Requirement.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(firstCandidateTaskID(input.Inputs.Candidates))
	}
	decision := RoutingDecision{
		SchemaVersion:             DecisionSchema,
		RecordVersion:             1,
		DecisionKey:               strings.TrimSpace(input.DecisionKey),
		DecisionKind:              DecisionKindRouting,
		ProjectID:                 strings.TrimSpace(input.ProjectID),
		DeliveryRunID:             strings.TrimSpace(input.DeliveryRunID),
		TaskID:                    taskID,
		TaskRequirementID:         strings.TrimSpace(input.TaskRequirementID),
		RoutingPolicyProfileID:    policy.RoutingPolicyProfileID,
		RoleDefinitionID:          strings.TrimSpace(input.RoleDefinitionID),
		PlanFingerprint:           strings.TrimSpace(input.PlanFingerprint),
		PolicyFingerprint:         strings.TrimSpace(input.PolicyFingerprint),
		AuthorizationFingerprint:  strings.TrimSpace(input.AuthorizationFingerprint),
		RuntimeHostName:           sanitize.Text(strings.TrimSpace(input.Inputs.HostName)),
		BudgetClass:               input.Inputs.BudgetClass,
		DeadlineClass:             input.Inputs.DeadlineClass,
		RoutingFingerprint:        routingFingerprint,
		InputRecordRefs:           refs,
		CandidateGenerationStatus: generationStatus,
		EligibleCandidates:        nonNilCandidates(eligibility.Eligible),
		RejectedCandidates:        nonNilRejectedCandidates(eligibility.Rejected),
		UserPinRefs:               nonNilStrings(pinIDs(input.Inputs.Pins)),
		FallbackChain:             []string{},
		BreakerGateRefs:           nonNilStrings(breakerRefs(eligibility)),
		PolicyInputRecords:        safePolicyInputRecords(input.PolicyInputRecords),
		OverrideProvenance:        nonNilOverrideProvenance(input.OverrideProvenance),
		OptimizationPolicy:        policy,
		RejectedSummary:           rejectedSummary(eligibility.Rejected),
		CreatedAt:                 delivery.CanonicalTimestamp(input.Now),
		UpdatedAt:                 delivery.CanonicalTimestamp(input.Now),
		DecidedBy:                 safeDeliveryActor(input.DecidedBy),
		Host:                      safeDeliveryHost(input.Host),
	}
	if hasRoutingPolicyProfile(input.RoutingPolicyProfile) {
		profile := input.RoutingPolicyProfile
		decision.RoutingPolicyProfile = &profile
	}
	decision.RoutingDecisionID = routingDecisionID(decision.ProjectID, decision.DeliveryRunID, decision.DecisionKey, decision.TaskID, decision.RoutingFingerprint)
	decision.ScoredCandidates = scored
	decision.HeuristicComponents = heuristicComponents(decision.ScoredCandidates)
	if len(decision.ScoredCandidates) == 0 {
		decision.DecisionStatus = DecisionStatusNoEligible
		decision.TerminalErrorCode = blockedErrorCode(eligibility.Rejected)
		if generationReason != "" {
			decision.ChosenReason = generationReason
		} else {
			decision.ChosenReason = "no hard-eligible candidates remain after deterministic eligibility"
		}
		return decision, nil
	}
	chosen := decision.ScoredCandidates[0]
	decision.DecisionStatus = DecisionStatusSelected
	decision.ChosenCandidateID = chosen.RoutingCandidateID
	decision.ChosenReason = fmt.Sprintf("selected %s with total score %s under policy %s", chosen.RoutingCandidateID, chosen.TotalScore, policy.PolicyVersion)
	for i := range decision.ScoredCandidates {
		if decision.ScoredCandidates[i].RoutingCandidateID == chosen.RoutingCandidateID {
			continue
		}
		decision.ScoredCandidates[i].WhyNotSelected = fmt.Sprintf("ranked below %s by total score or deterministic tie-break", chosen.RoutingCandidateID)
	}
	return decision, nil
}

func PersistRoutingDecision(ctx context.Context, store storage.Store, decision RoutingDecision) error {
	payload, err := delivery.CanonicalJSON(decision)
	if err != nil {
		return err
	}
	if err := validateRoutingDecision(decision); err != nil {
		return err
	}
	inputRefs, err := canonicalString(decision.InputRecordRefs)
	if err != nil {
		return err
	}
	eligible, err := canonicalString(decision.EligibleCandidates)
	if err != nil {
		return err
	}
	rejected, err := canonicalString(decision.RejectedCandidates)
	if err != nil {
		return err
	}
	scored, err := canonicalString(decision.ScoredCandidates)
	if err != nil {
		return err
	}
	summary, err := canonicalString(decision.RejectedSummary)
	if err != nil {
		return err
	}
	policy, err := canonicalString(decision.OptimizationPolicy)
	if err != nil {
		return err
	}
	actor, err := canonicalString(decision.DecidedBy)
	if err != nil {
		return err
	}
	host, err := canonicalString(decision.Host)
	if err != nil {
		return err
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO routing_decisions(
			routing_decision_id, schema_version, record_version, project_id, delivery_run_id, task_id, task_requirement_id,
			decision_key, decision_kind, routing_policy_profile_id, role_definition_id, plan_fingerprint, policy_fingerprint,
			routing_fingerprint, candidate_generation_status, decision_status, chosen_candidate_id, terminal_error_code,
			input_record_refs_json, eligible_candidates_json, rejected_candidates_json, scored_candidates_json,
			rejected_summary_json, optimization_policy_json, payload_json, created_at, updated_at, decided_by_json, host_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(routing_decision_id) DO NOTHING`,
			decision.RoutingDecisionID, decision.SchemaVersion, decision.RecordVersion, decision.ProjectID, decision.DeliveryRunID,
			decision.TaskID, decision.TaskRequirementID, decision.DecisionKey, decision.DecisionKind, decision.RoutingPolicyProfileID,
			decision.RoleDefinitionID, decision.PlanFingerprint, decision.PolicyFingerprint, decision.RoutingFingerprint,
			decision.CandidateGenerationStatus, decision.DecisionStatus, decision.ChosenCandidateID, string(decision.TerminalErrorCode),
			inputRefs, eligible, rejected, scored, summary, policy, string(payload), decision.CreatedAt, decision.UpdatedAt, actor, host)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 0 {
			return err
		}
		var existingPayload string
		if err := tx.QueryRow(ctx, `SELECT payload_json FROM routing_decisions WHERE routing_decision_id = ?`, decision.RoutingDecisionID).Scan(&existingPayload); err != nil {
			return err
		}
		var existing RoutingDecision
		if err := json.Unmarshal([]byte(existingPayload), &existing); err != nil {
			return err
		}
		if existing.RoutingFingerprint != decision.RoutingFingerprint ||
			existing.PolicyFingerprint != decision.PolicyFingerprint ||
			existing.PlanFingerprint != decision.PlanFingerprint ||
			existing.TaskRequirementID != decision.TaskRequirementID ||
			existing.ChosenCandidateID != decision.ChosenCandidateID ||
			existing.DecisionStatus != decision.DecisionStatus {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "stored routing decision conflicts with replay inputs"}
		}
		return nil
	})
}

func LoadRoutingDecision(ctx context.Context, store storage.Store, routingDecisionID string) (RoutingDecision, error) {
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM routing_decisions WHERE routing_decision_id = ?`, routingDecisionID).Scan(&payload)
	})
	if err != nil {
		return RoutingDecision{}, err
	}
	var decision RoutingDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return RoutingDecision{}, err
	}
	return decision, nil
}

func prepareDecisionInputFromStore(ctx context.Context, store storage.Store, input DecisionInput) (DecisionInput, error) {
	runAuthorizationFingerprint, err := loadRunAuthorizationFingerprint(ctx, store, input.ProjectID, input.DeliveryRunID)
	if err != nil {
		return input, err
	}
	if strings.TrimSpace(input.AuthorizationFingerprint) != "" && input.AuthorizationFingerprint != runAuthorizationFingerprint {
		return input, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "routing decision authorization fingerprint does not match delivery run"}
	}
	input.AuthorizationFingerprint = runAuthorizationFingerprint
	profile, err := resolveStoredDecisionProfile(ctx, store, input)
	if err != nil {
		return input, err
	}
	activeFingerprint := profile.PolicyFingerprint
	if strings.TrimSpace(input.PolicyFingerprint) != "" && strings.TrimSpace(input.PolicyFingerprint) != activeFingerprint {
		return input, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "caller policy fingerprint does not match stored routing policy profile"}
	}
	profileID := profile.RoutingPolicyProfileID
	records, err := LoadActivePolicyInputs(ctx, store, input.ProjectID, input.DeliveryRunID, profileID)
	if err != nil {
		return input, err
	}
	records = policyInputsForTask(records, input.Inputs.Requirement.TaskID, input.DecisionKey)
	if err := validateStoredPolicyInputsForDecision(records, input, profileID, activeFingerprint); err != nil {
		return input, err
	}
	if err := requireCallerInputsPersisted(input.Inputs.Pins, input.Inputs.Exclusions, records); err != nil {
		return input, err
	}
	pins, exclusions := constraintsFromPolicyInputRecords(records)
	input.RoutingPolicyProfileID = profileID
	input.RoutingPolicyProfile = profile
	input.PolicyFingerprint = activeFingerprint
	input.OptimizationPolicy = profile.OptimizationPolicy
	input.Inputs.Policy = profile.EligibilityPolicy
	input.PolicyInputRecords = records
	input.Inputs.Pins = pins
	input.Inputs.Exclusions = exclusions
	input.Inputs, err = InputsWithCachedInventory(ctx, store, input.Inputs)
	if err != nil {
		return input, err
	}
	return input, nil
}

func applyManualRoutingOverrides(inputs Inputs, overrides []OverrideProvenance, deliveryRunID string, now time.Time) Inputs {
	if len(overrides) == 0 {
		return inputs
	}
	out := inputs
	out.Candidates = append([]Candidate(nil), inputs.Candidates...)
	out.Inventory.QuotaSnapshots = append([]providerinventory.QuotaSnapshot(nil), inputs.Inventory.QuotaSnapshots...)
	out.ManualUnavailable = append([]ManualUnavailableOverride(nil), inputs.ManualUnavailable...)
	for _, override := range overrides {
		if !manualOverrideScopeMatches(override, deliveryRunID, inputs.Requirement.TaskID) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(override.OverrideKind)) {
		case "manual-unavailable-until":
			until, ok := parseTime(override.ManualUnavailableUntil)
			if !ok || !until.After(now.UTC()) {
				continue
			}
			out.ManualUnavailable = append(out.ManualUnavailable, ManualUnavailableOverride{
				OverrideID: strings.TrimSpace(override.OverrideID),
				Constraint: override.CandidateConstraint,
				Until:      delivery.CanonicalTimestamp(until),
			})
		case "manual-reset":
			for candidateIndex := range out.Candidates {
				candidate := out.Candidates[candidateIndex]
				if !constraintMatchesCandidate(override.CandidateConstraint, candidate) {
					continue
				}
				out.Candidates[candidateIndex].QuotaSnapshotIDs = manualResetQuotaSnapshotIDs(out.Candidates[candidateIndex], override, &out.Inventory.QuotaSnapshots, now)
				out.Candidates[candidateIndex].CandidateFingerprint = candidateFingerprint(out.Candidates[candidateIndex])
			}
		}
	}
	return out
}

func manualResetQuotaSnapshotIDs(candidate Candidate, override OverrideProvenance, snapshots *[]providerinventory.QuotaSnapshot, now time.Time) []string {
	if len(candidate.QuotaSnapshotIDs) == 0 {
		return candidate.QuotaSnapshotIDs
	}
	out := append([]string(nil), candidate.QuotaSnapshotIDs...)
	for i, id := range candidate.QuotaSnapshotIDs {
		snapshotIndex := quotaSnapshotIndex(*snapshots, id)
		if snapshotIndex < 0 {
			continue
		}
		snapshot := (*snapshots)[snapshotIndex]
		if override.ClearManualReset {
			snapshot.ResetAt = ""
			snapshot.WindowEnd = ""
			snapshot.ValidUntil = ""
		} else {
			resetAt, ok := parseTime(override.ManualResetAt)
			if !ok || !resetAt.After(now.UTC()) {
				continue
			}
			canonical := delivery.CanonicalTimestamp(resetAt)
			snapshot.ResetAt = canonical
			snapshot.WindowEnd = canonical
			snapshot.ValidUntil = canonical
		}
		snapshot.QuotaSnapshotID = manualResetQuotaSnapshotID(snapshot.QuotaSnapshotID, override.OverrideID, candidate.RoutingCandidateID)
		*snapshots = append(*snapshots, snapshot)
		out[i] = snapshot.QuotaSnapshotID
	}
	return dedupeStrings(out)
}

func quotaSnapshotIndex(snapshots []providerinventory.QuotaSnapshot, id string) int {
	for i, snapshot := range snapshots {
		if snapshot.QuotaSnapshotID == id {
			return i
		}
	}
	return -1
}

func manualResetQuotaSnapshotID(snapshotID, overrideID, candidateID string) string {
	return "manual-reset:" + shortDigest(hashHex("manual_reset_quota", snapshotID, overrideID, candidateID))
}

func manualOverrideScopeMatches(override OverrideProvenance, deliveryRunID, taskID string) bool {
	run := strings.TrimSpace(override.DeliveryRunID)
	task := strings.TrimSpace(override.TaskID)
	if run == "" && task == "" {
		return false
	}
	if strings.TrimSpace(override.Scope) != canonicalManualOverrideScope(override) {
		return false
	}
	if run != "" && run != strings.TrimSpace(deliveryRunID) {
		return false
	}
	if task != "" && task != strings.TrimSpace(taskID) {
		return false
	}
	return true
}

func validateOverrideDecisionScope(overrides []OverrideProvenance, deliveryRunID, taskID string) error {
	for _, override := range overrides {
		if !manualOverrideScopeMatches(override, deliveryRunID, taskID) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "manual override task/run scope does not match routing decision authority"}
		}
	}
	return nil
}

func canonicalManualOverrideScope(override OverrideProvenance) string {
	kind := strings.ToLower(strings.TrimSpace(override.OverrideKind))
	var parts []string
	if run := strings.TrimSpace(override.DeliveryRunID); run != "" {
		parts = append(parts, "run:"+run)
	}
	if task := strings.TrimSpace(override.TaskID); task != "" {
		parts = append(parts, "task:"+task)
	}
	if kind == "" || len(parts) == 0 {
		return strings.Join(parts, " ")
	}
	return kind + " " + strings.Join(parts, " ")
}

func redactOverrideProvenance(overrides []OverrideProvenance) []OverrideProvenance {
	if len(overrides) == 0 {
		return nil
	}
	out := append([]OverrideProvenance(nil), overrides...)
	for i := range out {
		out[i].Reason = redactSensitiveText(out[i].Reason)
		out[i].Source = redactSensitiveText(out[i].Source)
		out[i].Scope = canonicalManualOverrideScope(out[i])
		out[i].Actor = safeDeliveryActor(out[i].Actor)
		out[i].Host = safeDeliveryHost(out[i].Host)
	}
	return out
}

func redactSensitiveText(value string) string {
	return sanitize.Text(value)
}

func safeDeliveryActor(actor delivery.Actor) delivery.Actor {
	return delivery.Actor{
		ActorKind:         sanitize.Text(actor.ActorKind),
		ActorID:           sanitize.Text(actor.ActorID),
		DecisionAuthority: sanitize.Text(actor.DecisionAuthority),
		Source:            sanitize.Text(actor.Source),
	}
}

func safeDeliveryHost(host delivery.Host) delivery.Host {
	return delivery.Host{
		HostKind:         sanitize.Text(host.HostKind),
		HostID:           sanitize.Text(host.HostID),
		LoopcoderVersion: sanitize.Text(host.LoopcoderVersion),
		Platform:         sanitize.Text(host.Platform),
	}
}

func safePolicyInputRecords(records []PolicyInputRecord) []PolicyInputRecord {
	if len(records) == 0 {
		return []PolicyInputRecord{}
	}
	out := make([]PolicyInputRecord, 0, len(records))
	for _, record := range records {
		out = append(out, PolicyInputRecord{
			SchemaVersion:          record.SchemaVersion,
			RecordVersion:          record.RecordVersion,
			RoutingPolicyInputID:   record.RoutingPolicyInputID,
			InputKind:              record.InputKind,
			ProjectID:              record.ProjectID,
			DeliveryRunID:          record.DeliveryRunID,
			RoutingPolicyProfileID: record.RoutingPolicyProfileID,
			PolicyFingerprint:      record.PolicyFingerprint,
			Scope:                  record.Scope,
			DecisionKey:            record.DecisionKey,
			Status:                 record.Status,
			ExpiresAt:              record.ExpiresAt,
			Constraint:             record.Constraint,
			ValidationStatus:       record.ValidationStatus,
			Diagnostics:            []PolicyDiagnostic{},
		})
	}
	return out
}

func resolveStoredDecisionProfile(ctx context.Context, store storage.Store, input DecisionInput) (RoutingPolicyProfile, error) {
	profileID := strings.TrimSpace(input.RoutingPolicyProfileID)
	callerProfile := input.RoutingPolicyProfile
	if hasRoutingPolicyProfile(callerProfile) {
		callerProfile = normalizeRoutingPolicyProfile(callerProfile)
		if err := ValidateRoutingPolicyProfile(callerProfile); err != nil {
			return RoutingPolicyProfile{}, err
		}
		if profileID != "" && profileID != callerProfile.RoutingPolicyProfileID {
			return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "caller routing policy profile id does not match supplied profile"}
		}
		profileID = callerProfile.RoutingPolicyProfileID
	}
	if optProfileID := strings.TrimSpace(input.OptimizationPolicy.RoutingPolicyProfileID); optProfileID != "" {
		if profileID != "" && profileID != optProfileID {
			return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "caller optimization policy profile id does not match active profile"}
		}
		profileID = optProfileID
	}
	if profileID == "" {
		profileID = defaultStoredRoutingPolicyProfileID(input.Now)
	}
	stored, err := LoadRoutingPolicyProfile(ctx, store, profileID)
	if err != nil {
		return RoutingPolicyProfile{}, err
	}
	if hasRoutingPolicyProfile(callerProfile) {
		if err := requireSameRoutingPolicyProfile(callerProfile, stored); err != nil {
			return RoutingPolicyProfile{}, err
		}
	}
	if hasCallerOptimizationPolicy(input.OptimizationPolicy) {
		if strings.TrimSpace(input.OptimizationPolicy.PolicyFingerprint) != "" && input.OptimizationPolicy.PolicyFingerprint != stored.OptimizationPolicy.PolicyFingerprint {
			return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "caller optimization policy fingerprint does not match stored routing policy profile"}
		}
		policy, err := normalizeOptimizationPolicy(input.OptimizationPolicy, stored.RoutingPolicyProfileID)
		if err != nil {
			return RoutingPolicyProfile{}, err
		}
		if policy.PolicyFingerprint != stored.OptimizationPolicy.PolicyFingerprint {
			return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "caller optimization policy does not match stored routing policy profile"}
		}
	}
	return stored, nil
}

func hasCallerOptimizationPolicy(policy OptimizationPolicy) bool {
	return !isZeroOptimizationPolicy(policy) ||
		strings.TrimSpace(policy.TieBreakSeed) != "" ||
		len(policy.LocalAdapterIDs) > 0 ||
		len(policy.PreferredCandidateIDs) > 0 ||
		len(policy.DiversityHistory) > 0 ||
		strings.TrimSpace(policy.PolicyFingerprint) != ""
}

func requireSameRoutingPolicyProfile(caller, stored RoutingPolicyProfile) error {
	if caller.RoutingPolicyProfileID != stored.RoutingPolicyProfileID ||
		caller.ProfileKey != stored.ProfileKey ||
		caller.ProfileVersion != stored.ProfileVersion ||
		caller.PolicyVersion != stored.PolicyVersion ||
		caller.PolicyFingerprint != stored.PolicyFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "caller routing policy profile identity does not match stored profile"}
	}
	callerPayload, err := delivery.CanonicalJSON(caller)
	if err != nil {
		return err
	}
	storedPayload, err := delivery.CanonicalJSON(stored)
	if err != nil {
		return err
	}
	if string(callerPayload) != string(storedPayload) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "caller routing policy profile payload does not match stored profile"}
	}
	return nil
}

func defaultStoredRoutingPolicyProfileID(now time.Time) string {
	return BalancedRoutingPolicyProfile(now).RoutingPolicyProfileID
}

func loadRunAuthorizationFingerprint(ctx context.Context, store storage.Store, projectID, deliveryRunID string) (string, error) {
	var auth string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT authorization_fingerprint FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`,
			strings.TrimSpace(projectID), strings.TrimSpace(deliveryRunID)).Scan(&auth)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "delivery run authorization binding was not found"}
		}
		return "", err
	}
	if !validFingerprint(auth) {
		return "", &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "stored delivery run authorization fingerprint is invalid"}
	}
	return auth, nil
}

func ExplainJSON(decision RoutingDecision) ([]byte, error) {
	payload, err := delivery.CanonicalJSON(decision)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode canonical routing explain JSON: %w", err)
	}
	return delivery.CanonicalJSON(redactJSONStrings(value))
}

func redactJSONStrings(value any) any {
	switch typed := value.(type) {
	case string:
		return sanitize.Text(typed)
	case []any:
		redacted := make([]any, len(typed))
		for index, child := range typed {
			redacted[index] = redactJSONStrings(child)
		}
		return redacted
	case map[string]any:
		return redactJSONMap(typed)
	default:
		return value
	}
}

type redactedJSONMapEntry struct {
	value          any
	canonicalValue string
	keyUnchanged   bool
}

// redactJSONMap sanitizes dynamic JSON keys as well as values. Multiple input
// keys can collapse to the same redaction marker, so collisions receive a
// deterministic ordinal suffix. Existing sanitized keys are reserved first so
// a generated suffix cannot overwrite another entry, and ordering is based on
// redacted values rather than secret source keys.
func redactJSONMap(values map[string]any) map[string]any {
	groups := make(map[string][]redactedJSONMapEntry, len(values))
	reserved := make(map[string]struct{}, len(values))
	for key, child := range values {
		baseKey := sanitize.Text(key)
		if baseKey == "" && key != "" {
			baseKey = "[REDACTED_KEY]"
		}
		redactedValue := redactJSONStrings(child)
		canonicalValue, _ := json.Marshal(redactedValue)
		groups[baseKey] = append(groups[baseKey], redactedJSONMapEntry{
			value:          redactedValue,
			canonicalValue: string(canonicalValue),
			keyUnchanged:   key == baseKey,
		})
		reserved[baseKey] = struct{}{}
	}

	baseKeys := make([]string, 0, len(groups))
	for key := range groups {
		baseKeys = append(baseKeys, key)
	}
	sort.Strings(baseKeys)

	redacted := make(map[string]any, len(values))
	used := make(map[string]struct{}, len(values))
	for _, baseKey := range baseKeys {
		entries := groups[baseKey]
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].keyUnchanged != entries[j].keyUnchanged {
				return entries[i].keyUnchanged
			}
			return entries[i].canonicalValue < entries[j].canonicalValue
		})
		for index, entry := range entries {
			outputKey := baseKey
			if index > 0 {
				outputKey = uniqueRedactedJSONKey(baseKey, reserved, used)
			}
			redacted[outputKey] = entry.value
			used[outputKey] = struct{}{}
		}
	}
	return redacted
}

func uniqueRedactedJSONKey(baseKey string, reserved, used map[string]struct{}) string {
	for ordinal := 2; ; ordinal++ {
		candidate := baseKey + "#" + strconv.Itoa(ordinal)
		if _, exists := reserved[candidate]; exists {
			continue
		}
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

func ExplainHuman(decision RoutingDecision) string {
	var b strings.Builder
	if decision.DecisionStatus == DecisionStatusSelected {
		fmt.Fprintf(&b, "selected %s: %s\n", decision.ChosenCandidateID, decision.ChosenReason)
	} else {
		fmt.Fprintf(&b, "blocked %s: %s\n", decision.DecisionStatus, decision.TerminalErrorCode)
		if decision.CandidateGenerationStatus != CandidateGenerationFull {
			fmt.Fprintf(&b, "candidate generation: %s\n", decision.CandidateGenerationStatus)
		}
		if decision.ChosenReason != "" {
			fmt.Fprintf(&b, "reason: %s\n", decision.ChosenReason)
		}
	}
	if decision.RoutingPolicyProfile != nil {
		fmt.Fprintf(&b, "profile %s version %s fingerprint %s\n", decision.RoutingPolicyProfile.ProfileKey, decision.RoutingPolicyProfile.ProfileVersion, decision.RoutingPolicyProfile.PolicyFingerprint)
	}
	if decision.BudgetClass != "" || decision.DeadlineClass != "" {
		fmt.Fprintf(&b, "task fit budget_class=%s deadline_class=%s\n", decision.BudgetClass, decision.DeadlineClass)
	}
	if decision.RuntimeHostName != "" {
		fmt.Fprintf(&b, "runtime host %s\n", decision.RuntimeHostName)
	}
	policy := decision.OptimizationPolicy
	fmt.Fprintf(&b, "strategy %s version %s target_utilization=%s completion_reserve=%s verification_reserve=%s paid_overage=%t\n",
		firstNonEmpty(policy.StrategyKey, StrategyBalanced), firstNonEmpty(policy.StrategyVersion, StrategyVersion),
		formatBasisPoints(int(policy.TargetUtilizationBP*100)), formatBasisPoints(int(policy.CompletionReserveBP*100)),
		formatBasisPoints(int(policy.VerificationReserveBP*100)), policy.AllowPaidOverage)
	for _, band := range policy.ResetBands {
		fmt.Fprintf(&b, "reset window %s: min=%ds max=%ds max_task=%s urgency=%d\n", band.Name, band.MinResetSeconds, band.MaxResetSeconds, band.MaxTaskClass, band.ExpiryUrgencyScore)
	}
	for _, override := range decision.OverrideProvenance {
		fmt.Fprintf(&b, "override %s: %s scope=%s actor=%s expires=%s fingerprint=%s\n", override.OverrideID, override.Reason, override.Scope, override.Actor.ActorID, firstNonEmpty(override.ExpiresAt, "none"), override.PolicyFingerprint)
	}
	for _, candidate := range decision.ScoredCandidates {
		fmt.Fprintf(&b, "candidate %s rank=%d total=%s confidence=%s\n", candidate.RoutingCandidateID, candidate.Rank, candidate.TotalScore, candidate.Confidence)
		for _, component := range candidate.Components {
			fmt.Fprintf(&b, "  component %s score=%d weight=%d weighted=%s confidence=%s freshness=%s", component.Name, component.Score, component.Weight, component.WeightedScore, component.Confidence, component.FreshnessState)
			if component.ResetWindow != "" || component.ResetAt != "" {
				fmt.Fprintf(&b, " window=%s reset_at=%s", component.ResetWindow, component.ResetAt)
			}
			if component.ExpectedWaste != nil {
				fmt.Fprintf(&b, " expected_waste_avoided=%d", *component.ExpectedWaste)
			}
			fmt.Fprintln(&b)
		}
	}
	for _, candidate := range decision.ScoredCandidates {
		if candidate.RoutingCandidateID == decision.ChosenCandidateID {
			continue
		}
		reason := candidate.WhyNotSelected
		if reason == "" {
			reason = fmt.Sprintf("eligible but ranked below %s by total score or deterministic tie-break", decision.ChosenCandidateID)
		}
		fmt.Fprintf(&b, "- %s: %s\n", candidate.RoutingCandidateID, reason)
	}
	for _, rejected := range decision.RejectedCandidates {
		fmt.Fprintf(&b, "- %s: %s\n", rejected.Candidate.RoutingCandidateID, candidateLabel(rejected.Candidate)+" "+rejectionExplanation(rejected.Reasons))
	}
	return sanitize.Text(strings.TrimSpace(b.String()))
}

func normalizeOptimizationPolicy(policy OptimizationPolicy, profileID string) (OptimizationPolicy, error) {
	if strings.TrimSpace(policy.SchemaVersion) == "" {
		policy.SchemaVersion = "loopcoder.routing_optimization_policy.v1"
	}
	if strings.TrimSpace(policy.RoutingPolicyProfileID) == "" {
		policy.RoutingPolicyProfileID = strings.TrimSpace(profileID)
	}
	if strings.TrimSpace(policy.RoutingPolicyProfileID) == "" {
		policy.RoutingPolicyProfileID = DefaultProfileID
	}
	if strings.TrimSpace(policy.ProfileKey) == "" {
		policy.ProfileKey = DefaultProfileVersion
	}
	if strings.TrimSpace(policy.ProfileVersion) == "" {
		policy.ProfileVersion = "1"
	}
	if strings.TrimSpace(policy.TieBreakSeed) == "" {
		policy.TieBreakSeed = DefaultTieBreakSeed
	}
	if strings.TrimSpace(policy.StrategyKey) == "" {
		policy.StrategyKey = strategyFromProfileKey(policy.ProfileKey)
	}
	if !validStrategy(policy.StrategyKey) {
		return OptimizationPolicy{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("unsupported routing strategy %q", policy.StrategyKey)}
	}
	if strings.TrimSpace(policy.StrategyVersion) == "" {
		policy.StrategyVersion = StrategyVersion
	}
	if policy.TargetUtilizationBP == 0 {
		policy.TargetUtilizationBP = DefaultTargetUtilization
	}
	if policy.CompletionReserveBP == 0 {
		policy.CompletionReserveBP = 500
	}
	if policy.VerificationReserveBP == 0 {
		policy.VerificationReserveBP = 800
	}
	if policy.TargetUtilizationBP < 1 || policy.TargetUtilizationBP > 10000 || policy.CompletionReserveBP < 0 || policy.VerificationReserveBP < 0 || policy.CompletionReserveBP+policy.VerificationReserveBP > policy.TargetUtilizationBP {
		return OptimizationPolicy{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing strategy utilization and reserve basis points are invalid"}
	}
	if len(policy.ResetBands) == 0 {
		policy.ResetBands = defaultResetBands()
	}
	policy.ResetBands = normalizeResetBands(policy.ResetBands)
	if err := validateResetBands(policy.ResetBands); err != nil {
		return OptimizationPolicy{}, err
	}
	if len(policy.Weights) == 0 {
		policy.Weights = strategyWeights(policy.StrategyKey)
	}
	total := 0
	for name, weight := range policy.Weights {
		if !supportedComponents[name] {
			return OptimizationPolicy{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("unsupported routing optimization component %q", name)}
		}
		if weight < 0 || weight > 100 {
			return OptimizationPolicy{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("routing optimization weight %q must be between 0 and 100", name)}
		}
		total += weight
	}
	if total != 100 {
		return OptimizationPolicy{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing optimization weights must sum to 100"}
	}
	policy.LocalAdapterIDs = dedupeStrings(policy.LocalAdapterIDs)
	policy.PreferredCandidateIDs = dedupeStrings(policy.PreferredCandidateIDs)
	sort.Slice(policy.DiversityHistory, func(i, j int) bool {
		if policy.DiversityHistory[i].RoutingDecisionID != policy.DiversityHistory[j].RoutingDecisionID {
			return policy.DiversityHistory[i].RoutingDecisionID < policy.DiversityHistory[j].RoutingDecisionID
		}
		return policy.DiversityHistory[i].RoutingCandidateID < policy.DiversityHistory[j].RoutingCandidateID
	})
	fp, _, err := delivery.DigestCanonicalJSON(map[string]any{
		"schema_version":            policy.SchemaVersion,
		"routing_policy_profile_id": policy.RoutingPolicyProfileID,
		"profile_key":               policy.ProfileKey,
		"profile_version":           policy.ProfileVersion,
		"strategy_key":              policy.StrategyKey,
		"strategy_version":          policy.StrategyVersion,
		"target_utilization_bp":     policy.TargetUtilizationBP,
		"completion_reserve_bp":     policy.CompletionReserveBP,
		"verification_reserve_bp":   policy.VerificationReserveBP,
		"allow_paid_overage":        policy.AllowPaidOverage,
		"reset_bands":               policy.ResetBands,
		"weights":                   policy.Weights,
		"tie_break_seed":            policy.TieBreakSeed,
		"local_adapter_ids":         policy.LocalAdapterIDs,
		"preferred_candidate_ids":   policy.PreferredCandidateIDs,
		"diversity_history":         policy.DiversityHistory,
	})
	if err != nil {
		return OptimizationPolicy{}, err
	}
	policy.PolicyFingerprint = fp
	if strings.TrimSpace(policy.PolicyVersion) == "" {
		policy.PolicyVersion = "routing-optimization-" + shortDigest(fp)
	}
	return policy, nil
}

func validStrategy(value string) bool {
	switch strings.TrimSpace(value) {
	case StrategyQualityFirst, StrategyBalanced, StrategyBurnBeforeReset:
		return true
	default:
		return false
	}
}

func strategyFromProfileKey(profileKey string) string {
	switch strings.TrimSpace(profileKey) {
	case StrategyQualityFirst, ProfileKeyDeep:
		return StrategyQualityFirst
	case StrategyBurnBeforeReset, ProfileKeyFast:
		return StrategyBurnBeforeReset
	default:
		return StrategyBalanced
	}
}

func strategyWeights(strategy string) map[ComponentName]int {
	switch strategy {
	case StrategyQualityFirst:
		return map[ComponentName]int{ComponentExpiryUrgency: 5, ComponentTaskHeadroom: 10, ComponentCapacityTrust: 10, ComponentQualityFit: 35, ComponentCost: 10, ComponentLatency: 10, ComponentHealth: 10, ComponentDiversity: 10}
	case StrategyBurnBeforeReset:
		return map[ComponentName]int{ComponentExpiryUrgency: 30, ComponentTaskHeadroom: 15, ComponentCapacityTrust: 10, ComponentQualityFit: 15, ComponentCost: 10, ComponentLatency: 5, ComponentHealth: 10, ComponentDiversity: 5}
	default:
		return map[ComponentName]int{ComponentExpiryUrgency: 15, ComponentTaskHeadroom: 15, ComponentCapacityTrust: 10, ComponentQualityFit: 20, ComponentCost: 10, ComponentLatency: 10, ComponentHealth: 10, ComponentDiversity: 10}
	}
}

func defaultResetBands() []ResetBand {
	return []ResetBand{
		{Name: "under-15m", MinResetSeconds: 0, MaxResetSeconds: int64((15 * time.Minute).Seconds()), MaxTaskClass: "very-short", ExpiryUrgencyScore: 100},
		{Name: "15-60m", MinResetSeconds: int64((15 * time.Minute).Seconds()), MaxResetSeconds: int64(time.Hour.Seconds()), MaxTaskClass: "short", ExpiryUrgencyScore: 80},
		{Name: "over-1h", MinResetSeconds: int64(time.Hour.Seconds()), MaxTaskClass: "medium", ExpiryUrgencyScore: 40},
	}
}

func normalizeResetBands(bands []ResetBand) []ResetBand {
	out := append([]ResetBand(nil), bands...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].MinResetSeconds != out[j].MinResetSeconds {
			return out[i].MinResetSeconds < out[j].MinResetSeconds
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func validateResetBands(bands []ResetBand) error {
	if len(bands) == 0 {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing reset bands are required"}
	}
	for _, band := range bands {
		if strings.TrimSpace(band.Name) == "" || strings.TrimSpace(band.MaxTaskClass) == "" || band.MinResetSeconds < 0 || band.MaxResetSeconds < 0 || (band.MaxResetSeconds > 0 && band.MaxResetSeconds <= band.MinResetSeconds) || band.ExpiryUrgencyScore < 0 || band.ExpiryUrgencyScore > 100 {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing reset band is invalid"}
		}
	}
	return nil
}

func routingFingerprint(input DecisionInput, policy OptimizationPolicy, result Result, scored []ScoredCandidate, refs []InputRecordRef, generationStatus, generationReason string) (string, error) {
	providerInventory := providerInventoryFingerprintProjection(input.Inputs.Inventory)
	policyInputRecords := safePolicyInputRecords(input.PolicyInputRecords)
	overrideProvenance := redactOverrideProvenance(input.OverrideProvenance)
	payload := map[string]any{
		"schema_version":              "loopcoder.routing_fingerprint_input.v1",
		"decision_key":                input.DecisionKey,
		"project_id":                  input.ProjectID,
		"delivery_run_id":             input.DeliveryRunID,
		"task_requirement_id":         input.TaskRequirementID,
		"role_definition_id":          input.RoleDefinitionID,
		"role_definitions":            input.Inputs.RoleDefinitions,
		"plan_fingerprint":            input.PlanFingerprint,
		"policy_fingerprint":          firstNonEmpty(input.PolicyFingerprint, policy.PolicyFingerprint),
		"authorization_fingerprint":   input.AuthorizationFingerprint,
		"prior_routing_decision_id":   input.PriorRoutingDecisionID,
		"prior_routing_fingerprint":   input.PriorRoutingFingerprint,
		"budget_class":                input.Inputs.BudgetClass,
		"deadline_class":              input.Inputs.DeadlineClass,
		"candidate_generation_status": generationStatus,
		"candidate_generation_reason": generationReason,
		"hard_policy":                 input.Inputs.Policy,
		"optimization_policy":         policy,
		"runtime_contract":            input.Inputs.RuntimeContract,
		"host_name":                   input.Inputs.HostName,
		"provider_inventory":          providerInventory,
		"availability":                input.Inputs.Availability,
		"circuit_breakers":            input.Inputs.CircuitBreakers,
		"budgets":                     input.Inputs.Budgets,
		"input_record_refs":           refs,
		"eligible_candidates":         result.Eligible,
		"rejected_candidates":         result.Rejected,
		"scored_candidates":           scored,
		"user_pins":                   input.Inputs.Pins,
		"exclusions":                  input.Inputs.Exclusions,
		"worker_route":                input.Inputs.WorkerRoute,
	}
	if hasRoutingPolicyProfile(input.RoutingPolicyProfile) {
		payload["routing_profile"] = input.RoutingPolicyProfile
	}
	if len(overrideProvenance) > 0 {
		payload["override_provenance"] = overrideProvenance
	}
	if len(policyInputRecords) > 0 {
		payload["policy_input_records"] = policyInputRecords
	}
	digest, _, err := delivery.DigestCanonicalJSON(payload)
	return digest, err
}

// providerInventoryFingerprintProjection binds the scoped inventory evidence
// while excluding the report-generation clock. InventoryFingerprint is a
// derived field, so it is reproduced from the same stable evidence projection
// instead of trusting or fingerprinting a caller-supplied value.
func providerInventoryFingerprintProjection(report providerinventory.Report) providerinventory.Report {
	projection := report
	projection.GeneratedAt = ""
	projection.InventoryFingerprint = ""
	projection.InventoryFingerprint = "sha256:" + hashCanonical(projection)
	return projection
}

func scoreCandidates(candidates []Candidate, inputs Inputs, policy OptimizationPolicy, now time.Time) []ScoredCandidate {
	quotaByID := mapQuotaSnapshots(inputs.Inventory.QuotaSnapshots)
	scoreByID, scoreByCandidate := mapAvailabilityScores(inputs.Availability)
	budgetByID := mapBudgets(inputs.Budgets)
	out := make([]ScoredCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		components := make([]ComponentScore, 0, len(policy.Weights))
		for _, name := range scoringComponentOrder {
			if _, ok := policy.Weights[name]; !ok {
				continue
			}
			switch name {
			case ComponentExpiryUrgency:
				components = append(components, scoreExpiryUrgency(candidate, inputs.Requirement, inputs.DeadlineClass, quotaByID, policy, now))
			case ComponentTaskHeadroom:
				components = append(components, scoreTaskHeadroom(candidate, inputs.Requirement, inputs.BudgetClass, inputs.DeadlineClass, quotaByID, policy, now))
			case ComponentCapacityTrust:
				components = append(components, scoreCapacityTrust(candidate, quotaByID, policy))
			case ComponentAvailability:
				components = append(components, scoreAvailability(candidate, scoreByID, scoreByCandidate, policy))
			case ComponentQuotaHeadroom:
				components = append(components, scoreQuota(candidate, quotaByID, policy))
			case ComponentQualityFit:
				components = append(components, scoreQuality(candidate, inputs.Requirement, policy))
			case ComponentLatency:
				components = append(components, scoreLatency(candidate, scoreByID, scoreByCandidate, policy))
			case ComponentCost:
				components = append(components, scoreCost(candidate, budgetByID, policy))
			case ComponentDiversity:
				components = append(components, scoreDiversity(candidate, policy))
			case ComponentHealth:
				components = append(components, scoreHealth(candidate, scoreByID, scoreByCandidate, policy))
			case ComponentLocality:
				components = append(components, scoreLocality(candidate, policy))
			case ComponentUserPreference:
				components = append(components, scoreUserPreference(candidate, policy))
			}
		}
		totalBasis := 0
		heuristic := false
		confidence := providerinventory.ConfidenceExact
		for _, component := range components {
			totalBasis += component.Score * component.Weight * 100
			if component.Heuristic {
				heuristic = true
			}
			if confidenceRank(component.Confidence) < confidenceRank(confidence) {
				confidence = component.Confidence
			}
		}
		out = append(out, ScoredCandidate{
			RoutingCandidateID: candidate.RoutingCandidateID,
			Candidate:          candidate,
			Components:         components,
			TotalScore:         formatBasisPoints(totalBasis),
			TotalBasisPoints:   totalBasis,
			TieBreakValue:      tieBreakValue(policy.TieBreakSeed, candidate.RoutingCandidateID),
			Confidence:         confidence,
			Heuristic:          heuristic,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalBasisPoints != out[j].TotalBasisPoints {
			return out[i].TotalBasisPoints > out[j].TotalBasisPoints
		}
		if out[i].TieBreakValue != out[j].TieBreakValue {
			return out[i].TieBreakValue < out[j].TieBreakValue
		}
		return out[i].RoutingCandidateID < out[j].RoutingCandidateID
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func scoreQuality(candidate Candidate, requirement taskrequirements.TaskRequirement, policy OptimizationPolicy) ComponentScore {
	score := 80
	if qualityRank(candidate.QualityFloor) > qualityRank(requirement.QualityFloor) {
		score = 100
	}
	return component(ComponentQualityFit, score, policy, providerinventory.ConfidenceEstimated, true, "quality fit uses policy quality floor until conformance records are persisted", nil, nil, nil)
}

func scoreExpiryUrgency(candidate Candidate, requirement taskrequirements.TaskRequirement, deadlineClass DeadlineClass, quotaByID map[string]providerinventory.QuotaSnapshot, policy OptimizationPolicy, now time.Time) ComponentScore {
	if deadlineClass == "" {
		deadlineClass = DeadlineClass(taskClassForRequirement(requirement))
	}
	selected, ok := selectedQuotaSnapshot(candidate, quotaByID)
	if !ok {
		c := component(ComponentExpiryUrgency, 0, policy, providerinventory.ConfidenceUnknown, true, "expiry urgency requires fresh quota reset evidence", nil, candidate.QuotaSnapshotIDs, nil)
		c.TaskClass = string(deadlineClass)
		c.DeadlineClass = deadlineClass
		return c
	}
	score := 0
	windowName := "unknown"
	var resetAt string
	var expectedWaste *int64
	if reset, resetOK := parseTime(selected.ResetAt); resetOK && reset.After(now.UTC()) {
		resetAt = delivery.CanonicalTimestamp(reset)
		band := resetBandForDuration(policy.ResetBands, reset.Sub(now.UTC()))
		windowName = band.Name
		if taskClassAllowedInBand(string(deadlineClass), band.MaxTaskClass) {
			score = band.ExpiryUrgencyScore
			if selected.RemainingValue != nil {
				waste := expectedWasteAvoided(*selected.RemainingValue, policy.TargetUtilizationBP)
				expectedWaste = &waste
			}
		}
	}
	c := component(ComponentExpiryUrgency, score, policy, selected.Confidence, selected.Confidence != providerinventory.ConfidenceExact, "expiry urgency rewards eligible task-fit capacity before known reset windows", nil, []string{selected.QuotaSnapshotID}, selected.RemainingValue)
	c.ResetAt = resetAt
	c.ResetWindow = windowName
	c.TaskClass = string(deadlineClass)
	c.DeadlineClass = deadlineClass
	c.ExpectedWaste = expectedWaste
	c.FreshnessState = selected.FreshnessState
	return c
}

func scoreTaskHeadroom(candidate Candidate, requirement taskrequirements.TaskRequirement, budgetClass BudgetClass, deadlineClass DeadlineClass, quotaByID map[string]providerinventory.QuotaSnapshot, policy OptimizationPolicy, now time.Time) ComponentScore {
	if budgetClass == "" {
		budgetClass = BudgetClass(taskClassForRequirement(requirement))
	}
	if deadlineClass == "" {
		deadlineClass = DeadlineClass(taskClassForRequirement(requirement))
	}
	selected, ok := selectedQuotaSnapshot(candidate, quotaByID)
	if !ok || selected.RemainingValue == nil {
		c := component(ComponentTaskHeadroom, 0, policy, providerinventory.ConfidenceUnknown, true, "task-equivalent headroom requires remaining quota evidence", nil, candidate.QuotaSnapshotIDs, nil)
		c.TaskClass = string(budgetClass)
		c.BudgetClass = budgetClass
		c.DeadlineClass = deadlineClass
		return c
	}
	taskUnits := taskUnitsForClass(string(budgetClass))
	usable := usableAfterReserves(*selected.RemainingValue, policy)
	headroom := int64(0)
	if taskUnits > 0 {
		headroom = usable / taskUnits
	}
	score := int(minInt64(headroom*25, 100))
	windowName := "unknown"
	var resetAt string
	if reset, resetOK := parseTime(selected.ResetAt); resetOK && reset.After(now.UTC()) {
		resetAt = delivery.CanonicalTimestamp(reset)
		band := resetBandForDuration(policy.ResetBands, reset.Sub(now.UTC()))
		windowName = band.Name
		if !taskClassAllowedInBand(string(deadlineClass), band.MaxTaskClass) {
			score = 0
		}
	}
	value := headroom
	c := component(ComponentTaskHeadroom, score, policy, selected.Confidence, selected.Confidence != providerinventory.ConfidenceExact, "task-equivalent headroom applies target utilization and reserves before scoring capacity", nil, []string{selected.QuotaSnapshotID}, &value)
	c.ResetAt = resetAt
	c.ResetWindow = windowName
	c.TaskClass = string(budgetClass)
	c.BudgetClass = budgetClass
	c.DeadlineClass = deadlineClass
	c.ReserveBasisBP = policy.CompletionReserveBP + policy.VerificationReserveBP
	c.FreshnessState = selected.FreshnessState
	return c
}

func scoreCapacityTrust(candidate Candidate, quotaByID map[string]providerinventory.QuotaSnapshot, policy OptimizationPolicy) ComponentScore {
	selected, ok := selectedQuotaSnapshot(candidate, quotaByID)
	if !ok {
		return component(ComponentCapacityTrust, 0, policy, providerinventory.ConfidenceUnknown, true, "capacity trust requires quota evidence", nil, candidate.QuotaSnapshotIDs, nil)
	}
	score := confidenceRank(selected.Confidence) * 35
	if selected.FreshnessState == providerinventory.FreshnessFresh {
		score += 30
	}
	c := component(ComponentCapacityTrust, score, policy, selected.Confidence, selected.Confidence != providerinventory.ConfidenceExact, "capacity trust combines quota confidence and freshness", nil, []string{selected.QuotaSnapshotID}, selected.RemainingValue)
	c.FreshnessState = selected.FreshnessState
	return c
}

func scoreQuota(candidate Candidate, quotaByID map[string]providerinventory.QuotaSnapshot, policy OptimizationPolicy) ComponentScore {
	score := 0
	confidence := providerinventory.ConfidenceUnknown
	var selected *providerinventory.QuotaSnapshot
	for _, id := range candidate.QuotaSnapshotIDs {
		snapshot, ok := quotaByID[id]
		if !ok || snapshot.RemainingValue == nil {
			continue
		}
		if selected == nil || quotaSnapshotBetterForScore(snapshot, *selected) {
			selectedSnapshot := snapshot
			selected = &selectedSnapshot
		}
	}
	var value *int64
	var snapshots []string
	if selected != nil {
		value = selected.RemainingValue
		score = int(minInt64(*selected.RemainingValue, 100))
		confidence = selected.Confidence
		snapshots = []string{selected.QuotaSnapshotID}
	}
	return component(ComponentQuotaHeadroom, score, policy, confidence, confidence != providerinventory.ConfidenceExact, "quota headroom is capped at 100 from selected remaining capacity evidence", nil, snapshots, value)
}

func selectedQuotaSnapshot(candidate Candidate, quotaByID map[string]providerinventory.QuotaSnapshot) (providerinventory.QuotaSnapshot, bool) {
	var selected *providerinventory.QuotaSnapshot
	for _, id := range candidate.QuotaSnapshotIDs {
		snapshot, ok := quotaByID[id]
		if !ok || snapshot.RemainingValue == nil {
			continue
		}
		if selected == nil || quotaSnapshotBetterForScore(snapshot, *selected) {
			selectedSnapshot := snapshot
			selected = &selectedSnapshot
		}
	}
	if selected == nil {
		return providerinventory.QuotaSnapshot{}, false
	}
	return *selected, true
}

func parseTime(value string) (time.Time, bool) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func resetBandForDuration(bands []ResetBand, d time.Duration) ResetBand {
	seconds := int64(d.Seconds())
	for _, band := range bands {
		if seconds < band.MinResetSeconds {
			continue
		}
		if band.MaxResetSeconds == 0 || seconds < band.MaxResetSeconds {
			return band
		}
	}
	return ResetBand{Name: "unknown", MaxTaskClass: "very-short"}
}

func taskClassForRequirement(requirement taskrequirements.TaskRequirement) string {
	if requirement.RiskTier == taskrequirements.RiskHigh || requirement.RiskTier == taskrequirements.RiskCritical || requirement.QualityFloor == taskrequirements.QualityAdversarial || requirement.NestedAllowed {
		return "medium"
	}
	if requirement.RiskTier == taskrequirements.RiskMedium {
		return "short"
	}
	return "very-short"
}

func resolveTaskFitClasses(requirement taskrequirements.TaskRequirement, budgetClass BudgetClass, deadlineClass DeadlineClass) (BudgetClass, DeadlineClass, error) {
	floor := taskClassForRequirement(requirement)
	budgetClass = BudgetClass(strings.TrimSpace(string(budgetClass)))
	deadlineClass = DeadlineClass(strings.TrimSpace(string(deadlineClass)))
	if budgetClass == "" {
		budgetClass = BudgetClass(floor)
	}
	if deadlineClass == "" {
		deadlineClass = DeadlineClass(floor)
	}
	if !ValidBudgetClass(budgetClass) {
		return "", "", &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("unknown budget_class %q", budgetClass)}
	}
	if !ValidDeadlineClass(deadlineClass) {
		return "", "", &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("unknown deadline_class %q", deadlineClass)}
	}
	if taskClassRank(string(budgetClass)) < taskClassRank(floor) {
		return "", "", &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("budget_class %q is weaker than task requirement floor %q", budgetClass, floor)}
	}
	if taskClassRank(string(deadlineClass)) < taskClassRank(floor) {
		return "", "", &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("deadline_class %q is weaker than task requirement floor %q", deadlineClass, floor)}
	}
	return budgetClass, deadlineClass, nil
}

func taskClassAllowedInBand(taskClass, maxClass string) bool {
	return taskClassRank(taskClass) <= taskClassRank(maxClass)
}

func taskClassRank(value string) int {
	switch strings.TrimSpace(value) {
	case "very-short":
		return 1
	case "short":
		return 2
	case "medium":
		return 3
	default:
		return 100
	}
}

func taskUnitsForClass(value string) int64 {
	switch strings.TrimSpace(value) {
	case "very-short":
		return 10
	case "short":
		return 35
	case "medium":
		return 70
	default:
		return 100
	}
}

func usableAfterReserves(remaining int64, policy OptimizationPolicy) int64 {
	target := ceilDiv(remaining*policy.TargetUtilizationBP, 10000)
	completion := ceilDiv(remaining*policy.CompletionReserveBP, 10000)
	verification := ceilDiv(remaining*policy.VerificationReserveBP, 10000)
	usable := target - completion - verification
	if usable < 0 {
		return 0
	}
	return usable
}

func expectedWasteAvoided(remaining, targetBP int64) int64 {
	target := ceilDiv(remaining*targetBP, 10000)
	waste := remaining - target
	if waste < 0 {
		return 0
	}
	return waste
}

func ceilDiv(value, divisor int64) int64 {
	if divisor <= 0 || value <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}

func scoreCost(candidate Candidate, budgetByID map[string]budget.Summary, policy OptimizationPolicy) ComponentScore {
	score := 0
	confidence := providerinventory.ConfidenceUnknown
	var selected *budget.Summary
	for _, id := range candidate.BudgetPolicyIDs {
		summary, ok := budgetByID[id]
		if !ok {
			continue
		}
		if selected == nil || budgetSummaryBetterForScore(summary, *selected) {
			selectedSummary := summary
			selected = &selectedSummary
		}
	}
	var value *int64
	var evidence []string
	if selected != nil {
		value = &selected.AvailableValue
		score = int(minInt64(selected.AvailableValue, 100))
		confidence = selected.Confidence
		evidence = []string{selected.BudgetPolicyID}
	}
	return component(ComponentCost, score, policy, confidence, confidence != providerinventory.ConfidenceExact, "cost score uses selected hard budget headroom until exact unit-cost tables are available", evidence, nil, value)
}

func scoreLatency(candidate Candidate, scoreByID map[string]availability.Score, scoreByCandidate map[string]availability.Score, policy OptimizationPolicy) ComponentScore {
	score := 50
	confidence := providerinventory.ConfidenceEstimated
	evidence := []string{}
	if av, ok := availabilityForCandidate(candidate, scoreByID, scoreByCandidate); ok {
		evidence = append(evidence, av.AvailabilityScoreID)
		confidence = av.ScoreConfidence
		for _, comp := range av.Components {
			if comp.Name == string(ComponentLatency) {
				score = comp.Score
				confidence = comp.Confidence
				evidence = append(evidence, comp.EvidenceRecordIDs...)
				break
			}
		}
	}
	return component(ComponentLatency, score, policy, confidence, true, "latency defaults conservatively unless availability observations provide a latency component", evidence, nil, nil)
}

func scoreAvailability(candidate Candidate, scoreByID map[string]availability.Score, scoreByCandidate map[string]availability.Score, policy OptimizationPolicy) ComponentScore {
	score := 0
	confidence := providerinventory.ConfidenceUnknown
	var evidence []string
	if av, ok := availabilityForCandidate(candidate, scoreByID, scoreByCandidate); ok {
		score = av.Score
		confidence = av.ScoreConfidence
		evidence = append(evidence, av.AvailabilityScoreID)
		evidence = append(evidence, av.EvidenceRecordIDs...)
	}
	return component(ComponentAvailability, score, policy, confidence, confidence != providerinventory.ConfidenceExact, "availability score comes from the persisted AvailabilityScore record", evidence, nil, nil)
}

func scoreHealth(candidate Candidate, scoreByID map[string]availability.Score, scoreByCandidate map[string]availability.Score, policy OptimizationPolicy) ComponentScore {
	score := 50
	confidence := providerinventory.ConfidenceEstimated
	evidence := []string{}
	if av, ok := availabilityForCandidate(candidate, scoreByID, scoreByCandidate); ok {
		score = av.Score
		confidence = av.ScoreConfidence
		evidence = append(evidence, av.AvailabilityScoreID)
		evidence = append(evidence, av.EvidenceRecordIDs...)
	}
	return component(ComponentHealth, score, policy, confidence, confidence != providerinventory.ConfidenceExact, "health score uses the persisted availability score after hard availability gates pass", evidence, nil, nil)
}

func scoreDiversity(candidate Candidate, policy OptimizationPolicy) ComponentScore {
	score := 100
	for _, selected := range policy.DiversityHistory {
		if selected.AdapterID != "" && selected.AdapterID == candidate.AdapterID {
			score -= 30
		}
		if selected.AccountProfileID != "" && selected.AccountProfileID == candidate.AccountProfileID {
			score -= 30
		}
		if selected.ModelCapabilityID != "" && selected.ModelCapabilityID == candidate.ModelCapabilityID {
			score -= 20
		}
	}
	return component(ComponentDiversity, clampScore(score), policy, providerinventory.ConfidenceExact, false, "", nil, nil, nil)
}

func scoreLocality(candidate Candidate, policy OptimizationPolicy) ComponentScore {
	score := 50
	for _, adapterID := range policy.LocalAdapterIDs {
		if adapterID == candidate.AdapterID {
			score = 100
			break
		}
	}
	if len(policy.LocalAdapterIDs) == 0 && candidate.NetworkPermission == providerinventory.NetworkNotNeeded {
		score = 80
	}
	return component(ComponentLocality, score, policy, providerinventory.ConfidenceEstimated, true, "locality uses profile local adapter preference and network declaration", nil, nil, nil)
}

func scoreUserPreference(candidate Candidate, policy OptimizationPolicy) ComponentScore {
	score := 50
	for _, candidateID := range policy.PreferredCandidateIDs {
		if candidateID == candidate.RoutingCandidateID {
			score = 100
			break
		}
	}
	return component(ComponentUserPreference, score, policy, providerinventory.ConfidenceExact, false, "", policy.PreferredCandidateIDs, nil, nil)
}

func component(name ComponentName, score int, policy OptimizationPolicy, confidence providerinventory.Confidence, heuristic bool, reason string, evidence, snapshots []string, value *int64) ComponentScore {
	weight := policy.Weights[name]
	return ComponentScore{
		Name:              name,
		Score:             clampScore(score),
		Weight:            weight,
		WeightedScore:     formatBasisPoints(clampScore(score) * weight * 100),
		EvidenceValue:     value,
		Confidence:        confidence,
		Heuristic:         heuristic,
		HeuristicReason:   reason,
		EvidenceRecordIDs: dedupeStrings(evidence),
		SnapshotIDs:       dedupeStrings(snapshots),
	}
}

func quotaSnapshotBetterForScore(candidate, current providerinventory.QuotaSnapshot) bool {
	candidateValue := int64(0)
	currentValue := int64(0)
	if candidate.RemainingValue != nil {
		candidateValue = *candidate.RemainingValue
	}
	if current.RemainingValue != nil {
		currentValue = *current.RemainingValue
	}
	if candidateValue != currentValue {
		return candidateValue > currentValue
	}
	if confidenceRank(candidate.Confidence) != confidenceRank(current.Confidence) {
		return confidenceRank(candidate.Confidence) > confidenceRank(current.Confidence)
	}
	return candidate.QuotaSnapshotID < current.QuotaSnapshotID
}

func budgetSummaryBetterForScore(candidate, current budget.Summary) bool {
	if candidate.AvailableValue != current.AvailableValue {
		return candidate.AvailableValue > current.AvailableValue
	}
	if confidenceRank(candidate.Confidence) != confidenceRank(current.Confidence) {
		return confidenceRank(candidate.Confidence) > confidenceRank(current.Confidence)
	}
	return candidate.BudgetPolicyID < current.BudgetPolicyID
}

func availabilityForCandidate(candidate Candidate, scoreByID map[string]availability.Score, scoreByCandidate map[string]availability.Score) (availability.Score, bool) {
	if score, ok := scoreByID[candidate.AvailabilityScoreID]; ok {
		return score, true
	}
	score, ok := scoreByCandidate[availabilityKey(candidate)]
	return score, ok
}

func heuristicComponents(scored []ScoredCandidate) []ComponentScore {
	seen := map[string]ComponentScore{}
	for _, candidate := range scored {
		for _, component := range candidate.Components {
			if component.Heuristic {
				seen[string(component.Name)] = component
			}
		}
	}
	out := make([]ComponentScore, 0, len(seen))
	for _, component := range seen {
		out = append(out, component)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func candidateLabel(candidate Candidate) string {
	parts := []string{candidate.AdapterID, candidate.ModelCapabilityID}
	if candidate.AccountProfileID != "" {
		parts = append(parts, candidate.AccountProfileID)
	}
	return strings.Join(parts, "/")
}

func rejectionExplanation(reasons []RejectionReason) string {
	if len(reasons) == 0 {
		return "rejected by deterministic eligibility"
	}
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		if strings.TrimSpace(reason.Message) == "" {
			parts = append(parts, string(reason.Code))
			continue
		}
		parts = append(parts, string(reason.Code)+" ("+reason.Message+")")
	}
	sort.Strings(parts)
	return "rejected by hard eligibility: " + strings.Join(parts, ", ")
}

func rejectedSummary(rejected []RejectedCandidate) map[RejectionCode]int {
	out := map[RejectionCode]int{}
	for _, candidate := range rejected {
		for _, reason := range candidate.Reasons {
			out[reason.Code]++
		}
	}
	return out
}

func blockedErrorCode(rejected []RejectedCandidate) taskrequirements.ErrorCode {
	for _, candidate := range rejected {
		for _, reason := range candidate.Reasons {
			if reason.Code == RejectPinnedCandidateIneligible {
				return taskrequirements.ErrPinnedCandidateIneligibleCode
			}
		}
	}
	return taskrequirements.ErrNoEligibleCandidateCode
}

func routingTerminalError(code taskrequirements.ErrorCode) error {
	switch code {
	case taskrequirements.ErrNoEligibleCandidateCode:
		return taskrequirements.ErrNoEligibleCandidate
	case taskrequirements.ErrPinnedCandidateIneligibleCode:
		return &taskrequirements.TypedError{Code: taskrequirements.ErrPinnedCandidateIneligibleCode}
	case taskrequirements.ErrorCode(delivery.ErrApprovalRequiredCode):
		return delivery.ErrApprovalRequired
	case taskrequirements.ErrReplanRequiredCode:
		return taskrequirements.ErrReplanRequired
	case taskrequirements.ErrReplanBoundExceededCode:
		return taskrequirements.ErrReplanBoundExceeded
	default:
		return &taskrequirements.TypedError{Code: code}
	}
}

func inputRefs(input DecisionInput, result Result) []InputRecordRef {
	refs := []InputRecordRef{}
	providerInventory := providerInventoryFingerprintProjection(input.Inputs.Inventory)
	quotaByID := mapQuotaSnapshots(input.Inputs.Inventory.QuotaSnapshots)
	availabilityByID, _ := mapAvailabilityScores(input.Inputs.Availability)
	budgetByID := mapBudgets(input.Inputs.Budgets)
	breakerByID := mapBreakers(input.Inputs.CircuitBreakers)
	addRef := func(kind, id, fingerprint string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		refs = append(refs, InputRecordRef{RecordKind: kind, RecordID: id, Fingerprint: fingerprint})
	}
	addRef("delivery_run_authorization", input.DeliveryRunID, input.AuthorizationFingerprint)
	addRef("prior_routing_decision", input.PriorRoutingDecisionID, input.PriorRoutingFingerprint)
	addRef("task_requirement", input.TaskRequirementID, input.Inputs.Requirement.TaskRequirementFingerprint)
	if role, ok := ResolveRoleDefinition(input.Inputs.Requirement.RoleKey, input.Inputs.RoleDefinitions); ok {
		fp := "sha256:" + hashCanonical(role)
		addRef("role_definition", firstNonEmpty(input.RoleDefinitionID, role.RoleDefinitionID), fp)
	}
	addRef("provider_inventory", providerInventory.InventoryFingerprint, providerInventory.InventoryFingerprint)
	for _, installation := range input.Inputs.Inventory.Installations {
		addRef("provider_installation", installation.ProviderInstallationID, recordEvidenceFingerprint(installation))
	}
	for _, account := range input.Inputs.Inventory.AccountProfiles {
		addRef("account_profile", account.AccountProfileID, recordEvidenceFingerprint(account))
	}
	for _, readiness := range input.Inputs.Inventory.AuthReadiness {
		addRef("auth_readiness", readiness.AuthReadinessID, recordEvidenceFingerprint(readiness))
	}
	for _, catalog := range input.Inputs.Inventory.ModelCatalogSnapshots {
		addRef("model_catalog_snapshot", catalog.ModelCatalogSnapshotID, recordEvidenceFingerprint(catalog))
	}
	for _, model := range input.Inputs.Inventory.ModelCapabilities {
		addRef("model_capability", model.ModelCapabilityID, recordEvidenceFingerprint(model))
	}
	for _, candidate := range append([]Candidate{}, result.Eligible...) {
		addRef("routing_candidate", candidate.RoutingCandidateID, candidate.CandidateFingerprint)
		for _, id := range candidate.QuotaSnapshotIDs {
			addRef("quota_snapshot", id, quotaSnapshotFingerprint(quotaByID, id))
		}
		for _, id := range candidate.BudgetPolicyIDs {
			addRef("budget_policy", id, evidenceFingerprint(budgetByID, id))
		}
		if candidate.AvailabilityScoreID != "" {
			addRef("availability_score", candidate.AvailabilityScoreID, evidenceFingerprint(availabilityByID, candidate.AvailabilityScoreID))
		}
		for _, id := range candidate.CircuitBreakerIDs {
			addRef("circuit_breaker", id, evidenceFingerprint(breakerByID, id))
		}
	}
	for _, rejected := range result.Rejected {
		candidate := rejected.Candidate
		addRef("routing_candidate", candidate.RoutingCandidateID, candidate.CandidateFingerprint)
		for _, reason := range rejected.Reasons {
			for _, id := range reason.EvidenceRecordIDs {
				fingerprint := firstNonEmpty(
					evidenceFingerprint(availabilityByID, id),
					evidenceFingerprint(budgetByID, id),
					evidenceFingerprint(breakerByID, id),
				)
				addRef("rejection_evidence", id, fingerprint)
			}
			for _, id := range reason.SnapshotIDs {
				addRef("rejection_snapshot", id, quotaSnapshotFingerprint(quotaByID, id))
			}
		}
	}
	for _, pin := range input.Inputs.Pins {
		addRef("user_pin", pin.PinID, "sha256:"+hashCanonical(pin))
	}
	for _, exclusion := range input.Inputs.Exclusions {
		addRef("user_exclusion", exclusion.ExclusionID, "sha256:"+hashCanonical(exclusion))
	}
	for _, record := range safePolicyInputRecords(input.PolicyInputRecords) {
		addRef("routing_policy_input", record.RoutingPolicyInputID, recordEvidenceFingerprint(record))
	}
	if hasRoutingPolicyProfile(input.RoutingPolicyProfile) {
		addRef("routing_policy_profile", input.RoutingPolicyProfile.RoutingPolicyProfileID, input.RoutingPolicyProfile.PolicyFingerprint)
	}
	for _, override := range redactOverrideProvenance(input.OverrideProvenance) {
		addRef("override", override.OverrideID, recordEvidenceFingerprint(override))
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].RecordKind != refs[j].RecordKind {
			return refs[i].RecordKind < refs[j].RecordKind
		}
		if refs[i].RecordID != refs[j].RecordID {
			return refs[i].RecordID < refs[j].RecordID
		}
		return refs[i].Fingerprint < refs[j].Fingerprint
	})
	deduped := refs[:0]
	seen := map[string]bool{}
	for _, ref := range refs {
		key := ref.RecordKind + "\x00" + ref.RecordID + "\x00" + ref.Fingerprint
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, ref)
	}
	return deduped
}

func recordEvidenceFingerprint(value any) string {
	return "sha256:" + hashCanonical(value)
}

func quotaSnapshotFingerprint(quotaByID map[string]providerinventory.QuotaSnapshot, id string) string {
	snapshot, ok := quotaByID[id]
	if !ok {
		return ""
	}
	return "sha256:" + hashCanonical(snapshot)
}

func evidenceFingerprint[T any](values map[string]T, id string) string {
	value, ok := values[id]
	if !ok {
		return ""
	}
	return "sha256:" + hashCanonical(value)
}

func breakerRefs(result Result) []string {
	var ids []string
	for _, candidate := range result.Eligible {
		ids = append(ids, candidate.CircuitBreakerIDs...)
	}
	for _, rejected := range result.Rejected {
		ids = append(ids, rejected.Candidate.CircuitBreakerIDs...)
	}
	return dedupeStrings(ids)
}

func firstCandidateTaskID(candidates []Candidate) string {
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.TaskID) != "" {
			return candidate.TaskID
		}
	}
	return ""
}

func routingDecisionID(projectID, deliveryRunID, decisionKey, taskID, routingFingerprint string) string {
	return "rdec_" + digestBase32(projectID, deliveryRunID, decisionKey, taskID, routingFingerprint)
}

func digestBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
}

func tieBreakValue(seed, candidateID string) string {
	sum := sha256.Sum256([]byte(seed + "\x00" + candidateID))
	return fmt.Sprintf("%x", sum[:])
}

func shortDigest(digest string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func canonicalString(value any) (string, error) {
	data, err := delivery.CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func formatBasisPoints(value int) string {
	whole := value / 10000
	fraction := (value % 10000) / 100
	return strconv.Itoa(whole) + "." + fmt.Sprintf("%02d", fraction)
}

func clampScore(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func nonNilCandidates(values []Candidate) []Candidate {
	if values == nil {
		return []Candidate{}
	}
	return values
}

func nonNilRejectedCandidates(values []RejectedCandidate) []RejectedCandidate {
	if values == nil {
		return []RejectedCandidate{}
	}
	return values
}

func nonNilOverrideProvenance(values []OverrideProvenance) []OverrideProvenance {
	if values == nil {
		return []OverrideProvenance{}
	}
	return values
}

func hasRoutingPolicyProfile(profile RoutingPolicyProfile) bool {
	return strings.TrimSpace(profile.RoutingPolicyProfileID) != "" ||
		strings.TrimSpace(profile.ProfileKey) != "" ||
		strings.TrimSpace(profile.PolicyFingerprint) != ""
}

func isZeroOptimizationPolicy(policy OptimizationPolicy) bool {
	return strings.TrimSpace(policy.SchemaVersion) == "" &&
		strings.TrimSpace(policy.RoutingPolicyProfileID) == "" &&
		strings.TrimSpace(policy.ProfileKey) == "" &&
		strings.TrimSpace(policy.PolicyVersion) == "" &&
		len(policy.Weights) == 0
}

func isZeroHardPolicy(policy Policy) bool {
	return policy.EvidencePolicy == "" &&
		!policy.RequireExactQuota &&
		!policy.RequireBudgetEvidence &&
		!policy.RequireAvailabilityEvidence &&
		!policy.RequireBoundedScope &&
		policy.ContextReserveTokens == 0 &&
		policy.VerifierIndependence == "" &&
		!policy.AllowPaidOverage &&
		!policy.AllowHalfOpenBreakerProbe
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func validateDecisionInput(input DecisionInput, policy OptimizationPolicy) error {
	if strings.TrimSpace(input.TaskRequirementID) == "" || strings.TrimSpace(input.RoleDefinitionID) == "" || strings.TrimSpace(input.PlanFingerprint) == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "task_requirement_id, role_definition_id, and plan_fingerprint are required"}
	}
	if input.Now.IsZero() {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision clock is required"}
	}
	if !validFingerprint(input.PlanFingerprint) || !validFingerprint(input.PolicyFingerprint) || !validFingerprint(policy.PolicyFingerprint) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision fingerprints must be sha256 digests"}
	}
	if input.AuthorizationFingerprint != "" && !validFingerprint(input.AuthorizationFingerprint) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision authorization fingerprint must be a sha256 digest"}
	}
	if (strings.TrimSpace(input.PriorRoutingDecisionID) == "") != (strings.TrimSpace(input.PriorRoutingFingerprint) == "") ||
		(input.PriorRoutingFingerprint != "" && !validFingerprint(input.PriorRoutingFingerprint)) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "prior routing decision id and sha256 fingerprint must be provided together"}
	}
	if input.TaskRequirementID != input.Inputs.Requirement.TaskRequirementID {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "task_requirement_id does not match requirement record"}
	}
	if input.ProjectID != input.Inputs.Requirement.ProjectID || input.DeliveryRunID != input.Inputs.Requirement.DeliveryRunID {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision project/run does not match task requirement"}
	}
	if input.PlanFingerprint != input.Inputs.Requirement.PlanFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision plan fingerprint does not match task requirement"}
	}
	if err := taskrequirements.Validate(input.Inputs.Requirement); err != nil {
		return err
	}
	if err := validateActor(input.DecidedBy); err != nil {
		return err
	}
	return validateHost(input.Host)
}

func validateRoutingDecision(decision RoutingDecision) error {
	if decision.SchemaVersion != DecisionSchema || decision.RecordVersion != 1 ||
		decision.RoutingDecisionID == "" || decision.DecisionKey == "" || decision.DecisionKind != DecisionKindRouting ||
		decision.ProjectID == "" || decision.DeliveryRunID == "" || decision.TaskID == "" ||
		decision.TaskRequirementID == "" || decision.RoutingPolicyProfileID == "" || decision.RoleDefinitionID == "" ||
		decision.PlanFingerprint == "" || decision.PolicyFingerprint == "" || decision.RoutingFingerprint == "" ||
		decision.CandidateGenerationStatus == "" || decision.DecisionStatus == "" || decision.CreatedAt == "" || decision.UpdatedAt == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision has missing required fields"}
	}
	if decision.CandidateGenerationStatus != CandidateGenerationFull && decision.CandidateGenerationStatus != CandidateGenerationNeedsHuman {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown routing candidate generation status"}
	}
	if decision.DecisionStatus != DecisionStatusSelected && decision.DecisionStatus != DecisionStatusNoEligible {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown routing decision status"}
	}
	if decision.CandidateGenerationStatus == CandidateGenerationNeedsHuman &&
		(decision.DecisionStatus != DecisionStatusNoEligible || decision.TerminalErrorCode != taskrequirements.ErrNoEligibleCandidateCode || len(decision.EligibleCandidates) != 0 || len(decision.RejectedCandidates) != 0 || len(decision.ScoredCandidates) != 0 || decision.ChosenReason != CandidateGenerationZeroReason) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "needs-human candidate generation must be an actionable zero-candidate decision"}
	}
	if !validFingerprint(decision.PlanFingerprint) || !validFingerprint(decision.PolicyFingerprint) || !validFingerprint(decision.RoutingFingerprint) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision fingerprints must be sha256 digests"}
	}
	if decision.AuthorizationFingerprint != "" && !validFingerprint(decision.AuthorizationFingerprint) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision authorization fingerprint must be a sha256 digest"}
	}
	// Empty class fields are accepted only for persisted pre-v0.8.1 decisions.
	// Every newly built decision resolves both fields before persistence.
	if decision.BudgetClass != "" && !ValidBudgetClass(decision.BudgetClass) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision budget_class is invalid"}
	}
	if decision.DeadlineClass != "" && !ValidDeadlineClass(decision.DeadlineClass) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision deadline_class is invalid"}
	}
	expectedPolicyFingerprint := decision.OptimizationPolicy.PolicyFingerprint
	if decision.RoutingPolicyProfile != nil {
		expectedPolicyFingerprint = decision.RoutingPolicyProfile.PolicyFingerprint
	}
	if decision.PolicyFingerprint != expectedPolicyFingerprint {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "routing decision policy fingerprint does not match active routing policy"}
	}
	if decision.RoutingDecisionID != routingDecisionID(decision.ProjectID, decision.DeliveryRunID, decision.DecisionKey, decision.TaskID, decision.RoutingFingerprint) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "routing decision id does not match routing fingerprint"}
	}
	if len(decision.InputRecordRefs) == 0 || decision.UserPinRefs == nil || decision.FallbackChain == nil || decision.BreakerGateRefs == nil || decision.HeuristicComponents == nil || decision.RejectedSummary == nil {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision required arrays/maps must be present"}
	}
	if err := validateActor(decision.DecidedBy); err != nil {
		return err
	}
	if err := validateHost(decision.Host); err != nil {
		return err
	}
	if decision.DecisionStatus == DecisionStatusSelected {
		if decision.ChosenCandidateID == "" || decision.TerminalErrorCode != "" {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "selected routing decision must have chosen candidate and no terminal error"}
		}
		found := false
		for _, candidate := range decision.ScoredCandidates {
			if candidate.RoutingCandidateID == decision.ChosenCandidateID {
				found = true
				break
			}
		}
		if !found {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "chosen candidate was not scored"}
		}
		return nil
	}
	if decision.ChosenCandidateID != "" || len(decision.ScoredCandidates) != 0 || decision.TerminalErrorCode == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "blocked routing decision must not choose or score candidates"}
	}
	if decision.TerminalErrorCode != taskrequirements.ErrNoEligibleCandidateCode && decision.TerminalErrorCode != taskrequirements.ErrPinnedCandidateIneligibleCode {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown routing terminal error code"}
	}
	return nil
}

func validateActor(actor delivery.Actor) error {
	if strings.TrimSpace(actor.ActorKind) == "" || strings.TrimSpace(actor.ActorID) == "" || strings.TrimSpace(actor.DecisionAuthority) == "" || strings.TrimSpace(actor.Source) == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision provenance actor has missing required fields"}
	}
	if actor.DecisionAuthority != "router" && actor.DecisionAuthority != "user" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision authority must be router or user"}
	}
	return nil
}

func validateHost(host delivery.Host) error {
	if strings.TrimSpace(host.HostKind) == "" || strings.TrimSpace(host.HostID) == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision host has missing required fields"}
	}
	return nil
}

func validFingerprint(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, ch := range value[len("sha256:"):] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
