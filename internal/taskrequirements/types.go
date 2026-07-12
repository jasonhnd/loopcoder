// Package taskrequirements implements the v0.8 planner task requirement
// classification contract.
package taskrequirements

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

const (
	SchemaTaskRequirement         = "loopcoder.task_requirement.v1"
	SchemaTaskRequirementOverride = "loopcoder.task_requirement_override.v1"
	PolicyVersion                 = "task-requirement-classifier-v1"
)

type ErrorCode string

const (
	ErrRequirementUnknownCode                ErrorCode = "ErrRequirementUnknown"
	ErrRequirementConfidenceInsufficientCode ErrorCode = "ErrRequirementConfidenceInsufficient"
	ErrGraphBoundExceededCode                ErrorCode = "ErrGraphBoundExceeded"
	ErrGraphCycleDetectedCode                ErrorCode = "ErrGraphCycleDetected"
	ErrGraphDisconnectedCode                 ErrorCode = "ErrGraphDisconnected"
	ErrNoEligibleCandidateCode               ErrorCode = "ErrNoEligibleCandidate"
	ErrPinnedCandidateIneligibleCode         ErrorCode = "ErrPinnedCandidateIneligible"
	ErrRoleUnsupportedCode                   ErrorCode = "ErrRoleUnsupported"
	ErrCapabilityUnsupportedCode             ErrorCode = "ErrCapabilityUnsupported"
	ErrVerifierIndependenceRequiredCode      ErrorCode = "ErrVerifierIndependenceRequired"
	ErrFallbackWouldWeakenPolicyCode         ErrorCode = "ErrFallbackWouldWeakenPolicy"
	ErrReplanRequiredCode                    ErrorCode = "ErrReplanRequired"
	ErrReplanBoundExceededCode               ErrorCode = "ErrReplanBoundExceeded"
	ErrRoutingFingerprintMismatchCode        ErrorCode = "ErrRoutingFingerprintMismatch"
	ErrInvalidRecordCode                     ErrorCode = ErrorCode(delivery.ErrInvalidRecordCode)
	ErrDuplicateRecordCode                   ErrorCode = ErrorCode(delivery.ErrDuplicateRecordCode)
	ErrMissingReferenceCode                  ErrorCode = ErrorCode(delivery.ErrMissingReferenceCode)
	ErrCrossProjectReferenceCode             ErrorCode = ErrorCode(delivery.ErrCrossProjectReferenceCode)
	ErrUnknownRecordVersionCode              ErrorCode = ErrorCode(delivery.ErrUnknownRecordVersionCode)
)

