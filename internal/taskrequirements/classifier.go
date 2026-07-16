package taskrequirements

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

type Scope struct {
	Paths                   []string `json:"paths,omitempty"`
	Commands                []string `json:"commands,omitempty"`
	Issues                  []int    `json:"issues,omitempty"`
	PullRequests            []int    `json:"pull_requests,omitempty"`
	ExternalServices        []string `json:"external_services,omitempty"`
	Providers               []string `json:"providers,omitempty"`
	AllowsRepoWrite         bool     `json:"allows_repo_write,omitempty"`
	AllowsGitHubWrite       bool     `json:"allows_github_write,omitempty"`
	AllowsGitRemoteWrite    bool     `json:"allows_git_remote_write,omitempty"`
	AllowsExternalWrite     bool     `json:"allows_external_write,omitempty"`
	AllowsLocalRuntimeWrite bool     `json:"allows_local_runtime_write,omitempty"`
	AllowsProviderLaunch    bool     `json:"allows_provider_launch,omitempty"`
	CrossRepo               bool     `json:"cross_repo,omitempty"`
	ContainsSecretReference bool     `json:"contains_secret_reference,omitempty"`
	ContainsSecretMaterial  bool     `json:"contains_secret_material,omitempty"`
	CredentialAdjacent      bool     `json:"credential_adjacent,omitempty"`
	Destructive             bool     `json:"destructive,omitempty"`
	ReleasePublish          bool     `json:"release_publish,omitempty"`
	Research                bool     `json:"research,omitempty"`
	Tests                   bool     `json:"tests,omitempty"`
	Documentation           bool     `json:"documentation,omitempty"`
	RuntimeCommands         bool     `json:"runtime_commands,omitempty"`
	SecuritySensitive       bool     `json:"security_sensitive,omitempty"`
	Workflow                bool     `json:"workflow,omitempty"`
	Storage                 bool     `json:"storage,omitempty"`
	Policy                  bool     `json:"policy,omitempty"`
	Routing                 bool     `json:"routing,omitempty"`
	LargeChange             bool     `json:"large_change,omitempty"`
	Ambiguous               bool     `json:"ambiguous,omitempty"`
	RequiresNested          bool     `json:"requires_nested,omitempty"`
	RequiresJSON            bool     `json:"requires_json,omitempty"`
	RequiresMCP             bool     `json:"requires_mcp,omitempty"`
	RequiresImageInput      bool     `json:"requires_image_input,omitempty"`
	RequiresImageOutput     bool     `json:"requires_image_output,omitempty"`
	RequiredTools           []string `json:"required_tools,omitempty"`
	RequiredContextTokens   int      `json:"required_context_tokens,omitempty"`
	UserPin                 bool     `json:"user_pin,omitempty"`
	OverrideRequested       bool     `json:"override_requested,omitempty"`
	BudgetOverride          bool     `json:"budget_override,omitempty"`
}

type ClassificationInput struct {
	ProjectID       string
	DeliveryRunID   string
	TaskID          string
	TaskKey         string
	Title           string
	IntentSummary   string
	RoleKey         string
	PlanFingerprint string
	PolicyVersion   string
	RequiredOutput  OutputContract
	Scope           Scope
	CreatedBy       delivery.Actor
	Host            delivery.Host
	Now             time.Time
	Overrides       []RequirementOverride
}

