package orchestration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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
	release := make(chan struct{})
	var releaseOnce sync.Once

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
			if running == 2 {
				releaseOnce.Do(func() {
					close(release)
				})
			}
			mu.Unlock()
			waitForNestedSignal(t, release, "two concurrent children")
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
	firstRunning := make(chan struct{})
	release := make(chan struct{})
	var firstRunningOnce sync.Once
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
			RecordEvent: func(repoPath, runID string, event state.Event) error {
				if runID == "run-20260709T000000Z-wave" && event.Event == NestedEventChildRunning {
					var details nestedChildEventDetails
					detailsJSON, err := json.Marshal(event.Details)
					if err != nil {
						return err
					}
					if err := json.Unmarshal(detailsJSON, &details); err != nil {
						return err
					}
					if details.Child.ID == "first" {
						firstRunningOnce.Do(func() {
							close(firstRunning)
						})
					}
				}
				return state.AppendEvent(repoPath, runID, event)
			},
			Execute: func(ctx context.Context, child ChildRunPlan) (ChildRunResult, error) {
				if child.ID == "first" {
					<-release
				}
				if child.ID == "second" {
					if err := ctx.Err(); err != nil {
						return ChildRunResult{}, err
					}
					return ChildRunResult{Status: NestedStatusFailed, Error: "second child executed before cancellation"}, nil
				}
				return ChildRunResult{Status: NestedStatusSucceeded}, nil
			},
		})
		done <- struct {
			report NestedScheduleReport
			err    error
		}{report: report, err: err}
	}()

	waitForNestedSignal(t, firstRunning, "first child to start")
	cancel()
	close(release)
	outcome := receiveNestedTestValue(t, done, "nested scheduler")
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
	if report.Status != NestedStatusNeedsHuman {
		t.Fatalf("parent status = %s, want needs-human", report.Status)
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
	if report.Status != NestedStatusNeedsHuman {
		t.Fatalf("status = %s, want needs-human", report.Status)
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
	if report.Status != NestedStatusNeedsHuman {
		t.Fatalf("required skipped parent status = %s, want needs-human", report.Status)
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

func TestScheduleNestedRunsAggregationModes(t *testing.T) {
	tests := []struct {
		name        string
		child       ChildRunPlan
		childStatus string
		wantStatus  string
	}{
		{
			name: "gate optional failure blocks parent",
			child: ChildRunPlan{
				ID: "optional-gate-fail", Issue: 1, Permission: "read-only",
				Aggregation: ChildAggregation{Mode: ChildAggregationGate, Required: false, IncludeReport: true},
			},
			childStatus: NestedStatusFailed,
			wantStatus:  NestedStatusNeedsHuman,
		},
		{
			name: "ignore required failure does not block parent",
			child: ChildRunPlan{
				ID: "ignored-required-fail", Issue: 2, Permission: "write",
				Aggregation: ChildAggregation{Mode: ChildAggregationIgnore, Required: true, IncludeReport: true},
			},
			childStatus: NestedStatusFailed,
			wantStatus:  NestedStatusSucceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			now := nestedTestNow()
			report, err := ScheduleNestedRuns(context.Background(), NestedScheduleOptions{
				RepoPath:         repo,
				ParentRunID:      "run-20260709T000000Z-wave",
				ConcurrencyLimit: 1,
				MaxChildren:      1,
				Now:              now,
				Clock:            func() time.Time { return now },
				Children:         []ChildRunPlan{tt.child},
				Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
					return ChildRunResult{Status: tt.childStatus}, nil
				},
			})
			if err != nil {
				t.Fatalf("ScheduleNestedRuns returned error: %v", err)
			}
			if report.Status != tt.wantStatus {
				t.Fatalf("parent status = %s, want %s", report.Status, tt.wantStatus)
			}
		})
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

func TestScheduleNestedRunsPersistsOptionalCollectFailureParentStatus(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	plan := ChildPlan{
		SchemaVersion:  ChildPlanSchemaVersionV1,
		PlanID:         "plan-run-20260709T000000Z-wave-optional",
		ParentRunID:    "run-20260709T000000Z-wave",
		RootRunID:      "run-20260709T000000Z-wave",
		ParentDepth:    0,
		MaxDepth:       2,
		MaxConcurrency: 1,
		CreatedAt:      "2026-07-09T00:00:00Z",
		Items: []ChildRunPlan{
			{
				ChildKey: "required-ok", Title: "Required OK", Role: "worker",
				Scope: ChildScope{Repo: ".", Issues: []int{710}}, Permission: "write", DependsOn: []string{},
				Aggregation: ChildAggregation{Mode: ChildAggregationCollect, Required: true, IncludeReport: true},
			},
			{
				ChildKey: "optional-fail", Title: "Optional fail", Role: "worker",
				Scope: ChildScope{Repo: ".", Issues: []int{710}}, Permission: "read-only", DependsOn: []string{},
				Aggregation: ChildAggregation{Mode: ChildAggregationCollect, Required: false, IncludeReport: true},
			},
		},
	}
	parentDoneSawDurableTerminal := false
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 2,
		Now:         nestedTestNow(),
		Clock:       func() time.Time { return nestedTestNow() },
		Plan:        &plan,
		Store:       store,
		RecordEvent: func(repoPath, runID string, event state.Event) error {
			if event.Event == NestedEventParentDone {
				var status, endedAt string
				if err := store.WithTx(ctx, func(tx storage.Tx) error {
					return tx.QueryRow(ctx, `SELECT status, COALESCE(ended_at, '') FROM runs WHERE id = ?`, plan.ParentRunID).Scan(&status, &endedAt)
				}); err != nil {
					t.Fatalf("query parent before compatibility event: %v", err)
				}
				parentDoneSawDurableTerminal = status == NestedStatusSucceededWithOptionalFailures && endedAt != ""
			}
			return state.AppendEvent(repoPath, runID, event)
		},
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			var runStatus, edgeStatus string
			if err := store.WithTx(ctx, func(tx storage.Tx) error {
				if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = ?`, child.RunID).Scan(&runStatus); err != nil {
					return err
				}
				return tx.QueryRow(ctx, `SELECT status FROM run_edges WHERE parent_run_id = ? AND child_run_id = ?`, plan.ParentRunID, child.RunID).Scan(&edgeStatus)
			}); err != nil {
				t.Fatalf("query child before execute: %v", err)
			}
			if runStatus != NestedStatusRunning || edgeStatus != NestedStatusRunning {
				t.Fatalf("child %s durable status before execute = %s/%s, want running/running", child.ChildKey, runStatus, edgeStatus)
			}
			if child.ChildKey == "optional-fail" {
				return ChildRunResult{Status: NestedStatusFailed, Error: "optional child failed"}, nil
			}
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if report.Status != NestedStatusSucceededWithOptionalFailures {
		t.Fatalf("parent report status = %s, want %s", report.Status, NestedStatusSucceededWithOptionalFailures)
	}
	if !parentDoneSawDurableTerminal {
		t.Fatal("parent finished compatibility event was emitted before durable terminal parent status")
	}
	var parentStatus, parentEndedAt string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, COALESCE(ended_at, '') FROM runs WHERE id = ?`, plan.ParentRunID).Scan(&parentStatus, &parentEndedAt)
	}); err != nil {
		t.Fatalf("query durable parent: %v", err)
	}
	if parentStatus != NestedStatusSucceededWithOptionalFailures || parentEndedAt == "" {
		t.Fatalf("durable parent status/ended_at = %q/%q, want optional-failures with ended_at", parentStatus, parentEndedAt)
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
		{name: "failed", durableStatus: NestedStatusFailed, wantAction: ReplayActionReused, wantExecute: false, wantStatus: NestedStatusFailed},
		{name: "cancelled", durableStatus: NestedStatusCancelled, wantAction: ReplayActionReused, wantExecute: false, wantStatus: NestedStatusCancelled},
		{name: "timed_out", durableStatus: NestedStatusTimedOut, wantAction: ReplayActionReused, wantExecute: false, wantStatus: NestedStatusTimedOut},
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
	executeStarted := make(chan struct{})
	releaseExecute := make(chan struct{})
	var executeCount int64
	var startedOnce int32
	type outcome struct {
		report NestedScheduleReport
		err    error
	}
	done := make(chan outcome, 2)
	runtimeNow := nestedTestNow().Add(12 * time.Hour)
	run := func(store storage.Store, plan ChildPlan, now time.Time) {
		<-start
		report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
			RepoPath:     repo,
			MaxChildren:  1,
			Now:          now,
			Clock:        func() time.Time { return now },
			RuntimeClock: func() time.Time { return runtimeNow },
			Plan:         &plan,
			Store:        store,
			RecordEvent: func(string, string, state.Event) error {
				return nil
			},
			Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
				atomic.AddInt64(&executeCount, 1)
				if atomic.CompareAndSwapInt32(&startedOnce, 0, 1) {
					close(executeStarted)
				}
				<-releaseExecute
				return ChildRunResult{Status: NestedStatusSucceeded}, nil
			},
		})
		done <- outcome{report: report, err: err}
	}
	go run(storeA, planA, nestedTestNow().Add(10*time.Hour))
	go run(storeB, planB, nestedTestNow().Add(48*time.Hour))
	close(start)
	waitForNestedSignal(t, executeStarted, "first provider execution")
	first := receiveNestedTestValue(t, done, "active-owner observer")
	if first.err != nil {
		t.Fatalf("active-owner observer returned error before release: %v", first.err)
	}
	if got := first.report.Children[0].ClaimOutcome; got != storage.ClaimOutcomeAlreadyRunning {
		t.Fatalf("first completed replay claim outcome = %q, want already-running", got)
	}
	if got := atomic.LoadInt64(&executeCount); got != 1 {
		t.Fatalf("execute count before release = %d, want 1", got)
	}
	close(releaseExecute)
	second := <-done
	for i, result := range []outcome{first, second} {
		if result.err != nil {
			t.Fatalf("concurrent replay %d returned error: %v", i, result.err)
		}
		if got := result.report.Children[0].RunID; got != "run-20260709T000000Z-child-0-race-child" {
			t.Fatalf("concurrent replay %d run_id = %q, want created_at-derived identity", i, got)
		}
	}
	if got := atomic.LoadInt64(&executeCount); got != 1 {
		t.Fatalf("execute count = %d, want exactly one provider execution", got)
	}
	plans, runs, edges, orphans := countNestedDurableRows(t, ctx, storeA, planA.RootRunID, planA.PlanID)
	if plans != 1 || runs != 2 || edges != 1 || orphans != 0 {
		t.Fatalf("concurrent durable counts plans/runs/edges/orphans = %d/%d/%d/%d, want 1/2/1/0", plans, runs, edges, orphans)
	}
}

