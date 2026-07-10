package orchestration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestScheduleNestedRunsFansOutWithConcurrencyLimitAndDeterministicResults(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()
	var mu sync.Mutex
	running := 0
	maxRunning := 0

	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		BaseBranch:       "main",
		ConcurrencyLimit: 2,
		MaxChildren:      3,
		Now:              now,
		Clock:            func() time.Time { return now },
		Children: []ChildRunPlan{
			{ID: "c", Issue: 3, Permission: "write", Required: true},
			{ID: "a", Issue: 1, Permission: "write", Required: true},
			{ID: "b", Issue: 2, Permission: "write", Required: true},
		},
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			mu.Lock()
			running++
			if running > maxRunning {
				maxRunning = running
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", report.Status)
	}
	if maxRunning != 2 {
		t.Fatalf("max concurrent children = %d, want 2", maxRunning)
	}
	var gotIDs []string
	for _, result := range report.Children {
		gotIDs = append(gotIDs, result.ID)
	}
	if !reflect.DeepEqual(gotIDs, []string{"c", "a", "b"}) {
		t.Fatalf("child result order = %#v, want plan order", gotIDs)
	}
	if report.Summary.RequiredCount != 3 || report.Summary.SucceededCount != 3 {
		t.Fatalf("summary = %#v", report.Summary)
	}

	events := readNestedEvents(t, repo, report.ParentRunID)
	for _, want := range []string{NestedEventChildQueued, NestedEventChildRunning, NestedEventChildFinished, NestedEventParentDone} {
		if !strings.Contains(events, want) {
			t.Fatalf("parent events missing %q:\n%s", want, events)
		}
	}
	if !strings.Contains(readNestedEvents(t, repo, report.Children[0].RunID), NestedEventChildFinished) {
		t.Fatalf("child run events missing finished transition")
	}
}

func TestScheduleNestedRunsChildRunIDsUseIndexForSlugCollisions(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()

	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 2,
		MaxChildren:      2,
		Now:              now,
		Clock:            func() time.Time { return now },
		Children: []ChildRunPlan{
			{ID: "a b", Issue: 1, Permission: "write", Required: true},
			{ID: "a-b", Issue: 2, Permission: "write", Required: true},
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if len(report.Children) != 2 {
		t.Fatalf("child count = %d, want 2", len(report.Children))
	}
	if report.Children[0].RunID == report.Children[1].RunID {
		t.Fatalf("child RunIDs collided: %q", report.Children[0].RunID)
	}
	gotRunIDs := []string{report.Children[0].RunID, report.Children[1].RunID}
	wantRunIDs := []string{
		"run-20260709T000000Z-child-0-a-b",
		"run-20260709T000000Z-child-1-a-b",
	}
	if !reflect.DeepEqual(gotRunIDs, wantRunIDs) {
		t.Fatalf("child RunIDs = %#v, want %#v", gotRunIDs, wantRunIDs)
	}
}

func TestScheduleNestedRunsRejectsDuplicateResolvedChildRunID(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()

	_, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 1,
		MaxChildren:      2,
		Now:              now,
		Children: []ChildRunPlan{
			{ID: "first", RunID: "run-20260709T000000Z-child-0-shared", Issue: 1, Permission: "write", Required: true},
			{ID: "second", RunID: "run-20260709T000000Z-child-0-shared", Issue: 2, Permission: "write", Required: true},
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate child run id") {
		t.Fatalf("duplicate resolved RunID error = %v", err)
	}
}

func TestScheduleNestedRunsRecordsFinishedEventForCancelledQueuedChild(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct {
		report NestedScheduleReport
		err    error
	}, 1)

	go func() {
		report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
			RepoPath:         repo,
			ParentRunID:      "run-20260709T000000Z-wave",
			ConcurrencyLimit: 1,
			MaxChildren:      2,
			Now:              now,
			Clock:            func() time.Time { return now },
			Children: []ChildRunPlan{
				{ID: "first", Issue: 1, Permission: "write", Required: true},
				{ID: "second", Issue: 2, Permission: "write", Required: true},
			},
			Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
				if child.ID == "first" {
					close(started)
					<-release
				}
				return ChildRunResult{Status: NestedStatusSucceeded}, nil
			},
		})
		done <- struct {
			report NestedScheduleReport
			err    error
		}{report: report, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first child to start")
	}
	cancel()
	close(release)
	var outcome struct {
		report NestedScheduleReport
		err    error
	}
	select {
	case outcome = <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for nested scheduler")
	}
	if outcome.err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", outcome.err)
	}

	var cancelled ChildRunResult
	for _, child := range outcome.report.Children {
		if child.ID == "second" {
			cancelled = child
			break
		}
	}
	if cancelled.Status != NestedStatusCancelled || cancelled.FinishedAt == "" {
		t.Fatalf("cancelled child = %#v, want cancelled with FinishedAt", cancelled)
	}
	childEvents := readNestedEvents(t, repo, cancelled.RunID)
	if !strings.Contains(childEvents, NestedEventChildFinished) || !strings.Contains(childEvents, `"status":"cancelled"`) {
		t.Fatalf("cancelled child events missing finished cancelled event:\n%s", childEvents)
	}
	parentEvents := readNestedEvents(t, repo, outcome.report.ParentRunID)
	if !strings.Contains(parentEvents, NestedEventChildFinished) || !strings.Contains(parentEvents, `"status":"cancelled"`) {
		t.Fatalf("parent events missing finished cancelled child event:\n%s", parentEvents)
	}
}

