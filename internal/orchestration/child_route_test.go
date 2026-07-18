package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/state"
)

func TestScheduleNestedRunsRoutesChildrenToDifferentProviders(t *testing.T) {
	repo := t.TempDir()
	plan := ChildPlan{
		SchemaVersion:   ChildPlanSchemaVersionV1,
		PlanID:          "plan-route-mixed",
		ParentRunID:     "run-20260718T000000Z-parent",
		RootRunID:       "run-20260718T000000Z-parent",
		ParentDepth:     0,
		MaxDepth:        2,
		MaxConcurrency:  2,
		CreatedAt:       state.FormatTimestamp(nestedTestNow()),
		Items: []ChildRunPlan{
			{
				ChildKey:   "read-child",
				Title:      "read-child",
				Role:       "reviewer",
				Permission: "read-only",
				Scope:      ChildScope{Repo: ".", Paths: []string{"docs"}, Issues: []int{1}},
				Aggregation: ChildAggregation{Mode: "collect", Required: true},
				Metadata:   json.RawMessage(`{"prompt":"inspect docs"}`),
			},
			{
				ChildKey:   "write-child",
				Title:      "write-child",
				Role:       "worker",
				Permission: "write",
				Scope:      ChildScope{Repo: ".", Paths: []string{"src"}, Issues: []int{2}},
				Aggregation: ChildAggregation{Mode: "collect", Required: true},
				Metadata:   json.RawMessage(`{"prompt":"edit src"}`),
			},
		},
	}
	if err := PrepareNestedPlanForExecution(&plan, 8); err != nil {
		t.Fatalf("PrepareNestedPlanForExecution: %v", err)
	}

	seen := map[string]string{}
	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:                 repo,
		Plan:                     &plan,
		AllowUnbudgetedLocalTest: true,
		Now:                      nestedTestNow(),
		Clock:                    func() time.Time { return nestedTestNow() },
		RecordEvent:              func(string, string, state.Event) error { return nil },
		ResolveChildRoute: func(_ context.Context, request ChildExecutionRequest) (ChildRouteDecision, error) {
			switch request.Permission {
			case "read-only":
				return ChildRouteDecision{
					RoutingDecisionID: "route-read",
					AdapterID:         "claude",
					Model:             "claude-sonnet",
					Effort:            "high",
					Outcome:           "selected",
					ChosenReason:      "read-only headroom",
				}, nil
			case "write":
				return ChildRouteDecision{
					RoutingDecisionID: "route-write",
					AdapterID:         "codex",
					Model:             "codex-default",
					Effort:            "high",
					Outcome:           "selected",
					ChosenReason:      "bounded-write enforcement",
				}, nil
			default:
				return ChildRouteDecision{ZeroProviderLaunches: true, Outcome: "no_route"}, fmt.Errorf("unexpected permission")
			}
		},
		Execute: func(_ context.Context, request ChildExecutionRequest) (ChildRunResult, error) {
			seen[request.ChildKey] = request.Work.Provider
			if request.ProviderDecision.RoutingDecisionID == "" {
				t.Fatalf("child %s missing routing decision on launch", request.ChildKey)
			}
			if request.Work.Provider == "" || request.ProviderDecision.AdapterID == "" {
				t.Fatalf("child %s missing adapter on launch", request.ChildKey)
			}
			if request.Permission == "read-only" && request.Work.Provider != "claude" {
				t.Fatalf("read-only child provider = %q, want claude", request.Work.Provider)
			}
			if request.Permission == "write" && request.Work.Provider != "codex" {
				t.Fatalf("write child provider = %q, want codex", request.Work.Provider)
			}
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	if report.Status != NestedStatusSucceeded {
		t.Fatalf("status = %q, want succeeded; children=%+v", report.Status, report.Children)
	}
	if seen["read-child"] != "claude" || seen["write-child"] != "codex" {
		t.Fatalf("seen providers = %#v, want read=claude write=codex", seen)
	}
	for _, child := range report.Children {
		if child.RoutingDecisionID == "" || child.RouteAdapterID == "" {
			t.Fatalf("child %s missing route receipt: %+v", child.ChildKey, child)
		}
	}
}

func TestScheduleNestedRunsReadOnlyNeverSelectsWriteOnlyUnsafeProvider(t *testing.T) {
	repo := t.TempDir()
	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-route-readonly-gate",
		ParentRunID:    "run-20260718T000001Z-parent",
		RootRunID:      "run-20260718T000001Z-parent",
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      state.FormatTimestamp(nestedTestNow()),
		Items: []ChildRunPlan{
			{
				ChildKey:    "ro",
				Title:       "ro",
				Role:        "reviewer",
				Permission:  "read-only",
				Scope:       ChildScope{Repo: ".", Paths: []string{"docs"}, Issues: []int{1}},
				Aggregation: ChildAggregation{Mode: "collect", Required: true},
			},
		},
	}
	if err := PrepareNestedPlanForExecution(&plan, 8); err != nil {
		t.Fatalf("PrepareNestedPlanForExecution: %v", err)
	}
	launched := false
	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:                 repo,
		Plan:                     &plan,
		AllowUnbudgetedLocalTest: true,
		Now:                      nestedTestNow(),
		Clock:                    func() time.Time { return nestedTestNow() },
		RecordEvent:              func(string, string, state.Event) error { return nil },
		ResolveChildRoute: func(_ context.Context, request ChildExecutionRequest) (ChildRouteDecision, error) {
			// Resolver refuses providers that cannot enforce read-only nested mode.
			return ChildRouteDecision{
				RoutingDecisionID:    "route-blocked",
				Outcome:              "no_route",
				ZeroProviderLaunches: true,
				ChosenReason:         "provider lacks read-only nested enforcement",
			}, fmt.Errorf("provider lacks read-only nested enforcement")
		},
		Execute: func(context.Context, ChildExecutionRequest) (ChildRunResult, error) {
			launched = true
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	if launched {
		t.Fatal("executor launched despite no_route")
	}
	if report.Children[0].Status != NestedStatusNeedsHuman {
		t.Fatalf("status = %q, want needs-human", report.Children[0].Status)
	}
	if !strings.Contains(report.Children[0].Error, "read-only") {
		t.Fatalf("error = %q, want read-only gate", report.Children[0].Error)
	}
}

func TestScheduleNestedRunsExplicitPinStillPermissionChecked(t *testing.T) {
	repo := t.TempDir()
	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-route-pin",
		ParentRunID:    "run-20260718T000002Z-parent",
		RootRunID:      "run-20260718T000002Z-parent",
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      state.FormatTimestamp(nestedTestNow()),
		Items: []ChildRunPlan{
			{
				ChildKey:    "write-pin",
				Title:       "write-pin",
				Role:        "worker",
				Permission:  "write",
				Scope:       ChildScope{Repo: ".", Paths: []string{"src"}, Issues: []int{3}},
				Aggregation: ChildAggregation{Mode: "collect", Required: true},
				Metadata:    json.RawMessage(`{"provider":"claude","prompt":"edit"}`),
			},
		},
	}
	if err := PrepareNestedPlanForExecution(&plan, 8); err != nil {
		t.Fatalf("PrepareNestedPlanForExecution: %v", err)
	}
	var sawRequest ChildExecutionRequest
	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:                 repo,
		Plan:                     &plan,
		AllowUnbudgetedLocalTest: true,
		Now:                      nestedTestNow(),
		Clock:                    func() time.Time { return nestedTestNow() },
		RecordEvent:              func(string, string, state.Event) error { return nil },
		ResolveChildRoute: func(_ context.Context, request ChildExecutionRequest) (ChildRouteDecision, error) {
			sawRequest = request
			if request.Work.Provider != "claude" && request.ProviderDecision.AdapterID != "claude" {
				t.Fatalf("pin not visible on contract: work=%q decision=%q", request.Work.Provider, request.ProviderDecision.AdapterID)
			}
			// Explicit pin for write on claude is refused (no bounded-write nested adapter).
			return ChildRouteDecision{
				RoutingDecisionID:    "route-pin-refused",
				AdapterID:            "claude",
				Outcome:              "no_route",
				ZeroProviderLaunches: true,
				ChosenReason:         "provider claude has no registered nested bounded-write adapter",
			}, fmt.Errorf("provider claude has no registered nested bounded-write adapter")
		},
		Execute: func(context.Context, ChildExecutionRequest) (ChildRunResult, error) {
			t.Fatal("should not launch")
			return ChildRunResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	if sawRequest.ChildKey != "write-pin" {
		t.Fatalf("resolver not called: %#v", sawRequest)
	}
	if report.Children[0].Status != NestedStatusNeedsHuman {
		t.Fatalf("status = %q, want needs-human", report.Children[0].Status)
	}
}

func TestScheduleNestedRunsReusesRouteDecisionOnReplay(t *testing.T) {
	repo := t.TempDir()
	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-route-replay",
		ParentRunID:    "run-20260718T000003Z-parent",
		RootRunID:      "run-20260718T000003Z-parent",
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      state.FormatTimestamp(nestedTestNow()),
		Items: []ChildRunPlan{
			{
				ChildKey:    "child-a",
				Title:       "child-a",
				Role:        "worker",
				Permission:  "write",
				Scope:       ChildScope{Repo: ".", Paths: []string{"src"}, Issues: []int{4}},
				Aggregation: ChildAggregation{Mode: "collect", Required: true},
				Metadata:    json.RawMessage(`{"prompt":"work"}`),
			},
		},
	}
	if err := PrepareNestedPlanForExecution(&plan, 8); err != nil {
		t.Fatalf("PrepareNestedPlanForExecution: %v", err)
	}
	calls := 0
	resolver := func(_ context.Context, request ChildExecutionRequest) (ChildRouteDecision, error) {
		calls++
		return ChildRouteDecision{
			RoutingDecisionID: "route-stable",
			AdapterID:         "grok",
			Model:             "grok-code",
			Effort:            "high",
			Outcome:           "selected",
			Replayed:          calls > 1,
		}, nil
	}
	execute := func(_ context.Context, request ChildExecutionRequest) (ChildRunResult, error) {
		if request.ProviderDecision.RoutingDecisionID != "route-stable" || request.Work.Provider != "grok" {
			t.Fatalf("unexpected route on launch: %+v", request.ProviderDecision)
		}
		return ChildRunResult{Status: NestedStatusSucceeded}, nil
	}
	opts := NestedScheduleOptions{
		RepoPath:                 repo,
		Plan:                     &plan,
		AllowUnbudgetedLocalTest: true,
		Now:                      nestedTestNow(),
		Clock:                    func() time.Time { return nestedTestNow() },
		RecordEvent:              func(string, string, state.Event) error { return nil },
		ResolveChildRoute:        resolver,
		Execute:                  execute,
	}
	first, err := ScheduleNestedRuns(context.Background(), opts)
	if err != nil {
		t.Fatalf("first ScheduleNestedRuns: %v", err)
	}
	if first.Status != NestedStatusSucceeded {
		t.Fatalf("first status = %q", first.Status)
	}
	// Second schedule without store reuse still asks resolver; deterministic
	// decision id must apply the same adapter.
	second, err := ScheduleNestedRuns(context.Background(), opts)
	if err != nil {
		t.Fatalf("second ScheduleNestedRuns: %v", err)
	}
	if second.Status != NestedStatusSucceeded {
		t.Fatalf("second status = %q children=%+v", second.Status, second.Children)
	}
	if calls < 2 {
		t.Fatalf("resolver calls = %d, want >= 2", calls)
	}
	if first.Children[0].RouteAdapterID != "grok" || second.Children[0].RouteAdapterID != "grok" {
		t.Fatalf("route adapters first=%q second=%q", first.Children[0].RouteAdapterID, second.Children[0].RouteAdapterID)
	}
}