func TestScheduleNestedRunsConcurrentReplayTwoProcessesObservesActiveOwner(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "loopcoder.db")
	activePath := filepath.Join(tmp, "active")
	releasePath := filepath.Join(tmp, "release")
	duplicatePath := filepath.Join(tmp, "duplicate")
	reportAPath := filepath.Join(tmp, "a.json")
	reportBPath := filepath.Join(tmp, "b.json")

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	startHelper := func(reportPath string, now string) *exec.Cmd {
		cmd := exec.Command(exe, "-test.run", "^TestNestedReplayHelperProcess$", "-test.v=false")
		cmd.Env = append(os.Environ(),
			"LOOPCODER_NESTED_REPLAY_HELPER=1",
			"LOOPCODER_NESTED_REPLAY_REPO="+repo,
			"LOOPCODER_NESTED_REPLAY_DB="+dbPath,
			"LOOPCODER_NESTED_REPLAY_ACTIVE="+activePath,
			"LOOPCODER_NESTED_REPLAY_RELEASE="+releasePath,
			"LOOPCODER_NESTED_REPLAY_DUPLICATE="+duplicatePath,
			"LOOPCODER_NESTED_REPLAY_REPORT="+reportPath,
			"LOOPCODER_NESTED_REPLAY_NOW="+now,
		)
		return cmd
	}

	cmdA := startHelper(reportAPath, nestedTestNow().Add(10*time.Hour).Format(time.RFC3339Nano))
	var outA strings.Builder
	cmdA.Stdout = &outA
	cmdA.Stderr = &outA
	if err := cmdA.Start(); err != nil {
		t.Fatalf("start helper A: %v", err)
	}
	waitForFile(t, activePath)

	cmdB := startHelper(reportBPath, nestedTestNow().Add(10*time.Hour+time.Minute).Format(time.RFC3339Nano))
	outB, err := cmdB.CombinedOutput()
	if err != nil {
		t.Fatalf("helper B failed: %v\n%s", err, string(outB))
	}
	reportB := readNestedReportFile(t, reportBPath)
	if got := reportB.Children[0].ClaimOutcome; got != storage.ClaimOutcomeAlreadyRunning {
		t.Fatalf("helper B claim outcome = %q, want already-running", got)
	}
	if _, err := os.Stat(duplicatePath); err == nil {
		t.Fatal("duplicate provider execution marker was created")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat duplicate marker: %v", err)
	}

	if err := os.WriteFile(releasePath, []byte("release"), 0o644); err != nil {
		t.Fatalf("write release marker: %v", err)
	}
	if err := cmdA.Wait(); err != nil {
		t.Fatalf("helper A failed: %v\n%s", err, outA.String())
	}
	reportA := readNestedReportFile(t, reportAPath)
	if got := reportA.Children[0].ClaimOutcome; got != storage.ClaimOutcomeClaimed {
		t.Fatalf("helper A claim outcome = %q, want claimed", got)
	}
	store, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	plan := durableReplayTestPlan()
	plan.PlanID = "plan-concurrent-replay"
	plan.Items[0].ChildKey = "race-child"
	plans, runs, edges, orphans := countNestedDurableRows(t, ctx, store, plan.RootRunID, plan.PlanID)
	if plans != 1 || runs != 2 || edges != 1 || orphans != 0 {
		t.Fatalf("two-process durable counts plans/runs/edges/orphans = %d/%d/%d/%d, want 1/2/1/0", plans, runs, edges, orphans)
	}
}

