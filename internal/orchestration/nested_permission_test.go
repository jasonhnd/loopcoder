package orchestration

import (
	"errors"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func TestCheckNestedExecutorPermissionsRequiresExplicitRegistration(t *testing.T) {
	plan := nestedPermissionTestPlan("read-only", nil)

	err := CheckNestedExecutorPermissions(&plan, NestedExecutorCapability{
		ExecutorID:             "worker-dispatch",
		Provider:               "codex",
		EnforceablePermissions: []string{string(reporter.PermissionReadOnly)},
	})
	var refusalErr *PermissionNotEnforceableError
	if !errors.As(err, &refusalErr) {
		t.Fatalf("CheckNestedExecutorPermissions error = %v, want PermissionNotEnforceableError", err)
	}
	if len(refusalErr.Refusals) != 1 || refusalErr.Refusals[0].CapabilityResult != NestedCapabilityNotRegistered {
		t.Fatalf("refusals = %#v, want explicit not_registered refusal", refusalErr.Refusals)
	}

	err = CheckNestedExecutorPermissions(&plan, NestedExecutorCapability{
		ExecutorID:             "future-read-only-executor",
		RegistrationID:         "test:future-read-only:v1",
		Provider:               "codex",
		EnforceablePermissions: []string{string(reporter.PermissionReadOnly)},
	})
	if err != nil {
		t.Fatalf("explicit registered capability rejected: %v", err)
	}
}

func TestCheckNestedExecutorPermissionsRejectsProviderNativeIndependently(t *testing.T) {
	plan := nestedPermissionTestPlan("read-only", []byte(`{"provider_native_subagent":true}`))
	err := CheckNestedExecutorPermissions(&plan, NestedExecutorCapability{
		ExecutorID:             "future-read-only-executor",
		RegistrationID:         "test:future-read-only:v1",
		Provider:               "claude",
		EnforceablePermissions: []string{string(reporter.PermissionReadOnly)},
		ProviderNative:         false,
	})
	var refusalErr *PermissionNotEnforceableError
	if !errors.As(err, &refusalErr) {
		t.Fatalf("CheckNestedExecutorPermissions error = %v, want PermissionNotEnforceableError", err)
	}
	if len(refusalErr.Refusals) != 1 || refusalErr.Refusals[0].CapabilityResult != NestedCapabilityProviderNativeDenied || !refusalErr.Refusals[0].ProviderNativeRequested {
		t.Fatalf("refusals = %#v, want provider-native refusal", refusalErr.Refusals)
	}
}

func nestedPermissionTestPlan(permission string, metadata []byte) ChildPlan {
	return ChildPlan{Items: []ChildRunPlan{{
		ChildKey:   "alpha",
		Role:       string(reporter.RoleWorker),
		Permission: permission,
		Scope:      ChildScope{Repo: ".", Paths: []string{"internal/orchestration/nested_permission.go"}},
		Aggregation: ChildAggregation{
			Mode:          ChildAggregationCollect,
			Required:      true,
			IncludeReport: true,
		},
		Metadata: metadata,
	}}}
}
