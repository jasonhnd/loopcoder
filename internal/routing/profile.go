package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/planner"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	RoutingPolicyProfileSchema  = "loopcoder.routing_policy_profile.v1"
	PolicyInputSchema           = "loopcoder.routing_policy_input.v1"
	LegacyModelMappingSchema    = "loopcoder.routing_legacy_model_mapping.v1"
	RoutingPolicyVersion        = "0804.routing-policy.v1"
	builtInProfileEffectiveFrom = "1970-01-01T00:00:00Z"

	ProfileKeyFast     = "fast-v1"
	ProfileKeyBalanced = "balanced-v1"
	ProfileKeyDeep     = "deep-v1"

	PolicyInputKindPin       = "pin"
	PolicyInputKindExclusion = "exclusion"

	PolicyInputStatusActive = "active"

	ValidationStatusValid   = "valid"
	ValidationStatusInvalid = "invalid"
	ValidationStatusStale   = "stale"
)

type RoutingPolicyProfile struct {
	SchemaVersion          string                                                           `json:"schema_version"`
	RecordVersion          int                                                              `json:"record_version"`
	RoutingPolicyProfileID string                                                           `json:"routing_policy_profile_id"`
	ProfileKey             string                                                           `json:"profile_key"`
	ProfileVersion         string                                                           `json:"profile_version"`
	PolicyVersion          string                                                           `json:"policy_version"`
	GraphBounds            planner.GraphBounds                                              `json:"graph_bounds"`
	RoleDefinitionIDs      []string                                                         `json:"role_definition_ids"`
	RiskPolicy             map[taskrequirements.RiskTier]RiskPolicy                         `json:"risk_policy"`
	EligibilityPolicy      Policy                                                           `json:"eligibility_policy"`
	OptimizationPolicy     OptimizationPolicy                                               `json:"optimization_policy"`
	FallbackPolicy         FallbackPolicy                                                   `json:"fallback_policy"`
	ReplanPolicy           ReplanPolicy                                                     `json:"replan_policy"`
	UserPinPolicy          UserPinPolicy                                                    `json:"user_pin_policy"`
	BudgetSettings         BudgetSettings                                                   `json:"budget_settings"`
	VerifierSeparation     map[taskrequirements.RiskTier]taskrequirements.IndependenceLevel `json:"verifier_separation"`
	Provenance             ProfileProvenance                                                `json:"provenance"`
	EffectiveFrom          string                                                           `json:"effective_from"`
	SupersedesProfileID    string                                                           `json:"supersedes_profile_id,omitempty"`
	PolicyFingerprint      string                                                           `json:"policy_fingerprint"`
}

type RiskPolicy struct {
	MinimumVerification taskrequirements.VerificationKind  `json:"minimum_verification"`
	ApprovalRequired    bool                               `json:"approval_required"`
	IndependenceLevel   taskrequirements.IndependenceLevel `json:"independence_level"`
	QualityFloor        taskrequirements.QualityFloor      `json:"quality_floor"`
	PermissionCeiling   taskrequirements.Permission        `json:"permission_ceiling"`
	SideEffectCeiling   taskrequirements.SideEffectClass   `json:"side_effect_ceiling"`
	QuotaConfidence     providerinventory.Confidence       `json:"quota_confidence"`
	BudgetOverride      string                             `json:"budget_override"`
	VerificationOutput  taskrequirements.OutputContract    `json:"verification_output"`
	RequiredRoles       []string                           `json:"required_roles"`
}

type FallbackPolicy struct {
	PolicyVersion           string `json:"policy_version"`
	MaxFallbacks            int    `json:"max_fallbacks"`
	NeverWeakenPermission   bool   `json:"never_weaken_permission"`
	NeverWeakenScope        bool   `json:"never_weaken_scope"`
	NeverWeakenSideEffects  bool   `json:"never_weaken_side_effects"`
	NeverWeakenVerification bool   `json:"never_weaken_verification"`
	NeverIgnoreUserPin      bool   `json:"never_ignore_user_pin"`
	RequireFreshEligibility bool   `json:"require_fresh_eligibility"`
}

type ReplanPolicy struct {
	PolicyVersion                  string `json:"policy_version"`
	MaxReplanPasses                int    `json:"max_replan_passes"`
	RequireNewFingerprint          bool   `json:"require_new_fingerprint"`
	RequireFreshApproval           bool   `json:"require_fresh_approval"`
	GraphExpansionRequiresApproval bool   `json:"graph_expansion_requires_approval"`
}

type UserPinPolicy struct {
	PolicyVersion            string `json:"policy_version"`
	RequireActorProvenance   bool   `json:"require_actor_provenance"`
	RequireReason            bool   `json:"require_reason"`
	RequireScope             bool   `json:"require_scope"`
	RequirePolicyFingerprint bool   `json:"require_policy_fingerprint"`
	RejectStaleInventory     bool   `json:"reject_stale_inventory"`
	NarrowsCandidatesOnly    bool   `json:"narrows_candidates_only"`
}

type BudgetSettings struct {
	RequireBudgetEvidence bool                         `json:"require_budget_evidence"`
	RequireExactQuota     bool                         `json:"require_exact_quota"`
	AllowPaidOverage      bool                         `json:"allow_paid_overage"`
	MinimumConfidence     providerinventory.Confidence `json:"minimum_confidence"`
}

type ProfileProvenance struct {
	SourceKind      string `json:"source_kind"`
	SourceReference string `json:"source_reference"`
	SourceHash      string `json:"source_hash"`
	CreatedBy       string `json:"created_by"`
}

type PolicyInputRecord struct {
	SchemaVersion          string              `json:"schema_version"`
	RecordVersion          int                 `json:"record_version"`
	RoutingPolicyInputID   string              `json:"routing_policy_input_id"`
	InputKind              string              `json:"input_kind"`
	ProjectID              string              `json:"project_id"`
	DeliveryRunID          string              `json:"delivery_run_id,omitempty"`
	RoutingPolicyProfileID string              `json:"routing_policy_profile_id"`
	PolicyFingerprint      string              `json:"policy_fingerprint"`
	Scope                  string              `json:"scope"`
	DecisionKey            string              `json:"decision_key,omitempty"`
	Reason                 string              `json:"reason"`
	Status                 string              `json:"status"`
	ExpiresAt              string              `json:"expires_at,omitempty"`
	Constraint             CandidateConstraint `json:"constraint"`
	ValidationStatus       string              `json:"validation_status"`
	Diagnostics            []PolicyDiagnostic  `json:"diagnostics"`
	CreatedAt              string              `json:"created_at"`
	UpdatedAt              string              `json:"updated_at"`
	Actor                  delivery.Actor      `json:"actor"`
	Host                   delivery.Host       `json:"host"`
}

type CandidateConstraint struct {
	AdapterID              string `json:"adapter_id,omitempty"`
	ProviderInstallationID string `json:"provider_installation_id,omitempty"`
	AccountProfileID       string `json:"account_profile_id,omitempty"`
	ModelCapabilityID      string `json:"model_capability_id,omitempty"`
	InvocationProfileKey   string `json:"invocation_profile_key,omitempty"`
}

type PolicyDiagnostic struct {
	Code              taskrequirements.ErrorCode `json:"code"`
	Kind              string                     `json:"kind"`
	Field             string                     `json:"field"`
	Value             string                     `json:"value"`
	Message           string                     `json:"message"`
	EvidenceRecordIDs []string                   `json:"evidence_record_ids"`
	Action            string                     `json:"action"`
}

type OverrideProvenance struct {
	OverrideID               string              `json:"override_id"`
	OverrideKind             string              `json:"override_kind"`
	Reason                   string              `json:"reason"`
	Scope                    string              `json:"scope"`
	ExpiresAt                string              `json:"expires_at,omitempty"`
	TaskID                   string              `json:"task_id,omitempty"`
	DeliveryRunID            string              `json:"delivery_run_id,omitempty"`
	CandidateConstraint      CandidateConstraint `json:"candidate_constraint,omitempty"`
	ManualUnavailableUntil   string              `json:"manual_unavailable_until,omitempty"`
	ManualResetAt            string              `json:"manual_reset_at,omitempty"`
	ClearManualReset         bool                `json:"clear_manual_reset,omitempty"`
	Actor                    delivery.Actor      `json:"actor"`
	Host                     delivery.Host       `json:"host"`
	PolicyFingerprint        string              `json:"policy_fingerprint"`
	AuthorizationFingerprint string              `json:"authorization_fingerprint"`
	Source                   string              `json:"source"`
}

