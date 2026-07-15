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
	"sync"
	"time"

	compiler "github.com/jasonhnd/loopcoder/internal/compile"
	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/loopreview"
	"github.com/jasonhnd/loopcoder/internal/orchestrationcost"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/statebranch"
	gh "github.com/jasonhnd/loopcoder/internal/vcs/github"
	"github.com/jasonhnd/loopcoder/internal/waitstate"
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
	TickStopRecoverNeedsHuman         = "recover-needs-human"
	TickStopRiskGateNeedsHuman        = "risk-gate-needs-human"
	TickStopPreProdNeedsHuman         = "pre-prod-needs-human"
	TickStopStatePushFailed           = "state-push-failed"
	TickStopCompileFailed             = "compile-failed"
	TickStopReadySetFailed            = "ready-set-failed"
	TickStopAttemptLoadFailed         = "attempt-load-failed"
	TickStopDispatchWaveFailed        = "dispatch-wave-failed"
	TickStopNoReviewablePRsDispatched = "no-reviewable-prs-dispatched"
	TickStopOrchestrationCostBudget   = "orchestration-cost-budget"
	TickStopOrchestrationCostPersist  = "orchestration-cost-persist-failed"

	tickPendingPromotionEvent  = "tick.pending_promotion"
	tickStatusPendingPromotion = "pending-promotion"
)

type CompileFunc func(ctx context.Context, opts compiler.Options) (compiler.Report, error)
type DispatchWaveFunc func(ctx context.Context, opts DispatchWaveOptions) (DispatchWaveReport, error)
type LoopreviewFunc func(ctx context.Context, opts loopreview.Options) (loopreview.Result, error)
type RecoverFunc func(ctx context.Context, opts recovery.Options) (recovery.Result, error)
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
	ConfigFromBase         bool
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
	CostPolicy             orchestrationcost.Policy
	CostEvents             []orchestrationcost.Event
	CircuitBreaker         config.GuardrailCircuitBreaker
	Progress               progress.Recorder
	AdditionalRiskRedLines []RiskRedLine
	WaitForChecks          bool
	WaitPolicy             waitstate.Policy
	WaitClock              waitstate.Clock
	ProcessAlive           ProcessAliveFunc
	Clock                  func() time.Time
	Stderr                 io.Writer

	Compile         CompileFunc
	ComputeReadySet ReadySetFunc
	DispatchWave    DispatchWaveFunc
	Dispatch        WorkerDispatchFunc
	Loopreview      LoopreviewFunc
	Recover         RecoverFunc
	RiskGate        RiskGateFunc
	PreProdWriter   PreProdWriter
	StatePush       StatePushFunc
	LoadAttempts    LoadAttemptsFunc
}

// TickReport intentionally has no top-level conductor Report. The
// 0.4.0 tick path is deterministic Go orchestration, not an LLM conductor
// invocation, so there is no real provider/model/usage record to stamp. Worker
// dispatch and verifier loopreview reports remain surfaced on their own
// report entries.
type TickReport struct {
	Version           int                       `json:"version"`
	Repo              string                    `json:"repo"`
	RepoPath          string                    `json:"repo_path"`
	BaseBranch        string                    `json:"base_branch"`
	PreProdBranch     string                    `json:"pre_prod_branch"`
	RunID             string                    `json:"run_id"`
	Status            string                    `json:"status"`
	StopReason        string                    `json:"stop_reason"`
	StartedAt         string                    `json:"started_at"`
	FinishedAt        string                    `json:"finished_at"`
	Compile           compiler.Report           `json:"compile"`
	ReadySet          report.ReadySetReport     `json:"ready_set"`
	DispatchWave      *DispatchWaveReport       `json:"dispatch_wave,omitempty"`
	Recoveries        []TickRecoveryResult      `json:"recoveries,omitempty"`
	Reviews           []TickReviewResult        `json:"reviews"`
	RiskGates         []TickRiskGateResult      `json:"risk_gates"`
	PreProdMerges     []TickPreProdMergeResult  `json:"pre_prod_merges"`
	PreProdHealth     []TickPreProdHealthResult `json:"pre_prod_health"`
	PreProdReverts    []TickPreProdRevertResult `json:"pre_prod_reverts"`
	PendingPromotion  []TickPendingPromotion    `json:"pending_promotion,omitempty"`
	NeedsHuman        []TickIssue               `json:"needs_human"`
	Failures          []TickIssue               `json:"failures"`
	StatePush         *TickStatePush            `json:"state_push,omitempty"`
	OrchestrationCost orchestrationcost.Report  `json:"orchestration_cost"`
	Summary           TickSummary               `json:"summary"`
}

type TickReviewResult struct {
	Issue              int                           `json:"issue,omitempty"`
	PR                 string                        `json:"pr,omitempty"`
	PRNumber           int                           `json:"pr_number,omitempty"`
	Verdict            string                        `json:"verdict"`
	SpecConformance    string                        `json:"spec_conformance,omitempty"`
	Evidence           string                        `json:"evidence,omitempty"`
	Reason             string                        `json:"reason,omitempty"`
	NextAction         string                        `json:"next_action,omitempty"`
	ConfiguredEvidence []config.EvidenceArtifact     `json:"configured_evidence,omitempty"`
	RenderedArtifacts  []loopreview.RenderedArtifact `json:"rendered_artifacts,omitempty"`
	Findings           []loopreview.Finding          `json:"findings"`
	Error              string                        `json:"error,omitempty"`
	Report             *reporter.Report              `json:"report,omitempty"`
}