func TestScheduleNestedRunsRenewsClaimDuringLongExecution(t *testing.T) {
	oldLease := nestedClaimLeaseDuration
	oldRenew := nestedClaimRenewEvery
	nestedClaimLeaseDuration = 3 * time.Second
	nestedClaimRenewEvery = 200 * time.Millisecond
	defer func() {
		nestedClaimLeaseDuration = oldLease
		nestedClaimRenewEvery = oldRenew
	}()

	ctx := context.Background()
	repo := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	storeA, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open A: %v", err)
	}
	defer storeA.Close()
	storeB, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open B: %v", err)
	}
	defer storeB.Close()

	planA := durableReplayTestPlan()
	planA.PlanID = "plan-renew-long-execution"
	planA.Items[0].ChildKey = "renew-child"
	planB := durableReplayTestPlan()
	planB.PlanID = planA.PlanID
	planB.Items[0].ChildKey = "renew-child"

	executeStarted := make(chan struct{})
	releaseExecute := make(chan struct{})
	var releaseOnce sync.Once
	releaseOwner := func() {
		releaseOnce.Do(func() {
			close(releaseExecute)
		})
	}
	t.Cleanup(releaseOwner)
	var executeCount int64
	var ownerClockTicks int64
	doneA := make(chan struct {
		report NestedScheduleReport
		err    error
	}, 1)
	go func() {
		report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
			RepoPath:    repo,
			MaxChildren: 1,
			Now:         nestedTestNow(),
			Clock: func() time.Time {
				return nestedTestNow().Add(time.Duration(atomic.AddInt64(&ownerClockTicks, 1)) * time.Second)
			},
			Plan:  &planA,
			Store: storeA,
			RecordEvent: func(string, string, state.Event) error {
				return nil
			},
			Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
				atomic.AddInt64(&executeCount, 1)
				close(executeStarted)
				<-releaseExecute
				return ChildRunResult{Status: NestedStatusSucceeded}, nil
			},
		})
		doneA <- struct {
			report NestedScheduleReport
			err    error
		}{report: report, err: err}
	}()
	waitForNestedSignal(t, executeStarted, "provider execution")
	waitForDurableClaimRenewal(t, ctx, storeB)
	reportB, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow().Add(2 * time.Second),
		Clock:       func() time.Time { return nestedTestNow().Add(2 * time.Second) },
		Plan:        &planB,
		Store:       storeB,
		RecordEvent: func(string, string, state.Event) error {
			return nil
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			atomic.AddInt64(&executeCount, 1)
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("observer ScheduleNestedRuns returned error: %v", err)
	}
	if got := reportB.Children[0].ClaimOutcome; got != storage.ClaimOutcomeAlreadyRunning {
		t.Fatalf("observer claim outcome = %q, want already-running", got)
	}
	if got := atomic.LoadInt64(&executeCount); got != 1 {
		t.Fatalf("execute count while owner active = %d, want 1", got)
	}
	releaseOwner()
	outcomeA := <-doneA
	if outcomeA.err != nil {
		t.Fatalf("owner ScheduleNestedRuns returned error: %v", outcomeA.err)
	}
	if got := atomic.LoadInt64(&executeCount); got != 1 {
		t.Fatalf("execute count after owner completed = %d, want 1", got)
	}
}

func TestScheduleNestedRunsExpiredExecutingOwnerNeedsHumanWithoutDuplicateExecute(t *testing.T) {
	oldLease := nestedClaimLeaseDuration
	oldRenew := nestedClaimRenewEvery
	nestedClaimLeaseDuration = 80 * time.Millisecond
	nestedClaimRenewEvery = time.Hour
	defer func() {
		nestedClaimLeaseDuration = oldLease
		nestedClaimRenewEvery = oldRenew
	}()

	ctx := context.Background()
	repo := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
	storeA, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open A: %v", err)
	}
	defer storeA.Close()
	storeB, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open B: %v", err)
	}
	defer storeB.Close()

	planA := durableReplayTestPlan()
	planA.PlanID = "plan-expired-executing"
	planA.Items[0].ChildKey = "expired-child"
	planB := durableReplayTestPlan()
	planB.PlanID = planA.PlanID
	planB.Items[0].ChildKey = "expired-child"

	executeStarted := make(chan struct{})
	releaseExecute := make(chan struct{})
	var executeCount int64
	doneA := make(chan struct {
		report NestedScheduleReport
		err    error
	}, 1)
	go func() {
		report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
			RepoPath:    repo,
			MaxChildren: 1,
			Now:         nestedTestNow(),
			Clock:       func() time.Time { return nestedTestNow() },
			Plan:        &planA,
			Store:       storeA,
			RecordEvent: func(string, string, state.Event) error {
				return nil
			},
			Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
				atomic.AddInt64(&executeCount, 1)
				close(executeStarted)
				<-releaseExecute
				return ChildRunResult{Status: NestedStatusSucceeded}, nil
			},
		})
		doneA <- struct {
			report NestedScheduleReport
			err    error
		}{report: report, err: err}
	}()
	waitForNestedSignal(t, executeStarted, "provider execution")
	if err := storeB.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_claims SET lease_expires_at = ?`,
			nestedTestNow().Add(-time.Minute).UTC().Format(time.RFC3339Nano))
		return err
	}); err != nil {
		t.Fatalf("force expired executing claim: %v", err)
	}
	reportB, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow().Add(time.Hour),
		Clock:       func() time.Time { return nestedTestNow().Add(time.Hour) },
		Plan:        &planB,
		Store:       storeB,
		RecordEvent: func(string, string, state.Event) error {
			return nil
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			atomic.AddInt64(&executeCount, 1)
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("observer ScheduleNestedRuns returned error: %v", err)
	}
	if got := reportB.Children[0].Status; got != NestedStatusNeedsHuman {
		t.Fatalf("observer status = %q, want needs-human", got)
	}
	if got := atomic.LoadInt64(&executeCount); got != 1 {
		t.Fatalf("execute count while expired owner still active = %d, want 1", got)
	}
	close(releaseExecute)
	outcomeA := <-doneA
	if outcomeA.err == nil {
		t.Fatalf("owner ScheduleNestedRuns returned nil error after needs-human transition, want completion rejection")
	}
}

func TestScheduleNestedRunsMigratedLegacyExecutingClaimBlocksWithoutExecute(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	plan := durableReplayTestPlan()
	plan.PlanID = "plan-migrated-legacy-executing"
	plan.Items[0].ChildKey = "legacy-child"
	childRunID := state.RunIDForChild(plan.Items[0].ChildKey, 0, nestedTestNow())
	seedDurableReplayPlan(t, ctx, store, plan, childRunID, NestedStatusRunning)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO run_claims(run_id, executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at, phase, provider_idempotency_key, provider_receipt)
			VALUES (?, 'legacy-executor', 1, ?, ?, ?, ?, '', '')`,
			childRunID,
			state.FormatTimestamp(nestedTestNow()),
			state.FormatTimestamp(nestedTestNow().Add(-time.Minute)),
			state.FormatTimestamp(nestedTestNow()),
			storage.ClaimPhaseExecuting)
		return err
	}); err != nil {
		t.Fatalf("seed migrated legacy executing claim: %v", err)
	}

	var executeCount int64
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow().Add(time.Hour),
		Clock:       func() time.Time { return nestedTestNow().Add(time.Hour) },
		Plan:        &plan,
		Store:       store,
		RecordEvent: func(string, string, state.Event) error {
			return nil
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			atomic.AddInt64(&executeCount, 1)
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if got := atomic.LoadInt64(&executeCount); got != 0 {
		t.Fatalf("execute count = %d, want 0", got)
	}
	if got := report.Children[0].Status; got != NestedStatusNeedsHuman {
		t.Fatalf("child status = %q, want needs-human", got)
	}
	if got := report.Children[0].ClaimOutcome; got != storage.ClaimOutcomeBlocked {
		t.Fatalf("claim outcome = %q, want blocked", got)
	}
}