func Classify(input ClassificationInput) (TaskRequirement, error) {
	now := input.Now
	if now.IsZero() {
		return TaskRequirement{}, typed(ErrInvalidRecordCode, "classification now is required")
	}
	now = now.UTC()
	if input.PolicyVersion == "" {
		input.PolicyVersion = PolicyVersion
	}
	if input.RoleKey == "" {
		input.RoleKey = "worker"
	}
	scope := normalizeScope(input.Scope)
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return TaskRequirement{}, typed(ErrInvalidRecordCode, "marshal scope: %v", err)
	}
	requiredOutput := input.RequiredOutput
	if requiredOutput == "" {
		requiredOutput = defaultOutput(scope)
	}
	nowText := canonicalTimestamp(now)
	req := TaskRequirement{
		SchemaVersion:            SchemaTaskRequirement,
		RecordVersion:            1,
		ProjectID:                strings.TrimSpace(input.ProjectID),
		DeliveryRunID:            strings.TrimSpace(input.DeliveryRunID),
		TaskID:                   strings.TrimSpace(input.TaskID),
		TaskKey:                  strings.TrimSpace(input.TaskKey),
		RoleKey:                  strings.TrimSpace(input.RoleKey),
		RiskTier:                 RiskLow,
		PermissionRequired:       PermissionReadOnly,
		SideEffectClass:          SideEffectLocalRead,
		RequiredOutput:           requiredOutput,
		ScopeJSON:                string(scopeJSON),
		DataClassification:       "public",
		NetworkRequired:          NetworkNotNeeded,
		QualityFloor:             QualityStandard,
		PolicyVersion:            input.PolicyVersion,
		PlanFingerprint:          strings.TrimSpace(input.PlanFingerprint),
		CreatedAt:                nowText,
		UpdatedAt:                nowText,
		CreatedBy:                input.CreatedBy,
		UpdatedBy:                input.CreatedBy,
		Host:                     input.Host,
		Classification:           "planner-metadata",
		Confidence:               providerinventory.ConfidenceExact,
		Source:                   SourceDescriptor{SourceKind: "deterministic-classifier", SourceReference: "0804-task-requirement-rules"},
		ProvenanceSummary:        "classified by deterministic 0804 task requirement rules",
		GapReasons:               []string{},
		ClassificationRules:      []string{},
		RequiredCapabilities:     []CapabilityRequirement{},
		VerificationRequirements: []VerificationRequirement{},
		Heuristics:               []HeuristicOutput{},
	}
	if req.TaskRequirementID == "" && req.ProjectID != "" && req.DeliveryRunID != "" && req.TaskID != "" && req.PlanFingerprint != "" {
		req.TaskRequirementID = TaskRequirementID(req.ProjectID, req.DeliveryRunID, req.TaskID, req.PlanFingerprint)
	}

	floor := requirementFloor{risk: req.RiskTier, permission: req.PermissionRequired, sideEffect: req.SideEffectClass}
	applyRule := func(rule string) {
		req.ClassificationRules = append(req.ClassificationRules, rule)
	}
	raiseRisk := func(risk RiskTier) {
		if riskRanks[risk] > riskRanks[req.RiskTier] {
			req.RiskTier = risk
		}
		if riskRanks[risk] > riskRanks[floor.risk] {
			floor.risk = risk
		}
	}
	raisePermission := func(permission Permission) {
		if permissionRanks[permission] > permissionRanks[req.PermissionRequired] {
			req.PermissionRequired = permission
		}
		if permissionRanks[permission] > permissionRanks[floor.permission] {
			floor.permission = permission
		}
	}
	raiseSideEffect := func(sideEffect SideEffectClass) {
		if sideEffectRanks[sideEffect] > sideEffectRanks[req.SideEffectClass] {
			req.SideEffectClass = sideEffect
		}
		if sideEffectRanks[sideEffect] > sideEffectRanks[floor.sideEffect] {
			floor.sideEffect = sideEffect
		}
	}

	if isDocsOnly(scope) {
		applyRule("scope.docs-only")
		if scope.AllowsRepoWrite {
			raisePermission(PermissionWrite)
			raiseSideEffect(SideEffectRepoWrite)
			raiseRisk(RiskMedium)
		}
	}
	if scope.AllowsRepoWrite || writesFromOutput(requiredOutput) {
		applyRule("scope.repo-write")
		raisePermission(PermissionWrite)
		raiseSideEffect(SideEffectRepoWrite)
		raiseRisk(RiskMedium)
	}
	if scope.AllowsGitHubWrite {
		applyRule("scope.github-write")
		raisePermission(PermissionWrite)
		raiseSideEffect(SideEffectGitHubWrite)
		raiseRisk(RiskHigh)
	}
	if scope.AllowsGitRemoteWrite {
		applyRule("scope.git-remote-write")
		raisePermission(PermissionWrite)
		raiseSideEffect(SideEffectGitRemoteWrite)
		raiseRisk(RiskHigh)
	}
	if scope.AllowsExternalWrite {
		applyRule("scope.external-write")
		raisePermission(PermissionOrchestrate)
		raiseSideEffect(SideEffectExternalWrite)
		raiseRisk(RiskCritical)
		addVerification(&req, VerificationHumanApproval, []RiskTier{RiskCritical}, IndependenceHuman, PermissionReadOnly, OutputVerificationVerdict, "scope.external-write")
	}
	if scope.AllowsLocalRuntimeWrite {
		applyRule("scope.local-runtime-write")
		raisePermission(PermissionWrite)
		raiseSideEffect(SideEffectLocalWrite)
		raiseRisk(RiskMedium)
	}
	if scope.RuntimeCommands {
		applyRule("scope.local-runtime-write")
		raiseSideEffect(SideEffectLocalWrite)
		raiseRisk(RiskMedium)
	}
	if scope.AllowsProviderLaunch {
		applyRule("scope.provider-launch")
		raiseSideEffect(SideEffectProviderLaunch)
		raiseRisk(RiskMedium)
		req.NetworkRequired = NetworkAllowedIfProfileGrants
	}
	if requiredOutput == OutputJSON || requiredOutput == OutputJSONSchema || scope.RequiresJSON {
		applyRule("cap.output-json")
		if requiredOutput != OutputJSONSchema {
			req.RequiredOutput = OutputJSON
		}
		addCapability(&req, CapabilityJSONOutput, true, "deterministic-rule:cap.output-json")
	}
	if req.RoleKey == "verifier" {
		applyRule("cap.verifier-readonly")
		req.PermissionRequired = PermissionReadOnly
		req.RequiredOutput = OutputVerificationVerdict
		addCapability(&req, CapabilityReadOnly, true, "deterministic-rule:cap.verifier-readonly")
		addCapability(&req, CapabilityJSONOutput, true, "deterministic-rule:cap.verifier-readonly")
	}
	if scope.RequiresNested {
		applyRule("cap.nested")
		raisePermission(PermissionOrchestrate)
		raiseRisk(RiskHigh)
		req.NestedAllowed = true
		addCapability(&req, CapabilityNestedSubagents, true, "deterministic-rule:cap.nested")
	}
	if scope.RequiresMCP {
		addCapability(&req, CapabilityMCPConfig, true, "deterministic-rule:scope.requires-mcp")
	}
	if scope.RequiresImageInput {
		addCapability(&req, CapabilityImageInput, true, "deterministic-rule:scope.requires-image-input")
	}
	if scope.RequiresImageOutput {
		addCapability(&req, CapabilityImageOutput, true, "deterministic-rule:scope.requires-image-output")
	}
	if len(scope.RequiredTools) > 0 {
		addCapability(&req, CapabilityToolSupport, append([]string(nil), scope.RequiredTools...), "deterministic-rule:scope.required-tools")
	}
	if scope.RequiredContextTokens > 0 {
		addCapability(&req, CapabilityContextWindowTokens, scope.RequiredContextTokens, "deterministic-rule:scope.required-context")
	}
	if scope.AllowsProviderLaunch || scope.RequiresNested || scope.RuntimeCommands {
		applyRule("cap.cancellation")
		req.CancellationRequired = true
		addCapability(&req, CapabilityCancellation, true, "deterministic-rule:cap.cancellation")
	}
	if scope.BudgetOverride {
		applyRule("cap.token-usage-reporting")
		raiseRisk(RiskHigh)
		addCapability(&req, CapabilityTokenUsageReporting, true, "deterministic-rule:cap.token-usage-reporting")
	}
	if scope.ContainsSecretReference || scope.CredentialAdjacent {
		applyRule("data.secret-reference")
		req.DataClassification = "secret-reference"
		raiseRisk(RiskHigh)
		addVerification(&req, VerificationLoopReview, []RiskTier{RiskHigh, RiskCritical}, IndependenceDifferentProvider, PermissionReadOnly, OutputVerificationVerdict, "data.secret-reference")
	}
	if scope.ContainsSecretMaterial {
		applyRule("data.secret-material")
		req.DataClassification = "secret-material"
		req.TerminalErrorCode = string(ErrNoEligibleCandidateCode)
		req.GapReasons = appendReason(req.GapReasons, "secret-material-refusal")
		raiseRisk(RiskCritical)
		addVerification(&req, VerificationHumanApproval, []RiskTier{RiskCritical}, IndependenceHuman, PermissionReadOnly, OutputVerificationVerdict, "data.secret-material")
	}
	if scope.UserPin {
		applyRule("policy.user-pin")
	}
	if scope.OverrideRequested {
		applyRule("policy.override-requested")
		addVerification(&req, VerificationOverride, []RiskTier{RiskHigh, RiskCritical}, IndependenceHuman, PermissionReadOnly, OutputVerificationVerdict, "policy.override-requested")
	}
	if scope.SecuritySensitive || scope.Workflow || scope.Storage || scope.Policy || scope.Routing || scope.ReleasePublish || scope.CrossRepo || pathLooksCore(scope.Paths) {
		applyRule("quality.security-or-core")
		raiseRisk(RiskHigh)
		req.QualityFloor = QualityStrong
		addVerification(&req, VerificationLoopReview, []RiskTier{RiskHigh, RiskCritical}, IndependenceDifferentProvider, PermissionReadOnly, OutputVerificationVerdict, "quality.security-or-core")
	}
	if scope.ReleasePublish {
		req.ProvenanceSummary = "classified as release/publish-sensitive by deterministic 0804 rules"
	}
	if scope.Destructive || scope.CrossRepo || scope.CredentialAdjacent {
		raiseRisk(RiskCritical)
		addVerification(&req, VerificationHumanApproval, []RiskTier{RiskCritical}, IndependenceHuman, PermissionReadOnly, OutputVerificationVerdict, "quality.critical-side-effect")
	}
	if scope.LargeChange {
		applyRule("quality.large-change")
		req.Heuristic = true
		req.HeuristicReason = appendText(req.HeuristicReason, "large-change estimate may raise risk")
		req.Heuristics = append(req.Heuristics, HeuristicOutput{RuleID: "quality.large-change", Reason: "planned change exceeded classifier threshold input", Confidence: providerinventory.ConfidenceEstimated})
		if req.QualityFloor == QualityStandard {
			req.QualityFloor = QualityStrong
		}
	}
	if scope.Research {
		applyRule("scope.research")
		req.NetworkRequired = NetworkAllowedIfProfileGrants
		addVerification(&req, VerificationSelfCheck, []RiskTier{RiskLow, RiskMedium}, IndependenceNone, PermissionReadOnly, OutputMarkdown, "scope.research")
	}
	if scope.Tests {
		addVerification(&req, VerificationLocalCommand, []RiskTier{RiskMedium, RiskHigh, RiskCritical}, IndependenceNone, PermissionReadOnly, OutputMarkdown, "scope.tests")
	}
	if scope.Documentation && !contains(req.ClassificationRules, "scope.docs-only") {
		applyRule("scope.docs-only")
	}
	if scope.Ambiguous {
		applyRule("quality.ambiguous-intent")
		req.Heuristic = true
		req.HeuristicReason = appendText(req.HeuristicReason, "task intent is ambiguous")
		req.GapReasons = appendReason(req.GapReasons, "ambiguous-intent")
		req.Heuristics = append(req.Heuristics, HeuristicOutput{RuleID: "quality.ambiguous-intent", Reason: "intent could not prove bounded task outcome", Confidence: providerinventory.ConfidenceUnknown})
		if riskRanks[req.RiskTier] >= riskRanks[RiskHigh] {
			req.TerminalErrorCode = string(ErrRequirementUnknownCode)
			addVerification(&req, VerificationHumanApproval, []RiskTier{RiskHigh, RiskCritical}, IndependenceHuman, PermissionReadOnly, OutputVerificationVerdict, "quality.ambiguous-intent")
		}
	}
	if riskRanks[req.RiskTier] >= riskRanks[RiskHigh] {
		addVerification(&req, VerificationLoopReview, []RiskTier{RiskHigh, RiskCritical}, IndependenceDifferentProvider, PermissionReadOnly, OutputVerificationVerdict, "risk.high")
	}
	if len(req.ClassificationRules) == 0 {
		applyRule("scope.read-only")
	}
	if req.SideEffectClass == SideEffectNone || req.SideEffectClass == "" {
		req.SideEffectClass = SideEffectLocalRead
	}
	sort.Strings(req.GapReasons)
	req.ClassificationRules = uniqueStrings(req.ClassificationRules)
	req.RequiredCapabilities = uniqueCapabilities(req.RequiredCapabilities)
	req.VerificationRequirements = uniqueVerification(req.VerificationRequirements)

	applied, err := ApplyOverrides(req, floor, input.Overrides, now, input.CreatedBy, input.Host)
	if err != nil {
		return applied, err
	}
	req = applied
	if req.TaskRequirementFingerprint, err = FingerprintRequirement(req); err != nil {
		return req, typed(ErrInvalidRecordCode, "fingerprint requirement: %v", err)
	}
	if err := Validate(req); err != nil {
		return req, err
	}
	if req.TerminalErrorCode == string(ErrRequirementUnknownCode) {
		return req, typed(ErrRequirementUnknownCode, "ambiguous high-risk task requirement needs approval")
	}
	// Secret-material scope must fail closed with a typed refusal (spec 0804:
	// "unknown or secret-material requirements fail closed with typed errors";
	// data.secret-material => "no route is eligible"). Setting the terminal
	// error code alone is insufficient: a caller checking only the returned
	// error would treat the classification as routable.
	if req.TerminalErrorCode == string(ErrNoEligibleCandidateCode) {
		return req, typed(ErrNoEligibleCandidateCode, "secret-material scope has no eligible route")
	}
	return req, nil
}

