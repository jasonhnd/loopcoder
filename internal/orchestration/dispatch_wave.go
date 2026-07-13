package orchestration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/recovery"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

const (
	DispatchWaveStatusSucceeded  = "succeeded"
	DispatchWaveStatusFailed     = "failed"
	DispatchWaveStatusSkipped    = "skipped"
	DispatchWaveStatusNeedsHuman = "needs-human"
	DispatchWaveStatusCancelled  = "cancelled"
	DispatchWaveStatusTimedOut   = "timed_out"
	DispatchWaveStatusAbandoned  = "abandoned"
)

type ReadySetFunc func(ctx context.Context, opts Options) (report.ReadySetReport, error)
type WorkerDispatchFunc func(ctx context.Context, opts worker.Options) (worker.Result, error)
type LoadAttemptsFunc func(repoPath, runID string) ([]state.Attempt, error)
type DispatchWaveIssueCompleteFunc func(DispatchWaveIssueComplete) error

type DispatchWaveIssueComplete struct {
	RunID  string
	Result DispatchWaveIssueResult
}

type DispatchWaveOptions struct {
	Reader         GitHubReader
	RepoPath       string
	BaseBranch     string
	RunID          string
	IssueNumbers   []int
	ReadySet       *report.ReadySetReport
	Provider       string
	Model          string
	Effort         string
	Timeout        time.Duration
	ConfigFromBase bool
	ThrottleLimit  int
	Thresholds     config.ResilienceWorker
	Budget         config.GuardrailBudget
	CircuitBreaker config.GuardrailCircuitBreaker
	ProcessAlive   ProcessAliveFunc
	Now            time.Time
	Stderr         io.Writer

	ComputeReadySet ReadySetFunc
	Dispatch        WorkerDispatchFunc
	LoadAttempts    LoadAttemptsFunc
	OnIssueComplete DispatchWaveIssueCompleteFunc
}

type DispatchWaveReport struct {
	Repo            string
	RepoPath        string
	BaseBranch      string
	RunID           string
	IssuesRequested []int
	StartedAt       string
	FinishedAt      string
	Results         []DispatchWaveIssueResult
}

type DispatchWaveIssueResult struct {
	Issue               int
	Status              string
	Branch              string
	PR                  string
	AttemptPath         string
	RecoveryContextPath string
	Error               string
	Reason              string                    `json:"reason,omitempty"`
	NextAction          string                    `json:"next_action,omitempty"`
	ConfiguredEvidence  []config.EvidenceArtifact `json:"configured_evidence,omitempty"`
	Report              *reporter.Report          `json:"report,omitempty"`
}

