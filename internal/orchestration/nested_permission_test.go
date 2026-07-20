package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
		ProviderNative:         true,
	})
	var refusalErr *PermissionNotEnforceableError
	if !errors.As(err, &refusalErr) {
		t.Fatalf("CheckNestedExecutorPermissions error = %v, want PermissionNotEnforceableError", err)
	}
	if len(refusalErr.Refusals) != 1 || refusalErr.Refusals[0].CapabilityResult != NestedCapabilityProviderNativeDenied || !refusalErr.Refusals[0].ProviderNativeRequested {
		t.Fatalf("refusals = %#v, want provider-native refusal", refusalErr.Refusals)
	}
	refusal := refusalErr.Refusals[0]
	if refusal.ReasonCode != NestedReasonProviderNativeBridgeRequired || refusal.Remediation == "" || refusal.DelegationCapability.Result != NestedDelegationBridgeRequired {
		t.Fatalf("refusal = %#v, want stable bridge-required reason and remediation", refusal)
	}
}

func TestNestedDelegationCapabilityMatrixFailsClosedWithoutBridge(t *testing.T) {
	providers := []string{"codex", "claude", "gemini", "antigravity", "grok", "paseo", "future-provider"}
	for _, provider := range providers {
		t.Run(provider, func(t *testing.T) {
			metadata, err := json.Marshal(map[string]any{
				"provider_native_subagent": true,
				"adapter_id":               provider,
			})
			if err != nil {
				t.Fatalf("marshal metadata: %v", err)
			}
			plan := nestedPermissionTestPlan("read-only", metadata)
			capability := NestedExecutorCapability{
				ExecutorID:             "matrix-executor",
				RegistrationID:         "test:matrix:v1",
				Provider:               provider,
				EnforceablePermissions: []string{string(reporter.PermissionReadOnly)},
				// A legacy/provider-advertised boolean is deliberately not authority.
				ProviderNative: true,
			}
			result := EvaluateNestedDelegationCapability(plan.Items[0], capability)
			if result.Supported || result.Result != NestedDelegationBridgeRequired || result.ReasonCode != NestedReasonProviderNativeBridgeRequired {
				t.Fatalf("result = %#v, want bridge-required refusal", result)
			}
		})
	}
}

func TestNestedDelegationLegacyCapabilityMigrationDoesNotGrantBridge(t *testing.T) {
	var capability NestedExecutorCapability
	if err := json.Unmarshal([]byte(`{
		"executor_id":"legacy",
		"registration_id":"legacy:v1",
		"provider":"claude",
		"enforceable_permissions":["read-only"],
		"provider_native":true
	}`), &capability); err != nil {
		t.Fatalf("unmarshal legacy capability: %v", err)
	}
	if !capability.ProviderNative {
		t.Fatal("legacy provider_native field was not retained")
	}
	plan := nestedPermissionTestPlan("read-only", []byte(`{"provider_native_subagent":true,"adapter_id":"claude"}`))
	result := EvaluateNestedDelegationCapability(plan.Items[0], capability)
	if result.Supported || result.Result != NestedDelegationBridgeRequired {
		t.Fatalf("legacy capability result = %#v, want bridge-required refusal", result)
	}
}

func TestNestedDelegationCapabilityRequiresConcreteMatchingBridge(t *testing.T) {
	plan := nestedPermissionTestPlan("read-only", []byte(`{"provider_native_subagent":true,"adapter_id":"future-provider"}`))
	capability := NestedExecutorCapability{
		ExecutorID:             "future-executor",
		RegistrationID:         "test:future:v1",
		Provider:               "future-provider",
		EnforceablePermissions: []string{string(reporter.PermissionReadOnly)},
		NativeBridge: testNativeBridge{
			id:       "test:future-provider-bridge:v1",
			provider: "future-provider",
		},
	}
	result := EvaluateNestedDelegationCapability(plan.Items[0], capability)
	if !result.Supported || result.Result != NestedDelegationBridgeApproved || result.BridgeID != "test:future-provider-bridge:v1" {
		t.Fatalf("result = %#v, want approved concrete bridge", result)
	}
	if err := CheckNestedExecutorPermissions(&plan, capability); err != nil {
		t.Fatalf("approved bridge rejected: %v", err)
	}

	capability.NativeBridge = testNativeBridge{id: "test:wrong:v1", provider: "different-provider"}
	result = EvaluateNestedDelegationCapability(plan.Items[0], capability)
	if result.Supported || result.Result != NestedDelegationBridgeRequired {
		t.Fatalf("mismatched bridge result = %#v, want bridge-required refusal", result)
	}

	var nilBridge *testNativeBridge
	capability.NativeBridge = nilBridge
	result = EvaluateNestedDelegationCapability(plan.Items[0], capability)
	if result.Supported || result.Result != NestedDelegationBridgeRequired {
		t.Fatalf("typed nil bridge result = %#v, want bridge-required refusal", result)
	}
}

func TestNestedDelegationCapabilityAlwaysRejectsOrchestrate(t *testing.T) {
	plan := nestedPermissionTestPlan("orchestrate", []byte(`{"provider_native_subagent":true,"adapter_id":"future-provider"}`))
	capability := NestedExecutorCapability{
		ExecutorID:             "future-executor",
		RegistrationID:         "test:future:v1",
		Provider:               "future-provider",
		EnforceablePermissions: []string{string(reporter.PermissionOrchestrate)},
		NativeBridge:           testNativeBridge{id: "test:future:v1", provider: "future-provider"},
	}
	result := EvaluateNestedDelegationCapability(plan.Items[0], capability)
	if result.Supported || result.Result != NestedDelegationOrchestrateUnsupported || result.ReasonCode != NestedReasonOrchestrateUnsupported {
		t.Fatalf("result = %#v, want orchestrate refusal", result)
	}
}

func TestNestedDelegationSupportMatrixDocumentation(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	data, err := os.ReadFile(filepath.Join(root, "docs", "reference", "runtime-capabilities.md"))
	if err != nil {
		t.Fatalf("read runtime capability reference: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"Adapter availability and `nested_subagents` advertising are not delegation support.",
		"provider_native_bridge_required",
		"orchestrate_unsupported",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("runtime capability reference missing %q", want)
		}
	}
}

type testNativeBridge struct {
	id       string
	provider string
	execute  ChildRunExecutor
}

func (b testNativeBridge) BridgeID() string { return b.id }

func (b testNativeBridge) Provider() string { return b.provider }

func (b testNativeBridge) Execute(ctx context.Context, request ChildExecutionRequest) (ChildRunResult, error) {
	if b.execute == nil {
		return ChildRunResult{Status: NestedStatusSucceeded}, nil
	}
	return b.execute(ctx, request)
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
