package routing

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"sort"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	RoleDefinitionSchema        = "loopcoder.role_definition.v1"
	RoleDefinitionPolicyVersion = "role-definition-v1"
	RoleKeyWorker               = "worker"
	RoleKeyVerifier             = "verifier"
	RoleKeyNestedSubagent       = "nested-subagent"
	RoleKeySoul                 = "soul"
	RoleKeyTera                 = "tera"
	RoleKeyLuna                 = "luna"
)

type ReasoningDepth string

const (
	ReasoningDepthStandard    ReasoningDepth = "standard"
	ReasoningDepthDeep        ReasoningDepth = "deep"
	ReasoningDepthAdversarial ReasoningDepth = "adversarial"
)

type LatencyTolerance string

const (
	LatencyToleranceLow      LatencyTolerance = "low"
	LatencyToleranceStandard LatencyTolerance = "standard"
	LatencyToleranceRelaxed  LatencyTolerance = "relaxed"
)

type CostTolerance string

const (
	CostToleranceLow      CostTolerance = "low"
	CostToleranceStandard CostTolerance = "standard"
	CostToleranceHigh     CostTolerance = "high"
)

type RoleDefinition struct {
	SchemaVersion              string                                                           `json:"schema_version"`
	RecordVersion              int                                                              `json:"record_version"`
	RoleDefinitionID           string                                                           `json:"role_definition_id"`
	RoleKey                    string                                                           `json:"role_key"`
	RoleVersion                string                                                           `json:"role_version"`
	Description                string                                                           `json:"description"`
	AllowedRiskTiers           []taskrequirements.RiskTier                                      `json:"allowed_risk_tiers"`
	MinimumCapabilities        []taskrequirements.CapabilityRequirement                         `json:"minimum_capabilities"`
	PermissionFloor            taskrequirements.Permission                                      `json:"permission_floor"`
	PermissionCeiling          taskrequirements.Permission                                      `json:"permission_ceiling"`
	DefaultOutputContract      taskrequirements.OutputContract                                  `json:"default_output_contract"`
	IndependenceRequirements   map[taskrequirements.RiskTier]taskrequirements.IndependenceLevel `json:"independence_requirements"`
	ForbiddenBindings          []string                                                         `json:"forbidden_bindings"`
	QualityFloor               taskrequirements.QualityFloor                                    `json:"quality_floor"`
	ReasoningDepth             ReasoningDepth                                                   `json:"reasoning_depth"`
	RequiredTools              []string                                                         `json:"required_tools"`
	MinimumContextWindowTokens int                                                              `json:"minimum_context_window_tokens,omitempty"`
	MaxSideEffectClass         taskrequirements.SideEffectClass                                 `json:"max_side_effect_class"`
	VerificationRequirements   []taskrequirements.VerificationRequirement                       `json:"verification_requirements"`
	LatencyTolerance           LatencyTolerance                                                 `json:"latency_tolerance"`
	CostTolerance              CostTolerance                                                    `json:"cost_tolerance"`
	PolicyVersion              string                                                           `json:"policy_version"`
}

