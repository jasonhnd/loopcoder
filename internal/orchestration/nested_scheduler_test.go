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
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
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

func TestValidateChildPlanDefaultsMaxConcurrencyToNestedSpec(t *testing.T) {
	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-run-20260709T000000Z-wave-001",
		ParentRunID:    "run-20260709T000000Z-wave",
		RootRunID:      "run-20260709T000000Z-wave",
		ParentDepth:    0,
		MaxDepth:       2,
		MaxConcurrency: 0,
		CreatedAt:      state.FormatTimestamp(nestedTestNow()),
		Items: []ChildRunPlan{{
			ChildKey:   "child-a",
			Title:      "child-a",
			Role:       "worker",
			Permission: string(reporter.PermissionWrite),
			Scope:      ChildScope{Repo: ".", Paths: []string{"internal/orchestration/nested_scheduler.go"}, Issues: []int{646}},
			Aggregation: ChildAggregation{
				Mode:          ChildAggregationCollect,
				Required:      true,
				IncludeReport: true,
			},
		}},
	}
	if err := ValidateChildPlan(&plan); err != nil {
		t.Fatalf("ValidateChildPlan returned error: %v", err)
	}
	if plan.MaxConcurrency != 3 {
		t.Fatalf("MaxConcurrency = %d, want nested default 3", plan.MaxConcurrency)
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
	var reports []NestedScheduleReport
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
		reports = append(reports, report)
	}
	run()
	run()
	if executions != 1 {
		t.Fatalf("executions = %d, want replay to reuse succeeded child without re-executing", executions)
	}
	if reports[1].Children[0].ReplayAction != ReplayActionReused {
		t.Fatalf("second replay action = %q, want reused", reports[1].Children[0].ReplayAction)
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

func TestScheduleNestedRunsResumesDurableNonTerminalChildUnderPersistedRunID(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	plan := durableReplayTestPlan()
	persistedRunID := "run-20260709T000000Z-child-0-resume-child"
	storedPlan := plan
	storedPlan.Items = cloneChildPlans(plan.Items)
	storedPlan.Items[0].RunID = persistedRunID
	if err := ValidateChildPlan(&storedPlan); err != nil {
		t.Fatalf("Validate stored plan: %v", err)
	}
	planJSON, err := json.Marshal(storedPlan)
	if err != nil {
		t.Fatalf("marshal stored plan: %v", err)
	}
	at := storedPlan.CreatedAt
	if err := storage.PersistChildPlanGraph(ctx, store,
		storage.RunNode{RunID: storedPlan.ParentRunID, RootRunID: storedPlan.RootRunID, Depth: 0, Origin: "nested_parent", Status: state.StatusRunning, CreatedAt: at, UpdatedAt: at},
		[]storage.RunNode{{RunID: persistedRunID, ParentRunID: storedPlan.ParentRunID, RootRunID: storedPlan.RootRunID, Depth: 1, Origin: "sub_agent", Status: state.StatusQueued, CreatedAt: at, UpdatedAt: at}},
		storage.ChildPlanRecord{PlanID: storedPlan.PlanID, ParentRunID: storedPlan.ParentRunID, RootRunID: storedPlan.RootRunID, SchemaVersion: storedPlan.SchemaVersion, MaxDepth: storedPlan.MaxDepth, MaxConcurrency: storedPlan.MaxConcurrency, PlanJSON: string(planJSON), CreatedAt: at},
		[]storage.RunEdgeRecord{{ParentRunID: storedPlan.ParentRunID, ChildRunID: persistedRunID, RootRunID: storedPlan.RootRunID, PlanID: storedPlan.PlanID, ChildKey: "resume-child", Depth: 1, Ordinal: 0, ScopeJSON: `{"repo":".","issues":[709]}`, Permission: "write", AggregationJSON: `{"mode":"collect","required":true,"include_report":true}`, Status: state.StatusQueued, CreatedAt: at, UpdatedAt: at}},
	); err != nil {
		t.Fatalf("seed durable queued child: %v", err)
	}
	beforePlans, beforeRuns, beforeEdges, beforeOrphans := countNestedDurableRows(t, ctx, store, storedPlan.RootRunID, storedPlan.PlanID)

	executions := 0
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow().Add(2 * time.Hour),
		Clock:       func() time.Time { return nestedTestNow().Add(2 * time.Hour) },
		Plan:        &plan,
		Store:       store,
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			executions++
			if child.RunID != persistedRunID {
				t.Fatalf("executed run_id = %q, want persisted %q", child.RunID, persistedRunID)
			}
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if executions != 1 {
		t.Fatalf("executions = %d, want resumed child execution", executions)
	}
	if got := report.Children[0].ReplayAction; got != ReplayActionResumed {
		t.Fatalf("ReplayAction = %q, want resumed", got)
	}
	if got := report.Children[0].RunID; got != persistedRunID {
		t.Fatalf("report run_id = %q, want persisted %q", got, persistedRunID)
	}
	afterPlans, afterRuns, afterEdges, afterOrphans := countNestedDurableRows(t, ctx, store, storedPlan.RootRunID, storedPlan.PlanID)
	if beforePlans != afterPlans || beforeRuns != afterRuns || beforeEdges != afterEdges || beforeOrphans != afterOrphans {
		t.Fatalf("durable counts changed plans/runs/edges/orphans %d/%d/%d/%d -> %d/%d/%d/%d, want unchanged",
			beforePlans, beforeRuns, beforeEdges, beforeOrphans, afterPlans, afterRuns, afterEdges, afterOrphans)
	}
}

func TestScheduleNestedRunsRejectsPlanMutationWithoutChangingSQLState(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	plan := durableReplayTestPlan()
	if _, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow(),
		Clock:       func() time.Time { return nestedTestNow() },
		Plan:        &plan,
		Store:       store,
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	}); err != nil {
		t.Fatalf("initial ScheduleNestedRuns returned error: %v", err)
	}
	beforePlans, beforeRuns, beforeEdges, beforeOrphans := countNestedDurableRows(t, ctx, store, plan.RootRunID, plan.PlanID)

	mutated := durableReplayTestPlan()
	mutated.Items[0].Title = "Changed title"
	_, err = ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow().Add(time.Hour),
		Clock:       func() time.Time { return nestedTestNow().Add(time.Hour) },
		Plan:        &mutated,
		Store:       store,
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			t.Fatal("mutated plan executed despite immutable fingerprint rejection")
			return ChildRunResult{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "mutation rejected") || !strings.Contains(err.Error(), "items[0].title") {
		t.Fatalf("mutation error = %v, want precise title fingerprint rejection", err)
	}
	afterPlans, afterRuns, afterEdges, afterOrphans := countNestedDurableRows(t, ctx, store, plan.RootRunID, plan.PlanID)
	if beforePlans != afterPlans || beforeRuns != afterRuns || beforeEdges != afterEdges || afterOrphans != 0 || beforeOrphans != 0 {
		t.Fatalf("post-rejection counts plans/runs/edges/orphans %d/%d/%d/%d -> %d/%d/%d/%d, want stable and no orphans",
			beforePlans, beforeRuns, beforeEdges, beforeOrphans, afterPlans, afterRuns, afterEdges, afterOrphans)
	}
}

