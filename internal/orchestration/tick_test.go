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
		Reader:           cleanRiskReader(77, "README.md"),
		IssueWriter:      noopTickIssueWriter{},
		RepoPath:         repo,
		BaseBranch:       "trunk",
		PreProdBranch:    "pre-prod",
		RunID:            "run-test-wave",
		WorkerProvider:   "codex",
		VerifierProvider: "claude",
		RequiredChecks:   []string{"verify"},
		Clock: func() time.Time {
			return now
		},
		Compile: func(_ context.Context, opts compiler.Options) (compiler.Report, error) {
			order = append(order, "compile")
			if opts.RepoPath != repo {
				t.Fatalf("compile repo = %q, want %q", opts.RepoPath, repo)
			}
			if !opts.Now.Equal(now) {
				t.Fatalf("compile now = %s, want %s", opts.Now, now)
			}
			return tickCompileReport(false), nil
		},
		LoadAttempts: noAttempts,
		ComputeReadySet: func(_ context.Context, opts Options) (report.ReadySetReport, error) {
			order = append(order, "ready-set")
			if opts.RunID != "run-test-wave" || opts.BaseBranch != "trunk" {
				t.Fatalf("ready-set opts = %#v", opts)
			}
			if !opts.Now.Equal(now) {
				t.Fatalf("ready-set now = %s, want %s", opts.Now, now)
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
			if !opts.Now.Equal(now) {
				t.Fatalf("dispatch-wave now = %s, want %s", opts.Now, now)
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
		RiskGate: func(ctx context.Context, opts RiskGateOptions) (RiskGateDecision, error) {
			order = append(order, "risk-gate")
			return EvaluateRiskGate(ctx, opts)
		},
		PreProdWriter: tickPreProdWriterFunc(func(_ context.Context, prNumber int, branch string) (gh.PreProdMergeResult, error) {
			order = append(order, "pre-prod-merge")
			if prNumber != 77 || branch != "pre-prod" {
				t.Fatalf("pre-prod merge pr=%d branch=%q", prNumber, branch)
			}
			return gh.PreProdMergeResult{PRNumber: prNumber, Branch: branch, Head: "loop/issue-101", SHA: "abc123"}, nil
		}),
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
	if !reflect.DeepEqual(order, []string{"compile", "ready-set", "dispatch-wave", "loopreview", "risk-gate", "pre-prod-merge", "state-push"}) {
		t.Fatalf("call order = %#v", order)
	}
	if report.Summary.DispatchedPRCount != 1 || report.Summary.ReviewPassCount != 1 || report.Summary.RiskGateCleanCount != 1 || report.Summary.PreProdMergeCount != 1 || report.Summary.FailureCount != 0 {
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

func TestTickReviewFailNeedsHumanAndDoesNotMerge(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 12, "https://github.com/owner/repo/pull/120")
	mergeCalled := false
	opts.PreProdWriter = tickPreProdWriterFunc(func(context.Context, int, string) (gh.PreProdMergeResult, error) {
		mergeCalled = true
		return gh.PreProdMergeResult{}, nil
	})
	opts.Loopreview = func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
		if opts.PRNumber != 120 {
			t.Fatalf("loopreview pr = %d, want 120", opts.PRNumber)
		}
		return tickLoopreview(loopreview.VerdictFail, "regression found"), nil
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopReviewNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if len(report.Reviews) != 1 || report.Reviews[0].Verdict != loopreview.VerdictFail {
		t.Fatalf("reviews = %#v", report.Reviews)
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "loopreview" || report.NeedsHuman[0].Issue != 12 {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %#v", report.Failures)
	}
	if mergeCalled {
		t.Fatal("pre-prod merge was called after loopreview fail")
	}
	if report.Summary.ReviewFailCount != 1 || report.Summary.NeedsHumanCount != 1 || report.Summary.FailureCount != 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
}

func TestTickLoopreviewErrorNeedsHuman(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 13, "https://github.com/owner/repo/pull/130")
	opts.Loopreview = func(context.Context, loopreview.Options) (loopreview.Result, error) {
		return loopreview.Result{}, errors.New("loopreview crashed")
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopReviewNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if len(report.Reviews) != 1 || report.Reviews[0].Verdict != loopreview.VerdictNeedsHuman || report.Reviews[0].Error != "loopreview crashed" {
		t.Fatalf("reviews = %#v", report.Reviews)
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "loopreview" || report.NeedsHuman[0].Issue != 13 {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %#v, want none", report.Failures)
	}
}

func TestTickRiskGateRedLinesNeedHumanAndDoNotMerge(t *testing.T) {
	tests := []struct {
		name         string
		reader       fakeReader
		wantCategory string
	}{
		{
			name:         "destructive mass deletion",
			reader:       destructiveRiskReader(201),
			wantCategory: RiskRedLineDestructive,
		},
		{
			name: "build not green",
			reader: fakeReader{
				checks:    map[int][]gh.Check{201: {{Name: "verify", Bucket: "fail"}}},
				diffFiles: map[int][]string{201: {"README.md"}},
				diffs:     map[int]string{201: modifiedDiff("README.md")},
			},
			wantCategory: RiskRedLineBuild,
		},
		{
			name: "loopcoder core path",
			reader: fakeReader{
				checks:    map[int][]gh.Check{201: passChecks()},
				diffFiles: map[int][]string{201: {"internal/orchestration/tick.go"}},
				diffs:     map[int]string{201: modifiedDiff("internal/orchestration/tick.go")},
			},
			wantCategory: RiskRedLineCore,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergeCalled := false
			opts := reviewReadyTickOptions(t.TempDir(), 20, "https://github.com/owner/repo/pull/201")
			opts.Reader = tt.reader
			opts.PreProdWriter = tickPreProdWriterFunc(func(context.Context, int, string) (gh.PreProdMergeResult, error) {
				mergeCalled = true
				return gh.PreProdMergeResult{}, nil
			})

			report, err := Tick(context.Background(), opts)
			if err != nil {
				t.Fatalf("Tick returned error: %v", err)
			}
			if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopRiskGateNeedsHuman {
				t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
			}
			if mergeCalled {
				t.Fatal("pre-prod merge was called despite risk red line")
			}
			if len(report.RiskGates) != 1 || report.RiskGates[0].Status != RiskGateStatusNeedsHuman {
				t.Fatalf("risk gates = %#v", report.RiskGates)
			}
			if !riskGateHasCategory(report.RiskGates[0], tt.wantCategory) {
				t.Fatalf("risk gate red lines = %#v, want category %q", report.RiskGates[0].RedLines, tt.wantCategory)
			}
			if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "risk-gate" {
				t.Fatalf("needs-human = %#v", report.NeedsHuman)
			}
			if len(report.PreProdMerges) != 0 {
				t.Fatalf("pre-prod merges = %#v, want none", report.PreProdMerges)
			}
		})
	}
}

func TestTickRejectsMainAsPreProdBranchBeforeMerge(t *testing.T) {
	mergeCalled := false
	opts := reviewReadyTickOptions(t.TempDir(), 21, "https://github.com/owner/repo/pull/210")
	opts.PreProdBranch = "main"
	opts.PreProdWriter = tickPreProdWriterFunc(func(context.Context, int, string) (gh.PreProdMergeResult, error) {
		mergeCalled = true
		return gh.PreProdMergeResult{}, nil
	})

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopRiskGateNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if mergeCalled {
		t.Fatal("pre-prod writer was called with main target")
	}
	if len(report.RiskGates) != 1 || report.RiskGates[0].Status != RiskGateStatusClean {
		t.Fatalf("risk gates = %#v", report.RiskGates)
	}
	if len(report.PreProdMerges) != 1 || report.PreProdMerges[0].Status != TickStatusNeedsHuman || !strings.Contains(report.PreProdMerges[0].Error, "reserved") {
		t.Fatalf("pre-prod merges = %#v", report.PreProdMerges)
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "pre-prod-merge" {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
}

func TestTickAutoRevertsPreProdWhenMergedCommitTurnsCIRed(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 22, "https://github.com/owner/repo/pull/220")
	opts.Reader = fakeReader{
		checks:    map[int][]gh.Check{220: passChecks()},
		diffFiles: map[int][]string{220: {"README.md"}},
		diffs:     map[int]string{220: modifiedDiff("README.md")},
		branchChecks: map[string]gh.BranchChecksResult{
			"pre-prod": {
				Branch:  "pre-prod",
				HeadSHA: "merge-sha",
				Checks:  []gh.Check{{Name: "verify", Bucket: "fail"}},
			},
		},
	}
	writer := &recordingPreProdWriter{
		mergeResult:  gh.PreProdMergeResult{PRNumber: 220, Branch: "pre-prod", Head: "loop/issue-22", SHA: "merge-sha"},
		revertResult: gh.PreProdRevertResult{PRNumber: 220, Branch: "pre-prod", RevertedSHA: "merge-sha", SHA: "revert-sha"},
	}
	opts.PreProdWriter = writer

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopPreProdNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if writer.mergeCalls != 1 || writer.revertCalls != 1 {
		t.Fatalf("writer calls merge=%d revert=%d", writer.mergeCalls, writer.revertCalls)
	}
	if writer.revertBranch != "pre-prod" || writer.revertSHA != "merge-sha" {
		t.Fatalf("revert target branch=%q sha=%q", writer.revertBranch, writer.revertSHA)
	}
	if len(report.PreProdHealth) != 1 || report.PreProdHealth[0].Status != PreProdHealthStatusRed {
		t.Fatalf("pre-prod health = %#v", report.PreProdHealth)
	}
	if len(report.PreProdReverts) != 1 || report.PreProdReverts[0].Status != TickStatusSucceeded || report.PreProdReverts[0].RevertedSHA != "merge-sha" {
		t.Fatalf("pre-prod reverts = %#v", report.PreProdReverts)
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "pre-prod-revert" || report.NeedsHuman[0].Issue != 22 {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
	if report.Summary.PreProdMergeCount != 1 || report.Summary.PreProdRevertCount != 1 || report.Summary.NeedsHumanCount != 1 {
		t.Fatalf("summary = %#v", report.Summary)
	}
}

func TestTickDoesNotRevertWhenPreProdCIStaysGreen(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 23, "https://github.com/owner/repo/pull/230")
	opts.Reader = fakeReader{
		checks:    map[int][]gh.Check{230: passChecks()},
		diffFiles: map[int][]string{230: {"README.md"}},
		diffs:     map[int]string{230: modifiedDiff("README.md")},
		branchChecks: map[string]gh.BranchChecksResult{
			"pre-prod": {
				Branch:  "pre-prod",
				HeadSHA: "merge-sha",
				Checks:  passChecks(),
			},
		},
	}
	writer := &recordingPreProdWriter{
		mergeResult:  gh.PreProdMergeResult{PRNumber: 230, Branch: "pre-prod", Head: "loop/issue-23", SHA: "merge-sha"},
		revertResult: gh.PreProdRevertResult{PRNumber: 230, Branch: "pre-prod", RevertedSHA: "merge-sha", SHA: "revert-sha"},
	}
	opts.PreProdWriter = writer

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusSucceeded || report.StopReason != TickStopCompleted {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if writer.mergeCalls != 1 || writer.revertCalls != 0 {
		t.Fatalf("writer calls merge=%d revert=%d", writer.mergeCalls, writer.revertCalls)
	}
	if len(report.PreProdHealth) != 1 || report.PreProdHealth[0].Status != PreProdHealthStatusGreen {
		t.Fatalf("pre-prod health = %#v", report.PreProdHealth)
	}
	if len(report.PreProdReverts) != 0 {
		t.Fatalf("pre-prod reverts = %#v", report.PreProdReverts)
	}
	if len(report.NeedsHuman) != 0 {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
}

func TestRiskGateAdditionalRedLinesCanOnlyRaiseRisk(t *testing.T) {
	reader := cleanRiskReader(301, "README.md")

	decision, err := EvaluateRiskGate(context.Background(), RiskGateOptions{
		Reader:         reader,
		PRNumber:       301,
		RequiredChecks: []string{"verify"},
		AdditionalRedLines: []RiskRedLine{{
			Category: "configured-high-risk",
			Detail:   "release policy requires a human",
		}},
	})
	if err != nil {
		t.Fatalf("EvaluateRiskGate returned error: %v", err)
	}
	if decision.Status != RiskGateStatusNeedsHuman || len(decision.RedLines) != 1 {
		t.Fatalf("decision = %#v, want raised needs-human", decision)
	}
	if decision.RedLines[0].Category != "configured-high-risk" {
		t.Fatalf("red lines = %#v", decision.RedLines)
	}
}

func TestTickStatePushFailureStopsStatePushFailed(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 14, "https://github.com/owner/repo/pull/140")
	opts.Loopreview = func(context.Context, loopreview.Options) (loopreview.Result, error) {
		return tickLoopreview(loopreview.VerdictPass, "review passed"), nil
	}
	opts.StatePush = func(context.Context, statebranch.PushOptions) (statebranch.PushResult, error) {
		return statebranch.PushResult{
			Branch: statebranch.DefaultBranch,
			Remote: statebranch.DefaultRemote,
		}, errors.New("state branch push failed")
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusFailed || report.StopReason != TickStopStatePushFailed {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if report.StatePush == nil || report.StatePush.Error != "state branch push failed" {
		t.Fatalf("state push = %#v", report.StatePush)
	}
	if !hasStatePushFailure(report.Failures) {
		t.Fatalf("failures = %#v, want state-push failure", report.Failures)
	}
}

func TestTickDispatchWaveErrorStopsDispatchWaveFailed(t *testing.T) {
	reviewCalled := false
	statePushCalled := false
	opts := TickOptions{
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
			return readySetReport(15), nil
		},
		DispatchWave: func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
			return tickWaveReport(), errors.New("dispatch wave failed")
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			reviewCalled = true
			return loopreview.Result{}, nil
		},
		StatePush: func(context.Context, statebranch.PushOptions) (statebranch.PushResult, error) {
			statePushCalled = true
			return statebranch.PushResult{Branch: statebranch.DefaultBranch, Remote: statebranch.DefaultRemote, Pushed: true}, nil
		},
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusFailed || report.StopReason != TickStopDispatchWaveFailed {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if reviewCalled {
		t.Fatal("loopreview was called after dispatch-wave error")
	}
	if !statePushCalled {
		t.Fatal("state push was not called after dispatch-wave error")
	}
	if len(report.Failures) != 1 || report.Failures[0].Step != "dispatch-wave" {
		t.Fatalf("failures = %#v", report.Failures)
	}
}

func TestTickReportUsesClockForNonZeroDuration(t *testing.T) {
	started := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	calls := 0
	opts := reviewReadyTickOptions(t.TempDir(), 16, "https://github.com/owner/repo/pull/160")
	opts.Clock = func() time.Time {
		current := started.Add(time.Duration(calls) * 90 * time.Second)
		calls++
		return current
	}
	opts.Loopreview = func(context.Context, loopreview.Options) (loopreview.Result, error) {
		return tickLoopreview(loopreview.VerdictPass, "review passed"), nil
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.StartedAt != "2026-07-02T12:00:00Z" || report.FinishedAt != "2026-07-02T12:01:30Z" {
		t.Fatalf("timing = started %q finished %q", report.StartedAt, report.FinishedAt)
	}
	if report.StartedAt == report.FinishedAt {
		t.Fatalf("started and finished timestamps should differ: %q", report.StartedAt)
	}
	if calls != 2 {
		t.Fatalf("clock calls = %d, want 2", calls)
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
	checkTypeSurface := func(tpe reflect.Type) []string {
		var hits []string
		if tpe.Kind() != reflect.Interface {
			return hits
		}
		for i := 0; i < tpe.NumMethod(); i++ {
			name := strings.ToLower(tpe.Method(i).Name)
			if strings.Contains(name, "main") || strings.Contains(name, "promote") {
				hits = append(hits, tpe.String()+"."+tpe.Method(i).Name)
			}
			if strings.Contains(name, "merge") && !strings.Contains(name, "preprod") {
				hits = append(hits, tpe.String()+"."+tpe.Method(i).Name)
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
		if strings.Contains(name, "main") || strings.Contains(typeName, "main") ||
			strings.Contains(name, "promote") || strings.Contains(typeName, "promote") {
			hits = append(hits, field.Name+" "+field.Type.String())
		}
		if strings.Contains(name, "merge") && !strings.Contains(name, "preprod") {
			hits = append(hits, field.Name+" "+field.Type.String())
		}
		if strings.Contains(typeName, "merge") && !strings.Contains(typeName, "preprod") {
			hits = append(hits, field.Name+" "+field.Type.String())
		}
		hits = append(hits, checkTypeSurface(field.Type)...)
	}
	if len(hits) > 0 {
		t.Fatalf("tick dependency surface exposes forbidden main/promote or non-preprod merge names: %s", strings.Join(hits, ", "))
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

func reviewReadyTickOptions(repo string, issue int, pr string) TickOptions {
	prNumber, _ := parseTickPRNumber(pr)
	return TickOptions{
		Reader:           cleanRiskReader(prNumber, "README.md"),
		IssueWriter:      noopTickIssueWriter{},
		RepoPath:         repo,
		RunID:            "run-test-wave",
		VerifierProvider: "claude",
		PreProdBranch:    "pre-prod",
		RequiredChecks:   []string{"verify"},
		Compile: func(context.Context, compiler.Options) (compiler.Report, error) {
			return tickCompileReport(false), nil
		},
		LoadAttempts: noAttempts,
		ComputeReadySet: func(context.Context, Options) (report.ReadySetReport, error) {
			return readySetReport(issue), nil
		},
		DispatchWave: func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
			return tickWaveReport(DispatchWaveIssueResult{
				Issue:  issue,
				Status: DispatchWaveStatusSucceeded,
				PR:     pr,
			}), nil
		},
		Loopreview: func(context.Context, loopreview.Options) (loopreview.Result, error) {
			return tickLoopreview(loopreview.VerdictPass, "review passed"), nil
		},
		PreProdWriter: tickPreProdWriterFunc(func(_ context.Context, prNumber int, branch string) (gh.PreProdMergeResult, error) {
			return gh.PreProdMergeResult{PRNumber: prNumber, Branch: branch, Head: "loop/issue-test", SHA: "abc123"}, nil
		}),
		StatePush: func(context.Context, statebranch.PushOptions) (statebranch.PushResult, error) {
			return statebranch.PushResult{
				Branch: statebranch.DefaultBranch,
				Remote: statebranch.DefaultRemote,
				Pushed: true,
			}, nil
		},
	}
}

type tickPreProdWriterFunc func(context.Context, int, string) (gh.PreProdMergeResult, error)

func (f tickPreProdWriterFunc) MergeToPreProd(ctx context.Context, prNumber int, preProdBranch string) (gh.PreProdMergeResult, error) {
	return f(ctx, prNumber, preProdBranch)
}

func (f tickPreProdWriterFunc) RevertOnPreProd(context.Context, int, string, string) (gh.PreProdRevertResult, error) {
	return gh.PreProdRevertResult{}, nil
}

type recordingPreProdWriter struct {
	mergeResult  gh.PreProdMergeResult
	mergeErr     error
	revertResult gh.PreProdRevertResult
	revertErr    error
	mergeCalls   int
	revertCalls  int
	revertBranch string
	revertSHA    string
}

func (w *recordingPreProdWriter) MergeToPreProd(context.Context, int, string) (gh.PreProdMergeResult, error) {
	w.mergeCalls++
	return w.mergeResult, w.mergeErr
}

func (w *recordingPreProdWriter) RevertOnPreProd(_ context.Context, _ int, branch, mergeSHA string) (gh.PreProdRevertResult, error) {
	w.revertCalls++
	w.revertBranch = branch
	w.revertSHA = mergeSHA
	return w.revertResult, w.revertErr
}

func cleanRiskReader(prNumber int, files ...string) fakeReader {
	if len(files) == 0 {
		files = []string{"README.md"}
	}
	return fakeReader{
		checks:    map[int][]gh.Check{prNumber: passChecks()},
		diffFiles: map[int][]string{prNumber: files},
		diffs:     map[int]string{prNumber: modifiedDiff(files[0])},
	}
}

func destructiveRiskReader(prNumber int) fakeReader {
	files := []string{"a.txt", "b.txt", "c.txt", "d.txt", "e.txt"}
	return fakeReader{
		checks:    map[int][]gh.Check{prNumber: passChecks()},
		diffFiles: map[int][]string{prNumber: files},
		diffs:     map[int]string{prNumber: deletedDiff(files...)},
	}
}

func passChecks() []gh.Check {
	return []gh.Check{{Name: "verify", Bucket: "pass"}}
}

func modifiedDiff(file string) string {
	return "diff --git a/" + file + " b/" + file + "\n" +
		"--- a/" + file + "\n" +
		"+++ b/" + file + "\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n"
}

func deletedDiff(files ...string) string {
	var b strings.Builder
	for _, file := range files {
		b.WriteString("diff --git a/" + file + " b/" + file + "\n")
		b.WriteString("deleted file mode 100644\n")
		b.WriteString("--- a/" + file + "\n")
		b.WriteString("+++ /dev/null\n")
		b.WriteString("@@ -1 +0,0 @@\n")
		b.WriteString("-old\n")
	}
	return b.String()
}

func riskGateHasCategory(gate TickRiskGateResult, category string) bool {
	for _, line := range gate.RedLines {
		if line.Category == category {
			return true
		}
	}
	return false
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
