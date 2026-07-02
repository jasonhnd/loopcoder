package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
	TickStopPreProdNeedsHuman         = "pre-prod-needs-human"
	TickStopStatePushFailed           = "state-push-failed"
	TickStopCompileFailed             = "compile-failed"
	TickStopReadySetFailed            = "ready-set-failed"
	TickStopAttemptLoadFailed         = "attempt-load-failed"
	TickStopDispatchWaveFailed        = "dispatch-wave-failed"
	TickStopNoReviewablePRsDispatched = "no-reviewable-prs-dispatched"

	tickPendingPromotionEvent  = "tick.pending_promotion"
	tickStatusPendingPromotion = "pending-promotion"
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
	ConfiguredEvidence     []config.EvidenceArtifact
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
	Version          int                       `json:"version"`
	Repo             string                    `json:"repo"`
	RepoPath         string                    `json:"repo_path"`
	BaseBranch       string                    `json:"base_branch"`
	PreProdBranch    string                    `json:"pre_prod_branch"`
	RunID            string                    `json:"run_id"`
	Status           string                    `json:"status"`
	StopReason       string                    `json:"stop_reason"`
	StartedAt        string                    `json:"started_at"`
	FinishedAt       string                    `json:"finished_at"`
	Compile          compiler.Report           `json:"compile"`
	ReadySet         report.ReadySetReport     `json:"ready_set"`
	DispatchWave     *DispatchWaveReport       `json:"dispatch_wave,omitempty"`
	Reviews          []TickReviewResult        `json:"reviews"`
	RiskGates        []TickRiskGateResult      `json:"risk_gates"`
	PreProdMerges    []TickPreProdMergeResult  `json:"pre_prod_merges"`
	PreProdHealth    []TickPreProdHealthResult `json:"pre_prod_health"`
	PreProdReverts   []TickPreProdRevertResult `json:"pre_prod_reverts"`
	PendingPromotion []TickPendingPromotion    `json:"pending_promotion,omitempty"`
	NeedsHuman       []TickIssue               `json:"needs_human"`
	Failures         []TickIssue               `json:"failures"`
	StatePush        *TickStatePush            `json:"state_push,omitempty"`
	Summary          TickSummary               `json:"summary"`
}

type TickReviewResult struct {
	Issue              int                            `json:"issue,omitempty"`
	PR                 string                         `json:"pr,omitempty"`
	PRNumber           int                            `json:"pr_number,omitempty"`
	Verdict            string                         `json:"verdict"`
	SpecConformance    string                         `json:"spec_conformance,omitempty"`
	Evidence           string                         `json:"evidence,omitempty"`
	ConfiguredEvidence []config.EvidenceArtifact      `json:"configured_evidence,omitempty"`
	Findings           []loopreview.Finding           `json:"findings"`
	Error              string                         `json:"error,omitempty"`
	Attestation        *attestation.AttestationRecord `json:"attestation,omitempty"`
}

type TickIssue struct {
	Step               string                    `json:"step"`
	Issue              int                       `json:"issue,omitempty"`
	PR                 string                    `json:"pr,omitempty"`
	Detail             string                    `json:"detail"`
	ConfiguredEvidence []config.EvidenceArtifact `json:"configured_evidence,omitempty"`
}

type TickPendingPromotion struct {
	RunID              string                    `json:"run_id,omitempty"`
	Issue              int                       `json:"issue,omitempty"`
	PR                 string                    `json:"pr,omitempty"`
	PRNumber           int                       `json:"pr_number,omitempty"`
	Branch             string                    `json:"branch,omitempty"`
	Head               string                    `json:"head,omitempty"`
	SHA                string                    `json:"sha,omitempty"`
	URL                string                    `json:"url,omitempty"`
	Status             string                    `json:"status,omitempty"`
	Evidence           string                    `json:"evidence,omitempty"`
	ConfiguredEvidence []config.EvidenceArtifact `json:"configured_evidence,omitempty"`
}

type TickRiskGateResult struct {
	Issue              int                       `json:"issue,omitempty"`
	PR                 string                    `json:"pr,omitempty"`
	PRNumber           int                       `json:"pr_number,omitempty"`
	Status             string                    `json:"status"`
	RequiredChecks     []string                  `json:"required_checks"`
	ChangedFiles       []string                  `json:"changed_files"`
	Checks             []gh.Check                `json:"checks"`
	RedLines           []RiskRedLine             `json:"red_lines"`
	Error              string                    `json:"error,omitempty"`
	ConfiguredEvidence []config.EvidenceArtifact `json:"configured_evidence,omitempty"`
}