func BuiltInRoleDefinitions() []RoleDefinition {
	roles := []RoleDefinition{
		builtInRole(RoleKeyWorker, "Worker implementation role envelope", []taskrequirements.RiskTier{taskrequirements.RiskLow, taskrequirements.RiskMedium, taskrequirements.RiskHigh}, roleCapability(taskrequirements.CapabilityRolesSupported, RoleKeyWorker), taskrequirements.PermissionWrite, taskrequirements.PermissionWrite, taskrequirements.OutputPatch, taskrequirements.QualityStandard, ReasoningDepthStandard, taskrequirements.SideEffectProviderLaunch, LatencyToleranceStandard, CostToleranceStandard),
		builtInRole(RoleKeyVerifier, "Read-only verifier role envelope", []taskrequirements.RiskTier{taskrequirements.RiskLow, taskrequirements.RiskMedium, taskrequirements.RiskHigh}, roleCapability(taskrequirements.CapabilityRolesSupported, RoleKeyVerifier), taskrequirements.PermissionReadOnly, taskrequirements.PermissionReadOnly, taskrequirements.OutputVerificationVerdict, taskrequirements.QualityStandard, ReasoningDepthAdversarial, taskrequirements.SideEffectProviderLaunch, LatencyToleranceStandard, CostToleranceStandard),
		builtInRole(RoleKeyNestedSubagent, "Nested sub-agent delegation role envelope", []taskrequirements.RiskTier{taskrequirements.RiskLow, taskrequirements.RiskMedium, taskrequirements.RiskHigh}, roleCapability(taskrequirements.CapabilityRolesSupported, string(providerinventory.CatalogRoleNestedSubagents)), taskrequirements.PermissionReadOnly, taskrequirements.PermissionOrchestrate, taskrequirements.OutputReport, taskrequirements.QualityStandard, ReasoningDepthDeep, taskrequirements.SideEffectProviderLaunch, LatencyToleranceRelaxed, CostToleranceStandard),
		builtInRole(RoleKeyLuna, "Low-latency standard worker envelope", []taskrequirements.RiskTier{taskrequirements.RiskLow, taskrequirements.RiskMedium}, roleCapability(taskrequirements.CapabilityRolesSupported, RoleKeyWorker), taskrequirements.PermissionReadOnly, taskrequirements.PermissionWrite, taskrequirements.OutputMarkdown, taskrequirements.QualityStandard, ReasoningDepthStandard, taskrequirements.SideEffectProviderLaunch, LatencyToleranceLow, CostToleranceLow),
		builtInRole(RoleKeyTera, "Deep worker envelope for high-context implementation", []taskrequirements.RiskTier{taskrequirements.RiskMedium, taskrequirements.RiskHigh}, roleCapability(taskrequirements.CapabilityRolesSupported, RoleKeyWorker), taskrequirements.PermissionWrite, taskrequirements.PermissionWrite, taskrequirements.OutputPatch, taskrequirements.QualityStrong, ReasoningDepthDeep, taskrequirements.SideEffectProviderLaunch, LatencyToleranceRelaxed, CostToleranceHigh),
		builtInRole(RoleKeySoul, "Adversarial read-only review envelope", []taskrequirements.RiskTier{taskrequirements.RiskMedium, taskrequirements.RiskHigh, taskrequirements.RiskCritical}, roleCapability(taskrequirements.CapabilityRolesSupported, RoleKeyVerifier), taskrequirements.PermissionReadOnly, taskrequirements.PermissionReadOnly, taskrequirements.OutputVerificationVerdict, taskrequirements.QualityAdversarial, ReasoningDepthAdversarial, taskrequirements.SideEffectProviderLaunch, LatencyToleranceRelaxed, CostToleranceHigh),
	}
	for i := range roles {
		switch roles[i].RoleKey {
		case RoleKeyVerifier, RoleKeySoul:
			roles[i].MinimumCapabilities = append(roles[i].MinimumCapabilities,
				boolCapability(taskrequirements.CapabilityReadOnly),
				boolCapability(taskrequirements.CapabilityJSONOutput),
			)
		case RoleKeyNestedSubagent:
			roles[i].MinimumCapabilities = append(roles[i].MinimumCapabilities,
				boolCapability(taskrequirements.CapabilityNestedSubagents),
				boolCapability(taskrequirements.CapabilityCancellation),
			)
		}
		if roles[i].RoleKey == RoleKeySoul {
			roles[i].VerificationRequirements = append(roles[i].VerificationRequirements, taskrequirements.VerificationRequirement{
				VerificationKind:     taskrequirements.VerificationLoopReview,
				RequiredForRiskTiers: []taskrequirements.RiskTier{taskrequirements.RiskHigh, taskrequirements.RiskCritical},
				IndependenceLevel:    taskrequirements.IndependenceDifferentProvider,
				PermissionRequired:   taskrequirements.PermissionReadOnly,
				OutputContract:       taskrequirements.OutputVerificationVerdict,
				Source:               "role." + roles[i].RoleKey,
			})
		}
		roles[i] = normalizeRoleDefinition(roles[i])
	}
	return roles
}

func ResolveRoleDefinition(roleKey string, custom []RoleDefinition) (RoleDefinition, bool) {
	roleKey = normalizeRoleKey(roleKey)
	for _, role := range BuiltInRoleDefinitions() {
		if normalizeRoleKey(role.RoleKey) == roleKey {
			return role, true
		}
	}
	for _, role := range custom {
		if normalizeRoleKey(role.RoleKey) == roleKey {
			return role, true
		}
	}
	return RoleDefinition{}, false
}

