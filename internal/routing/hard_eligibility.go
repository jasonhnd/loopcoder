// Package routing implements deterministic, provider-neutral routing gates.
package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/availability"
	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	CandidateSchema = "loopcoder.routing_candidate.v1"
	PolicyVersion   = "routing-hard-eligibility-v1"
)

type RejectionCode string

const (
	RejectRoleUnsupported                  RejectionCode = "role-unsupported"
	RejectPermissionUnsupported            RejectionCode = "permission-unsupported"
	RejectSideEffectClassExceeded          RejectionCode = "side-effect-class-exceeded"
	RejectScopeUnsupported                 RejectionCode = "scope-unsupported"
	RejectReadOnlyUnsupported              RejectionCode = "read-only-unsupported"
	RejectJSONOutputUnsupported            RejectionCode = "json-output-unsupported"
	RejectNestedSubagentsUnsupported       RejectionCode = "nested-subagents-unsupported"
	RejectMCPConfigUnsupported             RejectionCode = "mcp-config-unsupported"
	RejectCancellationUnsupported          RejectionCode = "cancellation-unsupported"
	RejectUsageReportingUnsupported        RejectionCode = "usage-reporting-unsupported"
	RejectContextWindowInsufficient        RejectionCode = "context-window-insufficient"
	RejectToolSupportUnsupported           RejectionCode = "tool-support-unsupported"
	RejectImageInputUnsupported            RejectionCode = "image-input-unsupported"
	RejectImageOutputUnsupported           RejectionCode = "image-output-unsupported"
	RejectModelUnavailable                 RejectionCode = "model-unavailable"
	RejectAuthNotReady                     RejectionCode = "auth-not-ready"
	RejectAccountProfileAmbiguous          RejectionCode = "account-profile-ambiguous"
	RejectQuotaConfidenceInsufficient      RejectionCode = "quota-confidence-insufficient"
	RejectQuotaExhausted                   RejectionCode = "quota-exhausted"
	RejectBudgetExhausted                  RejectionCode = "budget-exhausted"
	RejectAvailabilityHardIneligible       RejectionCode = "availability-hard-ineligible"
	RejectBreakerOpen                      RejectionCode = "breaker-open"
	RejectNetworkPermissionMissing         RejectionCode = "network-permission-missing"
	RejectDataClassificationUnsupported    RejectionCode = "data-classification-unsupported"
	RejectRiskTierUnsupported              RejectionCode = "risk-tier-unsupported"
	RejectQualityFloorInsufficient         RejectionCode = "quality-floor-insufficient"
	RejectVerificationRouteMissing         RejectionCode = "verification-route-missing"
	RejectVerifierIndependenceInsufficient RejectionCode = "verifier-independence-insufficient"
	RejectPinnedCandidateNotMatched        RejectionCode = "pinned-candidate-not-matched"
	RejectPinnedCandidateIneligible        RejectionCode = "pinned-candidate-ineligible"
	RejectCandidateExcluded                RejectionCode = "candidate-excluded"
	RejectEvidenceStale                    RejectionCode = "evidence-stale"
	RejectRoutingFingerprintMismatch       RejectionCode = "routing-fingerprint-mismatch"
	RejectUnknownRecordVersion             RejectionCode = "unknown-record-version"
	RejectUnknownTelemetry                 RejectionCode = "unknown-telemetry"
)

type Candidate struct {
	SchemaVersion          string                              `json:"schema_version"`
	RoutingCandidateID     string                              `json:"routing_candidate_id"`
	TaskID                 string                              `json:"task_id"`
	RoleKey                string                              `json:"role_key"`
	AdapterID              string                              `json:"adapter_id"`
	ProviderInstallationID string                              `json:"provider_installation_id,omitempty"`
	AccountProfileID       string                              `json:"account_profile_id,omitempty"`
	AuthReadinessID        string                              `json:"auth_readiness_id,omitempty"`
	ModelCapabilityID      string                              `json:"model_capability_id"`
	CanonicalModelID       string                              `json:"canonical_model_id,omitempty"`
	InvocationProfileKey   string                              `json:"invocation_profile_key"`
	Permission             taskrequirements.Permission         `json:"permission"`
	LaunchSideEffectClass  taskrequirements.SideEffectClass    `json:"launch_side_effect_class"`
	NetworkPermission      providerinventory.NetworkPermission `json:"network_permission"`
	ScopeBounded           bool                                `json:"scope_bounded"`
	QualityFloor           taskrequirements.QualityFloor       `json:"quality_floor,omitempty"`
	RequiredContextTokens  int                                 `json:"required_context_tokens,omitempty"`
	RequiredTools          []string                            `json:"required_tools,omitempty"`
	QuotaSnapshotIDs       []string                            `json:"quota_snapshot_ids"`
	BudgetPolicyIDs        []string                            `json:"budget_policy_ids"`
	AvailabilityScoreID    string                              `json:"availability_score_id,omitempty"`
	CircuitBreakerIDs      []string                            `json:"circuit_breaker_ids"`
	CapabilitySummary      map[string]any                      `json:"capability_summary"`
	CandidateFingerprint   string                              `json:"candidate_fingerprint"`
}

type Pin struct {
	PinID                  string `json:"pin_id"`
	AdapterID              string `json:"adapter_id,omitempty"`
	ProviderInstallationID string `json:"provider_installation_id,omitempty"`
	AccountProfileID       string `json:"account_profile_id,omitempty"`
	ModelCapabilityID      string `json:"model_capability_id,omitempty"`
	InvocationProfileKey   string `json:"invocation_profile_key,omitempty"`
}

type Exclusion struct {
	ExclusionID            string `json:"exclusion_id"`
	AdapterID              string `json:"adapter_id,omitempty"`
	ProviderInstallationID string `json:"provider_installation_id,omitempty"`
	AccountProfileID       string `json:"account_profile_id,omitempty"`
	ModelCapabilityID      string `json:"model_capability_id,omitempty"`
	InvocationProfileKey   string `json:"invocation_profile_key,omitempty"`
}

type EvidencePolicy string

const (
	EvidenceRejectUnknownStale EvidencePolicy = "reject-unknown-stale"
	EvidenceAllowEstimated     EvidencePolicy = "allow-estimated"
)

type Policy struct {
	EvidencePolicy              EvidencePolicy
	RequireExactQuota           bool
	RequireBudgetEvidence       bool
	RequireAvailabilityEvidence bool
	RequireBoundedScope         bool
	ContextReserveTokens        int
	VerifierIndependence        taskrequirements.IndependenceLevel
	AllowHalfOpenBreakerProbe   bool
}

type Inputs struct {
	Requirement     taskrequirements.TaskRequirement
	RoleDefinitions []RoleDefinition
	Candidates      []Candidate
	Inventory       providerinventory.Report
	Availability    []availability.Score
	CircuitBreakers []availability.CircuitBreaker
	Budgets         []budget.Summary
	RuntimeContract runtimecap.Contract
	HostName        string
	Policy          Policy
	Pins            []Pin
	Exclusions      []Exclusion
	WorkerRoute     *Candidate
}

func InputsWithCachedInventory(ctx context.Context, store storage.Store, inputs Inputs) (Inputs, error) {
	report, err := providerinventory.Load(ctx, store)
	if err != nil {
		return inputs, err
	}
	inputs.Inventory = report
	role, hasRole := ResolveRoleDefinition(inputs.Requirement.RoleKey, inputs.RoleDefinitions)
	inputs.Candidates = candidatesFromInventory(inputs.Requirement, report, role, hasRole)
	return inputs, nil
}

