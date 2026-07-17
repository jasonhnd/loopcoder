package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const (
	NestedOutcomePermissionNotEnforceable = "permission_not_enforceable"

	NestedCapabilitySupported            = "supported"
	NestedCapabilityUnsupported          = "unsupported"
	NestedCapabilityNotRegistered        = "not_registered"
	NestedCapabilityUnknownPermission    = "unknown_permission"
	NestedCapabilityProviderNativeDenied = "provider_native_unsupported"

	NestedDelegationModeLoopCoderManaged NestedDelegationExecutionMode = "loopcoder_managed"
	NestedDelegationModeProviderNative   NestedDelegationExecutionMode = "provider_native"

	NestedDelegationLoopCoderManaged       NestedDelegationResult = "loopcoder_managed"
	NestedDelegationBridgeApproved         NestedDelegationResult = "provider_native_bridge_approved"
	NestedDelegationBridgeRequired         NestedDelegationResult = "provider_native_bridge_required"
	NestedDelegationOrchestrateUnsupported NestedDelegationResult = "orchestrate_unsupported"

	NestedReasonExecutorNotRegistered        = "nested_executor_not_registered"
	NestedReasonPermissionUnsupported        = "nested_permission_unsupported"
	NestedReasonUnknownPermission            = "nested_permission_unknown"
	NestedReasonProviderNativeBridgeRequired = "provider_native_bridge_required"
	NestedReasonOrchestrateUnsupported       = "orchestrate_unsupported"
)

type NestedDelegationExecutionMode string

type NestedDelegationResult string

// ProviderNativeBridge is the code-level extension point for a future audited
// provider-native bridge. Provider, prompt, environment, and host metadata
// cannot implement or register a bridge. No production bridge is registered in
// v0.8.1.
type ProviderNativeBridge interface {
	BridgeID() string
	Provider() string
	Execute(context.Context, ChildExecutionRequest) (ChildRunResult, error)
}

// NestedDelegationCapabilityResult is the typed decision made before child
// persistence, claim acquisition, lifecycle transitions, or provider launch.
type NestedDelegationCapabilityResult struct {
	ExecutionMode NestedDelegationExecutionMode `json:"execution_mode"`
	Result        NestedDelegationResult        `json:"result"`
	Supported     bool                          `json:"supported"`
	BridgeID      string                        `json:"bridge_id,omitempty"`
	ReasonCode    string                        `json:"reason_code"`
	Remediation   string                        `json:"remediation,omitempty"`
}

// NestedExecutorCapability is an explicit registration at the nested
// execution boundary. Provider or model identity never implies a capability.
type NestedExecutorCapability struct {
	ExecutorID             string   `json:"executor_id"`
	RegistrationID         string   `json:"registration_id"`
	Provider               string   `json:"provider,omitempty"`
	EnforceablePermissions []string `json:"enforceable_permissions"`
	// ProviderNative is retained for compatibility with v0.8.0 diagnostics.
	// It is never authority and cannot enable native delegation.
	ProviderNative bool `json:"provider_native"`
	// NativeBridge is code-only authority. It is intentionally absent from JSON
	// so external metadata cannot silently opt into provider-native execution.
	NativeBridge ProviderNativeBridge `json:"-"`
}

// NestedPermissionRefusal is the stable, bounded refusal emitted before any
// nested execution state or provider process is created.
type NestedPermissionRefusal struct {
	Code                    string                           `json:"code"`
	ChildKey                string                           `json:"child_key"`
	RequestedPermission     string                           `json:"requested_permission"`
	ExecutorID              string                           `json:"executor_id"`
	RegistrationID          string                           `json:"registration_id"`
	Provider                string                           `json:"provider,omitempty"`
	CapabilityResult        string                           `json:"capability_result"`
	ProviderNativeRequested bool                             `json:"provider_native_requested"`
	ReasonCode              string                           `json:"reason_code"`
	Remediation             string                           `json:"remediation"`
	DelegationCapability    NestedDelegationCapabilityResult `json:"delegation_capability"`
	Reason                  string                           `json:"reason"`
	NextAction              string                           `json:"next_action"`
}