type TickRecoveryResult struct {
	Issue    int                      `json:"issue"`
	PR       string                   `json:"pr,omitempty"`
	Action   string                   `json:"action"`
	Detail   string                   `json:"detail,omitempty"`
	Attempts []recovery.AttemptRecord `json:"attempts,omitempty"`
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
	Wait               *waitstate.Report         `json:"wait,omitempty"`
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
	PriorStableCommit  string                    `json:"prior_stable_commit,omitempty"`
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
	MergeCommit        string                    `json:"merge_commit,omitempty"`
	PriorStableCommit  string                    `json:"prior_stable_commit,omitempty"`
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
		opts.BaseBranch = lcdefaults.BaseBranch
	}
	if strings.TrimSpace(opts.PreProdBranch) == "" {
		opts.PreProdBranch = lcdefaults.PreProdBranch
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
	costRunLock, lockErr := orchestrationcost.AcquireRunLock(opts.RepoPath, opts.RunID, 0)
	if lockErr != nil {
		return TickReport{}, fmt.Errorf("acquire orchestration cost run lock: %w", lockErr)
	}
	defer costRunLock.Release()
	costEvents := append([]orchestrationcost.Event(nil), opts.CostEvents...)
	persistedCost, foundPersistedCost, loadCostErr := orchestrationcost.Load(opts.RepoPath, opts.RunID)
	if loadCostErr != nil {
		return TickReport{}, loadCostErr
	}
	if foundPersistedCost {
		costEvents = append(append([]orchestrationcost.Event(nil), persistedCost.Events...), costEvents...)
	}
	costReport, costErr := orchestrationcost.Build(opts.RunID, opts.CostPolicy, costEvents)
	if costErr != nil {
		return TickReport{}, costErr
	}
	if foundPersistedCost {
		costReport = orchestrationcost.RestoreDecisionState(costReport, persistedCost.BudgetDecisions, persistedCost.ReleaseGate)
	}
	tickReport.OrchestrationCost = costReport
	finish := func(status, stopReason string) (TickReport, error) {
		finished := opts.Clock().UTC()
		tickReport.Status = status
		tickReport.StopReason = stopReason
		tickReport.FinishedAt = state.FormatTimestamp(finished)
		attachTickConfiguredEvidence(&tickReport, opts.ConfiguredEvidence)
		tickReport.PendingPromotion = loadTickPendingPromotionLedger(opts.RepoPath, opts.PreProdBranch)
		if err := orchestrationcost.Write(tickReport.RepoPath, tickReport.OrchestrationCost); err != nil {
			tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(tickReport.OrchestrationCost, orchestrationcost.PersistenceFailure(err))
			tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "orchestration-cost", Detail: err.Error()})
			tickReport.Status = TickStatusFailed
			tickReport.StopReason = TickStopOrchestrationCostPersist
		}
		return normalizeTickReport(tickReport), nil
	}

	plannerStarted := time.Now()
	compiled, err := opts.Compile(ctx, compiler.Options{
		RepoPath: opts.RepoPath,
		Writer:   opts.IssueWriter,
		Now:      started,
	})
	plannerEvent := orchestrationcost.DeterministicEvent(
		"planner:compile",
		orchestrationcost.RolePlanner,
		orchestrationcost.ActivityPhase,
		"compile is deterministic Go orchestration",
	)
	plannerEvent.DurationMS = time.Since(plannerStarted).Milliseconds()
	recordTickCostEvent(&tickReport, plannerEvent)
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
		if candidate, ok := restoredReleaseCandidate(readySet, tickReport.OrchestrationCost); ok {
			if tickReport.OrchestrationCost.Status == orchestrationcost.StatusNeedsHuman {
				tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
					Step:   "orchestration-cost-release",
					Issue:  candidate.Issue,
					PR:     candidate.PR,
					Detail: formatCostDecision(*tickReport.OrchestrationCost.ReleaseGate),
				})
				return finish(TickStatusNeedsHuman, TickStopOrchestrationCostBudget)
			}
			prNumber, parsed := parseTickPRNumber(candidate.PR)
			if !parsed {
				tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "orchestration-cost-release", Issue: candidate.Issue, PR: candidate.PR, Detail: "could not parse restored release-gated pull request number"})
				return finish(TickStatusNeedsHuman, TickStopOrchestrationCostBudget)
			}
			runTickRiskGateAndPreProdMerge(ctx, opts, &tickReport, candidate, prNumber)
			pushTickState(ctx, opts, &tickReport)
			if len(tickReport.Failures) > 0 {
				if hasStatePushFailure(tickReport.Failures) {
					return finish(TickStatusFailed, TickStopStatePushFailed)
				}
				for _, failure := range tickReport.Failures {
					if failure.Step == "orchestration-cost" {
						return finish(TickStatusFailed, TickStopOrchestrationCostPersist)
					}
				}
				return finish(TickStatusFailed, TickStopReviewFailed)
			}
			if len(tickReport.NeedsHuman) > 0 {
				return finish(TickStatusNeedsHuman, tickNeedsHumanStopReason(tickReport.NeedsHuman))
			}
			return finish(TickStatusSucceeded, TickStopCompleted)
		}
		if candidate, ok := restoredVerificationCandidate(readySet, tickReport.OrchestrationCost); ok {
			if strings.TrimSpace(opts.VerifierProvider) == "" {
				tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "loopreview", Issue: candidate.Issue, PR: candidate.PR, Detail: "verifier provider is required to resume the budget-blocked pull request"})
				return finish(TickStatusNeedsHuman, TickStopVerifierProviderMissing)
			}
			runTickRestoredVerification(ctx, opts, &tickReport, candidate)
			pushTickState(ctx, opts, &tickReport)
			if len(tickReport.Failures) > 0 {
				if hasStatePushFailure(tickReport.Failures) {
					return finish(TickStatusFailed, TickStopStatePushFailed)
				}
				for _, failure := range tickReport.Failures {
					if failure.Step == "orchestration-cost" {
						return finish(TickStatusFailed, TickStopOrchestrationCostPersist)
					}
				}
				return finish(TickStatusFailed, TickStopReviewFailed)
			}
			if len(tickReport.NeedsHuman) > 0 {
				return finish(TickStatusNeedsHuman, tickNeedsHumanStopReason(tickReport.NeedsHuman))
			}
			return finish(TickStatusSucceeded, TickStopCompleted)
		}
		if tickReport.OrchestrationCost.Status == orchestrationcost.StatusNeedsHuman {
			detail := tickReport.OrchestrationCost.Reason
			if tickReport.OrchestrationCost.ReleaseGate != nil {
				detail = formatCostDecision(*tickReport.OrchestrationCost.ReleaseGate)
			}
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "orchestration-cost-release", Detail: detail})
			return finish(TickStatusNeedsHuman, TickStopOrchestrationCostBudget)
		}
		return finish(TickStatusNoReadyWork, TickStopNoReadyWork)
	}
	if strings.TrimSpace(opts.VerifierProvider) == "" {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "loopreview",
			Detail: "verifier provider is required before dispatching unattended work",
		})
		return finish(TickStatusNeedsHuman, TickStopVerifierProviderMissing)
	}
	var costMu sync.Mutex
	workerReservations := map[int]string{}
	workerResultsRecorded := map[int]bool{}
	wave, err := opts.DispatchWave(ctx, DispatchWaveOptions{
		Reader:         opts.Reader,
		RepoPath:       opts.RepoPath,
		BaseBranch:     opts.BaseBranch,
		RunID:          opts.RunID,
		ReadySet:       &readySet,
		Provider:       opts.WorkerProvider,
		Model:          opts.WorkerModel,
		Effort:         opts.WorkerEffort,
		ConfigFromBase: opts.ConfigFromBase,
		// Exact token usage is only known when a provider returns. Cost-budgeted
		// dispatch therefore advances one provider at a time so each completed
		// report can release the next conservative budget slot.
		ThrottleLimit:   1,
		Thresholds:      opts.Thresholds,
		Budget:          opts.Budget,
		CircuitBreaker:  opts.CircuitBreaker,
		ProcessAlive:    opts.ProcessAlive,
		Now:             started,
		Stderr:          opts.Stderr,
		ComputeReadySet: opts.ComputeReadySet,
		Dispatch:        opts.Dispatch,
		LoadAttempts:    opts.LoadAttempts,
		BeforeProviderCall: func(issue int) error {
			costMu.Lock()
			defer costMu.Unlock()
			decision := orchestrationcost.CheckBeforeModelCall(tickReport.OrchestrationCost, 1)
			tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(tickReport.OrchestrationCost, decision)
			if !decision.Allowed {
				if err := persistTickCostReport(&tickReport); err != nil {
					return err
				}
				return errors.New(formatCostDecision(decision))
			}
			eventID := nextTickCostEventID(tickReport.OrchestrationCost.Events, workerCostEventID(issue))
			if err := upsertTickCostEvent(&tickReport, orchestrationcost.EventFromReport(
				eventID, orchestrationcost.RoleWorker, true, nil,
				fmt.Sprintf("issue=%d", issue), "provider-call-reserved",
			)); err != nil {
				return err
			}
			workerReservations[issue] = eventID
			return nil
		},
		OnIssueComplete: func(completed DispatchWaveIssueComplete) error {
			costMu.Lock()
			defer costMu.Unlock()
			err := recordDispatchWaveResultCost(&tickReport, completed.Result, workerReservations)
			workerResultsRecorded[completed.Result.Issue] = true
			return err
		},
	})
	tickReport.DispatchWave = &wave
	recordDispatchWaveCost(&tickReport, wave, workerReservations, workerResultsRecorded)
	tickReport.Repo = firstNonEmpty(wave.Repo, tickReport.Repo)
	if err != nil {
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "dispatch-wave", Detail: err.Error()})
		pushTickState(ctx, opts, &tickReport)
		return finish(TickStatusFailed, TickStopDispatchWaveFailed)
	}

	for _, result := range wave.Results {
		switch result.Status {
		case DispatchWaveStatusNeedsHuman:
			step := "dispatch-wave"
			if tickReport.OrchestrationCost.Status == orchestrationcost.StatusNeedsHuman {
				step = "orchestration-cost"
			}
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   step,
				Issue:  result.Issue,
				PR:     result.PR,
				Detail: result.Error,
			})
		case DispatchWaveStatusFailed:
			if tickReport.OrchestrationCost.Status == orchestrationcost.StatusNeedsHuman {
				tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
					Step:   "orchestration-cost",
					Issue:  result.Issue,
					PR:     result.PR,
					Detail: firstNonEmpty(result.Error, tickReport.OrchestrationCost.Reason),
				})
				continue
			}
			runTickRecoverDispatchFailure(ctx, opts, &tickReport, result)
		}
	}
	if len(tickReport.NeedsHuman) > 0 {
		if tickReport.OrchestrationCost.Status == orchestrationcost.StatusNeedsHuman {
			markPendingVerifierCandidates(&tickReport, wave.Results)
			if err := persistTickCostReport(&tickReport); err != nil {
				return finish(TickStatusFailed, TickStopOrchestrationCostPersist)
			}
		}
		pushTickState(ctx, opts, &tickReport)
		if hasStatePushFailure(tickReport.Failures) {
			return finish(TickStatusFailed, TickStopStatePushFailed)
		}
		if len(tickReport.Failures) > 0 {
			return finish(TickStatusFailed, TickStopDispatchFailed)
		}
		stopReason := TickStopGuardrailNeedsHuman
		if tickNeedsHumanStopReason(tickReport.NeedsHuman) == TickStopOrchestrationCostBudget {
			stopReason = TickStopOrchestrationCostBudget
		}
		return finish(TickStatusNeedsHuman, stopReason)
	}
	if len(tickReport.Failures) > 0 {
		pushTickState(ctx, opts, &tickReport)
		if hasStatePushFailure(tickReport.Failures) {
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
		if len(tickReport.NeedsHuman) > 0 {
			return finish(TickStatusNeedsHuman, tickNeedsHumanStopReason(tickReport.NeedsHuman))
		}
		if len(tickReport.Recoveries) > 0 || len(tickReport.Reviews) > 0 || len(tickReport.PreProdMerges) > 0 {
			return finish(TickStatusSucceeded, TickStopCompleted)
		}
		return finish(TickStatusNoReadyWork, TickStopNoReviewablePRsDispatched)
	}

reviewLoop:
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
		result, err := runTickCostedLoopreview(ctx, opts, &tickReport, prNumber,
			fmt.Sprintf("verifier:pr-%d", prNumber), orchestrationcost.RoleVerifier, true,
			fmt.Sprintf("pr=%d", prNumber),
		)
		if err != nil {
			review.Verdict = loopreview.VerdictNeedsHuman
			review.Error = err.Error()
			tickReport.Reviews = append(tickReport.Reviews, review)
			step := "loopreview"
			if tickReport.OrchestrationCost.Status == orchestrationcost.StatusNeedsHuman {
				step = "orchestration-cost"
			}
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   step,
				Issue:  item.Issue,
				PR:     item.PR,
				Detail: err.Error(),
			})
			if step == "orchestration-cost" {
				break
			}
			continue
		}
		review = tickReviewResultFromLoopreview(item.Issue, item.PR, prNumber, result)
		tickReport.Reviews = append(tickReport.Reviews, review)
		switch result.Verdict.Verdict {
		case loopreview.VerdictPass:
			releaseDecision := orchestrationcost.BindReleaseDecision(orchestrationcost.CheckReleaseGate(tickReport.OrchestrationCost), prNumber)
			tickReport.OrchestrationCost = orchestrationcost.ApplyReleaseDecision(tickReport.OrchestrationCost, releaseDecision)
			if !releaseDecision.Allowed {
				tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "orchestration-cost-release", Issue: item.Issue, PR: item.PR, Detail: formatCostDecision(releaseDecision)})
				break reviewLoop
			}
			runTickRiskGateAndPreProdMerge(ctx, opts, &tickReport, item, prNumber)
		case loopreview.VerdictFail:
			runTickRecoverReviewFailure(ctx, opts, &tickReport, item, result)
		default:
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   "loopreview",
				Issue:  item.Issue,
				PR:     item.PR,
				Detail: firstNonEmpty(result.Verdict.Reason, result.Verdict.Evidence, "verifier returned needs-human"),
			})
		}
	}

	pushTickState(ctx, opts, &tickReport)
	if len(tickReport.Failures) > 0 {
		if hasStatePushFailure(tickReport.Failures) {
			return finish(TickStatusFailed, TickStopStatePushFailed)
		}
		for _, failure := range tickReport.Failures {
			if failure.Step == "orchestration-cost" {
				return finish(TickStatusFailed, TickStopOrchestrationCostPersist)
			}
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
	if opts.Recover == nil {
		opts.Recover = func(ctx context.Context, recoverOpts recovery.Options) (recovery.Result, error) {
			recoverDeps := recovery.DefaultDeps()
			recoverDeps.Dispatch = func(ctx context.Context, dispatchOpts recovery.DispatchOptions) (recovery.DispatchResult, error) {
				result, err := opts.Dispatch(ctx, worker.Options{
					RepoPath:           dispatchOpts.RepoPath,
					IssueNumber:        dispatchOpts.IssueNumber,
					IssueTitle:         dispatchOpts.IssueTitle,
					IssueBody:          dispatchOpts.IssueBody,
					BaseBranch:         dispatchOpts.BaseBranch,
					Branch:             dispatchOpts.Branch,
					RunID:              dispatchOpts.RunID,
					Attempt:            dispatchOpts.Attempt,
					RecoveryContext:    dispatchOpts.RecoveryContext,
					Provider:           dispatchOpts.Provider,
					Model:              dispatchOpts.Model,
					Effort:             dispatchOpts.Effort,
					ConfigFromBase:     dispatchOpts.ConfigFromBase,
					Stderr:             dispatchOpts.Stderr,
					BeforeProviderCall: dispatchOpts.BeforeProviderCall,
				})
				return recovery.DispatchResult{
					OK:              result.OK,
					ProviderInvoked: result.ProviderInvoked,
					Issue:           result.Issue,
					Branch:          result.Branch,
					RunID:           result.RunID,
					PR:              result.PR,
					Summary:         result.Summary,
					AttemptPath:     result.AttemptPath,
					Status:          result.Status,
					Outcome:         result.Outcome,
					ProviderOutcome: result.ProviderOutcome,
					DeliveryOutcome: result.DeliveryOutcome,
					Evidence:        result.Evidence,
					ExitCode:        result.ExitCode,
					LogBytes:        result.LogBytes,
					Reason:          result.Reason,
					NextAction:      result.NextAction,
					Report:          result.Report,
				}, err
			}
			recoverDeps.Review = func(ctx context.Context, reviewOpts loopreview.Options) (loopreview.Result, error) {
				return opts.Loopreview(ctx, reviewOpts)
			}
			return recovery.Run(ctx, recoverOpts, recoverDeps)
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
		opts.ThrottleLimit = lcdefaults.DispatchWaveThrottleLimit
	}
	if opts.CostPolicy == (orchestrationcost.Policy{}) {
		opts.CostPolicy = orchestrationcost.DefaultPolicy()
	}
	return opts
}

func recordTickCostEvent(tickReport *TickReport, event orchestrationcost.Event) {
	event.EventID = nextTickCostEventID(tickReport.OrchestrationCost.Events, event.EventID)
	events := append(append([]orchestrationcost.Event(nil), tickReport.OrchestrationCost.Events...), event)
	_ = replaceTickCostEvents(tickReport, events)
}

func upsertTickCostEvent(tickReport *TickReport, event orchestrationcost.Event) error {
	if err := upsertTickCostEventInMemory(tickReport, event); err != nil {
		return err
	}
	return persistTickCostReport(tickReport)
}

func upsertTickCostEventInMemory(tickReport *TickReport, event orchestrationcost.Event) error {
	events := append([]orchestrationcost.Event(nil), tickReport.OrchestrationCost.Events...)
	replaced := false
	for i := range events {
		if events[i].EventID == event.EventID {
			events[i] = event
			replaced = true
			break
		}
	}
	if !replaced {
		events = append(events, event)
	}
	return replaceTickCostEventsInMemory(tickReport, events)
}

func removeTickCostEvent(tickReport *TickReport, eventID string) error {
	events := make([]orchestrationcost.Event, 0, len(tickReport.OrchestrationCost.Events))
	for _, event := range tickReport.OrchestrationCost.Events {
		if event.EventID != eventID {
			events = append(events, event)
		}
	}
	return replaceTickCostEvents(tickReport, events)
}

func replaceTickCostEvents(tickReport *TickReport, events []orchestrationcost.Event) error {
	if err := replaceTickCostEventsInMemory(tickReport, events); err != nil {
		return err
	}
	return persistTickCostReport(tickReport)
}

func replaceTickCostEventsInMemory(tickReport *TickReport, events []orchestrationcost.Event) error {
	previous := tickReport.OrchestrationCost
	next, err := orchestrationcost.Build(previous.RunID, previous.Policy, events)
	if err != nil {
		tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(previous, orchestrationcost.AccountingFailure(err))
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "orchestration-cost", Detail: err.Error()})
		return err
	}
	next = orchestrationcost.RestoreDecisionState(next, previous.BudgetDecisions, previous.ReleaseGate)
	tickReport.OrchestrationCost = next
	return nil
}