type requirementFloor struct {
	risk       RiskTier
	permission Permission
	sideEffect SideEffectClass
}

func ApplyOverrides(req TaskRequirement, floor requirementFloor, overrides []RequirementOverride, now time.Time, actor delivery.Actor, host delivery.Host) (TaskRequirement, error) {
	for _, override := range overrides {
		if !overrideApplies(req, override) {
			continue
		}
		if strings.TrimSpace(override.Status) != "" && override.Status != "active" {
			continue
		}
		req.Source.RecordIDs = append(req.Source.RecordIDs, override.RequirementOverrideID)
		switch override.Field {
		case "risk_tier":
			var value RiskTier
			if err := json.Unmarshal([]byte(override.ValueJSON), &value); err != nil {
				return markOverrideInvalid(req, override, err)
			}
			if riskRanks[value] < riskRanks[floor.risk] {
				return markOverrideInvalid(req, override, typed(ErrRequirementConfidenceInsufficientCode, "override cannot lower risk below deterministic floor %s", floor.risk))
			}
			req.RiskTier = value
		case "permission_required":
			var value Permission
			if err := json.Unmarshal([]byte(override.ValueJSON), &value); err != nil {
				return markOverrideInvalid(req, override, err)
			}
			if permissionRanks[value] < permissionRanks[floor.permission] {
				return markOverrideInvalid(req, override, typed(ErrRequirementConfidenceInsufficientCode, "override cannot lower permission below deterministic floor %s", floor.permission))
			}
			req.PermissionRequired = value
		case "side_effect_class":
			var value SideEffectClass
			if err := json.Unmarshal([]byte(override.ValueJSON), &value); err != nil {
				return markOverrideInvalid(req, override, err)
			}
			if sideEffectRanks[value] < sideEffectRanks[floor.sideEffect] {
				return markOverrideInvalid(req, override, typed(ErrRequirementConfidenceInsufficientCode, "override cannot lower side effect below deterministic floor %s", floor.sideEffect))
			}
			req.SideEffectClass = value
		case "quality_floor":
			var value QualityFloor
			if err := json.Unmarshal([]byte(override.ValueJSON), &value); err != nil {
				return markOverrideInvalid(req, override, err)
			}
			if !qualityFloors[value] {
				return markOverrideInvalid(req, override, typed(ErrInvalidRecordCode, "unknown quality_floor %q", value))
			}
			req.QualityFloor = value
		case "network_required":
			var value NetworkRequirement
			if err := json.Unmarshal([]byte(override.ValueJSON), &value); err != nil {
				return markOverrideInvalid(req, override, err)
			}
			if !networkRequirements[value] {
				return markOverrideInvalid(req, override, typed(ErrInvalidRecordCode, "unknown network_required %q", value))
			}
			req.NetworkRequired = value
		case "data_classification":
			var value string
			if err := json.Unmarshal([]byte(override.ValueJSON), &value); err != nil {
				return markOverrideInvalid(req, override, err)
			}
			if value == "" {
				return markOverrideInvalid(req, override, typed(ErrInvalidRecordCode, "data classification override is empty"))
			}
			req.DataClassification = value
		default:
			return markOverrideInvalid(req, override, typed(ErrInvalidRecordCode, "unsupported override field %q", override.Field))
		}
		req.ClassificationRules = append(req.ClassificationRules, "policy.user-correction")
		req.UpdatedAt = canonicalTimestamp(now)
		req.UpdatedBy = actor
		req.Host = host
	}
	if riskRanks[req.RiskTier] >= riskRanks[RiskHigh] {
		addVerification(&req, VerificationLoopReview, []RiskTier{RiskHigh, RiskCritical}, IndependenceDifferentProvider, PermissionReadOnly, OutputVerificationVerdict, "risk.high")
	}
	if req.RiskTier == RiskCritical || req.SideEffectClass == SideEffectExternalWrite {
		addVerification(&req, VerificationHumanApproval, []RiskTier{RiskCritical}, IndependenceHuman, PermissionReadOnly, OutputVerificationVerdict, "risk.critical")
	}
	req.ClassificationRules = uniqueStrings(req.ClassificationRules)
	req.VerificationRequirements = uniqueVerification(req.VerificationRequirements)
	return req, nil
}

