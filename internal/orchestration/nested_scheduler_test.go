package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
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
	if !reflect.DeepEqual(gotIDs, []string{"a", "b", "c"}) {
		t.Fatalf("child result order = %#v, want sorted ids", gotIDs)
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
	if err == nil || !strings.Contains(err.Error(), "exactly one of required or optional") {
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
