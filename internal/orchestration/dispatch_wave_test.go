package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/jasonhnd/loopcoder/internal/reporter"
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
				result := waveWorkerResult(opts)
				result.Report = waveReport(opts.IssueNumber, 202)
				return result, errors.New("worker failed")
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
	if report.Results[1].Report == nil || report.Results[1].Report.Model != "worker-model-2" {
		t.Fatalf("issue #2 report = %#v, want preserved worker report", report.Results[1].Report)
	}
}

func TestDispatchWaveMarksQueuedChildAbandonedWhenParentCancels(t *testing.T) {
	repo := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	report, err := DispatchWave(ctx, DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			1: {Number: 1, Title: "One"},
			2: {Number: 2, Title: "Two"},
		}},
		RepoPath:      repo,
		RunID:         "run-test-wave",
		IssueNumbers:  []int{1, 2},
		ThrottleLimit: 1,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(1, 2), nil
		},
		Dispatch: func(ctx context.Context, opts worker.Options) (worker.Result, error) {
			cancel()
			time.Sleep(25 * time.Millisecond)
			return worker.Result{Issue: opts.IssueNumber, RunID: opts.RunID, Status: state.StatusCancelled}, ctx.Err()
		},
		LoadAttempts: state.LoadAttempts,
		Now:          time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("result count = %d, want 2", len(report.Results))
	}
	if report.Results[0].Status != DispatchWaveStatusCancelled {
		t.Fatalf("first result = %#v, want cancelled", report.Results[0])
	}
	if report.Results[1].Status != DispatchWaveStatusAbandoned {
		t.Fatalf("second result = %#v, want abandoned", report.Results[1])
	}
	if !strings.Contains(report.Results[1].Error, "child abandoned") ||
		!strings.Contains(report.Results[1].RecoveryContextPath, "job-2-abandoned-context.md") {
		t.Fatalf("abandoned result missing recovery hint: %#v", report.Results[1])
	}

	attempts, err := state.LoadAttempts(repo, "run-test-wave")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Issue != 2 || attempts[0].Status != DispatchWaveStatusAbandoned {
		t.Fatalf("abandoned synthetic attempt = %#v", attempts)
	}
	brief, err := os.ReadFile(state.RecoveryBriefPath(repo, "run-test-wave", "job-2-abandoned"))
	if err != nil {
		t.Fatalf("read recovery brief: %v", err)
	}
	if !strings.Contains(string(brief), "- Status: abandoned") || !strings.Contains(string(brief), "child abandoned") {
		t.Fatalf("abandoned recovery brief missing status/error:\n%s", string(brief))
	}
	if !DispatchWaveHasFailures(report) {
		t.Fatal("DispatchWaveHasFailures = false, want true for abandoned child")
	}
}

func TestDispatchWaveRecordsParentTimeoutBeforeChildDispatch(t *testing.T) {
	repo := t.TempDir()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	dispatchCalled := false

	report, err := DispatchWave(ctx, DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			3: {Number: 3, Title: "Three"},
		}},
		RepoPath:      repo,
		RunID:         "run-test-timeout",
		IssueNumbers:  []int{3},
		ThrottleLimit: 1,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(3), nil
		},
		Dispatch: func(context.Context, worker.Options) (worker.Result, error) {
			dispatchCalled = true
			return worker.Result{}, nil
		},
		LoadAttempts: state.LoadAttempts,
		Now:          time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}
	if dispatchCalled {
		t.Fatal("dispatch was called despite expired parent context")
	}
	if len(report.Results) != 1 || report.Results[0].Status != DispatchWaveStatusTimedOut {
		t.Fatalf("timeout result = %#v, want timed_out", report.Results)
	}
	if !strings.Contains(report.Results[0].Error, "parent run timed out") {
		t.Fatalf("timeout error missing hint: %#v", report.Results[0])
	}
	attempts, err := state.LoadAttempts(repo, "run-test-timeout")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Status != DispatchWaveStatusTimedOut {
		t.Fatalf("timeout synthetic attempt = %#v", attempts)
	}
}

func TestDispatchWavePropagatesNeedsHumanDispatchResult(t *testing.T) {
	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			1: {Number: 1, Title: "One"},
		}},
		RepoPath:      t.TempDir(),
		RunID:         "run-test-wave",
		IssueNumbers:  []int{1},
		ThrottleLimit: 1,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(1), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			result := waveWorkerResult(opts)
			result.Status = DispatchWaveStatusNeedsHuman
			result.PR = "https://github.com/owner/repo/pull/1"
			result.Report = waveReport(opts.IssueNumber, 101)
			return result, nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("result count = %d, want 1", len(report.Results))
	}
	got := report.Results[0]
	if got.Status != DispatchWaveStatusNeedsHuman || got.PR != "https://github.com/owner/repo/pull/1" {
		t.Fatalf("needs-human dispatch result = %#v", got)
	}
}