type RejectionReason struct {
	Code              RejectionCode              `json:"code"`
	ErrorCode         taskrequirements.ErrorCode `json:"error_code,omitempty"`
	Message           string                     `json:"message,omitempty"`
	EvidenceRecordIDs []string                   `json:"evidence_record_ids"`
	SnapshotIDs       []string                   `json:"snapshot_ids"`
	PolicyVersion     string                     `json:"policy_version"`
}

type RejectedCandidate struct {
	Candidate Candidate         `json:"candidate"`
	Reasons   []RejectionReason `json:"reasons"`
}

type Result struct {
	Eligible []Candidate         `json:"eligible_candidates"`
	Rejected []RejectedCandidate `json:"rejected_candidates"`
}

func FilterHardEligibility(inputs Inputs) Result {
	policy := normalizePolicy(inputs.Policy)
	requirement := inputs.Requirement
	roleDef, hasRoleDef := ResolveRoleDefinition(requirement.RoleKey, inputs.RoleDefinitions)
	var roleErr error
	if hasRoleDef {
		roleErr = ValidateRoleDefinition(roleDef)
	}
	if hasRoleDef && roleErr == nil {
		requirement, roleErr = ComposeRoleRequirement(requirement, roleDef)
	}
	candidates := normalizeCandidates(inputs.Candidates, requirement, inputs.Inventory, roleDef, hasRoleDef)
	contract := inputs.RuntimeContract
	if len(contract.Providers) == 0 && len(contract.Hosts) == 0 {
		contract = runtimecap.DefaultContract()
	}
	hostName := strings.TrimSpace(inputs.HostName)
	if hostName == "" {
		hostName = "generic-local"
	}

	authByID := mapAuthReadiness(inputs.Inventory.AuthReadiness)
	modelByID := mapModelCapabilities(inputs.Inventory.ModelCapabilities)
	installationByID := mapInstallations(inputs.Inventory.Installations)
	accountByID := mapAccounts(inputs.Inventory.AccountProfiles)
	quotaByID := mapQuotaSnapshots(inputs.Inventory.QuotaSnapshots)
	scoreByID, scoreByCandidate := mapAvailabilityScores(inputs.Availability)
	breakerByID := mapBreakers(inputs.CircuitBreakers)
	budgetByID := mapBudgets(inputs.Budgets)

	var eligible []Candidate
	var rejected []RejectedCandidate
	for _, candidate := range candidates {
		if len(candidate.BudgetPolicyIDs) == 0 {
			candidate.BudgetPolicyIDs = budgetIDsForCandidate(candidate, inputs.Budgets)
		}
		if len(candidate.CircuitBreakerIDs) == 0 {
			candidate.CircuitBreakerIDs = breakerIDsForCandidate(candidate, inputs.CircuitBreakers)
		}
		if candidate.AvailabilityScoreID == "" {
			if score, ok := scoreByCandidate[availabilityKey(candidate)]; ok {
				candidate.AvailabilityScoreID = score.AvailabilityScoreID
			}
		}
		candidate.CandidateFingerprint = candidateFingerprint(candidate)
		var reasons []RejectionReason
		model, hasModel := modelByID[candidate.ModelCapabilityID]
		auth, hasAuth := authByID[candidate.AuthReadinessID]
		installation, hasInstallation := installationByID[candidate.ProviderInstallationID]
		account, hasAccount := accountByID[candidate.AccountProfileID]

		reasons = append(reasons, evaluatePins(candidate, inputs.Pins)...)
		reasons = append(reasons, evaluateExclusions(candidate, inputs.Exclusions)...)
		if !hasRoleDef {
			reasons = append(reasons, reason(RejectRoleUnsupported, taskrequirements.ErrRoleUnsupportedCode, "role definition is missing", nil, nil))
		} else if roleErr != nil {
			reasons = append(reasons, reason(RejectRoleUnsupported, taskrequirements.ErrRoleUnsupportedCode, roleErr.Error(), []string{roleDef.RoleDefinitionID}, nil))
		}
		if !candidate.ScopeBounded && policy.RequireBoundedScope {
			reasons = append(reasons, reason(RejectScopeUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "candidate does not prove bounded task scope", nil, nil))
		}
		if hasInstallation {
			reasons = append(reasons, staleReason(installation.ProviderInstallationID, installation.FreshnessState, installation.Confidence, policy)...)
			if installation.InstallationState != providerinventory.InstallationInstalled || installation.UsableForInvocation != "yes" {
				reasons = append(reasons, reason(RejectAvailabilityHardIneligible, taskrequirements.ErrCapabilityUnsupportedCode, "provider installation is not usable for invocation", []string{installation.ProviderInstallationID}, nil))
			}
		} else {
			reasons = append(reasons, reason(RejectAvailabilityHardIneligible, taskrequirements.ErrCapabilityUnsupportedCode, "provider installation evidence is missing", []string{candidate.ProviderInstallationID}, nil))
		}
		if candidate.AccountProfileID != "" && hasAccount {
			reasons = append(reasons, staleReason(account.AccountProfileID, account.FreshnessState, account.Confidence, policy)...)
			if len(account.CollisionSet) > 0 {
				reasons = append(reasons, reason(RejectAccountProfileAmbiguous, taskrequirements.ErrCapabilityUnsupportedCode, "account profile has a collision set", append([]string{account.AccountProfileID}, account.CollisionSet...), nil))
			}
		}
		if hasAuth {
			reasons = append(reasons, staleReason(auth.AuthReadinessID, auth.FreshnessState, auth.Confidence, policy)...)
			if auth.ReadinessState != providerinventory.ReadinessReady {
				reasons = append(reasons, reason(RejectAuthNotReady, taskrequirements.ErrRequirementConfidenceInsufficientCode, "auth readiness is not ready", []string{auth.AuthReadinessID}, nil))
			}
		} else {
			reasons = append(reasons, reason(RejectAuthNotReady, taskrequirements.ErrRequirementConfidenceInsufficientCode, "auth readiness evidence is missing", []string{candidate.AuthReadinessID}, nil))
		}
		if hasModel {
			reasons = append(reasons, evaluateModel(requirement, candidate, model, policy, roleDef, hasRoleDef)...)
		} else {
			reasons = append(reasons, reason(RejectModelUnavailable, taskrequirements.ErrCapabilityUnsupportedCode, "model capability evidence is missing", []string{candidate.ModelCapabilityID}, nil))
		}
		reasons = append(reasons, evaluatePermissionAndSideEffects(requirement, candidate, roleDef, hasRoleDef)...)
		reasons = append(reasons, evaluateRuntimeCompatibility(contract, hostName, requirement, candidate, roleDef, hasRoleDef)...)
		reasons = append(reasons, evaluateQuota(candidate, quotaByID, policy)...)
		reasons = append(reasons, evaluateBudgets(candidate, budgetByID, policy)...)
		reasons = append(reasons, evaluateAvailability(candidate, scoreByID, scoreByCandidate, policy)...)
		reasons = append(reasons, evaluateBreakers(candidate, breakerByID, policy)...)
		reasons = append(reasons, evaluateNetwork(requirement, candidate)...)
		reasons = append(reasons, evaluateClassificationAndQuality(requirement, candidate)...)
		reasons = append(reasons, evaluateRiskAndVerification(requirement, inputs.WorkerRoute, candidate, policy)...)

		if len(inputs.Pins) > 0 && matchesAnyPin(candidate, inputs.Pins) && len(rootPolicyReasons(reasons)) > 0 {
			reasons = append(reasons, reason(RejectPinnedCandidateIneligible, taskrequirements.ErrPinnedCandidateIneligibleCode, "pinned candidate violates deterministic policy", pinIDs(inputs.Pins), nil))
		}
		reasons = normalizeReasons(reasons)
		if len(reasons) == 0 {
			eligible = append(eligible, candidate)
			continue
		}
		rejected = append(rejected, RejectedCandidate{Candidate: candidate, Reasons: reasons})
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].RoutingCandidateID < eligible[j].RoutingCandidateID })
	sort.Slice(rejected, func(i, j int) bool {
		return rejected[i].Candidate.RoutingCandidateID < rejected[j].Candidate.RoutingCandidateID
	})
	return Result{Eligible: eligible, Rejected: rejected}
}