func persistTickCostReport(tickReport *TickReport) error {
	if err := orchestrationcost.Write(tickReport.RepoPath, tickReport.OrchestrationCost); err != nil {
		next := tickReport.OrchestrationCost
		tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(next, orchestrationcost.PersistenceFailure(err))
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "orchestration-cost", Detail: err.Error()})
		return err
	}
	return nil
}

func nextTickCostEventID(events []orchestrationcost.Event, base string) string {
	base = strings.TrimSpace(base)
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		seen[event.EventID] = true
	}
	if !seen[base] {
		return base
	}
	for ordinal := 2; ; ordinal++ {
		candidate := fmt.Sprintf("%s:%d", base, ordinal)
		if !seen[candidate] {
			return candidate
		}
	}
}

func workerCostEventID(issue int) string {
	return fmt.Sprintf("worker:issue-%d", issue)
}

func recordDispatchWaveCost(tickReport *TickReport, wave DispatchWaveReport, currentReservations map[int]string, recorded map[int]bool) {
	for _, result := range wave.Results {
		if recorded[result.Issue] {
			continue
		}
		_ = recordDispatchWaveResultCost(tickReport, result, currentReservations)
	}
}

func recordDispatchWaveResultCost(tickReport *TickReport, result DispatchWaveIssueResult, currentReservations map[int]string) error {
	eventID := currentReservations[result.Issue]
	delete(currentReservations, result.Issue)
	if !result.ProviderInvoked {
		if eventID != "" {
			if err := removeTickCostEvent(tickReport, eventID); err != nil {
				return err
			}
		}
		if strings.TrimSpace(result.ProviderOutcome) == "" || strings.TrimSpace(result.DeliveryOutcome) == "" {
			return nil
		}
		deliveryEvent := orchestrationcost.DeterministicEvent(
			nextTickCostEventID(tickReport.OrchestrationCost.Events, fmt.Sprintf("delivery:issue-%d:retry", result.Issue)),
			orchestrationcost.RoleDelivery,
			orchestrationcost.ActivityDeliveryRetry,
			append([]string{
				fmt.Sprintf("issue=%d", result.Issue),
				fmt.Sprintf("outcome=%s", result.Outcome),
				fmt.Sprintf("provider_outcome=%s", result.ProviderOutcome),
				fmt.Sprintf("delivery_outcome=%s", result.DeliveryOutcome),
			}, result.Evidence...)...,
		)
		deliveryEvent.Retries = 1
		deliveryEvent.DeliveryOnlyRetries = 1
		return upsertTickCostEvent(tickReport, deliveryEvent)
	}
	if eventID == "" {
		eventID = nextTickCostEventID(tickReport.OrchestrationCost.Events, workerCostEventID(result.Issue))
	}
	return upsertTickCostEvent(tickReport, orchestrationcost.EventFromReport(
		eventID, orchestrationcost.RoleWorker, true, result.Report,
		fmt.Sprintf("issue=%d", result.Issue), fmt.Sprintf("status=%s", result.Status),
	))
}