func TestDispatchWavePreservesPerWorkerReports(t *testing.T) {
	report, err := DispatchWave(context.Background(), DispatchWaveOptions{
		Reader: fakeReader{views: map[int]gh.Issue{
			11: {Number: 11, Title: "Eleven"},
			12: {Number: 12, Title: "Twelve"},
		}},
		RepoPath:      t.TempDir(),
		RunID:         "run-test-wave",
		IssueNumbers:  []int{11, 12},
		ThrottleLimit: 1,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(11, 12), nil
		},
		Dispatch: func(_ context.Context, opts worker.Options) (worker.Result, error) {
			result := waveWorkerResult(opts)
			result.Report = waveReport(opts.IssueNumber, int64(opts.IssueNumber*100))
			return result, nil
		},
		LoadAttempts: noAttempts,
	})
	if err != nil {
		t.Fatalf("DispatchWave returned error: %v", err)
	}
	if len(report.Results) != 2 {
		t.Fatalf("result count = %d, want 2", len(report.Results))
	}
	for i, result := range report.Results {
		if result.Report == nil {
			t.Fatalf("result %d missing report: %#v", i, result)
		}
		if result.Report.Action != fmt.Sprintf("implement issue #%d", result.Issue) {
			t.Fatalf("result %d report action = %q, want issue-specific action", i, result.Report.Action)
		}
	}
	if report.Results[0].Report.Model != "worker-model-11" ||
		report.Results[1].Report.Model != "worker-model-12" {
		t.Fatalf("reports collapsed or reordered: %#v", report.Results)
	}
	if report.Results[0].Report.Usage.TotalTokens == nil ||
		*report.Results[0].Report.Usage.TotalTokens != 1100 ||
		report.Results[1].Report.Usage.TotalTokens == nil ||
		*report.Results[1].Report.Usage.TotalTokens != 1200 {
		t.Fatalf("report token usage not preserved per issue: %#v", report.Results)
	}

	data, err := json.Marshal(report.Results)
	if err != nil {
		t.Fatalf("Marshal results: %v", err)
	}
	if got := strings.Count(string(data), `"report"`); got != 2 {
		t.Fatalf("marshaled results contain %d report fields, want 2: %s", got, string(data))
	}
}