type TickPreProdMergeResult struct {
	Issue              int                       `json:"issue,omitempty"`
	PR                 string                    `json:"pr,omitempty"`
	PRNumber           int                       `json:"pr_number,omitempty"`
	Branch             string                    `json:"branch"`
	Head               string                    `json:"head,omitempty"`
	SHA                string                    `json:"sha,omitempty"`
	URL                string                    `json:"url,omitempty"`
	Status             string                    `json:"status"`
	Error              string                    `json:"error,omitempty"`
	ConfiguredEvidence []config.EvidenceArtifact `json:"configured_evidence,omitempty"`
}

type TickPreProdHealthResult struct {
	Issue              int                       `json:"issue,omitempty"`
	PR                 string                    `json:"pr,omitempty"`
	PRNumber           int                       `json:"pr_number,omitempty"`
	Branch             string                    `json:"branch"`
	HeadSHA            string                    `json:"head_sha,omitempty"`
	MergeSHA           string                    `json:"merge_sha,omitempty"`
	Status             string                    `json:"status"`
	RequiredChecks     []string                  `json:"required_checks"`
	Checks             []gh.Check                `json:"checks"`
	Problems           []string                  `json:"problems"`
	Error              string                    `json:"error,omitempty"`
	ConfiguredEvidence []config.EvidenceArtifact `json:"configured_evidence,omitempty"`
}

type TickPreProdRevertResult struct {
	Issue              int                       `json:"issue,omitempty"`
	PR                 string                    `json:"pr,omitempty"`
	PRNumber           int                       `json:"pr_number,omitempty"`
	Branch             string                    `json:"branch"`
	RevertedSHA        string                    `json:"reverted_sha,omitempty"`
	SHA                string                    `json:"sha,omitempty"`
	URL                string                    `json:"url,omitempty"`
	Status             string                    `json:"status"`
	Error              string                    `json:"error,omitempty"`
	ConfiguredEvidence []config.EvidenceArtifact `json:"configured_evidence,omitempty"`
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
	PreProdRevertCount      int `json:"pre_prod_revert_count"`
	PendingPromotionCount   int `json:"pending_promotion_count,omitempty"`
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
		Version:        TickReportVersion,
		Repo:           filepath.ToSlash(opts.RepoPath),
		RepoPath:       filepath.ToSlash(opts.RepoPath),
		BaseBranch:     opts.BaseBranch,
		PreProdBranch:  opts.PreProdBranch,
		RunID:          opts.RunID,
		StartedAt:      state.FormatTimestamp(started),
		Reviews:        []TickReviewResult{},
		RiskGates:      []TickRiskGateResult{},
		PreProdMerges:  []TickPreProdMergeResult{},
		PreProdHealth:  []TickPreProdHealthResult{},
		PreProdReverts: []TickPreProdRevertResult{},
		NeedsHuman:     []TickIssue{},
		Failures:       []TickIssue{},
	}
	finish := func(status, stopReason string) (TickReport, error) {
		finished := opts.Clock().UTC()
		tickReport.Status = status
		tickReport.StopReason = stopReason
		tickReport.FinishedAt = state.FormatTimestamp(finished)
		attachTickConfiguredEvidence(&tickReport, opts.ConfiguredEvidence)
		tickReport.PendingPromotion = loadTickPendingPromotionLedger(opts.RepoPath, opts.PreProdBranch)
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
	runTickPreProdKeepsGreen(ctx, opts, tickReport, item, prNumber, mergeResult)
}

func runTickPreProdKeepsGreen(ctx context.Context, opts TickOptions, tickReport *TickReport, item tickReviewCandidate, prNumber int, mergeResult TickPreProdMergeResult) {
	healthReader, ok := opts.Reader.(PreProdHealthReader)
	if !ok {
		detail := "github reader does not support pre-prod branch CI status reads"
		tickReport.PreProdHealth = append(tickReport.PreProdHealth, TickPreProdHealthResult{
			Issue:    item.Issue,
			PR:       item.PR,
			PRNumber: prNumber,
			Branch:   opts.PreProdBranch,
			MergeSHA: mergeResult.SHA,
			Status:   PreProdHealthStatusUnknown,
			Error:    detail,
		})
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "pre-prod-health",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: detail,
		})
		return
	}
	if strings.TrimSpace(mergeResult.SHA) == "" {
		detail := "pre-prod merge did not return a merge commit SHA"
		tickReport.PreProdHealth = append(tickReport.PreProdHealth, TickPreProdHealthResult{
			Issue:    item.Issue,
			PR:       item.PR,
			PRNumber: prNumber,
			Branch:   opts.PreProdBranch,
			Status:   PreProdHealthStatusUnknown,
			Error:    detail,
		})
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "pre-prod-health",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: detail,
		})
		return
	}

	branchChecks, err := healthReader.BranchChecks(ctx, opts.PreProdBranch)
	health := TickPreProdHealthResult{
		Issue:          item.Issue,
		PR:             item.PR,
		PRNumber:       prNumber,
		Branch:         firstNonEmpty(branchChecks.Branch, opts.PreProdBranch),
		HeadSHA:        branchChecks.HeadSHA,
		MergeSHA:       mergeResult.SHA,
		RequiredChecks: normalizeRequiredChecks(opts.RequiredChecks),
		Checks:         append([]gh.Check(nil), branchChecks.Checks...),
		Problems:       []string{},
	}
	if err != nil {
		health.Status = PreProdHealthStatusUnknown
		health.Error = err.Error()
		tickReport.PreProdHealth = append(tickReport.PreProdHealth, health)
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "pre-prod-health",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: err.Error(),
		})
		return
	}
	health.Status, health.Problems = preProdHealthStatus(health.RequiredChecks, health.Checks)
	tickReport.PreProdHealth = append(tickReport.PreProdHealth, health)
	switch health.Status {
	case PreProdHealthStatusGreen:
		return
	case PreProdHealthStatusRed:
		if sameGitSHA(health.HeadSHA, mergeResult.SHA) {
			runTickPreProdRevert(ctx, opts, tickReport, item, prNumber, mergeResult, health)
			return
		}
		detail := fmt.Sprintf("pre-prod CI is red at %s, not the just-merged commit %s", firstNonEmpty(health.HeadSHA, "unknown"), mergeResult.SHA)
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "pre-prod-health",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: detail,
		})
	default:
		detail := "pre-prod CI is not green: " + strings.Join(health.Problems, ", ")
		if len(health.Problems) == 0 {
			detail = "pre-prod CI status is " + health.Status
		}
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "pre-prod-health",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: detail,
		})
	}
}