func recordRecoveryCost(tickReport *TickReport, issue int, result recovery.Result, providerCallsRecorded bool) {
	recorded := false
	reviewRecorded := false
	if !providerCallsRecorded {
		for _, attempt := range result.RecoveryAttempts {
			if attempt.DispatchResult != nil && attempt.DispatchResult.ProviderInvoked {
				recorded = true
				recordTickCostEvent(tickReport, orchestrationcost.EventFromReport(
					fmt.Sprintf("recovery:issue-%d:attempt-%d:worker", issue, attempt.Attempt), orchestrationcost.RoleRecovery, false, attempt.DispatchResult.Report,
					fmt.Sprintf("strategy=%s", attempt.Strategy), fmt.Sprintf("status=%s", attempt.Status),
				))
			}
			if attempt.Review != nil && attempt.Review.Report != nil {
				recorded = true
				reviewRecorded = true
				recordTickCostEvent(tickReport, orchestrationcost.EventFromReport(
					fmt.Sprintf("recovery:issue-%d:attempt-%d:verifier", issue, attempt.Attempt), orchestrationcost.RoleRecovery, false, attempt.Review.Report,
					fmt.Sprintf("strategy=%s", attempt.Strategy), "recovery verifier call",
				))
			}
		}
		if !recorded && result.DispatchResult != nil && result.DispatchResult.ProviderInvoked {
			recordTickCostEvent(tickReport, orchestrationcost.EventFromReport(
				fmt.Sprintf("recovery:issue-%d:worker", issue), orchestrationcost.RoleRecovery, false, result.DispatchResult.Report,
				"recovery worker call",
			))
		}
		if !reviewRecorded && result.ReviewResult != nil && result.ReviewResult.ProviderInvoked {
			recordTickCostEvent(tickReport, orchestrationcost.EventFromReport(
				fmt.Sprintf("recovery:issue-%d:verifier", issue), orchestrationcost.RoleRecovery, false, result.ReviewResult.Verdict.Report,
				"recovery verifier call",
			))
		}
	}
	retryEvent := orchestrationcost.DeterministicEvent(
		fmt.Sprintf("recovery:issue-%d:retries", issue), orchestrationcost.RoleRecovery, orchestrationcost.ActivityRecoveryRetry,
		"recovery retry ledger",
	)
	retryEvent.Retries = len(result.RecoveryAttempts)
	for _, attempt := range result.RecoveryAttempts {
		if attempt.Strategy == recovery.AttemptStrategySameConfig {
			retryEvent.DuplicateRetries++
		}
	}
	recordTickCostEvent(tickReport, retryEvent)
}

func recordRiskGateWaitCost(tickReport *TickReport, prNumber int, wait *waitstate.Report) error {
	if wait == nil {
		return nil
	}
	event := orchestrationcost.DeterministicEvent(
		fmt.Sprintf("waiting:github-ci:pr-%d", prNumber), orchestrationcost.RoleWaiting, orchestrationcost.ActivityCIPoll,
		fmt.Sprintf("polls=%d", wait.Polls), fmt.Sprintf("receipts=%d", wait.Receipts), fmt.Sprintf("stop_reason=%s", wait.StopReason),
	)
	if wait.Polls > 1 {
		event.Retries = wait.Polls - 1
	}
	event.DuplicateSuppressions = wait.DuplicateSuppressions
	event.ContextPacketBytes = len(wait.LastPacket)
	event.DurationMS = wait.DurationMS
	event.EventID = nextTickCostEventID(tickReport.OrchestrationCost.Events, event.EventID)
	return upsertTickCostEvent(tickReport, event)
}

func formatCostDecision(decision orchestrationcost.Decision) string {
	return fmt.Sprintf("%s: observed=%s limit=%s; %s", decision.Reason, decision.Observed, decision.Limit, decision.Remediation)
}