type LegacyModelMapping struct {
	SchemaVersion           string                     `json:"schema_version"`
	RecordVersion           int                        `json:"record_version"`
	LegacyModelMappingID    string                     `json:"legacy_model_mapping_id"`
	ProjectID               string                     `json:"project_id"`
	RoleKey                 string                     `json:"role_key"`
	LegacyProvider          string                     `json:"legacy_provider"`
	LegacyModel             string                     `json:"legacy_model"`
	LegacyReasoningEffort   string                     `json:"legacy_reasoning_effort,omitempty"`
	MappingStatus           string                     `json:"mapping_status"`
	MappedAdapterID         string                     `json:"mapped_adapter_id,omitempty"`
	MappedModelCapabilityID string                     `json:"mapped_model_capability_id,omitempty"`
	DiagnosticCode          taskrequirements.ErrorCode `json:"diagnostic_code,omitempty"`
	Diagnostic              string                     `json:"diagnostic,omitempty"`
	PolicyFingerprint       string                     `json:"policy_fingerprint,omitempty"`
	CreatedAt               string                     `json:"created_at"`
	UpdatedAt               string                     `json:"updated_at"`
}

func BuiltInRoutingPolicyProfiles(_ time.Time) []RoutingPolicyProfile {
	effectiveAt := time.Unix(0, 0).UTC()
	return []RoutingPolicyProfile{
		normalizeRoutingPolicyProfile(profileTemplate(ProfileKeyFast, effectiveAt, map[ComponentName]int{
			ComponentAvailability: 35, ComponentQuotaHeadroom: 20, ComponentQualityFit: 15, ComponentLatency: 20, ComponentCost: 10,
		}, planner.GraphBounds{
			MaxTasks: 16, MaxDepth: 3, MaxFanOut: 6, MaxDependenciesPerTask: 6, MaxParallelReady: 4, MaxReplanPasses: 1, MaxGraphValidationMS: 3000, MaxRequirementBytes: 32768, MaxScopePathsPerTask: 64,
		}, taskrequirements.IndependenceDifferentAccount, taskrequirements.QualityStandard)),
		normalizeRoutingPolicyProfile(profileTemplate(ProfileKeyBalanced, effectiveAt, map[ComponentName]int{
			ComponentAvailability: 30, ComponentQuotaHeadroom: 20, ComponentQualityFit: 20, ComponentLatency: 10, ComponentCost: 10, ComponentDiversity: 10,
		}, planner.DefaultGraphBounds(), taskrequirements.IndependenceDifferentProvider, taskrequirements.QualityStrong)),
		normalizeRoutingPolicyProfile(profileTemplate(ProfileKeyDeep, effectiveAt, map[ComponentName]int{
			ComponentAvailability: 15, ComponentQuotaHeadroom: 15, ComponentQualityFit: 35, ComponentLatency: 5, ComponentCost: 10, ComponentDiversity: 20,
		}, planner.HardGraphBounds(), taskrequirements.IndependenceDifferentProvider, taskrequirements.QualityAdversarial)),
	}
}

func BuiltInRoutingPolicyProfile(key string, now time.Time) (RoutingPolicyProfile, bool) {
	key = strings.TrimSpace(key)
	for _, profile := range BuiltInRoutingPolicyProfiles(now) {
		if profile.ProfileKey == key {
			return profile, true
		}
	}
	return RoutingPolicyProfile{}, false
}

func BalancedRoutingPolicyProfile(now time.Time) RoutingPolicyProfile {
	profile, _ := BuiltInRoutingPolicyProfile(ProfileKeyBalanced, now)
	return profile
}

func PersistRoutingPolicyProfile(ctx context.Context, store storage.Store, profile RoutingPolicyProfile) (RoutingPolicyProfile, error) {
	if store == nil {
		return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	profile = normalizeRoutingPolicyProfile(profile)
	if err := ValidateRoutingPolicyProfile(profile); err != nil {
		return RoutingPolicyProfile{}, err
	}
	payload, err := delivery.CanonicalJSON(profile)
	if err != nil {
		return RoutingPolicyProfile{}, err
	}
	provenance, err := delivery.CanonicalJSON(profile.Provenance)
	if err != nil {
		return RoutingPolicyProfile{}, err
	}
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if err := verifyProfileRoleReferences(ctx, tx, profile); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `INSERT INTO routing_policy_profiles(
			routing_policy_profile_id, schema_version, record_version, profile_key, profile_version, policy_version,
			policy_fingerprint, payload_json, provenance_json, effective_from, supersedes_profile_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(routing_policy_profile_id) DO NOTHING`,
			profile.RoutingPolicyProfileID, profile.SchemaVersion, profile.RecordVersion, profile.ProfileKey, profile.ProfileVersion, profile.PolicyVersion,
			profile.PolicyFingerprint, string(payload), string(provenance), profile.EffectiveFrom, profile.SupersedesProfileID, profile.EffectiveFrom)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 0 {
			return err
		}
		var existingPayload string
		if err := tx.QueryRow(ctx, `SELECT payload_json FROM routing_policy_profiles WHERE routing_policy_profile_id = ?`, profile.RoutingPolicyProfileID).Scan(&existingPayload); err != nil {
			return err
		}
		if existingPayload != string(payload) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrDuplicateRecordCode, Message: "routing profile id already exists with different payload"}
		}
		return nil
	})
	if err != nil {
		return RoutingPolicyProfile{}, err
	}
	return LoadRoutingPolicyProfile(ctx, store, profile.RoutingPolicyProfileID)
}

func LoadRoutingPolicyProfile(ctx context.Context, store storage.Store, id string) (RoutingPolicyProfile, error) {
	if store == nil {
		return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM routing_policy_profiles WHERE routing_policy_profile_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "routing policy profile was not persisted"}
		}
		return RoutingPolicyProfile{}, err
	}
	var profile RoutingPolicyProfile
	if err := json.Unmarshal([]byte(payload), &profile); err != nil {
		return RoutingPolicyProfile{}, err
	}
	if err := ValidateRoutingPolicyProfile(profile); err != nil {
		return RoutingPolicyProfile{}, err
	}
	return profile, nil
}