func ComposeRoleRequirement(req taskrequirements.TaskRequirement, role RoleDefinition) (taskrequirements.TaskRequirement, error) {
	if err := ValidateRoleDefinition(role); err != nil {
		return req, err
	}
	role = normalizeRoleDefinition(role)
	if normalizeRoleKey(req.RoleKey) != normalizeRoleKey(role.RoleKey) {
		return req, &taskrequirements.TypedError{Code: taskrequirements.ErrRoleUnsupportedCode, Message: "role definition key does not match task role"}
	}
	if len(role.AllowedRiskTiers) > 0 && !riskTierAllowed(req.RiskTier, role.AllowedRiskTiers) {
		return req, &taskrequirements.TypedError{Code: taskrequirements.ErrRoleUnsupportedCode, Message: fmt.Sprintf("role %s does not allow risk tier %s", role.RoleKey, req.RiskTier)}
	}
	if role.MaxSideEffectClass != "" && sideEffectRank(req.SideEffectClass) > sideEffectRank(role.MaxSideEffectClass) {
		return req, &taskrequirements.TypedError{Code: taskrequirements.ErrRoleUnsupportedCode, Message: fmt.Sprintf("role %s side-effect ceiling %s is below task requirement %s", role.RoleKey, role.MaxSideEffectClass, req.SideEffectClass)}
	}
	if permissionRank(role.PermissionFloor) > permissionRank(req.PermissionRequired) {
		req.PermissionRequired = role.PermissionFloor
	}
	if qualityRank(role.QualityFloor) > qualityRank(req.QualityFloor) {
		req.QualityFloor = role.QualityFloor
	}
	if req.RequiredOutput == "" {
		req.RequiredOutput = role.DefaultOutputContract
	}
	req.RequiredCapabilities = append(req.RequiredCapabilities, role.MinimumCapabilities...)
	if role.MinimumContextWindowTokens > 0 {
		req.RequiredCapabilities = append(req.RequiredCapabilities, taskrequirements.CapabilityRequirement{
			Dimension:         taskrequirements.CapabilityContextWindowTokens,
			RequiredValue:     role.MinimumContextWindowTokens,
			MinimumConfidence: providerinventory.ConfidenceExact,
			FreshnessRequired: providerinventory.FreshnessFresh,
			Source:            "role." + role.RoleKey + ".minimum_context_window_tokens",
		})
	}
	if len(role.RequiredTools) > 0 {
		req.RequiredCapabilities = append(req.RequiredCapabilities, taskrequirements.CapabilityRequirement{
			Dimension:         taskrequirements.CapabilityToolSupport,
			RequiredValue:     append([]string(nil), role.RequiredTools...),
			MinimumConfidence: providerinventory.ConfidenceExact,
			FreshnessRequired: providerinventory.FreshnessFresh,
			Source:            "role." + role.RoleKey + ".required_tools",
		})
	}
	if level, ok := role.IndependenceRequirements[req.RiskTier]; ok && level != taskrequirements.IndependenceNone {
		req.VerificationRequirements = append(req.VerificationRequirements, taskrequirements.VerificationRequirement{
			VerificationKind:     taskrequirements.VerificationLoopReview,
			RequiredForRiskTiers: []taskrequirements.RiskTier{req.RiskTier},
			IndependenceLevel:    level,
			PermissionRequired:   taskrequirements.PermissionReadOnly,
			OutputContract:       taskrequirements.OutputVerificationVerdict,
			Source:               "role." + role.RoleKey + ".independence_requirements",
		})
	}
	req.VerificationRequirements = append(req.VerificationRequirements, role.VerificationRequirements...)
	return req, nil
}

func RoleDefinitionsFromConfig(roles []config.RoleDefinition) ([]RoleDefinition, error) {
	out := make([]RoleDefinition, 0, len(roles))
	for index, role := range roles {
		converted, err := RoleDefinitionFromConfig(role)
		if err != nil {
			return nil, fmt.Errorf("role_definitions[%d]: %w", index, err)
		}
		out = append(out, converted)
	}
	return out, nil
}

