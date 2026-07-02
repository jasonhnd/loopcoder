package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

const (
	TickReportVersion = 1

	TickStatusSucceeded   = "succeeded"
	TickStatusFailed      = "failed"
	TickStatusNeedsHuman  = "needs-human"
	TickStatusNoReadyWork = "no-ready-work"

	TickStopCompleted                 = "completed"
	TickStopPlanApprovalRequired      = "plan-approval-required"
	TickStopNoReadyWork               = "no-ready-work"
	TickStopGuardrailFrozen           = "guardrail-frozen"
	TickStopGuardrailNeedsHuman       = "guardrail-needs-human"
	TickStopVerifierProviderMissing   = "verifier-provider-missing"
	TickStopDispatchFailed            = "dispatch-failed"
	TickStopReviewFailed              = "review-failed"
	TickStopReviewNeedsHuman          = "review-needs-human"
	TickStopRiskGateNeedsHuman        = "risk-gate-needs-human"
	TickStopStatePushFailed           = "state-push-failed"
	TickStopCompileFailed             = "compile-failed"
	TickStopReadySetFailed            = "ready-set-failed"
	TickStopAttemptLoadFailed         = "attempt-load-failed"
	TickStopDispatchWaveFailed        = "dispatch-wave-failed"
	TickStopNoReviewablePRsDispatched = "no-reviewable-prs-dispatched"
)

type CompileFunc func(ctx context.Context, opts compiler.Options) (compiler.Report, error)
type DispatchWaveFunc func(ctx context.Context, opts DispatchWaveOptions) (DispatchWaveReport, error)
type LoopreviewFunc func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error)
type StatePushFunc func(ctx context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error)

type TickOptions struct {
	Reader                 GitHubReader
	IssueWriter            compiler.IssueWriter
	RepoPath               string
	BaseBranch             string
	RunID                  string
	WorkerProvider         string
	WorkerModel            string
	WorkerEffort           string
	VerifierProvider       string
	VerifierModel          string
	VerifierEffort         string
	VerifierTimeout        time.Duration
	PreProdBranch          string
	RequiredChecks         []string
	ThrottleLimit          int
	Thresholds             config.ResilienceWorker
	Budget                 config.GuardrailBudget
	CircuitBreaker         config.GuardrailCircuitBreaker
	AdditionalRiskRedLines []RiskRedLine
	ProcessAlive           ProcessAliveFunc
	Clock                  func() time.Time
	Stderr                 io.Writer

	Compile         CompileFunc
	ComputeReadySet ReadySetFunc
	DispatchWave    DispatchWaveFunc
	Dispatch        WorkerDispatchFunc
	Loopreview      LoopreviewFunc
	RiskGate        RiskGateFunc
	PreProdWriter   PreProdWriter
	StatePush       StatePushFunc
	LoadAttempts    LoadAttemptsFunc
}

type TickReport struct {
	Version       int                      `json:"version"`
	Repo          string                   `json:"repo"`
	RepoPath      string                   `json:"repo_path"`
	BaseBranch    string                   `json:"base_branch"`
	PreProdBranch string                   `json:"pre_prod_branch"`
	RunID         string                   `json:"run_id"`
	Status        string                   `json:"status"`
	StopReason    string                   `json:"stop_reason"`
	StartedAt     string                   `json:"started_at"`
	FinishedAt    string                   `json:"finished_at"`
	Compile       compiler.Report          `json:"compile"`
	ReadySet      report.ReadySetReport    `json:"ready_set"`
	DispatchWave  *DispatchWaveReport      `json:"dispatch_wave,omitempty"`
	Reviews       []TickReviewResult       `json:"reviews"`
	RiskGates     []TickRiskGateResult     `json:"risk_gates"`
	PreProdMerges []TickPreProdMergeResult `json:"pre_prod_merges"`
	NeedsHuman    []TickIssue              `json:"needs_human"`
	Failures      []TickIssue              `json:"failures"`
	StatePush     *TickStatePush           `json:"state_push,omitempty"`
	Summary       TickSummary              `json:"summary"`
}

type TickReviewResult struct {
	Issue           int                            `json:"issue,omitempty"`
	PR              string                         `json:"pr,omitempty"`
	PRNumber        int                            `json:"pr_number,omitempty"`
	Verdict         string                         `json:"verdict"`
	SpecConformance string                         `json:"spec_conformance,omitempty"`
	Evidence        string                         `json:"evidence,omitempty"`
	Findings        []loopreview.Finding           `json:"findings"`
	Error           string                         `json:"error,omitempty"`
	Attestation     *attestation.AttestationRecord `json:"attestation,omitempty"`
}

