package orchestration

import (
	"context"
	"errors"
	"fmt"
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

func TestDispatchWaveExplicitIssueNumbers(t *testing.T) {
	var calls []worker.Options
	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			101: {Number: 101, Title: "First", Body: "First body"},
			102: {Number: 102, Title: "Second", Body: "Second body"},
		}},
		RepoPath:      t.TempDir(),
		BaseBranch:    "trunk",
		RunID:         "run-test-wave",
		IssueNumbers:  []int{102, 101, 102},
		ThrottleLimit: 1,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(102, 101), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			calls = append(calls, opts)
			return waveWorkerResult(opts), nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}

	if got, want := report.IssuesRequested, []int{102, 101}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IssuesRequested = %#v, want %#v", got, want)
	}
	if len(calls) != 2 {
		t.Fatalf("dispatch calls = %d, want 2", len(calls))
	}
	if calls[0].IssueNumber != 102 || calls[0].IssueTitle != "Second" || calls[0].IssueBody != "Second body" {
		t.Fatalf("first dispatch opts = %#v", calls[0])
	}
	if calls[0].BaseBranch != "trunk" || calls[0].RunID != "run-test-wave" {
		t.Fatalf("first dispatch base/run = %#v", calls[0])
	}
	if calls[0].Provider != "" || calls[0].Model != "" || calls[0].Effort != "" {
		t.Fatalf("provider/model/effort should be omitted unless supplied: %#v", calls[0])
	}
}

func TestDispatchWaveFromReadySetSelection(t *testing.T) {
	var dispatched []int
	snapshot := readySetReport(3, 5)
	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			3: {Number: 3, Title: "Three"},
			5: {Number: 5, Title: "Five"},
			8: {Number: 8, Title: "Eight"},
		}},
		RepoPath:      t.TempDir(),
		RunID:         "run-test-wave",
		ReadySet:      &snapshot,
		ThrottleLimit: 1,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(3, 5, 8), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			dispatched = append(dispatched, opts.IssueNumber)
			return waveWorkerResult(opts), nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}

	if got, want := report.IssuesRequested, []int{3, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("IssuesRequested = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(dispatched, []int{3, 5}) {
		t.Fatalf("dispatched issues = %#v, want [3 5]", dispatched)
	}
}

func TestDispatchWavePreflightSkip(t *testing.T) {
	snapshot := readySetReport(7)
	calledDispatch := false
	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader:   fakeReader{views: map[int]gh.Issue{7: {Number: 7, Title: "Seven"}}},
		RepoPath: t.TempDir(),
		RunID:    "run-test-wave",
		ReadySet: &snapshot,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return report.ReadySetReport{
				Repo:       "owner/repo",
				BaseBranch: "main",
				Blocked: []report.BlockedIssue{{
					Issue:          7,
					Title:          "Seven",
					Classification: "has-open-PR",
					Reason:         "open PR #70 exists for loop/issue-7",
					OpenPRs: []report.OpenPRSummary{{
						Number: 70,
						URL:    "https://github.com/owner/repo/pull/70",
						Head:   "loop/issue-7",
					}},
				}},
			}, nil
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			calledDispatch = true
			return worker.Result{}, nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}
	if calledDispatch {
		t.Fatal("dispatch was called for a preflight-skipped issue")
	}
	if len(report.Results) != 1 || report.Results[0].Status != DispatchWaveStatusSkipped {
		t.Fatalf("results = %#v, want one skipped result", report.Results)
	}
	if report.Results[0].PR != "https://github.com/owner/repo/pull/70" ||
		!strings.Contains(report.Results[0].Error, "open PR #70") {
		t.Fatalf("skip result missing PR/reason: %#v", report.Results[0])
	}
}