func DispatchWave(ctx context.Context, opts DispatchWaveOptions) (DispatchWaveReport, error) {
	if opts.Reader == nil {
		return DispatchWaveReport{}, fmt.Errorf("github reader is required")
	}
	if opts.Dispatch == nil {
		return DispatchWaveReport{}, fmt.Errorf("worker dispatch is required")
	}
	if opts.ComputeReadySet == nil {
		opts.ComputeReadySet = ComputeReadySet
	}
	if opts.LoadAttempts == nil {
		opts.LoadAttempts = state.LoadAttempts
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
	}
	if opts.ThrottleLimit <= 0 {
		opts.ThrottleLimit = lcdefaults.DispatchWaveThrottleLimit
	}
	started := opts.Now
	if started.IsZero() {
		started = time.Now().UTC()
	} else {
		started = started.UTC()
	}
	if strings.TrimSpace(opts.RunID) == "" {
		opts.RunID = state.RunIDForWave(started)
	}

	var preflight report.ReadySetReport
	var err error
	havePreflight := false
	selected := normalizeIssueNumbers(opts.IssueNumbers)
	if len(selected) == 0 && opts.ReadySet != nil {
		selected = issueNumbersFromReadySet(*opts.ReadySet)
	}
	if len(selected) == 0 {
		preflight, err = computeWaveReadySet(ctx, opts, started)
		if err != nil {
			return DispatchWaveReport{}, err
		}
		havePreflight = true
		selected = issueNumbersFromReadySet(preflight)
	}
	if !havePreflight {
		preflight, err = computeWaveReadySet(ctx, opts, started)
		if err != nil {
			return DispatchWaveReport{}, err
		}
	}

	results := make([]DispatchWaveIssueResult, len(selected))
	readyByIssue := readyIssuesByNumber(preflight.Ready)
	blockedByIssue := blockedIssuesByNumber(preflight.Blocked)
	priorAttemptCounts := loadIssueAttemptCounts(opts.RepoPath, opts.RunID, opts.LoadAttempts)
	dispatchJobs := make([]int, 0, len(selected))
	plannedAttempts := 0
	for i, issue := range selected {
		result := DispatchWaveIssueResult{Issue: issue}
		if _, ok := readyByIssue[issue]; !ok {
			result.Status = DispatchWaveStatusSkipped
			result.Error = "issue was not ready during preflight"
			if blocked, ok := blockedByIssue[issue]; ok {
				result.Error = blocked.Reason
				if len(blocked.OpenPRs) > 0 {
					result.PR = blocked.OpenPRs[0].URL
					result.Branch = blocked.OpenPRs[0].Head
				}
				if len(blocked.Attempts) > 0 {
					latest := blocked.Attempts[len(blocked.Attempts)-1]
					result.Branch = firstNonEmpty(result.Branch, latest.Branch)
					result.AttemptPath = latest.Path
				}
			}
			result = withDispatchWaveDecision(result)
			results[i] = result
			continue
		}
		if opts.Budget.Enabled() {
			decision := guardrails.EvaluateBudget(guardrails.BudgetOptions{
				RepoPath:         opts.RepoPath,
				RunID:            opts.RunID,
				BaseBranch:       opts.BaseBranch,
				Issue:            issue,
				ScopeIssues:      selected,
				Budget:           opts.Budget,
				PlannedAttempts:  plannedAttempts,
				ProposedAttempts: 1,
				Now:              started,
			})
			if _, err := guardrails.RecordDecision(opts.RepoPath, decision); err != nil {
				result.Status = DispatchWaveStatusNeedsHuman
				result.Error = fmt.Sprintf("needs-human: guardrails.budget ledger write failed: %v", err)
				result = withDispatchWaveDecision(result)
				results[i] = result
				continue
			}
			if !decision.Allowed {
				result.Status = DispatchWaveStatusNeedsHuman
				result.Error = decision.Message
				result.AttemptPath = decision.LatestAttemptPath
				result.RecoveryContextPath = decision.RecoveryContextPath
				result = withDispatchWaveDecision(result)
				results[i] = result
				continue
			}
			plannedAttempts++
		}
		if opts.CircuitBreaker.Enabled() {
			decision := guardrails.EvaluateCircuitBreaker(guardrails.CircuitOptions{
				RepoPath:       opts.RepoPath,
				RunID:          opts.RunID,
				BaseBranch:     opts.BaseBranch,
				Issue:          issue,
				ScopeIssues:    selected,
				CircuitBreaker: opts.CircuitBreaker,
				Now:            started,
			})
			if _, err := guardrails.RecordDecision(opts.RepoPath, decision); err != nil {
				result.Status = DispatchWaveStatusNeedsHuman
				result.Error = fmt.Sprintf("needs-human: guardrails.circuit_breaker ledger write failed: %v", err)
				result = withDispatchWaveDecision(result)
				results[i] = result
				continue
			}
			if !decision.Allowed {
				result.Status = DispatchWaveStatusNeedsHuman
				result.Error = decision.Message
				result.AttemptPath = decision.LatestAttemptPath
				result.RecoveryContextPath = decision.RecoveryContextPath
				result = withDispatchWaveDecision(result)
				results[i] = result
				continue
			}
		}
		dispatchJobs = append(dispatchJobs, i)
	}

	var wg sync.WaitGroup
	var completeMu sync.Mutex
	var completeErr error
	sem := make(chan struct{}, opts.ThrottleLimit)
	for _, index := range dispatchJobs {
		if err := ctx.Err(); err != nil {
			results[index] = recordAbandonedDispatchWaveChild(opts, selected[index], err, started)
			continue
		}
		select {
		case <-ctx.Done():
			results[index] = recordAbandonedDispatchWaveChild(opts, selected[index], ctx.Err(), started)
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			defer func() { <-sem }()
			result := dispatchWaveIssue(ctx, opts, selected[index])
			results[index] = result
			if opts.OnIssueComplete != nil {
				completeMu.Lock()
				if err := opts.OnIssueComplete(DispatchWaveIssueComplete{
					RunID:  opts.RunID,
					Result: result,
				}); err != nil && completeErr == nil {
					completeErr = err
				}
				completeMu.Unlock()
			}
		}(index)
	}
	wg.Wait()
	if opts.CircuitBreaker.Enabled() {
		recordDispatchWaveCircuitOutcomes(opts, selected, results, priorAttemptCounts, started)
	}

	finished := time.Now().UTC()
	if !opts.Now.IsZero() {
		finished = opts.Now.UTC()
	}
	report := DispatchWaveReport{
		Repo:            firstNonEmpty(preflight.Repo, opts.RepoPath),
		RepoPath:        filepath.ToSlash(opts.RepoPath),
		BaseBranch:      opts.BaseBranch,
		RunID:           opts.RunID,
		IssuesRequested: selected,
		StartedAt:       state.FormatTimestamp(started),
		FinishedAt:      state.FormatTimestamp(finished),
		Results:         results,
	}
	if completeErr != nil {
		return report, completeErr
	}
	return report, nil
}

