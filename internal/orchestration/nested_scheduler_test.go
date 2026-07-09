package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestNestedScheduleFanOutFanInAggregatesInSpecOrder(t *testing.T) {
	repo := t.TempDir()
	started := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})

	done := make(chan struct {
		report NestedScheduleReport
		err    error
	}, 1)
	go func() {
		report, err := NestedSchedule(context.Background(), NestedScheduleOptions{
			Reader:           fakeReader{},
			RepoPath:         repo,
			BaseBranch:       "trunk",
			ParentRunID:      "run-20260709T120000Z-wave",
			ConcurrencyLimit: 2,
			Now:              started,
			Children: []ChildRunSpec{
				{ID: "slow", IssueNumbers: []int{20}, Required: true},
				{ID: "fast", IssueNumbers: []int{10}, Required: true},
			},
			DispatchWave: func(_ context.Context, opts DispatchWaveOptions) (DispatchWaveReport, error) {
				if strings.Contains(opts.RunID, "slow") {
					close(slowStarted)
					<-releaseSlow
				} else {
					select {
					case <-slowStarted:
					case <-time.After(time.Second):
						t.Fatal("fast child ran before slow child was scheduled concurrently")
					}
				}
				return DispatchWaveReport{
					Repo:            "owner/repo",
					RepoPath:        repo,
					BaseBranch:      opts.BaseBranch,
					RunID:           opts.RunID,
					IssuesRequested: append([]int(nil), opts.IssueNumbers...),
					Results: []DispatchWaveIssueResult{{
						Issue:  opts.IssueNumbers[0],
						Status: DispatchWaveStatusSucceeded,
					}},
				}, nil
			},
			AppendEvent: state.AppendEvent,
		})
		done <- struct {
			report NestedScheduleReport
			err    error
		}{report: report, err: err}
	}()
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for slow child")
	}
	close(releaseSlow)
	var report NestedScheduleReport
	var err error
	select {
	case got := <-done:
		report, err = got.report, got.err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for nested schedule")
	}
	if err != nil {
		t.Fatalf("NestedSchedule returned error: %v", err)
	}
	if report.Status != NestedScheduleStatusSucceeded || report.StopReason != NestedScheduleStopCompleted {
		t.Fatalf("status = %s stop = %s", report.Status, report.StopReason)
	}
	if got, want := []string{report.Children[0].ID, report.Children[1].ID}, []string{"slow", "fast"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("child order = %#v, want %#v", got, want)
	}
	if report.Children[0].RunID != "run-20260709T120000Z-wave-child-slow" ||
		report.Children[1].RunID != "run-20260709T120000Z-wave-child-fast" {
		t.Fatalf("child run ids = %#v", report.Children)
	}
	if report.Summary.ChildSucceededCount != 2 || report.Summary.RequiredChildCount != 2 {
		t.Fatalf("summary = %#v", report.Summary)
	}
}

func TestNestedScheduleEnforcesParentConcurrencyLimit(t *testing.T) {
	var mu sync.Mutex
	active := 0
	maxActive := 0

	report, err := NestedSchedule(context.Background(), NestedScheduleOptions{
		Reader:           fakeReader{},
		RepoPath:         t.TempDir(),
		ParentRunID:      "run-20260709T120000Z-wave",
		ConcurrencyLimit: 2,
		Children: []ChildRunSpec{
			{ID: "one", IssueNumbers: []int{1}, Required: true},
			{ID: "two", IssueNumbers: []int{2}, Required: true},
			{ID: "three", IssueNumbers: []int{3}, Required: true},
		},
		DispatchWave: func(_ context.Context, opts DispatchWaveOptions) (DispatchWaveReport, error) {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return nestedTestWave(opts, DispatchWaveStatusSucceeded), nil
		},
	})
	if err != nil {
		t.Fatalf("NestedSchedule returned error: %v", err)
	}
	if report.Status != NestedScheduleStatusSucceeded {
		t.Fatalf("status = %s", report.Status)
	}
	if maxActive > 2 {
		t.Fatalf("max active children = %d, want <= 2", maxActive)
	}
}