func normalizePolicy(policy Policy) Policy {
	if policy.EvidencePolicy == "" {
		policy.EvidencePolicy = EvidenceRejectUnknownStale
	}
	if policy.VerifierIndependence == "" {
		policy.VerifierIndependence = taskrequirements.IndependenceNone
	}
	return policy
}

func normalizeCandidates(candidates []Candidate, requirement taskrequirements.TaskRequirement, inventory providerinventory.Report, role RoleDefinition, hasRole bool) []Candidate {
	if len(candidates) == 0 {
		candidates = candidatesFromInventory(requirement, inventory, role, hasRole)
	}
	out := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.SchemaVersion = CandidateSchema
		if candidate.TaskID == "" {
			candidate.TaskID = requirement.TaskID
		}
		if candidate.RoleKey == "" {
			candidate.RoleKey = requirement.RoleKey
		}
		if candidate.InvocationProfileKey == "" {
			candidate.InvocationProfileKey = "default"
		}
		if candidate.Permission == "" {
			candidate.Permission = defaultPermissionForRequirement(requirement, role, hasRole)
		}
		if candidate.LaunchSideEffectClass == "" {
			candidate.LaunchSideEffectClass = defaultSideEffectForRequirement(requirement)
		}
		if candidate.NetworkPermission == "" {
			candidate.NetworkPermission = providerinventory.NetworkNotNeeded
		}
		if hasRole {
			if role.MinimumContextWindowTokens > candidate.RequiredContextTokens {
				candidate.RequiredContextTokens = role.MinimumContextWindowTokens
			}
			candidate.RequiredTools = dedupeStrings(append(candidate.RequiredTools, role.RequiredTools...))
		}
		candidate.QuotaSnapshotIDs = dedupeStrings(candidate.QuotaSnapshotIDs)
		candidate.BudgetPolicyIDs = dedupeStrings(candidate.BudgetPolicyIDs)
		candidate.CircuitBreakerIDs = dedupeStrings(candidate.CircuitBreakerIDs)
		if candidate.RoutingCandidateID == "" {
			candidate.RoutingCandidateID = candidateID(candidate)
		}
		candidate.CandidateFingerprint = candidateFingerprint(candidate)
		out = append(out, candidate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoutingCandidateID < out[j].RoutingCandidateID })
	return out
}

func candidatesFromInventory(requirement taskrequirements.TaskRequirement, inventory providerinventory.Report, role RoleDefinition, hasRole bool) []Candidate {
	installations := inventory.Installations
	sort.Slice(installations, func(i, j int) bool {
		return installations[i].ProviderInstallationID < installations[j].ProviderInstallationID
	})
	accountsByAdapter := map[string][]providerinventory.AccountProfile{}
	for _, account := range inventory.AccountProfiles {
		accountsByAdapter[account.AdapterID] = append(accountsByAdapter[account.AdapterID], account)
	}
	for adapter := range accountsByAdapter {
		sort.Slice(accountsByAdapter[adapter], func(i, j int) bool {
			return accountsByAdapter[adapter][i].AccountProfileID < accountsByAdapter[adapter][j].AccountProfileID
		})
	}
	authByAdapterAccount := map[string]providerinventory.AuthReadiness{}
	for _, auth := range inventory.AuthReadiness {
		key := auth.AdapterID + "\x00" + ptrValue(auth.AccountProfileID)
		if existing, ok := authByAdapterAccount[key]; !ok || auth.CapturedAt > existing.CapturedAt || (auth.CapturedAt == existing.CapturedAt && auth.AuthReadinessID > existing.AuthReadinessID) {
			authByAdapterAccount[key] = auth
		}
	}
	var out []Candidate
	for _, model := range inventory.ModelCapabilities {
		for _, installation := range installations {
			if installation.AdapterID != model.AdapterID {
				continue
			}
			accounts := accountsByAdapter[model.AdapterID]
			if len(accounts) == 0 {
				accounts = []providerinventory.AccountProfile{{AdapterID: model.AdapterID}}
			}
			for _, account := range accounts {
				accountID := strings.TrimSpace(account.AccountProfileID)
				auth := authByAdapterAccount[model.AdapterID+"\x00"+accountID]
				candidate := Candidate{
					TaskID:                 requirement.TaskID,
					RoleKey:                requirement.RoleKey,
					AdapterID:              model.AdapterID,
					ProviderInstallationID: installation.ProviderInstallationID,
					AccountProfileID:       accountID,
					AuthReadinessID:        auth.AuthReadinessID,
					ModelCapabilityID:      model.ModelCapabilityID,
					CanonicalModelID:       model.CanonicalModelID,
					InvocationProfileKey:   "default",
					Permission:             defaultPermissionForRequirement(requirement, role, hasRole),
					LaunchSideEffectClass:  defaultSideEffectForRequirement(requirement),
					NetworkPermission:      providerinventory.NetworkNotNeeded,
					ScopeBounded:           true,
					QualityFloor:           taskrequirements.QualityStandard,
					RequiredContextTokens:  0,
					CapabilitySummary:      capabilitySummary(model),
				}
				if hasRole {
					candidate.RequiredContextTokens = role.MinimumContextWindowTokens
					candidate.RequiredTools = append([]string(nil), role.RequiredTools...)
					candidate.QualityFloor = role.QualityFloor
				}
				candidate.QuotaSnapshotIDs = quotaIDsForCandidate(candidate, inventory.QuotaSnapshots)
				out = append(out, candidate)
			}
		}
	}
	return out
}

func defaultSideEffectForRequirement(requirement taskrequirements.TaskRequirement) taskrequirements.SideEffectClass {
	if requirement.SideEffectClass != "" {
		return requirement.SideEffectClass
	}
	return taskrequirements.SideEffectProviderLaunch
}