func runTickCostedLoopreview(
	ctx context.Context,
	opts TickOptions,
	tickReport *TickReport,
	prNumber int,
	eventID string,
	role orchestrationcost.Role,
	useful bool,
	evidence ...string,
) (loopreview.Result, error) {
	reserved := false
	reservationEventID := ""
	result, err := opts.Loopreview(ctx, loopreview.Options{
		RepoPath:       opts.RepoPath,
		PRNumber:       prNumber,
		Provider:       opts.VerifierProvider,
		Model:          opts.VerifierModel,
		Effort:         opts.VerifierEffort,
		BaseBranch:     opts.BaseBranch,
		ConfigFromBase: opts.ConfigFromBase,
		Timeout:        opts.VerifierTimeout,
		Stderr:         opts.Stderr,
		BeforeProviderCall: func() error {
			decision := orchestrationcost.BindDecisionToPR(orchestrationcost.CheckBeforeModelCall(tickReport.OrchestrationCost, 1), prNumber)
			tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(tickReport.OrchestrationCost, decision)
			if !decision.Allowed {
				if err := persistTickCostReport(tickReport); err != nil {
					return err
				}
				return errors.New(formatCostDecision(decision))
			}
			reservationEventID = nextTickCostEventID(tickReport.OrchestrationCost.Events, eventID)
			pendingEvidence := append(append([]string(nil), evidence...), "provider-call-reserved")
			if err := upsertTickCostEvent(tickReport, orchestrationcost.EventFromReport(reservationEventID, role, useful, nil, pendingEvidence...)); err != nil {
				return err
			}
			reserved = true
			return nil
		},
	})
	if result.ProviderInvoked {
		if reservationEventID == "" {
			reservationEventID = nextTickCostEventID(tickReport.OrchestrationCost.Events, eventID)
		}
		costErr := upsertTickCostEventInMemory(tickReport, orchestrationcost.EventFromReport(reservationEventID, role, useful, result.Verdict.Report, evidence...))
		if costErr == nil {
			tickReport.OrchestrationCost = orchestrationcost.MarkBudgetDecisionConsumed(tickReport.OrchestrationCost, prNumber)
			costErr = persistTickCostReport(tickReport)
		}
		if costErr != nil && err == nil {
			err = costErr
		}
	} else if reserved {
		if costErr := removeTickCostEvent(tickReport, reservationEventID); costErr != nil && err == nil {
			err = costErr
		}
	}
	return result, err
}

func runTickRecoverDispatchFailure(ctx context.Context, opts TickOptions, tickReport *TickReport, result DispatchWaveIssueResult) {
	issue, err := opts.Reader.ViewIssue(ctx, result.Issue)
	if err != nil {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  result.Issue,
			PR:     result.PR,
			Detail: fmt.Sprintf("read issue for dispatch recovery: %v", err),
		})
		return
	}
	runTickRecoverFailure(ctx, opts, tickReport, tickRecoveryRequest{
		IssueNumber:    result.Issue,
		IssueTitle:     firstNonEmpty(issue.Title, fmt.Sprintf("Issue #%d", result.Issue)),
		IssueBody:      issue.Body,
		TriggerStep:    "dispatch-wave",
		PR:             result.PR,
		Detail:         result.Error,
		FailureContext: renderTickDispatchRecoveryContext(result),
		SkipAdoptPR:    false,
	})
}

func runTickRecoverReviewFailure(ctx context.Context, opts TickOptions, tickReport *TickReport, item tickReviewCandidate, review loopreview.Result) {
	issue, err := opts.Reader.ViewIssue(ctx, item.Issue)
	if err != nil {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: fmt.Sprintf("read issue for review recovery: %v", err),
		})
		return
	}
	runTickRecoverFailure(ctx, opts, tickReport, tickRecoveryRequest{
		IssueNumber:    item.Issue,
		IssueTitle:     firstNonEmpty(issue.Title, fmt.Sprintf("Issue #%d", item.Issue)),
		IssueBody:      issue.Body,
		TriggerStep:    "loopreview",
		PR:             item.PR,
		Detail:         firstNonEmpty(review.Verdict.Reason, review.Verdict.Evidence, "verifier returned fail"),
		FailureContext: renderTickReviewRecoveryContext(item, review),
		SkipAdoptPR:    true,
	})
}

type tickRecoveryRequest struct {
	IssueNumber    int
	IssueTitle     string
	IssueBody      string
	TriggerStep    string
	PR             string
	Detail         string
	FailureContext string
	SkipAdoptPR    bool
}

func runTickRecoverFailure(ctx context.Context, opts TickOptions, tickReport *TickReport, request tickRecoveryRequest) {
	providerCallsRecorded := false
	pendingProviderEventID := ""
	var activeBudgetRefusal *orchestrationcost.Decision
	result, err := opts.Recover(ctx, recovery.Options{
		RepoPath:         opts.RepoPath,
		IssueNumber:      request.IssueNumber,
		IssueTitle:       request.IssueTitle,
		IssueBody:        request.IssueBody,
		RunID:            opts.RunID,
		BaseBranch:       opts.BaseBranch,
		MaxAttempts:      opts.Thresholds.MaxAttempts,
		BackoffSeconds:   opts.Thresholds.RetryBackoffSeconds,
		Provider:         opts.WorkerProvider,
		Model:            opts.WorkerModel,
		Effort:           opts.WorkerEffort,
		FailureContext:   request.FailureContext,
		SkipAdoptPR:      request.SkipAdoptPR,
		VerifierProvider: opts.VerifierProvider,
		VerifierModel:    opts.VerifierModel,
		VerifierEffort:   opts.VerifierEffort,
		VerifierTimeout:  opts.VerifierTimeout,
		ConfigFromBase:   opts.ConfigFromBase,
		Budget:           opts.Budget,
		CircuitBreaker:   opts.CircuitBreaker,
		Progress:         opts.Progress,
		Now:              opts.Clock(),
		Stderr:           opts.Stderr,
		BeforeProviderCall: func(kind string) error {
			callDecision := orchestrationcost.CheckBeforeModelCall(tickReport.OrchestrationCost, 1)
			tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(tickReport.OrchestrationCost, callDecision)
			if !callDecision.Allowed {
				decisionCopy := callDecision
				activeBudgetRefusal = &decisionCopy
				if err := persistTickCostReport(tickReport); err != nil {
					return err
				}
				return errors.New(formatCostDecision(callDecision))
			}
			pendingProviderEventID = nextTickCostEventID(
				tickReport.OrchestrationCost.Events,
				fmt.Sprintf("recovery:issue-%d:%s", request.IssueNumber, kind),
			)
			return upsertTickCostEvent(tickReport, orchestrationcost.EventFromReport(
				pendingProviderEventID, orchestrationcost.RoleRecovery, false, nil,
				fmt.Sprintf("provider_kind=%s", kind), "provider-call-reserved",
			))
		},
		AfterProviderCall: func(kind string, invoked bool, providerReport *reporter.Report) {
			if !invoked {
				if pendingProviderEventID != "" {
					_ = removeTickCostEvent(tickReport, pendingProviderEventID)
					pendingProviderEventID = ""
				}
				return
			}
			providerCallsRecorded = true
			if pendingProviderEventID == "" {
				pendingProviderEventID = nextTickCostEventID(
					tickReport.OrchestrationCost.Events,
					fmt.Sprintf("recovery:issue-%d:%s", request.IssueNumber, kind),
				)
			}
			_ = upsertTickCostEvent(tickReport, orchestrationcost.EventFromReport(
				pendingProviderEventID,
				orchestrationcost.RoleRecovery,
				false,
				providerReport,
				fmt.Sprintf("provider_kind=%s", kind),
			))
			pendingProviderEventID = ""
		},
	})
	recordRecoveryCost(tickReport, request.IssueNumber, result, providerCallsRecorded)
	if activeBudgetRefusal != nil {
		tickReport.OrchestrationCost = orchestrationcost.ReapplyBudgetDecision(tickReport.OrchestrationCost, *activeBudgetRefusal)
		_ = persistTickCostReport(tickReport)
	}
	recoveryReport := TickRecoveryResult{
		Issue:    request.IssueNumber,
		PR:       request.PR,
		Action:   string(result.Action),
		Detail:   firstNonEmpty(request.Detail, request.TriggerStep),
		Attempts: append([]recovery.AttemptRecord(nil), result.RecoveryAttempts...),
	}
	if result.DispatchResult != nil {
		recoveryReport.PR = firstNonEmpty(result.DispatchResult.PR, recoveryReport.PR)
	}
	if result.AdoptedPR != nil {
		recoveryReport.PR = firstNonEmpty(result.AdoptedPR.URL, fmt.Sprintf("#%d", result.AdoptedPR.Number), recoveryReport.PR)
	}
	if err != nil {
		recoveryReport.Detail = err.Error()
	}
	tickReport.Recoveries = append(tickReport.Recoveries, recoveryReport)
	if err != nil {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  request.IssueNumber,
			PR:     recoveryReport.PR,
			Detail: err.Error(),
		})
		return
	}

	switch result.Action {
	case recovery.ActionSucceeded:
		runTickRecoveredPR(ctx, opts, tickReport, request.IssueNumber, result.DispatchResult, result.ReviewResult)
	case recovery.ActionAdopt:
		runTickAdoptedPR(ctx, opts, tickReport, request.IssueNumber, result.AdoptedPR)
	case recovery.ActionRetry:
		runTickRecoveredPR(ctx, opts, tickReport, request.IssueNumber, result.DispatchResult, result.ReviewResult)
	default:
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  request.IssueNumber,
			PR:     recoveryReport.PR,
			Detail: firstNonEmpty(recoveryBlockedDetail(result.Report), request.Detail, "recovery exhausted"),
		})
	}
}