func TestNestedScheduleRequiredAndOptionalStatusAggregation(t *testing.T) {
	tests := []struct {
		name       string
		children   []ChildRunSpec
		statusByID map[string]string
		wantStatus string
		wantStop   string
	}{
		{
			name: "required failure fails parent",
			children: []ChildRunSpec{
				{ID: "required", IssueNumbers: []int{1}, Required: true},
				{ID: "optional", IssueNumbers: []int{2}, Optional: true},
			},
			statusByID: map[string]string{"required": DispatchWaveStatusFailed, "optional": DispatchWaveStatusSucceeded},
			wantStatus: NestedScheduleStatusFailed,
			wantStop:   NestedScheduleStopChildFailed,
		},
		{
			name: "required needs-human needs-human parent",
			children: []ChildRunSpec{
				{ID: "required", IssueNumbers: []int{1}, Required: true},
				{ID: "optional", IssueNumbers: []int{2}, Optional: true},
			},
			statusByID: map[string]string{"required": DispatchWaveStatusNeedsHuman, "optional": DispatchWaveStatusSucceeded},
			wantStatus: NestedScheduleStatusNeedsHuman,
			wantStop:   NestedScheduleStopChildNeedsHuman,
		},
		{
			name: "optional failure is reported but does not fail parent",
			children: []ChildRunSpec{
				{ID: "required", IssueNumbers: []int{1}, Required: true},
				{ID: "optional", IssueNumbers: []int{2}, Optional: true},
			},
			statusByID: map[string]string{"required": DispatchWaveStatusSucceeded, "optional": DispatchWaveStatusFailed},
			wantStatus: NestedScheduleStatusSucceeded,
			wantStop:   NestedScheduleStopCompleted,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := NestedSchedule(context.Background(), NestedScheduleOptions{
				Reader:           fakeReader{},
				RepoPath:         t.TempDir(),
				ParentRunID:      "run-20260709T120000Z-wave",
				ConcurrencyLimit: 1,
				Children:         tt.children,
				DispatchWave: func(_ context.Context, opts DispatchWaveOptions) (DispatchWaveReport, error) {
					status := DispatchWaveStatusSucceeded
					for id, candidate := range tt.statusByID {
						if strings.Contains(opts.RunID, id) {
							status = candidate
						}
					}
					return nestedTestWave(opts, status), nil
				},
			})
			if err != nil {
				t.Fatalf("NestedSchedule returned error: %v", err)
			}
			if report.Status != tt.wantStatus || report.StopReason != tt.wantStop {
				t.Fatalf("status = %s stop = %s, want %s/%s", report.Status, report.StopReason, tt.wantStatus, tt.wantStop)
			}
		})
	}
}