func evaluateModel(requirement taskrequirements.TaskRequirement, candidate Candidate, model providerinventory.ModelCapability, policy Policy, role RoleDefinition, hasRole bool) []RejectionReason {
	var reasons []RejectionReason
	reasons = append(reasons, staleReason(model.ModelCapabilityID, model.FreshnessState, model.Confidence, policy)...)
	if model.LifecycleState == providerinventory.LifecycleRemoved || model.AvailabilityState != providerinventory.AvailabilityAvailable {
		reasons = append(reasons, reason(RejectModelUnavailable, taskrequirements.ErrCapabilityUnsupportedCode, "model is not available", []string{model.ModelCapabilityID}, nil))
	}
	if hasRole && len(role.MinimumCapabilities) == 0 {
		reasons = append(reasons, reason(RejectRoleUnsupported, taskrequirements.ErrRoleUnsupportedCode, "role definition has no minimum capability evidence floor", []string{role.RoleDefinitionID}, nil))
	}
	for _, req := range requirement.RequiredCapabilities {
		reasons = append(reasons, evaluateCapability(req, candidate, model, policy)...)
	}
	if requirement.CancellationRequired && model.Cancellation != providerinventory.CapabilityTrue {
		reasons = append(reasons, reason(RejectCancellationUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "required cancellation is unsupported", []string{model.ModelCapabilityID}, nil))
	}
	if candidate.RequiredContextTokens > 0 {
		required := candidate.RequiredContextTokens + policy.ContextReserveTokens
		if model.ContextWindowTokens == nil || model.ContextWindowTokens.Value < required || !confidenceAllowed(model.ContextWindowTokens.Confidence, policy) {
			reasons = append(reasons, reason(RejectContextWindowInsufficient, taskrequirements.ErrCapabilityUnsupportedCode, "context window cannot satisfy required reserve", []string{model.ModelCapabilityID}, nil))
		}
	}
	if len(candidate.RequiredTools) > 0 && !toolsSupported(candidate.RequiredTools, model.ToolSupport, policy) {
		reasons = append(reasons, reason(RejectToolSupportUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "required tool support is missing", []string{model.ModelCapabilityID}, nil))
	}
	return reasons
}

func evaluateCapability(req taskrequirements.CapabilityRequirement, candidate Candidate, model providerinventory.ModelCapability, policy Policy) []RejectionReason {
	evidence := []string{model.ModelCapabilityID}
	if req.FreshnessRequired == providerinventory.FreshnessFresh && model.FreshnessState != providerinventory.FreshnessFresh {
		return []RejectionReason{reason(RejectEvidenceStale, taskrequirements.ErrRequirementConfidenceInsufficientCode, "capability evidence is not fresh", evidence, nil)}
	}
	if req.MinimumConfidence != "" && !meetsMinimumConfidence(model.Confidence, req.MinimumConfidence, policy) {
		return []RejectionReason{reason(RejectUnknownTelemetry, taskrequirements.ErrRequirementConfidenceInsufficientCode, "capability confidence is insufficient", evidence, nil)}
	}
	switch req.Dimension {
	case taskrequirements.CapabilityRolesSupported:
		roles, ok := stringListRequirement(req.RequiredValue)
		if !ok || len(roles) == 0 {
			return []RejectionReason{reason(RejectUnknownRecordVersion, taskrequirements.ErrInvalidRecordCode, "roles_supported capability requirement is malformed", evidence, nil)}
		}
		for _, want := range roles {
			if !roleSupported(want, model.RolesSupported) {
				return []RejectionReason{reason(RejectRoleUnsupported, taskrequirements.ErrRoleUnsupportedCode, "model does not support role capability "+want, evidence, nil)}
			}
		}
		return nil
	case taskrequirements.CapabilityContextWindowTokens:
		required, ok := numericRequirementValue(req.RequiredValue)
		if !ok || required < 0 {
			return []RejectionReason{reason(RejectUnknownRecordVersion, taskrequirements.ErrInvalidRecordCode, "context_window_tokens capability requirement is malformed", evidence, nil)}
		}
		if required > 0 && (model.ContextWindowTokens == nil || model.ContextWindowTokens.Value < required || !meetsMinimumConfidence(model.ContextWindowTokens.Confidence, req.MinimumConfidence, policy)) {
			return []RejectionReason{reason(RejectContextWindowInsufficient, taskrequirements.ErrCapabilityUnsupportedCode, "context window capability is insufficient", evidence, nil)}
		}
		return nil
	case taskrequirements.CapabilityToolSupport:
		tools, ok := stringListRequirement(req.RequiredValue)
		if !ok || len(tools) == 0 {
			return []RejectionReason{reason(RejectUnknownRecordVersion, taskrequirements.ErrInvalidRecordCode, "tool_support capability requirement is malformed", evidence, nil)}
		}
		if !toolsSupportedWithMinimum(tools, model.ToolSupport, req.MinimumConfidence, policy) {
			return []RejectionReason{reason(RejectToolSupportUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "tool support is unsupported", evidence, nil)}
		}
		return nil
	}
	requiredBool, boolRequired := req.RequiredValue.(bool)
	if !boolRequired || !requiredBool {
		return []RejectionReason{reason(RejectUnknownRecordVersion, taskrequirements.ErrInvalidRecordCode, "boolean capability requirement is malformed", evidence, nil)}
	}
	switch req.Dimension {
	case taskrequirements.CapabilityReadOnly:
		if model.ReadOnly != providerinventory.CapabilityTrue {
			return []RejectionReason{reason(RejectReadOnlyUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "read-only capability is unsupported", evidence, nil)}
		}
	case taskrequirements.CapabilityJSONOutput:
		if model.JSONOutput != providerinventory.CapabilityTrue {
			return []RejectionReason{reason(RejectJSONOutputUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "JSON output capability is unsupported", evidence, nil)}
		}
	case taskrequirements.CapabilityNestedSubagents:
		if model.NestedSubagents != providerinventory.CapabilityTrue {
			return []RejectionReason{reason(RejectNestedSubagentsUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "nested sub-agent capability is unsupported", evidence, nil)}
		}
	case taskrequirements.CapabilityMCPConfig:
		if model.MCPConfig != providerinventory.CapabilityTrue {
			return []RejectionReason{reason(RejectMCPConfigUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "MCP config capability is unsupported", evidence, nil)}
		}
	case taskrequirements.CapabilityCancellation:
		if model.Cancellation != providerinventory.CapabilityTrue {
			return []RejectionReason{reason(RejectCancellationUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "cancellation capability is unsupported", evidence, nil)}
		}
	case taskrequirements.CapabilityTokenUsageReporting:
		if model.TokenUsageReporting != providerinventory.CapabilityTrue {
			return []RejectionReason{reason(RejectUsageReportingUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "token usage reporting is unsupported", evidence, nil)}
		}
	case taskrequirements.CapabilityImageInput:
		if model.ImageInput != providerinventory.CapabilityTrue {
			return []RejectionReason{reason(RejectImageInputUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "image input is unsupported", evidence, nil)}
		}
	case taskrequirements.CapabilityImageOutput:
		if model.ImageOutput != providerinventory.CapabilityTrue {
			return []RejectionReason{reason(RejectImageOutputUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "image output is unsupported", evidence, nil)}
		}
	default:
		return []RejectionReason{reason(RejectUnknownRecordVersion, taskrequirements.ErrInvalidRecordCode, "unknown capability dimension "+string(req.Dimension), evidence, nil)}
	}
	return nil
}