func TestScheduleNestedRunsReplayPolicyForDurableStatuses(t *testing.T) {
	tests := []struct {
		name          string
		durableStatus string
		wantAction    string
		wantExecute   bool
		wantStatus    string
	}{
		{name: "failed", durableStatus: NestedStatusFailed, wantAction: ReplayActionRetried, wantExecute: true, wantStatus: NestedStatusSucceeded},
		{name: "cancelled", durableStatus: NestedStatusCancelled, wantAction: ReplayActionRetried, wantExecute: true, wantStatus: NestedStatusSucceeded},
		{name: "timed_out", durableStatus: NestedStatusTimedOut, wantAction: ReplayActionRetried, wantExecute: true, wantStatus: NestedStatusSucceeded},
		{name: "needs_human", durableStatus: NestedStatusNeedsHuman, wantAction: ReplayActionBlocked, wantExecute: false, wantStatus: NestedStatusNeedsHuman},
		{name: "interrupted", durableStatus: "interrupted", wantAction: ReplayActionResumed, wantExecute: true, wantStatus: NestedStatusSucceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			repo := t.TempDir()
			store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
			if err != nil {
				t.Fatalf("storage.Open: %v", err)
			}
			defer store.Close()

			plan := durableReplayTestPlan()
			plan.PlanID = "plan-replay-policy-" + tt.name
			persistedRunID := "run-20260709T000000Z-child-0-" + strings.ReplaceAll(tt.name, "_", "-")
			seedDurableReplayPlan(t, ctx, store, plan, persistedRunID, tt.durableStatus)

			executions := 0
			report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
				RepoPath:    repo,
				MaxChildren: 1,
				Now:         nestedTestNow().Add(time.Hour),
				Clock:       func() time.Time { return nestedTestNow().Add(time.Hour) },
				Plan:        &plan,
				Store:       store,
				Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
					executions++
					if child.RunID != persistedRunID {
						t.Fatalf("executed run_id = %q, want persisted %q", child.RunID, persistedRunID)
					}
					return ChildRunResult{Status: NestedStatusSucceeded}, nil
				},
			})
			if err != nil {
				t.Fatalf("ScheduleNestedRuns returned error: %v", err)
			}
			if gotExecute := executions > 0; gotExecute != tt.wantExecute {
				t.Fatalf("executed = %t, want %t", gotExecute, tt.wantExecute)
			}
			if got := report.Children[0].ReplayAction; got != tt.wantAction {
				t.Fatalf("ReplayAction = %q, want %q", got, tt.wantAction)
			}
			if got := report.Children[0].Status; got != tt.wantStatus {
				t.Fatalf("Status = %q, want %q", got, tt.wantStatus)
			}
			plans, runs, edges, orphans := countNestedDurableRows(t, ctx, store, plan.RootRunID, plan.PlanID)
			if plans != 1 || runs != 2 || edges != 1 || orphans != 0 {
				t.Fatalf("durable counts plans/runs/edges/orphans = %d/%d/%d/%d, want 1/2/1/0", plans, runs, edges, orphans)
			}
		})
	}
}