var (
	ErrRequirementUnknown                = &TypedError{Code: ErrRequirementUnknownCode}
	ErrNoEligibleCandidate               = &TypedError{Code: ErrNoEligibleCandidateCode}
	ErrRequirementConfidenceInsufficient = &TypedError{Code: ErrRequirementConfidenceInsufficientCode}
	ErrCapabilityUnsupported             = &TypedError{Code: ErrCapabilityUnsupportedCode}
	ErrInvalidRecord                     = &TypedError{Code: ErrInvalidRecordCode}
	ErrDuplicateRecord                   = &TypedError{Code: ErrDuplicateRecordCode}
	ErrMissingReference                  = &TypedError{Code: ErrMissingReferenceCode}
	ErrCrossProjectReference             = &TypedError{Code: ErrCrossProjectReferenceCode}
	ErrUnknownRecordVersion              = &TypedError{Code: ErrUnknownRecordVersionCode}
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

type RiskTier string

const (
	RiskLow      RiskTier = "low"
	RiskMedium   RiskTier = "medium"
	RiskHigh     RiskTier = "high"
	RiskCritical RiskTier = "critical"
)

type Permission string

const (
	PermissionReadOnly    Permission = "read-only"
	PermissionWrite       Permission = "write"
	PermissionOrchestrate Permission = "orchestrate"
)

type SideEffectClass string

const (
	SideEffectNone           SideEffectClass = "none"
	SideEffectLocalRead      SideEffectClass = "local-read"
	SideEffectLocalWrite     SideEffectClass = "local-write"
	SideEffectRepoWrite      SideEffectClass = "repo-write"
	SideEffectGitRemoteWrite SideEffectClass = "git-remote-write"
	SideEffectGitHubWrite    SideEffectClass = "github-write"
	SideEffectProviderLaunch SideEffectClass = "provider-launch"
	SideEffectExternalWrite  SideEffectClass = "external-write"
)

type OutputContract string

const (
	OutputFreeform            OutputContract = "freeform"
	OutputMarkdown            OutputContract = "markdown"
	OutputJSON                OutputContract = "json"
	OutputJSONSchema          OutputContract = "json-schema"
	OutputPatch               OutputContract = "patch"
	OutputBranch              OutputContract = "branch"
	OutputPR                  OutputContract = "pr"
	OutputReport              OutputContract = "report"
	OutputVerificationVerdict OutputContract = "verification-verdict"
)

type NetworkRequirement string

const (
	NetworkNotNeeded              NetworkRequirement = "not-needed"
	NetworkAllowedIfProfileGrants NetworkRequirement = "allowed-if-profile-grants"
	NetworkRequired               NetworkRequirement = "required"
)

type QualityFloor string

const (
	QualityStandard    QualityFloor = "standard"
	QualityStrong      QualityFloor = "strong"
	QualityAdversarial QualityFloor = "adversarial"
)

type VerificationKind string

const (
	VerificationNone           VerificationKind = "none"
	VerificationSelfCheck      VerificationKind = "self-check"
	VerificationLocalCommand   VerificationKind = "local-command"
	VerificationHostedCheck    VerificationKind = "hosted-check"
	VerificationLoopReview     VerificationKind = "loopreview"
	VerificationSecurityReview VerificationKind = "security-review"
	VerificationHumanApproval  VerificationKind = "human-approval"
	VerificationOverride       VerificationKind = "override"
)

type IndependenceLevel string

const (
	IndependenceNone              IndependenceLevel = "none"
	IndependenceDifferentModel    IndependenceLevel = "different-model"
	IndependenceDifferentAccount  IndependenceLevel = "different-account"
	IndependenceDifferentProvider IndependenceLevel = "different-provider"
	IndependenceHuman             IndependenceLevel = "human"
)

type CapabilityDimension string

const (
	CapabilityRolesSupported      CapabilityDimension = "roles_supported"
	CapabilityReadOnly            CapabilityDimension = "read_only"
	CapabilityJSONOutput          CapabilityDimension = "json_output"
	CapabilityNestedSubagents     CapabilityDimension = "nested_subagents"
	CapabilityMCPConfig           CapabilityDimension = "mcp_config"
	CapabilityCancellation        CapabilityDimension = "cancellation"
	CapabilityTokenUsageReporting CapabilityDimension = "token_usage_reporting"
	CapabilityContextWindowTokens CapabilityDimension = "context_window_tokens"
	CapabilityToolSupport         CapabilityDimension = "tool_support"
	CapabilityImageInput          CapabilityDimension = "image_input"
	CapabilityImageOutput         CapabilityDimension = "image_output"
)

type CapabilityRequirement struct {
	Dimension         CapabilityDimension              `json:"dimension"`
	RequiredValue     any                              `json:"required_value"`
	MinimumConfidence providerinventory.Confidence     `json:"minimum_confidence"`
	FreshnessRequired providerinventory.FreshnessState `json:"freshness_required"`
	Source            string                           `json:"source"`
}

type VerificationRequirement struct {
	VerificationKind     VerificationKind  `json:"verification_kind"`
	RequiredForRiskTiers []RiskTier        `json:"required_for_risk_tiers"`
	IndependenceLevel    IndependenceLevel `json:"independence_level"`
	PermissionRequired   Permission        `json:"permission_required"`
	OutputContract       OutputContract    `json:"output_contract"`
	Source               string            `json:"source"`
}

type HeuristicOutput struct {
	RuleID     string                       `json:"rule_id"`
	Reason     string                       `json:"reason"`
	Confidence providerinventory.Confidence `json:"confidence"`
}

type SourceDescriptor struct {
	SourceKind        string   `json:"source_kind"`
	SourceReference   string   `json:"source_reference"`
	InputFingerprints []string `json:"input_fingerprints,omitempty"`
	RecordIDs         []string `json:"record_ids,omitempty"`
}

type TaskRequirement struct {
	SchemaVersion              string                       `json:"schema_version"`
	RecordVersion              int                          `json:"record_version"`
	TaskRequirementID          string                       `json:"task_requirement_id"`
	TaskRequirementFingerprint string                       `json:"task_requirement_fingerprint"`
	ProjectID                  string                       `json:"project_id"`
	DeliveryRunID              string                       `json:"delivery_run_id"`
	TaskID                     string                       `json:"task_id"`
	TaskKey                    string                       `json:"task_key"`
	RoleKey                    string                       `json:"role_key"`
	RiskTier                   RiskTier                     `json:"risk_tier"`
	PermissionRequired         Permission                   `json:"permission_required"`
	SideEffectClass            SideEffectClass              `json:"side_effect_class"`
	RequiredCapabilities       []CapabilityRequirement      `json:"required_capabilities"`
	RequiredOutput             OutputContract               `json:"required_output"`
	VerificationRequirements   []VerificationRequirement    `json:"verification_requirements"`
	ScopeJSON                  string                       `json:"scope_json"`
	DataClassification         string                       `json:"data_classification"`
	NetworkRequired            NetworkRequirement           `json:"network_required"`
	NestedAllowed              bool                         `json:"nested_allowed"`
	CancellationRequired       bool                         `json:"cancellation_required"`
	QualityFloor               QualityFloor                 `json:"quality_floor"`
	ClassificationRules        []string                     `json:"classification_rules"`
	Heuristics                 []HeuristicOutput            `json:"heuristics"`
	ProvenanceSummary          string                       `json:"provenance_summary"`
	PolicyVersion              string                       `json:"policy_version"`
	PlanFingerprint            string                       `json:"plan_fingerprint"`
	CreatedAt                  string                       `json:"created_at"`
	UpdatedAt                  string                       `json:"updated_at"`
	CreatedBy                  delivery.Actor               `json:"created_by"`
	UpdatedBy                  delivery.Actor               `json:"updated_by"`
	Host                       delivery.Host                `json:"host"`
	Classification             string                       `json:"classification"`
	Confidence                 providerinventory.Confidence `json:"confidence"`
	Source                     SourceDescriptor             `json:"source"`
	Heuristic                  bool                         `json:"heuristic"`
	HeuristicReason            string                       `json:"heuristic_reason,omitempty"`
	TerminalErrorCode          string                       `json:"terminal_error_code,omitempty"`
	GapReasons                 []string                     `json:"gap_reasons"`
}

type RequirementOverride struct {
	SchemaVersion                string                       `json:"schema_version"`
	RecordVersion                int                          `json:"record_version"`
	RequirementOverrideID        string                       `json:"requirement_override_id"`
	ProjectID                    string                       `json:"project_id"`
	DeliveryRunID                string                       `json:"delivery_run_id"`
	TaskID                       string                       `json:"task_id,omitempty"`
	TaskKey                      string                       `json:"task_key,omitempty"`
	PlanFingerprint              string                       `json:"plan_fingerprint,omitempty"`
	ScopeKey                     string                       `json:"scope_key"`
	Field                        string                       `json:"field"`
	ValueJSON                    string                       `json:"value_json"`
	Justification                string                       `json:"justification"`
	Status                       string                       `json:"status"`
	ValidationStatus             string                       `json:"validation_status"`
	ValidationErrorCode          string                       `json:"validation_error_code,omitempty"`
	InputRequirementFingerprint  string                       `json:"input_requirement_fingerprint,omitempty"`
	OutputRequirementFingerprint string                       `json:"output_requirement_fingerprint,omitempty"`
	CreatedAt                    string                       `json:"created_at"`
	UpdatedAt                    string                       `json:"updated_at"`
	CreatedBy                    delivery.Actor               `json:"created_by"`
	UpdatedBy                    delivery.Actor               `json:"updated_by"`
	Host                         delivery.Host                `json:"host"`
	PolicyVersion                string                       `json:"policy_version"`
	Classification               string                       `json:"classification"`
	SideEffectClass              SideEffectClass              `json:"side_effect_class"`
	Confidence                   providerinventory.Confidence `json:"confidence"`
	Source                       SourceDescriptor             `json:"source"`
	TerminalErrorCode            string                       `json:"terminal_error_code,omitempty"`
	GapReasons                   []string                     `json:"gap_reasons"`
}

type CapabilityEvidence struct {
	Value      any                              `json:"value"`
	Confidence providerinventory.Confidence     `json:"confidence"`
	Freshness  providerinventory.FreshnessState `json:"freshness"`
}

type CapabilityEvidenceSet map[CapabilityDimension]CapabilityEvidence

func (req CapabilityRequirement) SatisfiedBy(evidence CapabilityEvidenceSet) bool {
	fact, ok := evidence[req.Dimension]
	if !ok {
		return false
	}
	if fact.Freshness == providerinventory.FreshnessStale || fact.Freshness == providerinventory.FreshnessExpired {
		return false
	}
	if req.FreshnessRequired == providerinventory.FreshnessFresh && fact.Freshness != providerinventory.FreshnessFresh {
		return false
	}
	if fact.Confidence == providerinventory.ConfidenceUnknown || fact.Confidence == providerinventory.ConfidenceUnavailable || fact.Confidence == providerinventory.ConfidenceStale {
		return false
	}
	if req.MinimumConfidence == providerinventory.ConfidenceExact && fact.Confidence != providerinventory.ConfidenceExact {
		return false
	}
	return requiredValueSatisfied(req.RequiredValue, fact.Value)
}

func HardRequirementsSatisfied(requirements []CapabilityRequirement, evidence CapabilityEvidenceSet) bool {
	return CheckHardRequirementsSatisfied(requirements, evidence) == nil
}

func CheckHardRequirementsSatisfied(requirements []CapabilityRequirement, evidence CapabilityEvidenceSet) error {
	for _, req := range requirements {
		if !req.SatisfiedBy(evidence) {
			return typed(ErrCapabilityUnsupportedCode, "hard capability requirement %s is not satisfied", req.Dimension)
		}
	}
	return nil
}

func requiredValueSatisfied(required, actual any) bool {
	switch want := required.(type) {
	case bool:
		got, ok := actual.(bool)
		return ok && got == want
	case string:
		got, ok := actual.(string)
		return ok && got == want
	case []string:
		switch got := actual.(type) {
		case []string:
			for _, value := range want {
				if !contains(got, value) {
					return false
				}
			}
			return true
		case string:
			return len(want) == 1 && want[0] == got
		default:
			return false
		}
	case int:
		return numericAtLeast(actual, int64(want))
	case int64:
		return numericAtLeast(actual, want)
	case float64:
		return numericAtLeast(actual, int64(want))
	default:
		return false
	}
}

func numericAtLeast(actual any, want int64) bool {
	switch got := actual.(type) {
	case int:
		return int64(got) >= want
	case int64:
		return got >= want
	case float64:
		return int64(got) >= want
	default:
		return false
	}
}

func TaskRequirementID(projectID, deliveryRunID, taskID, planFingerprint string) string {
	return "treq_" + digestBase32(projectID, deliveryRunID, taskID, planFingerprint)
}

func RequirementOverrideID(projectID, deliveryRunID, scopeKey, field, valueJSON string) string {
	return "treqovr_" + digestBase32(projectID, deliveryRunID, scopeKey, field, valueJSON)
}

func FingerprintRequirement(req TaskRequirement) (string, error) {
	copy := req
	copy.TaskRequirementFingerprint = ""
	data, err := delivery.CanonicalJSON(copy)
	if err != nil {
		return "", err
	}
	return delivery.SHA256Digest(data), nil
}

func canonicalTimestamp(t time.Time) string {
	return delivery.CanonicalTimestamp(t.UTC())
}

func digestBase32(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum.Sum(nil)))
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (r *RiskTier) UnmarshalJSON(data []byte) error {
	value, err := enumString(data, "risk_tier")
	if err != nil {
		return err
	}
	switch RiskTier(value) {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		*r = RiskTier(value)
		return nil
	default:
		return typed(ErrInvalidRecordCode, "unknown risk_tier %q", value)
	}
}