func TestScheduleNestedRunsPassesIdempotencyKeyAndPersistsOnlyRealReceipt(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	plan := durableReplayTestPlan()
	plan.PlanID = "plan-provider-key-receipt"
	var executedRunID, executedProviderKey string
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow(),
		Clock:       func() time.Time { return nestedTestNow() },
		Plan:        &plan,
		Store:       store,
		RecordEvent: func(string, string, state.Event) error {
			return nil
		},
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			executedRunID = child.RunID
			executedProviderKey = child.ProviderKey
			var receipt string
			if err := store.WithTx(ctx, func(tx storage.Tx) error {
				return tx.QueryRow(ctx, `SELECT provider_receipt FROM run_claims WHERE run_id = ?`, child.RunID).Scan(&receipt)
			}); err != nil {
				t.Fatalf("query provider receipt during execute: %v", err)
			}
			if receipt != "" {
				t.Fatalf("provider_receipt during execute = %q, want empty until real provider response", receipt)
			}
			return ChildRunResult{Status: NestedStatusSucceeded, ProviderReceipt: "receipt:" + child.RunID}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if executedProviderKey == "" || executedProviderKey != "child-run:"+executedRunID {
		t.Fatalf("executed provider key = %q for run %q, want stable child-run key", executedProviderKey, executedRunID)
	}
	if got := report.Children[0].ProviderKey; got != executedProviderKey {
		t.Fatalf("report provider key = %q, want %q", got, executedProviderKey)
	}
	var receipt string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_receipt FROM run_claims WHERE run_id = ?`, executedRunID).Scan(&receipt)
	}); err != nil {
		t.Fatalf("query provider receipt after completion: %v", err)
	}
	if receipt != "receipt:"+executedRunID {
		t.Fatalf("provider_receipt = %q, want real execution receipt", receipt)
	}
}

func TestScheduleNestedRunsRefusesNativeChildWithoutRegistration(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	executed := false
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    t.TempDir(),
		RootRunID:   "run-native-parent",
		ParentRunID: "run-native-parent",
		Store:       store,
		Now:         nestedTestNow(),
		Clock:       func() time.Time { return nestedTestNow() },
		Children: []ChildRunPlan{{
			ChildKey:   "native-child",
			Title:      "native child",
			Permission: "write",
			Scope:      ChildScope{Paths: []string{"src/native.go"}},
			Aggregation: ChildAggregation{
				Mode:     ChildAggregationCollect,
				Required: true,
			},
			Metadata: json.RawMessage(`{"native_subagent":true}`),
		}},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			executed = true
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	if executed {
		t.Fatalf("native child executor was called without registration")
	}
	if len(report.Children) != 1 || report.Children[0].Status != NestedStatusNeedsHuman {
		t.Fatalf("children = %#v, want needs-human refusal", report.Children)
	}
	if !strings.Contains(report.Children[0].Error, "missing expected_outputs") {
		t.Fatalf("native refusal error = %q, want missing expected_outputs", report.Children[0].Error)
	}
}

func TestScheduleNestedRunsNativeChildRequiresExplicitAuthorityMetadata(t *testing.T) {
	ctx := context.Background()
	now := nestedTestNow()
	projectID := "project-native-required"
	deliveryRunID := "drun-native-required"
	parentRunID := "run-native-required-parent"
	taskID := "task-native-required"
	attemptID := "attempt-native-required"
	childKey := "native-child"
	planID := "plan-native-required"
	planFingerprint := "sha256:plan-native-required"
	childAgentID := nestedTestAgentID(projectID, deliveryRunID, parentRunID, taskID, attemptID, childKey, planFingerprint)

	tests := []struct {
		name        string
		mutate      func(map[string]any)
		wantError   string
		wantClaim   bool
		wantExecute bool
	}{
		{name: "missing_scope", mutate: func(m map[string]any) { delete(m, "scope") }, wantError: "missing scope"},
		{name: "missing_parent_scope", mutate: func(m map[string]any) { delete(m, "parent_scope") }, wantError: "missing parent_scope"},
		{name: "missing_side_effect", mutate: func(m map[string]any) { delete(m, "side_effect_class") }, wantError: "missing side_effect_class"},
		{name: "missing_cancellation", mutate: func(m map[string]any) { delete(m, "cancellation_channel") }, wantError: "missing cancellation_channel"},
		{name: "missing_outputs", mutate: func(m map[string]any) { delete(m, "expected_outputs") }, wantError: "missing expected_outputs"},
		{name: "missing_budget", mutate: func(m map[string]any) { delete(m, "budget_bindings") }, wantError: "missing budget_bindings"},
		{name: "missing_write_locks", mutate: func(m map[string]any) { delete(m, "ownership_locks") }, wantError: "missing ownership_locks"},
		{name: "metadata_scope_widening", mutate: func(m map[string]any) {
			scope := nativeSchedulerScope(projectID, deliveryRunID, childAgentID, storage.PermissionWrite, storage.SideEffectRepoWrite, planFingerprint)
			scope.WriteScope = append(scope.WriteScope, "src/widened.go")
			scope.PathScope = append(scope.PathScope, "src/widened.go")
			m["scope"] = scope
		}, wantError: "ErrScopeWidening"},
		{name: "durable_scope_widening", mutate: func(m map[string]any) {
			scope := nativeSchedulerScope(projectID, deliveryRunID, childAgentID, storage.PermissionWrite, storage.SideEffectRepoWrite, planFingerprint)
			scope.WriteScope = append(scope.WriteScope, "src/widened.go")
			scope.PathScope = append(scope.PathScope, "src/widened.go")
			m["scope"] = scope
			m["parent_scope"] = scope
			m["ownership_locks"] = []map[string]any{
				{"resource_kind": "repo-path", "resource_key": "src/native.go", "state": storage.OwnershipStateHeld},
				{"resource_kind": "repo-path", "resource_key": "src/widened.go", "state": storage.OwnershipStateHeld},
			}
		}, wantError: "ErrScopeWidening"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return now }})
			if err != nil {
				t.Fatalf("storage.Open: %v", err)
			}
			defer store.Close()
			if err := seedNativeSchedulerAuthority(ctx, store, projectID, deliveryRunID, parentRunID, taskID, attemptID, childKey, childAgentID, planFingerprint, now); err != nil {
				t.Fatalf("seed native authority: %v", err)
			}
			metadata := validNativeSchedulerMetadata(projectID, deliveryRunID, taskID, attemptID, childKey, childAgentID, planFingerprint)
			tt.mutate(metadata)
			metadataBytes, err := json.Marshal(metadata)
			if err != nil {
				t.Fatalf("marshal metadata: %v", err)
			}
			executed := false
			report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
				RepoPath:    t.TempDir(),
				RootRunID:   parentRunID,
				ParentRunID: parentRunID,
				PlanID:      planID,
				Store:       store,
				Now:         now,
				Clock:       func() time.Time { return now },
				Children: []ChildRunPlan{{
					ChildKey:   childKey,
					Title:      "native child",
					Permission: storage.PermissionWrite,
					Scope:      ChildScope{Paths: []string{"src/native.go"}},
					Aggregation: ChildAggregation{
						Mode:     ChildAggregationCollect,
						Required: true,
					},
					Metadata: metadataBytes,
				}},
				Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
					executed = true
					return ChildRunResult{Status: NestedStatusSucceeded}, nil
				},
			})
			if err != nil {
				t.Fatalf("ScheduleNestedRuns: %v", err)
			}
			if executed != tt.wantExecute {
				t.Fatalf("executed = %t, want %t", executed, tt.wantExecute)
			}
			if got := report.Children[0].Status; got != NestedStatusNeedsHuman {
				t.Fatalf("status = %q, want needs-human; child=%#v", got, report.Children[0])
			}
			if !strings.Contains(report.Children[0].Error, tt.wantError) {
				t.Fatalf("error = %q, want %q", report.Children[0].Error, tt.wantError)
			}
			var claimCount int
			if err := store.WithTx(ctx, func(tx storage.Tx) error {
				return tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_claims`).Scan(&claimCount)
			}); err != nil {
				t.Fatalf("count claims: %v", err)
			}
			if gotClaim := claimCount > 0; gotClaim != tt.wantClaim {
				t.Fatalf("claim persisted = %t, want %t", gotClaim, tt.wantClaim)
			}
		})
	}
}

