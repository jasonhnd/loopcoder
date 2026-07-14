package routing

import (
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
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	DecisionSchema           = "loopcoder.routing_decision.v1"
	DecisionKindRouting      = "routing"
	DefaultProfileID         = "rprof_balanced_v1"
	DefaultProfileVersion    = "balanced-v1"
	DefaultTieBreakSeed      = "0804-routing-balanced-v1"
	CandidateGenerationFull  = "complete"
	DecisionStatusSelected   = "selected"
	DecisionStatusNoEligible = "no-eligible-candidate"
)

type ComponentName string

const (
	ComponentQualityFit     ComponentName = "quality_fit"
	ComponentQuotaHeadroom  ComponentName = "quota_headroom"
	ComponentAvailability   ComponentName = "availability"
	ComponentCost           ComponentName = "cost"
	ComponentLatency        ComponentName = "latency"
	ComponentDiversity      ComponentName = "diversity"
	ComponentLocality       ComponentName = "locality"
	ComponentUserPreference ComponentName = "user_preference"
)

var supportedComponents = map[ComponentName]bool{
	ComponentAvailability:   true,
	ComponentQuotaHeadroom:  true,
	ComponentQualityFit:     true,
	ComponentLatency:        true,
	ComponentCost:           true,
	ComponentDiversity:      true,
	ComponentLocality:       true,
	ComponentUserPreference: true,
}

var scoringComponentOrder = []ComponentName{
	ComponentAvailability,
	ComponentQuotaHeadroom,
	ComponentQualityFit,
	ComponentLatency,
	ComponentCost,
	ComponentDiversity,
	ComponentLocality,
	ComponentUserPreference,
}

type OptimizationPolicy struct {
	SchemaVersion          string                `json:"schema_version"`
	RoutingPolicyProfileID string                `json:"routing_policy_profile_id"`
	ProfileKey             string                `json:"profile_key"`
	ProfileVersion         string                `json:"profile_version"`
	PolicyVersion          string                `json:"policy_version"`
	Weights                map[ComponentName]int `json:"weights"`
	TieBreakSeed           string                `json:"tie_break_seed"`
	LocalAdapterIDs        []string              `json:"local_adapter_ids"`
	PreferredCandidateIDs  []string              `json:"preferred_candidate_ids"`
	DiversityHistory       []SelectedRouteRef    `json:"diversity_history"`
	PolicyFingerprint      string                `json:"policy_fingerprint"`
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
	if err := validateDecisionInput(input, policy); err != nil {
		return RoutingDecision{}, err
	}
	eligibility := FilterHardEligibility(input.Inputs)
	refs := inputRefs(input, eligibility)
	routingFingerprint, err := routingFingerprint(input, policy, eligibility, refs)
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
		RoutingFingerprint:        routingFingerprint,
		InputRecordRefs:           refs,
		CandidateGenerationStatus: CandidateGenerationFull,
		EligibleCandidates:        nonNilCandidates(eligibility.Eligible),
		RejectedCandidates:        nonNilRejectedCandidates(eligibility.Rejected),
		UserPinRefs:               nonNilStrings(pinIDs(input.Inputs.Pins)),
		FallbackChain:             []string{},
		BreakerGateRefs:           nonNilStrings(breakerRefs(eligibility)),
		PolicyInputRecords:        nonNilPolicyInputRecords(input.PolicyInputRecords),
		OverrideProvenance:        nonNilOverrideProvenance(input.OverrideProvenance),
		OptimizationPolicy:        policy,
		RejectedSummary:           rejectedSummary(eligibility.Rejected),
		CreatedAt:                 delivery.CanonicalTimestamp(input.Now),
		UpdatedAt:                 delivery.CanonicalTimestamp(input.Now),
		DecidedBy:                 input.DecidedBy,
		Host:                      input.Host,
	}
	if hasRoutingPolicyProfile(input.RoutingPolicyProfile) {
		profile := input.RoutingPolicyProfile
		decision.RoutingPolicyProfile = &profile
	}
	decision.RoutingDecisionID = routingDecisionID(decision.ProjectID, decision.DeliveryRunID, decision.DecisionKey, decision.TaskID, decision.RoutingFingerprint)
	decision.ScoredCandidates = scoreCandidates(eligibility.Eligible, input.Inputs, policy)
	decision.HeuristicComponents = heuristicComponents(decision.ScoredCandidates)
	if len(decision.ScoredCandidates) == 0 {
		decision.DecisionStatus = DecisionStatusNoEligible
		decision.TerminalErrorCode = blockedErrorCode(eligibility.Rejected)
		decision.ChosenReason = "no hard-eligible candidates remain after deterministic eligibility"
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
	return delivery.CanonicalJSON(decision)
}