func RoleDefinitionFromConfig(role config.RoleDefinition) (RoleDefinition, error) {
	out := RoleDefinition{
		SchemaVersion:              strings.TrimSpace(role.SchemaVersion),
		RecordVersion:              role.RecordVersion,
		RoleDefinitionID:           strings.TrimSpace(role.RoleDefinitionID),
		RoleKey:                    strings.TrimSpace(role.RoleKey),
		RoleVersion:                strings.TrimSpace(role.RoleVersion),
		Description:                strings.TrimSpace(role.Description),
		AllowedRiskTiers:           make([]taskrequirements.RiskTier, 0, len(role.AllowedRiskTiers)),
		MinimumCapabilities:        make([]taskrequirements.CapabilityRequirement, 0, len(role.MinimumCapabilities)),
		PermissionFloor:            taskrequirements.Permission(strings.TrimSpace(role.PermissionFloor)),
		PermissionCeiling:          taskrequirements.Permission(strings.TrimSpace(role.PermissionCeiling)),
		DefaultOutputContract:      taskrequirements.OutputContract(strings.TrimSpace(role.DefaultOutputContract)),
		IndependenceRequirements:   map[taskrequirements.RiskTier]taskrequirements.IndependenceLevel{},
		ForbiddenBindings:          append([]string(nil), role.ForbiddenBindings...),
		QualityFloor:               taskrequirements.QualityFloor(strings.TrimSpace(role.QualityFloor)),
		ReasoningDepth:             ReasoningDepth(strings.TrimSpace(role.ReasoningDepth)),
		RequiredTools:              append([]string(nil), role.RequiredTools...),
		MinimumContextWindowTokens: role.MinimumContextWindowTokens,
		MaxSideEffectClass:         taskrequirements.SideEffectClass(strings.TrimSpace(role.MaxSideEffectClass)),
		VerificationRequirements:   make([]taskrequirements.VerificationRequirement, 0, len(role.VerificationRequirements)),
		LatencyTolerance:           LatencyTolerance(strings.TrimSpace(role.LatencyTolerance)),
		CostTolerance:              CostTolerance(strings.TrimSpace(role.CostTolerance)),
		PolicyVersion:              strings.TrimSpace(role.PolicyVersion),
	}
	for _, risk := range role.AllowedRiskTiers {
		out.AllowedRiskTiers = append(out.AllowedRiskTiers, taskrequirements.RiskTier(strings.TrimSpace(risk)))
	}
	for risk, independence := range role.IndependenceRequirements {
		out.IndependenceRequirements[taskrequirements.RiskTier(strings.TrimSpace(risk))] = taskrequirements.IndependenceLevel(strings.TrimSpace(independence))
	}
	for _, capability := range role.MinimumCapabilities {
		converted := taskrequirements.CapabilityRequirement{
			Dimension:         taskrequirements.CapabilityDimension(strings.TrimSpace(capability.Dimension)),
			RequiredValue:     normalizeCapabilityValue(taskrequirements.CapabilityDimension(strings.TrimSpace(capability.Dimension)), capability.RequiredValue),
			MinimumConfidence: providerinventory.Confidence(strings.TrimSpace(capability.MinimumConfidence)),
			FreshnessRequired: providerinventory.FreshnessState(strings.TrimSpace(capability.FreshnessRequired)),
			Source:            strings.TrimSpace(capability.Source),
		}
		out.MinimumCapabilities = append(out.MinimumCapabilities, converted)
	}
	for _, verification := range role.VerificationRequirements {
		converted := taskrequirements.VerificationRequirement{
			VerificationKind:     taskrequirements.VerificationKind(strings.TrimSpace(verification.VerificationKind)),
			RequiredForRiskTiers: make([]taskrequirements.RiskTier, 0, len(verification.RequiredForRiskTiers)),
			IndependenceLevel:    taskrequirements.IndependenceLevel(strings.TrimSpace(verification.IndependenceLevel)),
			PermissionRequired:   taskrequirements.Permission(strings.TrimSpace(verification.PermissionRequired)),
			OutputContract:       taskrequirements.OutputContract(strings.TrimSpace(verification.OutputContract)),
			Source:               strings.TrimSpace(verification.Source),
		}
		for _, risk := range verification.RequiredForRiskTiers {
			converted.RequiredForRiskTiers = append(converted.RequiredForRiskTiers, taskrequirements.RiskTier(strings.TrimSpace(risk)))
		}
		out.VerificationRequirements = append(out.VerificationRequirements, converted)
	}
	out = normalizeRoleDefinition(out)
	if err := ValidateRoleDefinition(out); err != nil {
		return RoleDefinition{}, err
	}
	return out, nil
}