func evaluatePermissionAndSideEffects(requirement taskrequirements.TaskRequirement, candidate Candidate, role RoleDefinition, hasRole bool) []RejectionReason {
	var reasons []RejectionReason
	if permissionRank(candidate.Permission) < permissionRank(requirement.PermissionRequired) {
		reasons = append(reasons, reason(RejectPermissionUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "candidate cannot enforce required permission", nil, nil))
	}
	if hasRole && permissionRank(candidate.Permission) > permissionRank(role.PermissionCeiling) {
		reasons = append(reasons, reason(RejectPermissionUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "candidate permission exceeds role envelope ceiling", []string{role.RoleDefinitionID}, nil))
	}
	if normalizeRoleKey(requirement.RoleKey) == RoleKeyNestedSubagent && permissionRank(candidate.Permission) > permissionRank(requirement.PermissionRequired) {
		reasons = append(reasons, reason(RejectPermissionUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "nested sub-agent permission exceeds parent-delegated task permission", nil, nil))
	}
	if sideEffectRank(candidate.LaunchSideEffectClass) > sideEffectRank(requirement.SideEffectClass) && requirement.SideEffectClass != "" {
		reasons = append(reasons, reason(RejectSideEffectClassExceeded, taskrequirements.ErrCapabilityUnsupportedCode, "candidate side-effect class exceeds task authorization", nil, nil))
	}
	if hasRole && role.MaxSideEffectClass != "" && sideEffectRank(candidate.LaunchSideEffectClass) > sideEffectRank(role.MaxSideEffectClass) {
		reasons = append(reasons, reason(RejectSideEffectClassExceeded, taskrequirements.ErrCapabilityUnsupportedCode, "candidate side-effect class exceeds role envelope", []string{role.RoleDefinitionID}, nil))
	}
	return reasons
}

func evaluateRuntimeCompatibility(contract runtimecap.Contract, hostName string, requirement taskrequirements.TaskRequirement, candidate Candidate, role RoleDefinition, hasRole bool) []RejectionReason {
	runtimeRole := runtimeRoleForRequirement(requirement, role, hasRole)
	entry := contract.EvaluateCompatibility(candidate.AdapterID, hostName, runtimeRole)
	if entry.Support == runtimecap.SupportUnsupported {
		code := RejectRoleUnsupported
		if entry.Code == "unsupported_json_output" {
			code = RejectJSONOutputUnsupported
		}
		if entry.Code == "unsupported_cancellation" {
			code = RejectCancellationUnsupported
		}
		if entry.Code == "unsupported_read_only_mode" {
			code = RejectReadOnlyUnsupported
		}
		return []RejectionReason{reason(code, taskrequirements.ErrCapabilityUnsupportedCode, "provider is incompatible with host for role: "+entry.Code, nil, nil)}
	}
	return nil
}

func evaluateQuota(candidate Candidate, quotaByID map[string]providerinventory.QuotaSnapshot, policy Policy) []RejectionReason {
	var reasons []RejectionReason
	var sawFreshExact bool
	var sawFreshEstimate bool
	for _, id := range candidate.QuotaSnapshotIDs {
		snapshot, ok := quotaByID[id]
		if !ok {
			reasons = append(reasons, reason(RejectQuotaConfidenceInsufficient, taskrequirements.ErrRequirementConfidenceInsufficientCode, "quota snapshot is missing", nil, []string{id}))
			continue
		}
		reasons = append(reasons, staleReason(snapshot.QuotaSnapshotID, snapshot.FreshnessState, snapshot.Confidence, policy)...)
		if len(snapshot.ConflictSet) > 0 {
			reasons = append(reasons, reason(RejectQuotaConfidenceInsufficient, taskrequirements.ErrRequirementConfidenceInsufficientCode, "quota snapshots conflict", snapshot.ConflictSet, []string{snapshot.QuotaSnapshotID}))
		}
		if snapshot.Confidence == providerinventory.ConfidenceExact && snapshot.FreshnessState == providerinventory.FreshnessFresh {
			sawFreshExact = true
		}
		if snapshot.Confidence == providerinventory.ConfidenceEstimated && snapshot.FreshnessState == providerinventory.FreshnessFresh {
			sawFreshEstimate = true
		}
		if snapshot.RemainingValue != nil && *snapshot.RemainingValue <= 0 {
			reasons = append(reasons, reason(RejectQuotaExhausted, taskrequirements.ErrRequirementConfidenceInsufficientCode, "quota reports no remaining capacity", nil, []string{snapshot.QuotaSnapshotID}))
		}
	}
	if policy.RequireExactQuota && !sawFreshExact {
		reasons = append(reasons, reason(RejectQuotaConfidenceInsufficient, taskrequirements.ErrRequirementConfidenceInsufficientCode, "fresh exact quota evidence is required", nil, candidate.QuotaSnapshotIDs))
	}
	if !policy.RequireExactQuota && len(candidate.QuotaSnapshotIDs) > 0 && !sawFreshExact && !sawFreshEstimate && policy.EvidencePolicy == EvidenceRejectUnknownStale {
		reasons = append(reasons, reason(RejectQuotaConfidenceInsufficient, taskrequirements.ErrRequirementConfidenceInsufficientCode, "quota confidence is insufficient", nil, candidate.QuotaSnapshotIDs))
	}
	return reasons
}

func evaluateBudgets(candidate Candidate, budgetByID map[string]budget.Summary, policy Policy) []RejectionReason {
	if len(candidate.BudgetPolicyIDs) == 0 {
		if policy.RequireBudgetEvidence {
			return []RejectionReason{reason(RejectBudgetExhausted, taskrequirements.ErrRequirementConfidenceInsufficientCode, "budget evidence is required", nil, nil)}
		}
		return nil
	}
	var reasons []RejectionReason
	for _, id := range candidate.BudgetPolicyIDs {
		summary, ok := budgetByID[id]
		if !ok {
			reasons = append(reasons, reason(RejectBudgetExhausted, taskrequirements.ErrRequirementConfidenceInsufficientCode, "budget summary is missing", []string{id}, nil))
			continue
		}
		if summary.PolicyMode == budget.PolicyHard && (strings.TrimSpace(summary.Denial) != "" || summary.AvailableValue <= 0) {
			reasons = append(reasons, reason(RejectBudgetExhausted, taskrequirements.ErrRequirementConfidenceInsufficientCode, "hard budget has no available capacity", []string{id}, nil))
		}
		if !confidenceAllowed(summary.Confidence, policy) {
			reasons = append(reasons, reason(RejectUnknownTelemetry, taskrequirements.ErrRequirementConfidenceInsufficientCode, "budget confidence is insufficient", []string{id}, nil))
		}
	}
	return reasons
}

func evaluateAvailability(candidate Candidate, scoreByID map[string]availability.Score, scoreByCandidate map[string]availability.Score, policy Policy) []RejectionReason {
	score, ok := scoreByID[candidate.AvailabilityScoreID]
	if !ok {
		score, ok = scoreByCandidate[availabilityKey(candidate)]
	}
	if !ok {
		if policy.RequireAvailabilityEvidence {
			return []RejectionReason{reason(RejectAvailabilityHardIneligible, taskrequirements.ErrRequirementConfidenceInsufficientCode, "availability score is required", []string{candidate.AvailabilityScoreID}, nil)}
		}
		return nil
	}
	if len(score.HardIneligibleReasons) == 0 {
		return nil
	}
	evidence := append([]string{score.AvailabilityScoreID}, score.EvidenceRecordIDs...)
	return []RejectionReason{reason(RejectAvailabilityHardIneligible, taskrequirements.ErrCapabilityUnsupportedCode, "availability has hard ineligible reasons: "+joinAvailabilityReasons(score.HardIneligibleReasons), evidence, score.QuotaSnapshotIDs)}
}

