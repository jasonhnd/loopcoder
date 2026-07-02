package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
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

func TestTickConfiguredEvidenceSurfacesInJSONAndText(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 31, "https://github.com/owner/repo/pull/310")
	opts.ConfiguredEvidence = []config.EvidenceArtifact{
		{ProjectType: "website", PreviewURL: "https://preview.example.com/pr-310"},
		{ProjectType: "cli", ExampleOutput: "$ loopcoder --version\nversion=dev"},
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	wantEvidence := opts.ConfiguredEvidence
	if !reflect.DeepEqual(report.DispatchWave.Results[0].ConfiguredEvidence, wantEvidence) {
		t.Fatalf("dispatch evidence = %#v, want %#v", report.DispatchWave.Results[0].ConfiguredEvidence, wantEvidence)
	}
	if !reflect.DeepEqual(report.Reviews[0].ConfiguredEvidence, wantEvidence) {
		t.Fatalf("review evidence = %#v, want %#v", report.Reviews[0].ConfiguredEvidence, wantEvidence)
	}
	if !reflect.DeepEqual(report.PreProdMerges[0].ConfiguredEvidence, wantEvidence) {
		t.Fatalf("pre-prod merge evidence = %#v, want %#v", report.PreProdMerges[0].ConfiguredEvidence, wantEvidence)
	}

	data, err := MarshalTickJSON(report)
	if err != nil {
		t.Fatalf("MarshalTickJSON returned error: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("tick JSON is invalid:\n%s", string(data))
	}
	for _, want := range []string{
		`"configured_evidence"`,
		`"project_type": "website"`,
		`"preview_url": "https://preview.example.com/pr-310"`,
		`"example_output": "$ loopcoder --version\nversion=dev"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("tick JSON missing %q:\n%s", want, string(data))
		}
	}

	text := RenderTickText(report)
	for _, want := range []string{
		"configured_evidence: website preview_url=https://preview.example.com/pr-310",
		`configured_evidence: cli example_output=$ loopcoder --version\nversion=dev`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("tick text missing %q:\n%s", want, text)
		}
	}
}

func TestTickConfiguredEvidenceAbsentOmitsJSONAndText(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 32, "https://github.com/owner/repo/pull/320")

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	data, err := MarshalTickJSON(report)
	if err != nil {
		t.Fatalf("MarshalTickJSON returned error: %v", err)
	}
	if strings.Contains(string(data), "configured_evidence") {
		t.Fatalf("tick JSON unexpectedly contains configured_evidence:\n%s", string(data))
	}
	text := RenderTickText(report)
	if strings.Contains(text, "configured_evidence") {
		t.Fatalf("tick text unexpectedly contains configured_evidence:\n%s", text)
	}
}

func TestTickPendingPromotionSurfacesLedgerStateInJSONAndText(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 33, "https://github.com/owner/repo/pull/330")
	opts.ConfiguredEvidence = []config.EvidenceArtifact{
		{ProjectType: "website", PreviewURL: "https://preview.example.com/pr-330"},
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if len(report.PendingPromotion) != 1 {
		t.Fatalf("pending promotion = %#v, want one item", report.PendingPromotion)
	}
	pending := report.PendingPromotion[0]
	if pending.Issue != 33 || pending.PRNumber != 330 || pending.Branch != "pre-prod" || pending.Status != tickStatusPendingPromotion {
		t.Fatalf("pending promotion item = %#v", pending)
	}
	if pending.Evidence != "review passed" {
		t.Fatalf("pending evidence = %q, want review evidence", pending.Evidence)
	}
	if !reflect.DeepEqual(pending.ConfiguredEvidence, opts.ConfiguredEvidence) {
		t.Fatalf("pending configured evidence = %#v, want %#v", pending.ConfiguredEvidence, opts.ConfiguredEvidence)
	}
	if report.Summary.PendingPromotionCount != 1 {
		t.Fatalf("summary pending count = %d, want 1", report.Summary.PendingPromotionCount)
	}

	data, err := MarshalTickJSON(report)
	if err != nil {
		t.Fatalf("MarshalTickJSON returned error: %v", err)
	}
	for _, want := range []string{
		`"pending_promotion"`,
		`"pr_number": 330`,
		`"status": "pending-promotion"`,
		`"evidence": "review passed"`,
		`"preview_url": "https://preview.example.com/pr-330"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("tick JSON missing %q:\n%s", want, string(data))
		}
	}

	text := RenderTickText(report)
	for _, want := range []string{
		"Pending promotion",
		"PR #330 issue #33 pending-promotion branch=pre-prod",
		"evidence: review passed",
		"configured_evidence: website preview_url=https://preview.example.com/pr-330",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("tick text missing %q:\n%s", want, text)
		}
	}
}