func ValidateRoleDefinition(role RoleDefinition) error {
	if strings.TrimSpace(role.SchemaVersion) != RoleDefinitionSchema {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrUnknownRecordVersionCode, Message: fmt.Sprintf("unsupported role schema_version %q", role.SchemaVersion)}
	}
	if role.RecordVersion != 1 {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrUnknownRecordVersionCode, Message: fmt.Sprintf("unsupported role record_version %d", role.RecordVersion)}
	}
	if strings.TrimSpace(role.RoleKey) == "" || strings.TrimSpace(role.RoleVersion) == "" || strings.TrimSpace(role.Description) == "" || strings.TrimSpace(role.PolicyVersion) == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "role definition has missing role key, version, or description"}
	}
	role = normalizeRoleDefinition(role)
	if want := RoleDefinitionID(role.RoleKey, role.RoleVersion, role.PolicyVersion); role.RoleDefinitionID != "" && role.RoleDefinitionID != want {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "role_definition_id does not match role key, version, and policy version"}
	}
	if len(role.AllowedRiskTiers) == 0 || len(role.MinimumCapabilities) == 0 {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "role definition requires risk tiers and minimum capabilities"}
	}
	if !validPermission(role.PermissionFloor) || !validPermission(role.PermissionCeiling) || permissionRank(role.PermissionFloor) > permissionRank(role.PermissionCeiling) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "role permission floor and ceiling are invalid"}
	}
	if !validOutputContract(role.DefaultOutputContract) || !validQuality(role.QualityFloor) || !validReasoningDepth(role.ReasoningDepth) || !validSideEffect(role.MaxSideEffectClass) || !validLatency(role.LatencyTolerance) || !validCost(role.CostTolerance) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "role definition contains an unknown enum value"}
	}
	if role.MinimumContextWindowTokens < 0 {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "minimum_context_window_tokens must be non-negative"}
	}
	for _, risk := range role.AllowedRiskTiers {
		if !validRiskTier(risk) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("unknown allowed risk tier %q", risk)}
		}
	}
	for risk, independence := range role.IndependenceRequirements {
		if !validRiskTier(risk) || !validIndependence(independence) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown independence requirement"}
		}
	}
	for _, capability := range role.MinimumCapabilities {
		if err := validateCapabilityRequirement(capability); err != nil {
			return err
		}
	}
	for _, verification := range role.VerificationRequirements {
		if !validVerificationKind(verification.VerificationKind) || !validIndependence(verification.IndependenceLevel) || !validPermission(verification.PermissionRequired) || !validOutputContract(verification.OutputContract) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "unknown verification requirement field"}
		}
		for _, risk := range verification.RequiredForRiskTiers {
			if !validRiskTier(risk) {
				return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("unknown verification risk tier %q", risk)}
			}
		}
	}
	return nil
}

func RoleDefinitionID(roleKey, roleVersion, policyVersion string) string {
	return "roledef_" + roleDigestBase32(normalizeRoleKey(roleKey), strings.TrimSpace(roleVersion), strings.TrimSpace(policyVersion))
}

func roleCapability(dimension taskrequirements.CapabilityDimension, value any) taskrequirements.CapabilityRequirement {
	return taskrequirements.CapabilityRequirement{
		Dimension:         dimension,
		RequiredValue:     value,
		MinimumConfidence: providerinventory.ConfidenceExact,
		FreshnessRequired: providerinventory.FreshnessFresh,
		Source:            "built-in-role",
	}
}

func boolCapability(dimension taskrequirements.CapabilityDimension) taskrequirements.CapabilityRequirement {
	return roleCapability(dimension, true)
}

func builtInRole(key, description string, risks []taskrequirements.RiskTier, roleCap taskrequirements.CapabilityRequirement, floor, ceiling taskrequirements.Permission, output taskrequirements.OutputContract, quality taskrequirements.QualityFloor, depth ReasoningDepth, maxSideEffect taskrequirements.SideEffectClass, latency LatencyTolerance, cost CostTolerance) RoleDefinition {
	return RoleDefinition{
		SchemaVersion:            RoleDefinitionSchema,
		RecordVersion:            1,
		RoleKey:                  key,
		RoleVersion:              "1",
		Description:              description,
		AllowedRiskTiers:         append([]taskrequirements.RiskTier(nil), risks...),
		MinimumCapabilities:      []taskrequirements.CapabilityRequirement{roleCap},
		PermissionFloor:          floor,
		PermissionCeiling:        ceiling,
		DefaultOutputContract:    output,
		IndependenceRequirements: map[taskrequirements.RiskTier]taskrequirements.IndependenceLevel{},
		ForbiddenBindings:        []string{"account_profile_id", "model_id", "provider_name"},
		QualityFloor:             quality,
		ReasoningDepth:           depth,
		MaxSideEffectClass:       maxSideEffect,
		LatencyTolerance:         latency,
		CostTolerance:            cost,
		PolicyVersion:            RoleDefinitionPolicyVersion,
	}
}