func runTickRecoveredPR(ctx context.Context, opts TickOptions, tickReport *TickReport, issueNumber int, dispatchResult *recovery.DispatchResult, reviewResult *loopreview.Result) {
	if dispatchResult == nil || strings.TrimSpace(dispatchResult.PR) == "" {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  issueNumber,
			Detail: "recovery did not produce a reviewable PR",
		})
		return
	}
	prNumber, ok := parseTickPRNumber(dispatchResult.PR)
	if !ok {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  issueNumber,
			PR:     dispatchResult.PR,
			Detail: "could not parse recovered pull request number",
		})
		return
	}
	if reviewResult == nil {
		result, err := runTickCostedLoopreview(ctx, opts, tickReport, prNumber,
			fmt.Sprintf("recovery:%d:post-review", issueNumber), orchestrationcost.RoleRecovery, false,
			"recovered PR verifier call", fmt.Sprintf("pr=%d", prNumber),
		)
		if err != nil {
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
				Step:   "recover",
				Issue:  issueNumber,
				PR:     dispatchResult.PR,
				Detail: err.Error(),
			})
			return
		}
		reviewResult = &result
	}
	review := tickReviewResultFromLoopreview(issueNumber, dispatchResult.PR, prNumber, *reviewResult)
	tickReport.Reviews = append(tickReport.Reviews, review)
	if review.Verdict != loopreview.VerdictPass {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  issueNumber,
			PR:     dispatchResult.PR,
			Detail: firstNonEmpty(review.Reason, review.Evidence, review.Error, "recovered PR did not pass loopreview"),
		})
		return
	}
	releaseDecision := orchestrationcost.BindReleaseDecision(orchestrationcost.CheckReleaseGate(tickReport.OrchestrationCost), prNumber)
	tickReport.OrchestrationCost = orchestrationcost.ApplyReleaseDecision(tickReport.OrchestrationCost, releaseDecision)
	if !releaseDecision.Allowed {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "orchestration-cost-release", Issue: issueNumber, PR: dispatchResult.PR, Detail: formatCostDecision(releaseDecision)})
		return
	}
	runTickRiskGateAndPreProdMerge(ctx, opts, tickReport, tickReviewCandidate{Issue: issueNumber, PR: dispatchResult.PR}, prNumber)
}

func runTickAdoptedPR(ctx context.Context, opts TickOptions, tickReport *TickReport, issueNumber int, adopted *recovery.AdoptedPR) {
	if adopted == nil || adopted.Number <= 0 {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  issueNumber,
			Detail: "recovery adopted a PR without a PR number",
		})
		return
	}
	pr := firstNonEmpty(adopted.URL, fmt.Sprintf("#%d", adopted.Number))
	result, err := runTickCostedLoopreview(ctx, opts, tickReport, adopted.Number,
		fmt.Sprintf("recovery:%d:adopt-review", issueNumber), orchestrationcost.RoleRecovery, false,
		"adopted PR verifier call", fmt.Sprintf("pr=%d", adopted.Number),
	)
	if err != nil {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  issueNumber,
			PR:     pr,
			Detail: err.Error(),
		})
		return
	}
	review := tickReviewResultFromLoopreview(issueNumber, pr, adopted.Number, result)
	tickReport.Reviews = append(tickReport.Reviews, review)
	if review.Verdict != loopreview.VerdictPass {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "recover",
			Issue:  issueNumber,
			PR:     pr,
			Detail: firstNonEmpty(review.Reason, review.Evidence, review.Error, "adopted PR did not pass loopreview"),
		})
		return
	}
	releaseDecision := orchestrationcost.BindReleaseDecision(orchestrationcost.CheckReleaseGate(tickReport.OrchestrationCost), adopted.Number)
	tickReport.OrchestrationCost = orchestrationcost.ApplyReleaseDecision(tickReport.OrchestrationCost, releaseDecision)
	if !releaseDecision.Allowed {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "orchestration-cost-release", Issue: issueNumber, PR: pr, Detail: formatCostDecision(releaseDecision)})
		return
	}
	runTickRiskGateAndPreProdMerge(ctx, opts, tickReport, tickReviewCandidate{Issue: issueNumber, PR: pr}, adopted.Number)
}

func tickReviewResultFromLoopreview(issueNumber int, pr string, prNumber int, result loopreview.Result) TickReviewResult {
	verdict := loopreview.NormalizeVerdict(result.Verdict)
	return TickReviewResult{
		Issue:             issueNumber,
		PR:                pr,
		PRNumber:          prNumber,
		Verdict:           verdict.Verdict,
		SpecConformance:   verdict.SpecConformance,
		Evidence:          verdict.Evidence,
		Reason:            verdict.Reason,
		NextAction:        verdict.NextAction,
		RenderedArtifacts: copyRenderedArtifacts(verdict.RenderedArtifacts),
		Findings:          append([]loopreview.Finding(nil), verdict.Findings...),
		Report:            verdict.Report,
	}
}

func renderTickDispatchRecoveryContext(result DispatchWaveIssueResult) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Recovery context for dispatch failure on issue #%d\n\n", result.Issue)
	if strings.TrimSpace(result.PR) != "" {
		fmt.Fprintf(&out, "- PR: %s\n", result.PR)
	}
	if strings.TrimSpace(result.Branch) != "" {
		fmt.Fprintf(&out, "- Branch: %s\n", result.Branch)
	}
	if strings.TrimSpace(result.AttemptPath) != "" {
		fmt.Fprintf(&out, "- Attempt path: %s\n", result.AttemptPath)
	}
	if strings.TrimSpace(result.RecoveryContextPath) != "" {
		fmt.Fprintf(&out, "- Recovery context path: %s\n", result.RecoveryContextPath)
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## Dispatch failure")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "```text")
	fmt.Fprintln(&out, result.Error)
	fmt.Fprintln(&out, "```")
	return out.String()
}

func renderTickReviewRecoveryContext(item tickReviewCandidate, result loopreview.Result) string {
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Recovery context for loopreview failure on issue #%d\n\n", item.Issue)
	if strings.TrimSpace(item.PR) != "" {
		fmt.Fprintf(&out, "- PR: %s\n", item.PR)
	}
	fmt.Fprintf(&out, "- Verdict: %s\n", result.Verdict.Verdict)
	if strings.TrimSpace(result.Verdict.SpecConformance) != "" {
		fmt.Fprintf(&out, "- Spec conformance: %s\n", result.Verdict.SpecConformance)
	}
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "## Verifier evidence")
	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "```text")
	fmt.Fprintln(&out, result.Verdict.Evidence)
	fmt.Fprintln(&out, "```")
	if len(result.Verdict.Findings) > 0 {
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "## Verifier findings")
		fmt.Fprintln(&out)
		fmt.Fprintln(&out, "```text")
		for _, finding := range result.Verdict.Findings {
			fmt.Fprintf(&out, "- %s %s: %s\n", finding.Severity, finding.File, finding.Note)
		}
		fmt.Fprintln(&out, "```")
	}
	return out.String()
}