func TestScheduleNestedRunsParentCancelBlocksNewRouting(t *testing.T) {
	repo := t.TempDir()
	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-route-cancel",
		ParentRunID:    "run-20260718T000004Z-parent",
		RootRunID:      "run-20260718T000004Z-parent",
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      state.FormatTimestamp(nestedTestNow()),
		Items: []ChildRunPlan{
			{
				ChildKey:    "child-cancel",
				Title:       "child-cancel",
				Role:        "worker",
				Permission:  "write",
				Scope:       ChildScope{Repo: ".", Paths: []string{filepath.ToSlash("src")}, Issues: []int{5}},
				Aggregation: ChildAggregation{Mode: "collect", Required: true},
			},
		},
	}
	if err := PrepareNestedPlanForExecution(&plan, 8); err != nil {
		t.Fatalf("PrepareNestedPlanForExecution: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	launched := false
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:                 repo,
		Plan:                     &plan,
		AllowUnbudgetedLocalTest: true,
		Now:                      nestedTestNow(),
		Clock:                    func() time.Time { return nestedTestNow() },
		RecordEvent:              func(string, string, state.Event) error { return nil },
		ResolveChildRoute: func(context.Context, ChildExecutionRequest) (ChildRouteDecision, error) {
			t.Fatal("resolver should not run after parent cancel")
			return ChildRouteDecision{}, nil
		},
		Execute: func(context.Context, ChildExecutionRequest) (ChildRunResult, error) {
			launched = true
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	if launched {
		t.Fatal("executor launched after parent cancel")
	}
	if report.Children[0].Status != NestedStatusNeedsHuman {
		t.Fatalf("status = %q, want needs-human", report.Children[0].Status)
	}
	if !strings.Contains(report.Children[0].Error, "cancelled") {
		t.Fatalf("error = %q, want cancelled", report.Children[0].Error)
	}
}

func TestApplyChildRouteDecisionUpdatesFingerprint(t *testing.T) {
	repo := t.TempDir()
	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-apply",
		ParentRunID:    "run-20260718T000005Z-parent",
		RootRunID:      "run-20260718T000005Z-parent",
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      state.FormatTimestamp(nestedTestNow()),
		Items: []ChildRunPlan{
			{
				ChildKey:    "child",
				Title:       "child",
				Role:        "worker",
				RunID:       "run-20260718T000005Z-child",
				Permission:  "write",
				Scope:       ChildScope{Repo: ".", Paths: []string{"src"}, Issues: []int{6}},
				Aggregation: ChildAggregation{Mode: "collect", Required: true},
				Metadata:    json.RawMessage(`{"prompt":"work"}`),
			},
		},
	}
	request, err := BuildChildExecutionRequest(repo, plan, plan.Items[0])
	if err != nil {
		t.Fatalf("BuildChildExecutionRequest: %v", err)
	}
	before := request.ContractFingerprint
	applied, err := ApplyChildRouteDecision(request, ChildRouteDecision{
		RoutingDecisionID: "route-1",
		AdapterID:         "codex",
		Model:             "gpt-5",
		Effort:            "high",
	})
	if err != nil {
		t.Fatalf("ApplyChildRouteDecision: %v", err)
	}
	if applied.ContractFingerprint == "" || applied.ContractFingerprint == before {
		t.Fatalf("fingerprint did not change: before=%q after=%q", before, applied.ContractFingerprint)
	}
	if applied.Work.Provider != "codex" || applied.ProviderDecision.AdapterID != "codex" {
		t.Fatalf("provider not applied: %+v", applied)
	}
}
