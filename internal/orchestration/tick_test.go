package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/orchestrationcost"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	"github.com/jasonhnd/loopcoder/internal/storage"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/waitstate"
)

func TestTickHappyPass(t *testing.T) {
	repo := t.TempDir()
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	var order []string
	progressRecorder := &recordingProgressRecorder{}

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
		Progress:         progressRecorder,
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
	if !progressRecorder.hasKnown(progress.KnownWaitingCI) {
		t.Fatalf("tick did not emit waiting-for-ci progress observation: %#v", progressRecorder.observations)
	}
	if !progressRecorder.hasTerminal("risk-gate", RiskGateStatusClean) || !progressRecorder.hasTerminal("pre-prod-health", PreProdHealthStatusGreen) {
		t.Fatalf("tick did not terminalize CI progress observations: %#v", progressRecorder.observations)
	}
	for _, observation := range progressRecorder.observations {
		if !observation.OccurredAt.Equal(now) {
			t.Fatalf("progress observation occurred_at = %s, want injected clock %s", observation.OccurredAt, now)
		}
	}
	if report.Summary.DispatchedPRCount != 1 || report.Summary.ReviewPassCount != 1 || report.Summary.RiskGateCleanCount != 1 || report.Summary.PreProdMergeCount != 1 || report.Summary.FailureCount != 0 {
		t.Fatalf("summary = %#v", report.Summary)
	}
	if report.StatePush == nil || !report.StatePush.Pushed {
		t.Fatalf("state push = %#v, want pushed", report.StatePush)
	}
}

func TestTickCostBudgetPreservesCompletedWorkerAndBlocksVerifier(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 101, "https://github.com/owner/repo/pull/77")
	opts.CostPolicy = orchestrationcost.Policy{MaxModelCalls: 1, MaxTokens: 10_000, MaxOverheadPercent: 10}
	workerTokens := int64(100)
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		return tickWaveReport(DispatchWaveIssueResult{Issue: 101, Status: DispatchWaveStatusSucceeded, ProviderInvoked: true, PR: "https://github.com/owner/repo/pull/77", Report: tickProviderReport(workerTokens)}), nil
	}
	verifierCalls := 0
	opts.Loopreview = func(_ context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
		if err := reviewOpts.BeforeProviderCall(); err != nil {
			return loopreview.Result{}, err
		}
		verifierCalls++
		return tickLoopreview(loopreview.VerdictPass, "unexpected"), nil
	}

	firstReport, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if firstReport.Status != TickStatusNeedsHuman || firstReport.StopReason != TickStopOrchestrationCostBudget || verifierCalls != 0 {
		t.Fatalf("status=%s stop=%s verifier_calls=%d", firstReport.Status, firstReport.StopReason, verifierCalls)
	}
	if firstReport.DispatchWave == nil || len(firstReport.DispatchWave.Results) != 1 || firstReport.DispatchWave.Results[0].PR == "" {
		t.Fatalf("completed worker result was not preserved: %#v", firstReport.DispatchWave)
	}
	if firstReport.OrchestrationCost.Totals.ModelCalls != 1 || firstReport.OrchestrationCost.Totals.Tokens == nil || *firstReport.OrchestrationCost.Totals.Tokens != workerTokens {
		t.Fatalf("cost = %#v", firstReport.OrchestrationCost)
	}
	if len(firstReport.OrchestrationCost.BudgetDecisions) == 0 || firstReport.OrchestrationCost.BudgetDecisions[len(firstReport.OrchestrationCost.BudgetDecisions)-1].PRNumber != 77 {
		t.Fatalf("budget decisions = %#v", firstReport.OrchestrationCost.BudgetDecisions)
	}

	opts.CostPolicy = orchestrationcost.Policy{MaxModelCalls: 2, MaxTokens: 10_000, MaxOverheadPercent: 10}
	opts.ComputeReadySet = func(context.Context, Options) (report.ReadySetReport, error) {
		return report.ReadySetReport{Blocked: []report.BlockedIssue{{
			Issue:          101,
			Classification: "has-open-PR",
			OpenPRs:        []report.OpenPRSummary{{Number: 77, URL: "https://github.com/owner/repo/pull/77"}},
		}}}, nil
	}
	dispatchCalls, mergeCalls := 0, 0
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		dispatchCalls++
		return DispatchWaveReport{}, nil
	}
	opts.Loopreview = func(_ context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
		if err := reviewOpts.BeforeProviderCall(); err != nil {
			return loopreview.Result{}, err
		}
		verifierCalls++
		result := tickLoopreview(loopreview.VerdictPass, "review passed")
		result.ProviderInvoked = true
		result.Verdict.Report = tickProviderReport(0)
		return result, nil
	}
	opts.PreProdWriter = tickPreProdWriterFunc(func(_ context.Context, prNumber int, branch string) (gh.PreProdMergeResult, error) {
		mergeCalls++
		return gh.PreProdMergeResult{PRNumber: prNumber, Branch: branch, Head: "loop/issue-101", SHA: "abc123"}, nil
	})

	resumed, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("resumed Tick: %v", err)
	}
	if resumed.Status != TickStatusSucceeded || dispatchCalls != 0 || verifierCalls != 1 || mergeCalls != 1 {
		t.Fatalf("status=%s dispatch_calls=%d verifier_calls=%d merge_calls=%d failures=%#v needs_human=%#v", resumed.Status, dispatchCalls, verifierCalls, mergeCalls, resumed.Failures, resumed.NeedsHuman)
	}
	if !resumed.OrchestrationCost.BudgetDecisions[len(resumed.OrchestrationCost.BudgetDecisions)-2].Consumed {
		t.Fatalf("budget decisions = %#v", resumed.OrchestrationCost.BudgetDecisions)
	}
}

func TestTickDispatchAdmitsCallsUntilPerRunCallBudget(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 201, "https://github.com/owner/repo/pull/201")
	opts.CostPolicy = orchestrationcost.Policy{MaxModelCalls: 2, MaxTokens: 10_000, MaxOverheadPercent: 10}
	opts.ComputeReadySet = func(context.Context, Options) (report.ReadySetReport, error) {
		return report.ReadySetReport{Ready: []report.ReadyIssue{{Issue: 201}, {Issue: 202}, {Issue: 203}}}, nil
	}
	providerCalls := 0
	opts.DispatchWave = func(_ context.Context, waveOpts DispatchWaveOptions) (DispatchWaveReport, error) {
		results := make([]DispatchWaveIssueResult, 0, 3)
		for _, issue := range []int{201, 202, 203} {
			if err := waveOpts.BeforeProviderCall(issue); err != nil {
				results = append(results, DispatchWaveIssueResult{Issue: issue, Status: DispatchWaveStatusNeedsHuman, Error: err.Error()})
				break
			}
			providerCalls++
			result := DispatchWaveIssueResult{Issue: issue, Status: DispatchWaveStatusSucceeded, ProviderInvoked: true, Report: tickProviderReport(10)}
			results = append(results, result)
			if err := waveOpts.OnIssueComplete(DispatchWaveIssueComplete{RunID: waveOpts.RunID, Result: result}); err != nil {
				return DispatchWaveReport{}, err
			}
		}
		return tickWaveReport(results...), nil
	}

	result, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if providerCalls != 2 || result.Status != TickStatusNeedsHuman || result.StopReason != TickStopOrchestrationCostBudget {
		t.Fatalf("provider_calls=%d status=%s stop=%s", providerCalls, result.Status, result.StopReason)
	}
	if result.OrchestrationCost.Totals.ModelCalls != 2 {
		t.Fatalf("cost = %#v", result.OrchestrationCost)
	}
}