func TestTickPendingPromotionAbsentOmitsJSON(t *testing.T) {
	report := normalizeTickReport(TickReport{
		Version:       TickReportVersion,
		Repo:          "owner/repo",
		RepoPath:      "/repo",
		BaseBranch:    "main",
		PreProdBranch: "pre-prod",
		RunID:         "run-test-empty",
		Status:        TickStatusNoReadyWork,
		StopReason:    TickStopNoReadyWork,
	})

	data, err := MarshalTickJSON(report)
	if err != nil {
		t.Fatalf("MarshalTickJSON returned error: %v", err)
	}
	if strings.Contains(string(data), "pending_promotion") || strings.Contains(string(data), "pending_promotion_count") {
		t.Fatalf("tick JSON unexpectedly contains pending promotion fields:\n%s", string(data))
	}
	text := RenderTickText(report)
	if !strings.Contains(text, "Pending promotion\n- none") {
		t.Fatalf("tick text missing empty pending promotion section:\n%s", text)
	}
}

func TestTickPendingPromotionPromoteLedgerClearsDurablePending(t *testing.T) {
	repo := t.TempDir()
	appendPendingPromotionEvent(t, repo, "run-20260702T120000Z-wave", "2026-07-02T12:00:00Z", TickPendingPromotion{
		RunID:    "run-20260702T120000Z-wave",
		Issue:    44,
		PR:       "https://github.com/owner/repo/pull/440",
		PRNumber: 440,
		Branch:   "pre-prod",
		SHA:      "merge-sha",
		Status:   tickStatusPendingPromotion,
		Evidence: "ready for promotion",
	})
	appendPromoteAttemptEvent(t, repo, "run-20260702T130000Z-wave", "2026-07-02T13:00:00Z", PromoteReport{
		Version:       PromoteReportVersion,
		RepoPath:      repo,
		RunID:         "run-20260702T130000Z-wave",
		PreProdBranch: "pre-prod",
		MainBranch:    "main",
		Status:        PromoteStatusSucceeded,
		Promoted: PromoteMainResult{
			PreProdBranch: "pre-prod",
			MainBranch:    "main",
			Status:        PromoteStatusSucceeded,
			SHA:           "main-sha",
		},
	})

	pending := loadTickPendingPromotionLedger(repo, "pre-prod")
	if len(pending) != 0 {
		t.Fatalf("pending promotion = %#v, want cleared by promote ledger", pending)
	}
}

func TestTickPendingPromotionKickBackLedgerRemovesOnlyKickedItem(t *testing.T) {
	repo := t.TempDir()
	appendPendingPromotionEvent(t, repo, "run-20260702T120000Z-wave", "2026-07-02T12:00:00Z",
		TickPendingPromotion{Issue: 45, PRNumber: 450, Branch: "pre-prod", SHA: "merge-a", Status: tickStatusPendingPromotion},
		TickPendingPromotion{Issue: 46, PRNumber: 460, Branch: "pre-prod", SHA: "merge-b", Status: tickStatusPendingPromotion},
	)
	appendPromoteAttemptEvent(t, repo, "run-20260702T130000Z-wave", "2026-07-02T13:00:00Z", PromoteReport{
		Version:       PromoteReportVersion,
		RepoPath:      repo,
		RunID:         "run-20260702T130000Z-wave",
		PreProdBranch: "pre-prod",
		MainBranch:    "main",
		Status:        PromoteStatusFailed,
		KickedBack: []PromoteKickBackResult{{
			Item:        "#450",
			PRNumber:    450,
			Branch:      "pre-prod",
			RevertedSHA: "merge-a",
			Status:      PromoteStatusSucceeded,
		}},
		Promoted: PromoteMainResult{
			PreProdBranch: "pre-prod",
			MainBranch:    "main",
			Status:        PromoteStatusFailed,
			Error:         "promotion failed",
		},
	})

	pending := loadTickPendingPromotionLedger(repo, "pre-prod")
	if len(pending) != 1 || pending[0].PRNumber != 460 {
		t.Fatalf("pending promotion = %#v, want only PR #460 after kick-back", pending)
	}
}