// PermissionNotEnforceableError is returned before scheduling whenever an
// explicit executor registration cannot enforce every requested child mode.
type PermissionNotEnforceableError struct {
	Refusals []NestedPermissionRefusal
}

func (e *PermissionNotEnforceableError) Error() string {
	if e == nil || len(e.Refusals) == 0 {
		return NestedOutcomePermissionNotEnforceable
	}
	first := e.Refusals[0]
	if len(e.Refusals) == 1 {
		return first.Reason
	}
	return fmt.Sprintf("%s (%d child requests refused; first: %s)", NestedOutcomePermissionNotEnforceable, len(e.Refusals), first.Reason)
}

// PrepareNestedPlanForExecution performs non-persistent identity checks and
// assigns deterministic child run IDs before permission capability evaluation.
func PrepareNestedPlanForExecution(plan *ChildPlan, maxChildren int) error {
	if plan == nil {
		return fmt.Errorf("child plan is required")
	}
	if maxChildren <= 0 {
		maxChildren = lcdefaults.NestedSchedulerMaxChildren
	}
	identityTime, err := state.ParseTimestamp(plan.CreatedAt)
	if err != nil {
		return err
	}
	children, err := normalizeChildRunPlans(plan.Items, NestedScheduleOptions{
		ParentRunID:      plan.ParentRunID,
		RootRunID:        plan.RootRunID,
		ParentDepth:      plan.ParentDepth,
		MaxDepth:         plan.MaxDepth,
		MaxChildren:      maxChildren,
		ConcurrencyLimit: plan.MaxConcurrency,
	}, identityTime)
	if err != nil {
		return err
	}
	plan.Items = children
	return nil
}

// CheckNestedExecutorPermissions evaluates only an explicit executor
// registration. It does not inspect executable presence, provider name, model,
// prompt text, or optimistic defaults.
func CheckNestedExecutorPermissions(plan *ChildPlan, capability NestedExecutorCapability) error {
	if plan == nil {
		return fmt.Errorf("child plan is required")
	}
	permissions := map[string]bool{}
	for _, permission := range capability.EnforceablePermissions {
		permission = normalizeChildPermission(permission)
		if validChildPermission(permission) {
			permissions[permission] = true
		}
	}
	refusals := make([]NestedPermissionRefusal, 0)
	for _, child := range plan.Items {
		permission := normalizeChildPermission(child.Permission)
		providerNative := childRequiresNativeRegistration(child)
		delegation := EvaluateNestedDelegationCapability(child, capability)
		result := NestedCapabilitySupported
		switch {
		case !validChildPermission(permission):
			result = NestedCapabilityUnknownPermission
		case !delegation.Supported && delegation.Result == NestedDelegationOrchestrateUnsupported:
			result = NestedCapabilityUnsupported
		case !delegation.Supported && delegation.Result == NestedDelegationBridgeRequired:
			result = NestedCapabilityProviderNativeDenied
		case strings.TrimSpace(capability.RegistrationID) == "":
			result = NestedCapabilityNotRegistered
		case !permissions[permission]:
			result = NestedCapabilityUnsupported
		}
		if result == NestedCapabilitySupported {
			continue
		}
		refusals = append(refusals, newNestedPermissionRefusal(child, capability, permission, providerNative, result, delegation))
	}
	if len(refusals) == 0 {
		return nil
	}
	return &PermissionNotEnforceableError{Refusals: refusals}
}

// CheckNestedDelegationCapabilities is the scheduler's entry-point-independent
// gate. It checks only delegation modes, so ordinary managed read-only and
// bounded-write scheduler tests remain provider-neutral.
func CheckNestedDelegationCapabilities(plan *ChildPlan, capability NestedExecutorCapability) error {
	if plan == nil {
		return fmt.Errorf("child plan is required")
	}
	refusals := make([]NestedPermissionRefusal, 0)
	for _, child := range plan.Items {
		delegation := EvaluateNestedDelegationCapability(child, capability)
		if delegation.Supported {
			continue
		}
		result := NestedCapabilityUnsupported
		if delegation.Result == NestedDelegationBridgeRequired {
			result = NestedCapabilityProviderNativeDenied
		}
		refusals = append(refusals, newNestedPermissionRefusal(
			child,
			capability,
			normalizeChildPermission(child.Permission),
			childRequiresNativeRegistration(child),
			result,
			delegation,
		))
	}
	if len(refusals) == 0 {
		return nil
	}
	return &PermissionNotEnforceableError{Refusals: refusals}
}