func TestScheduleNestedRunsNativeChildRegistersAndTerminalizesAtomically(t *testing.T) {
	ctx := context.Background()
	now := nestedTestNow()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	projectID := "project-native"
	deliveryRunID := "drun-native"
	parentRunID := "run-native-parent"
	taskID := "task-native"
	attemptID := "attempt-native"
	childKey := "native-child"
	planID := "plan-native"
	planFingerprint := "sha256:plan-native"
	childAgentID := nestedTestAgentID(projectID, deliveryRunID, parentRunID, taskID, attemptID, childKey, planFingerprint)
	if err := seedNativeSchedulerAuthority(ctx, store, projectID, deliveryRunID, parentRunID, taskID, attemptID, childKey, childAgentID, planFingerprint, now); err != nil {
		t.Fatalf("seed native authority: %v", err)
	}

	metadata := map[string]any{}
	for key, value := range validNativeSchedulerMetadata(projectID, deliveryRunID, taskID, attemptID, childKey, childAgentID, planFingerprint) {
		metadata[key] = value
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	executed := false
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    t.TempDir(),
		RootRunID:   parentRunID,
		ParentRunID: parentRunID,
		PlanID:      planID,
		Store:       store,
		Now:         now,
		Clock:       func() time.Time { return now },
		Children: []ChildRunPlan{{
			ChildKey:   childKey,
			Title:      "native child",
			Permission: storage.PermissionWrite,
			Scope:      ChildScope{Paths: []string{"src/native.go"}},
			Aggregation: ChildAggregation{
				Mode:     ChildAggregationCollect,
				Required: true,
			},
			Metadata: metadataBytes,
		}},
		Execute: func(ctx context.Context, child ChildRunPlan) (ChildRunResult, error) {
			executed = true
			var state string
			if err := store.WithTx(ctx, func(tx storage.Tx) error {
				return tx.QueryRow(ctx, `SELECT registration_state FROM agent_registrations WHERE id = ?`, childAgentID).Scan(&state)
			}); err != nil {
				t.Fatalf("query registration during execute: %v", err)
			}
			if state != storage.AgentStateRunning {
				t.Fatalf("registration state during execute = %q, want running", state)
			}
			return ChildRunResult{Status: NestedStatusSucceeded, ProviderReceipt: "receipt-native"}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	if !executed {
		t.Fatalf("native child executor was not called; children=%#v", report.Children)
	}
	if got := report.Children[0].Status; got != NestedStatusSucceeded {
		t.Fatalf("native child status = %q, want succeeded", got)
	}
	var registrationState, lockState, reservationState, receipt string
	var reservedValue, committedValue int64
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT registration_state, provider_receipt FROM agent_registrations WHERE id = ?`, childAgentID).Scan(&registrationState, &receipt); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state FROM agent_ownership_locks WHERE child_agent_id = ?`, childAgentID).Scan(&lockState); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT state, reserved_value, committed_value FROM budget_reservations WHERE budget_reservation_id = 'bres-native'`).Scan(&reservationState, &reservedValue, &committedValue)
	}); err != nil {
		t.Fatalf("query terminal native state: %v", err)
	}
	if registrationState != storage.AgentStateSucceeded || lockState != storage.OwnershipStateReleased || reservationState != "committed" || reservedValue != 0 || committedValue != 100 || receipt != "receipt-native" {
		t.Fatalf("terminal native state registration=%q lock=%q reservation=%q reserved=%d committed=%d receipt=%q", registrationState, lockState, reservationState, reservedValue, committedValue, receipt)
	}
}

func TestScheduleNestedRunsSuppressesFinishedEventsForStaleOwner(t *testing.T) {
	oldLease := nestedClaimLeaseDuration
	oldRenew := nestedClaimRenewEvery
	nestedClaimLeaseDuration = time.Minute
	nestedClaimRenewEvery = time.Hour
	defer func() {
		nestedClaimLeaseDuration = oldLease
		nestedClaimRenewEvery = oldRenew
	}()

	ctx := context.Background()
	repo := t.TempDir()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	plan := durableReplayTestPlan()
	plan.PlanID = "plan-stale-owner-events"
	plan.Items[0].ChildKey = "stale-child"
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow(),
		Clock:       func() time.Time { return nestedTestNow() },
		Plan:        &plan,
		Store:       store,
		Execute: func(_ context.Context, child ChildRunPlan) (ChildRunResult, error) {
			if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE run_claims
					SET phase = ?, lease_expires_at = ?
					WHERE run_id = ?`, storage.ClaimPhaseClaimed, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), child.RunID)
				return err
			}); err != nil {
				t.Fatalf("force stale claim state: %v", err)
			}
			if _, err := storage.ClaimChildRunExecution(ctx, store, plan.ParentRunID, child.RunID, "takeover-executor", time.Now().UTC(), time.Now().Add(time.Minute).UTC()); err != nil {
				t.Fatalf("take over stale claim: %v", err)
			}
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	if got := report.Children[0].Status; got != NestedStatusNeedsHuman {
		t.Fatalf("stale owner result status = %q, want needs-human", got)
	}
	childEvents := readNestedEvents(t, repo, report.Children[0].RunID)
	if strings.Contains(childEvents, NestedEventChildFinished) {
		t.Fatalf("stale owner child events include finished event:\n%s", childEvents)
	}
	parentEvents := readNestedEvents(t, repo, report.ParentRunID)
	if strings.Contains(parentEvents, NestedEventChildFinished) || strings.Contains(parentEvents, NestedEventParentDone) {
		t.Fatalf("stale owner parent events include terminal event:\n%s", parentEvents)
	}
}