type TickIssue struct {
	Step   string `json:"step"`
	Issue  int    `json:"issue,omitempty"`
	PR     string `json:"pr,omitempty"`
	Detail string `json:"detail"`
}

type TickRiskGateResult struct {
	Issue          int           `json:"issue,omitempty"`
	PR             string        `json:"pr,omitempty"`
	PRNumber       int           `json:"pr_number,omitempty"`
	Status         string        `json:"status"`
	RequiredChecks []string      `json:"required_checks"`
	ChangedFiles   []string      `json:"changed_files"`
	Checks         []gh.Check    `json:"checks"`
	RedLines       []RiskRedLine `json:"red_lines"`
	Error          string        `json:"error,omitempty"`
}

type TickPreProdMergeResult struct {
	Issue    int    `json:"issue,omitempty"`
	PR       string `json:"pr,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`
	Branch   string `json:"branch"`
	Head     string `json:"head,omitempty"`
	SHA      string `json:"sha,omitempty"`
	URL      string `json:"url,omitempty"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type TickStatePush struct {
	Branch    string   `json:"branch"`
	Remote    string   `json:"remote"`
	Committed bool     `json:"committed"`
	Pushed    bool     `json:"pushed"`
	PushError string   `json:"push_error,omitempty"`
	Files     []string `json:"files"`
	Error     string   `json:"error,omitempty"`
}

type TickSummary struct {
	CompiledCreatedCount    int `json:"compiled_created_count"`
	CompiledUpdatedCount    int `json:"compiled_updated_count"`
	CompiledUnchangedCount  int `json:"compiled_unchanged_count"`
	CompiledClosedCount     int `json:"compiled_closed_count"`
	ReadyCount              int `json:"ready_count"`
	BlockedCount            int `json:"blocked_count"`
	DispatchedPRCount       int `json:"dispatched_pr_count"`
	ReviewPassCount         int `json:"review_pass_count"`
	ReviewFailCount         int `json:"review_fail_count"`
	ReviewNeedsHumanCount   int `json:"review_needs_human_count"`
	RiskGateCleanCount      int `json:"risk_gate_clean_count"`
	RiskGateNeedsHumanCount int `json:"risk_gate_needs_human_count"`
	PreProdMergeCount       int `json:"pre_prod_merge_count"`
	NeedsHumanCount         int `json:"needs_human_count"`
	FailureCount            int `json:"failure_count"`
}

func Tick(ctx context.Context, opts TickOptions) (TickReport, error) {
	if opts.Reader == nil {
		return TickReport{}, errors.New("github reader is required")
	}
	if opts.IssueWriter == nil {
		return TickReport{}, errors.New("github issue writer is required")
	}
	opts = withTickDefaults(opts)

	started := opts.Clock().UTC()
	if strings.TrimSpace(opts.RunID) == "" {
		opts.RunID = state.RunIDForWave(started)
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = "main"
	}
	if strings.TrimSpace(opts.PreProdBranch) == "" {
		opts.PreProdBranch = "pre-prod"
	}

	tickReport := TickReport{
		Version:       TickReportVersion,
		Repo:          filepath.ToSlash(opts.RepoPath),
		RepoPath:      filepath.ToSlash(opts.RepoPath),
		BaseBranch:    opts.BaseBranch,
		PreProdBranch: opts.PreProdBranch,
		RunID:         opts.RunID,
		StartedAt:     state.FormatTimestamp(started),
		Reviews:       []TickReviewResult{},
		RiskGates:     []TickRiskGateResult{},
		PreProdMerges: []TickPreProdMergeResult{},
		NeedsHuman:    []TickIssue{},
		Failures:      []TickIssue{},
	}
	finish := func(status, stopReason string) (TickReport, error) {
		finished := opts.Clock().UTC()
		tickReport.Status = status
		tickReport.StopReason = stopReason
		tickReport.FinishedAt = state.FormatTimestamp(finished)
		tickReport.Summary = summarizeTick(tickReport)
		return normalizeTickReport(tickReport), nil
	}

	compiled, err := opts.Compile(ctx, compiler.Options{
		RepoPath: opts.RepoPath,
		Writer:   opts.IssueWriter,
		Now:      started,
	})
	tickReport.Compile = compiled
	tickReport.Repo = firstNonEmpty(compiled.Repo, tickReport.Repo)
	if err != nil {
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "compile", Detail: err.Error()})
		return finish(TickStatusFailed, TickStopCompileFailed)
	}
	if compiled.PlanApprovalRequired {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "compile",
			Detail: "compiled plan requires human approval before dispatch",
		})
		return finish(TickStatusNeedsHuman, TickStopPlanApprovalRequired)
	}

	attempts, err := opts.LoadAttempts(opts.RepoPath, opts.RunID)
	if err != nil {
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "ready-set", Detail: fmt.Sprintf("load attempts: %v", err)})
		return finish(TickStatusFailed, TickStopAttemptLoadFailed)
	}
	readySet, err := opts.ComputeReadySet(ctx, Options{
		Reader:       opts.Reader,
		RepoPath:     opts.RepoPath,
		BaseBranch:   opts.BaseBranch,
		RunID:        opts.RunID,
		Attempts:     attempts,
		Thresholds:   opts.Thresholds,
		ProcessAlive: opts.ProcessAlive,
		Now:          started,
	})
	tickReport.ReadySet = readySet
	tickReport.Repo = firstNonEmpty(readySet.Repo, tickReport.Repo)
	if err != nil {
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "ready-set", Detail: err.Error()})
		return finish(TickStatusFailed, TickStopReadySetFailed)
	}
	for _, item := range readySet.Blocked {
		if item.Classification == "guardrail-frozen" {
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   "ready-set",
				Issue:  item.Issue,
				Detail: item.Reason,
			})
		}
	}
	if len(tickReport.NeedsHuman) > 0 {
		return finish(TickStatusNeedsHuman, TickStopGuardrailFrozen)
	}
	if len(readySet.Ready) == 0 {
		return finish(TickStatusNoReadyWork, TickStopNoReadyWork)
	}
	if strings.TrimSpace(opts.VerifierProvider) == "" {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "loopreview",
			Detail: "verifier provider is required before dispatching unattended work",
		})
		return finish(TickStatusNeedsHuman, TickStopVerifierProviderMissing)
	}

	wave, err := opts.DispatchWave(ctx, DispatchWaveOptions{
		Reader:          opts.Reader,
		RepoPath:        opts.RepoPath,
		BaseBranch:      opts.BaseBranch,
		RunID:           opts.RunID,
		ReadySet:        &readySet,
		Provider:        opts.WorkerProvider,
		Model:           opts.WorkerModel,
		Effort:          opts.WorkerEffort,
		ThrottleLimit:   opts.ThrottleLimit,
		Thresholds:      opts.Thresholds,
		Budget:          opts.Budget,
		CircuitBreaker:  opts.CircuitBreaker,
		ProcessAlive:    opts.ProcessAlive,
		Now:             started,
		Stderr:          opts.Stderr,
		ComputeReadySet: opts.ComputeReadySet,
		Dispatch:        opts.Dispatch,
		LoadAttempts:    opts.LoadAttempts,
	})
	tickReport.DispatchWave = &wave
	tickReport.Repo = firstNonEmpty(wave.Repo, tickReport.Repo)
	if err != nil {
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "dispatch-wave", Detail: err.Error()})
		pushTickState(ctx, opts, &tickReport)
		return finish(TickStatusFailed, TickStopDispatchWaveFailed)
	}

	for _, result := range wave.Results {
		switch result.Status {
		case DispatchWaveStatusNeedsHuman:
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   "dispatch-wave",
				Issue:  result.Issue,
				PR:     result.PR,
				Detail: result.Error,
			})
		case DispatchWaveStatusFailed:
			tickReport.Failures = append(tickReport.Failures, TickIssue{
				Step:   "dispatch-wave",
				Issue:  result.Issue,
				PR:     result.PR,
				Detail: result.Error,
			})
		}
	}
	if len(tickReport.NeedsHuman) > 0 {
		pushTickState(ctx, opts, &tickReport)
		if hasStatePushFailure(tickReport.Failures) {
			return finish(TickStatusFailed, TickStopStatePushFailed)
		}
		if len(tickReport.Failures) > 0 {
			return finish(TickStatusFailed, TickStopDispatchFailed)
		}
		return finish(TickStatusNeedsHuman, TickStopGuardrailNeedsHuman)
	}
	if len(tickReport.Failures) > 0 {
		pushTickState(ctx, opts, &tickReport)
		if len(tickReport.Failures) > 0 && hasStatePushFailure(tickReport.Failures) {
			return finish(TickStatusFailed, TickStopStatePushFailed)
		}
		return finish(TickStatusFailed, TickStopDispatchFailed)
	}

	reviewable := reviewablePRs(wave.Results)
	if len(reviewable) == 0 {
		pushTickState(ctx, opts, &tickReport)
		if len(tickReport.Failures) > 0 {
			return finish(TickStatusFailed, TickStopStatePushFailed)
		}
		return finish(TickStatusNoReadyWork, TickStopNoReviewablePRsDispatched)
	}

	for _, item := range reviewable {
		review := TickReviewResult{
			Issue:    item.Issue,
			PR:       item.PR,
			Findings: []loopreview.Finding{},
		}
		prNumber, ok := parseTickPRNumber(item.PR)
		if !ok {
			review.Verdict = loopreview.VerdictNeedsHuman
			review.Error = "could not parse pull request number"
			tickReport.Reviews = append(tickReport.Reviews, review)
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   "loopreview",
				Issue:  item.Issue,
				PR:     item.PR,
				Detail: review.Error,
			})
			continue
		}
		review.PRNumber = prNumber
		result, err := opts.Loopreview(ctx, loopreview.Options{
			RepoPath:   opts.RepoPath,
			PRNumber:   prNumber,
			Provider:   opts.VerifierProvider,
			Model:      opts.VerifierModel,
			Effort:     opts.VerifierEffort,
			BaseBranch: opts.BaseBranch,
			Timeout:    opts.VerifierTimeout,
			Stderr:     opts.Stderr,
		})
		if err != nil {
			review.Verdict = loopreview.VerdictNeedsHuman
			review.Error = err.Error()
			tickReport.Reviews = append(tickReport.Reviews, review)
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   "loopreview",
				Issue:  item.Issue,
				PR:     item.PR,
				Detail: err.Error(),
			})
			continue
		}
		review.Verdict = result.Verdict.Verdict
		review.SpecConformance = result.Verdict.SpecConformance
		review.Evidence = result.Verdict.Evidence
		review.Findings = result.Verdict.Findings
		review.Attestation = result.Verdict.Attestation
		tickReport.Reviews = append(tickReport.Reviews, review)
		switch result.Verdict.Verdict {
		case loopreview.VerdictPass:
			runTickRiskGateAndPreProdMerge(ctx, opts, &tickReport, item, prNumber)
		case loopreview.VerdictFail:
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   "loopreview",
				Issue:  item.Issue,
				PR:     item.PR,
				Detail: firstNonEmpty(result.Verdict.Evidence, "verifier returned fail"),
			})
		default:
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   "loopreview",
				Issue:  item.Issue,
				PR:     item.PR,
				Detail: firstNonEmpty(result.Verdict.Evidence, "verifier returned needs-human"),
			})
		}
	}

	pushTickState(ctx, opts, &tickReport)
	if len(tickReport.Failures) > 0 {
		if hasStatePushFailure(tickReport.Failures) {
			return finish(TickStatusFailed, TickStopStatePushFailed)
		}
		return finish(TickStatusFailed, TickStopReviewFailed)
	}
	if len(tickReport.NeedsHuman) > 0 {
		return finish(TickStatusNeedsHuman, tickNeedsHumanStopReason(tickReport.NeedsHuman))
	}
	return finish(TickStatusSucceeded, TickStopCompleted)
}

func withTickDefaults(opts TickOptions) TickOptions {
	if opts.Clock == nil {
		opts.Clock = func() time.Time {
			return time.Now().UTC()
		}
	}
	if opts.Compile == nil {
		opts.Compile = func(ctx context.Context, opts compiler.Options) (compiler.Report, error) {
			return compiler.Run(ctx, opts, compiler.DefaultDeps())
		}
	}
	if opts.ComputeReadySet == nil {
		opts.ComputeReadySet = ComputeReadySet
	}
	if opts.DispatchWave == nil {
		opts.DispatchWave = DispatchWave
	}
	if opts.Dispatch == nil {
		opts.Dispatch = func(ctx context.Context, opts worker.Options) (worker.Result, error) {
			return worker.Dispatch(ctx, opts, worker.DefaultDeps())
		}
	}
	if opts.Loopreview == nil {
		opts.Loopreview = func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error) {
			return loopreview.Run(ctx, opts, loopreview.DefaultDeps())
		}
	}
	if opts.RiskGate == nil {
		opts.RiskGate = EvaluateRiskGate
	}
	if opts.StatePush == nil {
		opts.StatePush = func(ctx context.Context, opts statebranch.PushOptions) (statebranch.PushResult, error) {
			return statebranch.Push(ctx, opts, statebranch.DefaultDeps())
		}
	}
	if opts.LoadAttempts == nil {
		opts.LoadAttempts = state.LoadAttempts
	}
	if opts.ThrottleLimit <= 0 {
		opts.ThrottleLimit = 4
	}
	return opts
}

func runTickRiskGateAndPreProdMerge(ctx context.Context, opts TickOptions, tickReport *TickReport, item tickReviewCandidate, prNumber int) {
	gateReader, ok := opts.Reader.(RiskGateReader)
	if !ok {
		detail := "github reader does not support PR diff and check reads for risk gate"
		tickReport.RiskGates = append(tickReport.RiskGates, TickRiskGateResult{
			Issue:    item.Issue,
			PR:       item.PR,
			PRNumber: prNumber,
			Status:   RiskGateStatusNeedsHuman,
			Error:    detail,
		})
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "risk-gate",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: detail,
		})
		return
	}

	decision, err := opts.RiskGate(ctx, RiskGateOptions{
		Reader:             gateReader,
		PRNumber:           prNumber,
		RequiredChecks:     opts.RequiredChecks,
		AdditionalRedLines: opts.AdditionalRiskRedLines,
	})
	gateResult := TickRiskGateResult{
		Issue:          item.Issue,
		PR:             item.PR,
		PRNumber:       prNumber,
		Status:         decision.Status,
		RequiredChecks: append([]string(nil), decision.RequiredChecks...),
		ChangedFiles:   append([]string(nil), decision.ChangedFiles...),
		Checks:         append([]gh.Check(nil), decision.Checks...),
		RedLines:       append([]RiskRedLine(nil), decision.RedLines...),
	}
	if err != nil {
		gateResult.Status = RiskGateStatusNeedsHuman
		gateResult.Error = err.Error()
		tickReport.RiskGates = append(tickReport.RiskGates, gateResult)
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "risk-gate",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: err.Error(),
		})
		return
	}
	tickReport.RiskGates = append(tickReport.RiskGates, gateResult)
	if decision.Status != RiskGateStatusClean || len(decision.RedLines) > 0 {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "risk-gate",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: formatRiskRedLines(decision.RedLines),
		})
		return
	}

	if detail := preProdBranchProblem(opts.PreProdBranch, opts.BaseBranch); detail != "" {
		tickReport.PreProdMerges = append(tickReport.PreProdMerges, TickPreProdMergeResult{
			Issue:    item.Issue,
			PR:       item.PR,
			PRNumber: prNumber,
			Branch:   opts.PreProdBranch,
			Status:   TickStatusNeedsHuman,
			Error:    detail,
		})
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "pre-prod-merge",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: detail,
		})
		return
	}
	if opts.PreProdWriter == nil {
		detail := "pre-prod writer is required for unattended pre-prod merge"
		tickReport.PreProdMerges = append(tickReport.PreProdMerges, TickPreProdMergeResult{
			Issue:    item.Issue,
			PR:       item.PR,
			PRNumber: prNumber,
			Branch:   opts.PreProdBranch,
			Status:   TickStatusNeedsHuman,
			Error:    detail,
		})
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "pre-prod-merge",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: detail,
		})
		return
	}

	merged, err := opts.PreProdWriter.MergeToPreProd(ctx, prNumber, opts.PreProdBranch)
	mergeResult := TickPreProdMergeResult{
		Issue:    item.Issue,
		PR:       item.PR,
		PRNumber: prNumber,
		Branch:   opts.PreProdBranch,
		Status:   TickStatusSucceeded,
	}
	if err != nil {
		mergeResult.Status = TickStatusNeedsHuman
		mergeResult.Error = err.Error()
		tickReport.PreProdMerges = append(tickReport.PreProdMerges, mergeResult)
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "pre-prod-merge",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: err.Error(),
		})
		return
	}
	mergeResult.Branch = firstNonEmpty(merged.Branch, opts.PreProdBranch)
	mergeResult.Head = merged.Head
	mergeResult.SHA = merged.SHA
	mergeResult.URL = merged.URL
	tickReport.PreProdMerges = append(tickReport.PreProdMerges, mergeResult)
}

func preProdBranchProblem(preProdBranch, baseBranch string) string {
	branch := strings.TrimSpace(preProdBranch)
	if branch == "" {
		return "pre-prod branch is not configured"
	}
	switch strings.ToLower(branch) {
	case "main", "master", "prod", "production":
		return fmt.Sprintf("pre-prod branch %q is reserved for human promotion", branch)
	}
	if strings.EqualFold(branch, strings.TrimSpace(baseBranch)) {
		return fmt.Sprintf("pre-prod branch %q must differ from base branch %q", branch, strings.TrimSpace(baseBranch))
	}
	return ""
}

func formatRiskRedLines(lines []RiskRedLine) string {
	if len(lines) == 0 {
		return "risk gate returned needs-human"
	}
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		category := strings.TrimSpace(line.Category)
		detail := strings.TrimSpace(line.Detail)
		if category == "" {
			category = RiskRedLineRaised
		}
		if detail == "" {
			detail = "risk raised"
		}
		parts = append(parts, category+": "+detail)
	}
	return strings.Join(parts, "; ")
}

func tickNeedsHumanStopReason(items []TickIssue) string {
	for _, item := range items {
		if item.Step == "risk-gate" || item.Step == "pre-prod-merge" {
			return TickStopRiskGateNeedsHuman
		}
	}
	return TickStopReviewNeedsHuman
}

func pushTickState(ctx context.Context, opts TickOptions, tickReport *TickReport) {
	result, err := opts.StatePush(ctx, statebranch.PushOptions{
		RepoPath: opts.RepoPath,
		RunID:    opts.RunID,
	})
	statePush := TickStatePush{
		Branch:    result.Branch,
		Remote:    result.Remote,
		Committed: result.Committed,
		Pushed:    result.Pushed,
		PushError: result.PushError,
		Files:     append([]string(nil), result.Files...),
	}
	if err != nil {
		statePush.Error = err.Error()
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "state-push", Detail: err.Error()})
	} else if strings.TrimSpace(result.PushError) != "" {
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "state-push", Detail: result.PushError})
	}
	tickReport.StatePush = &statePush
}

func MarshalTickJSON(report TickReport) ([]byte, error) {
	report = normalizeTickReport(report)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal tick JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func RenderTickText(report TickReport) string {
	report = normalizeTickReport(report)
	var out bytes.Buffer

	fmt.Fprintln(&out, "TICK")
	fmt.Fprintf(&out, "Repo: %s\n", report.Repo)
	fmt.Fprintf(&out, "Base branch: %s\n", report.BaseBranch)
	fmt.Fprintf(&out, "Pre-prod branch: %s\n", report.PreProdBranch)
	fmt.Fprintf(&out, "RunId: %s\n", report.RunID)
	fmt.Fprintf(&out, "Status: %s\n", report.Status)
	fmt.Fprintf(&out, "Stop reason: %s\n", report.StopReason)
	fmt.Fprintf(&out, "Started at: %s\n", report.StartedAt)
	fmt.Fprintf(&out, "Finished at: %s\n", report.FinishedAt)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Compile")
	fmt.Fprintf(&out, "- created=%d updated=%d unchanged=%d closed=%d plan_approval_required=%s\n",
		report.Summary.CompiledCreatedCount,
		report.Summary.CompiledUpdatedCount,
		report.Summary.CompiledUnchangedCount,
		report.Summary.CompiledClosedCount,
		tickYesNo(report.Compile.PlanApprovalRequired),
	)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Ready set")
	fmt.Fprintf(&out, "- ready=%d blocked=%d\n", report.Summary.ReadyCount, report.Summary.BlockedCount)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Dispatch")
	if report.DispatchWave == nil || len(report.DispatchWave.Results) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, result := range report.DispatchWave.Results {
			fmt.Fprintf(&out, "- #%d %s\n", result.Issue, result.Status)
			if strings.TrimSpace(result.PR) != "" {
				fmt.Fprintf(&out, "  pr: %s\n", result.PR)
			}
			if strings.TrimSpace(result.Branch) != "" {
				fmt.Fprintf(&out, "  branch: %s\n", result.Branch)
			}
			if strings.TrimSpace(result.Error) != "" {
				fmt.Fprintf(&out, "  detail: %s\n", result.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Reviews")
	if len(report.Reviews) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, review := range report.Reviews {
			target := review.PR
			if review.PRNumber > 0 {
				target = fmt.Sprintf("#%d", review.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s\n", target, review.Issue, review.Verdict)
			if strings.TrimSpace(review.SpecConformance) != "" {
				fmt.Fprintf(&out, "  spec_conformance: %s\n", review.SpecConformance)
			}
			if strings.TrimSpace(review.Evidence) != "" {
				fmt.Fprintf(&out, "  evidence: %s\n", review.Evidence)
			}
			if strings.TrimSpace(review.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", review.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Risk gates")
	if len(report.RiskGates) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, gate := range report.RiskGates {
			target := gate.PR
			if gate.PRNumber > 0 {
				target = fmt.Sprintf("#%d", gate.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s\n", target, gate.Issue, gate.Status)
			if len(gate.RequiredChecks) > 0 {
				fmt.Fprintf(&out, "  required_checks: %s\n", strings.Join(gate.RequiredChecks, ", "))
			}
			if len(gate.ChangedFiles) > 0 {
				fmt.Fprintf(&out, "  changed_files: %s\n", strings.Join(gate.ChangedFiles, ", "))
			}
			if len(gate.RedLines) > 0 {
				fmt.Fprintf(&out, "  red_lines: %s\n", formatRiskRedLines(gate.RedLines))
			}
			if strings.TrimSpace(gate.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", gate.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Pre-prod merges")
	if len(report.PreProdMerges) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, merged := range report.PreProdMerges {
			target := merged.PR
			if merged.PRNumber > 0 {
				target = fmt.Sprintf("#%d", merged.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s branch=%s\n", target, merged.Issue, merged.Status, merged.Branch)
			if strings.TrimSpace(merged.Head) != "" {
				fmt.Fprintf(&out, "  head: %s\n", merged.Head)
			}
			if strings.TrimSpace(merged.SHA) != "" {
				fmt.Fprintf(&out, "  sha: %s\n", merged.SHA)
			}
			if strings.TrimSpace(merged.URL) != "" {
				fmt.Fprintf(&out, "  url: %s\n", merged.URL)
			}
			if strings.TrimSpace(merged.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", merged.Error)
			}
		}
	}

	renderTickIssueSection(&out, "Needs human", report.NeedsHuman)
	renderTickIssueSection(&out, "Failures", report.Failures)

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "State")
	if report.StatePush == nil {
		fmt.Fprintln(&out, "- not pushed")
	} else {
		fmt.Fprintf(&out, "- branch=%s remote=%s committed=%t pushed=%t files=%d\n",
			report.StatePush.Branch,
			report.StatePush.Remote,
			report.StatePush.Committed,
			report.StatePush.Pushed,
			len(report.StatePush.Files),
		)
		if strings.TrimSpace(report.StatePush.PushError) != "" {
			fmt.Fprintf(&out, "  push_error: %s\n", report.StatePush.PushError)
		}
		if strings.TrimSpace(report.StatePush.Error) != "" {
			fmt.Fprintf(&out, "  error: %s\n", report.StatePush.Error)
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Next")
	switch report.Status {
	case TickStatusSucceeded:
		fmt.Fprintln(&out, "- Clean passing PRs were integrated into pre-prod; human promotion to main remains separate.")
	case TickStatusNoReadyWork:
		fmt.Fprintln(&out, "- No ready issues were dispatched in this pass.")
	case TickStatusNeedsHuman:
		fmt.Fprintln(&out, "- Resolve the needs-human items before the next unattended pass.")
	default:
		fmt.Fprintln(&out, "- Fix or recover failed items before the next unattended pass.")
	}
	return out.String()
}

func TickExitCode(report TickReport) int {
	switch report.Status {
	case TickStatusSucceeded, TickStatusNoReadyWork:
		return 0
	case TickStatusNeedsHuman:
		return 2
	default:
		return 1
	}
}

func normalizeTickReport(report TickReport) TickReport {
	if report.Reviews == nil {
		report.Reviews = []TickReviewResult{}
	}
	for i := range report.Reviews {
		if report.Reviews[i].Findings == nil {
			report.Reviews[i].Findings = []loopreview.Finding{}
		}
	}
	if report.RiskGates == nil {
		report.RiskGates = []TickRiskGateResult{}
	}
	for i := range report.RiskGates {
		if report.RiskGates[i].RequiredChecks == nil {
			report.RiskGates[i].RequiredChecks = []string{}
		}
		if report.RiskGates[i].ChangedFiles == nil {
			report.RiskGates[i].ChangedFiles = []string{}
		}
		if report.RiskGates[i].Checks == nil {
			report.RiskGates[i].Checks = []gh.Check{}
		}
		if report.RiskGates[i].RedLines == nil {
			report.RiskGates[i].RedLines = []RiskRedLine{}
		}
	}
	if report.PreProdMerges == nil {
		report.PreProdMerges = []TickPreProdMergeResult{}
	}
	if report.NeedsHuman == nil {
		report.NeedsHuman = []TickIssue{}
	}
	if report.Failures == nil {
		report.Failures = []TickIssue{}
	}
	if report.StatePush != nil && report.StatePush.Files == nil {
		report.StatePush.Files = []string{}
	}
	report.Summary = summarizeTick(report)
	return report
}

func summarizeTick(report TickReport) TickSummary {
	summary := TickSummary{
		CompiledCreatedCount:   report.Compile.Summary.CreatedCount,
		CompiledUpdatedCount:   report.Compile.Summary.UpdatedCount,
		CompiledUnchangedCount: report.Compile.Summary.UnchangedCount,
		CompiledClosedCount:    report.Compile.Summary.ClosedCount,
		ReadyCount:             len(report.ReadySet.Ready),
		BlockedCount:           len(report.ReadySet.Blocked),
		NeedsHumanCount:        len(report.NeedsHuman),
		FailureCount:           len(report.Failures),
	}
	if report.DispatchWave != nil {
		for _, result := range report.DispatchWave.Results {
			if result.Status == DispatchWaveStatusSucceeded && strings.TrimSpace(result.PR) != "" {
				summary.DispatchedPRCount++
			}
		}
	}
	for _, review := range report.Reviews {
		switch review.Verdict {
		case loopreview.VerdictPass:
			summary.ReviewPassCount++
		case loopreview.VerdictFail:
			summary.ReviewFailCount++
		case loopreview.VerdictNeedsHuman:
			summary.ReviewNeedsHumanCount++
		}
	}
	for _, gate := range report.RiskGates {
		switch gate.Status {
		case RiskGateStatusClean:
			summary.RiskGateCleanCount++
		case RiskGateStatusNeedsHuman:
			summary.RiskGateNeedsHumanCount++
		}
	}
	for _, merged := range report.PreProdMerges {
		if merged.Status == TickStatusSucceeded {
			summary.PreProdMergeCount++
		}
	}
	return summary
}

func renderTickIssueSection(out *bytes.Buffer, title string, issues []TickIssue) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, title)
	if len(issues) == 0 {
		fmt.Fprintln(out, "- none")
		return
	}
	for _, item := range issues {
		target := item.Step
		if item.Issue > 0 {
			target += fmt.Sprintf(" #%d", item.Issue)
		}
		if strings.TrimSpace(item.PR) != "" {
			target += " " + item.PR
		}
		fmt.Fprintf(out, "- %s: %s\n", target, item.Detail)
	}
}

type tickReviewCandidate struct {
	Issue int
	PR    string
}

func reviewablePRs(results []DispatchWaveIssueResult) []tickReviewCandidate {
	out := make([]tickReviewCandidate, 0, len(results))
	for _, result := range results {
		if result.Status != DispatchWaveStatusSucceeded || strings.TrimSpace(result.PR) == "" {
			continue
		}
		out = append(out, tickReviewCandidate{Issue: result.Issue, PR: result.PR})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Issue != out[j].Issue {
			return out[i].Issue < out[j].Issue
		}
		return out[i].PR < out[j].PR
	})
	return out
}

var tickPRNumberPattern = regexp.MustCompile(`(?i)(?:/pull/|#)([1-9]\d*)\s*$`)

func parseTickPRNumber(value string) (int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if number, err := strconv.Atoi(value); err == nil && number > 0 {
		return number, true
	}
	matches := tickPRNumberPattern.FindStringSubmatch(value)
	if len(matches) != 2 {
		return 0, false
	}
	number, err := strconv.Atoi(matches[1])
	return number, err == nil && number > 0
}

func hasStatePushFailure(items []TickIssue) bool {
	for _, item := range items {
		if item.Step == "state-push" {
			return true
		}
	}
	return false
}

func tickYesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
