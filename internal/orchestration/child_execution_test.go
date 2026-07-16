package orchestration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestChildExecutionRequestSerializationGolden(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	plan, child := childExecutionRequestFixture()
	request, err := BuildChildExecutionRequest(repo, plan, child)
	if err != nil {
		t.Fatalf("BuildChildExecutionRequest: %v", err)
	}
	request.RepositoryIdentity = "project:test"
	request.CheckoutIdentity = "/repo"
	request.ScopedRepositoryIdentity = "/repo"
	request.CanonicalPaths = []string{"/repo/src"}
	request.MutationScope.Paths = []string{"/repo/src"}
	request.ContractFingerprint = childExecutionRequestFingerprint(request)
	got, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "child_execution_request_v1.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("child execution request golden mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
	var roundTrip ChildExecutionRequest
	if err := json.Unmarshal(got, &roundTrip); err != nil {
		t.Fatalf("Unmarshal golden: %v", err)
	}
	if !reflect.DeepEqual(roundTrip, request) {
		t.Fatalf("round trip changed request\ngot:  %#v\nwant: %#v", roundTrip, request)
	}
}

func TestBuildChildExecutionRequestCanonicalizesAndRejectsEscapes(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	plan, child := childExecutionRequestFixture()
	request, err := BuildChildExecutionRequest(repo, plan, child)
	if err != nil {
		t.Fatalf("valid request: %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(filepath.Join(repo, "src"))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if len(request.CanonicalPaths) != 1 || request.CanonicalPaths[0] != filepath.ToSlash(wantPath) {
		t.Fatalf("canonical paths = %#v, want %q", request.CanonicalPaths, filepath.ToSlash(wantPath))
	}
	if !reflect.DeepEqual(request.MutationScope.Paths, request.CanonicalPaths) {
		t.Fatalf("mutation paths = %#v, canonical paths = %#v", request.MutationScope.Paths, request.CanonicalPaths)
	}

	tests := []struct {
		name  string
		paths []string
		want  string
	}{
		{name: "relative escape", paths: []string{filepath.Join("..", "outside")}, want: "escapes registered checkout"},
		{name: "absolute escape", paths: []string{outside}, want: "escapes registered checkout"},
		{name: "wildcard scope", paths: []string{"src/**"}, want: "not a canonical concrete path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := child
			candidate.Scope = cloneChildScope(child.Scope)
			candidate.Scope.Paths = tt.paths
			_, err := BuildChildExecutionRequest(repo, plan, candidate)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("BuildChildExecutionRequest error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestBuildChildExecutionRequestRejectsPhysicalPathEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows junction escape is covered by CLI scope tests")
	}
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "owned.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	plan, child := childExecutionRequestFixture()
	child.Scope.Paths = []string{filepath.Join("escape", "owned.txt")}
	_, err := BuildChildExecutionRequest(repo, plan, child)
	if err == nil || !strings.Contains(err.Error(), "resolves outside registered checkout") {
		t.Fatalf("BuildChildExecutionRequest error = %v, want physical escape rejection", err)
	}
}

func TestScheduleNestedRunsRejectsMovedCheckoutFingerprintBeforeExecutor(t *testing.T) {
	ctx := context.Background()
	repoA := t.TempDir()
	repoB := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return childExecutionFixtureTime() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	plan, child := childExecutionRequestFixture()
	child.Scope.Paths = nil
	child.Scope.Issues = []int{1005}
	plan.Items = []ChildRunPlan{child}
	firstExecutions := 0
	first, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:                 repoA,
		Plan:                     &plan,
		Store:                    store,
		Now:                      childExecutionFixtureTime(),
		Clock:                    childExecutionFixtureTime,
		AllowUnbudgetedLocalTest: true,
		Execute: func(context.Context, ChildExecutionRequest) (ChildRunResult, error) {
			firstExecutions++
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil || first.Status != NestedStatusSucceeded || firstExecutions != 1 {
		t.Fatalf("initial schedule = status %q executions %d error %v", first.Status, firstExecutions, err)
	}
	secondExecutions := 0
	_, err = ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:                 repoB,
		Plan:                     &plan,
		Store:                    store,
		Now:                      childExecutionFixtureTime().Add(time.Hour),
		Clock:                    func() time.Time { return childExecutionFixtureTime().Add(time.Hour) },
		AllowUnbudgetedLocalTest: true,
		Execute: func(context.Context, ChildExecutionRequest) (ChildRunResult, error) {
			secondExecutions++
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("moved checkout error = %v, want fingerprint mismatch", err)
	}
	if secondExecutions != 0 {
		t.Fatalf("moved checkout executions = %d, want 0", secondExecutions)
	}
}

func TestScheduleNestedRunsReopensAndPassesExactPersistedContract(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return childExecutionFixtureTime() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	plan, child := childExecutionRequestFixture()
	child.Scope.Paths = nil
	child.Scope.Issues = []int{1005}
	plan.Items = []ChildRunPlan{child}
	normalized, err := normalizeChildRunPlans(plan.Items, NestedScheduleOptions{ParentRunID: plan.ParentRunID, RootRunID: plan.RootRunID, ParentDepth: plan.ParentDepth, MaxDepth: plan.MaxDepth, MaxChildren: 1}, childExecutionFixtureTime())
	if err != nil {
		t.Fatalf("normalizeChildRunPlans: %v", err)
	}
	plan.Items = normalized
	expected, err := BuildChildExecutionRequest(repo, plan, normalized[0])
	if err != nil {
		t.Fatalf("BuildChildExecutionRequest: %v", err)
	}
	if err := persistAcceptedChildPlan(ctx, NestedScheduleOptions{Store: store}, plan, []ChildExecutionRequest{expected}, childExecutionFixtureTime()); err != nil {
		t.Fatalf("persistAcceptedChildPlan: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}
	store, err = storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return childExecutionFixtureTime().Add(time.Minute) }})
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer store.Close()

	var observed ChildExecutionRequest
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:                 repo,
		Plan:                     &plan,
		Store:                    store,
		Now:                      childExecutionFixtureTime().Add(time.Minute),
		Clock:                    func() time.Time { return childExecutionFixtureTime().Add(time.Minute) },
		AllowUnbudgetedLocalTest: true,
		Execute: func(_ context.Context, request ChildExecutionRequest) (ChildRunResult, error) {
			observed = request
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil || report.Status != NestedStatusSucceeded {
		t.Fatalf("replayed schedule status=%q error=%v", report.Status, err)
	}
	if report.Children[0].ContractSchema != expected.SchemaVersion || report.Children[0].ContractFingerprint != expected.ContractFingerprint || report.Children[0].Permission != expected.Permission || !reflect.DeepEqual(report.Children[0].Scope, expected.Scope) {
		t.Fatalf("audit output lost execution contract: %#v", report.Children[0])
	}
	want := expected
	want.ClaimGeneration = 1
	want.LifecycleStatus = NestedStatusRunning
	if !reflect.DeepEqual(observed, want) {
		t.Fatalf("executor contract mismatch\ngot:  %#v\nwant: %#v", observed, want)
	}
	persisted, ok, err := storage.LoadChildExecutionRequest(ctx, store, expected.RunID)
	if err != nil || !ok {
		t.Fatalf("LoadChildExecutionRequest ok=%t error=%v", ok, err)
	}
	if persisted.Permission != expected.Permission || persisted.ScopeJSON == "" || persisted.ContractFingerprint != expected.ContractFingerprint || persisted.ClaimGeneration != 1 || persisted.LifecycleStatus != NestedStatusSucceeded {
		t.Fatalf("completed persisted contract = %#v", persisted)
	}
}

func TestScheduleNestedRunsRejectsPermissionAndScopeReplayMutationBeforeExecutor(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ChildRunPlan)
		want   string
	}{
		{name: "permission", mutate: func(child *ChildRunPlan) { child.Permission = "orchestrate" }, want: "items[0].permission"},
		{name: "scope", mutate: func(child *ChildRunPlan) { child.Scope.Paths = []string{"src"} }, want: "items[0].scope.paths"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, "src"), 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: childExecutionFixtureTime})
			if err != nil {
				t.Fatalf("storage.Open: %v", err)
			}
			defer store.Close()
			plan, child := childExecutionRequestFixture()
			child.Scope.Paths = nil
			child.Scope.Issues = []int{1005}
			plan.Items = []ChildRunPlan{child}
			if _, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
				RepoPath: repo, Plan: &plan, Store: store, Now: childExecutionFixtureTime(), Clock: childExecutionFixtureTime,
				AllowUnbudgetedLocalTest: true,
				Execute: func(context.Context, ChildExecutionRequest) (ChildRunResult, error) {
					return ChildRunResult{Status: NestedStatusSucceeded}, nil
				},
			}); err != nil {
				t.Fatalf("initial ScheduleNestedRuns: %v", err)
			}
			mutated := plan
			mutated.Items = cloneChildPlans(plan.Items)
			tt.mutate(&mutated.Items[0])
			executions := 0
			_, err = ScheduleNestedRuns(ctx, NestedScheduleOptions{
				RepoPath: repo, Plan: &mutated, Store: store, Now: childExecutionFixtureTime().Add(time.Minute), Clock: func() time.Time { return childExecutionFixtureTime().Add(time.Minute) },
				AllowUnbudgetedLocalTest: true,
				Execute: func(context.Context, ChildExecutionRequest) (ChildRunResult, error) {
					executions++
					return ChildRunResult{Status: NestedStatusSucceeded}, nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), "mutation rejected") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("mutation error = %v, want path %q", err, tt.want)
			}
			if executions != 0 {
				t.Fatalf("mutated replay executions = %d, want 0", executions)
			}
		})
	}
}