func TestScheduleNestedRunsRecordsFinishedEventForTerminalChildStatuses(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()
	statusByID := map[string]string{
		"abandoned": NestedStatusAbandoned,
		"cancelled": NestedStatusCancelled,
		"timed-out": NestedStatusTimedOut,
	}

	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 3,
		MaxChildren:      3,
		Now:              now,
		Clock:            func() time.Time { return now },
		Children: []ChildRunPlan{
			{ID: "abandoned", Issue: 1, Permission: "write", Required: true},
			{ID: "cancelled", Issue: 2, Permission: "write", Required: true},
			{ID: "timed-out", Issue: 3, Permission: "write", Required: true},
		},
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{Status: statusByID[child.ID]}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusFailed {
		t.Fatalf("parent status = %s, want failed", report.Status)
	}
	for _, child := range report.Children {
		events := readNestedEvents(t, repo, child.RunID)
		if !strings.Contains(events, NestedEventChildFinished) || !strings.Contains(events, `"status":"`+child.Status+`"`) {
			t.Fatalf("%s events missing finished %s event:\n%s", child.ID, child.Status, events)
		}
	}
}

func TestScheduleNestedRunsAggregatesRequiredFailuresPredictably(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()

	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 2,
		MaxChildren:      2,
		Now:              now,
		Clock:            func() time.Time { return now },
		Children: []ChildRunPlan{
			{ID: "optional-fail", Issue: 1, Permission: "read", Optional: true},
			{ID: "required-needs-human", Issue: 2, Permission: "write", Required: true},
		},
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			if child.Optional {
				return ChildRunResult{Status: NestedStatusFailed, Error: "optional child failed"}, nil
			}
			return ChildRunResult{Status: NestedStatusNeedsHuman, Error: "clarification needed"}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusNeedsHuman {
		t.Fatalf("status = %s, want needs-human", report.Status)
	}
	if report.Summary.FailedCount != 1 || report.Summary.NeedsHumanCount != 1 {
		t.Fatalf("summary = %#v", report.Summary)
	}

	report, err = ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000001Z-wave",
		ConcurrencyLimit: 1,
		MaxChildren:      1,
		Now:              now,
		Clock:            func() time.Time { return now },
		Children: []ChildRunPlan{
			{ID: "required-fail", Issue: 3, Permission: "write", Required: true},
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{Status: NestedStatusFailed, Error: "required child failed"}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusFailed {
		t.Fatalf("status = %s, want failed", report.Status)
	}
}

func TestScheduleNestedRunsRequiredSkippedBlocksParentSuccess(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()

	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 1,
		MaxChildren:      1,
		Now:              now,
		Clock:            func() time.Time { return now },
		Children: []ChildRunPlan{
			{ID: "required-skip", Issue: 1, Permission: "write", Required: true},
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{Status: NestedStatusSkipped}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusFailed {
		t.Fatalf("required skipped parent status = %s, want failed", report.Status)
	}

	report, err = ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000001Z-wave",
		ConcurrencyLimit: 1,
		MaxChildren:      2,
		Now:              now,
		Clock:            func() time.Time { return now },
		Children: []ChildRunPlan{
			{ID: "required-ok", Issue: 2, Permission: "write", Required: true},
			{ID: "optional-skip", Issue: 3, Permission: "read", Optional: true},
		},
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			if child.Optional {
				return ChildRunResult{Status: NestedStatusSkipped}, nil
			}
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusSucceeded {
		t.Fatalf("optional skipped parent status = %s, want succeeded", report.Status)
	}
}

func TestScheduleNestedRunsBudgetBlocksBeforeDispatch(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()
	maxAttempts := 1
	var dispatched []string

	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 1,
		MaxChildren:      2,
		Now:              now,
		Clock:            func() time.Time { return now },
		Budget:           config.GuardrailBudget{MaxTotalAttempts: &maxAttempts},
		Children: []ChildRunPlan{
			{ID: "first", Issue: 1, Permission: "write", Required: true},
			{ID: "second", Issue: 2, Permission: "write", Required: true},
		},
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			dispatched = append(dispatched, child.ID)
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if !reflect.DeepEqual(dispatched, []string{"first"}) {
		t.Fatalf("dispatched = %#v, want only first child", dispatched)
	}
	if report.Status != NestedStatusNeedsHuman {
		t.Fatalf("status = %s, want needs-human", report.Status)
	}
	if report.Children[1].Status != NestedStatusNeedsHuman ||
		!strings.Contains(report.Children[1].Error, "guardrails.budget.max_total_attempts") {
		t.Fatalf("second child = %#v", report.Children[1])
	}
	data, err := os.ReadFile(guardrails.LedgerPath(repo, report.ParentRunID, 2))
	if err != nil {
		t.Fatalf("ReadFile guardrail ledger: %v", err)
	}
	if !strings.Contains(string(data), `"status": "needs-human"`) {
		t.Fatalf("ledger missing needs-human status:\n%s", string(data))
	}
}

func TestScheduleNestedRunsCircuitBreakerConvertsNoProgressChildToNeedsHuman(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()
	maxWaves := 1

	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 1,
		MaxChildren:      1,
		Now:              now,
		Clock:            func() time.Time { return now },
		CircuitBreaker: config.GuardrailCircuitBreaker{
			MaxNoProgressWaves: &maxWaves,
		},
		Children: []ChildRunPlan{
			{ID: "stalled", Issue: 9, Permission: "write", Required: true},
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{Status: NestedStatusFailed, Error: "no progress"}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusNeedsHuman {
		t.Fatalf("status = %s, want needs-human", report.Status)
	}
	if report.Children[0].Status != NestedStatusNeedsHuman ||
		!strings.Contains(report.Children[0].Error, "guardrails.circuit_breaker.max_no_progress_waves") {
		t.Fatalf("child result = %#v", report.Children[0])
	}
}

func TestScheduleNestedRunsValidatesExplicitOptionalAndDepth(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()

	_, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 1,
		MaxChildren:      1,
		Now:              now,
		Children: []ChildRunPlan{
			{ID: "implicit", Issue: 1, Permission: "write"},
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "aggregation is required") {
		t.Fatalf("implicit optional error = %v", err)
	}

	_, err = ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 1,
		MaxChildren:      1,
		MaxDepth:         1,
		Now:              now,
		Children: []ChildRunPlan{
			{ID: "too-deep", Issue: 1, Permission: "write", Required: true, Depth: 2},
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds max depth") {
		t.Fatalf("depth error = %v", err)
	}
}

func TestParseChildPlanJSONStrictGoldenV1(t *testing.T) {
	raw := []byte(`{
  "schema_version": "loopcoder.child_plan.v1",
  "plan_id": "plan-run-20260709T000000Z-wave-001",
  "parent_run_id": "run-20260709T000000Z-wave",
  "root_run_id": "run-20260709T000000Z-wave",
  "parent_depth": 0,
  "max_depth": 2,
  "max_concurrency": 2,
  "created_at": "2026-07-09T00:00:00Z",
  "items": [
    {
      "child_key": "docs-pass",
      "title": "Review docs contract",
      "role": "worker",
      "scope": {
        "repo": ".",
        "paths": ["docs/specs/"],
        "issues": [646],
        "commands": ["go test ./internal/orchestration/..."]
      },
      "permission": "read-only",
      "depends_on": [],
      "aggregation": {
        "mode": "collect",
        "required": true,
        "include_report": true
      }
    }
  ]
}`)
	plan, err := ParseChildPlanJSON(raw)
	if err != nil {
		t.Fatalf("ParseChildPlanJSON returned error: %v", err)
	}
	if plan.SchemaVersion != ChildPlanSchemaVersionV1 || plan.PlanID != "plan-run-20260709T000000Z-wave-001" {
		t.Fatalf("parsed plan identity = %s %s", plan.SchemaVersion, plan.PlanID)
	}
	if got := plan.Items[0].Ordinal; got != 0 {
		t.Fatalf("child ordinal = %d, want 0", got)
	}

	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	asMap["unexpected"] = true
	strictRaw, err := json.Marshal(asMap)
	if err != nil {
		t.Fatalf("marshal strict fixture: %v", err)
	}
	if _, err := ParseChildPlanJSON(strictRaw); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field error = %v", err)
	}
}

func TestValidateChildPlanRejectsInvalidBoundedFields(t *testing.T) {
	base := func() ChildPlan {
		return ChildPlan{
			SchemaVersion:  ChildPlanSchemaVersionV1,
			PlanID:         "plan-run-20260709T000000Z-wave",
			ParentRunID:    "run-20260709T000000Z-wave",
			RootRunID:      "run-20260709T000000Z-wave",
			ParentDepth:    0,
			MaxDepth:       2,
			MaxConcurrency: 2,
			CreatedAt:      "2026-07-09T00:00:00Z",
			Items: []ChildRunPlan{{
				ChildKey:   "a",
				Title:      "A",
				Role:       "worker",
				Scope:      ChildScope{Repo: ".", Issues: []int{689}},
				Permission: "write",
				DependsOn:  []string{},
				Aggregation: ChildAggregation{
					Mode:          ChildAggregationCollect,
					Required:      true,
					IncludeReport: true,
				},
			}},
		}
	}
	tests := []struct {
		name string
		edit func(*ChildPlan)
		want string
	}{
		{name: "invalid permission", edit: func(p *ChildPlan) { p.Items[0].Permission = "admin" }, want: "permission must be one of"},
		{name: "duplicate child key", edit: func(p *ChildPlan) { p.Items = append(p.Items, p.Items[0]) }, want: "duplicate child_key"},
		{name: "unknown dependency", edit: func(p *ChildPlan) { p.Items[0].DependsOn = []string{"missing"} }, want: "depends on unknown"},
		{name: "cycle", edit: func(p *ChildPlan) {
			p.Items = append(p.Items, ChildRunPlan{
				ChildKey: "b", Title: "B", Role: "worker", Scope: ChildScope{Repo: ".", Issues: []int{690}},
				Permission: "read-only", DependsOn: []string{"a"},
				Aggregation: ChildAggregation{Mode: ChildAggregationCollect, Required: true, IncludeReport: true},
			})
			p.Items[0].DependsOn = []string{"b"}
		}, want: "cycle"},
		{name: "hard depth cap", edit: func(p *ChildPlan) { p.MaxDepth = NestedHardMaxDepth + 1 }, want: "hard maximum"},
		{name: "unbounded write scope", edit: func(p *ChildPlan) { p.Items[0].Scope = ChildScope{Repo: ".", Paths: []string{"**"}} }, want: "unbounded path scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := base()
			tt.edit(&plan)
			err := ValidateChildPlan(&plan)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateChildPlan error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestScheduleNestedRunsHonorsDependencies(t *testing.T) {
	repo := t.TempDir()
	now := nestedTestNow()
	var order []string
	report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
		RepoPath:         repo,
		ParentRunID:      "run-20260709T000000Z-wave",
		ConcurrencyLimit: 2,
		MaxChildren:      2,
		Now:              now,
		Clock:            func() time.Time { return now },
		Children: []ChildRunPlan{
			{ID: "second", Issue: 2, Permission: "write", Required: true, DependsOn: []string{"first"}},
			{ID: "first", Issue: 1, Permission: "write", Required: true},
		},
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			order = append(order, child.ChildKey)
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusSucceeded {
		t.Fatalf("status = %s, want succeeded", report.Status)
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("execution order = %#v, want dependency order", order)
	}
}

func TestScheduleNestedRunsPersistsDurablePlanBeforeLaunchAndReplaysIdempotently(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-run-20260709T000000Z-wave",
		ParentRunID:    "run-20260709T000000Z-wave",
		RootRunID:      "run-20260709T000000Z-wave",
		ParentDepth:    0,
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      "2026-07-09T00:00:00Z",
		Items: []ChildRunPlan{{
			ChildKey: "durable-child", Title: "Durable child", Role: "worker",
			Scope: ChildScope{Repo: ".", Issues: []int{689}}, Permission: "write", DependsOn: []string{},
			Aggregation: ChildAggregation{Mode: ChildAggregationCollect, Required: true, IncludeReport: true},
		}},
	}
	executions := 0
	run := func() {
		t.Helper()
		report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
			RepoPath:    repo,
			MaxChildren: 1,
			Now:         nestedTestNow(),
			Clock:       func() time.Time { return nestedTestNow() },
			Plan:        &plan,
			Store:       store,
			Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
				executions++
				var count int
				if err := store.WithTx(ctx, func(tx storage.Tx) error {
					return tx.QueryRow(ctx, `SELECT COUNT(*) FROM child_plans WHERE plan_id = ?`, plan.PlanID).Scan(&count)
				}); err != nil {
					t.Fatalf("query child_plans during execute: %v", err)
				}
				if count != 1 {
					t.Fatalf("child plan count during execute = %d, want durable before launch", count)
				}
				return ChildRunResult{Status: NestedStatusSucceeded}, nil
			},
		})
		if err != nil {
			t.Fatalf("ScheduleNestedRuns returned error: %v", err)
		}
		if report.Status != NestedStatusSucceeded {
			t.Fatalf("status = %s, want succeeded", report.Status)
		}
	}
	run()
	run()
	if executions != 2 {
		t.Fatalf("executions = %d, want replay to execute same child twice without duplicating graph", executions)
	}
	var plans, runs, edges int
	var edgeStatus string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM child_plans WHERE plan_id = ?`, plan.PlanID).Scan(&plans); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM runs WHERE root_run_id = ?`, plan.RootRunID).Scan(&runs); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_edges WHERE plan_id = ?`, plan.PlanID).Scan(&edges); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status FROM run_edges WHERE plan_id = ? AND child_key = ?`, plan.PlanID, "durable-child").Scan(&edgeStatus)
	}); err != nil {
		t.Fatalf("query durable graph: %v", err)
	}
	if plans != 1 || runs != 2 || edges != 1 {
		t.Fatalf("durable counts plans/runs/edges = %d/%d/%d, want 1/2/1", plans, runs, edges)
	}
	if edgeStatus != NestedStatusSucceeded {
		t.Fatalf("edge status = %q, want succeeded", edgeStatus)
	}
	events := readNestedEvents(t, repo, plan.ParentRunID)
	if !strings.Contains(events, NestedEventChildQueued) || !strings.Contains(events, NestedEventChildFinished) || !strings.Contains(events, `"status":"succeeded"`) {
		t.Fatalf("compatibility events do not mirror durable success:\n%s", events)
	}
}

func nestedTestNow() time.Time {
	return time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
}

func readNestedEvents(t *testing.T, repo, runID string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, ".loopcoder", "runs", runID, "events.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile events for %s: %v", runID, err)
	}
	return string(data)
}