func evaluateBreakers(candidate Candidate, breakerByID map[string]availability.CircuitBreaker, policy Policy) []RejectionReason {
	var reasons []RejectionReason
	for _, id := range candidate.CircuitBreakerIDs {
		breaker, ok := breakerByID[id]
		if !ok {
			continue
		}
		if breaker.State == availability.BreakerOpen || (breaker.State == availability.BreakerHalfOpen && !policy.AllowHalfOpenBreakerProbe) {
			reasons = append(reasons, reason(RejectBreakerOpen, taskrequirements.ErrRequirementConfidenceInsufficientCode, "circuit breaker blocks candidate", []string{id}, nil))
		}
	}
	return reasons
}

func evaluateNetwork(requirement taskrequirements.TaskRequirement, candidate Candidate) []RejectionReason {
	switch requirement.NetworkRequired {
	case taskrequirements.NetworkRequired:
		if candidate.NetworkPermission != providerinventory.NetworkGranted {
			return []RejectionReason{reason(RejectNetworkPermissionMissing, taskrequirements.ErrCapabilityUnsupportedCode, "network permission is required", nil, nil)}
		}
	case taskrequirements.NetworkAllowedIfProfileGrants:
		if candidate.NetworkPermission == providerinventory.NetworkDenied {
			return []RejectionReason{reason(RejectNetworkPermissionMissing, taskrequirements.ErrCapabilityUnsupportedCode, "network permission is denied", nil, nil)}
		}
	}
	return nil
}

func evaluateClassificationAndQuality(requirement taskrequirements.TaskRequirement, candidate Candidate) []RejectionReason {
	var reasons []RejectionReason
	if strings.TrimSpace(requirement.DataClassification) == "secret-material" {
		reasons = append(reasons, reason(RejectDataClassificationUnsupported, taskrequirements.ErrCapabilityUnsupportedCode, "secret material cannot be routed to a provider candidate", nil, nil))
	}
	if qualityRank(candidate.QualityFloor) < qualityRank(requirement.QualityFloor) {
		reasons = append(reasons, reason(RejectQualityFloorInsufficient, taskrequirements.ErrCapabilityUnsupportedCode, "candidate quality floor is below requirement", nil, nil))
	}
	return reasons
}

func evaluateRiskAndVerification(requirement taskrequirements.TaskRequirement, worker *Candidate, candidate Candidate, policy Policy) []RejectionReason {
	var reasons []RejectionReason
	if requirement.RiskTier == taskrequirements.RiskCritical {
		reasons = append(reasons, reason(RejectRiskTierUnsupported, taskrequirements.ErrRequirementConfidenceInsufficientCode, "critical risk requires explicit approval outside automatic routing", nil, nil))
	}
	required := policy.VerifierIndependence
	for _, verification := range requirement.VerificationRequirements {
		if riskApplies(requirement.RiskTier, verification.RequiredForRiskTiers) && independenceRank(verification.IndependenceLevel) > independenceRank(required) {
			required = verification.IndependenceLevel
		}
	}
	if worker == nil || required == taskrequirements.IndependenceNone {
		return reasons
	}
	if !independentEnough(*worker, candidate, required) {
		reasons = append(reasons, reason(RejectVerifierIndependenceInsufficient, taskrequirements.ErrVerifierIndependenceRequiredCode, "verifier is not independent from worker route", []string{worker.RoutingCandidateID}, nil))
	}
	return reasons
}

func evaluatePins(candidate Candidate, pins []Pin) []RejectionReason {
	if len(pins) == 0 || matchesAnyPin(candidate, pins) {
		return nil
	}
	return []RejectionReason{reason(RejectPinnedCandidateNotMatched, taskrequirements.ErrPinnedCandidateIneligibleCode, "candidate does not match user pin", pinIDs(pins), nil)}
}

func evaluateExclusions(candidate Candidate, exclusions []Exclusion) []RejectionReason {
	var reasons []RejectionReason
	for _, exclusion := range exclusions {
		if matchesSelector(candidate, exclusion.AdapterID, exclusion.ProviderInstallationID, exclusion.AccountProfileID, exclusion.ModelCapabilityID, exclusion.InvocationProfileKey) {
			reasons = append(reasons, reason(RejectCandidateExcluded, taskrequirements.ErrNoEligibleCandidateCode, "candidate matches exclusion", []string{exclusion.ExclusionID}, nil))
		}
	}
	return reasons
}

func staleReason(recordID string, freshness providerinventory.FreshnessState, confidence providerinventory.Confidence, policy Policy) []RejectionReason {
	if freshness == providerinventory.FreshnessStale || freshness == providerinventory.FreshnessExpired || confidence == providerinventory.ConfidenceStale {
		return []RejectionReason{reason(RejectEvidenceStale, taskrequirements.ErrRequirementConfidenceInsufficientCode, "hard evidence is stale under policy "+string(policy.EvidencePolicy), []string{recordID}, nil)}
	}
	if !confidenceAllowed(confidence, policy) {
		return []RejectionReason{reason(RejectUnknownTelemetry, taskrequirements.ErrRequirementConfidenceInsufficientCode, "hard evidence confidence is unknown under policy "+string(policy.EvidencePolicy), []string{recordID}, nil)}
	}
	return nil
}

func confidenceAllowed(confidence providerinventory.Confidence, policy Policy) bool {
	switch confidence {
	case providerinventory.ConfidenceExact:
		return true
	case providerinventory.ConfidenceEstimated:
		return policy.EvidencePolicy == EvidenceAllowEstimated
	default:
		return policy.EvidencePolicy == EvidenceAllowEstimated && confidence == providerinventory.ConfidenceEstimated
	}
}

func meetsMinimumConfidence(got, want providerinventory.Confidence, policy Policy) bool {
	if !confidenceAllowed(got, policy) {
		return false
	}
	if want == "" {
		return true
	}
	return confidenceRank(got) >= confidenceRank(want)
}

func confidenceRank(value providerinventory.Confidence) int {
	switch value {
	case providerinventory.ConfidenceExact:
		return 4
	case providerinventory.ConfidenceEstimated:
		return 3
	default:
		return 0
	}
}

func roleSupported(role string, roles []providerinventory.CatalogRole) bool {
	want := providerinventory.CatalogRole(role)
	if role == RoleKeyNestedSubagent {
		want = providerinventory.CatalogRoleNestedSubagents
	}
	for _, have := range roles {
		if have == want || (role == RoleKeyVerifier && have == providerinventory.CatalogRoleAuditReview) {
			return true
		}
	}
	return false
}

func runtimeRoleForRequirement(requirement taskrequirements.TaskRequirement, role RoleDefinition, hasRole bool) runtimecap.CompatibilityRole {
	if hasRole {
		for _, capability := range role.MinimumCapabilities {
			if capability.Dimension != taskrequirements.CapabilityRolesSupported {
				continue
			}
			for _, required := range requiredRoles(capability.RequiredValue) {
				switch required {
				case string(providerinventory.CatalogRoleNestedSubagents), RoleKeyNestedSubagent:
					return runtimecap.RoleNestedSubagents
				case string(providerinventory.CatalogRoleVerifier), string(providerinventory.CatalogRoleAuditReview), RoleKeySoul:
					return runtimecap.RoleVerifier
				}
			}
		}
	}
	switch normalizeRoleKey(requirement.RoleKey) {
	case RoleKeyVerifier, RoleKeySoul:
		return runtimecap.RoleVerifier
	case RoleKeyNestedSubagent:
		return runtimecap.RoleNestedSubagents
	default:
		return runtimecap.RoleWorker
	}
}