func TestDispatchWavePartialFailure(t *testing.T) {
	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		}},
		RepoPath:      t.TempDir(),
		RunID:         "run-test-wave",
		IssueNumbers:  []int{1, 2},
		ThrottleLimit: 2,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(1, 2), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			if opts.IssueNumber == 2 {
				return worker.Result{}, errors.New("worker failed")
			}
			return waveWorkerResult(opts), nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("result count = %d, want 2", len(report.Results))
	}
	if report.Results[0].Status != DispatchWaveStatusSucceeded {
		t.Fatalf("issue #1 result = %#v, want succeeded", report.Results[0])
	}
	if report.Results[1].Status != DispatchWaveStatusFailed || report.Results[1].Error != "worker failed" {
		t.Fatalf("issue #2 result = %#v, want failed worker error", report.Results[1])
	}
}

func TestDispatchWaveSharedRunIDPropagation(t *testing.T) {
	var mu sync.Mutex
	var runIDs []string
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			4: {Number: 4, Title: "Four"},
			6: {Number: 6, Title: "Six"},
		}},
		RepoPath:      t.TempDir(),
		IssueNumbers:  []int{4, 6},
		ThrottleLimit: 2,
		Now:           now,
		Provider:      "codex",
		Model:         "gpt-5",
		Effort:        "high",
		ComputeReadySet: func(_ context.Context, opts Options) (report.ReadySetReport, error) {
			if opts.RunID != "run-20260626T120000Z-wave" {
				t.Fatalf("preflight RunID = %q, want generated wave run id", opts.RunID)
			}
			return readySetReport(4, 6), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			mu.Lock()
			runIDs = append(runIDs, opts.RunID)
			mu.Unlock()
			if opts.Provider != "codex" || opts.Model != "gpt-5" || opts.Effort != "high" {
				t.Fatalf("provider/model/effort pass-through = %#v", opts)
			}
			return waveWorkerResult(opts), nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}
	if report.RunID != "run-20260626T120000Z-wave" {
		t.Fatalf("report RunID = %q, want generated wave run id", report.RunID)
	}
	for _, runID := range runIDs {
		if runID != report.RunID {
			t.Fatalf("dispatch run id = %q, want %q", runID, report.RunID)
		}
	}
	if len(runIDs) != 2 {
		t.Fatalf("dispatch run id count = %d, want 2", len(runIDs))
	}
}

func TestDispatchWaveBudgetNeedsHumanWithoutDispatchingOverCap(t *testing.T) {
	repo := t.TempDir()
	maxAttempts := 1
	var dispatched []int

	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		}},
		RepoPath:      repo,
		RunID:         "run-test-wave",
		IssueNumbers:  []int{1, 2},
		ThrottleLimit: 1,
		Budget:        config.GuardrailBudget{MaxTotalAttempts: &maxAttempts},
		Now:           time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(1, 2), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			dispatched = append(dispatched, opts.IssueNumber)
			return waveWorkerResult(opts), nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}

	if !reflect.DeepEqual(dispatched, []int{1}) {
		t.Fatalf("dispatched issues = %#v, want [1]", dispatched)
	}
	if report.Results[0].Status != DispatchWaveStatusSucceeded {
		t.Fatalf("issue #1 result = %#v, want succeeded", report.Results[0])
	}
	if report.Results[1].Status != DispatchWaveStatusNeedsHuman {
		t.Fatalf("issue #2 result = %#v, want needs-human", report.Results[1])
	}
	for _, want := range []string{"guardrails.budget.max_total_attempts", "planned_increment=1", "proposed_increment=1"} {
		if !strings.Contains(report.Results[1].Error, want) {
			t.Fatalf("needs-human error missing %q:\n%s", want, report.Results[1].Error)
		}
	}
	if !DispatchWaveHasFailures(report) {
		t.Fatal("DispatchWaveHasFailures = false, want true for needs-human")
	}

	ledgerPath := guardrails.LedgerPath(repo, "run-test-wave", 2)
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("ReadFile ledger: %v", err)
	}
	if !strings.Contains(string(data), `"status": "needs-human"`) {
		t.Fatalf("ledger missing needs-human status:\n%s", string(data))
	}
}