func runTickPreProdRevert(ctx context.Context, opts TickOptions, tickReport *TickReport, item tickReviewCandidate, prNumber int, mergeResult TickPreProdMergeResult, health TickPreProdHealthResult) {
	reverted, err := opts.PreProdWriter.RevertOnPreProd(ctx, prNumber, opts.PreProdBranch, mergeResult.SHA)
	revertResult := TickPreProdRevertResult{
		Issue:       item.Issue,
		PR:          item.PR,
		PRNumber:    prNumber,
		Branch:      opts.PreProdBranch,
		RevertedSHA: mergeResult.SHA,
		Status:      TickStatusSucceeded,
	}
	if err != nil {
		revertResult.Status = TickStatusFailed
		revertResult.Error = err.Error()
		tickReport.PreProdReverts = append(tickReport.PreProdReverts, revertResult)
		tickReport.Failures = append(tickReport.Failures, TickIssue{
			Step:   "pre-prod-revert",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: err.Error(),
		})
		return
	}
	revertResult.Branch = firstNonEmpty(reverted.Branch, opts.PreProdBranch)
	revertResult.RevertedSHA = firstNonEmpty(reverted.RevertedSHA, mergeResult.SHA)
	revertResult.SHA = reverted.SHA
	revertResult.URL = reverted.URL
	tickReport.PreProdReverts = append(tickReport.PreProdReverts, revertResult)

	detail := fmt.Sprintf("pre-prod CI red after merge %s; reverted on %s", mergeResult.SHA, revertResult.Branch)
	if len(health.Problems) > 0 {
		detail += ": " + strings.Join(health.Problems, ", ")
	}
	tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
		Step:   "pre-prod-revert",
		Issue:  item.Issue,
		PR:     item.PR,
		Detail: detail,
	})
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

func preProdHealthStatus(requiredChecks []string, checks []gh.Check) (string, []string) {
	if len(requiredChecks) == 0 {
		return PreProdHealthStatusUnknown, []string{"no required CI checks configured for pre-prod health"}
	}

	byName := map[string]gh.Check{}
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			continue
		}
		if existing, ok := byName[name]; ok && checkFailed(existing) {
			continue
		}
		byName[name] = check
	}

	status := PreProdHealthStatusGreen
	var problems []string
	for _, requiredCheck := range requiredChecks {
		check, ok := byName[requiredCheck]
		if !ok {
			if status == PreProdHealthStatusGreen {
				status = PreProdHealthStatusPending
			}
			problems = append(problems, requiredCheck+" missing")
			continue
		}
		if checkPassed(check) {
			continue
		}
		problems = append(problems, fmt.Sprintf("%s %s", requiredCheck, checkStatus(check)))
		if checkFailed(check) {
			status = PreProdHealthStatusRed
		} else if status == PreProdHealthStatusGreen {
			status = PreProdHealthStatusPending
		}
	}
	return status, problems
}

func sameGitSHA(a, b string) bool {
	left := strings.ToLower(strings.TrimSpace(a))
	right := strings.ToLower(strings.TrimSpace(b))
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	if len(left) >= 7 && strings.HasPrefix(right, left) {
		return true
	}
	return len(right) >= 7 && strings.HasPrefix(left, right)
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
		if item.Step == "pre-prod-health" || item.Step == "pre-prod-revert" {
			return TickStopPreProdNeedsHuman
		}
	}
	return TickStopReviewNeedsHuman
}