func requiredRoles(value any) []string {
	values, ok := stringListRequirement(value)
	if ok {
		return values
	}
	return nil
}

func stringListRequirement(value any) ([]string, bool) {
	switch v := value.(type) {
	case string:
		values := dedupeStrings([]string{v})
		return values, len(values) > 0
	case []string:
		values := dedupeStrings(v)
		return values, len(values) > 0
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		values := dedupeStrings(out)
		return values, len(values) > 0
	default:
		return nil, false
	}
}

func numericRequirementValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		if v > int64(^uint(0)>>1) {
			return int(^uint(0) >> 1), true
		}
		return int(v), true
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

func toolsSupported(required []string, facts []providerinventory.CapabilityFact, policy Policy) bool {
	return toolsSupportedWithMinimum(required, facts, "", policy)
}

func toolsSupportedWithMinimum(required []string, facts []providerinventory.CapabilityFact, minimum providerinventory.Confidence, policy Policy) bool {
	have := map[string]bool{}
	for _, fact := range facts {
		if minimum != "" {
			if !meetsMinimumConfidence(fact.Confidence, minimum, policy) {
				continue
			}
		} else if !confidenceAllowed(fact.Confidence, policy) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(fact.Value), "true") || strings.EqualFold(strings.TrimSpace(fact.Value), "supported") {
			have[strings.TrimSpace(fact.Name)] = true
		}
	}
	for _, tool := range required {
		if !have[strings.TrimSpace(tool)] {
			return false
		}
	}
	return true
}

func matchesAnyPin(candidate Candidate, pins []Pin) bool {
	for _, pin := range pins {
		if matchesSelector(candidate, pin.AdapterID, pin.ProviderInstallationID, pin.AccountProfileID, pin.ModelCapabilityID, pin.InvocationProfileKey) {
			return true
		}
	}
	return len(pins) == 0
}

func matchesSelector(candidate Candidate, adapterID, installationID, accountID, modelID, invocationProfileKey string) bool {
	if strings.TrimSpace(adapterID) != "" && candidate.AdapterID != strings.TrimSpace(adapterID) {
		return false
	}
	if strings.TrimSpace(installationID) != "" && candidate.ProviderInstallationID != strings.TrimSpace(installationID) {
		return false
	}
	if strings.TrimSpace(accountID) != "" && candidate.AccountProfileID != strings.TrimSpace(accountID) {
		return false
	}
	if strings.TrimSpace(modelID) != "" && candidate.ModelCapabilityID != strings.TrimSpace(modelID) {
		return false
	}
	if strings.TrimSpace(invocationProfileKey) != "" && candidate.InvocationProfileKey != strings.TrimSpace(invocationProfileKey) {
		return false
	}
	return true
}

func independentEnough(worker, verifier Candidate, required taskrequirements.IndependenceLevel) bool {
	switch required {
	case taskrequirements.IndependenceDifferentProvider:
		return worker.AdapterID != verifier.AdapterID
	case taskrequirements.IndependenceDifferentAccount:
		return worker.AdapterID != verifier.AdapterID || worker.AccountProfileID != verifier.AccountProfileID
	case taskrequirements.IndependenceDifferentModel:
		return worker.ModelCapabilityID != verifier.ModelCapabilityID
	case taskrequirements.IndependenceHuman:
		return false
	default:
		return true
	}
}

func riskApplies(risk taskrequirements.RiskTier, risks []taskrequirements.RiskTier) bool {
	for _, candidate := range risks {
		if candidate == risk {
			return true
		}
	}
	return false
}

func rootPolicyReasons(reasons []RejectionReason) []RejectionReason {
	var out []RejectionReason
	for _, r := range reasons {
		if r.Code != RejectPinnedCandidateNotMatched && r.Code != RejectPinnedCandidateIneligible {
			out = append(out, r)
		}
	}
	return out
}

func reason(code RejectionCode, errorCode taskrequirements.ErrorCode, message string, evidence, snapshots []string) RejectionReason {
	return RejectionReason{
		Code:              code,
		ErrorCode:         errorCode,
		Message:           message,
		EvidenceRecordIDs: dedupeStrings(evidence),
		SnapshotIDs:       dedupeStrings(snapshots),
		PolicyVersion:     PolicyVersion,
	}
}

func normalizeReasons(reasons []RejectionReason) []RejectionReason {
	seen := map[string]RejectionReason{}
	for _, r := range reasons {
		if r.Code == "" {
			continue
		}
		r.EvidenceRecordIDs = dedupeStrings(r.EvidenceRecordIDs)
		r.SnapshotIDs = dedupeStrings(r.SnapshotIDs)
		key := string(r.Code) + "\x00" + string(r.ErrorCode) + "\x00" + strings.Join(r.EvidenceRecordIDs, ",") + "\x00" + strings.Join(r.SnapshotIDs, ",")
		if existing, ok := seen[key]; ok {
			if existing.Message == "" {
				existing.Message = r.Message
			}
			seen[key] = existing
			continue
		}
		seen[key] = r
	}
	out := make([]RejectionReason, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		if strings.Join(out[i].EvidenceRecordIDs, ",") != strings.Join(out[j].EvidenceRecordIDs, ",") {
			return strings.Join(out[i].EvidenceRecordIDs, ",") < strings.Join(out[j].EvidenceRecordIDs, ",")
		}
		return strings.Join(out[i].SnapshotIDs, ",") < strings.Join(out[j].SnapshotIDs, ",")
	})
	return out
}

func mapAuthReadiness(values []providerinventory.AuthReadiness) map[string]providerinventory.AuthReadiness {
	out := map[string]providerinventory.AuthReadiness{}
	for _, value := range values {
		out[value.AuthReadinessID] = value
	}
	return out
}

func mapModelCapabilities(values []providerinventory.ModelCapability) map[string]providerinventory.ModelCapability {
	out := map[string]providerinventory.ModelCapability{}
	for _, value := range values {
		out[value.ModelCapabilityID] = value
	}
	return out
}

func mapInstallations(values []providerinventory.ProviderInstallation) map[string]providerinventory.ProviderInstallation {
	out := map[string]providerinventory.ProviderInstallation{}
	for _, value := range values {
		out[value.ProviderInstallationID] = value
	}
	return out
}

func mapAccounts(values []providerinventory.AccountProfile) map[string]providerinventory.AccountProfile {
	out := map[string]providerinventory.AccountProfile{}
	for _, value := range values {
		out[value.AccountProfileID] = value
	}
	return out
}

func mapQuotaSnapshots(values []providerinventory.QuotaSnapshot) map[string]providerinventory.QuotaSnapshot {
	out := map[string]providerinventory.QuotaSnapshot{}
	for _, value := range values {
		out[value.QuotaSnapshotID] = value
	}
	return out
}