func recordDispatchWaveCircuitOutcomes(opts DispatchWaveOptions, selected []int, results []DispatchWaveIssueResult, priorAttemptCounts map[int]int, now time.Time) {
	for i := range results {
		result := &results[i]
		if result.Status == DispatchWaveStatusNeedsHuman {
			continue
		}
		if result.Status == "" {
			continue
		}
		materialProgress := dispatchWaveResultMaterialProgress(opts, selected[i], *result, priorAttemptCounts[selected[i]])
		decision := guardrails.EvaluateCircuitBreaker(guardrails.CircuitOptions{
			RepoPath:       opts.RepoPath,
			RunID:          opts.RunID,
			BaseBranch:     opts.BaseBranch,
			Issue:          selected[i],
			ScopeIssues:    selected,
			CircuitBreaker: opts.CircuitBreaker,
			Outcome: &guardrails.CircuitOutcome{
				Kind:             guardrails.CircuitOutcomeWave,
				MaterialProgress: materialProgress,
				Detail:           result.Status,
			},
			Now: now,
		})
		if _, err := guardrails.RecordDecision(opts.RepoPath, decision); err != nil {
			if result.Status != DispatchWaveStatusSucceeded {
				result.Status = DispatchWaveStatusNeedsHuman
				result.Error = fmt.Sprintf("needs-human: guardrails.circuit_breaker ledger write failed: %v", err)
				*result = withDispatchWaveDecision(*result)
			}
			continue
		}
		if !decision.Allowed {
			result.Status = DispatchWaveStatusNeedsHuman
			result.Error = decision.Message
			result.AttemptPath = firstNonEmpty(result.AttemptPath, decision.LatestAttemptPath)
			result.RecoveryContextPath = firstNonEmpty(result.RecoveryContextPath, decision.RecoveryContextPath)
			*result = withDispatchWaveDecision(*result)
		}
	}
}

func dispatchWaveResultMaterialProgress(opts DispatchWaveOptions, issue int, result DispatchWaveIssueResult, priorAttemptCount int) bool {
	if strings.TrimSpace(result.PR) != "" || result.Status == DispatchWaveStatusSucceeded {
		return true
	}
	attempts, err := opts.LoadAttempts(opts.RepoPath, opts.RunID)
	if err != nil {
		return false
	}
	issueAttempts := groupAttemptsByIssue(attempts)[issue]
	if len(issueAttempts) <= priorAttemptCount {
		return false
	}
	for _, attempt := range issueAttempts[priorAttemptCount:] {
		if guardrails.AttemptHasMaterialProgress(opts.RepoPath, opts.RunID, attempt) {
			return true
		}
	}
	return false
}

func loadIssueAttemptCounts(repoPath, runID string, loadAttempts LoadAttemptsFunc) map[int]int {
	counts := map[int]int{}
	if loadAttempts == nil {
		return counts
	}
	attempts, err := loadAttempts(repoPath, runID)
	if err != nil {
		return counts
	}
	for _, attempt := range attempts {
		if attempt.Issue > 0 {
			counts[attempt.Issue]++
		}
	}
	return counts
}

func computeWaveReadySet(ctx context.Context, opts DispatchWaveOptions, now time.Time) (report.ReadySetReport, error) {
	attempts, err := opts.LoadAttempts(opts.RepoPath, opts.RunID)
	if err != nil {
		return report.ReadySetReport{}, fmt.Errorf("load attempts: %w", err)
	}
	readySet, err := opts.ComputeReadySet(ctx, Options{
		Reader:       opts.Reader,
		RepoPath:     opts.RepoPath,
		BaseBranch:   opts.BaseBranch,
		RunID:        opts.RunID,
		Attempts:     attempts,
		Thresholds:   opts.Thresholds,
		ProcessAlive: opts.ProcessAlive,
		Now:          now,
	})
	if err != nil {
		return report.ReadySetReport{}, fmt.Errorf("compute ready set: %w", err)
	}
	return readySet, nil
}

