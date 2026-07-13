package routing

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"sort"
	"strings"

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
		role = normalizeRoleDefinition(role)
		if normalizeRoleKey(role.RoleKey) == roleKey {
			return role, true
		}
	}
	return RoleDefinition{}, false
}

func ComposeRoleRequirement(req taskrequirements.TaskRequirement, role RoleDefinition) (taskrequirements.TaskRequirement, error) {
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
	req.VerificationRequirements = append(req.VerificationRequirements, role.VerificationRequirements...)
	return req, nil
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