func mapAvailabilityScores(values []availability.Score) (map[string]availability.Score, map[string]availability.Score) {
	byID := map[string]availability.Score{}
	byCandidate := map[string]availability.Score{}
	for _, value := range values {
		byID[value.AvailabilityScoreID] = value
		key := value.Scope.AdapterID + "\x00" + value.Scope.ProviderInstallationID + "\x00" + value.Scope.AccountProfileID + "\x00" + value.Scope.ModelCapabilityID
		byCandidate[key] = value
	}
	return byID, byCandidate
}

func mapBreakers(values []availability.CircuitBreaker) map[string]availability.CircuitBreaker {
	out := map[string]availability.CircuitBreaker{}
	for _, value := range values {
		out[value.CircuitBreakerID] = value
	}
	return out
}

func mapBudgets(values []budget.Summary) map[string]budget.Summary {
	out := map[string]budget.Summary{}
	for _, value := range values {
		out[value.BudgetPolicyID] = value
	}
	return out
}

func quotaIDsForCandidate(candidate Candidate, snapshots []providerinventory.QuotaSnapshot) []string {
	var ids []string
	for _, snapshot := range snapshots {
		if snapshot.AdapterID != candidate.AdapterID {
			continue
		}
		if ptrValue(snapshot.ProviderInstallationID) != "" && ptrValue(snapshot.ProviderInstallationID) != candidate.ProviderInstallationID {
			continue
		}
		if ptrValue(snapshot.AccountProfileID) != "" && ptrValue(snapshot.AccountProfileID) != candidate.AccountProfileID {
			continue
		}
		if ptrValue(snapshot.ModelCapabilityID) != "" && ptrValue(snapshot.ModelCapabilityID) != candidate.ModelCapabilityID {
			continue
		}
		ids = append(ids, snapshot.QuotaSnapshotID)
	}
	return dedupeStrings(ids)
}

func budgetIDsForCandidate(candidate Candidate, summaries []budget.Summary) []string {
	var ids []string
	for _, summary := range summaries {
		if !scopeMatchesCandidate(summary.Scope.AdapterID, candidate.AdapterID) {
			continue
		}
		if !scopeMatchesCandidate(summary.Scope.AccountProfileID, candidate.AccountProfileID) {
			continue
		}
		if !scopeMatchesCandidate(summary.Scope.ModelCapabilityID, candidate.ModelCapabilityID) {
			continue
		}
		ids = append(ids, summary.BudgetPolicyID)
	}
	return dedupeStrings(ids)
}

func breakerIDsForCandidate(candidate Candidate, breakers []availability.CircuitBreaker) []string {
	var ids []string
	for _, breaker := range breakers {
		if !scopeMatchesCandidate(breaker.Scope.AdapterID, candidate.AdapterID) {
			continue
		}
		if !scopeMatchesCandidate(breaker.Scope.ProviderInstallationID, candidate.ProviderInstallationID) {
			continue
		}
		if !scopeMatchesCandidate(breaker.Scope.AccountProfileID, candidate.AccountProfileID) {
			continue
		}
		if !scopeMatchesCandidate(breaker.Scope.ModelCapabilityID, candidate.ModelCapabilityID) {
			continue
		}
		ids = append(ids, breaker.CircuitBreakerID)
	}
	return dedupeStrings(ids)
}

func scopeMatchesCandidate(scopeValue, candidateValue string) bool {
	scopeValue = strings.TrimSpace(scopeValue)
	return scopeValue == "" || scopeValue == strings.TrimSpace(candidateValue)
}

func availabilityKey(candidate Candidate) string {
	return candidate.AdapterID + "\x00" + candidate.ProviderInstallationID + "\x00" + candidate.AccountProfileID + "\x00" + candidate.ModelCapabilityID
}

func capabilitySummary(model providerinventory.ModelCapability) map[string]any {
	return map[string]any{
		"roles_supported":       model.RolesSupported,
		"read_only":             model.ReadOnly,
		"json_output":           model.JSONOutput,
		"nested_subagents":      model.NestedSubagents,
		"mcp_config":            model.MCPConfig,
		"cancellation":          model.Cancellation,
		"token_usage_reporting": model.TokenUsageReporting,
		"image_input":           model.ImageInput,
		"image_output":          model.ImageOutput,
	}
}

func candidateID(candidate Candidate) string {
	return "rcand_" + hashHex(candidate.TaskID, candidate.AdapterID, candidate.ProviderInstallationID, candidate.AccountProfileID, candidate.ModelCapabilityID, candidate.InvocationProfileKey)
}

func candidateFingerprint(candidate Candidate) string {
	copy := candidate
	copy.CandidateFingerprint = ""
	return "sha256:" + hashCanonical(copy)
}

func hashHex(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashCanonical(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func defaultPermissionForRoleDefinition(roleKey string, role RoleDefinition, hasRole bool) taskrequirements.Permission {
	if hasRole && role.PermissionFloor != "" {
		return role.PermissionFloor
	}
	if normalizeRoleKey(roleKey) == RoleKeyVerifier || normalizeRoleKey(roleKey) == RoleKeySoul {
		return taskrequirements.PermissionReadOnly
	}
	return taskrequirements.PermissionWrite
}

func defaultPermissionForRequirement(requirement taskrequirements.TaskRequirement, role RoleDefinition, hasRole bool) taskrequirements.Permission {
	if requirement.PermissionRequired != "" {
		return requirement.PermissionRequired
	}
	return defaultPermissionForRoleDefinition(requirement.RoleKey, role, hasRole)
}

func permissionRank(permission taskrequirements.Permission) int {
	switch permission {
	case taskrequirements.PermissionReadOnly:
		return 1
	case taskrequirements.PermissionWrite:
		return 2
	case taskrequirements.PermissionOrchestrate:
		return 3
	default:
		return 0
	}
}

func sideEffectRank(sideEffect taskrequirements.SideEffectClass) int {
	switch sideEffect {
	case taskrequirements.SideEffectNone:
		return 0
	case taskrequirements.SideEffectLocalRead:
		return 1
	case taskrequirements.SideEffectLocalWrite:
		return 2
	case taskrequirements.SideEffectRepoWrite:
		return 3
	case taskrequirements.SideEffectProviderLaunch:
		return 4
	case taskrequirements.SideEffectGitRemoteWrite:
		return 5
	case taskrequirements.SideEffectGitHubWrite:
		return 6
	case taskrequirements.SideEffectExternalWrite:
		return 7
	default:
		return 100
	}
}

func independenceRank(level taskrequirements.IndependenceLevel) int {
	switch level {
	case taskrequirements.IndependenceDifferentModel:
		return 1
	case taskrequirements.IndependenceDifferentAccount:
		return 2
	case taskrequirements.IndependenceDifferentProvider:
		return 3
	case taskrequirements.IndependenceHuman:
		return 4
	default:
		return 0
	}
}

func qualityRank(quality taskrequirements.QualityFloor) int {
	switch quality {
	case "", taskrequirements.QualityStandard:
		return 1
	case taskrequirements.QualityStrong:
		return 2
	case taskrequirements.QualityAdversarial:
		return 3
	default:
		return 0
	}
}

func ptrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func pinIDs(pins []Pin) []string {
	ids := make([]string, 0, len(pins))
	for _, pin := range pins {
		ids = append(ids, pin.PinID)
	}
	return dedupeStrings(ids)
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func joinAvailabilityReasons(values []availability.ReasonCode) string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