func EnsureBuiltInRoutingPolicyProfiles(ctx context.Context, store storage.Store, now time.Time) ([]RoutingPolicyProfile, error) {
	if _, err := EnsureBuiltInRoleDefinitions(ctx, store, now); err != nil {
		return nil, err
	}
	profiles := BuiltInRoutingPolicyProfiles(now)
	out := make([]RoutingPolicyProfile, 0, len(profiles))
	for _, profile := range profiles {
		stored, err := PersistRoutingPolicyProfile(ctx, store, profile)
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	return out, nil
}

func PersistRoleDefinition(ctx context.Context, store storage.Store, role RoleDefinition, now time.Time) (RoleDefinition, error) {
	if store == nil {
		return RoleDefinition{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	role = normalizeRoleDefinition(role)
	if err := ValidateRoleDefinition(role); err != nil {
		return RoleDefinition{}, err
	}
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	payload, err := delivery.CanonicalJSON(role)
	if err != nil {
		return RoleDefinition{}, err
	}
	fingerprint := delivery.SHA256Digest(payload)
	provenance, err := delivery.CanonicalJSON(map[string]string{
		"source_kind":      "built-in",
		"source_reference": "docs/specs/0804-decomposition-routing.md",
		"source_hash":      fingerprint,
	})
	if err != nil {
		return RoleDefinition{}, err
	}
	createdAt := delivery.CanonicalTimestamp(now.UTC())
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO role_definitions(
			role_definition_id, schema_version, record_version, role_key, role_version, policy_version,
			payload_json, payload_fingerprint, provenance_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(role_definition_id) DO NOTHING`,
			role.RoleDefinitionID, role.SchemaVersion, role.RecordVersion, role.RoleKey, role.RoleVersion, role.PolicyVersion,
			string(payload), fingerprint, string(provenance), createdAt)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 0 {
			return err
		}
		var existing string
		if err := tx.QueryRow(ctx, `SELECT payload_json FROM role_definitions WHERE role_definition_id = ?`, role.RoleDefinitionID).Scan(&existing); err != nil {
			return err
		}
		if existing != string(payload) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrDuplicateRecordCode, Message: "role definition id already exists with different payload"}
		}
		return nil
	})
	if err != nil {
		return RoleDefinition{}, err
	}
	return LoadRoleDefinition(ctx, store, role.RoleDefinitionID)
}

func LoadRoleDefinition(ctx context.Context, store storage.Store, id string) (RoleDefinition, error) {
	if store == nil {
		return RoleDefinition{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM role_definitions WHERE role_definition_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RoleDefinition{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "role definition was not persisted"}
		}
		return RoleDefinition{}, err
	}
	var role RoleDefinition
	if err := json.Unmarshal([]byte(payload), &role); err != nil {
		return RoleDefinition{}, err
	}
	if err := ValidateRoleDefinition(role); err != nil {
		return RoleDefinition{}, err
	}
	return role, nil
}

func EnsureBuiltInRoleDefinitions(ctx context.Context, store storage.Store, now time.Time) ([]RoleDefinition, error) {
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	roles := BuiltInRoleDefinitions()
	out := make([]RoleDefinition, 0, len(roles))
	for _, role := range roles {
		stored, err := PersistRoleDefinition(ctx, store, role, now)
		if err != nil {
			return nil, err
		}
		out = append(out, stored)
	}
	return out, nil
}

func PersistPolicyInput(ctx context.Context, store storage.Store, record PolicyInputRecord) (PolicyInputRecord, error) {
	if store == nil {
		return PolicyInputRecord{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	record = normalizePolicyInput(record, store.Now())
	if err := validatePolicyInput(record); err != nil {
		return PolicyInputRecord{}, err
	}
	payload, err := delivery.CanonicalJSON(record)
	if err != nil {
		return PolicyInputRecord{}, err
	}
	constraint, err := delivery.CanonicalJSON(record.Constraint)
	if err != nil {
		return PolicyInputRecord{}, err
	}
	diagnostics, err := delivery.CanonicalJSON(record.Diagnostics)
	if err != nil {
		return PolicyInputRecord{}, err
	}
	actor, err := delivery.CanonicalJSON(record.Actor)
	if err != nil {
		return PolicyInputRecord{}, err
	}
	host, err := delivery.CanonicalJSON(record.Host)
	if err != nil {
		return PolicyInputRecord{}, err
	}
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO routing_policy_inputs(
			routing_policy_input_id, schema_version, record_version, input_kind, project_id, delivery_run_id,
			routing_policy_profile_id, policy_fingerprint, scope, reason, status, expires_at, constraint_json,
			validation_status, diagnostics_json, created_at, updated_at, actor_json, host_json, payload_json
		) VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(routing_policy_input_id) DO NOTHING`,
			record.RoutingPolicyInputID, record.SchemaVersion, record.RecordVersion, record.InputKind, record.ProjectID, record.DeliveryRunID,
			record.RoutingPolicyProfileID, record.PolicyFingerprint, record.Scope, record.Reason, record.Status, record.ExpiresAt, string(constraint),
			record.ValidationStatus, string(diagnostics), record.CreatedAt, record.UpdatedAt, string(actor), string(host), string(payload))
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 0 {
			return err
		}
		var existing string
		if err := tx.QueryRow(ctx, `SELECT payload_json FROM routing_policy_inputs WHERE routing_policy_input_id = ?`, record.RoutingPolicyInputID).Scan(&existing); err != nil {
			return err
		}
		if existing != string(payload) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrDuplicateRecordCode, Message: "routing policy input id already exists with different payload"}
		}
		return nil
	})
	if err != nil {
		return PolicyInputRecord{}, err
	}
	return LoadPolicyInput(ctx, store, record.RoutingPolicyInputID)
}

func LoadPolicyInput(ctx context.Context, store storage.Store, id string) (PolicyInputRecord, error) {
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM routing_policy_inputs WHERE routing_policy_input_id = ?`, strings.TrimSpace(id)).Scan(&payload)
	})
	if err != nil {
		return PolicyInputRecord{}, err
	}
	var record PolicyInputRecord
	if err := json.Unmarshal([]byte(payload), &record); err != nil {
		return PolicyInputRecord{}, err
	}
	return record, nil
}

func LoadActivePolicyInputs(ctx context.Context, store storage.Store, projectID, deliveryRunID, routingPolicyProfileID string) ([]PolicyInputRecord, error) {
	if store == nil {
		return nil, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	var payloads []string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload_json FROM routing_policy_inputs
			WHERE project_id = ? AND routing_policy_profile_id = ? AND status = ?
				AND (delivery_run_id = ? OR delivery_run_id IS NULL)
			ORDER BY routing_policy_input_id`,
			strings.TrimSpace(projectID), strings.TrimSpace(routingPolicyProfileID), PolicyInputStatusActive, strings.TrimSpace(deliveryRunID))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			payloads = append(payloads, payload)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := make([]PolicyInputRecord, 0, len(payloads))
	for _, payload := range payloads {
		var record PolicyInputRecord
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

func ValidatePolicyInputs(inventory providerinventory.Report, pins []Pin, exclusions []Exclusion, policyFingerprint string, now time.Time) []PolicyDiagnostic {
	var diagnostics []PolicyDiagnostic
	for _, pin := range pins {
		diagnostics = append(diagnostics, validateConstraint(PolicyInputKindPin, pin.PinID, constraintFromPin(pin), inventory, policyFingerprint, now)...)
	}
	for _, exclusion := range exclusions {
		diagnostics = append(diagnostics, validateConstraint(PolicyInputKindExclusion, exclusion.ExclusionID, constraintFromExclusion(exclusion), inventory, policyFingerprint, now)...)
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func ValidateOverrideProvenance(overrides []OverrideProvenance, now time.Time, activeBindings ...string) []PolicyDiagnostic {
	var diagnostics []PolicyDiagnostic
	activePolicyFingerprint := ""
	activeAuthorizationFingerprint := ""
	if len(activeBindings) > 0 {
		activePolicyFingerprint = strings.TrimSpace(activeBindings[0])
	}
	if len(activeBindings) > 1 {
		activeAuthorizationFingerprint = strings.TrimSpace(activeBindings[1])
	}
	for _, override := range overrides {
		id := strings.TrimSpace(override.OverrideID)
		if strings.TrimSpace(override.Reason) == "" {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "reason", id, "override requires explicit reason", nil, "record a bounded override reason"))
		}
		if strings.TrimSpace(override.Scope) == "" {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "scope", id, "override requires explicit scope", nil, "record the exact override scope"))
		}
		if strings.TrimSpace(override.PolicyFingerprint) == "" || !strings.HasPrefix(override.PolicyFingerprint, "sha256:") {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrorCode(delivery.ErrStaleApprovalCode), "stale", "policy_fingerprint", id, "override must bind to the active policy fingerprint", nil, "re-approve against the current policy fingerprint"))
		} else if activePolicyFingerprint != "" && override.PolicyFingerprint != activePolicyFingerprint {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrRoutingFingerprintMismatchCode, "stale", "policy_fingerprint", id, "override policy fingerprint does not match the active routing policy fingerprint", nil, "re-approve against the current policy fingerprint"))
		}
		if strings.TrimSpace(override.AuthorizationFingerprint) == "" || !strings.HasPrefix(override.AuthorizationFingerprint, "sha256:") {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrorCode(delivery.ErrStaleApprovalCode), "stale", "authorization_fingerprint", id, "override must bind to the current authorization fingerprint", nil, "re-approve against the current authorization binding"))
		} else if activeAuthorizationFingerprint == "" || override.AuthorizationFingerprint != activeAuthorizationFingerprint {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrRoutingFingerprintMismatchCode, "stale", "authorization_fingerprint", id, "override authorization fingerprint does not match the current authorization binding", nil, "re-approve against the current authorization binding"))
		}
		if override.Actor.ActorID == "" || override.Actor.ActorKind != "user" {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "actor", id, "override requires a user actor", nil, "record the user actor that granted the override"))
		}
		if override.Host.HostID == "" {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "host", id, "override requires host provenance", nil, "record host provenance"))
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(override.ExpiresAt))
		if strings.TrimSpace(override.ExpiresAt) == "" || err != nil || (!now.IsZero() && !expiresAt.After(now.UTC())) {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrorCode(delivery.ErrExpiredApprovalCode), "stale", "expires_at", id, "override requires a future expiry", nil, "record a fresh override with a future expiry"))
		}
		if strings.TrimSpace(override.TaskID) == "" && strings.TrimSpace(override.DeliveryRunID) == "" {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "scope", id, "override requires an explicit structured task_id or delivery_run_id binding", nil, "bind the override to an exact task or delivery run"))
		} else if strings.TrimSpace(override.Scope) != canonicalManualOverrideScope(override) {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "scope", id, "override scope must exactly match structured task/run binding", nil, "use the canonical structured scope representation"))
		}
		if isHardGateOverride(override.OverrideKind, override.Scope) {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrorCode(delivery.ErrPolicyDeniedCode), "invalid", "override_kind", id, "override cannot weaken hard eligibility, permission, side-effect, scope, approval, independence, budget, or release gates", nil, "change the plan or choose an eligible route instead"))
		}
		if !supportedBoundedOverride(override.OverrideKind, override.Scope) {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "override_kind", id, "unsupported routing override kind or scope", nil, "use a bounded routing, fallback, replan, or budget preference override"))
		}
		switch strings.ToLower(strings.TrimSpace(override.OverrideKind)) {
		case "manual-unavailable-until":
			if emptyCandidateConstraint(override.CandidateConstraint) {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "candidate_constraint", id, "manual unavailable override requires an explicit candidate constraint", nil, "bind the override to adapter/account/model evidence"))
			}
			if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(override.ManualUnavailableUntil)); strings.TrimSpace(override.ManualUnavailableUntil) == "" || err != nil {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "manual_unavailable_until", id, "manual unavailable override requires a bounded unavailable-until timestamp", nil, "record a bounded unavailable-until timestamp"))
			}
		case "manual-reset":
			if emptyCandidateConstraint(override.CandidateConstraint) {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "candidate_constraint", id, "manual reset override requires an explicit candidate constraint", nil, "bind the override to adapter/account/model evidence"))
			}
			resetAt := strings.TrimSpace(override.ManualResetAt)
			if !override.ClearManualReset {
				parsed, err := time.Parse(time.RFC3339Nano, resetAt)
				if resetAt == "" || err != nil || (!now.IsZero() && !parsed.After(now.UTC())) {
					diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "manual_reset_at", id, "manual reset override requires a future reset timestamp or explicit clear flag", nil, "record explicit bounded reset evidence"))
				}
			}
			if override.ClearManualReset && resetAt != "" {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "manual_reset_at", id, "manual reset override cannot both replace and clear reset evidence", nil, "choose replacement reset evidence or clear the reset assumption"))
			}
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func LegacyModelMappingsFromConfig(projectID string, cfg config.Config, inventory providerinventory.Report, policyFingerprint string, now time.Time) []LegacyModelMapping {
	var out []LegacyModelMapping
	out = append(out, legacyModelMapping(projectID, RoleKeyWorker, cfg.Adapters.Worker, cfg.Worker.Model, cfg.Worker.ReasoningEffort, inventory, policyFingerprint, now))
	out = append(out, legacyModelMapping(projectID, RoleKeyVerifier, cfg.Adapters.Verifier, cfg.Verifier.Model, cfg.Verifier.ReasoningEffort, inventory, policyFingerprint, now))
	filtered := out[:0]
	for _, mapping := range out {
		if mapping.LegacyProvider == "" && mapping.LegacyModel == "" && mapping.LegacyReasoningEffort == "" {
			continue
		}
		filtered = append(filtered, mapping)
	}
	return filtered
}

