package orchestration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
)

func TestTickHappyPass(t *testing.T) {
	repo := t.TempDir()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	var order []string

	report, err := Tick(context.Background(), TickOptions{
		Reader:           fakeReader{},
		IssueWriter:      noopTickIssueWriter{},
		RepoPath:         repo,
		BaseBranch:       "trunk",
		RunID:            "run-test-wave",
		WorkerProvider:   "codex",
		VerifierProvider: "claude",
		Now:              now,
		Compile: func(_ context.Context, opts compiler.Options) (compiler.Report, error) {
			order = append(order, "compile")
			if opts.RepoPath != repo {
				t.Fatalf("compile repo = %q, want %q", opts.RepoPath, repo)
			}
			return tickCompileReport(false), nil
		},
		LoadAttempts: noAttempts,
		ComputeReadySet: func(_ context.Context, opts Options) (report.ReadySetReport, error) {
			order = append(order, "ready-set")
			if opts.RunID != "run-test-wave" || opts.BaseBranch != "trunk" {
				t.Fatalf("ready-set opts = %#v", opts)
			}
			return readySetReport(101), nil
		},
		DispatchWave: func(_ context.Context, opts DispatchWaveOptions) (DispatchWaveReport, error) {
			order = append(order, "dispatch-wave")
			if opts.ReadySet == nil || len(opts.ReadySet.Ready) != 1 {
				t.Fatalf("dispatch-wave ready-set = %#v", opts.ReadySet)
			}
			if opts.Provider != "codex" || opts.BaseBranch != "trunk" || opts.RunID != "run-test-wave" {
				t.Fatalf("dispatch-wave opts = %#v", opts)
			}
			return tickWaveReport(DispatchWaveIssueResult{
				Issue:  101,
				Status: DispatchWaveStatusSucceeded,
				Branch: "loop/issue-101",
				PR:     "https://github.com/owner/repo/pull/77",
			}), nil
		},
		Loopreview: func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
			order = append(order, "loopreview")
			if opts.PRNumber != 77 || opts.Provider != "claude" || opts.BaseBranch != "trunk" {
				t.Fatalf("loopreview opts = %#v", opts)
			}
			return tickLoopreview(loopreview.VerdictPass, "review passed"), nil
		},
		StatePush: func(_ context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error) {
			order = append(order, "state-push")
			if opts.RepoPath != repo || opts.RunID != "run-test-wave" {
				t.Fatalf("state push opts = %#v", opts)
			}
			return statebranch.PushResult{
				RepoPath:  repo,
				RunID:     opts.RunID,
				Branch:    statebranch.DefaultBranch,
				Remote:    statebranch.DefaultRemote,
				Committed: true,
				Pushed:    true,
				Files:     []string{"runs/run-test-wave/state.json"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusSucceeded || report.StopReason != TickStopCompleted {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if !reflect.DeepEqual(order, []string{"compile", "ready-set", "dispatch-wave", "loopreview", "state-push"}) {
		t.Fatalf("call order = %#v", order)
	}
	if report.Summary.DispatchedPRCount != 1 || report.Summary.ReviewPassCount != 1 || report.Summary.FailureCount != 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	if report.StatePush == nil || !report.StatePush.Pushed {
		t.Fatalf("state push = %#v, want pushed", report.StatePush)
	}
}

func TestTickGuardrailBlockedStopsBeforeReview(t *testing.T) {
	repo := t.TempDir()
	reviewCalled := false
	statePushCalled := false

	report, err := Tick(context.Background(), TickOptions{
		Reader:           fakeReader{},
		IssueWriter:      noopTickIssueWriter{},
		RepoPath:         repo,
		RunID:            "run-test-wave",
		VerifierProvider: "claude",
		Compile: func(context.Context, compiler.Options) (compiler.Report, error) {
			return tickCompileReport(false), nil
		},
		LoadAttempts: noAttempts,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(1, 2), nil
		},
		DispatchWave: func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
			return tickWaveReport(
				DispatchWaveIssueResult{Issue: 1, Status: DispatchWaveStatusSucceeded, PR: "https://github.com/owner/repo/pull/11"},
				DispatchWaveIssueResult{Issue: 2, Status: DispatchWaveStatusNeedsHuman, Error: "needs-human: guardrails.budget.max_total_attempts"},
			), nil
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			reviewCalled = true
			return loopreview.Result{}, nil
		},
		StatePush: func(context.Context, statebranch.PushOptions) (statebranch.PushResult, error) {
			statePushCalled = true
			return statebranch.PushResult{Branch: statebranch.DefaultBranch, Remote: statebranch.DefaultRemote, Pushed: true}, nil
		},
	})
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopGuardrailNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if reviewCalled {
		t.Fatal("loopreview was called after a guardrail needs-human result")
	}
	if !statePushCalled {
		t.Fatal("state push was not called after dispatch-wave produced run state")
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Issue != 2 {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
}

func TestTickNeedsHumanPRIsReported(t *testing.T) {
	repo := t.TempDir()

	report, err := Tick(context.Background(), TickOptions{
		Reader:           fakeReader{},
		IssueWriter:      noopTickIssueWriter{},
		RepoPath:         repo,
		RunID:            "run-test-wave",
		VerifierProvider: "claude",
		Compile: func(context.Context, compiler.Options) (compiler.Report, error) {
			return tickCompileReport(false), nil
		},
		LoadAttempts: noAttempts,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(9), nil
		},
		DispatchWave: func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
			return tickWaveReport(DispatchWaveIssueResult{
				Issue:  9,
				Status: DispatchWaveStatusSucceeded,
				PR:     "https://github.com/owner/repo/pull/90",
			}), nil
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			return tickLoopreview(loopreview.VerdictNeedsHuman, "manual review required"), nil
		},
		StatePush: func(context.Context, statebranch.PushOptions) (statebranch.PushResult, error) {
			return statebranch.PushResult{Branch: statebranch.DefaultBranch, Remote: statebranch.DefaultRemote, Pushed: true}, nil
		},
	})
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopReviewNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if len(report.Reviews) != 1 || report.Reviews[0].Verdict != loopreview.VerdictNeedsHuman {
		t.Fatalf("reviews = %#v", report.Reviews)
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].PR != "https://github.com/owner/repo/pull/90" {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
}

func TestTickNoReadyWorkStopsWithoutDispatch(t *testing.T) {
	dispatchCalled := false
	statePushCalled := false

	report, err := Tick(context.Background(), TickOptions{
		Reader:           fakeReader{},
		IssueWriter:      noopTickIssueWriter{},
		RepoPath:         t.TempDir(),
		RunID:            "run-test-wave",
		VerifierProvider: "claude",
		Compile: func(context.Context, compiler.Options) (compiler.Report, error) {
			return tickCompileReport(false), nil
		},
		LoadAttempts: noAttempts,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return report.ReadySetReport{
				Repo:       "owner/repo",
				BaseBranch: "main",
				Ready:      []report.ReadyIssue{},
				Blocked: []report.BlockedIssue{{
					Issue:          4,
					Title:          "Blocked",
					Classification: "blocked-by-unmerged-dep",
					Reason:         "blocked by #3",
				}},
			}, nil
		},
		DispatchWave: func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
			dispatchCalled = true
			return DispatchWaveReport{}, nil
		},
		StatePush: func(context.Context, statebranch.PushOptions) (statebranch.PushResult, error) {
			statePushCalled = true
			return statebranch.PushResult{}, nil
		},
	})
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNoReadyWork || report.StopReason != TickStopNoReadyWork {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if dispatchCalled {
		t.Fatal("dispatch-wave was called with no ready work")
	}
	if statePushCalled {
		t.Fatal("state push was called without dispatch-created run state")
	}
}

func TestTickDependencySurfaceHasNoProductionUpdateMethod(t *testing.T) {
	forbidden := []string{"merge", "promote"}
	checkTypeSurface := func(tpe reflect.Type) []string {
		var hits []string
		if tpe.Kind() != reflect.Interface {
			return hits
		}
		for i := 0; i < tpe.NumMethod(); i++ {
			name := strings.ToLower(tpe.Method(i).Name)
			for _, word := range forbidden {
				if strings.Contains(name, word) {
					hits = append(hits, tpe.String()+"."+tpe.Method(i).Name)
				}
			}
		}
		return hits
	}

	tickOptions := reflect.TypeOf(TickOptions{})
	var hits []string
	for i := 0; i < tickOptions.NumField(); i++ {
		field := tickOptions.Field(i)
		name := strings.ToLower(field.Name)
		typeName := strings.ToLower(field.Type.String())
		for _, word := range forbidden {
			if strings.Contains(name, word) || strings.Contains(typeName, word) {
				hits = append(hits, field.Name+" "+field.Type.String())
			}
		}
		hits = append(hits, checkTypeSurface(field.Type)...)
	}
	if len(hits) > 0 {
		t.Fatalf("tick dependency surface exposes forbidden production-update names: %s", strings.Join(hits, ", "))
	}
}

func TestTickCompileErrorStillReportsFailure(t *testing.T) {
	report, err := Tick(context.Background(), TickOptions{
		Reader:      fakeReader{},
		IssueWriter: noopTickIssueWriter{},
		RepoPath:    t.TempDir(),
		Compile: func(context.Context, compiler.Options) (compiler.Report, error) {
			return tickCompileReport(false), errors.New("compile failed")
		},
	})
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusFailed || report.StopReason != TickStopCompileFailed {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].Detail, "compile failed") {
		t.Fatalf("failures = %#v", report.Failures)
	}
}

func tickCompileReport(planApproval bool) compiler.Report {
	return compiler.Report{
		Version:              compiler.ReportVersion,
		Repo:                 "owner/repo",
		RepoPath:             "/repo",
		PlanApprovalRequired: planApproval,
		Summary: compiler.Summary{
			CreatedCount:   1,
			UpdatedCount:   2,
			UnchangedCount: 3,
			ClosedCount:    4,
			TotalCount:     10,
		},
	}
}

func tickWaveReport(results ...DispatchWaveIssueResult) DispatchWaveReport {
	return DispatchWaveReport{
		Repo:            "owner/repo",
		RepoPath:        "/repo",
		BaseBranch:      "main",
		RunID:           "run-test-wave",
		IssuesRequested: []int{1},
		Results:         results,
	}
}

func tickLoopreview(verdict, evidence string) loopreview.Result {
	return loopreview.Result{
		Verdict: loopreview.Verdict{
			Verdict:         verdict,
			Findings:        []loopreview.Finding{},
			Evidence:        evidence,
			SpecConformance: loopreview.SpecConformancePass,
		},
		ExitCode: loopreview.ExitCodeForVerdict(verdict),
	}
}

type noopTickIssueWriter struct{}

func (noopTickIssueWriter) RepoName(context.Context) (string, error) { return "owner/repo", nil }
func (noopTickIssueWriter) ListIssues(context.Context, string) ([]gh.Issue, error) {
	return nil, nil
}
func (noopTickIssueWriter) CreateIssue(context.Context, string, string, []string) (gh.Issue, error) {
	return gh.Issue{}, nil
}
func (noopTickIssueWriter) UpdateIssue(context.Context, int, string, string, []string, []string) (gh.Issue, error) {
	return gh.Issue{}, nil
}
func (noopTickIssueWriter) CloseIssue(context.Context, int) error { return nil }