func recoveryBlockedDetail(report string) string {
	for _, line := range strings.Split(strings.ReplaceAll(report, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "BLOCKED:") {
			return line
		}
	}
	return ""
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

	riskGateRecordID := fmt.Sprintf("pr-%d", prNumber)
	emitCIProgress(ctx, opts.Progress, opts.Clock(), opts.RunID, item.Issue, item.PR, "risk-gate", riskGateRecordID, progress.KnownWaitingCI, progress.KnownWaitingCI, "waiting for PR checks", false)
	decision, err := opts.RiskGate(ctx, RiskGateOptions{
		Reader:             gateReader,
		PRNumber:           prNumber,
		RequiredChecks:     opts.RequiredChecks,
		AdditionalRedLines: opts.AdditionalRiskRedLines,
		WaitForChecks:      opts.WaitForChecks,
		WaitPolicy:         opts.WaitPolicy,
		WaitClock:          opts.WaitClock,
		WaitReceipt: func(ctx context.Context, receipt waitstate.Receipt) error {
			at, parseErr := time.Parse(time.RFC3339Nano, receipt.OccurredAt)
			if parseErr != nil {
				at = opts.Clock().UTC()
			}
			emitCIProgress(ctx, opts.Progress, at, opts.RunID, item.Issue, item.PR, "risk-gate", riskGateRecordID, progress.KnownWaitingCI, progress.KnownWaitingCI, "waiting for PR checks", false)
			return nil
		},
	})
	waitCostErr := recordRiskGateWaitCost(tickReport, prNumber, decision.Wait)
	gateResult := TickRiskGateResult{
		Issue:          item.Issue,
		PR:             item.PR,
		PRNumber:       prNumber,
		Status:         decision.Status,
		RequiredChecks: append([]string(nil), decision.RequiredChecks...),
		ChangedFiles:   append([]string(nil), decision.ChangedFiles...),
		Checks:         append([]gh.Check(nil), decision.Checks...),
		RedLines:       append([]RiskRedLine(nil), decision.RedLines...),
		Wait:           decision.Wait,
	}
	if waitCostErr != nil {
		gateResult.Status = RiskGateStatusNeedsHuman
		gateResult.Error = waitCostErr.Error()
		emitCIProgress(ctx, opts.Progress, opts.Clock(), opts.RunID, item.Issue, item.PR, "risk-gate", riskGateRecordID, RiskGateStatusNeedsHuman, progress.KnownBlocked, "orchestration cost persistence failed: "+waitCostErr.Error(), true)
		tickReport.RiskGates = append(tickReport.RiskGates, gateResult)
		return
	}
	if err != nil {
		gateResult.Status = RiskGateStatusNeedsHuman
		gateResult.Error = err.Error()
		emitCIProgress(ctx, opts.Progress, opts.Clock(), opts.RunID, item.Issue, item.PR, "risk-gate", riskGateRecordID, RiskGateStatusNeedsHuman, progress.KnownBlocked, "risk gate check read failed: "+err.Error(), true)
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
		emitCIProgress(ctx, opts.Progress, opts.Clock(), opts.RunID, item.Issue, item.PR, "risk-gate", riskGateRecordID, RiskGateStatusNeedsHuman, progress.KnownBlocked, "risk gate requires human review", true)
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{
			Step:   "risk-gate",
			Issue:  item.Issue,
			PR:     item.PR,
			Detail: formatRiskRedLines(decision.RedLines),
		})
		return
	}
	emitCIProgress(ctx, opts.Progress, opts.Clock(), opts.RunID, item.Issue, item.PR, "risk-gate", riskGateRecordID, RiskGateStatusClean, progress.KnownTerminal, "risk gate checks are clean", true)

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

	priorStableCommit := readTickPriorStableCommit(ctx, opts, opts.PreProdBranch)
	deliveryStarted := time.Now()
	merged, err := opts.PreProdWriter.MergeToPreProd(ctx, prNumber, opts.PreProdBranch)
	deliveryEvent := orchestrationcost.DeterministicEvent(
		fmt.Sprintf("delivery:pre-prod-merge:%d", prNumber), orchestrationcost.RoleDelivery, orchestrationcost.ActivityPhase,
		"pre-prod merge is deterministic delivery",
	)
	deliveryEvent.DurationMS = time.Since(deliveryStarted).Milliseconds()
	recordTickCostEvent(tickReport, deliveryEvent)
	mergeResult := TickPreProdMergeResult{
		Issue:             item.Issue,
		PR:                item.PR,
		PRNumber:          prNumber,
		Branch:            opts.PreProdBranch,
		PriorStableCommit: priorStableCommit,
		Status:            TickStatusSucceeded,
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
	consumedCost, consumeErr := orchestrationcost.MarkReleaseConsumed(tickReport.OrchestrationCost, prNumber)
	if consumeErr != nil {
		tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(tickReport.OrchestrationCost, orchestrationcost.AccountingFailure(consumeErr))
		tickReport.Failures = append(tickReport.Failures, TickIssue{Step: "orchestration-cost", Issue: item.Issue, PR: item.PR, Detail: consumeErr.Error()})
		return
	}
	tickReport.OrchestrationCost = consumedCost
	if err := persistTickCostReport(tickReport); err != nil {
		return
	}
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

	preProdRecordID := firstNonEmpty(mergeResult.SHA, opts.PreProdBranch)
	emitCIProgress(ctx, opts.Progress, opts.Clock(), opts.RunID, item.Issue, item.PR, "pre-prod-health", preProdRecordID, progress.KnownWaitingCI, progress.KnownWaitingCI, "waiting for pre-prod branch checks", false)
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
		emitCIProgress(ctx, opts.Progress, opts.Clock(), opts.RunID, item.Issue, item.PR, "pre-prod-health", preProdRecordID, PreProdHealthStatusUnknown, progress.KnownBlocked, "pre-prod branch check read failed: "+err.Error(), true)
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
	emitCIProgress(ctx, opts.Progress, opts.Clock(), opts.RunID, item.Issue, item.PR, "pre-prod-health", preProdRecordID, health.Status, knownPreProdHealthState(health.Status), preProdHealthProgressSummary(health), health.Status != PreProdHealthStatusPending)
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
	case PreProdHealthStatusPending:
		return
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

func knownPreProdHealthState(status string) string {
	switch status {
	case PreProdHealthStatusPending:
		return progress.KnownWaitingCI
	case PreProdHealthStatusGreen:
		return progress.KnownTerminal
	default:
		return progress.KnownBlocked
	}
}

func preProdHealthProgressSummary(health TickPreProdHealthResult) string {
	switch health.Status {
	case PreProdHealthStatusGreen:
		return "pre-prod branch checks are green"
	case PreProdHealthStatusRed:
		return "pre-prod branch checks are red"
	case PreProdHealthStatusPending:
		return "pre-prod branch checks are still pending"
	default:
		if strings.TrimSpace(health.Error) != "" {
			return "pre-prod branch check status is unknown: " + health.Error
		}
		if len(health.Problems) > 0 {
			return "pre-prod branch check status is unknown: " + strings.Join(health.Problems, ", ")
		}
		return "pre-prod branch check status is unknown"
	}
}

func runTickPreProdRevert(ctx context.Context, opts TickOptions, tickReport *TickReport, item tickReviewCandidate, prNumber int, mergeResult TickPreProdMergeResult, health TickPreProdHealthResult) {
	reverted, err := opts.PreProdWriter.RevertOnPreProd(ctx, prNumber, opts.PreProdBranch, mergeResult.SHA)
	revertResult := TickPreProdRevertResult{
		Issue:             item.Issue,
		PR:                item.PR,
		PRNumber:          prNumber,
		Branch:            opts.PreProdBranch,
		RevertedSHA:       mergeResult.SHA,
		MergeCommit:       mergeResult.SHA,
		PriorStableCommit: mergeResult.PriorStableCommit,
		Status:            TickStatusSucceeded,
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
	revertResult.MergeCommit = firstNonEmpty(revertResult.MergeCommit, revertResult.RevertedSHA)
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

func readTickPriorStableCommit(ctx context.Context, opts TickOptions, branch string) string {
	reader, ok := opts.Reader.(BranchHeadReader)
	if !ok {
		writeTickWarning(opts, "warning: github reader does not support branch head reads before pre-prod merge\n")
		return ""
	}
	sha, err := reader.BranchHeadSHA(ctx, branch)
	if err != nil {
		writeTickWarning(opts, "warning: could not read %s head before pre-prod merge: %v\n", branch, err)
		return ""
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		writeTickWarning(opts, "warning: %s head read before pre-prod merge returned an empty SHA\n", branch)
	}
	return sha
}

func writeTickWarning(opts TickOptions, format string, args ...any) {
	out := opts.Stderr
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, format, args...)
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
		if item.Step == "orchestration-cost" || item.Step == "orchestration-cost-release" {
			return TickStopOrchestrationCostBudget
		}
		if item.Step == "recover" {
			return TickStopRecoverNeedsHuman
		}
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
	event   tickLedgerEvent
	path    string
	index   int
	line    string
	modTime time.Time
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
		var modTime time.Time
		if info, err := os.Stat(path); err == nil {
			modTime = info.ModTime()
		}
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
			records = append(records, tickLedgerRecord{event: event, path: path, index: i, line: line, modTime: modTime})
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		leftTime, leftOK := parseTickLedgerTimestamp(records[i].event.Timestamp)
		rightTime, rightOK := parseTickLedgerTimestamp(records[j].event.Timestamp)
		if leftOK && rightOK && !leftTime.Equal(rightTime) {
			return leftTime.Before(rightTime)
		}
		if leftOK && rightOK {
			if before, ok := tickLedgerRecordModTimeBefore(records[i], records[j]); ok {
				return before
			}
		}
		if leftOK != rightOK {
			return leftOK
		}
		if records[i].event.Timestamp != records[j].event.Timestamp {
			return records[i].event.Timestamp < records[j].event.Timestamp
		}
		if before, ok := tickLedgerRecordModTimeBefore(records[i], records[j]); ok {
			return before
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

func tickLedgerRecordModTimeBefore(left, right tickLedgerRecord) (bool, bool) {
	if left.modTime.IsZero() || right.modTime.IsZero() || left.modTime.Equal(right.modTime) {
		return false, false
	}
	return left.modTime.Before(right.modTime), true
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

func markPendingVerifierCandidates(tickReport *TickReport, results []DispatchWaveIssueResult) {
	if tickReport == nil {
		return
	}
	var refusal *orchestrationcost.Decision
	for i := len(tickReport.OrchestrationCost.BudgetDecisions) - 1; i >= 0; i-- {
		decision := tickReport.OrchestrationCost.BudgetDecisions[i]
		if !decision.Allowed {
			copy := decision
			refusal = &copy
			break
		}
	}
	if refusal == nil {
		return
	}
	existing := map[int]bool{}
	for _, decision := range tickReport.OrchestrationCost.BudgetDecisions {
		if !decision.Allowed && !decision.Consumed && decision.PRNumber > 0 {
			existing[decision.PRNumber] = true
		}
	}
	for _, item := range reviewablePRs(results) {
		prNumber, ok := parseTickPRNumber(item.PR)
		if !ok || existing[prNumber] {
			continue
		}
		decision := *refusal
		decision.PRNumber = prNumber
		decision.Consumed = false
		decision.Evidence = append(append([]string(nil), refusal.Evidence...), fmt.Sprintf("pending-verifier-pr=%d", prNumber))
		tickReport.OrchestrationCost = orchestrationcost.ApplyBudgetDecision(tickReport.OrchestrationCost, decision)
		existing[prNumber] = true
	}
}

func restoredReleaseCandidate(readySet report.ReadySetReport, cost orchestrationcost.Report) (tickReviewCandidate, bool) {
	if cost.ReleaseGate == nil || cost.ReleaseGate.Consumed || cost.ReleaseGate.PRNumber <= 0 {
		return tickReviewCandidate{}, false
	}
	prNumber := cost.ReleaseGate.PRNumber
	for _, blocked := range readySet.Blocked {
		for _, pr := range blocked.OpenPRs {
			if pr.Number != prNumber {
				continue
			}
			return tickReviewCandidate{Issue: blocked.Issue, PR: firstNonEmpty(pr.URL, fmt.Sprintf("#%d", pr.Number))}, true
		}
	}
	return tickReviewCandidate{}, false
}

func restoredVerificationCandidate(readySet report.ReadySetReport, cost orchestrationcost.Report) (tickReviewCandidate, bool) {
	prNumber := 0
	for i := len(cost.BudgetDecisions) - 1; i >= 0; i-- {
		decision := cost.BudgetDecisions[i]
		if decision.Allowed || decision.Consumed || decision.PRNumber <= 0 {
			continue
		}
		prNumber = decision.PRNumber
		break
	}
	if prNumber <= 0 {
		return tickReviewCandidate{}, false
	}
	for _, blocked := range readySet.Blocked {
		for _, pr := range blocked.OpenPRs {
			if pr.Number == prNumber {
				return tickReviewCandidate{Issue: blocked.Issue, PR: firstNonEmpty(pr.URL, fmt.Sprintf("#%d", pr.Number))}, true
			}
		}
	}
	return tickReviewCandidate{}, false
}

func runTickRestoredVerification(ctx context.Context, opts TickOptions, tickReport *TickReport, item tickReviewCandidate) {
	prNumber, ok := parseTickPRNumber(item.PR)
	if !ok {
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "loopreview", Issue: item.Issue, PR: item.PR, Detail: "could not parse restored verifier pull request number"})
		return
	}
	result, err := runTickCostedLoopreview(ctx, opts, tickReport, prNumber,
		fmt.Sprintf("verifier:pr-%d", prNumber), orchestrationcost.RoleVerifier, true,
		fmt.Sprintf("pr=%d", prNumber), "resumed budget-blocked verifier call",
	)
	if err != nil {
		step := "loopreview"
		if tickReport.OrchestrationCost.Status == orchestrationcost.StatusNeedsHuman {
			step = "orchestration-cost"
		}
		tickReport.Reviews = append(tickReport.Reviews, TickReviewResult{Issue: item.Issue, PR: item.PR, PRNumber: prNumber, Verdict: loopreview.VerdictNeedsHuman, Findings: []loopreview.Finding{}, Error: err.Error()})
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: step, Issue: item.Issue, PR: item.PR, Detail: err.Error()})
		return
	}
	tickReport.Reviews = append(tickReport.Reviews, tickReviewResultFromLoopreview(item.Issue, item.PR, prNumber, result))
	switch result.Verdict.Verdict {
	case loopreview.VerdictPass:
		decision := orchestrationcost.BindReleaseDecision(orchestrationcost.CheckReleaseGate(tickReport.OrchestrationCost), prNumber)
		tickReport.OrchestrationCost = orchestrationcost.ApplyReleaseDecision(tickReport.OrchestrationCost, decision)
		if !decision.Allowed {
			tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "orchestration-cost-release", Issue: item.Issue, PR: item.PR, Detail: formatCostDecision(decision)})
			return
		}
		runTickRiskGateAndPreProdMerge(ctx, opts, tickReport, item, prNumber)
	case loopreview.VerdictFail:
		runTickRecoverReviewFailure(ctx, opts, tickReport, item, result)
	default:
		tickReport.NeedsHuman = append(tickReport.NeedsHuman, TickIssue{Step: "loopreview", Issue: item.Issue, PR: item.PR, Detail: firstNonEmpty(result.Verdict.Reason, result.Verdict.Evidence, "verifier returned needs-human")})
	}
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