func ExplainHuman(decision RoutingDecision) string {
	var b strings.Builder
	if decision.DecisionStatus == DecisionStatusSelected {
		fmt.Fprintf(&b, "selected %s: %s\n", decision.ChosenCandidateID, decision.ChosenReason)
	} else {
		fmt.Fprintf(&b, "blocked %s: %s\n", decision.DecisionStatus, decision.TerminalErrorCode)
	}
	if decision.RoutingPolicyProfile != nil {
		fmt.Fprintf(&b, "profile %s version %s fingerprint %s\n", decision.RoutingPolicyProfile.ProfileKey, decision.RoutingPolicyProfile.ProfileVersion, decision.RoutingPolicyProfile.PolicyFingerprint)
	}
	for _, override := range decision.OverrideProvenance {
		fmt.Fprintf(&b, "override %s: %s scope=%s actor=%s expires=%s fingerprint=%s\n", override.OverrideID, override.Reason, override.Scope, override.Actor.ActorID, firstNonEmpty(override.ExpiresAt, "none"), override.PolicyFingerprint)
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
	return strings.TrimSpace(b.String())
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
	if len(policy.Weights) == 0 {
		policy.Weights = map[ComponentName]int{
			ComponentAvailability:  30,
			ComponentQuotaHeadroom: 20,
			ComponentQualityFit:    20,
			ComponentLatency:       10,
			ComponentCost:          10,
			ComponentDiversity:     10,
		}
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

func routingFingerprint(input DecisionInput, policy OptimizationPolicy, result Result, refs []InputRecordRef) (string, error) {
	payload := map[string]any{
		"schema_version":      "loopcoder.routing_fingerprint_input.v1",
		"decision_key":        input.DecisionKey,
		"project_id":          input.ProjectID,
		"delivery_run_id":     input.DeliveryRunID,
		"task_requirement_id": input.TaskRequirementID,
		"role_definition_id":  input.RoleDefinitionID,
		"role_definitions":    input.Inputs.RoleDefinitions,
		"plan_fingerprint":    input.PlanFingerprint,
		"policy_fingerprint":  firstNonEmpty(input.PolicyFingerprint, policy.PolicyFingerprint),
		"hard_policy":         input.Inputs.Policy,
		"optimization_policy": policy,
		"input_record_refs":   refs,
		"eligible_candidates": result.Eligible,
		"rejected_candidates": result.Rejected,
		"user_pins":           input.Inputs.Pins,
		"exclusions":          input.Inputs.Exclusions,
		"worker_route":        input.Inputs.WorkerRoute,
	}
	if hasRoutingPolicyProfile(input.RoutingPolicyProfile) {
		payload["routing_profile"] = input.RoutingPolicyProfile
	}
	if len(input.OverrideProvenance) > 0 {
		payload["override_provenance"] = input.OverrideProvenance
	}
	if len(input.PolicyInputRecords) > 0 {
		payload["policy_input_records"] = input.PolicyInputRecords
	}
	digest, _, err := delivery.DigestCanonicalJSON(payload)
	return digest, err
}

func scoreCandidates(candidates []Candidate, inputs Inputs, policy OptimizationPolicy) []ScoredCandidate {
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
		parts = append(parts, string(reason.Code))
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
	addRef := func(kind, id, fingerprint string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		refs = append(refs, InputRecordRef{RecordKind: kind, RecordID: id, Fingerprint: fingerprint})
	}
	addRef("task_requirement", input.TaskRequirementID, input.Inputs.Requirement.TaskRequirementFingerprint)
	if role, ok := ResolveRoleDefinition(input.Inputs.Requirement.RoleKey, input.Inputs.RoleDefinitions); ok {
		fp := "sha256:" + hashCanonical(role)
		addRef("role_definition", firstNonEmpty(input.RoleDefinitionID, role.RoleDefinitionID), fp)
	}
	for _, candidate := range append([]Candidate{}, result.Eligible...) {
		addRef("routing_candidate", candidate.RoutingCandidateID, candidate.CandidateFingerprint)
		for _, id := range candidate.QuotaSnapshotIDs {
			addRef("quota_snapshot", id, "")
		}
		for _, id := range candidate.BudgetPolicyIDs {
			addRef("budget_policy", id, "")
		}
		if candidate.AvailabilityScoreID != "" {
			addRef("availability_score", candidate.AvailabilityScoreID, "")
		}
		for _, id := range candidate.CircuitBreakerIDs {
			addRef("circuit_breaker", id, "")
		}
	}
	for _, rejected := range result.Rejected {
		candidate := rejected.Candidate
		addRef("routing_candidate", candidate.RoutingCandidateID, candidate.CandidateFingerprint)
		for _, reason := range rejected.Reasons {
			for _, id := range reason.EvidenceRecordIDs {
				addRef("rejection_evidence", id, "")
			}
			for _, id := range reason.SnapshotIDs {
				addRef("rejection_snapshot", id, "")
			}
		}
	}
	for _, pin := range input.Inputs.Pins {
		addRef("user_pin", pin.PinID, "")
	}
	for _, exclusion := range input.Inputs.Exclusions {
		addRef("user_exclusion", exclusion.ExclusionID, "")
	}
	for _, record := range input.PolicyInputRecords {
		addRef("routing_policy_input", record.RoutingPolicyInputID, record.PolicyFingerprint)
	}
	if hasRoutingPolicyProfile(input.RoutingPolicyProfile) {
		addRef("routing_policy_profile", input.RoutingPolicyProfile.RoutingPolicyProfileID, input.RoutingPolicyProfile.PolicyFingerprint)
	}
	for _, override := range input.OverrideProvenance {
		addRef("override", override.OverrideID, override.PolicyFingerprint)
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

func nonNilPolicyInputRecords(values []PolicyInputRecord) []PolicyInputRecord {
	if values == nil {
		return []PolicyInputRecord{}
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
	if decision.CandidateGenerationStatus != CandidateGenerationFull {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown routing candidate generation status"}
	}
	if decision.DecisionStatus != DecisionStatusSelected && decision.DecisionStatus != DecisionStatusNoEligible {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown routing decision status"}
	}
	if !validFingerprint(decision.PlanFingerprint) || !validFingerprint(decision.PolicyFingerprint) || !validFingerprint(decision.RoutingFingerprint) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing decision fingerprints must be sha256 digests"}
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