func normalizeRoleDefinition(role RoleDefinition) RoleDefinition {
	role.RoleKey = normalizeRoleKey(role.RoleKey)
	if role.SchemaVersion == "" {
		role.SchemaVersion = RoleDefinitionSchema
	}
	if role.RecordVersion == 0 {
		role.RecordVersion = 1
	}
	if role.RoleVersion == "" {
		role.RoleVersion = "1"
	}
	if role.PolicyVersion == "" {
		role.PolicyVersion = RoleDefinitionPolicyVersion
	}
	if role.RoleDefinitionID == "" && role.RoleKey != "" {
		role.RoleDefinitionID = RoleDefinitionID(role.RoleKey, role.RoleVersion, role.PolicyVersion)
	}
	if role.PermissionFloor == "" {
		role.PermissionFloor = taskrequirements.PermissionReadOnly
	}
	if role.PermissionCeiling == "" {
		role.PermissionCeiling = taskrequirements.PermissionWrite
	}
	if role.DefaultOutputContract == "" {
		role.DefaultOutputContract = taskrequirements.OutputFreeform
	}
	if role.QualityFloor == "" {
		role.QualityFloor = taskrequirements.QualityStandard
	}
	if role.ReasoningDepth == "" {
		role.ReasoningDepth = ReasoningDepthStandard
	}
	if role.MaxSideEffectClass == "" {
		role.MaxSideEffectClass = taskrequirements.SideEffectProviderLaunch
	}
	if role.LatencyTolerance == "" {
		role.LatencyTolerance = LatencyToleranceStandard
	}
	if role.CostTolerance == "" {
		role.CostTolerance = CostToleranceStandard
	}
	role.ForbiddenBindings = dedupeStrings(role.ForbiddenBindings)
	role.RequiredTools = dedupeStrings(role.RequiredTools)
	sort.Slice(role.AllowedRiskTiers, func(i, j int) bool { return role.AllowedRiskTiers[i] < role.AllowedRiskTiers[j] })
	return role
}

func normalizeCapabilityValue(dimension taskrequirements.CapabilityDimension, value any) any {
	switch dimension {
	case taskrequirements.CapabilityRolesSupported, taskrequirements.CapabilityToolSupport:
		if values, ok := stringListRequirement(value); ok {
			return values
		}
		return value
	case taskrequirements.CapabilityContextWindowTokens:
		if numeric, ok := numericRequirementValue(value); ok {
			return numeric
		}
		return value
	default:
		return value
	}
}

func validateCapabilityRequirement(capability taskrequirements.CapabilityRequirement) error {
	if !validCapabilityDimension(capability.Dimension) {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("unknown capability dimension %q", capability.Dimension)}
	}
	if !validHardConfidence(capability.MinimumConfidence) || capability.FreshnessRequired != providerinventory.FreshnessFresh {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "capability requires fresh exact or estimated evidence"}
	}
	switch capability.Dimension {
	case taskrequirements.CapabilityRolesSupported, taskrequirements.CapabilityToolSupport:
		values, ok := stringListRequirement(capability.RequiredValue)
		if !ok || len(values) == 0 {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("%s requires a non-empty string list", capability.Dimension)}
		}
	case taskrequirements.CapabilityContextWindowTokens:
		value, ok := numericRequirementValue(capability.RequiredValue)
		if !ok || value < 0 {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "context_window_tokens must be non-negative"}
		}
	case taskrequirements.CapabilityReadOnly, taskrequirements.CapabilityJSONOutput, taskrequirements.CapabilityNestedSubagents, taskrequirements.CapabilityMCPConfig, taskrequirements.CapabilityCancellation, taskrequirements.CapabilityTokenUsageReporting, taskrequirements.CapabilityImageInput, taskrequirements.CapabilityImageOutput:
		required, ok := capability.RequiredValue.(bool)
		if !ok || !required {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("%s requires required_value true", capability.Dimension)}
		}
	}
	return nil
}

