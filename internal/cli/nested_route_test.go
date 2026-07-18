package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/routing"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestNestedPermissionSafeAdapterMatrix(t *testing.T) {
	if err := nestedPermissionSafeAdapter("read-only", "claude"); err != nil {
		t.Fatalf("claude read-only: %v", err)
	}
	if err := nestedPermissionSafeAdapter("write", "claude"); err == nil {
		t.Fatal("claude write should be refused")
	}
	if err := nestedPermissionSafeAdapter("write", "codex"); err != nil {
		t.Fatalf("codex write: %v", err)
	}
	if err := nestedPermissionSafeAdapter("read-only", "unknown"); err == nil {
		t.Fatal("unknown read-only should be refused")
	}
}

func TestNestedChildRouteProductionUsesDecideAndPermissionGate(t *testing.T) {
	var captured routing.StoredRouteRequest
	decide := func(_ context.Context, _ storage.Store, request routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
		captured = request
		return routing.RouteOperationResult{
			Outcome: routing.RouteOutcomeSelected,
			Decision: routing.RoutingDecision{
				RoutingDecisionID: "rd-nested-1",
				DecisionStatus:    routing.DecisionStatusSelected,
				ChosenCandidateID: "cand-1",
				ChosenReason:      "eligible",
				EligibleCandidates: []routing.Candidate{{
					RoutingCandidateID: "cand-1",
					AdapterID:          "codex",
					ModelCapabilityID:  "model-codex",
					CanonicalModelID:   "codex-model",
					InvocationProfileKey: "default",
				}},
			},
		}, nil
	}
	// nestedChildRouteProduction needs a registered project. Use a mock that
	// fails closed before store when repo is unregistered — exercise permission
	// gate via nestedPermissionSafeAdapter through decide returning claude for write.
	request := orchestration.ChildExecutionRequest{
		SchemaVersion: orchestration.ChildExecutionRequestSchemaVersionV1,
		ParentRunID:   "parent",
		PlanID:        "plan",
		ID:            "child",
		ChildKey:      "child",
		RunID:         "child-run",
		Title:         "child",
		Role:          "worker",
		Permission:    "write",
		Scope: orchestration.ChildScope{
			Repo:   ".",
			Paths:  []string{"src"},
			Issues: []int{1},
		},
		Work: orchestration.ChildExecutionWork{Instructions: "edit"},
	}
	// Without registered project the function fails before decide — that is the
	// production fail-closed path.
	_, err := nestedChildRouteProduction(context.Background(), request, NestedChildRouteInput{
		RepoPath:       t.TempDir(),
		ParentRunID:    "parent",
		Now:            time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC),
		PermissionSafe: nestedPermissionSafeAdapter,
	}, decide)
	if err == nil {
		t.Fatal("expected unregistered project failure")
	}
	if !strings.Contains(err.Error(), "registered project") {
		t.Fatalf("error = %q, want registered project", err.Error())
	}
	if captured.DecisionKey != "" {
		t.Fatalf("decide should not run before registration: %#v", captured)
	}
}

func TestNestedChildRouteProductionRefusesOrchestrateAndNativeDelegation(t *testing.T) {
	decide := func(context.Context, storage.Store, routing.StoredRouteRequest) (routing.RouteOperationResult, error) {
		t.Fatal("decide should not run")
		return routing.RouteOperationResult{}, nil
	}
	req := orchestration.ChildExecutionRequest{
		Permission: "orchestrate",
		ChildKey:   "orch",
		PlanID:     "plan",
	}
	if _, err := nestedChildRouteProduction(context.Background(), req, NestedChildRouteInput{RepoPath: t.TempDir(), PermissionSafe: nestedPermissionSafeAdapter}, decide); err == nil {
		t.Fatal("orchestrate should be refused")
	}
	req = orchestration.ChildExecutionRequest{
		Permission:   "write",
		ChildKey:     "native",
		PlanID:       "plan",
		Capabilities: orchestration.ChildExecutionCapabilities{Delegation: []string{"provider_native"}},
	}
	if _, err := nestedChildRouteProduction(context.Background(), req, NestedChildRouteInput{RepoPath: t.TempDir(), PermissionSafe: nestedPermissionSafeAdapter}, decide); err == nil {
		t.Fatal("native delegation should be refused")
	}
}

func TestNestedChildRouteResolverNilForTestSubprocess(t *testing.T) {
	resolver := nestedChildRouteResolver(nestedRunOptions{Provider: nestedTestSubprocessProvider}, false, time.Now)
	if resolver != nil {
		t.Fatal("test-subprocess must not enable production nested routing")
	}
}

func TestValidateNestedPlanProvidersAllowsDistinctChildPinsWhenUnpinnedGlobally(t *testing.T) {
	plan := orchestration.ChildPlan{
		Items: []orchestration.ChildRunPlan{
			{
				ChildKey:    "ro",
				Permission:  "read-only",
				Metadata:    []byte(`{"provider":"claude"}`),
			},
			{
				ChildKey:    "wr",
				Permission:  "write",
				Metadata:    []byte(`{"provider":"codex"}`),
			},
		},
	}
	if err := validateNestedPlanProviders(plan, ""); err != nil {
		t.Fatalf("unpinned multi-provider pins: %v", err)
	}
	if err := validateNestedPlanProviders(plan, "codex"); err == nil {
		t.Fatal("global codex pin should reject claude child pin")
	}
}