// EvaluateNestedDelegationCapability separates provider feature advertising
// from LoopCoder delegation authority. A bridge must be a concrete code-level
// implementation for the exact provider; legacy booleans and metadata are not
// sufficient.
func EvaluateNestedDelegationCapability(child ChildRunPlan, capability NestedExecutorCapability) NestedDelegationCapabilityResult {
	permission := normalizeChildPermission(child.Permission)
	providerNative := childRequiresNativeRegistration(child)
	mode := NestedDelegationModeLoopCoderManaged
	if providerNative {
		mode = NestedDelegationModeProviderNative
	}
	if permission == string(reporter.PermissionOrchestrate) {
		return NestedDelegationCapabilityResult{
			ExecutionMode: mode,
			Result:        NestedDelegationOrchestrateUnsupported,
			Supported:     false,
			ReasonCode:    NestedReasonOrchestrateUnsupported,
			Remediation:   "use a read-only or bounded-write LoopCoder-managed child; orchestrate execution is not available in v0.8.1",
		}
	}
	if !providerNative {
		return NestedDelegationCapabilityResult{
			ExecutionMode: mode,
			Result:        NestedDelegationLoopCoderManaged,
			Supported:     true,
			ReasonCode:    string(NestedDelegationLoopCoderManaged),
		}
	}
	provider := firstNonEmptyChild(nestedProviderFromChild(child), strings.TrimSpace(capability.Provider))
	bridge := capability.NativeBridge
	if bridgeID, bridgeProvider, ok := nestedNativeBridgeIdentity(bridge); ok {
		if bridgeID != "" && provider != "" && bridgeProvider == provider {
			return NestedDelegationCapabilityResult{
				ExecutionMode: mode,
				Result:        NestedDelegationBridgeApproved,
				Supported:     true,
				BridgeID:      bridgeID,
				ReasonCode:    string(NestedDelegationBridgeApproved),
			}
		}
	}
	return NestedDelegationCapabilityResult{
		ExecutionMode: mode,
		Result:        NestedDelegationBridgeRequired,
		Supported:     false,
		ReasonCode:    NestedReasonProviderNativeBridgeRequired,
		Remediation:   "use a LoopCoder-managed child or install a separately implemented, registered, and tested bridge for the exact provider",
	}
}

func nestedNativeBridgeIdentity(bridge ProviderNativeBridge) (string, string, bool) {
	if bridge == nil {
		return "", "", false
	}
	value := reflect.ValueOf(bridge)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return "", "", false
		}
	}
	id := strings.TrimSpace(bridge.BridgeID())
	provider := strings.TrimSpace(bridge.Provider())
	return id, provider, id != "" && provider != ""
}

// NestedPermissionRefusalReport renders a refusal without persisting a plan,
// child run, lifecycle event, claim, budget charge, or progress receipt.
func NestedPermissionRefusalReport(repoPath, baseBranch string, plan ChildPlan, capability NestedExecutorCapability, refusalErr *PermissionNotEnforceableError, at time.Time) NestedScheduleReport {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	refusals := append([]NestedPermissionRefusal(nil), refusalErr.Refusals...)
	byChild := make(map[string]NestedPermissionRefusal, len(refusals))
	for i := range refusals {
		if refusals[i].ExecutorID == "" {
			refusals[i].ExecutorID = boundedNestedPermissionText(capability.ExecutorID)
		}
		if refusals[i].RegistrationID == "" {
			refusals[i].RegistrationID = boundedNestedPermissionText(capability.RegistrationID)
		}
		if refusals[i].Provider == "" {
			refusals[i].Provider = boundedNestedPermissionText(capability.Provider)
		}
		byChild[refusals[i].ChildKey] = refusals[i]
	}
	children := make([]ChildRunResult, 0, len(plan.Items))
	for _, child := range plan.Items {
		result := childResultFromPlan(child)
		result.Outcome = NestedOutcomePermissionNotEnforceable
		if refusal, ok := byChild[child.ChildKey]; ok {
			result.Status = NestedStatusNeedsHuman
			result.Error = refusal.Code
			result.Reason = refusal.Reason
			result.NextAction = refusal.NextAction
		} else {
			result.Status = NestedStatusBlocked
			result.Error = NestedOutcomePermissionNotEnforceable
			result.Reason = "the child plan was refused before execution because another child permission is not enforceable"
			result.NextAction = "register every required nested executor capability, then replay the unchanged plan"
		}
		children = append(children, result)
	}
	report := NestedScheduleReport{
		Version:            1,
		RepoPath:           strings.TrimSpace(repoPath),
		BaseBranch:         strings.TrimSpace(baseBranch),
		ParentRunID:        strings.TrimSpace(plan.ParentRunID),
		Status:             NestedStatusNeedsHuman,
		Outcome:            NestedOutcomePermissionNotEnforceable,
		FinishedAt:         state.FormatTimestamp(at),
		ConcurrencyLimit:   plan.MaxConcurrency,
		Children:           children,
		ExecutorCapability: &capability,
		Refusals:           refusals,
	}
	report.Summary = nestedSummary(children)
	return report
}