func dispatchWaveIssue(ctx context.Context, opts DispatchWaveOptions, issueNumber int) DispatchWaveIssueResult {
	result := DispatchWaveIssueResult{Issue: issueNumber}
	issue, err := opts.Reader.ViewIssue(ctx, issueNumber)
	if err != nil {
		result.Status = dispatchWaveFailureStatus(err, worker.Result{})
		result.Error = fmt.Sprintf("read issue #%d: %v", issueNumber, err)
		return withDispatchWaveDecision(result)
	}
	dispatchResult, err := opts.Dispatch(ctx, worker.Options{
		RepoPath:       opts.RepoPath,
		IssueNumber:    issueNumber,
		IssueTitle:     issue.Title,
		IssueBody:      issue.Body,
		BaseBranch:     opts.BaseBranch,
		RunID:          opts.RunID,
		Attempt:        1,
		Provider:       opts.Provider,
		Model:          opts.Model,
		Effort:         opts.Effort,
		Timeout:        opts.Timeout,
		ConfigFromBase: opts.ConfigFromBase,
		Stderr:         opts.Stderr,
	})
	if err != nil {
		result.Status = dispatchWaveFailureStatus(err, dispatchResult)
		result.Error = err.Error()
		result.Reason = firstNonEmpty(dispatchResult.Reason, result.Error)
		result.NextAction = dispatchResult.NextAction
		result.Report = dispatchResult.Report
		enrichDispatchWaveFailure(&result, opts)
		return withDispatchWaveDecision(result)
	}
	result.Status = firstNonEmpty(dispatchResult.Status, DispatchWaveStatusSucceeded)
	result.Branch = dispatchResult.Branch
	result.PR = dispatchResult.PR
	result.AttemptPath = dispatchResult.AttemptPath
	result.Reason = dispatchResult.Reason
	result.NextAction = dispatchResult.NextAction
	result.Report = dispatchResult.Report
	return withDispatchWaveDecision(result)
}

func dispatchWaveFailureStatus(err error, result worker.Result) string {
	if mapped := state.FailureStatus(err); mapped != state.StatusFailed {
		return mapped
	}
	status := state.NormalizeStatus(result.Status)
	if status != "" && status != state.StatusSucceeded && state.IsTerminalStatus(status) {
		return status
	}
	return state.StatusFailed
}