func (p *Permission) UnmarshalJSON(data []byte) error {
	value, err := enumString(data, "permission_required")
	if err != nil {
		return err
	}
	switch Permission(value) {
	case PermissionReadOnly, PermissionWrite, PermissionOrchestrate:
		*p = Permission(value)
		return nil
	default:
		return typed(ErrInvalidRecordCode, "unknown permission_required %q", value)
	}
}

func (s *SideEffectClass) UnmarshalJSON(data []byte) error {
	value, err := enumString(data, "side_effect_class")
	if err != nil {
		return err
	}
	if _, ok := sideEffectRanks[SideEffectClass(value)]; !ok {
		return typed(ErrInvalidRecordCode, "unknown side_effect_class %q", value)
	}
	*s = SideEffectClass(value)
	return nil
}

func enumString(data []byte, label string) (string, error) {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return "", typed(ErrInvalidRecordCode, "%s must be a string", label)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", typed(ErrInvalidRecordCode, "%s is required", label)
	}
	return value, nil
}

var sideEffectRanks = map[SideEffectClass]int{
	SideEffectNone:           0,
	SideEffectLocalRead:      1,
	SideEffectLocalWrite:     2,
	SideEffectRepoWrite:      3,
	SideEffectGitRemoteWrite: 4,
	SideEffectGitHubWrite:    5,
	SideEffectProviderLaunch: 6,
	SideEffectExternalWrite:  7,
}