func TestDispatchWaveCircuitBreakerFreezesOnlyNoProgressIssue(t *testing.T) {
	repo := t.TempDir()
	maxWaves := 1
	var dispatched []int

	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		}},
		RepoPath:      repo,
		RunID:         "run-test-wave",
		IssueNumbers:  []int{1, 2},
		ThrottleLimit: 1,
		CircuitBreaker: config.GuardrailCircuitBreaker{
			MaxNoProgressWaves: &maxWaves,
		},
		Now: time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(1, 2), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			dispatched = append(dispatched, opts.IssueNumber)
			if opts.IssueNumber == 2 {
				return worker.Result{}, errors.New("worker repeated same failure")
			}
			return waveWorkerResult(opts), nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}

	if !reflect.DeepEqual(dispatched, []int{1, 2}) {
		t.Fatalf("dispatched issues = %#v, want [1 2]", dispatched)
	}
	if report.Results[0].Status != DispatchWaveStatusSucceeded {
		t.Fatalf("issue #1 result = %#v, want succeeded", report.Results[0])
	}
	if report.Results[1].Status != DispatchWaveStatusNeedsHuman {
		t.Fatalf("issue #2 result = %#v, want needs-human", report.Results[1])
	}
	for _, want := range []string{
		"guardrails.circuit_breaker.max_no_progress_waves",
		"no_progress_waves=1",
		"last_material_progress=unknown",
		"human_decision=clarify the issue",
	} {
		if !strings.Contains(report.Results[1].Error, want) {
			t.Fatalf("needs-human error missing %q:\n%s", want, report.Results[1].Error)
		}
	}

	issueOneLedger, err := os.ReadFile(guardrails.LedgerPath(repo, "run-test-wave", 1))
	if err != nil {
		t.Fatalf("ReadFile issue #1 ledger: %v", err)
	}
	if !strings.Contains(string(issueOneLedger), `"status": "allowed"`) {
		t.Fatalf("issue #1 ledger should remain allowed:\n%s", string(issueOneLedger))
	}
	issueTwoLedger, err := os.ReadFile(guardrails.LedgerPath(repo, "run-test-wave", 2))
	if err != nil {
		t.Fatalf("ReadFile issue #2 ledger: %v", err)
	}
	if !strings.Contains(string(issueTwoLedger), `"status": "needs-human"`) ||
		!strings.Contains(string(issueTwoLedger), `"reason": "guardrails.circuit_breaker.max_no_progress_waves"`) {
		t.Fatalf("issue #2 ledger should be frozen:\n%s", string(issueTwoLedger))
	}
}

func readySetReport(numbers ...int) report.ReadySetReport {
	ready := make([]report.ReadyIssue, 0, len(numbers))
	for _, number := range numbers {
		ready = append(ready, report.ReadyIssue{
			Issue:  number,
			Title:  fmt.Sprintf("Issue %d", number),
			Reason: "ready",
		})
	}
	return report.ReadySetReport{
		Repo:       "owner/repo",
		BaseBranch: "main",
		Ready:      ready,
	}
}

func waveWorkerResult(opts worker.Options) worker.Result {
	return worker.Result{
		OK:          true,
		Issue:       opts.IssueNumber,
		Branch:      fmt.Sprintf("loop/issue-%d", opts.IssueNumber),
		RunID:       opts.RunID,
		PR:          fmt.Sprintf("https://github.com/owner/repo/pull/%d", opts.IssueNumber),
		AttemptPath: fmt.Sprintf(".loopcoder/runs/%s/workers/job-%d.attempt.json", opts.RunID, opts.IssueNumber),
		Status:      "succeeded",
	}
}

func noAttempts(string, string) ([]state.Attempt, error) {
	return nil, nil
}