func TestTickPendingPromotionLedgerUsesFileWriteOrderForSameTimestamp(t *testing.T) {
	repo := t.TempDir()
	timestamp := "2026-07-02T12:00:00Z"
	pendingRunID := "run-z-pending"
	promoteRunID := "run-a-promote"

	appendPendingPromotionEvent(t, repo, pendingRunID, timestamp, TickPendingPromotion{
		Issue:    47,
		PRNumber: 470,
		Branch:   "pre-prod",
		SHA:      "merge-sha",
		Status:   tickStatusPendingPromotion,
	})
	appendPromoteAttemptEvent(t, repo, promoteRunID, timestamp, PromoteReport{
		Version:       PromoteReportVersion,
		RepoPath:      repo,
		RunID:         promoteRunID,
		PreProdBranch: "pre-prod",
		MainBranch:    "main",
		Status:        PromoteStatusSucceeded,
		Promoted: PromoteMainResult{
			PreProdBranch: "pre-prod",
			MainBranch:    "main",
			Status:        PromoteStatusSucceeded,
			SHA:           "main-sha",
		},
	})
	pendingTime := time.Date(2026, 7, 2, 12, 0, 0, 100, time.UTC)
	promoteTime := pendingTime.Add(time.Millisecond)
	if err := os.Chtimes(state.EventsPath(repo, pendingRunID), pendingTime, pendingTime); err != nil {
		t.Fatalf("Chtimes pending event: %v", err)
	}
	if err := os.Chtimes(state.EventsPath(repo, promoteRunID), promoteTime, promoteTime); err != nil {
		t.Fatalf("Chtimes promote event: %v", err)
	}

	pending := loadTickPendingPromotionLedger(repo, "pre-prod")
	if len(pending) != 0 {
		t.Fatalf("pending promotion = %#v, want cleared by later promote event with identical timestamp", pending)
	}
}