func markOverrideInvalid(req TaskRequirement, override RequirementOverride, err error) (TaskRequirement, error) {
	req.Source.RecordIDs = append(req.Source.RecordIDs, override.RequirementOverrideID)
	req.GapReasons = appendReason(req.GapReasons, "invalid-user-correction")
	var typedErr *TypedError
	if errors.As(err, &typedErr) {
		req.TerminalErrorCode = string(typedErr.Code)
		return req, typedErr
	}
	req.TerminalErrorCode = string(ErrInvalidRecordCode)
	return req, typed(ErrInvalidRecordCode, "invalid override %s: %v", override.RequirementOverrideID, err)
}

func overrideApplies(req TaskRequirement, override RequirementOverride) bool {
	if override.ProjectID != "" && override.ProjectID != req.ProjectID {
		return false
	}
	if override.DeliveryRunID != "" && override.DeliveryRunID != req.DeliveryRunID {
		return false
	}
	if override.TaskID != "" && override.TaskID != req.TaskID {
		return false
	}
	if override.TaskKey != "" && override.TaskKey != req.TaskKey {
		return false
	}
	if override.PlanFingerprint != "" && override.PlanFingerprint != req.PlanFingerprint {
		return false
	}
	return true
}

func Validate(req TaskRequirement) error {
	if req.SchemaVersion != SchemaTaskRequirement || req.RecordVersion != 1 || req.TaskRequirementID == "" || req.ProjectID == "" || req.DeliveryRunID == "" || req.TaskID == "" || req.TaskKey == "" || req.RoleKey == "" || req.PlanFingerprint == "" {
		return typed(ErrInvalidRecordCode, "task requirement has missing required fields")
	}
	if !json.Valid([]byte(req.ScopeJSON)) {
		return typed(ErrInvalidRecordCode, "scope_json is invalid")
	}
	if _, ok := riskRanks[req.RiskTier]; !ok {
		return typed(ErrInvalidRecordCode, "unknown risk_tier %q", req.RiskTier)
	}
	if _, ok := permissionRanks[req.PermissionRequired]; !ok {
		return typed(ErrInvalidRecordCode, "unknown permission_required %q", req.PermissionRequired)
	}
	if _, ok := sideEffectRanks[req.SideEffectClass]; !ok {
		return typed(ErrInvalidRecordCode, "unknown side_effect_class %q", req.SideEffectClass)
	}
	if !outputContracts[req.RequiredOutput] {
		return typed(ErrInvalidRecordCode, "unknown required_output %q", req.RequiredOutput)
	}
	if !networkRequirements[req.NetworkRequired] {
		return typed(ErrInvalidRecordCode, "unknown network_required %q", req.NetworkRequired)
	}
	if !qualityFloors[req.QualityFloor] {
		return typed(ErrInvalidRecordCode, "unknown quality_floor %q", req.QualityFloor)
	}
	for _, capability := range req.RequiredCapabilities {
		if !capabilityDimensions[capability.Dimension] {
			return typed(ErrInvalidRecordCode, "unknown capability dimension %q", capability.Dimension)
		}
	}
	for _, verification := range req.VerificationRequirements {
		if !verificationKinds[verification.VerificationKind] {
			return typed(ErrInvalidRecordCode, "unknown verification_kind %q", verification.VerificationKind)
		}
		if !independenceLevels[verification.IndependenceLevel] {
			return typed(ErrInvalidRecordCode, "unknown independence_level %q", verification.IndependenceLevel)
		}
		if _, ok := permissionRanks[verification.PermissionRequired]; !ok {
			return typed(ErrInvalidRecordCode, "unknown verification permission_required %q", verification.PermissionRequired)
		}
		if !outputContracts[verification.OutputContract] {
			return typed(ErrInvalidRecordCode, "unknown verification output_contract %q", verification.OutputContract)
		}
	}
	if req.Heuristic && strings.TrimSpace(req.HeuristicReason) == "" {
		return typed(ErrInvalidRecordCode, "heuristic requirement requires heuristic_reason")
	}
	if len(req.ClassificationRules) == 0 {
		return typed(ErrRequirementUnknownCode, "no deterministic classification rule matched")
	}
	if req.Confidence == providerinventory.ConfidenceUnknown || req.Confidence == providerinventory.ConfidenceUnavailable || req.Confidence == providerinventory.ConfidenceStale {
		return typed(ErrRequirementConfidenceInsufficientCode, "requirement confidence %q cannot satisfy hard routing", req.Confidence)
	}
	if req.TaskRequirementFingerprint != "" {
		fp, err := FingerprintRequirement(req)
		if err != nil {
			return typed(ErrInvalidRecordCode, "fingerprint requirement: %v", err)
		}
		if fp != req.TaskRequirementFingerprint {
			return typed(ErrInvalidRecordCode, "task requirement fingerprint mismatch")
		}
	}
	return nil
}