func newNestedPermissionRefusal(child ChildRunPlan, capability NestedExecutorCapability, permission string, providerNative bool, result string, delegation NestedDelegationCapabilityResult) NestedPermissionRefusal {
	next := "register an executor that explicitly enforces the requested permission, then replay the unchanged plan"
	reasonCode := NestedReasonPermissionUnsupported
	switch {
	case result == NestedCapabilityUnknownPermission:
		next = "use read-only, write, or orchestrate and register an executor that explicitly enforces it"
		reasonCode = NestedReasonUnknownPermission
	case !delegation.Supported:
		next = delegation.Remediation
		reasonCode = delegation.ReasonCode
	case result == NestedCapabilityNotRegistered:
		reasonCode = NestedReasonExecutorNotRegistered
	case permission == string(reporter.PermissionReadOnly):
		next = "wait for a registered mutation-free read-only nested executor"
	case permission == string(reporter.PermissionWrite):
		next = "wait for a registered bounded-write nested executor"
	case permission == string(reporter.PermissionOrchestrate):
		next = "wait for an approved delegation bridge with enforceable child authority"
	}
	executorID := boundedNestedPermissionText(capability.ExecutorID)
	if executorID == "" {
		executorID = "unregistered"
	}
	reason := fmt.Sprintf("executor %q capability result %q cannot enforce child %q permission %q", executorID, result, boundedNestedPermissionText(child.ChildKey), boundedNestedPermissionText(permission))
	if providerNative {
		reason += " with provider-native execution"
	}
	provider := firstNonEmptyChild(strings.TrimSpace(capability.Provider), nestedProviderFromChild(child))
	return NestedPermissionRefusal{
		Code:                    NestedOutcomePermissionNotEnforceable,
		ChildKey:                boundedNestedPermissionText(child.ChildKey),
		RequestedPermission:     boundedNestedPermissionText(permission),
		ExecutorID:              executorID,
		RegistrationID:          boundedNestedPermissionText(capability.RegistrationID),
		Provider:                boundedNestedPermissionText(provider),
		CapabilityResult:        result,
		ProviderNativeRequested: providerNative,
		ReasonCode:              boundedNestedPermissionText(reasonCode),
		Remediation:             boundedNestedPermissionText(next),
		DelegationCapability:    delegation,
		Reason:                  boundedNestedPermissionText(reason),
		NextAction:              boundedNestedPermissionText(next),
	}
}

func nestedProviderFromChild(child ChildRunPlan) string {
	if len(child.Metadata) == 0 {
		return ""
	}
	var metadata struct {
		Provider  string `json:"provider"`
		AdapterID string `json:"adapter_id"`
	}
	if err := json.Unmarshal(child.Metadata, &metadata); err != nil {
		return ""
	}
	return firstNonEmptyChild(strings.TrimSpace(metadata.Provider), strings.TrimSpace(metadata.AdapterID))
}

func boundedNestedPermissionText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 240 {
		return value
	}
	return value[:240]
}