func pushTickState(ctx context.Context, opts TickOptions, tickReport *TickReport) {
	if err := recordTickPendingPromotionLedger(opts, tickReport); err != nil {
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "state-push", Detail: err.Error()})
	}
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
	fmt.Fprintln(&out, "Pending promotion")
	if len(report.PendingPromotion) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, item := range report.PendingPromotion {
			fmt.Fprintf(&out, "- %s %s", formatTickPendingPromotionTarget(item), firstNonEmpty(item.Status, tickStatusPendingPromotion))
			if strings.TrimSpace(item.Branch) != "" {
				fmt.Fprintf(&out, " branch=%s", item.Branch)
			}
			fmt.Fprintln(&out)
			if strings.TrimSpace(item.RunID) != "" {
				fmt.Fprintf(&out, "  run_id: %s\n", item.RunID)
			}
			if strings.TrimSpace(item.Head) != "" {
				fmt.Fprintf(&out, "  head: %s\n", item.Head)
			}
			if strings.TrimSpace(item.SHA) != "" {
				fmt.Fprintf(&out, "  sha: %s\n", item.SHA)
			}
			if strings.TrimSpace(item.URL) != "" {
				fmt.Fprintf(&out, "  url: %s\n", item.URL)
			}
			if strings.TrimSpace(item.Evidence) != "" {
				fmt.Fprintf(&out, "  evidence: %s\n", item.Evidence)
			}
			renderTickConfiguredEvidence(&out, item.ConfiguredEvidence)
		}
	}

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
			renderTickConfiguredEvidence(&out, result.ConfiguredEvidence)
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
			renderTickConfiguredEvidence(&out, review.ConfiguredEvidence)
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
			renderTickConfiguredEvidence(&out, gate.ConfiguredEvidence)
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
			renderTickConfiguredEvidence(&out, merged.ConfiguredEvidence)
			if strings.TrimSpace(merged.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", merged.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Pre-prod health")
	if len(report.PreProdHealth) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, health := range report.PreProdHealth {
			target := health.PR
			if health.PRNumber > 0 {
				target = fmt.Sprintf("#%d", health.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s branch=%s\n", target, health.Issue, health.Status, health.Branch)
			if strings.TrimSpace(health.HeadSHA) != "" {
				fmt.Fprintf(&out, "  head_sha: %s\n", health.HeadSHA)
			}
			if strings.TrimSpace(health.MergeSHA) != "" {
				fmt.Fprintf(&out, "  merge_sha: %s\n", health.MergeSHA)
			}
			if len(health.RequiredChecks) > 0 {
				fmt.Fprintf(&out, "  required_checks: %s\n", strings.Join(health.RequiredChecks, ", "))
			}
			if len(health.Problems) > 0 {
				fmt.Fprintf(&out, "  problems: %s\n", strings.Join(health.Problems, ", "))
			}
			renderTickConfiguredEvidence(&out, health.ConfiguredEvidence)
			if strings.TrimSpace(health.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", health.Error)
			}
		}
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Pre-prod reverts")
	if len(report.PreProdReverts) == 0 {
		fmt.Fprintln(&out, "- none")
	} else {
		for _, reverted := range report.PreProdReverts {
			target := reverted.PR
			if reverted.PRNumber > 0 {
				target = fmt.Sprintf("#%d", reverted.PRNumber)
			}
			fmt.Fprintf(&out, "- PR %s issue #%d %s branch=%s\n", target, reverted.Issue, reverted.Status, reverted.Branch)
			if strings.TrimSpace(reverted.RevertedSHA) != "" {
				fmt.Fprintf(&out, "  reverted_sha: %s\n", reverted.RevertedSHA)
			}
			if strings.TrimSpace(reverted.SHA) != "" {
				fmt.Fprintf(&out, "  sha: %s\n", reverted.SHA)
			}
			if strings.TrimSpace(reverted.URL) != "" {
				fmt.Fprintf(&out, "  url: %s\n", reverted.URL)
			}
			renderTickConfiguredEvidence(&out, reverted.ConfiguredEvidence)
			if strings.TrimSpace(reverted.Error) != "" {
				fmt.Fprintf(&out, "  error: %s\n", reverted.Error)
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
	if report.PreProdHealth == nil {
		report.PreProdHealth = []TickPreProdHealthResult{}
	}
	for i := range report.PreProdHealth {
		if report.PreProdHealth[i].RequiredChecks == nil {
			report.PreProdHealth[i].RequiredChecks = []string{}
		}
		if report.PreProdHealth[i].Checks == nil {
			report.PreProdHealth[i].Checks = []gh.Check{}
		}
		if report.PreProdHealth[i].Problems == nil {
			report.PreProdHealth[i].Problems = []string{}
		}
	}
	if report.PreProdReverts == nil {
		report.PreProdReverts = []TickPreProdRevertResult{}
	}
	report.PendingPromotion = normalizeTickPendingPromotionItems(report.PendingPromotion, report.PreProdBranch, "")
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
		PendingPromotionCount:  len(report.PendingPromotion),
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
	for _, reverted := range report.PreProdReverts {
		if reverted.Status == TickStatusSucceeded {
			summary.PreProdRevertCount++
		}
	}
	return summary
}

func attachTickConfiguredEvidence(report *TickReport, evidence []config.EvidenceArtifact) {
	evidence = normalizeConfiguredEvidence(evidence)
	if len(evidence) == 0 {
		return
	}
	if report.DispatchWave != nil {
		for i := range report.DispatchWave.Results {
			result := &report.DispatchWave.Results[i]
			if tickHasReportTarget(result.Issue, result.PR) {
				result.ConfiguredEvidence = copyConfiguredEvidence(evidence)
			}
		}
	}
	for i := range report.Reviews {
		item := &report.Reviews[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.RiskGates {
		item := &report.RiskGates[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.PreProdMerges {
		item := &report.PreProdMerges[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.PreProdHealth {
		item := &report.PreProdHealth[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.PreProdReverts {
		item := &report.PreProdReverts[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.NeedsHuman {
		item := &report.NeedsHuman[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
	for i := range report.Failures {
		item := &report.Failures[i]
		if tickHasReportTarget(item.Issue, item.PR) {
			item.ConfiguredEvidence = copyConfiguredEvidence(evidence)
		}
	}
}

func normalizeConfiguredEvidence(evidence []config.EvidenceArtifact) []config.EvidenceArtifact {
	out := make([]config.EvidenceArtifact, 0, len(evidence))
	for _, item := range evidence {
		item.ProjectType = strings.TrimSpace(item.ProjectType)
		item.PreviewURL = strings.TrimSpace(item.PreviewURL)
		item.ExampleOutput = strings.TrimSpace(item.ExampleOutput)
		item.TestResults = strings.TrimSpace(item.TestResults)
		item.PreviewBuild = strings.TrimSpace(item.PreviewBuild)
		if item.ProjectType == "" || tickConfiguredEvidenceEmpty(item) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func tickConfiguredEvidenceEmpty(item config.EvidenceArtifact) bool {
	return strings.TrimSpace(item.PreviewURL) == "" &&
		strings.TrimSpace(item.ExampleOutput) == "" &&
		strings.TrimSpace(item.TestResults) == "" &&
		strings.TrimSpace(item.PreviewBuild) == ""
}

func copyConfiguredEvidence(evidence []config.EvidenceArtifact) []config.EvidenceArtifact {
	return append([]config.EvidenceArtifact(nil), evidence...)
}

func tickHasReportTarget(issue int, pr string) bool {
	return issue > 0 || strings.TrimSpace(pr) != ""
}

func renderTickConfiguredEvidence(out *bytes.Buffer, evidence []config.EvidenceArtifact) {
	evidence = normalizeConfiguredEvidence(evidence)
	for _, item := range evidence {
		parts := make([]string, 0, 4)
		if item.PreviewURL != "" {
			parts = append(parts, "preview_url="+formatTickEvidenceValue(item.PreviewURL))
		}
		if item.ExampleOutput != "" {
			parts = append(parts, "example_output="+formatTickEvidenceValue(item.ExampleOutput))
		}
		if item.TestResults != "" {
			parts = append(parts, "test_results="+formatTickEvidenceValue(item.TestResults))
		}
		if item.PreviewBuild != "" {
			parts = append(parts, "preview_build="+formatTickEvidenceValue(item.PreviewBuild))
		}
		if len(parts) == 0 {
			continue
		}
		fmt.Fprintf(out, "  configured_evidence: %s %s\n", item.ProjectType, strings.Join(parts, " "))
	}
}

func formatTickEvidenceValue(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
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
		renderTickConfiguredEvidence(out, item.ConfiguredEvidence)
	}
}

type tickPendingPromotionLedgerDetails struct {
	Version          int                    `json:"version,omitempty"`
	Repo             string                 `json:"repo,omitempty"`
	RepoPath         string                 `json:"repo_path,omitempty"`
	RunID            string                 `json:"run_id,omitempty"`
	PreProdBranch    string                 `json:"pre_prod_branch,omitempty"`
	PendingPromotion []TickPendingPromotion `json:"pending_promotion,omitempty"`
}

type tickLedgerEvent struct {
	Timestamp string          `json:"ts"`
	RunID     string          `json:"run_id"`
	Event     string          `json:"event,omitempty"`
	Outcome   string          `json:"outcome,omitempty"`
	Status    string          `json:"status,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
}

type tickLedgerRecord struct {
	event tickLedgerEvent
	path  string
	index int
	line  string
}

func recordTickPendingPromotionLedger(opts TickOptions, report *TickReport) error {
	attachTickConfiguredEvidence(report, opts.ConfiguredEvidence)
	pending := tickPendingPromotionFromReport(*report)
	if len(pending) == 0 {
		return nil
	}
	detailsJSON, err := json.Marshal(tickPendingPromotionLedgerDetails{
		Version:          1,
		Repo:             report.Repo,
		RepoPath:         filepath.ToSlash(opts.RepoPath),
		RunID:            report.RunID,
		PreProdBranch:    report.PreProdBranch,
		PendingPromotion: pending,
	})
	if err != nil {
		return fmt.Errorf("marshal pending-promotion ledger event: %w", err)
	}
	timestamp := firstNonEmpty(report.FinishedAt, report.StartedAt)
	if err := state.AppendEvent(opts.RepoPath, report.RunID, state.Event{
		Timestamp: timestamp,
		RunID:     report.RunID,
		JobID:     "tick",
		Issue:     0,
		Phase:     "report",
		Status:    tickStatusPendingPromotion,
		LogBytes:  0,
		Event:     tickPendingPromotionEvent,
		Outcome:   tickStatusPendingPromotion,
		Details:   json.RawMessage(detailsJSON),
	}); err != nil {
		return fmt.Errorf("append pending-promotion ledger event: %w", err)
	}
	return nil
}

func loadTickPendingPromotionLedger(repoPath, preProdBranch string) []TickPendingPromotion {
	records := readTickLedgerRecords(repoPath)
	if len(records) == 0 {
		return nil
	}

	pending := map[string]TickPendingPromotion{}
	for _, record := range records {
		eventName := strings.TrimSpace(record.event.Event)
		switch eventName {
		case tickPendingPromotionEvent:
			var details tickPendingPromotionLedgerDetails
			if !unmarshalTickLedgerDetails(record.event.Details, &details) {
				continue
			}
			if !sameTickPreProdBranch(firstNonEmpty(details.PreProdBranch, preProdBranch), preProdBranch) {
				continue
			}
			runID := firstNonEmpty(details.RunID, record.event.RunID)
			for _, item := range normalizeTickPendingPromotionItems(details.PendingPromotion, firstNonEmpty(details.PreProdBranch, preProdBranch), runID) {
				pending[tickPendingPromotionKey(item)] = item
			}
		case promoteLedgerEvent:
			var report PromoteReport
			if !unmarshalTickLedgerDetails(record.event.Details, &report) {
				continue
			}
			if !sameTickPreProdBranch(report.PreProdBranch, preProdBranch) {
				continue
			}
			if tickPromoteClearsPending(record.event, report) {
				pending = map[string]TickPendingPromotion{}
				continue
			}
			for _, kicked := range report.KickedBack {
				if kicked.Status == PromoteStatusSucceeded {
					removeTickPendingPromotion(pending, kicked)
				}
			}
		}
	}

	out := make([]TickPendingPromotion, 0, len(pending))
	for _, item := range pending {
		out = append(out, item)
	}
	sortTickPendingPromotion(out)
	return out
}

func readTickLedgerRecords(repoPath string) []tickLedgerRecord {
	paths := tickLedgerEventPaths(repoPath)
	if len(paths) == 0 {
		return nil
	}
	seenLines := map[string]bool{}
	records := make([]tickLedgerRecord, 0)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || seenLines[line] {
				continue
			}
			seenLines[line] = true
			var event tickLedgerEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				continue
			}
			records = append(records, tickLedgerRecord{event: event, path: path, index: i, line: line})
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		leftTime, leftOK := parseTickLedgerTimestamp(records[i].event.Timestamp)
		rightTime, rightOK := parseTickLedgerTimestamp(records[j].event.Timestamp)
		if leftOK && rightOK && !leftTime.Equal(rightTime) {
			return leftTime.Before(rightTime)
		}
		if leftOK != rightOK {
			return leftOK
		}
		if records[i].event.Timestamp != records[j].event.Timestamp {
			return records[i].event.Timestamp < records[j].event.Timestamp
		}
		if records[i].event.RunID != records[j].event.RunID {
			return records[i].event.RunID < records[j].event.RunID
		}
		if records[i].path != records[j].path {
			return records[i].path < records[j].path
		}
		return records[i].index < records[j].index
	})
	return records
}

func tickLedgerEventPaths(repoPath string) []string {
	roots := []string{state.RunsRoot(repoPath)}
	stateBranchRoot := filepath.Join(repoPath, ".loopcoder", "state-branch")
	if entries, err := os.ReadDir(stateBranchRoot); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				roots = append(roots, filepath.Join(stateBranchRoot, entry.Name(), "runs"))
			}
		}
	}

	seen := map[string]bool{}
	paths := make([]string, 0)
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(root, entry.Name(), "events.jsonl")
			if seen[path] {
				continue
			}
			seen[path] = true
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func parseTickLedgerTimestamp(value string) (time.Time, bool) {
	t, err := state.ParseTimestamp(value)
	return t, err == nil
}

func unmarshalTickLedgerDetails(data json.RawMessage, target any) bool {
	if len(bytes.TrimSpace(data)) == 0 {
		return false
	}
	return json.Unmarshal(data, target) == nil
}

func tickPromoteClearsPending(event tickLedgerEvent, report PromoteReport) bool {
	switch strings.TrimSpace(firstNonEmpty(event.Outcome, report.Promoted.Status)) {
	case PromoteOutcomePromoted, PromoteOutcomeSkippedAsDone:
		return true
	}
	return report.Status == PromoteStatusSucceeded &&
		(report.Promoted.Status == PromoteStatusSucceeded || report.Promoted.AlreadyUpToDate)
}

func removeTickPendingPromotion(pending map[string]TickPendingPromotion, kicked PromoteKickBackResult) {
	for key, item := range pending {
		if tickKickBackMatchesPending(kicked, item) {
			delete(pending, key)
		}
	}
}

func tickKickBackMatchesPending(kicked PromoteKickBackResult, item TickPendingPromotion) bool {
	if kicked.PRNumber > 0 && item.PRNumber > 0 {
		return kicked.PRNumber == item.PRNumber
	}
	kickItem := normalizeKickBackItem(kicked.Item)
	if kickItem != "" {
		if item.PRNumber > 0 && kickItem == fmt.Sprintf("#%d", item.PRNumber) {
			return true
		}
		if strings.EqualFold(kickItem, normalizeKickBackItem(item.PR)) {
			return true
		}
		if strings.EqualFold(kickItem, strings.TrimSpace(item.SHA)) || strings.EqualFold(kickItem, strings.TrimSpace(item.Head)) {
			return true
		}
	}
	if strings.TrimSpace(kicked.SHA) != "" && sameGitSHA(kicked.SHA, item.SHA) {
		return true
	}
	return strings.TrimSpace(kicked.RevertedSHA) != "" && sameGitSHA(kicked.RevertedSHA, item.SHA)
}

func tickPendingPromotionFromReport(report TickReport) []TickPendingPromotion {
	if len(report.PreProdMerges) == 0 {
		return nil
	}
	pending := make([]TickPendingPromotion, 0, len(report.PreProdMerges))
	for _, merged := range report.PreProdMerges {
		if merged.Status != TickStatusSucceeded || tickPreProdMergeReverted(report, merged) || !tickPreProdMergeReady(report, merged) {
			continue
		}
		item := TickPendingPromotion{
			RunID:              report.RunID,
			Issue:              merged.Issue,
			PR:                 merged.PR,
			PRNumber:           merged.PRNumber,
			Branch:             firstNonEmpty(merged.Branch, report.PreProdBranch),
			Head:               merged.Head,
			SHA:                merged.SHA,
			URL:                merged.URL,
			Status:             tickStatusPendingPromotion,
			ConfiguredEvidence: copyConfiguredEvidence(merged.ConfiguredEvidence),
		}
		if review, ok := tickReviewForPreProdMerge(report.Reviews, merged); ok {
			item.Evidence = review.Evidence
			if len(item.ConfiguredEvidence) == 0 {
				item.ConfiguredEvidence = copyConfiguredEvidence(review.ConfiguredEvidence)
			}
		}
		pending = append(pending, item)
	}
	return normalizeTickPendingPromotionItems(pending, report.PreProdBranch, report.RunID)
}

func tickPreProdMergeReverted(report TickReport, merged TickPreProdMergeResult) bool {
	for _, reverted := range report.PreProdReverts {
		if reverted.Status != TickStatusSucceeded {
			continue
		}
		if tickReportTargetsMatch(merged.Issue, merged.PR, merged.PRNumber, reverted.Issue, reverted.PR, reverted.PRNumber) {
			return true
		}
		if strings.TrimSpace(merged.SHA) != "" && sameGitSHA(merged.SHA, reverted.RevertedSHA) {
			return true
		}
	}
	return false
}

func tickPreProdMergeReady(report TickReport, merged TickPreProdMergeResult) bool {
	seen := false
	for _, health := range report.PreProdHealth {
		if !tickReportTargetsMatch(merged.Issue, merged.PR, merged.PRNumber, health.Issue, health.PR, health.PRNumber) {
			continue
		}
		seen = true
		if health.Status == PreProdHealthStatusGreen {
			return true
		}
	}
	return !seen
}

func tickReviewForPreProdMerge(reviews []TickReviewResult, merged TickPreProdMergeResult) (TickReviewResult, bool) {
	for _, review := range reviews {
		if tickReportTargetsMatch(merged.Issue, merged.PR, merged.PRNumber, review.Issue, review.PR, review.PRNumber) {
			return review, true
		}
	}
	return TickReviewResult{}, false
}

func normalizeTickPendingPromotionItems(items []TickPendingPromotion, preProdBranch, runID string) []TickPendingPromotion {
	if len(items) == 0 {
		return nil
	}
	out := make([]TickPendingPromotion, 0, len(items))
	for _, item := range items {
		item.RunID = strings.TrimSpace(firstNonEmpty(item.RunID, runID))
		item.PR = strings.TrimSpace(item.PR)
		item.Branch = strings.TrimSpace(firstNonEmpty(item.Branch, preProdBranch))
		item.Head = strings.TrimSpace(item.Head)
		item.SHA = strings.TrimSpace(item.SHA)
		item.URL = strings.TrimSpace(item.URL)
		item.Status = strings.TrimSpace(firstNonEmpty(item.Status, tickStatusPendingPromotion))
		item.Evidence = strings.TrimSpace(item.Evidence)
		item.ConfiguredEvidence = normalizeConfiguredEvidence(item.ConfiguredEvidence)
		if item.PRNumber == 0 {
			if number, ok := parseTickPRNumber(item.PR); ok {
				item.PRNumber = number
			}
		}
		if item.Issue <= 0 && item.PRNumber <= 0 && item.PR == "" && item.SHA == "" && item.Head == "" {
			continue
		}
		out = append(out, item)
	}
	sortTickPendingPromotion(out)
	return out
}

func sortTickPendingPromotion(items []TickPendingPromotion) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].PRNumber != items[j].PRNumber {
			if items[i].PRNumber == 0 {
				return false
			}
			if items[j].PRNumber == 0 {
				return true
			}
			return items[i].PRNumber < items[j].PRNumber
		}
		if items[i].Issue != items[j].Issue {
			if items[i].Issue == 0 {
				return false
			}
			if items[j].Issue == 0 {
				return true
			}
			return items[i].Issue < items[j].Issue
		}
		if items[i].RunID != items[j].RunID {
			return items[i].RunID < items[j].RunID
		}
		return tickPendingPromotionKey(items[i]) < tickPendingPromotionKey(items[j])
	})
}

func tickPendingPromotionKey(item TickPendingPromotion) string {
	branch := strings.ToLower(strings.TrimSpace(item.Branch))
	if item.PRNumber > 0 {
		return branch + "|pr|" + strconv.Itoa(item.PRNumber)
	}
	if strings.TrimSpace(item.PR) != "" {
		return branch + "|pr|" + strings.ToLower(strings.TrimSpace(item.PR))
	}
	if item.Issue > 0 {
		return branch + "|issue|" + strconv.Itoa(item.Issue)
	}
	if strings.TrimSpace(item.SHA) != "" {
		return branch + "|sha|" + strings.ToLower(strings.TrimSpace(item.SHA))
	}
	return branch + "|head|" + strings.ToLower(strings.TrimSpace(item.Head))
}

func tickReportTargetsMatch(leftIssue int, leftPR string, leftPRNumber int, rightIssue int, rightPR string, rightPRNumber int) bool {
	if leftPRNumber == 0 {
		if number, ok := parseTickPRNumber(leftPR); ok {
			leftPRNumber = number
		}
	}
	if rightPRNumber == 0 {
		if number, ok := parseTickPRNumber(rightPR); ok {
			rightPRNumber = number
		}
	}
	if leftPRNumber > 0 && rightPRNumber > 0 {
		return leftPRNumber == rightPRNumber
	}
	if strings.TrimSpace(leftPR) != "" && strings.TrimSpace(rightPR) != "" {
		return strings.EqualFold(strings.TrimSpace(leftPR), strings.TrimSpace(rightPR))
	}
	return leftIssue > 0 && rightIssue > 0 && leftIssue == rightIssue
}

func sameTickPreProdBranch(left, right string) bool {
	right = strings.TrimSpace(right)
	if right == "" {
		return true
	}
	left = strings.TrimSpace(left)
	return left == "" || strings.EqualFold(left, right)
}

func formatTickPendingPromotionTarget(item TickPendingPromotion) string {
	target := strings.TrimSpace(item.PR)
	if item.PRNumber > 0 {
		target = fmt.Sprintf("PR #%d", item.PRNumber)
	} else if target != "" {
		target = "PR " + target
	} else if item.Issue > 0 {
		target = fmt.Sprintf("issue #%d", item.Issue)
	} else if strings.TrimSpace(item.SHA) != "" {
		target = "commit " + item.SHA
	} else {
		target = "item"
	}
	if item.Issue > 0 && !strings.EqualFold(target, fmt.Sprintf("issue #%d", item.Issue)) {
		target += fmt.Sprintf(" issue #%d", item.Issue)
	}
	return target
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