func ValidateOverride(override RequirementOverride) error {
	if override.SchemaVersion != "" && override.SchemaVersion != SchemaTaskRequirementOverride {
		return typed(ErrUnknownRecordVersionCode, "unsupported override schema_version %q", override.SchemaVersion)
	}
	if strings.TrimSpace(override.ProjectID) == "" || strings.TrimSpace(override.ScopeKey) == "" || strings.TrimSpace(override.Field) == "" || strings.TrimSpace(override.ValueJSON) == "" || strings.TrimSpace(override.Justification) == "" {
		return typed(ErrInvalidRecordCode, "requirement override has missing required fields")
	}
	if !json.Valid([]byte(override.ValueJSON)) {
		return typed(ErrInvalidRecordCode, "override value_json is invalid")
	}
	return nil
}

func normalizeScope(scope Scope) Scope {
	sort.Strings(scope.Paths)
	sort.Strings(scope.Commands)
	sort.Strings(scope.ExternalServices)
	sort.Strings(scope.Providers)
	sort.Strings(scope.RequiredTools)
	if len(scope.Commands) > 0 {
		scope.RuntimeCommands = true
	}
	return scope
}

func defaultOutput(scope Scope) OutputContract {
	if scope.Tests || scope.Research {
		return OutputMarkdown
	}
	if scope.AllowsRepoWrite {
		return OutputPatch
	}
	if scope.AllowsGitRemoteWrite {
		return OutputBranch
	}
	if scope.AllowsGitHubWrite {
		return OutputPR
	}
	return OutputFreeform
}

