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
	Reader           GitHubReader
	IssueWriter      compiler.IssueWriter
	RepoPath         string
	BaseBranch       string
	RunID            string
	WorkerProvider   string
	WorkerModel      string
	WorkerEffort     string
	VerifierProvider string
	VerifierModel    string
	VerifierEffort   string
	VerifierTimeout  time.Duration
	ThrottleLimit    int
	Thresholds       config.ResilienceWorker
	Budget           config.GuardrailBudget
	CircuitBreaker   config.GuardrailCircuitBreaker
	ProcessAlive     ProcessAliveFunc
	Clock            func() time.Time
	Stderr           io.Writer

	Compile         CompileFunc
	ComputeReadySet ReadySetFunc
	DispatchWave    DispatchWaveFunc
	Dispatch        WorkerDispatchFunc
	Loopreview      LoopreviewFunc
	StatePush       StatePushFunc
	LoadAttempts    LoadAttemptsFunc
}

type TickReport struct {
	Version      int                   `json:"version"`
	Repo         string                `json:"repo"`
	RepoPath     string                `json:"repo_path"`
	BaseBranch   string                `json:"base_branch"`
	RunID        string                `json:"run_id"`
	Status       string                `json:"status"`
	StopReason   string                `json:"stop_reason"`
	StartedAt    string                `json:"started_at"`
	FinishedAt   string                `json:"finished_at"`
	Compile      compiler.Report       `json:"compile"`
	ReadySet     report.ReadySetReport `json:"ready_set"`
	DispatchWave *DispatchWaveReport   `json:"dispatch_wave,omitempty"`
	Reviews      []TickReviewResult    `json:"reviews"`
	NeedsHuman   []TickIssue           `json:"needs_human"`
	Failures     []TickIssue           `json:"failures"`
	StatePush    *TickStatePush        `json:"state_push,omitempty"`
	Summary      TickSummary           `json:"summary"`
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
	CompiledCreatedCount   int `json:"compiled_created_count"`
	CompiledUpdatedCount   int `json:"compiled_updated_count"`
	CompiledUnchangedCount int `json:"compiled_unchanged_count"`
	CompiledClosedCount    int `json:"compiled_closed_count"`
	ReadyCount             int `json:"ready_count"`
	BlockedCount           int `json:"blocked_count"`
	DispatchedPRCount      int `json:"dispatched_pr_count"`
	ReviewPassCount        int `json:"review_pass_count"`
	ReviewFailCount        int `json:"review_fail_count"`
	ReviewNeedsHumanCount  int `json:"review_needs_human_count"`
	NeedsHumanCount        int `json:"needs_human_count"`
	FailureCount           int `json:"failure_count"`
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

	tickReport := TickReport{
		Version:    TickReportVersion,
		Repo:       filepath.ToSlash(opts.RepoPath),
		RepoPath:   filepath.ToSlash(opts.RepoPath),
		BaseBranch: opts.BaseBranch,
		RunID:      opts.RunID,
		StartedAt:  state.FormatTimestamp(started),
		Reviews:    []TickReviewResult{},
		NeedsHuman: []TickIssue{},
		Failures:   []TickIssue{},
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
		case loopreview.VerdictFail:
			tickReport.Failures = append(tickReport.Failures, TickIssue{
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
		return finish(TickStatusNeedsHuman, TickStopReviewNeedsHuman)
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
		fmt.Fprintln(&out, "- Human review can decide what to do with passing PRs later.")
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