func TestTickHumanDecisionSectionsSurfaceInJSONAndText(t *testing.T) {
	evidence := []config.EvidenceArtifact{{ProjectType: "cli", TestResults: "go test ./... passed"}}
	report := normalizeTickReport(TickReport{
		Version:       TickReportVersion,
		Repo:          "owner/repo",
		RepoPath:      "/repo",
		BaseBranch:    "main",
		PreProdBranch: "pre-prod",
		RunID:         "run-test-sections",
		Status:        TickStatusNeedsHuman,
		StopReason:    TickStopReviewNeedsHuman,
		NeedsHuman: []TickIssue{{
			Step:               "loopreview",
			Issue:              70,
			PR:                 "https://github.com/owner/repo/pull/700",
			Detail:             "manual review required",
			ConfiguredEvidence: evidence,
		}},
		Failures: []TickIssue{{
			Step:               "dispatch-wave",
			Issue:              71,
			Detail:             "worker failed",
			ConfiguredEvidence: evidence,
		}},
	})

	data, err := MarshalTickJSON(report)
	if err != nil {
		t.Fatalf("MarshalTickJSON returned error: %v", err)
	}
	for _, want := range []string{
		`"needs_human"`,
		`"failures"`,
		`"detail": "manual review required"`,
		`"detail": "worker failed"`,
		`"test_results": "go test ./... passed"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("tick JSON missing %q:\n%s", want, string(data))
		}
	}

	text := RenderTickText(report)
	for _, want := range []string{
		"Needs human",
		"- loopreview #70 https://github.com/owner/repo/pull/700: manual review required",
		"Failures",
		"- dispatch-wave #71: worker failed",
		"configured_evidence: cli test_results=go test ./... passed",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("tick text missing %q:\n%s", want, text)
		}
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

func TestTickGuardrailFrozenReadySetStopsBeforeDispatch(t *testing.T) {
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
					Issue:          24,
					Title:          "Frozen",
					Classification: "guardrail-frozen",
					Reason:         "guardrail circuit is frozen",
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
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopGuardrailFrozen {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if dispatchCalled {
		t.Fatal("dispatch-wave was called for guardrail-frozen ready-set")
	}
	if statePushCalled {
		t.Fatal("state push was called before dispatch-created run state")
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "ready-set" || report.NeedsHuman[0].Issue != 24 {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
}

func TestTickDispatchFailureRecoveredPRContinuesThroughPreProd(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 22, "https://github.com/owner/repo/pull/220")
	reader := cleanRiskReader(222, "README.md")
	reader.views = map[int]gh.Issue{22: {Number: 22, Title: "Issue 22", Body: "issue body"}}
	opts.Reader = reader
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		return tickWaveReport(DispatchWaveIssueResult{
			Issue:               22,
			Status:              DispatchWaveStatusFailed,
			Branch:              "loop/issue-22",
			Error:               "worker failed",
			RecoveryContextPath: ".loopcoder/runs/run-test-wave/recovery/job-22-context.md",
		}), nil
	}
	recoverCalled := false
	opts.Recover = func(_ context.Context, opts recovery.Options) (recovery.Result, error) {
		recoverCalled = true
		if opts.IssueNumber != 22 || opts.SkipAdoptPR || !strings.Contains(opts.FailureContext, "worker failed") {
			t.Fatalf("recover opts = %#v", opts)
		}
		review := tickLoopreview(loopreview.VerdictPass, "recovered review passed")
		return recovery.Result{
			Action: recovery.ActionSucceeded,
			DispatchResult: &recovery.DispatchResult{
				OK:     true,
				Issue:  22,
				Branch: "loop/issue-22-retry-2",
				RunID:  opts.RunID,
				PR:     "https://github.com/owner/repo/pull/222",
				Status: "succeeded",
			},
			ReviewResult: &review,
			RecoveryAttempts: []recovery.AttemptRecord{{
				Version:  recovery.AttemptRecordVersion,
				Issue:    22,
				RunID:    opts.RunID,
				Attempt:  2,
				Strategy: recovery.AttemptStrategySameConfig,
				Status:   "succeeded",
				PR:       "https://github.com/owner/repo/pull/222",
			}},
		}, nil
	}
	mergedPR := 0
	opts.PreProdWriter = tickPreProdWriterFunc(func(_ context.Context, prNumber int, branch string) (gh.PreProdMergeResult, error) {
		mergedPR = prNumber
		return gh.PreProdMergeResult{PRNumber: prNumber, Branch: branch, Head: "loop/issue-22-retry-2", SHA: "abc123"}, nil
	})

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusSucceeded || report.StopReason != TickStopCompleted {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if !recoverCalled || mergedPR != 222 {
		t.Fatalf("recoverCalled=%v mergedPR=%d", recoverCalled, mergedPR)
	}
	if len(report.Recoveries) != 1 || report.Recoveries[0].Action != string(recovery.ActionSucceeded) {
		t.Fatalf("recoveries = %#v", report.Recoveries)
	}
	if len(report.Reviews) != 1 || report.Reviews[0].Verdict != loopreview.VerdictPass || report.Reviews[0].PRNumber != 222 {
		t.Fatalf("reviews = %#v", report.Reviews)
	}
	if len(report.NeedsHuman) != 0 || len(report.Failures) != 0 {
		t.Fatalf("needs-human=%#v failures=%#v", report.NeedsHuman, report.Failures)
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
	recoverCalled := false
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
	opts.Recover = func(_ context.Context, opts recovery.Options) (recovery.Result, error) {
		recoverCalled = true
		if opts.IssueNumber != 12 || !opts.SkipAdoptPR || !strings.Contains(opts.FailureContext, "regression found") {
			t.Fatalf("recover opts = %#v", opts)
		}
		return recovery.Result{
			Action: recovery.ActionBlocked,
			Report: "BLOCKED: retry limit reached\n",
		}, nil
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopRecoverNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if !recoverCalled {
		t.Fatal("recover was not called after loopreview fail")
	}
	if len(report.Reviews) != 1 || report.Reviews[0].Verdict != loopreview.VerdictFail {
		t.Fatalf("reviews = %#v", report.Reviews)
	}
	if len(report.Recoveries) != 1 || report.Recoveries[0].Action != string(recovery.ActionBlocked) {
		t.Fatalf("recoveries = %#v", report.Recoveries)
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "recover" || report.NeedsHuman[0].Issue != 12 {
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

func TestTickReviewFailRecoveredPRContinuesThroughPreProd(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 12, "https://github.com/owner/repo/pull/120")
	opts.Reader = cleanRiskReader(121, "README.md")
	loopreviewCalls := 0
	opts.Loopreview = func(_ context.Context, opts loopreview.Options) (loopreview.Result, error) {
		loopreviewCalls++
		if opts.PRNumber != 120 {
			t.Fatalf("initial loopreview pr = %d, want 120", opts.PRNumber)
		}
		return tickLoopreview(loopreview.VerdictFail, "regression found"), nil
	}
	recoverCalled := false
	opts.Recover = func(_ context.Context, opts recovery.Options) (recovery.Result, error) {
		recoverCalled = true
		if opts.IssueNumber != 12 || !opts.SkipAdoptPR || !strings.Contains(opts.FailureContext, "regression found") {
			t.Fatalf("recover opts = %#v", opts)
		}
		review := tickLoopreview(loopreview.VerdictPass, "recovered review passed")
		return recovery.Result{
			Action: recovery.ActionSucceeded,
			DispatchResult: &recovery.DispatchResult{
				OK:     true,
				Issue:  12,
				Branch: "loop/issue-12-retry-2",
				RunID:  opts.RunID,
				PR:     "https://github.com/owner/repo/pull/121",
				Status: "succeeded",
			},
			ReviewResult: &review,
			RecoveryAttempts: []recovery.AttemptRecord{{
				Version:  recovery.AttemptRecordVersion,
				Issue:    12,
				RunID:    opts.RunID,
				Attempt:  2,
				Strategy: recovery.AttemptStrategySameConfig,
				Status:   "succeeded",
				PR:       "https://github.com/owner/repo/pull/121",
			}},
		}, nil
	}
	mergedPR := 0
	opts.PreProdWriter = tickPreProdWriterFunc(func(_ context.Context, prNumber int, branch string) (gh.PreProdMergeResult, error) {
		mergedPR = prNumber
		return gh.PreProdMergeResult{PRNumber: prNumber, Branch: branch, Head: "loop/issue-12-retry-2", SHA: "abc123"}, nil
	})

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusSucceeded || report.StopReason != TickStopCompleted {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if !recoverCalled || loopreviewCalls != 1 {
		t.Fatalf("recoverCalled=%v loopreviewCalls=%d", recoverCalled, loopreviewCalls)
	}
	if mergedPR != 121 {
		t.Fatalf("merged PR = %d, want 121", mergedPR)
	}
	if len(report.Recoveries) != 1 || report.Recoveries[0].Action != string(recovery.ActionSucceeded) {
		t.Fatalf("recoveries = %#v", report.Recoveries)
	}
	if len(report.Reviews) != 2 || report.Reviews[0].Verdict != loopreview.VerdictFail || report.Reviews[1].Verdict != loopreview.VerdictPass || report.Reviews[1].PRNumber != 121 {
		t.Fatalf("reviews = %#v", report.Reviews)
	}
	if len(report.NeedsHuman) != 0 || len(report.Failures) != 0 {
		t.Fatalf("needs-human=%#v failures=%#v", report.NeedsHuman, report.Failures)
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

func TestTickLeavesPendingPreProdCIForLaterTick(t *testing.T) {
	tests := []struct {
		name   string
		checks []gh.Check
	}{
		{name: "no check runs yet", checks: nil},
		{name: "pending check run", checks: []gh.Check{{Name: "verify", Bucket: "pending"}}},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prNumber := 240 + i
			opts := reviewReadyTickOptions(t.TempDir(), 24+i, "https://github.com/owner/repo/pull/"+strconv.Itoa(prNumber))
			opts.Reader = fakeReader{
				checks:    map[int][]gh.Check{prNumber: passChecks()},
				diffFiles: map[int][]string{prNumber: {"README.md"}},
				diffs:     map[int]string{prNumber: modifiedDiff("README.md")},
				branchChecks: map[string]gh.BranchChecksResult{
					"pre-prod": {
						Branch:  "pre-prod",
						HeadSHA: "merge-sha",
						Checks:  tt.checks,
					},
				},
			}
			writer := &recordingPreProdWriter{
				mergeResult:  gh.PreProdMergeResult{PRNumber: prNumber, Branch: "pre-prod", Head: "loop/issue-24", SHA: "merge-sha"},
				revertResult: gh.PreProdRevertResult{PRNumber: prNumber, Branch: "pre-prod", RevertedSHA: "merge-sha", SHA: "revert-sha"},
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
			if len(report.PreProdHealth) != 1 || report.PreProdHealth[0].Status != PreProdHealthStatusPending {
				t.Fatalf("pre-prod health = %#v", report.PreProdHealth)
			}
			if len(report.NeedsHuman) != 0 || len(report.Failures) != 0 {
				t.Fatalf("needs-human=%#v failures=%#v", report.NeedsHuman, report.Failures)
			}
			if len(report.PreProdReverts) != 0 {
				t.Fatalf("pre-prod reverts = %#v", report.PreProdReverts)
			}
		})
	}
}

func TestTickDoesNotRevertWhenPreProdRedIsNotAtMergeSHA(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 25, "https://github.com/owner/repo/pull/250")
	opts.Reader = fakeReader{
		checks:    map[int][]gh.Check{250: passChecks()},
		diffFiles: map[int][]string{250: {"README.md"}},
		diffs:     map[int]string{250: modifiedDiff("README.md")},
		branchChecks: map[string]gh.BranchChecksResult{
			"pre-prod": {
				Branch:  "pre-prod",
				HeadSHA: "newer-preprod-head",
				Checks:  []gh.Check{{Name: "verify", Bucket: "fail"}},
			},
		},
	}
	writer := &recordingPreProdWriter{
		mergeResult:  gh.PreProdMergeResult{PRNumber: 250, Branch: "pre-prod", Head: "loop/issue-25", SHA: "merge-sha"},
		revertResult: gh.PreProdRevertResult{PRNumber: 250, Branch: "pre-prod", RevertedSHA: "merge-sha", SHA: "revert-sha"},
	}
	opts.PreProdWriter = writer

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopPreProdNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if writer.mergeCalls != 1 || writer.revertCalls != 0 {
		t.Fatalf("writer calls merge=%d revert=%d", writer.mergeCalls, writer.revertCalls)
	}
	if len(report.PreProdHealth) != 1 || report.PreProdHealth[0].Status != PreProdHealthStatusRed || report.PreProdHealth[0].HeadSHA != "newer-preprod-head" {
		t.Fatalf("pre-prod health = %#v", report.PreProdHealth)
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "pre-prod-health" {
		t.Fatalf("needs-human=%#v failures=%#v", report.NeedsHuman, report.Failures)
	}
	if len(report.Failures) != 0 {
		t.Fatalf("failures = %#v", report.Failures)
	}
}

func TestTickPreProdRevertFailureIsReportedAsFailure(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 26, "https://github.com/owner/repo/pull/260")
	opts.Reader = fakeReader{
		checks:    map[int][]gh.Check{260: passChecks()},
		diffFiles: map[int][]string{260: {"README.md"}},
		diffs:     map[int]string{260: modifiedDiff("README.md")},
		branchChecks: map[string]gh.BranchChecksResult{
			"pre-prod": {
				Branch:  "pre-prod",
				HeadSHA: "merge-sha",
				Checks:  []gh.Check{{Name: "verify", Bucket: "fail"}},
			},
		},
	}
	writer := &recordingPreProdWriter{
		mergeResult: gh.PreProdMergeResult{PRNumber: 260, Branch: "pre-prod", Head: "loop/issue-26", SHA: "merge-sha"},
		revertErr:   errors.New("revert failed"),
	}
	opts.PreProdWriter = writer

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusFailed || report.StopReason != TickStopReviewFailed {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if writer.mergeCalls != 1 || writer.revertCalls != 1 {
		t.Fatalf("writer calls merge=%d revert=%d", writer.mergeCalls, writer.revertCalls)
	}
	if len(report.PreProdReverts) != 1 || report.PreProdReverts[0].Status != TickStatusFailed || report.PreProdReverts[0].Error != "revert failed" {
		t.Fatalf("pre-prod reverts = %#v", report.PreProdReverts)
	}
	if len(report.Failures) != 1 || report.Failures[0].Step != "pre-prod-revert" {
		t.Fatalf("failures = %#v", report.Failures)
	}
	if len(report.NeedsHuman) != 0 {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
	}
}

func TestTickNeedsHumanWhenReaderCannotReadPreProdBranchChecks(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 27, "https://github.com/owner/repo/pull/270")
	opts.Reader = tickReaderWithoutBranchChecks{fake: cleanRiskReader(270, "README.md")}
	writer := &recordingPreProdWriter{
		mergeResult:  gh.PreProdMergeResult{PRNumber: 270, Branch: "pre-prod", Head: "loop/issue-27", SHA: "merge-sha"},
		revertResult: gh.PreProdRevertResult{PRNumber: 270, Branch: "pre-prod", RevertedSHA: "merge-sha", SHA: "revert-sha"},
	}
	opts.PreProdWriter = writer

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopPreProdNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if writer.mergeCalls != 1 || writer.revertCalls != 0 {
		t.Fatalf("writer calls merge=%d revert=%d", writer.mergeCalls, writer.revertCalls)
	}
	if len(report.PreProdHealth) != 1 || report.PreProdHealth[0].Status != PreProdHealthStatusUnknown {
		t.Fatalf("pre-prod health = %#v", report.PreProdHealth)
	}
	if len(report.NeedsHuman) != 1 || report.NeedsHuman[0].Step != "pre-prod-health" {
		t.Fatalf("needs-human = %#v", report.NeedsHuman)
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

func appendPendingPromotionEvent(t *testing.T, repo, runID, timestamp string, pending ...TickPendingPromotion) {
	t.Helper()
	details, err := json.Marshal(tickPendingPromotionLedgerDetails{
		Version:          1,
		Repo:             "owner/repo",
		RepoPath:         repo,
		RunID:            runID,
		PreProdBranch:    "pre-prod",
		PendingPromotion: pending,
	})
	if err != nil {
		t.Fatalf("Marshal pending-promotion details: %v", err)
	}
	if err := state.AppendEvent(repo, runID, state.Event{
		Timestamp: timestamp,
		RunID:     runID,
		JobID:     "tick",
		Phase:     "report",
		Status:    tickStatusPendingPromotion,
		LogBytes:  0,
		Event:     tickPendingPromotionEvent,
		Outcome:   tickStatusPendingPromotion,
		Details:   json.RawMessage(details),
	}); err != nil {
		t.Fatalf("AppendEvent pending promotion: %v", err)
	}
}

func appendPromoteAttemptEvent(t *testing.T, repo, runID, timestamp string, report PromoteReport) {
	t.Helper()
	report.RunID = runID
	report.FinishedAt = timestamp
	reportJSON, err := json.Marshal(normalizePromoteReport(report))
	if err != nil {
		t.Fatalf("Marshal promote report: %v", err)
	}
	if err := state.AppendEvent(repo, runID, state.Event{
		Timestamp: timestamp,
		RunID:     runID,
		JobID:     "promote",
		Phase:     "promote",
		Status:    promoteLedgerOutcome(report),
		LogBytes:  0,
		Event:     promoteLedgerEvent,
		Outcome:   promoteLedgerOutcome(report),
		Details:   json.RawMessage(reportJSON),
	}); err != nil {
		t.Fatalf("AppendEvent promote attempt: %v", err)
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

type tickReaderWithoutBranchChecks struct {
	fake fakeReader
}

func (r tickReaderWithoutBranchChecks) RepoName(ctx context.Context) (string, error) {
	return r.fake.RepoName(ctx)
}

func (r tickReaderWithoutBranchChecks) ListIssues(ctx context.Context, state string) ([]gh.Issue, error) {
	return r.fake.ListIssues(ctx, state)
}

func (r tickReaderWithoutBranchChecks) ViewIssue(ctx context.Context, number int) (gh.Issue, error) {
	return r.fake.ViewIssue(ctx, number)
}

func (r tickReaderWithoutBranchChecks) ListOpenPRs(ctx context.Context) ([]gh.PullRequest, error) {
	return r.fake.ListOpenPRs(ctx)
}

func (r tickReaderWithoutBranchChecks) PRChecks(ctx context.Context, number int) ([]gh.Check, error) {
	return r.fake.PRChecks(ctx, number)
}

func (r tickReaderWithoutBranchChecks) PRDiff(ctx context.Context, number int) (string, error) {
	return r.fake.PRDiff(ctx, number)
}

func (r tickReaderWithoutBranchChecks) PRDiffNameOnly(ctx context.Context, number int) ([]string, error) {
	return r.fake.PRDiffNameOnly(ctx, number)
}