var riskRanks = map[RiskTier]int{
	RiskLow:      0,
	RiskMedium:   1,
	RiskHigh:     2,
	RiskCritical: 3,
}

var permissionRanks = map[Permission]int{
	PermissionReadOnly:    0,
	PermissionWrite:       1,
	PermissionOrchestrate: 2,
}

var outputContracts = map[OutputContract]bool{
	OutputFreeform:            true,
	OutputMarkdown:            true,
	OutputJSON:                true,
	OutputJSONSchema:          true,
	OutputPatch:               true,
	OutputBranch:              true,
	OutputPR:                  true,
	OutputReport:              true,
	OutputVerificationVerdict: true,
}

var networkRequirements = map[NetworkRequirement]bool{
	NetworkNotNeeded:              true,
	NetworkAllowedIfProfileGrants: true,
	NetworkRequired:               true,
}

var qualityFloors = map[QualityFloor]bool{
	QualityStandard:    true,
	QualityStrong:      true,
	QualityAdversarial: true,
}

var verificationKinds = map[VerificationKind]bool{
	VerificationNone:           true,
	VerificationSelfCheck:      true,
	VerificationLocalCommand:   true,
	VerificationHostedCheck:    true,
	VerificationLoopReview:     true,
	VerificationSecurityReview: true,
	VerificationHumanApproval:  true,
	VerificationOverride:       true,
}

var independenceLevels = map[IndependenceLevel]bool{
	IndependenceNone:              true,
	IndependenceDifferentModel:    true,
	IndependenceDifferentAccount:  true,
	IndependenceDifferentProvider: true,
	IndependenceHuman:             true,
}

var capabilityDimensions = map[CapabilityDimension]bool{
	CapabilityRolesSupported:      true,
	CapabilityReadOnly:            true,
	CapabilityJSONOutput:          true,
	CapabilityNestedSubagents:     true,
	CapabilityMCPConfig:           true,
	CapabilityCancellation:        true,
	CapabilityTokenUsageReporting: true,
	CapabilityContextWindowTokens: true,
	CapabilityToolSupport:         true,
	CapabilityImageInput:          true,
	CapabilityImageOutput:         true,
}