func TestDispatchWaveStreamsWorkerCompletionBlocksAsWorkersFinish(t *testing.T) {
	var out bytes.Buffer
	started := make(chan int, 2)
	releaseSlow := make(chan struct{})
	completed := make(chan int, 2)

	reportDone := make(chan struct {
		report DispatchWaveReport
		err    error
	}, 1)
	go func() {
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
				started <- opts.IssueNumber
				if opts.IssueNumber == 1 {
					<-releaseSlow
				}
				result := waveWorkerResult(opts)
				result.Report = waveReport(opts.IssueNumber, int64(opts.IssueNumber*100))
				return result, nil
			},
			LoadAttempts: noAttempts,
			OnIssueComplete: func(completion DispatchWaveIssueComplete) error {
				pretty := completion.Result.Report.Pretty(reporter.PrettyOptions{Mode: reporter.PrettyModePlain})
				out.WriteString(RenderDispatchWaveIssueCompletion(completion.Result, pretty))
				completed <- completion.Result.Issue
				return nil
			},
		})
		reportDone <- struct {
			report DispatchWaveReport
			err    error
		}{report: report, err: err}
	}()

	seen := map[int]bool{}
	for len(seen) < 2 {
		select {
		case issue := <-started:
			seen[issue] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for workers to start")
		}
	}
	select {
	case issue := <-completed:
		if issue != 2 {
			t.Fatalf("first completed issue = %d, want 2", issue)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first worker completion")
	}
	close(releaseSlow)
	select {
	case issue := <-completed:
		if issue != 1 {
			t.Fatalf("second completed issue = %d, want 1", issue)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second worker completion")
	}

	select {
	case got := <-reportDone:
		if got.err != nil {
			t.Fatalf("DispatchWave returned error: %v", got.err)
		}
		if len(got.report.Results) != 2 {
			t.Fatalf("result count = %d, want 2", len(got.report.Results))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for dispatch wave")
	}

	text := out.String()
	for _, want := range []string{
		"DISPATCH WAVE WORKER #2 succeeded",
		"loopcoder report: worker succeeded",
		"- action: \"implement issue #2\"",
		"DISPATCH WAVE WORKER #1 succeeded",
		"- action: \"implement issue #1\"",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("streamed output missing %q:\n%s", want, text)
		}
	}
	blockTwoStart := strings.Index(text, "DISPATCH WAVE WORKER #2")
	blockOneStart := strings.Index(text, "DISPATCH WAVE WORKER #1")
	if blockTwoStart == -1 || blockOneStart == -1 || blockTwoStart > blockOneStart {
		t.Fatalf("streamed output not in completion order:\n%s", text)
	}
	blockTwoEnd := blockTwoStart + strings.Index(text[blockTwoStart:], "\n\n")
	if blockTwoEnd < blockTwoStart {
		t.Fatalf("streamed issue #2 block missing terminator:\n%s", text)
	}
	if blockOneStart < blockTwoEnd {
		t.Fatalf("streamed blocks interleaved:\n%s", text)
	}
}

func TestRenderDispatchWaveTextSurfacesPerWorkerReports(t *testing.T) {
	split := waveSplitReport(21, 2447, 4461, 6908)
	split.Provider = "claude"
	split.Model = "claude-sonnet-4-5"
	split.Effort = "high"
	split.DurationMS = 42000

	totalOnly := waveReport(22, 102585)
	totalOnly.Provider = "codex"
	totalOnly.Model = "gpt-5.5"
	totalOnly.Effort = "xhigh"
	totalOnly.DurationMS = 42000

	report := DispatchWaveReport{
		Repo:            "owner/repo",
		RepoPath:        "/repo",
		BaseBranch:      "main",
		RunID:           "run-test-wave",
		IssuesRequested: []int{21, 22, 23},
		StartedAt:       "2026-06-29T00:00:00Z",
		FinishedAt:      "2026-06-29T00:00:42Z",
		Results: []DispatchWaveIssueResult{
			{
				Issue:       21,
				Status:      DispatchWaveStatusSucceeded,
				Branch:      "loop/issue-21",
				PR:          "https://github.com/owner/repo/pull/21",
				AttemptPath: ".loopcoder/runs/run-test-wave/workers/job-21.attempt.json",
				Report:      split,
			},
			{
				Issue:       22,
				Status:      DispatchWaveStatusSucceeded,
				Branch:      "loop/issue-22",
				PR:          "https://github.com/owner/repo/pull/22",
				AttemptPath: ".loopcoder/runs/run-test-wave/workers/job-22.attempt.json",
				Report:      totalOnly,
			},
			{
				Issue:  23,
				Status: DispatchWaveStatusSkipped,
				Error:  "issue was not ready during preflight",
			},
		},
	}

	text := RenderDispatchWaveText(report)
	for _, want := range []string{
		"- #21 succeeded",
		"  branch: loop/issue-21",
		"  pr: https://github.com/owner/repo/pull/21",
		"  report: provider=claude model=claude-sonnet-4-5 (high) source=parsed permission=write duration=42s tokens input=2447 output=4461 total=6908 verified=true",
		"  attempt: .loopcoder/runs/run-test-wave/workers/job-21.attempt.json",
		"- #22 succeeded",
		"  report: provider=codex model=gpt-5.5 (xhigh) source=parsed permission=write duration=42s tokens input=not reported output=not reported total=102585 verified=true",
		"- #23 skipped",
		"  error: issue was not ready during preflight",
		"Verify successful PRs",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rendered dispatch wave missing %q:\n%s", want, text)
		}
	}
	if got := strings.Count(text, "  report: "); got != 2 {
		t.Fatalf("rendered %d report lines, want 2:\n%s", got, text)
	}
	if strings.Contains(text, "report: not reported") {
		t.Fatalf("nil report result should omit report line:\n%s", text)
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

func waveReport(issue int, totalTokens int64) *reporter.Report {
	started := time.Date(2026, 6, 29, 12, 0, issue, 0, time.UTC)
	return &reporter.Report{
		Role:        reporter.RoleWorker,
		Provider:    fmt.Sprintf("worker-provider-%d", issue),
		Model:       fmt.Sprintf("worker-model-%d", issue),
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionWrite,
		Action:      fmt.Sprintf("implement issue #%d", issue),
		ExitCode:    0,
		StartedAt:   started.Format(time.RFC3339),
		EndedAt:     started.Add(time.Second).Format(time.RFC3339),
		DurationMS:  1000,
		Usage: reporter.Usage{
			TotalTokens: &totalTokens,
		},
		Verified: true,
	}
}

func waveSplitReport(issue int, inputTokens, outputTokens, totalTokens int64) *reporter.Report {
	record := waveReport(issue, totalTokens)
	record.Usage.InputTokens = &inputTokens
	record.Usage.OutputTokens = &outputTokens
	return record
}

func noAttempts(string, string) ([]state.Attempt, error) {
	return nil, nil
}