func TestNestedScheduleRequiresExplicitRequiredOrOptional(t *testing.T) {
	_, err := NestedSchedule(context.Background(), NestedScheduleOptions{
		RepoPath:    t.TempDir(),
		ParentRunID: "run-20260709T120000Z-wave",
		Children: []ChildRunSpec{{
			ID:           "implicit",
			IssueNumbers: []int{1},
		}},
		DispatchWave: func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
			t.Fatal("dispatch wave should not run for invalid child")
			return DispatchWaveReport{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one of Required or Optional") {
		t.Fatalf("error = %v, want explicit optionality error", err)
	}
}

func TestNestedScheduleRecordsParentChildTransitions(t *testing.T) {
	repo := t.TempDir()
	report, err := NestedSchedule(context.Background(), NestedScheduleOptions{
		Reader:      fakeReader{},
		RepoPath:    repo,
		ParentRunID: "run-20260709T120000Z-wave",
		Now:         time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
		Children: []ChildRunSpec{{
			ID:           "alpha",
			IssueNumbers: []int{3},
			Required:     true,
		}},
		DispatchWave: func(_ context.Context, opts DispatchWaveOptions) (DispatchWaveReport, error) {
			return nestedTestWave(opts, DispatchWaveStatusSucceeded), nil
		},
	})
	if err != nil {
		t.Fatalf("NestedSchedule returned error: %v", err)
	}
	if report.Status != NestedScheduleStatusSucceeded {
		t.Fatalf("status = %s", report.Status)
	}

	data, err := os.ReadFile(state.EventsPath(repo, "run-20260709T120000Z-wave"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("event lines = %d, want scheduled and completed: %s", len(lines), string(data))
	}
	for index, want := range []string{nestedChildScheduledEvent, nestedChildCompletedEvent} {
		var got struct {
			Event   string `json:"event"`
			RunID   string `json:"run_id"`
			Details struct {
				ParentRunID string `json:"parent_run_id"`
				ChildID     string `json:"child_id"`
				ChildRunID  string `json:"child_run_id"`
				Required    bool   `json:"required"`
			} `json:"details"`
		}
		if err := json.Unmarshal([]byte(lines[index]), &got); err != nil {
			t.Fatalf("Unmarshal event %d: %v", index, err)
		}
		if got.Event != want || got.RunID != "run-20260709T120000Z-wave" ||
			got.Details.ParentRunID != "run-20260709T120000Z-wave" ||
			got.Details.ChildID != "alpha" ||
			got.Details.ChildRunID != "run-20260709T120000Z-wave-child-alpha" ||
			!got.Details.Required {
			t.Fatalf("event %d = %#v", index, got)
		}
	}
}

func TestNestedSchedulePassesGuardrailsToChildDispatchWave(t *testing.T) {
	repo := t.TempDir()
	maxRuns := 1
	report, err := NestedSchedule(context.Background(), NestedScheduleOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			1: {Number: 1, Title: "One"},
		}},
		RepoPath:         repo,
		ParentRunID:      "run-20260709T120000Z-wave",
		ConcurrencyLimit: 1,
		ThrottleLimit:    1,
		Budget:           config.GuardrailBudget{MaxRuns: &maxRuns},
		Children: []ChildRunSpec{
			{ID: "first", IssueNumbers: []int{1}, Required: true},
			{ID: "second", IssueNumbers: []int{1}, Required: true},
		},
		ComputeReadySet: func(_ context.Context, opts Options) (report.ReadySetReport, error) {
			return readySetReport(1), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			return waveWorkerResult(opts), nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("NestedSchedule returned error: %v", err)
	}
	if report.Status != NestedScheduleStatusNeedsHuman || report.StopReason != NestedScheduleStopChildNeedsHuman {
		t.Fatalf("status = %s stop = %s", report.Status, report.StopReason)
	}
	if report.Children[0].Status != NestedScheduleStatusSucceeded {
		t.Fatalf("first child = %#v", report.Children[0])
	}
	if report.Children[1].Status != NestedScheduleStatusNeedsHuman ||
		!strings.Contains(report.Children[1].Error, "guardrails.budget.max_runs") {
		t.Fatalf("second child = %#v, want budget needs-human", report.Children[1])
	}
	if _, err := os.Stat(guardrails.LedgerPath(repo, report.Children[1].RunID, 1)); err != nil {
		t.Fatalf("missing child guardrail ledger: %v", err)
	}
}

func TestNestedSchedulePassesCircuitBreakerToChildDispatchWave(t *testing.T) {
	repo := t.TempDir()
	maxWaves := 1
	report, err := NestedSchedule(context.Background(), NestedScheduleOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			4: {Number: 4, Title: "Four"},
		}},
		RepoPath:         repo,
		ParentRunID:      "run-20260709T120000Z-wave",
		ConcurrencyLimit: 1,
		CircuitBreaker:   config.GuardrailCircuitBreaker{MaxNoProgressWaves: &maxWaves},
		Children: []ChildRunSpec{{
			ID:           "no-progress",
			IssueNumbers: []int{4},
			Required:     true,
		}},
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(4), nil
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			return worker.Result{}, errors.New("same failure")
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("NestedSchedule returned error: %v", err)
	}
	if report.Status != NestedScheduleStatusNeedsHuman {
		t.Fatalf("status = %s, want needs-human", report.Status)
	}
	if len(report.Children) != 1 ||
		report.Children[0].Status != NestedScheduleStatusNeedsHuman ||
		!strings.Contains(report.Children[0].Error, "guardrails.circuit_breaker.max_no_progress_waves") {
		t.Fatalf("child = %#v, want circuit breaker needs-human", report.Children)
	}
}

func nestedTestWave(opts DispatchWaveOptions, status string) DispatchWaveReport {
	issue := 0
	if len(opts.IssueNumbers) > 0 {
		issue = opts.IssueNumbers[0]
	}
	result := DispatchWaveIssueResult{Issue: issue, Status: status}
	if status == DispatchWaveStatusNeedsHuman {
		result.Error = "needs human"
	}
	return DispatchWaveReport{
		Repo:            "owner/repo",
		RepoPath:        opts.RepoPath,
		BaseBranch:      opts.BaseBranch,
		RunID:           opts.RunID,
		IssuesRequested: append([]int(nil), opts.IssueNumbers...),
		Results:         []DispatchWaveIssueResult{result},
	}
}