func TestScheduleNestedRunsConcurrentReplayUsesPlanCreatedAtIdentityTime(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	storeA, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open A: %v", err)
	}
	defer storeA.Close()
	storeB, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return nestedTestNow().Add(24 * time.Hour) }})
	if err != nil {
		t.Fatalf("storage.Open B: %v", err)
	}
	defer storeB.Close()

	planA := durableReplayTestPlan()
	planA.PlanID = "plan-concurrent-replay"
	planA.Items[0].ChildKey = "race-child"
	planA.Items[0].ID = ""
	planA.Items[0].Title = "Race child"
	planB := durableReplayTestPlan()
	planB.PlanID = planA.PlanID
	planB.Items[0].ChildKey = "race-child"
	planB.Items[0].ID = ""
	planB.Items[0].Title = "Race child"

	start := make(chan struct{})
	type outcome struct {
		report NestedScheduleReport
		err    error
	}
	done := make(chan outcome, 2)
	run := func(store storage.Store, plan ChildPlan, now time.Time) {
		<-start
		report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
			RepoPath:    repo,
			MaxChildren: 1,
			Now:         now,
			Clock:       func() time.Time { return now },
			Plan:        &plan,
			Store:       store,
			Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
				return ChildRunResult{Status: NestedStatusSucceeded}, nil
			},
		})
		done <- outcome{report: report, err: err}
	}
	go run(storeA, planA, nestedTestNow().Add(10*time.Hour))
	go run(storeB, planB, nestedTestNow().Add(48*time.Hour))
	close(start)
	first := <-done
	second := <-done
	for i, result := range []outcome{first, second} {
		if result.err != nil {
			t.Fatalf("concurrent replay %d returned error: %v", i, result.err)
		}
		if got := result.report.Children[0].RunID; got != "run-20260709T000000Z-child-0-race-child" {
			t.Fatalf("concurrent replay %d run_id = %q, want created_at-derived identity", i, got)
		}
	}
	plans, runs, edges, orphans := countNestedDurableRows(t, ctx, storeA, planA.RootRunID, planA.PlanID)
	if plans != 1 || runs != 2 || edges != 1 || orphans != 0 {
		t.Fatalf("concurrent durable counts plans/runs/edges/orphans = %d/%d/%d/%d, want 1/2/1/0", plans, runs, edges, orphans)
	}
}