func TestScheduleNestedRunsSuppressesParentDoneAfterChildCompletionPersistenceFailure(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	baseStore, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer baseStore.Close()
	store := failExecStore{
		Store: baseStore,
		match: func(query string) bool {
			return strings.Contains(query, "CASE WHEN provider_receipt = ''")
		},
		err: errors.New("injected child completion failure"),
	}

	plan := durableReplayTestPlan()
	plan.PlanID = "plan-complete-failure-suppresses-parent"
	var eventsMu sync.Mutex
	var events []state.Event
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow(),
		Clock:       func() time.Time { return nestedTestNow() },
		Plan:        &plan,
		Store:       store,
		RecordEvent: func(_ string, _ string, event state.Event) error {
			eventsMu.Lock()
			defer eventsMu.Unlock()
			events = append(events, event)
			return nil
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected child completion failure") {
		t.Fatalf("ScheduleNestedRuns error = %v, want injected child completion failure", err)
	}
	var parentStatus string
	if err := baseStore.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = ?`, report.ParentRunID).Scan(&parentStatus)
	}); err != nil {
		t.Fatalf("query parent status: %v", err)
	}
	if parentStatus == NestedStatusSucceeded {
		t.Fatalf("parent status = %q, want non-terminal after child completion failure", parentStatus)
	}
	for _, event := range events {
		if event.Event == NestedEventParentDone {
			t.Fatalf("recorded parent finished event after child completion failure: %#v", event)
		}
	}
}

func TestScheduleNestedRunsCancellationPersistsTerminalStatusWithCleanupContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	repo := t.TempDir()
	store, err := storage.Open(context.Background(), storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	plan := durableReplayTestPlan()
	plan.PlanID = "plan-cancel-cleanup"
	plan.Items[0].ChildKey = "cancel-child"
	executeStarted := make(chan struct{})
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    repo,
		MaxChildren: 1,
		Now:         nestedTestNow(),
		Clock:       func() time.Time { return nestedTestNow() },
		Plan:        &plan,
		Store:       store,
		Execute: func(ctx context.Context, child ChildRunPlan) (ChildRunResult, error) {
			close(executeStarted)
			cancel()
			<-ctx.Done()
			return ChildRunResult{}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns returned error: %v", err)
	}
	select {
	case <-executeStarted:
	default:
		t.Fatal("execute did not start")
	}
	if got := report.Children[0].Status; got != NestedStatusCancelled {
		t.Fatalf("cancelled child status = %q, want cancelled", got)
	}
	var durableStatus string
	if err := store.WithTx(context.Background(), func(tx storage.Tx) error {
		return tx.QueryRow(context.Background(), `SELECT status FROM runs WHERE id = ?`, report.Children[0].RunID).Scan(&durableStatus)
	}); err != nil {
		t.Fatalf("query durable status: %v", err)
	}
	if durableStatus != NestedStatusCancelled {
		t.Fatalf("durable status = %q, want cancelled", durableStatus)
	}
}

func TestNestedReplayHelperProcess(t *testing.T) {
	if os.Getenv("LOOPCODER_NESTED_REPLAY_HELPER") != "1" {
		return
	}
	ctx := context.Background()
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("LOOPCODER_NESTED_REPLAY_NOW"))
	if err != nil {
		t.Fatalf("parse helper now: %v", err)
	}
	store, err := storage.Open(ctx, storage.Options{Path: os.Getenv("LOOPCODER_NESTED_REPLAY_DB"), Now: func() time.Time { return nestedTestNow() }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()
	plan := durableReplayTestPlan()
	plan.PlanID = "plan-concurrent-replay"
	plan.Items[0].ChildKey = "race-child"
	plan.Items[0].ID = ""
	plan.Items[0].Title = "Race child"
	report, err := ScheduleNestedRuns(ctx, NestedScheduleOptions{
		RepoPath:    os.Getenv("LOOPCODER_NESTED_REPLAY_REPO"),
		MaxChildren: 1,
		Now:         now,
		Clock:       func() time.Time { return now },
		Plan:        &plan,
		Store:       store,
		RecordEvent: func(string, string, state.Event) error {
			return nil
		},
		Execute: func(context.Context, ChildRunPlan) (ChildRunResult, error) {
			activePath := os.Getenv("LOOPCODER_NESTED_REPLAY_ACTIVE")
			duplicatePath := os.Getenv("LOOPCODER_NESTED_REPLAY_DUPLICATE")
			file, err := os.OpenFile(activePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				_ = os.WriteFile(duplicatePath, []byte(err.Error()), 0o644)
				t.Fatalf("duplicate provider execution: %v", err)
			}
			_ = file.Close()
			waitForFile(t, os.Getenv("LOOPCODER_NESTED_REPLAY_RELEASE"))
			return ChildRunResult{Status: NestedStatusSucceeded}, nil
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if err := os.WriteFile(os.Getenv("LOOPCODER_NESTED_REPLAY_REPORT"), data, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
}

func receiveNestedTestValue[T any](t *testing.T, ch <-chan T, description string) T {
	t.Helper()
	timer := time.NewTimer(time.Until(nestedTestHangDeadline(t)))
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
	}
	var zero T
	return zero
}

func waitForNestedSignal(t *testing.T, ch <-chan struct{}, description string) {
	t.Helper()
	timer := time.NewTimer(time.Until(nestedTestHangDeadline(t)))
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
	}
}

func nestedTestHangDeadline(t *testing.T) time.Time {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		if time.Until(deadline) > 2*time.Second {
			return deadline.Add(-time.Second)
		}
		return deadline
	}
	return time.Now().Add(60 * time.Second)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := nestedTestHangDeadline(t)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("stat %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		<-ticker.C
	}
}

func waitForDurableClaimRenewal(t *testing.T, ctx context.Context, store storage.Store) {
	t.Helper()
	type claimSnapshot struct {
		heartbeatAt    time.Time
		leaseExpiresAt time.Time
		phase          string
	}
	readSnapshot := func() (claimSnapshot, bool) {
		var heartbeatAt, leaseExpiresAt, phase string
		err := store.WithTx(ctx, func(tx storage.Tx) error {
			return tx.QueryRow(ctx, `SELECT heartbeat_at, lease_expires_at, phase FROM run_claims LIMIT 1`).Scan(&heartbeatAt, &leaseExpiresAt, &phase)
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return claimSnapshot{}, false
			}
			t.Fatalf("read run claim: %v", err)
		}
		heartbeat, err := time.Parse(time.RFC3339Nano, heartbeatAt)
		if err != nil {
			t.Fatalf("parse heartbeat_at %q: %v", heartbeatAt, err)
		}
		lease, err := time.Parse(time.RFC3339Nano, leaseExpiresAt)
		if err != nil {
			t.Fatalf("parse lease_expires_at %q: %v", leaseExpiresAt, err)
		}
		return claimSnapshot{heartbeatAt: heartbeat, leaseExpiresAt: lease, phase: phase}, true
	}

	deadline := nestedTestHangDeadline(t)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var initial claimSnapshot
	for {
		snapshot, ok := readSnapshot()
		if ok {
			initial = snapshot
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for initial durable claim")
		}
		<-ticker.C
	}
	for {
		snapshot, ok := readSnapshot()
		if ok &&
			snapshot.phase == storage.ClaimPhaseExecuting &&
			snapshot.heartbeatAt.After(initial.heartbeatAt) &&
			snapshot.leaseExpiresAt.After(initial.leaseExpiresAt) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for durable claim renewal after heartbeat=%s lease=%s phase=%s",
				initial.heartbeatAt.Format(time.RFC3339Nano),
				initial.leaseExpiresAt.Format(time.RFC3339Nano),
				initial.phase)
		}
		<-ticker.C
	}
}

func readNestedReportFile(t *testing.T, path string) NestedScheduleReport {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report %s: %v", path, err)
	}
	var report NestedScheduleReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("unmarshal report %s: %v", path, err)
	}
	return report
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

func nestedTestAgentID(projectID, deliveryRunID, parentRunID, taskID, attemptID, childKey, planFingerprint string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{projectID, deliveryRunID, parentRunID, taskID, attemptID, childKey, planFingerprint}, "\x00")))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "agent_" + strings.ToLower(encoded)
}

func validNativeSchedulerMetadata(projectID, deliveryRunID, taskID, attemptID, childKey, childAgentID, planFingerprint string) map[string]any {
	return map[string]any{
		"native_subagent":           true,
		"project_id":                projectID,
		"delivery_run_id":           deliveryRunID,
		"task_id":                   taskID,
		"attempt_id":                attemptID,
		"adapter_id":                "claude",
		"provider_installation_id":  "pinst-native",
		"account_profile_id":        "acct-native",
		"model_capability_id":       "mcap-native",
		"routing_decision_id":       "route-native",
		"provider_session_ref":      "session-ref-native",
		"plan_fingerprint":          planFingerprint,
		"policy_fingerprint":        "sha256:policy-native",
		"authorization_fingerprint": "sha256:auth-native",
		"side_effect_class":         storage.SideEffectRepoWrite,
		"cancellation_channel":      "local-cancel",
		"expected_outputs": map[string]any{
			"schema": "loopcoder.native_child.output.v1",
		},
		"parent_scope": nativeSchedulerScope(projectID, deliveryRunID, childAgentID, storage.PermissionWrite, storage.SideEffectRepoWrite, planFingerprint),
		"scope":        nativeSchedulerScope(projectID, deliveryRunID, childAgentID, storage.PermissionWrite, storage.SideEffectRepoWrite, planFingerprint),
		"budget_bindings": []map[string]any{{
			"budget_policy_id":          "bpol-native",
			"budget_reservation_id":     "bres-native",
			"reserved_quantities_json":  "{}",
			"ancestor_budget_refs_json": "[]",
			"reservation_state":         "active",
		}},
		"ownership_locks": []map[string]any{{
			"resource_kind": "repo-path",
			"resource_key":  "src/native.go",
			"state":         storage.OwnershipStateHeld,
		}, {
			"resource_kind": "provider-receipt",
			"resource_key":  "run-20260709T000000Z-child-0-native-child",
			"state":         storage.OwnershipStateHeld,
		}},
	}
}

func nativeSchedulerScope(projectID, deliveryRunID, childAgentID, permission, sideEffectClass, planFingerprint string) storage.AgentScopeGrant {
	return storage.AgentScopeGrant{
		ProjectID:                projectID,
		DeliveryRunID:            deliveryRunID,
		ChildAgentID:             childAgentID,
		ReadScope:                []string{"src/native.go"},
		WriteScope:               []string{"src/native.go"},
		PathScope:                []string{"src/native.go"},
		RepositoryScope:          []string{"."},
		CommandScope:             []string{"go test ./internal/orchestration"},
		NetworkScope:             []string{"none"},
		CredentialScope:          []string{"none"},
		SideEffectScope:          []string{sideEffectClass},
		ApprovalScope:            []string{"none"},
		Permission:               permission,
		SideEffectClass:          sideEffectClass,
		PolicyVersion:            storage.AgentPolicyVersion,
		PolicyFingerprint:        "sha256:policy-native",
		PlanFingerprint:          planFingerprint,
		AuthorizationFingerprint: "sha256:auth-native",
	}
}

func seedNativeSchedulerAuthority(ctx context.Context, store storage.Store, projectID, deliveryRunID, rootRunID, taskID, attemptID, childKey, childAgentID, planFingerprint string, now time.Time) error {
	at := state.FormatTimestamp(now)
	scopeJSON, err := json.Marshal(nativeSchedulerScope(projectID, deliveryRunID, childAgentID, storage.PermissionWrite, storage.SideEffectRepoWrite, planFingerprint))
	if err != nil {
		return err
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
			projectID, "/tmp/"+projectID, at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_runs(
			delivery_run_id, run_id, schema_version, record_version, project_id, root_run_id, parent_run_id,
			state, intent_summary, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint,
			policy_version, max_side_effect_class, approval_status, override_status, created_at, updated_at,
			created_by_json, updated_by_json, host_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}')`,
			deliveryRunID, "delivery-"+childKey, "loopcoder.delivery_run.v1", 1, projectID, rootRunID, "",
			"approved", "native scheduler test", "sha256:input-native", "sha256:policy-native", planFingerprint, "sha256:auth-native",
			"0805.agent_federation.v1", storage.SideEffectRepoWrite, "approved", "none", at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_tasks(
			task_id, schema_version, record_version, project_id, delivery_run_id, task_key, state, title,
			requirements_json, scope_json, permission, side_effect_class, policy_version, plan_fingerprint,
			authorization_fingerprint, created_at, updated_at, created_by_json, updated_by_json, host_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}')`,
			taskID, "loopcoder.delivery_task.v1", 1, projectID, deliveryRunID, "task-native", "approved", "native task",
			string(scopeJSON), storage.PermissionWrite, storage.SideEffectRepoWrite, "0805.agent_federation.v1", planFingerprint, "sha256:auth-native", at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_requirements(
			task_requirement_id, schema_version, record_version, task_requirement_fingerprint, project_id, delivery_run_id,
			task_id, task_key, role_key, risk_tier, permission_required, side_effect_class, required_output, scope_json,
			data_classification, network_required, nested_allowed, cancellation_required, quality_floor,
			provenance_summary, policy_version, plan_fingerprint, created_at, updated_at, created_by_json,
			updated_by_json, host_json, classification, confidence, heuristic, payload_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}', ?, ?, ?, '{}')`,
			"treq-native", "loopcoder.task_requirement.v1", 1, "sha256:req-native", projectID, deliveryRunID,
			taskID, "task-native", "worker", "high", storage.PermissionWrite, storage.SideEffectRepoWrite, "json",
			string(scopeJSON), "internal", "none", 1, 1, "standard", "test", "0805.agent_federation.v1", planFingerprint,
			at, at, "local-diagnostic", "high", 0); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO provider_installations(
			provider_installation_id, schema_version, record_version, scope, project_id, adapter_id, adapter_declaration_id,
			provider_display_name, executable_name, executable_identity_json, canonical_path_redacted, discovery_source,
			discovery_order, platform, version_confidence, installation_state, usable_for_invocation, created_at,
			updated_at, captured_at, freshness_state, confidence, side_effect_class, classification, payload_json)
			VALUES ('pinst-native', 'loopcoder.provider_installation.v1', 1, 'project', ?, 'claude', 'adecl-native',
			'Claude', 'claude', '{}', 'claude', 'test', 1, 'test', 'high', 'active', 'yes', ?, ?, ?, 'fresh', 'high', ?, 'local-diagnostic', '{}')`,
			projectID, at, at, at, storage.SideEffectLocalRead); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO account_profiles(account_profile_id, adapter_id, provider_installation_id, payload_json) VALUES ('acct-native', 'claude', 'pinst-native', '{}')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO model_catalog_snapshots(model_catalog_snapshot_id, adapter_id, provider_installation_id, payload_json) VALUES ('snap-native', 'claude', 'pinst-native', '{}')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO model_capabilities(model_capability_id, model_catalog_snapshot_id, adapter_id, payload_json) VALUES ('mcap-native', 'snap-native', 'claude', '{}')`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO budget_policies(
			budget_policy_id, project_id, delivery_run_id, task_id, sub_agent_id, adapter_id, account_profile_id,
			model_capability_id, scope_kind, scope_key, quantity_kind, unit, window_kind, policy_mode,
			ceiling_value, active, policy_version, payload_json)
			VALUES ('bpol-native', ?, ?, ?, ?, 'claude', 'acct-native', 'mcap-native', 'sub-agent', ?, 'local-policy', 'unit', 'run', 'hard', 1000, 1, '0805.agent_federation.v1', '{}')`,
			projectID, deliveryRunID, taskID, childAgentID, "sub-agent:"+childAgentID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO budget_aggregates(budget_policy_id, reserved_value, committed_value, updated_at) VALUES ('bpol-native', 100, 0, ?)`, at); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO budget_reservations(
			budget_reservation_id, idempotency_key, request_fingerprint, requester_id, authorization_fingerprint,
			project_id, delivery_run_id, task_id, sub_agent_id, adapter_id, account_profile_id, model_capability_id,
			quantity_kind, unit, requested_value, reserved_value, state, generation, lease_expires_at, scope_key,
			policy_ids_json, payload_json, created_at, updated_at)
			VALUES ('bres-native', 'idem-native', 'sha256:budget-native', 'test', 'sha256:auth-native',
			?, ?, ?, ?, 'claude', 'acct-native', 'mcap-native', 'local-policy', 'unit', 100, 100, 'active', 1, ?, ?, '["bpol-native"]',
			'{"reserved_value":100,"committed_value":0,"released_value":0,"state":"active","generation":1}', ?, ?)`,
			projectID, deliveryRunID, taskID, childAgentID, state.FormatTimestamp(now.Add(time.Hour)), "sub-agent:"+childAgentID, at, at)
		return err
	})
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

type failExecStore struct {
	storage.Store
	match func(string) bool
	err   error
}

func (s failExecStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	return s.Store.WithWriteTx(ctx, func(tx storage.Tx) error {
		return fn(failExecTx{Tx: tx, match: s.match, err: s.err})
	})
}

type failExecTx struct {
	storage.Tx
	match func(string) bool
	err   error
}

func (tx failExecTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx.match != nil && tx.match(query) {
		return nil, tx.err
	}
	return tx.Tx.Exec(ctx, query, args...)
}
