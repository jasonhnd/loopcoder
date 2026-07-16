package orchestration

import (
	"fmt"
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
)

// NestedExecutorCapability is an explicit registration at the nested
// execution boundary. Provider or model identity never implies a capability.
type NestedExecutorCapability struct {
	ExecutorID             string   `json:"executor_id"`
	RegistrationID         string   `json:"registration_id"`
	Provider               string   `json:"provider,omitempty"`
	EnforceablePermissions []string `json:"enforceable_permissions"`
	ProviderNative         bool     `json:"provider_native"`
}

// NestedPermissionRefusal is the stable, bounded refusal emitted before any
// nested execution state or provider process is created.
type NestedPermissionRefusal struct {
	Code                    string `json:"code"`
	ChildKey                string `json:"child_key"`
	RequestedPermission     string `json:"requested_permission"`
	ExecutorID              string `json:"executor_id"`
	RegistrationID          string `json:"registration_id"`
	Provider                string `json:"provider,omitempty"`
	CapabilityResult        string `json:"capability_result"`
	ProviderNativeRequested bool   `json:"provider_native_requested"`
	Reason                  string `json:"reason"`
	NextAction              string `json:"next_action"`
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
		result := NestedCapabilitySupported
		switch {
		case !validChildPermission(permission):
			result = NestedCapabilityUnknownPermission
		case strings.TrimSpace(capability.RegistrationID) == "":
			result = NestedCapabilityNotRegistered
		case providerNative && !capability.ProviderNative:
			result = NestedCapabilityProviderNativeDenied
		case !permissions[permission]:
			result = NestedCapabilityUnsupported
		}
		if result == NestedCapabilitySupported {
			continue
		}
		refusals = append(refusals, newNestedPermissionRefusal(child, capability, permission, providerNative, result))
	}
	if len(refusals) == 0 {
		return nil
	}
	return &PermissionNotEnforceableError{Refusals: refusals}
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

func newNestedPermissionRefusal(child ChildRunPlan, capability NestedExecutorCapability, permission string, providerNative bool, result string) NestedPermissionRefusal {
	next := "register an executor that explicitly enforces the requested permission, then replay the unchanged plan"
	switch {
	case result == NestedCapabilityUnknownPermission:
		next = "use read-only, write, or orchestrate and register an executor that explicitly enforces it"
	case providerNative:
		next = "wait for an approved provider-native delegation bridge with claim, budget, progress, and scope enforcement"
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
	return NestedPermissionRefusal{
		Code:                    NestedOutcomePermissionNotEnforceable,
		ChildKey:                boundedNestedPermissionText(child.ChildKey),
		RequestedPermission:     boundedNestedPermissionText(permission),
		ExecutorID:              executorID,
		RegistrationID:          boundedNestedPermissionText(capability.RegistrationID),
		Provider:                boundedNestedPermissionText(capability.Provider),
		CapabilityResult:        result,
		ProviderNativeRequested: providerNative,
		Reason:                  boundedNestedPermissionText(reason),
		NextAction:              boundedNestedPermissionText(next),
	}
}

func boundedNestedPermissionText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 240 {
		return value
	}
	return value[:240]
}