func TestPendingVerifierMarkersPreserveEveryCompletedWavePR(t *testing.T) {
	tokens := int64(10)
	cost, err := orchestrationcost.Build("run-pending-wave", orchestrationcost.Policy{MaxModelCalls: 1, MaxTokens: 100, MaxOverheadPercent: 10}, []orchestrationcost.Event{
		orchestrationcost.EventFromReport("worker:issue-1", orchestrationcost.RoleWorker, true, tickProviderReport(tokens)),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cost = orchestrationcost.ApplyBudgetDecision(cost, orchestrationcost.CheckBeforeModelCall(cost, 1))
	tickReport := TickReport{OrchestrationCost: cost}
	markPendingVerifierCandidates(&tickReport, []DispatchWaveIssueResult{
		{Issue: 1, Status: DispatchWaveStatusSucceeded, PR: "https://github.com/owner/repo/pull/77"},
		{Issue: 2, Status: DispatchWaveStatusSucceeded, PR: "https://github.com/owner/repo/pull/78"},
		{Issue: 3, Status: DispatchWaveStatusNeedsHuman},
	})

	ready := report.ReadySetReport{Blocked: []report.BlockedIssue{
		{Issue: 1, OpenPRs: []report.OpenPRSummary{{Number: 77, URL: "https://github.com/owner/repo/pull/77"}}},
		{Issue: 2, OpenPRs: []report.OpenPRSummary{{Number: 78, URL: "https://github.com/owner/repo/pull/78"}}},
	}}
	first, ok := restoredVerificationCandidate(ready, tickReport.OrchestrationCost)
	if !ok || first.Issue != 2 {
		t.Fatalf("first candidate = %#v, ok=%v decisions=%#v", first, ok, tickReport.OrchestrationCost.BudgetDecisions)
	}
	reblocked := orchestrationcost.BindDecisionToPR(orchestrationcost.CheckBeforeModelCall(tickReport.OrchestrationCost, 1), 78)
	if reblocked.Allowed {
		t.Fatalf("reblocked decision unexpectedly allowed: %#v", reblocked)
	}
	tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(tickReport.OrchestrationCost, reblocked)
	tickReport.OrchestrationCost = orchestrationcost.MarkBudgetDecisionConsumed(tickReport.OrchestrationCost, 78)
	for _, decision := range tickReport.OrchestrationCost.BudgetDecisions {
		if !decision.Allowed && decision.PRNumber == 78 && !decision.Consumed {
			t.Fatalf("PR #78 retained an unconsumed retry marker: %#v", tickReport.OrchestrationCost.BudgetDecisions)
		}
	}
	second, ok := restoredVerificationCandidate(ready, tickReport.OrchestrationCost)
	if !ok || second.Issue != 1 {
		t.Fatalf("second candidate = %#v, ok=%v decisions=%#v", second, ok, tickReport.OrchestrationCost.BudgetDecisions)
	}
}

func TestTickPersistsPendingVerifierBindingsBeforeStatePush(t *testing.T) {
	repo := t.TempDir()
	opts := reviewReadyTickOptions(repo, 301, "https://github.com/owner/repo/pull/91")
	opts.CostPolicy = orchestrationcost.DefaultPolicy()
	opts.CostPolicy.MaxModelCalls = 1
	opts.ComputeReadySet = func(context.Context, Options) (report.ReadySetReport, error) {
		return report.ReadySetReport{Ready: []report.ReadyIssue{{Issue: 301}, {Issue: 302}}}, nil
	}
	opts.DispatchWave = func(_ context.Context, waveOpts DispatchWaveOptions) (DispatchWaveReport, error) {
		if err := waveOpts.BeforeProviderCall(301); err != nil {
			return DispatchWaveReport{}, err
		}
		first := DispatchWaveIssueResult{
			Issue: 301, Status: DispatchWaveStatusSucceeded, ProviderInvoked: true,
			PR: "https://github.com/owner/repo/pull/91", Report: tickProviderReport(10),
		}
		if err := waveOpts.OnIssueComplete(DispatchWaveIssueComplete{RunID: waveOpts.RunID, Result: first}); err != nil {
			return DispatchWaveReport{}, err
		}
		second := DispatchWaveIssueResult{Issue: 302, Status: DispatchWaveStatusNeedsHuman}
		if err := waveOpts.BeforeProviderCall(302); err != nil {
			second.Error = err.Error()
		}
		return tickWaveReport(first, second), nil
	}
	opts.StatePush = func(context.Context, statebranch.PushOptions) (statebranch.PushResult, error) {
		persisted, found, err := orchestrationcost.Load(repo, opts.RunID)
		if err != nil {
			t.Fatalf("Load before state push: %v", err)
		}
		if !found {
			t.Fatal("orchestration cost ledger was not persisted before state push")
		}
		for _, decision := range persisted.BudgetDecisions {
			if !decision.Allowed && !decision.Consumed && decision.PRNumber == 91 {
				return statebranch.PushResult{Pushed: true}, nil
			}
		}
		t.Fatalf("persisted ledger has no pending verifier binding for PR #91: %#v", persisted.BudgetDecisions)
		return statebranch.PushResult{}, nil
	}

	result, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Status != TickStatusNeedsHuman || result.StopReason != TickStopOrchestrationCostBudget {
		t.Fatalf("status=%s stop=%s", result.Status, result.StopReason)
	}
}

func TestTickPersistsVerifierReservationBeforeProviderReturns(t *testing.T) {
	repo := t.TempDir()
	opts := reviewReadyTickOptions(repo, 204, "https://github.com/owner/repo/pull/204")
	opts.Loopreview = func(_ context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
		if err := reviewOpts.BeforeProviderCall(); err != nil {
			return loopreview.Result{}, err
		}
		persisted, found, err := orchestrationcost.Load(repo, opts.RunID)
		if err != nil {
			t.Fatalf("Load reservation: %v", err)
		}
		if !found || persisted.Totals.ModelCalls != 1 || persisted.Totals.UsageState != orchestrationcost.UsageUnknown {
			t.Fatalf("persisted reservation = %#v, found=%v", persisted, found)
		}
		result := tickLoopreview(loopreview.VerdictPass, "review passed")
		result.ProviderInvoked = true
		result.Verdict.Report = tickProviderReport(10)
		return result, nil
	}

	result, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Status != TickStatusSucceeded || result.OrchestrationCost.Totals.ModelCalls != 1 || result.OrchestrationCost.Totals.UsageState != orchestrationcost.UsageExact {
		t.Fatalf("result = %#v", result)
	}
}

func TestCostedVerifierAtomicallyPersistsCompletionAndConsumesEveryPRMarker(t *testing.T) {
	repo := t.TempDir()
	cost, err := orchestrationcost.Build("run-verifier-atomic", orchestrationcost.DefaultPolicy(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var historical []orchestrationcost.Decision
	for range 2 {
		decision := orchestrationcost.BindDecisionToPR(orchestrationcost.Decision{
			Status: orchestrationcost.StatusNeedsHuman, Allowed: false, Reason: "model call budget exhausted",
			Observed: "1", Limit: "1", Evidence: []string{"test refusal"}, Remediation: "raise the budget",
		}, 205)
		historical = append(historical, decision)
	}
	cost = orchestrationcost.RestoreDecisionState(cost, historical, nil)
	if err := orchestrationcost.Write(repo, cost); err != nil {
		t.Fatalf("Write initial ledger: %v", err)
	}
	report := TickReport{RepoPath: repo, OrchestrationCost: cost}
	opts := TickOptions{
		RepoPath: repo,
		Loopreview: func(_ context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
			if err := reviewOpts.BeforeProviderCall(); err != nil {
				return loopreview.Result{}, err
			}
			result := tickLoopreview(loopreview.VerdictPass, "pass")
			result.ProviderInvoked = true
			result.Verdict.Report = tickProviderReport(10)
			return result, nil
		},
	}
	if _, err := runTickCostedLoopreview(context.Background(), opts, &report, 205, "verifier:pr-205", orchestrationcost.RoleVerifier, true, "pr=205"); err != nil {
		t.Fatalf("runTickCostedLoopreview: %v", err)
	}
	persisted, found, err := orchestrationcost.Load(repo, cost.RunID)
	if err != nil || !found {
		t.Fatalf("Load completed ledger: found=%v err=%v", found, err)
	}
	if persisted.Totals.ModelCalls != 1 || persisted.Totals.Tokens == nil || *persisted.Totals.Tokens != 10 {
		t.Fatalf("persisted completed cost = %#v", persisted.Totals)
	}
	for _, decision := range persisted.BudgetDecisions {
		if !decision.Allowed && decision.PRNumber == 205 && !decision.Consumed {
			t.Fatalf("persisted stale PR marker: %#v", persisted.BudgetDecisions)
		}
	}
}

func TestCostedVerifierRemovesReservationWhenReportExistsWithoutLaunch(t *testing.T) {
	repo := t.TempDir()
	cost, err := orchestrationcost.Build("run-verifier-no-launch", orchestrationcost.DefaultPolicy(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	report := TickReport{RepoPath: repo, OrchestrationCost: cost}
	opts := TickOptions{
		RepoPath: repo,
		Loopreview: func(_ context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
			if err := reviewOpts.BeforeProviderCall(); err != nil {
				return loopreview.Result{}, err
			}
			return loopreview.Result{
				ProviderInvoked: false,
				Verdict: loopreview.Verdict{
					Verdict: loopreview.VerdictNeedsHuman,
					Report:  tickProviderReportUnknown(),
				},
			}, nil
		},
	}
	if _, err := runTickCostedLoopreview(context.Background(), opts, &report, 205, "verifier:pr-205", orchestrationcost.RoleVerifier, true); err != nil {
		t.Fatalf("runTickCostedLoopreview: %v", err)
	}
	if report.OrchestrationCost.Totals.ModelCalls != 0 || len(report.OrchestrationCost.Events) != 0 {
		t.Fatalf("cost = %#v, want confirmed no-launch reservation removed", report.OrchestrationCost)
	}
}

func TestCostedProviderOccurrencesNeverOverwritePriorCalls(t *testing.T) {
	repo := t.TempDir()
	cost, err := orchestrationcost.Build("run-repeat", orchestrationcost.DefaultPolicy(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	report := TickReport{RepoPath: repo, OrchestrationCost: cost}
	opts := TickOptions{
		RepoPath: repo,
		Loopreview: func(_ context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
			if err := reviewOpts.BeforeProviderCall(); err != nil {
				return loopreview.Result{}, err
			}
			result := tickLoopreview(loopreview.VerdictPass, "pass")
			result.ProviderInvoked = true
			result.Verdict.Report = tickProviderReport(10)
			return result, nil
		},
	}
	for range 2 {
		if _, err := runTickCostedLoopreview(context.Background(), opts, &report, 205, "verifier:pr-205", orchestrationcost.RoleVerifier, true, "pr=205"); err != nil {
			t.Fatalf("runTickCostedLoopreview: %v", err)
		}
	}
	workerReservations := map[int]string{}
	for range 2 {
		if err := recordDispatchWaveResultCost(&report, DispatchWaveIssueResult{Issue: 206, ProviderInvoked: true, Status: DispatchWaveStatusSucceeded, Report: tickProviderReport(5)}, workerReservations); err != nil {
			t.Fatalf("recordDispatchWaveResultCost: %v", err)
		}
	}
	if report.OrchestrationCost.Totals.ModelCalls != 4 || report.OrchestrationCost.Totals.Tokens == nil || *report.OrchestrationCost.Totals.Tokens != 30 || len(report.OrchestrationCost.Events) != 4 {
		t.Fatalf("cost = %#v", report.OrchestrationCost)
	}
}

func TestRecordDispatchWaveCostClassifiesProviderFreeDeliveryRetry(t *testing.T) {
	repo := t.TempDir()
	cost, err := orchestrationcost.Build("run-delivery-only", orchestrationcost.DefaultPolicy(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	report := TickReport{RepoPath: repo, OrchestrationCost: cost}
	err = recordDispatchWaveResultCost(&report, DispatchWaveIssueResult{
		Issue:           207,
		Status:          DispatchWaveStatusSucceeded,
		Outcome:         "pr_adopted",
		ProviderOutcome: "provider_completed",
		DeliveryOutcome: "pr_adopted",
		Evidence:        []string{"reused prior provider work"},
	}, map[int]string{})
	if err != nil {
		t.Fatalf("recordDispatchWaveResultCost: %v", err)
	}
	if report.OrchestrationCost.Totals.ModelCalls != 0 || report.OrchestrationCost.Totals.DeliveryOnlyRetries != 1 || report.OrchestrationCost.Totals.Retries != 1 {
		t.Fatalf("cost = %#v", report.OrchestrationCost)
	}
}

func TestRecordRecoveryCostCountsSameConfigDuplicateRetries(t *testing.T) {
	cost, err := orchestrationcost.Build("run-duplicate-retries", orchestrationcost.DefaultPolicy(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	report := TickReport{RepoPath: t.TempDir(), OrchestrationCost: cost}
	recordRecoveryCost(&report, 209, recovery.Result{RecoveryAttempts: []recovery.AttemptRecord{
		{Attempt: 2, Strategy: recovery.AttemptStrategySameConfig},
		{Attempt: 3, Strategy: recovery.AttemptStrategyUpgradedConfig},
	}}, false)
	if report.OrchestrationCost.Totals.Retries != 2 || report.OrchestrationCost.Totals.DuplicateRetries != 1 {
		t.Fatalf("cost = %#v", report.OrchestrationCost)
	}
}

func TestRecoveryBudgetRefusalRemainsActiveAfterRetryAccounting(t *testing.T) {
	tokens := int64(10)
	policy := orchestrationcost.Policy{MaxModelCalls: 1, MaxTokens: 100, MaxOverheadPercent: 10}
	cost, err := orchestrationcost.Build("run-recovery-refusal", policy, []orchestrationcost.Event{
		orchestrationcost.EventFromReport("worker:completed", orchestrationcost.RoleWorker, true, tickProviderReport(tokens)),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	report := TickReport{RepoPath: t.TempDir(), RunID: "run-recovery-refusal", OrchestrationCost: cost}
	opts := TickOptions{
		RepoPath: report.RepoPath,
		RunID:    report.RunID,
		Clock:    func() time.Time { return time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC) },
		Recover: func(_ context.Context, recoverOpts recovery.Options) (recovery.Result, error) {
			if err := recoverOpts.BeforeProviderCall("worker"); err == nil {
				t.Fatal("recovery provider call was allowed at the call cap")
			}
			return recovery.Result{Action: recovery.ActionBlocked, Report: "BLOCKED: orchestration cost budget"}, nil
		},
	}
	runTickRecoverFailure(context.Background(), opts, &report, tickRecoveryRequest{IssueNumber: 210, TriggerStep: "dispatch-wave"})
	if report.OrchestrationCost.Status != orchestrationcost.StatusNeedsHuman || report.OrchestrationCost.Reason != "model-call-budget" {
		t.Fatalf("cost = %#v, want active model-call-budget refusal", report.OrchestrationCost)
	}
}

func TestRiskGateWaitCostPersistenceFailureBlocksPreProdMerge(t *testing.T) {
	repo := t.TempDir()
	runID := "run-risk-cost-failure"
	cost, err := orchestrationcost.Build(runID, orchestrationcost.DefaultPolicy(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	runPath := state.RunPath(repo, runID)
	if err := os.MkdirAll(filepath.Dir(runPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(runPath, []byte("blocks-ledger-directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	mergeCalls := 0
	tickReport := TickReport{RepoPath: repo, RunID: runID, OrchestrationCost: cost}
	runTickRiskGateAndPreProdMerge(context.Background(), TickOptions{
		Reader:        cleanRiskReader(88, "README.md"),
		RepoPath:      repo,
		RunID:         runID,
		BaseBranch:    "main",
		PreProdBranch: "pre-prod",
		Clock:         func() time.Time { return time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC) },
		RiskGate: func(context.Context, RiskGateOptions) (RiskGateDecision, error) {
			return RiskGateDecision{
				PRNumber: 88,
				Status:   RiskGateStatusClean,
				Wait:     &waitstate.Report{Polls: 2, DurationMS: 10},
			}, nil
		},
		PreProdWriter: tickPreProdWriterFunc(func(context.Context, int, string) (gh.PreProdMergeResult, error) {
			mergeCalls++
			return gh.PreProdMergeResult{}, nil
		}),
	}, &tickReport, tickReviewCandidate{Issue: 208, PR: "https://github.com/owner/repo/pull/88"}, 88)
	if mergeCalls != 0 || len(tickReport.Failures) == 0 || len(tickReport.RiskGates) != 1 || tickReport.RiskGates[0].Status != RiskGateStatusNeedsHuman {
		t.Fatalf("merge_calls=%d failures=%#v risk_gates=%#v", mergeCalls, tickReport.Failures, tickReport.RiskGates)
	}
}

func TestRecordTickCostEventAssignsStableOccurrenceIDs(t *testing.T) {
	cost, err := orchestrationcost.Build("run-occurrences", orchestrationcost.DefaultPolicy(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	report := TickReport{RepoPath: t.TempDir(), OrchestrationCost: cost}
	event := orchestrationcost.DeterministicEvent("waiting:ci:pr-77", orchestrationcost.RoleWaiting, orchestrationcost.ActivityCIPoll, "pending")
	recordTickCostEvent(&report, event)
	recordTickCostEvent(&report, event)
	if len(report.OrchestrationCost.Events) != 2 || report.OrchestrationCost.Events[0].EventID != "waiting:ci:pr-77" || report.OrchestrationCost.Events[1].EventID != "waiting:ci:pr-77:2" {
		t.Fatalf("events = %#v", report.OrchestrationCost.Events)
	}
	if report.OrchestrationCost.Totals.Waits != 2 || report.OrchestrationCost.Totals.ModelCalls != 0 {
		t.Fatalf("totals = %#v", report.OrchestrationCost.Totals)
	}
}

func TestTickTerminalUnknownProviderUsageAllowsVerifierButBlocksRelease(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 102, "https://github.com/owner/repo/pull/78")
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		return tickWaveReport(DispatchWaveIssueResult{Issue: 102, Status: DispatchWaveStatusSucceeded, ProviderInvoked: true, PR: "https://github.com/owner/repo/pull/78", Report: tickProviderReportUnknown()}), nil
	}
	verifierCalls := 0
	opts.Loopreview = func(_ context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
		if err := reviewOpts.BeforeProviderCall(); err != nil {
			return loopreview.Result{}, err
		}
		verifierCalls++
		result := tickLoopreview(loopreview.VerdictPass, "review passed")
		result.ProviderInvoked = true
		result.Verdict.Report = tickProviderReport(10)
		return result, nil
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if verifierCalls != 1 || report.Status != TickStatusNeedsHuman || report.OrchestrationCost.Totals.Tokens != nil || report.OrchestrationCost.Totals.UsageState != orchestrationcost.UsageUnknown {
		t.Fatalf("verifier_calls=%d cost=%#v", verifierCalls, report.OrchestrationCost)
	}
	if report.OrchestrationCost.ReleaseGate == nil || report.OrchestrationCost.ReleaseGate.Allowed || report.OrchestrationCost.ReleaseGate.Reason != "token-budget-unknown" {
		t.Fatalf("release gate = %#v", report.OrchestrationCost.ReleaseGate)
	}
	if report.OrchestrationCost.ExternalHostUsage.State != orchestrationcost.UsageUnknown || report.OrchestrationCost.ExternalHostUsage.Tokens != nil {
		t.Fatalf("external host usage = %#v", report.OrchestrationCost.ExternalHostUsage)
	}
}

func TestTickReloadsPerRunCostLedgerBeforeAnotherProvider(t *testing.T) {
	repo := t.TempDir()
	opts := reviewReadyTickOptions(repo, 104, "https://github.com/owner/repo/pull/80")
	opts.CostPolicy = orchestrationcost.Policy{MaxModelCalls: 1, MaxTokens: 500_000, MaxOverheadPercent: 10}
	persisted, err := orchestrationcost.Build(opts.RunID, orchestrationcost.DefaultPolicy(), []orchestrationcost.Event{
		orchestrationcost.EventFromReport("worker:prior", orchestrationcost.RoleWorker, true, tickProviderReportUnknown(), "prior tick"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := orchestrationcost.Write(repo, persisted); err != nil {
		t.Fatalf("Write: %v", err)
	}
	dispatchCalls := 0
	opts.DispatchWave = func(_ context.Context, waveOpts DispatchWaveOptions) (DispatchWaveReport, error) {
		dispatchCalls++
		if err := waveOpts.BeforeProviderCall(104); err != nil {
			return tickWaveReport(DispatchWaveIssueResult{Issue: 104, Status: DispatchWaveStatusFailed, Error: err.Error()}), nil
		}
		return DispatchWaveReport{}, nil
	}
	recoveryCalls := 0
	opts.Recover = func(context.Context, recovery.Options) (recovery.Result, error) {
		recoveryCalls++
		return recovery.Result{}, nil
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if dispatchCalls != 1 || recoveryCalls != 0 || report.Status != TickStatusNeedsHuman || report.StopReason != TickStopOrchestrationCostBudget {
		t.Fatalf("dispatch_calls=%d recovery_calls=%d status=%s stop=%s", dispatchCalls, recoveryCalls, report.Status, report.StopReason)
	}
	if report.OrchestrationCost.Totals.ModelCalls != 1 || report.OrchestrationCost.Totals.UsageState != orchestrationcost.UsageUnknown {
		t.Fatalf("reloaded cost = %#v", report.OrchestrationCost)
	}
}

func TestTickRestoresPersistedReleaseBlockBeforeAnotherProvider(t *testing.T) {
	repo := t.TempDir()
	opts := reviewReadyTickOptions(repo, 105, "https://github.com/owner/repo/pull/81")
	usefulTokens, overheadTokens := int64(1000), int64(101)
	persisted, err := orchestrationcost.Build(opts.RunID, orchestrationcost.DefaultPolicy(), []orchestrationcost.Event{
		orchestrationcost.EventFromReport("worker:prior", orchestrationcost.RoleWorker, true, tickProviderReport(usefulTokens), "prior worker"),
		orchestrationcost.EventFromReport("recovery:prior", orchestrationcost.RoleRecovery, false, tickProviderReport(overheadTokens), "prior recovery"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	persisted = orchestrationcost.ApplyReleaseDecision(persisted, orchestrationcost.BindReleaseDecision(orchestrationcost.CheckReleaseGate(persisted), 81))
	if persisted.Status != orchestrationcost.StatusNeedsHuman {
		t.Fatalf("persisted cost = %#v", persisted)
	}
	if err := orchestrationcost.Write(repo, persisted); err != nil {
		t.Fatalf("Write: %v", err)
	}
	dispatchCalls := 0
	opts.DispatchWave = func(_ context.Context, waveOpts DispatchWaveOptions) (DispatchWaveReport, error) {
		dispatchCalls++
		if err := waveOpts.BeforeProviderCall(105); err != nil {
			return tickWaveReport(DispatchWaveIssueResult{Issue: 105, Status: DispatchWaveStatusNeedsHuman, Error: err.Error()}), nil
		}
		return DispatchWaveReport{}, nil
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if dispatchCalls != 1 || report.Status != TickStatusNeedsHuman || report.OrchestrationCost.Status != orchestrationcost.StatusNeedsHuman {
		t.Fatalf("dispatch_calls=%d tick_status=%s cost=%#v", dispatchCalls, report.Status, report.OrchestrationCost)
	}
}

func TestTickKeepsRestoredReleaseCandidateBlockedWithoutAnotherProvider(t *testing.T) {
	repo := t.TempDir()
	const (
		issue    = 211
		prNumber = 89
	)
	opts := reviewReadyTickOptions(repo, issue, "https://github.com/owner/repo/pull/89")
	persistReleaseBlockedCost(t, repo, opts.RunID, prNumber)
	opts.ComputeReadySet = func(context.Context, Options) (report.ReadySetReport, error) {
		return report.ReadySetReport{Blocked: []report.BlockedIssue{{
			Issue:          issue,
			Classification: "has-open-PR",
			OpenPRs:        []report.OpenPRSummary{{Number: prNumber, URL: "https://github.com/owner/repo/pull/89"}},
		}}}, nil
	}
	dispatchCalls, verifierCalls, mergeCalls := 0, 0, 0
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		dispatchCalls++
		return DispatchWaveReport{}, nil
	}
	opts.Loopreview = func(context.Context, loopreview.Options) (loopreview.Result, error) {
		verifierCalls++
		return loopreview.Result{}, nil
	}
	opts.PreProdWriter = tickPreProdWriterFunc(func(context.Context, int, string) (gh.PreProdMergeResult, error) {
		mergeCalls++
		return gh.PreProdMergeResult{}, nil
	})

	result, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Status != TickStatusNeedsHuman || result.StopReason != TickStopOrchestrationCostBudget {
		t.Fatalf("status=%s stop=%s", result.Status, result.StopReason)
	}
	if dispatchCalls != 0 || verifierCalls != 0 || mergeCalls != 0 {
		t.Fatalf("dispatch_calls=%d verifier_calls=%d merge_calls=%d", dispatchCalls, verifierCalls, mergeCalls)
	}
	if len(result.NeedsHuman) != 1 || result.NeedsHuman[0].Issue != issue || result.NeedsHuman[0].PR != "https://github.com/owner/repo/pull/89" {
		t.Fatalf("needs_human = %#v", result.NeedsHuman)
	}
}

func TestTickRaisedPolicyResumesRestoredReleaseCandidateWithoutAnotherProvider(t *testing.T) {
	repo := t.TempDir()
	const (
		issue    = 212
		prNumber = 90
	)
	opts := reviewReadyTickOptions(repo, issue, "https://github.com/owner/repo/pull/90")
	persistReleaseBlockedCost(t, repo, opts.RunID, prNumber)
	opts.CostPolicy = orchestrationcost.Policy{MaxModelCalls: 8, MaxTokens: 500_000, MaxOverheadPercent: 20}
	opts.ComputeReadySet = func(context.Context, Options) (report.ReadySetReport, error) {
		return report.ReadySetReport{Blocked: []report.BlockedIssue{{
			Issue:          issue,
			Classification: "has-open-PR",
			OpenPRs:        []report.OpenPRSummary{{Number: prNumber, URL: "https://github.com/owner/repo/pull/90"}},
		}}}, nil
	}
	dispatchCalls, verifierCalls := 0, 0
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		dispatchCalls++
		return DispatchWaveReport{}, nil
	}
	opts.Loopreview = func(context.Context, loopreview.Options) (loopreview.Result, error) {
		verifierCalls++
		return loopreview.Result{}, nil
	}
	writer := &recordingPreProdWriter{mergeResult: gh.PreProdMergeResult{
		PRNumber: prNumber,
		Branch:   "pre-prod",
		Head:     "loop/issue-968",
		SHA:      "abc123",
	}}
	opts.PreProdWriter = writer

	result, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if result.Status != TickStatusSucceeded || result.StopReason != TickStopCompleted {
		t.Fatalf("status=%s stop=%s failures=%#v needs_human=%#v", result.Status, result.StopReason, result.Failures, result.NeedsHuman)
	}
	if dispatchCalls != 0 || verifierCalls != 0 || writer.mergeCalls != 1 {
		t.Fatalf("dispatch_calls=%d verifier_calls=%d merge_calls=%d", dispatchCalls, verifierCalls, writer.mergeCalls)
	}
	if result.OrchestrationCost.ReleaseGate == nil || !result.OrchestrationCost.ReleaseGate.Allowed || !result.OrchestrationCost.ReleaseGate.Consumed || result.OrchestrationCost.ReleaseGate.PRNumber != prNumber {
		t.Fatalf("cost = %#v", result.OrchestrationCost)
	}

	second, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if second.Status != TickStatusNoReadyWork || writer.mergeCalls != 1 {
		t.Fatalf("second status=%s merge_calls=%d cost=%#v", second.Status, writer.mergeCalls, second.OrchestrationCost)
	}
}

func persistReleaseBlockedCost(t *testing.T, repo, runID string, prNumber int) {
	t.Helper()
	usefulTokens, verifierTokens, overheadTokens := int64(1000), int64(0), int64(101)
	persisted, err := orchestrationcost.Build(runID, orchestrationcost.DefaultPolicy(), []orchestrationcost.Event{
		orchestrationcost.EventFromReport("worker:prior", orchestrationcost.RoleWorker, true, tickProviderReport(usefulTokens), "prior worker"),
		orchestrationcost.EventFromReport(fmt.Sprintf("verifier:pr-%d", prNumber), orchestrationcost.RoleVerifier, true, tickProviderReport(verifierTokens), fmt.Sprintf("pr=%d", prNumber)),
		orchestrationcost.EventFromReport("recovery:prior", orchestrationcost.RoleRecovery, false, tickProviderReport(overheadTokens), "prior recovery"),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	persisted = orchestrationcost.ApplyReleaseDecision(persisted, orchestrationcost.BindReleaseDecision(orchestrationcost.CheckReleaseGate(persisted), prNumber))
	if persisted.Status != orchestrationcost.StatusNeedsHuman {
		t.Fatalf("persisted cost = %#v", persisted)
	}
	if err := orchestrationcost.Write(repo, persisted); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestTickVerifierTokenOverrunBlocksAutomaticMerge(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 106, "https://github.com/owner/repo/pull/82")
	opts.CostPolicy = orchestrationcost.Policy{MaxModelCalls: 4, MaxTokens: 100, MaxOverheadPercent: 10}
	workerTokens, verifierTokens := int64(60), int64(50)
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		return tickWaveReport(DispatchWaveIssueResult{Issue: 106, Status: DispatchWaveStatusSucceeded, ProviderInvoked: true, PR: "https://github.com/owner/repo/pull/82", Report: tickProviderReport(workerTokens)}), nil
	}
	opts.Loopreview = func(context.Context, loopreview.Options) (loopreview.Result, error) {
		result := tickLoopreview(loopreview.VerdictPass, "review passed")
		result.ProviderInvoked = true
		result.Verdict.Report = tickProviderReport(verifierTokens)
		return result, nil
	}
	mergeCalls := 0
	opts.PreProdWriter = tickPreProdWriterFunc(func(context.Context, int, string) (gh.PreProdMergeResult, error) {
		mergeCalls++
		return gh.PreProdMergeResult{}, nil
	})

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if mergeCalls != 0 || report.Status != TickStatusNeedsHuman || report.OrchestrationCost.Reason != "token-budget-exceeded" {
		t.Fatalf("merge_calls=%d status=%s cost=%#v", mergeCalls, report.Status, report.OrchestrationCost)
	}
}

func TestTickHungVerifierWithoutReportBlocksNextVerifier(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 107, "https://github.com/owner/repo/pull/83")
	workerTokens := int64(10)
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		return tickWaveReport(
			DispatchWaveIssueResult{Issue: 107, Status: DispatchWaveStatusSucceeded, ProviderInvoked: true, PR: "https://github.com/owner/repo/pull/83", Report: tickProviderReport(workerTokens)},
			DispatchWaveIssueResult{Issue: 108, Status: DispatchWaveStatusSucceeded, ProviderInvoked: true, PR: "https://github.com/owner/repo/pull/84", Report: tickProviderReport(workerTokens)},
		), nil
	}
	verifierCalls := 0
	opts.Loopreview = func(_ context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
		if err := reviewOpts.BeforeProviderCall(); err != nil {
			return loopreview.Result{}, err
		}
		verifierCalls++
		return loopreview.Result{
			ProviderInvoked: true,
			Verdict:         loopreview.Verdict{Verdict: loopreview.VerdictNeedsHuman, Findings: []loopreview.Finding{}, Evidence: "verifier hung", SpecConformance: loopreview.SpecConformanceNotApplicable},
			ExitCode:        2,
		}, nil
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if verifierCalls != 1 || report.Status != TickStatusNeedsHuman || report.OrchestrationCost.Totals.UsageState != orchestrationcost.UsageUnknown || report.OrchestrationCost.Totals.ModelCalls != 3 {
		t.Fatalf("verifier_calls=%d status=%s cost=%#v", verifierCalls, report.Status, report.OrchestrationCost)
	}
}

func TestTickReleaseGateBlocksOverheadAboveTenPercent(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 103, "https://github.com/owner/repo/pull/79")
	overheadTokens := int64(101)
	workerTokens := int64(1000)
	verifierTokens := int64(0)
	opts.CostEvents = []orchestrationcost.Event{
		orchestrationcost.EventFromReport("recovery:prior", orchestrationcost.RoleRecovery, false, tickProviderReport(overheadTokens), "prior recovery call"),
	}
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		return tickWaveReport(DispatchWaveIssueResult{Issue: 103, Status: DispatchWaveStatusSucceeded, ProviderInvoked: true, PR: "https://github.com/owner/repo/pull/79", Report: tickProviderReport(workerTokens)}), nil
	}
	opts.Loopreview = func(context.Context, loopreview.Options) (loopreview.Result, error) {
		result := tickLoopreview(loopreview.VerdictPass, "review passed")
		result.Verdict.Report = tickProviderReport(verifierTokens)
		return result, nil
	}
	mergeCalls := 0
	opts.PreProdWriter = tickPreProdWriterFunc(func(context.Context, int, string) (gh.PreProdMergeResult, error) {
		mergeCalls++
		return gh.PreProdMergeResult{}, nil
	})

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopOrchestrationCostBudget || mergeCalls != 0 {
		t.Fatalf("status=%s stop=%s merge_calls=%d", report.Status, report.StopReason, mergeCalls)
	}
	if report.OrchestrationCost.OverheadRatio.Display != "10.10%" || report.OrchestrationCost.ReleaseGate == nil || report.OrchestrationCost.ReleaseGate.Allowed {
		t.Fatalf("cost = %#v", report.OrchestrationCost)
	}
}

func TestTickPreProdHealthProgressCorrelationsArePerItem(t *testing.T) {
	progressRecorder := &recordingProgressRecorder{}
	opts := reviewReadyTickOptions(t.TempDir(), 41, "https://github.com/owner/repo/pull/410")
	opts.Progress = progressRecorder
	opts.Reader = fakeReader{
		checks: map[int][]gh.Check{
			410: passChecks(),
			411: passChecks(),
		},
		diffFiles: map[int][]string{
			410: {"README.md"},
			411: {"docs/README.md"},
		},
		diffs: map[int]string{
			410: modifiedDiff("README.md"),
			411: modifiedDiff("docs/README.md"),
		},
		branchChecks: map[string]gh.BranchChecksResult{
			"pre-prod": {Branch: "pre-prod", HeadSHA: "shared-preprod-head", Checks: passChecks()},
		},
	}
	opts.ComputeReadySet = func(context.Context, Options) (report.ReadySetReport, error) {
		out := readySetReport(41)
		out.Ready = append(out.Ready, report.ReadyIssue{Issue: 42, Title: "Issue 42", Reason: "ready"})
		return out, nil
	}
	opts.DispatchWave = func(context.Context, DispatchWaveOptions) (DispatchWaveReport, error) {
		return tickWaveReport(
			DispatchWaveIssueResult{Issue: 41, Status: DispatchWaveStatusSucceeded, PR: "https://github.com/owner/repo/pull/410"},
			DispatchWaveIssueResult{Issue: 42, Status: DispatchWaveStatusSucceeded, PR: "https://github.com/owner/repo/pull/411"},
		), nil
	}
	opts.PreProdWriter = tickPreProdWriterFunc(func(_ context.Context, prNumber int, branch string) (gh.PreProdMergeResult, error) {
		return gh.PreProdMergeResult{PRNumber: prNumber, Branch: branch, Head: "loop/issue-test", SHA: "shared-merge-sha"}, nil
	})

	reportResult, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if reportResult.Status != TickStatusSucceeded || len(reportResult.PreProdHealth) != 2 {
		t.Fatalf("tick status=%s health=%#v", reportResult.Status, reportResult.PreProdHealth)
	}
	correlations := progressRecorder.correlationsFor("pre-prod-health", PreProdHealthStatusGreen, true)
	if len(correlations) != 2 {
		t.Fatalf("pre-prod terminal correlations = %#v, want two distinct issue/PR-scoped correlations", correlations)
	}
}

func TestTickPreProdHealthProgressTerminalizesOnlyClosedOutcomes(t *testing.T) {
	now := time.Date(2026, 7, 2, 13, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		checks     []gh.Check
		readErr    error
		wantStatus string
		terminal   bool
	}{
		{name: "red", checks: []gh.Check{{Name: "verify", Bucket: "fail"}}, wantStatus: PreProdHealthStatusRed, terminal: true},
		{name: "pending", checks: []gh.Check{{Name: "verify", Bucket: "pending"}}, wantStatus: PreProdHealthStatusPending, terminal: false},
		{name: "read error", readErr: errors.New("branch checks unavailable"), wantStatus: PreProdHealthStatusUnknown, terminal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			progressRecorder := &recordingProgressRecorder{}
			opts := reviewReadyTickOptions(t.TempDir(), 43, "https://github.com/owner/repo/pull/430")
			opts.Progress = progressRecorder
			opts.Clock = func() time.Time { return now }
			opts.Reader = fakeReader{
				checks:    map[int][]gh.Check{430: passChecks()},
				diffFiles: map[int][]string{430: {"README.md"}},
				diffs:     map[int]string{430: modifiedDiff("README.md")},
				branchChecks: map[string]gh.BranchChecksResult{
					"pre-prod": {Branch: "pre-prod", HeadSHA: "merge-sha", Checks: tt.checks},
				},
				branchCheckErrs: map[string]error{"pre-prod": tt.readErr},
			}
			opts.PreProdWriter = &recordingPreProdWriter{
				mergeResult:  gh.PreProdMergeResult{PRNumber: 430, Branch: "pre-prod", Head: "loop/issue-43", SHA: "merge-sha"},
				revertResult: gh.PreProdRevertResult{PRNumber: 430, Branch: "pre-prod", RevertedSHA: "merge-sha", SHA: "revert-sha"},
			}

			_, err := Tick(context.Background(), opts)
			if err != nil {
				t.Fatalf("Tick returned error: %v", err)
			}
			gotTerminal := progressRecorder.hasTerminal("pre-prod-health", tt.wantStatus)
			if gotTerminal != tt.terminal {
				t.Fatalf("progress observations = %#v, terminal(%s) = %t, want %t", progressRecorder.observations, tt.wantStatus, gotTerminal, tt.terminal)
			}
			if !tt.terminal && !progressRecorder.hasStatus(tt.wantStatus) {
				t.Fatalf("progress observations = %#v, want active pre-prod-health status %s", progressRecorder.observations, tt.wantStatus)
			}
			for _, observation := range progressRecorder.observations {
				if observation.Phase == "pre-prod-health" && !observation.OccurredAt.Equal(now) {
					t.Fatalf("pre-prod progress occurred_at = %s, want injected clock %s", observation.OccurredAt, now)
				}
			}
		})
	}
}

func TestTickPreProdHealthPendingStaysActiveUntilGreenTerminal(t *testing.T) {
	ctx := context.Background()
	clock := newOrchestrationManualClock(time.Date(2026, 7, 2, 15, 0, 0, 0, time.UTC))
	store := newOrchestrationProgressStore(t, ctx, clock, "proj_progress")
	defer store.Close()
	supervisor, err := progress.NewSupervisor(progress.SupervisorOptions{
		Store:              store,
		ProjectID:          "proj_progress",
		DeliveryRunID:      "run-ci",
		RunID:              "run-ci",
		MaxSilenceInterval: 5 * time.Minute,
		Clock:              clock,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	defer func() {
		if err := supervisor.Stop(context.Background()); err != nil {
			t.Fatalf("Stop supervisor: %v", err)
		}
	}()

	correlationID := ciProgressCorrelation("run-ci", 43, "430", "pre-prod-health", "merge-sha")
	emitCIProgress(ctx, supervisor, clock.Now(), "run-ci", 43, "430", "pre-prod-health", "merge-sha", PreProdHealthStatusPending, progress.KnownWaitingCI, "pre-prod branch checks are still pending", false)
	clock.Advance(5 * time.Minute)
	waitForOrchestrationProgressReceipts(t, ctx, store, correlationID, 2)
	receipts := listOrchestrationProgressReceipts(t, ctx, store, correlationID)
	if len(receipts) != 2 || receipts[0].Status != PreProdHealthStatusPending || receipts[1].Status != PreProdHealthStatusPending {
		t.Fatalf("pending receipts = %#v, want state-change plus periodic wait", receipts)
	}
	if !orchestrationContainsString(receipts[1].GapReasons, progress.ReasonMaxGenerationSilence) || !orchestrationContainsString(receipts[1].GapReasons, progress.KnownWaitingCI) {
		t.Fatalf("periodic pending gap reasons = %#v, want max-silence waiting-for-ci", receipts[1].GapReasons)
	}

	clock.Advance(time.Minute)
	emitCIProgress(ctx, supervisor, clock.Now(), "run-ci", 43, "430", "pre-prod-health", "merge-sha", PreProdHealthStatusPending, progress.KnownWaitingCI, "pre-prod branch checks are still pending", false)
	clock.Advance(time.Minute)
	emitCIProgress(ctx, supervisor, clock.Now(), "run-ci", 43, "430", "pre-prod-health", "merge-sha", PreProdHealthStatusGreen, progress.KnownTerminal, "pre-prod branch checks are green", true)
	receipts = listOrchestrationProgressReceipts(t, ctx, store, correlationID)
	if !orchestrationReceiptsContainStatusWithGap(receipts, PreProdHealthStatusGreen, progress.ReasonTerminal) {
		t.Fatalf("receipts = %#v, want green terminal receipt after repeated pending", receipts)
	}
	countAfterGreen := len(receipts)

	clock.Advance(time.Minute)
	emitCIProgress(ctx, supervisor, clock.Now(), "run-ci", 43, "430", "pre-prod-health", "merge-sha", PreProdHealthStatusPending, progress.KnownWaitingCI, "pre-prod branch checks are still pending", false)
	receipts = listOrchestrationProgressReceipts(t, ctx, store, correlationID)
	if len(receipts) != countAfterGreen {
		t.Fatalf("post-terminal pending persisted receipts = %#v, want count %d", receipts, countAfterGreen)
	}
}

func TestTickRiskGateProgressTerminalizesNeedsHumanWithInjectedClock(t *testing.T) {
	now := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	progressRecorder := &recordingProgressRecorder{}
	opts := reviewReadyTickOptions(t.TempDir(), 44, "https://github.com/owner/repo/pull/440")
	opts.Progress = progressRecorder
	opts.Clock = func() time.Time { return now }
	opts.Reader = destructiveRiskReader(440)

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if report.Status != TickStatusNeedsHuman || report.StopReason != TickStopRiskGateNeedsHuman {
		t.Fatalf("tick status = %s stop = %s", report.Status, report.StopReason)
	}
	if !progressRecorder.hasTerminal("risk-gate", RiskGateStatusNeedsHuman) {
		t.Fatalf("progress observations = %#v, want terminal risk-gate needs-human", progressRecorder.observations)
	}
	for _, observation := range progressRecorder.observations {
		if observation.Phase == "risk-gate" && !observation.OccurredAt.Equal(now) {
			t.Fatalf("risk-gate progress occurred_at = %s, want injected clock %s", observation.OccurredAt, now)
		}
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

func TestTickRenderedArtifactsSurfaceInJSONAndText(t *testing.T) {
	opts := reviewReadyTickOptions(t.TempDir(), 34, "https://github.com/owner/repo/pull/340")
	artifact := loopreview.RenderedArtifact{
		Source:         "domain.evidence.producer",
		Status:         "available",
		DeclaredOutput: "out/report.pdf",
		Path:           "out/report.pdf",
		Kind:           "pdf",
		Bytes:          1234,
		Summary:        "PDF binary summary: version=1.7 bytes=1234 sha256=abc",
	}
	opts.Loopreview = func(context.Context, loopreview.Options) (loopreview.Result, error) {
		result := tickLoopreview(loopreview.VerdictPass, "review passed with rendered artifact")
		result.Verdict.RenderedArtifacts = []loopreview.RenderedArtifact{artifact}
		return result, nil
	}

	report, err := Tick(context.Background(), opts)
	if err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}
	if len(report.Reviews) != 1 || !reflect.DeepEqual(report.Reviews[0].RenderedArtifacts, []loopreview.RenderedArtifact{artifact}) {
		t.Fatalf("review rendered artifacts = %#v, want %#v", report.Reviews, artifact)
	}

	data, err := MarshalTickJSON(report)
	if err != nil {
		t.Fatalf("MarshalTickJSON returned error: %v", err)
	}
	for _, want := range []string{
		`"rendered_artifacts"`,
		`"source": "domain.evidence.producer"`,
		`"path": "out/report.pdf"`,
		`"kind": "pdf"`,
	} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("tick JSON missing %q:\n%s", want, string(data))
		}
	}

	text := RenderTickText(report)
	for _, want := range []string{
		"rendered_artifact: domain.evidence.producer available path=out/report.pdf",
		"summary=PDF binary summary: version=1.7 bytes=1234 sha256=abc",
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
		branchHeads: map[string]string{"pre-prod": "preprod-prior-sha"},
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
	if len(report.PreProdMerges) != 1 || report.PreProdMerges[0].PriorStableCommit != "preprod-prior-sha" {
		t.Fatalf("pre-prod merges = %#v", report.PreProdMerges)
	}
	if len(report.PreProdReverts) != 1 ||
		report.PreProdReverts[0].Status != TickStatusSucceeded ||
		report.PreProdReverts[0].RevertedSHA != "merge-sha" ||
		report.PreProdReverts[0].MergeCommit != "merge-sha" ||
		report.PreProdReverts[0].PriorStableCommit != "preprod-prior-sha" {
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
	if report.StartedAt != "2026-07-02T12:00:00Z" {
		t.Fatalf("started = %q", report.StartedAt)
	}
	finishedAt, err := time.Parse(time.RFC3339Nano, report.FinishedAt)
	if err != nil {
		t.Fatalf("parse finished_at %q: %v", report.FinishedAt, err)
	}
	if !finishedAt.After(started) {
		t.Fatalf("finished_at = %s, want after %s", finishedAt, started)
	}
	if report.StartedAt == report.FinishedAt {
		t.Fatalf("started and finished timestamps should differ: %q", report.StartedAt)
	}
	if calls < 2 {
		t.Fatalf("clock calls = %d, want at least start and finish", calls)
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

func tickProviderReport(totalTokens int64) *reporter.Report {
	return &reporter.Report{DurationMS: 100, Usage: reporter.Usage{TotalTokens: &totalTokens}}
}

func tickProviderReportUnknown() *reporter.Report {
	return &reporter.Report{DurationMS: 100}
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

type recordingProgressRecorder struct {
	observations []progress.Observation
}

func (r *recordingProgressRecorder) Emit(_ context.Context, observation progress.Observation) (progress.EmitResult, error) {
	r.observations = append(r.observations, observation)
	return progress.EmitResult{Emitted: true}, nil
}

func (r *recordingProgressRecorder) Terminal(_ context.Context, observation progress.Observation) (progress.EmitResult, error) {
	observation.Terminal = true
	r.observations = append(r.observations, observation)
	return progress.EmitResult{Emitted: true}, nil
}

func (r *recordingProgressRecorder) hasKnown(known string) bool {
	for _, observation := range r.observations {
		if observation.KnownState == known {
			return true
		}
	}
	return false
}

func (r *recordingProgressRecorder) hasStatus(status string) bool {
	for _, observation := range r.observations {
		if observation.Status == status {
			return true
		}
	}
	return false
}

func (r *recordingProgressRecorder) hasTerminal(phase, status string) bool {
	for _, observation := range r.observations {
		if observation.Phase == phase && observation.Status == status && observation.Terminal {
			return true
		}
	}
	return false
}

func (r *recordingProgressRecorder) correlationsFor(phase, status string, terminal bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, observation := range r.observations {
		if observation.Phase != phase || observation.Status != status || observation.Terminal != terminal {
			continue
		}
		if !seen[observation.CorrelationID] {
			seen[observation.CorrelationID] = true
			out = append(out, observation.CorrelationID)
		}
	}
	return out
}

type orchestrationManualClock struct {
	mu  sync.Mutex
	now time.Time
	ch  chan time.Time
}

func newOrchestrationManualClock(now time.Time) *orchestrationManualClock {
	return &orchestrationManualClock{now: now.UTC(), ch: make(chan time.Time, 16)}
}

func (c *orchestrationManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *orchestrationManualClock) NewTicker(time.Duration) progress.Ticker {
	return orchestrationManualTicker{ch: c.ch}
}

func (c *orchestrationManualClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	now := c.now
	c.mu.Unlock()
	c.ch <- now
}

type orchestrationManualTicker struct {
	ch <-chan time.Time
}

func (t orchestrationManualTicker) C() <-chan time.Time { return t.ch }
func (t orchestrationManualTicker) Stop()               {}

func newOrchestrationProgressStore(t *testing.T, ctx context.Context, clock *orchestrationManualClock, projectID string) storage.Store {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: clock.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, local_path_canonical, display_name, identity_source, created_at, updated_at)
			VALUES (?, '/repo', '/repo', 'repo', 'local-path', '2026-07-02T15:00:00Z', '2026-07-02T15:00:00Z')
			ON CONFLICT(id) DO NOTHING`, projectID)
		return err
	}); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	return store
}

func listOrchestrationProgressReceipts(t *testing.T, ctx context.Context, store storage.Store, correlationID string) []progress.ProgressReceipt {
	t.Helper()
	receipts, err := progress.ListReceipts(ctx, store, progress.ListFilter{
		ProjectID:     "proj_progress",
		DeliveryRunID: "run-ci",
		CorrelationID: correlationID,
	})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	return receipts
}

func waitForOrchestrationProgressReceipts(t *testing.T, ctx context.Context, store storage.Store, correlationID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := len(listOrchestrationProgressReceipts(t, ctx, store, correlationID)); got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("receipt count = %d, want at least %d", len(listOrchestrationProgressReceipts(t, ctx, store, correlationID)), want)
}

func orchestrationReceiptsContainStatusWithGap(receipts []progress.ProgressReceipt, status, gap string) bool {
	for _, receipt := range receipts {
		if receipt.Status == status && orchestrationContainsString(receipt.GapReasons, gap) {
			return true
		}
	}
	return false
}

func orchestrationContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