func writesFromOutput(output OutputContract) bool {
	switch output {
	case OutputPatch, OutputBranch, OutputPR:
		return true
	default:
		return false
	}
}

func isDocsOnly(scope Scope) bool {
	if scope.AllowsGitHubWrite || scope.AllowsGitRemoteWrite || scope.AllowsExternalWrite || scope.AllowsLocalRuntimeWrite || scope.AllowsProviderLaunch || scope.RuntimeCommands {
		return false
	}
	if scope.Documentation {
		return true
	}
	if len(scope.Paths) == 0 {
		return false
	}
	for _, p := range scope.Paths {
		p = filepath.ToSlash(strings.TrimSpace(p))
		ext := strings.ToLower(filepath.Ext(p))
		if !(strings.HasPrefix(p, "docs/") || ext == ".md" || strings.HasPrefix(p, "README") || strings.Contains(p, "/README")) {
			return false
		}
	}
	return true
}

func pathLooksCore(paths []string) bool {
	for _, p := range paths {
		p = filepath.ToSlash(strings.ToLower(strings.TrimSpace(p)))
		if strings.HasPrefix(p, ".github/workflows/") || strings.Contains(p, "security") || strings.Contains(p, "credential") || strings.Contains(p, "secret") || strings.HasPrefix(p, "internal/storage/") || strings.HasPrefix(p, "internal/orchestration/") || strings.HasPrefix(p, "internal/providerinventory/") || strings.HasPrefix(p, "internal/usageledger/") {
			return true
		}
	}
	return false
}