func seedDurableReplayPlan(t *testing.T, ctx context.Context, store storage.Store, plan ChildPlan, childRunID, status string) {
	t.Helper()
	storedPlan := plan
	storedPlan.Items = cloneChildPlans(plan.Items)
	storedPlan.Items[0].RunID = childRunID
	if err := ValidateChildPlan(&storedPlan); err != nil {
		t.Fatalf("Validate stored plan: %v", err)
	}
	planJSON, err := json.Marshal(storedPlan)
	if err != nil {
		t.Fatalf("marshal stored plan: %v", err)
	}
	scopeJSON, err := json.Marshal(storedPlan.Items[0].Scope)
	if err != nil {
		t.Fatalf("marshal stored scope: %v", err)
	}
	aggregationJSON, err := json.Marshal(storedPlan.Items[0].Aggregation)
	if err != nil {
		t.Fatalf("marshal stored aggregation: %v", err)
	}
	at := storedPlan.CreatedAt
	if err := storage.PersistChildPlanGraph(ctx, store,
		storage.RunNode{RunID: storedPlan.ParentRunID, RootRunID: storedPlan.RootRunID, Depth: 0, Origin: "nested_parent", Status: state.StatusRunning, CreatedAt: at, UpdatedAt: at},
		[]storage.RunNode{{RunID: childRunID, ParentRunID: storedPlan.ParentRunID, RootRunID: storedPlan.RootRunID, Depth: 1, Origin: "sub_agent", Status: status, CreatedAt: at, UpdatedAt: at}},
		storage.ChildPlanRecord{PlanID: storedPlan.PlanID, ParentRunID: storedPlan.ParentRunID, RootRunID: storedPlan.RootRunID, SchemaVersion: storedPlan.SchemaVersion, MaxDepth: storedPlan.MaxDepth, MaxConcurrency: storedPlan.MaxConcurrency, PlanJSON: string(planJSON), CreatedAt: at},
		[]storage.RunEdgeRecord{{ParentRunID: storedPlan.ParentRunID, ChildRunID: childRunID, RootRunID: storedPlan.RootRunID, PlanID: storedPlan.PlanID, ChildKey: storedPlan.Items[0].ChildKey, Depth: 1, Ordinal: 0, ScopeJSON: string(scopeJSON), Permission: storedPlan.Items[0].Permission, AggregationJSON: string(aggregationJSON), Status: status, CreatedAt: at, UpdatedAt: at}},
	); err != nil {
		t.Fatalf("seed durable replay plan: %v", err)
	}
}

func durableReplayTestPlan() ChildPlan {
	return ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-run-20260709T000000Z-wave",
		ParentRunID:    "run-20260709T000000Z-wave",
		RootRunID:      "run-20260709T000000Z-wave",
		ParentDepth:    0,
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      "2026-07-09T00:00:00Z",
		Items: []ChildRunPlan{{
			ChildKey: "resume-child", Title: "Resume child", Role: "worker",
			Scope: ChildScope{Repo: ".", Issues: []int{709}}, Permission: "write", DependsOn: []string{},
			Aggregation: ChildAggregation{Mode: ChildAggregationCollect, Required: true, IncludeReport: true},
		}},
	}
}

func countNestedDurableRows(t *testing.T, ctx context.Context, store storage.Store, rootRunID, planID string) (plans, runs, edges, orphans int) {
	t.Helper()
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM child_plans WHERE plan_id = ?`, planID).Scan(&plans); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM runs WHERE root_run_id = ?`, rootRunID).Scan(&runs); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_edges WHERE plan_id = ?`, planID).Scan(&edges); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*)
			FROM runs r
			WHERE r.parent_run_id IS NOT NULL
				AND r.parent_run_id <> ''
				AND r.root_run_id = ?
				AND NOT EXISTS (
					SELECT 1 FROM run_edges e WHERE e.parent_run_id = r.parent_run_id AND e.child_run_id = r.id
				)`, rootRunID).Scan(&orphans)
	}); err != nil {
		t.Fatalf("query durable rows: %v", err)
	}
	return plans, runs, edges, orphans
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