func PersistLegacyModelMappings(ctx context.Context, store storage.Store, mappings []LegacyModelMapping) ([]LegacyModelMapping, error) {
	if store == nil {
		return nil, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	stored := make([]LegacyModelMapping, 0, len(mappings))
	for _, mapping := range mappings {
		mapping = normalizeLegacyModelMapping(mapping, store.Now())
		payload, err := delivery.CanonicalJSON(mapping)
		if err != nil {
			return nil, err
		}
		err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO routing_legacy_model_mappings(
				legacy_model_mapping_id, schema_version, record_version, project_id, role_key, legacy_provider, legacy_model,
				legacy_reasoning_effort, mapping_status, mapped_adapter_id, mapped_model_capability_id, diagnostic_code,
				diagnostic, policy_fingerprint, payload_json, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, role_key, legacy_provider, legacy_model, legacy_reasoning_effort) DO NOTHING`,
				mapping.LegacyModelMappingID, mapping.SchemaVersion, mapping.RecordVersion, mapping.ProjectID, mapping.RoleKey, mapping.LegacyProvider, mapping.LegacyModel,
				mapping.LegacyReasoningEffort, mapping.MappingStatus, mapping.MappedAdapterID, mapping.MappedModelCapabilityID, string(mapping.DiagnosticCode),
				mapping.Diagnostic, mapping.PolicyFingerprint, string(payload), mapping.CreatedAt, mapping.UpdatedAt)
			return err
		})
		if err != nil {
			return nil, err
		}
		loaded, err := loadLegacyModelMapping(ctx, store, mapping.ProjectID, mapping.RoleKey, mapping.LegacyProvider, mapping.LegacyModel, mapping.LegacyReasoningEffort)
		if err != nil {
			return nil, err
		}
		stored = append(stored, loaded)
	}
	return stored, nil
}

func ValidateRoutingPolicyProfile(profile RoutingPolicyProfile) error {
	if profile.SchemaVersion != RoutingPolicyProfileSchema || profile.RecordVersion != 1 || profile.RoutingPolicyProfileID == "" || profile.ProfileKey == "" || profile.ProfileVersion == "" || profile.PolicyVersion == "" || profile.PolicyFingerprint == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy profile has missing required fields"}
	}
	if want := routingProfileID(profile.ProfileKey, profile.ProfileVersion, profile.PolicyVersion); profile.RoutingPolicyProfileID != want {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy profile id does not match profile key, version, and policy version"}
	}
	if _, err := time.Parse(time.RFC3339Nano, profile.EffectiveFrom); err != nil {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy profile effective_from must be RFC3339"}
	}
	if len(profile.RoleDefinitionIDs) == 0 || len(profile.RiskPolicy) == 0 || len(profile.VerifierSeparation) == 0 {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy profile requires role, risk, and verifier policies"}
	}
	if profile.GraphBounds.MaxTasks <= 0 || profile.GraphBounds.MaxDepth <= 0 || profile.GraphBounds.MaxReplanPasses < 0 {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy profile graph bounds are invalid"}
	}
	if _, err := normalizeOptimizationPolicy(profile.OptimizationPolicy, profile.RoutingPolicyProfileID); err != nil {
		return err
	}
	if profile.EligibilityPolicy.EvidencePolicy == "" || profile.FallbackPolicy.PolicyVersion == "" || profile.ReplanPolicy.PolicyVersion == "" || profile.UserPinPolicy.PolicyVersion == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy profile has missing hard policy fields"}
	}
	if profile.Provenance.SourceKind == "" || profile.Provenance.SourceReference == "" || profile.Provenance.CreatedBy == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy profile provenance is required"}
	}
	if want := profileFingerprint(profile); profile.PolicyFingerprint != want {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "routing policy profile fingerprint does not match canonical payload"}
	}
	return nil
}

func profileTemplate(key string, now time.Time, weights map[ComponentName]int, bounds planner.GraphBounds, highIndependence taskrequirements.IndependenceLevel, highQuality taskrequirements.QualityFloor) RoutingPolicyProfile {
	roleIDs := []string{}
	for _, role := range BuiltInRoleDefinitions() {
		switch role.RoleKey {
		case RoleKeyWorker, RoleKeyVerifier, RoleKeyNestedSubagent:
			roleIDs = append(roleIDs, role.RoleDefinitionID)
		}
	}
	sort.Strings(roleIDs)
	return RoutingPolicyProfile{
		SchemaVersion:     RoutingPolicyProfileSchema,
		RecordVersion:     1,
		ProfileKey:        key,
		ProfileVersion:    "1",
		PolicyVersion:     RoutingPolicyVersion,
		GraphBounds:       bounds,
		RoleDefinitionIDs: roleIDs,
		RiskPolicy: map[taskrequirements.RiskTier]RiskPolicy{
			taskrequirements.RiskLow: {
				MinimumVerification: taskrequirements.VerificationSelfCheck,
				IndependenceLevel:   taskrequirements.IndependenceNone,
				QualityFloor:        taskrequirements.QualityStandard,
				PermissionCeiling:   taskrequirements.PermissionWrite,
				SideEffectCeiling:   taskrequirements.SideEffectLocalRead,
				QuotaConfidence:     providerinventory.ConfidenceEstimated,
				BudgetOverride:      "deny-paid-overage",
				VerificationOutput:  taskrequirements.OutputReport,
				RequiredRoles:       []string{RoleKeyWorker},
			},
			taskrequirements.RiskMedium: {
				MinimumVerification: taskrequirements.VerificationLoopReview,
				IndependenceLevel:   taskrequirements.IndependenceDifferentAccount,
				QualityFloor:        taskrequirements.QualityStandard,
				PermissionCeiling:   taskrequirements.PermissionWrite,
				SideEffectCeiling:   taskrequirements.SideEffectProviderLaunch,
				QuotaConfidence:     providerinventory.ConfidenceEstimated,
				BudgetOverride:      "explicit-only",
				VerificationOutput:  taskrequirements.OutputVerificationVerdict,
				RequiredRoles:       []string{RoleKeyWorker, RoleKeyVerifier},
			},
			taskrequirements.RiskHigh: {
				MinimumVerification: taskrequirements.VerificationLoopReview,
				ApprovalRequired:    true,
				IndependenceLevel:   highIndependence,
				QualityFloor:        highQuality,
				PermissionCeiling:   taskrequirements.PermissionWrite,
				SideEffectCeiling:   taskrequirements.SideEffectGitHubWrite,
				QuotaConfidence:     providerinventory.ConfidenceExact,
				BudgetOverride:      "explicit-with-expiry",
				VerificationOutput:  taskrequirements.OutputVerificationVerdict,
				RequiredRoles:       []string{RoleKeyWorker, RoleKeyVerifier},
			},
			taskrequirements.RiskCritical: {
				MinimumVerification: taskrequirements.VerificationHumanApproval,
				ApprovalRequired:    true,
				IndependenceLevel:   taskrequirements.IndependenceHuman,
				QualityFloor:        taskrequirements.QualityAdversarial,
				PermissionCeiling:   taskrequirements.PermissionReadOnly,
				SideEffectCeiling:   taskrequirements.SideEffectLocalRead,
				QuotaConfidence:     providerinventory.ConfidenceExact,
				BudgetOverride:      "human-only",
				VerificationOutput:  taskrequirements.OutputVerificationVerdict,
				RequiredRoles:       []string{RoleKeyVerifier},
			},
		},
		EligibilityPolicy: Policy{
			EvidencePolicy:              EvidenceRejectUnknownStale,
			RequireBudgetEvidence:       true,
			RequireAvailabilityEvidence: true,
			RequireBoundedScope:         true,
			ContextReserveTokens:        8000,
			VerifierIndependence:        highIndependence,
			AllowPaidOverage:            false,
		},
		OptimizationPolicy: OptimizationPolicy{
			SchemaVersion:  "loopcoder.routing_optimization_policy.v1",
			ProfileKey:     key,
			ProfileVersion: "1",
			PolicyVersion:  "routing-optimization-" + strings.TrimSuffix(key, "-v1"),
			Weights:        weights,
			TieBreakSeed:   "0804-routing-" + key,
		},
		FallbackPolicy: FallbackPolicy{
			PolicyVersion:           "0804.fallback.v1",
			MaxFallbacks:            3,
			NeverWeakenPermission:   true,
			NeverWeakenScope:        true,
			NeverWeakenSideEffects:  true,
			NeverWeakenVerification: true,
			NeverIgnoreUserPin:      true,
			RequireFreshEligibility: true,
		},
		ReplanPolicy: ReplanPolicy{
			PolicyVersion:                  "0804.replan.v1",
			MaxReplanPasses:                bounds.MaxReplanPasses,
			RequireNewFingerprint:          true,
			RequireFreshApproval:           true,
			GraphExpansionRequiresApproval: true,
		},
		UserPinPolicy: UserPinPolicy{
			PolicyVersion:            "0804.user-pin.v1",
			RequireActorProvenance:   true,
			RequireReason:            true,
			RequireScope:             true,
			RequirePolicyFingerprint: true,
			RejectStaleInventory:     true,
			NarrowsCandidatesOnly:    true,
		},
		BudgetSettings: BudgetSettings{
			RequireBudgetEvidence: true,
			RequireExactQuota:     key == ProfileKeyDeep,
			AllowPaidOverage:      false,
			MinimumConfidence:     providerinventory.ConfidenceEstimated,
		},
		VerifierSeparation: map[taskrequirements.RiskTier]taskrequirements.IndependenceLevel{
			taskrequirements.RiskLow:      taskrequirements.IndependenceNone,
			taskrequirements.RiskMedium:   taskrequirements.IndependenceDifferentAccount,
			taskrequirements.RiskHigh:     highIndependence,
			taskrequirements.RiskCritical: taskrequirements.IndependenceHuman,
		},
		Provenance:    ProfileProvenance{SourceKind: "built-in", SourceReference: "docs/specs/0804-decomposition-routing.md", CreatedBy: "loopcoder"},
		EffectiveFrom: builtInProfileEffectiveFrom,
	}
}

func normalizeRoutingPolicyProfile(profile RoutingPolicyProfile) RoutingPolicyProfile {
	if profile.SchemaVersion == "" {
		profile.SchemaVersion = RoutingPolicyProfileSchema
	}
	if profile.RecordVersion == 0 {
		profile.RecordVersion = 1
	}
	if profile.ProfileVersion == "" {
		profile.ProfileVersion = "1"
	}
	if profile.PolicyVersion == "" {
		profile.PolicyVersion = RoutingPolicyVersion
	}
	if profile.EffectiveFrom == "" {
		profile.EffectiveFrom = delivery.CanonicalTimestamp(time.Unix(0, 0).UTC())
	}
	profile.RoleDefinitionIDs = dedupeStrings(profile.RoleDefinitionIDs)
	if profile.RoutingPolicyProfileID == "" {
		profile.RoutingPolicyProfileID = routingProfileID(profile.ProfileKey, profile.ProfileVersion, profile.PolicyVersion)
	}
	profile.OptimizationPolicy.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	profile.OptimizationPolicy.ProfileKey = firstNonEmpty(profile.OptimizationPolicy.ProfileKey, profile.ProfileKey)
	profile.OptimizationPolicy.ProfileVersion = firstNonEmpty(profile.OptimizationPolicy.ProfileVersion, profile.ProfileVersion)
	profile.OptimizationPolicy.AllowPaidOverage = profile.BudgetSettings.AllowPaidOverage
	opt, err := normalizeOptimizationPolicy(profile.OptimizationPolicy, profile.RoutingPolicyProfileID)
	if err == nil {
		profile.OptimizationPolicy = opt
	}
	if profile.PolicyFingerprint == "" {
		profile.PolicyFingerprint = profileFingerprint(profile)
	}
	if profile.Provenance.SourceHash == "" {
		profile.Provenance.SourceHash = profile.PolicyFingerprint
	}
	return profile
}

func profileFingerprint(profile RoutingPolicyProfile) string {
	copy := profile
	copy.RoutingPolicyProfileID = ""
	copy.PolicyFingerprint = ""
	copy.Provenance.SourceHash = ""
	payload, err := delivery.CanonicalJSON(copy)
	if err != nil {
		return ""
	}
	return delivery.SHA256Digest(payload)
}

func routingProfileID(key, version, policyVersion string) string {
	return "rprof_" + digestBase32(key, version, policyVersion)[:32]
}

func normalizePolicyInput(record PolicyInputRecord, now time.Time) PolicyInputRecord {
	record.DecisionKey = strings.TrimSpace(record.DecisionKey)
	if record.SchemaVersion == "" {
		record.SchemaVersion = PolicyInputSchema
	}
	if record.RecordVersion == 0 {
		record.RecordVersion = 1
	}
	if record.Status == "" {
		record.Status = PolicyInputStatusActive
	}
	if record.ValidationStatus == "" {
		record.ValidationStatus = ValidationStatusValid
	}
	if record.CreatedAt == "" {
		record.CreatedAt = delivery.CanonicalTimestamp(now.UTC())
	}
	if record.UpdatedAt == "" {
		record.UpdatedAt = record.CreatedAt
	}
	if record.RoutingPolicyInputID == "" {
		record.RoutingPolicyInputID = policyInputID(record)
		if record.InputKind == PolicyInputKindExclusion {
			record.RoutingPolicyInputID = "rexcl_" + strings.TrimPrefix(record.RoutingPolicyInputID, "rpin_")
		}
	}
	if record.Diagnostics == nil {
		record.Diagnostics = []PolicyDiagnostic{}
	}
	return record
}

func policyInputID(record PolicyInputRecord) string {
	parts := []string{
		record.ProjectID,
		record.DeliveryRunID,
		record.InputKind,
		record.PolicyFingerprint,
		record.Scope,
	}
	// Preserve the pre-v0.8.1 identity for task-wide policy inputs. Only
	// decision-scoped inputs add the new dimension, so an upgrade cannot insert
	// a second row for an already persisted legacy pin or exclusion.
	if record.DecisionKey != "" {
		parts = append(parts, record.DecisionKey)
	}
	parts = append(parts, record.Reason, constraintKey(record.Constraint))
	return "rpin_" + digestBase32(parts...)[:32]
}

func validatePolicyInput(record PolicyInputRecord) error {
	if record.SchemaVersion != PolicyInputSchema || record.RecordVersion != 1 || record.RoutingPolicyInputID == "" || record.InputKind == "" || record.ProjectID == "" || record.RoutingPolicyProfileID == "" || record.PolicyFingerprint == "" || record.Scope == "" || record.Reason == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy input has missing required fields"}
	}
	if record.Actor.ActorID == "" || record.Actor.ActorKind != "user" || record.Host.HostID == "" {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy input requires actor and host provenance"}
	}
	if record.InputKind != PolicyInputKindPin && record.InputKind != PolicyInputKindExclusion {
		return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy input kind must be pin or exclusion"}
	}
	for _, diagnostic := range record.Diagnostics {
		if diagnostic.Kind == "invalid" || diagnostic.Kind == "stale" {
			if diagnostic.Code == taskrequirements.ErrorCode(delivery.ErrStaleApprovalCode) {
				return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: diagnostic.Message}
			}
			return &taskrequirements.TypedError{Code: diagnostic.Code, Message: diagnostic.Message}
		}
	}
	return nil
}

func validateConstraint(kind, id string, c CandidateConstraint, inventory providerinventory.Report, policyFingerprint string, now time.Time) []PolicyDiagnostic {
	var diagnostics []PolicyDiagnostic
	if strings.TrimSpace(policyFingerprint) == "" || !strings.HasPrefix(policyFingerprint, "sha256:") {
		diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrorCode(delivery.ErrStaleApprovalCode), "stale", "policy_fingerprint", id, "pin/exclusion policy fingerprint is missing or stale", nil, "recreate the pin against the active policy fingerprint"))
	}
	if c.AdapterID == "" && c.ProviderInstallationID == "" && c.AccountProfileID == "" && c.ModelCapabilityID == "" && c.InvocationProfileKey == "" {
		diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "constraint", id, kind+" must name at least one provider/account/model/profile constraint", nil, "add an adapter, account, model, installation, or invocation profile id"))
		return diagnostics
	}
	installations := map[string]providerinventory.ProviderInstallation{}
	adapters := map[string]bool{}
	for _, installation := range inventory.Installations {
		installations[installation.ProviderInstallationID] = installation
		adapters[installation.AdapterID] = true
	}
	accounts := map[string]providerinventory.AccountProfile{}
	for _, account := range inventory.AccountProfiles {
		accounts[account.AccountProfileID] = account
		adapters[account.AdapterID] = true
	}
	models := map[string]providerinventory.ModelCapability{}
	for _, model := range inventory.ModelCapabilities {
		models[model.ModelCapabilityID] = model
		adapters[model.AdapterID] = true
	}
	if c.AdapterID != "" && !adapters[c.AdapterID] {
		diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrMissingReferenceCode, "invalid", "adapter_id", c.AdapterID, "adapter is not present in current provider inventory", nil, "run providers refresh or remove the stale constraint"))
	}
	if c.ProviderInstallationID != "" {
		installation, ok := installations[c.ProviderInstallationID]
		if !ok {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrMissingReferenceCode, "invalid", "provider_installation_id", c.ProviderInstallationID, "provider installation is not present in current inventory", nil, "run providers refresh or remove the stale installation pin"))
		} else {
			diagnostics = append(diagnostics, staleInventoryDiagnostics("provider_installation_id", installation.ProviderInstallationID, installation.FreshnessState, installation.Confidence, now)...)
			if installation.AdapterID != "" && c.AdapterID != "" && installation.AdapterID != c.AdapterID {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrCrossProjectReferenceCode, "invalid", "provider_installation_id", c.ProviderInstallationID, "provider installation does not belong to pinned adapter", []string{installation.ProviderInstallationID}, "choose matching adapter and installation ids"))
			}
		}
	}
	if c.AccountProfileID != "" {
		account, ok := accounts[c.AccountProfileID]
		if !ok {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrMissingReferenceCode, "invalid", "account_profile_id", c.AccountProfileID, "account profile is not present in current inventory", nil, "run providers refresh or remove the stale account pin"))
		} else {
			diagnostics = append(diagnostics, staleInventoryDiagnostics("account_profile_id", account.AccountProfileID, account.FreshnessState, account.Confidence, now)...)
			if len(account.CollisionSet) > 0 {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrCapabilityUnsupportedCode, "invalid", "account_profile_id", c.AccountProfileID, "account profile display is ambiguous; use a collision-safe account_profile_id", append([]string{account.AccountProfileID}, account.CollisionSet...), "refresh inventory and choose the opaque account profile id"))
			}
			if c.AdapterID != "" && account.AdapterID != c.AdapterID {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrCrossProjectReferenceCode, "invalid", "account_profile_id", c.AccountProfileID, "account profile does not belong to pinned adapter", []string{account.AccountProfileID}, "choose matching adapter and account ids"))
			}
		}
	}
	if c.ModelCapabilityID != "" {
		model, ok := models[c.ModelCapabilityID]
		if !ok {
			// Accept registry display names / aliases that map to exactly one model.
			var resolved []providerinventory.ModelCapability
			for _, candidate := range inventory.ModelCapabilities {
				if c.AdapterID != "" && candidate.AdapterID != c.AdapterID {
					continue
				}
				if modelNameMatches(candidate, c.ModelCapabilityID) {
					resolved = append(resolved, candidate)
				}
			}
			if len(resolved) == 1 {
				model = resolved[0]
				ok = true
			}
		}
		if !ok {
			diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrMissingReferenceCode, "invalid", "model_capability_id", c.ModelCapabilityID, "model capability is not present in current inventory", nil, "run providers refresh or remove the stale model pin"))
		} else {
			diagnostics = append(diagnostics, staleInventoryDiagnostics("model_capability_id", model.ModelCapabilityID, model.FreshnessState, model.Confidence, now)...)
			if c.AdapterID != "" && model.AdapterID != c.AdapterID {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrCrossProjectReferenceCode, "invalid", "model_capability_id", c.ModelCapabilityID, "model capability does not belong to pinned adapter", []string{model.ModelCapabilityID}, "choose matching adapter and model ids"))
			}
			if model.LifecycleState == providerinventory.LifecycleRemoved || model.AvailabilityState == providerinventory.AvailabilityRemoved {
				diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrCapabilityUnsupportedCode, "invalid", "model_capability_id", c.ModelCapabilityID, "model is removed or unavailable in current inventory", []string{model.ModelCapabilityID}, "choose an available model capability id"))
			}
		}
	}
	if c.InvocationProfileKey != "" && strings.TrimSpace(c.InvocationProfileKey) == "" {
		diagnostics = append(diagnostics, policyDiag(taskrequirements.ErrInvalidRecordCode, "invalid", "invocation_profile_key", id, "invocation profile key must not be blank", nil, "record a concrete invocation profile key"))
	}
	return diagnostics
}

func staleInventoryDiagnostics(field, id string, freshness providerinventory.FreshnessState, confidence providerinventory.Confidence, _ time.Time) []PolicyDiagnostic {
	if freshness == providerinventory.FreshnessStale || freshness == providerinventory.FreshnessExpired || confidence == providerinventory.ConfidenceStale || confidence == providerinventory.ConfidenceUnknown || confidence == providerinventory.ConfidenceUnavailable {
		return []PolicyDiagnostic{policyDiag(taskrequirements.ErrorCode(delivery.ErrStaleApprovalCode), "stale", field, id, "referenced inventory evidence is stale or unavailable", []string{id}, "refresh provider inventory and re-record the pin/exclusion")}
	}
	return nil
}

func policyDiag(code taskrequirements.ErrorCode, kind, field, value, message string, evidence []string, action string) PolicyDiagnostic {
	return PolicyDiagnostic{Code: code, Kind: kind, Field: field, Value: value, Message: message, EvidenceRecordIDs: append([]string(nil), evidence...), Action: action}
}

func sortDiagnostics(diagnostics []PolicyDiagnostic) {
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].Kind != diagnostics[j].Kind {
			return diagnostics[i].Kind < diagnostics[j].Kind
		}
		if diagnostics[i].Field != diagnostics[j].Field {
			return diagnostics[i].Field < diagnostics[j].Field
		}
		return diagnostics[i].Value < diagnostics[j].Value
	})
}

func constraintFromPin(pin Pin) CandidateConstraint {
	return CandidateConstraint{AdapterID: pin.AdapterID, ProviderInstallationID: pin.ProviderInstallationID, AccountProfileID: pin.AccountProfileID, ModelCapabilityID: pin.ModelCapabilityID, InvocationProfileKey: pin.InvocationProfileKey}
}

func constraintFromExclusion(exclusion Exclusion) CandidateConstraint {
	return CandidateConstraint{AdapterID: exclusion.AdapterID, ProviderInstallationID: exclusion.ProviderInstallationID, AccountProfileID: exclusion.AccountProfileID, ModelCapabilityID: exclusion.ModelCapabilityID, InvocationProfileKey: exclusion.InvocationProfileKey}
}

func constraintKey(c CandidateConstraint) string {
	payload, err := delivery.CanonicalJSON(c)
	if err != nil {
		return fmt.Sprintf("%s/%s/%s/%s/%s", c.AdapterID, c.ProviderInstallationID, c.AccountProfileID, c.ModelCapabilityID, c.InvocationProfileKey)
	}
	return delivery.SHA256Digest(payload)
}

func isHardGateOverride(kind, scope string) bool {
	value := strings.ToLower(strings.TrimSpace(kind + " " + scope))
	for _, gate := range []string{"hard-eligibility", "capability", "permission", "side-effect", "side_effect", "scope", "approval", "independence", "budget", "release"} {
		if strings.Contains(value, gate) {
			return true
		}
	}
	return false
}

func supportedBoundedOverride(kind, scope string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	scope = strings.ToLower(strings.TrimSpace(scope))
	if kind == "" || scope == "" {
		return false
	}
	for _, allowedKind := range []string{"routing", "routing-preference", "manual-unavailable-until", "manual-reset", "fallback", "fallback-preference", "replan", "budget", "budget-preference"} {
		if kind == allowedKind {
			for _, allowedScope := range []string{"routing-preference", "manual-unavailable-until", "manual-reset", "fallback-preference", "replan", "budget-preference", "task:", "run:"} {
				if strings.Contains(scope, allowedScope) {
					return true
				}
			}
		}
	}
	return false
}

func emptyCandidateConstraint(c CandidateConstraint) bool {
	return strings.TrimSpace(c.AdapterID) == "" &&
		strings.TrimSpace(c.ProviderInstallationID) == "" &&
		strings.TrimSpace(c.AccountProfileID) == "" &&
		strings.TrimSpace(c.ModelCapabilityID) == "" &&
		strings.TrimSpace(c.InvocationProfileKey) == ""
}

func verifyProfileRoleReferences(ctx context.Context, tx storage.Tx, profile RoutingPolicyProfile) error {
	for _, roleID := range profile.RoleDefinitionIDs {
		roleID = strings.TrimSpace(roleID)
		if roleID == "" {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy profile references an empty role definition id"}
		}
		var existing string
		if err := tx.QueryRow(ctx, `SELECT role_definition_id FROM role_definitions WHERE role_definition_id = ?`, roleID).Scan(&existing); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "routing policy profile references missing role definition " + roleID}
			}
			return err
		}
	}
	return nil
}

func validateStoredPolicyInputsForDecision(records []PolicyInputRecord, input DecisionInput, profileID, policyFingerprint string) error {
	now := input.Now
	for _, record := range records {
		if err := validatePolicyInput(record); err != nil {
			return err
		}
		if record.Status != PolicyInputStatusActive || record.ValidationStatus != ValidationStatusValid {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "routing policy input is not active and valid"}
		}
		if record.ProjectID != input.ProjectID || (record.DeliveryRunID != "" && record.DeliveryRunID != input.DeliveryRunID) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrCrossProjectReferenceCode, Message: "routing policy input project/run does not match decision"}
		}
		if record.RoutingPolicyProfileID != profileID {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "routing policy input profile does not match active profile"}
		}
		if record.PolicyFingerprint != policyFingerprint {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "routing policy input fingerprint does not match active policy fingerprint"}
		}
		if record.ExpiresAt != "" {
			expiresAt, err := time.Parse(time.RFC3339Nano, record.ExpiresAt)
			if err != nil || (!now.IsZero() && !expiresAt.After(now.UTC())) {
				return &taskrequirements.TypedError{Code: taskrequirements.ErrorCode(delivery.ErrExpiredApprovalCode), Message: "routing policy input is expired"}
			}
		}
		diagnostics := ValidatePolicyInputs(input.Inputs.Inventory, pinsForRecord(record), exclusionsForRecord(record), policyFingerprint, now)
		diagnostics = append(diagnostics, validateInvocationProfileConstraint(record, input.Inputs.Candidates)...)
		sortDiagnostics(diagnostics)
		if len(diagnostics) > 0 {
			return &taskrequirements.TypedError{Code: diagnostics[0].Code, Message: diagnostics[0].Message}
		}
	}
	return nil
}

func validateInvocationProfileConstraint(record PolicyInputRecord, candidates []Candidate) []PolicyDiagnostic {
	key := strings.TrimSpace(record.Constraint.InvocationProfileKey)
	if key == "" {
		return nil
	}
	if len(candidates) == 0 {
		if key == "default" {
			return nil
		}
		return []PolicyDiagnostic{policyDiag(taskrequirements.ErrMissingReferenceCode, "invalid", "invocation_profile_key", key, "invocation profile is not present in current routing candidates", nil, "refresh routing inventory or remove the stale invocation profile constraint")}
	}
	for _, candidate := range candidates {
		if matchesSelector(candidate, record.Constraint.AdapterID, record.Constraint.ProviderInstallationID, record.Constraint.AccountProfileID, record.Constraint.ModelCapabilityID, record.Constraint.InvocationProfileKey) && strings.TrimSpace(candidate.InvocationProfileKey) == key {
			return nil
		}
	}
	return []PolicyDiagnostic{policyDiag(taskrequirements.ErrMissingReferenceCode, "invalid", "invocation_profile_key", key, "invocation profile is not present in current routing candidates", nil, "refresh routing inventory or remove the stale invocation profile constraint")}
}

func requireCallerInputsPersisted(pins []Pin, exclusions []Exclusion, records []PolicyInputRecord) error {
	for _, pin := range pins {
		if !storedConstraintExists(PolicyInputKindPin, constraintFromPin(pin), records) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "caller-supplied pin is not backed by an active persisted policy input"}
		}
	}
	for _, exclusion := range exclusions {
		if !storedConstraintExists(PolicyInputKindExclusion, constraintFromExclusion(exclusion), records) {
			return &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "caller-supplied exclusion is not backed by an active persisted policy input"}
		}
	}
	return nil
}

func storedConstraintExists(kind string, constraint CandidateConstraint, records []PolicyInputRecord) bool {
	for _, record := range records {
		if record.InputKind == kind && sameConstraint(record.Constraint, constraint) {
			return true
		}
	}
	return false
}

func constraintsFromPolicyInputRecords(records []PolicyInputRecord) ([]Pin, []Exclusion) {
	pins := []Pin{}
	exclusions := []Exclusion{}
	for _, record := range records {
		switch record.InputKind {
		case PolicyInputKindPin:
			pins = append(pins, pinForRecord(record))
		case PolicyInputKindExclusion:
			exclusions = append(exclusions, exclusionForRecord(record))
		}
	}
	return pins, exclusions
}

func policyInputsForTask(records []PolicyInputRecord, taskID, decisionKey string) []PolicyInputRecord {
	taskID = strings.TrimSpace(taskID)
	decisionKey = strings.TrimSpace(decisionKey)
	out := make([]PolicyInputRecord, 0, len(records))
	hasDecisionPin := false
	for _, record := range records {
		scope := strings.TrimSpace(record.Scope)
		if taskID != "" && strings.HasPrefix(scope, "task:") && strings.TrimSpace(strings.TrimPrefix(scope, "task:")) != taskID {
			continue
		}
		recordDecisionKey := strings.TrimSpace(record.DecisionKey)
		if recordDecisionKey != "" && recordDecisionKey != decisionKey {
			continue
		}
		if record.InputKind == PolicyInputKindPin && recordDecisionKey != "" {
			hasDecisionPin = true
		}
		out = append(out, record)
	}
	if !hasDecisionPin {
		return out
	}
	filtered := out[:0]
	for _, record := range out {
		if record.InputKind == PolicyInputKindPin && strings.TrimSpace(record.DecisionKey) == "" {
			continue
		}
		filtered = append(filtered, record)
	}
	return filtered
}

func pinsForRecord(record PolicyInputRecord) []Pin {
	if record.InputKind != PolicyInputKindPin {
		return nil
	}
	return []Pin{pinForRecord(record)}
}

func exclusionsForRecord(record PolicyInputRecord) []Exclusion {
	if record.InputKind != PolicyInputKindExclusion {
		return nil
	}
	return []Exclusion{exclusionForRecord(record)}
}

func pinForRecord(record PolicyInputRecord) Pin {
	return Pin{
		PinID:                  record.RoutingPolicyInputID,
		AdapterID:              record.Constraint.AdapterID,
		ProviderInstallationID: record.Constraint.ProviderInstallationID,
		AccountProfileID:       record.Constraint.AccountProfileID,
		ModelCapabilityID:      record.Constraint.ModelCapabilityID,
		InvocationProfileKey:   record.Constraint.InvocationProfileKey,
	}
}

func exclusionForRecord(record PolicyInputRecord) Exclusion {
	return Exclusion{
		ExclusionID:            record.RoutingPolicyInputID,
		AdapterID:              record.Constraint.AdapterID,
		ProviderInstallationID: record.Constraint.ProviderInstallationID,
		AccountProfileID:       record.Constraint.AccountProfileID,
		ModelCapabilityID:      record.Constraint.ModelCapabilityID,
		InvocationProfileKey:   record.Constraint.InvocationProfileKey,
	}
}

func sameConstraint(a, b CandidateConstraint) bool {
	return strings.TrimSpace(a.AdapterID) == strings.TrimSpace(b.AdapterID) &&
		strings.TrimSpace(a.ProviderInstallationID) == strings.TrimSpace(b.ProviderInstallationID) &&
		strings.TrimSpace(a.AccountProfileID) == strings.TrimSpace(b.AccountProfileID) &&
		strings.TrimSpace(a.ModelCapabilityID) == strings.TrimSpace(b.ModelCapabilityID) &&
		strings.TrimSpace(a.InvocationProfileKey) == strings.TrimSpace(b.InvocationProfileKey)
}

func legacyModelMapping(projectID, roleKey, provider, modelName, effort string, inventory providerinventory.Report, policyFingerprint string, now time.Time) LegacyModelMapping {
	mapping := normalizeLegacyModelMapping(LegacyModelMapping{
		ProjectID:             strings.TrimSpace(projectID),
		RoleKey:               strings.TrimSpace(roleKey),
		LegacyProvider:        strings.TrimSpace(provider),
		LegacyModel:           strings.TrimSpace(modelName),
		LegacyReasoningEffort: strings.TrimSpace(effort),
		PolicyFingerprint:     strings.TrimSpace(policyFingerprint),
	}, now)
	if mapping.LegacyModel == "" {
		mapping.MappingStatus = "unresolved"
		mapping.DiagnosticCode = taskrequirements.ErrRequirementUnknownCode
		mapping.Diagnostic = "legacy role has no explicit model; no deterministic model pin was created"
		return mapping
	}
	var matches []providerinventory.ModelCapability
	for _, model := range inventory.ModelCapabilities {
		if mapping.LegacyProvider != "" && model.AdapterID != mapping.LegacyProvider {
			continue
		}
		if modelNameMatches(model, mapping.LegacyModel) {
			matches = append(matches, model)
		}
	}
	if len(matches) == 1 && matches[0].LifecycleState == providerinventory.LifecycleAvailable && matches[0].AvailabilityState == providerinventory.AvailabilityAvailable {
		mapping.MappingStatus = "mapped"
		mapping.MappedAdapterID = matches[0].AdapterID
		mapping.MappedModelCapabilityID = matches[0].ModelCapabilityID
		return mapping
	}
	mapping.MappingStatus = "unresolved"
	mapping.DiagnosticCode = taskrequirements.ErrRequirementUnknownCode
	if len(matches) == 0 {
		mapping.Diagnostic = "legacy model setting did not match exactly one current model capability"
	} else {
		mapping.Diagnostic = "legacy model setting is ambiguous or maps to an unavailable model capability"
	}
	return mapping
}

func normalizeLegacyModelMapping(mapping LegacyModelMapping, now time.Time) LegacyModelMapping {
	if mapping.SchemaVersion == "" {
		mapping.SchemaVersion = LegacyModelMappingSchema
	}
	if mapping.RecordVersion == 0 {
		mapping.RecordVersion = 1
	}
	if mapping.CreatedAt == "" {
		mapping.CreatedAt = delivery.CanonicalTimestamp(now.UTC())
	}
	if mapping.UpdatedAt == "" {
		mapping.UpdatedAt = mapping.CreatedAt
	}
	if mapping.LegacyModelMappingID == "" {
		mapping.LegacyModelMappingID = "rlmap_" + digestBase32(mapping.ProjectID, mapping.RoleKey, mapping.LegacyProvider, mapping.LegacyModel, mapping.LegacyReasoningEffort)[:32]
	}
	return mapping
}

func loadLegacyModelMapping(ctx context.Context, store storage.Store, projectID, roleKey, provider, model, effort string) (LegacyModelMapping, error) {
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM routing_legacy_model_mappings WHERE project_id = ? AND role_key = ? AND legacy_provider = ? AND legacy_model = ? AND legacy_reasoning_effort = ?`,
			projectID, roleKey, provider, model, effort).Scan(&payload)
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LegacyModelMapping{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: "legacy model mapping was not persisted"}
		}
		return LegacyModelMapping{}, err
	}
	var mapping LegacyModelMapping
	if err := json.Unmarshal([]byte(payload), &mapping); err != nil {
		return LegacyModelMapping{}, err
	}
	return mapping, nil
}

func modelNameMatches(model providerinventory.ModelCapability, legacy string) bool {
	legacy = strings.TrimSpace(legacy)
	if legacy == "" {
		return false
	}
	if model.ModelCapabilityID == legacy || model.CanonicalModelID == legacy || model.DisplayName == legacy {
		return true
	}
	for _, alias := range model.Aliases {
		if alias.Alias == legacy {
			return true
		}
	}
	return false
}