func addCapability(req *TaskRequirement, dimension CapabilityDimension, value any, source string) {
	req.RequiredCapabilities = append(req.RequiredCapabilities, CapabilityRequirement{
		Dimension:         dimension,
		RequiredValue:     value,
		MinimumConfidence: providerinventory.ConfidenceExact,
		FreshnessRequired: providerinventory.FreshnessFresh,
		Source:            source,
	})
}

func addVerification(req *TaskRequirement, kind VerificationKind, tiers []RiskTier, independence IndependenceLevel, permission Permission, output OutputContract, source string) {
	req.VerificationRequirements = append(req.VerificationRequirements, VerificationRequirement{
		VerificationKind:     kind,
		RequiredForRiskTiers: tiers,
		IndependenceLevel:    independence,
		PermissionRequired:   permission,
		OutputContract:       output,
		Source:               source,
	})
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func uniqueCapabilities(values []CapabilityRequirement) []CapabilityRequirement {
	seen := map[CapabilityDimension]bool{}
	out := make([]CapabilityRequirement, 0, len(values))
	for _, value := range values {
		if seen[value.Dimension] {
			continue
		}
		seen[value.Dimension] = true
		out = append(out, value)
	}
	return out
}

func uniqueVerification(values []VerificationRequirement) []VerificationRequirement {
	seen := map[string]bool{}
	out := make([]VerificationRequirement, 0, len(values))
	for _, value := range values {
		key := string(value.VerificationKind) + "\x00" + value.Source
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func appendReason(reasons []string, reason string) []string {
	if reason == "" || contains(reasons, reason) {
		return reasons
	}
	return append(reasons, reason)
}

func appendText(existing, next string) string {
	if strings.TrimSpace(existing) == "" {
		return next
	}
	return existing + "; " + next
}