func TestScheduleNestedRunsFailsClosedWhenExecutorChangesContract(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: childExecutionFixtureTime})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	plan, child := childExecutionRequestFixture()
	child.Scope.Paths = nil
	child.Scope.Issues = []int{1005}
	plan.Items = []ChildRunPlan{child}
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath: repo, Plan: &plan, Store: store, Now: childExecutionFixtureTime(), Clock: childExecutionFixtureTime,
		AllowUnbudgetedLocalTest: true,
		Execute: func(context.Context, ChildExecutionRequest) (ChildRunResult, error) {
			return ChildRunResult{Status: NestedStatusSucceeded, Permission: "orchestrate"}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	if report.Children[0].Status != NestedStatusNeedsHuman || report.Children[0].Permission != "write" || !strings.Contains(report.Children[0].Error, "changed immutable permission") {
		t.Fatalf("executor contract violation report = %#v", report.Children[0])
	}
	persisted, ok, err := storage.LoadChildExecutionRequest(ctx, store, report.Children[0].RunID)
	if err != nil || !ok || persisted.Permission != "write" || persisted.LifecycleStatus != NestedStatusNeedsHuman {
		t.Fatalf("persisted contract after executor violation ok=%t record=%#v error=%v", ok, persisted, err)
	}
}

func childExecutionRequestFixture() (ChildPlan, ChildRunPlan) {
	created := state.FormatTimestamp(childExecutionFixtureTime())
	child := ChildRunPlan{
		ID:          "contract-child",
		ChildKey:    "contract-child",
		Title:       "Contract child",
		Role:        "worker",
		RunID:       "run-20260717T010203Z-child-0-contract-child",
		Issue:       1005,
		Scope:       ChildScope{Repo: ".", Paths: []string{"src"}, Issues: []int{1005}, Commands: []string{"go test ./internal/orchestration"}},
		Permission:  "write",
		DependsOn:   []string{},
		Aggregation: ChildAggregation{Mode: ChildAggregationCollect, Required: true, IncludeReport: true},
		Required:    true,
		Ordinal:     0,
		Depth:       1,
		Metadata: json.RawMessage(`{
			"issue_body":"Verify the immutable child contract.",
			"branch":"feature/issue-1005",
			"provider":"generic-adapter",
			"model":"capability-reasoning",
			"effort":"high",
			"timeout_seconds":120,
			"network_capabilities":["github-api"],
			"budget_reference_ids":["budget:test"]
		}`),
	}
	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-run-20260717T010203Z-contract",
		ParentRunID:    "run-20260717T010203Z-wave",
		RootRunID:      "run-20260717T010203Z-wave",
		ParentDepth:    0,
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      created,
		Items:          []ChildRunPlan{child},
	}
	return plan, child
}

func childExecutionFixtureTime() time.Time {
	return time.Date(2026, 7, 17, 1, 2, 3, 0, time.UTC)
}