func validRiskTier(value taskrequirements.RiskTier) bool {
	switch value {
	case taskrequirements.RiskLow, taskrequirements.RiskMedium, taskrequirements.RiskHigh, taskrequirements.RiskCritical:
		return true
	default:
		return false
	}
}

func validPermission(value taskrequirements.Permission) bool {
	switch value {
	case taskrequirements.PermissionReadOnly, taskrequirements.PermissionWrite, taskrequirements.PermissionOrchestrate:
		return true
	default:
		return false
	}
}

func validSideEffect(value taskrequirements.SideEffectClass) bool {
	switch value {
	case taskrequirements.SideEffectNone, taskrequirements.SideEffectLocalRead, taskrequirements.SideEffectLocalWrite, taskrequirements.SideEffectRepoWrite, taskrequirements.SideEffectProviderLaunch, taskrequirements.SideEffectGitRemoteWrite, taskrequirements.SideEffectGitHubWrite, taskrequirements.SideEffectExternalWrite:
		return true
	default:
		return false
	}
}

func validOutputContract(value taskrequirements.OutputContract) bool {
	switch value {
	case taskrequirements.OutputFreeform, taskrequirements.OutputMarkdown, taskrequirements.OutputJSON, taskrequirements.OutputJSONSchema, taskrequirements.OutputPatch, taskrequirements.OutputBranch, taskrequirements.OutputPR, taskrequirements.OutputReport, taskrequirements.OutputVerificationVerdict:
		return true
	default:
		return false
	}
}

func validQuality(value taskrequirements.QualityFloor) bool {
	switch value {
	case taskrequirements.QualityStandard, taskrequirements.QualityStrong, taskrequirements.QualityAdversarial:
		return true
	default:
		return false
	}
}

func validReasoningDepth(value ReasoningDepth) bool {
	switch value {
	case ReasoningDepthStandard, ReasoningDepthDeep, ReasoningDepthAdversarial:
		return true
	default:
		return false
	}
}

func validLatency(value LatencyTolerance) bool {
	switch value {
	case LatencyToleranceLow, LatencyToleranceStandard, LatencyToleranceRelaxed:
		return true
	default:
		return false
	}
}

func validCost(value CostTolerance) bool {
	switch value {
	case CostToleranceLow, CostToleranceStandard, CostToleranceHigh:
		return true
	default:
		return false
	}
}

func validIndependence(value taskrequirements.IndependenceLevel) bool {
	switch value {
	case taskrequirements.IndependenceNone, taskrequirements.IndependenceDifferentModel, taskrequirements.IndependenceDifferentAccount, taskrequirements.IndependenceDifferentProvider, taskrequirements.IndependenceHuman:
		return true
	default:
		return false
	}
}

func validVerificationKind(value taskrequirements.VerificationKind) bool {
	switch value {
	case taskrequirements.VerificationNone, taskrequirements.VerificationSelfCheck, taskrequirements.VerificationLocalCommand, taskrequirements.VerificationHostedCheck, taskrequirements.VerificationLoopReview, taskrequirements.VerificationSecurityReview, taskrequirements.VerificationHumanApproval, taskrequirements.VerificationOverride:
		return true
	default:
		return false
	}
}

func validCapabilityDimension(value taskrequirements.CapabilityDimension) bool {
	switch value {
	case taskrequirements.CapabilityRolesSupported, taskrequirements.CapabilityReadOnly, taskrequirements.CapabilityJSONOutput, taskrequirements.CapabilityNestedSubagents, taskrequirements.CapabilityMCPConfig, taskrequirements.CapabilityCancellation, taskrequirements.CapabilityTokenUsageReporting, taskrequirements.CapabilityContextWindowTokens, taskrequirements.CapabilityToolSupport, taskrequirements.CapabilityImageInput, taskrequirements.CapabilityImageOutput:
		return true
	default:
		return false
	}
}

func validHardConfidence(value providerinventory.Confidence) bool {
	switch value {
	case providerinventory.ConfidenceExact, providerinventory.ConfidenceEstimated:
		return true
	default:
		return false
	}
}

func normalizeRoleKey(roleKey string) string {
	return strings.ToLower(strings.TrimSpace(roleKey))
}

func riskTierAllowed(risk taskrequirements.RiskTier, allowed []taskrequirements.RiskTier) bool {
	for _, candidate := range allowed {
		if candidate == risk {
			return true
		}
	}
	return false
}

func roleDigestBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
}