func recordAbandonedDispatchWaveChild(opts DispatchWaveOptions, issueNumber int, cause error, now time.Time) DispatchWaveIssueResult {
	status := DispatchWaveStatusAbandoned
	if errors.Is(cause, context.DeadlineExceeded) {
		status = DispatchWaveStatusTimedOut
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	jobID := fmt.Sprintf("job-%d-abandoned", issueNumber)
	branch := fmt.Sprintf("loop/issue-%d", issueNumber)
	recoveryPath := state.RecoveryBriefPath(opts.RepoPath, opts.RunID, jobID)
	errorText := dispatchWaveParentStopError(status, cause)
	_ = writeDispatchWaveChildRecovery(recoveryPath, issueNumber, branch, status, errorText)
	_, _ = state.WriteAttempt(opts.RepoPath, opts.RunID, state.AttemptRecord{
		Version:             1,
		JobID:               jobID,
		Issue:               issueNumber,
		Attempt:             1,
		Provider:            firstNonEmpty(opts.Provider, "unknown"),
		PID:                 0,
		Phase:               "parent_stopped_before_dispatch",
		Status:              status,
		Branch:              branch,
		RecoveryContextPath: recoveryPath,
		StartedAt:           state.FormatTimestamp(now),
		HeartbeatAt:         state.FormatTimestamp(now),
		LastProgressAt:      state.FormatTimestamp(now),
		LogBytes:            0,
		Error:               &errorText,
	})
	_ = state.AppendEvent(opts.RepoPath, opts.RunID, state.Event{
		Timestamp: state.FormatTimestamp(now),
		RunID:     opts.RunID,
		JobID:     jobID,
		Issue:     issueNumber,
		Phase:     "parent_stopped_before_dispatch",
		Status:    status,
		Error:     &errorText,
		Event:     "child_abandoned",
		Outcome:   status,
		Details: map[string]string{
			"recovery_context_path": filepath.ToSlash(recoveryPath),
		},
	})
	return withDispatchWaveDecision(DispatchWaveIssueResult{
		Issue:               issueNumber,
		Status:              status,
		Branch:              branch,
		AttemptPath:         state.AttemptPath(opts.RepoPath, opts.RunID, jobID),
		RecoveryContextPath: recoveryPath,
		Error:               errorText,
	})
}

func dispatchWaveParentStopError(status string, cause error) string {
	causeText := "parent context stopped before this child was dispatched"
	if cause != nil {
		causeText = cause.Error()
	}
	if status == DispatchWaveStatusTimedOut {
		return "parent run timed out before this child was dispatched; recover or mark needs-human: " + causeText
	}
	return "parent run stopped before this child was dispatched; child abandoned and must be recovered or marked needs-human: " + causeText
}

func writeDispatchWaveChildRecovery(path string, issueNumber int, branch, status, errorText string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := recovery.RenderBrief(recovery.BriefInput{
		IssueNumber:    issueNumber,
		IssueTitle:     "(not loaded; child did not start)",
		Branch:         branch,
		WorktreePath:   "(not created)",
		LogPath:        "(not created)",
		SummaryPath:    "(not created)",
		AttemptNumber:  1,
		LastPhase:      "parent_stopped_before_dispatch",
		Status:         status,
		Error:          errorText,
		ChangedFiles:   "(none; child did not start)",
		ExistingPRText: "unknown; child did not start",
		LogTail:        "(no log; child did not start)",
	})
	return os.WriteFile(path, []byte(body), 0o600)
}

func enrichDispatchWaveFailure(result *DispatchWaveIssueResult, opts DispatchWaveOptions) {
	attempts, err := opts.LoadAttempts(opts.RepoPath, opts.RunID)
	if err != nil {
		return
	}
	latest := latestAttempt(groupAttemptsByIssue(attempts)[result.Issue])
	if latest == nil {
		return
	}
	result.Branch = firstNonEmpty(result.Branch, latest.Branch)
	result.AttemptPath = firstNonEmpty(result.AttemptPath, latest.Path)
	result.RecoveryContextPath = firstNonEmpty(result.RecoveryContextPath, latest.RecoveryContextPath)
	if result.RecoveryContextPath == "" && latest.JobID != "" {
		recoveryPath := state.RecoveryBriefPathForRead(opts.RepoPath, opts.RunID, latest.JobID)
		if info, err := filepath.Abs(recoveryPath); err == nil {
			result.RecoveryContextPath = info
		} else {
			result.RecoveryContextPath = recoveryPath
		}
	}
}

func withDispatchWaveDecision(result DispatchWaveIssueResult) DispatchWaveIssueResult {
	if !dispatchWaveActionableStatus(result.Status) && strings.TrimSpace(result.Reason) == "" && strings.TrimSpace(result.NextAction) == "" && strings.TrimSpace(result.Error) == "" {
		return result
	}
	receipt := reporter.NormalizeDecision(reporter.DecisionInput{
		Status:             result.Status,
		ExplicitReason:     result.Reason,
		ConcreteError:      result.Error,
		ExplicitNextAction: result.NextAction,
	})
	result.Reason = receipt.Reason
	result.NextAction = receipt.NextAction
	return result
}

func dispatchWaveActionableStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", DispatchWaveStatusSucceeded:
		return false
	default:
		return true
	}
}

func normalizeIssueNumbers(numbers []int) []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(numbers))
	for _, number := range numbers {
		if number <= 0 || seen[number] {
			continue
		}
		seen[number] = true
		out = append(out, number)
	}
	return out
}

func issueNumbersFromReadySet(readySet report.ReadySetReport) []int {
	out := make([]int, 0, len(readySet.Ready))
	seen := map[int]bool{}
	for _, item := range readySet.Ready {
		if item.Issue <= 0 || seen[item.Issue] {
			continue
		}
		seen[item.Issue] = true
		out = append(out, item.Issue)
	}
	return out
}

func readyIssuesByNumber(ready []report.ReadyIssue) map[int]report.ReadyIssue {
	out := make(map[int]report.ReadyIssue, len(ready))
	for _, item := range ready {
		out[item.Issue] = item
	}
	return out
}

func blockedIssuesByNumber(blocked []report.BlockedIssue) map[int]report.BlockedIssue {
	out := make(map[int]report.BlockedIssue, len(blocked))
	for _, item := range blocked {
		out[item.Issue] = item
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
