package orchestration

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/attestation"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

const (
	DispatchWaveStatusSucceeded  = "succeeded"
	DispatchWaveStatusFailed     = "failed"
	DispatchWaveStatusSkipped    = "skipped"
	DispatchWaveStatusNeedsHuman = "needs-human"
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
	ConfiguredEvidence  []config.EvidenceArtifact      `json:"configured_evidence,omitempty"`
	Attestation         *attestation.AttestationRecord `json:"attestation,omitempty"`
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
		opts.BaseBranch = "main"
	}
	if opts.ThrottleLimit <= 0 {
		opts.ThrottleLimit = 4
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
				results[i] = result
				continue
			}
			if !decision.Allowed {
				result.Status = DispatchWaveStatusNeedsHuman
				result.Error = decision.Message
				result.AttemptPath = decision.LatestAttemptPath
				result.RecoveryContextPath = decision.RecoveryContextPath
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
				results[i] = result
				continue
			}
			if !decision.Allowed {
				result.Status = DispatchWaveStatusNeedsHuman
				result.Error = decision.Message
				result.AttemptPath = decision.LatestAttemptPath
				result.RecoveryContextPath = decision.RecoveryContextPath
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
		select {
		case <-ctx.Done():
			results[index] = DispatchWaveIssueResult{
				Issue:  selected[index],
				Status: DispatchWaveStatusFailed,
				Error:  ctx.Err().Error(),
			}
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
			}
			continue
		}
		if !decision.Allowed {
			result.Status = DispatchWaveStatusNeedsHuman
			result.Error = decision.Message
			result.AttemptPath = firstNonEmpty(result.AttemptPath, decision.LatestAttemptPath)
			result.RecoveryContextPath = firstNonEmpty(result.RecoveryContextPath, decision.RecoveryContextPath)
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
		result.Status = DispatchWaveStatusFailed
		result.Error = fmt.Sprintf("read issue #%d: %v", issueNumber, err)
		return result
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
		ConfigFromBase: opts.ConfigFromBase,
		Stderr:         opts.Stderr,
	})
	if err != nil {
		result.Status = DispatchWaveStatusFailed
		result.Error = err.Error()
		result.Attestation = dispatchResult.Attestation
		enrichDispatchWaveFailure(&result, opts)
		return result
	}
	result.Status = firstNonEmpty(dispatchResult.Status, DispatchWaveStatusSucceeded)
	result.Branch = dispatchResult.Branch
	result.PR = dispatchResult.PR
	result.AttemptPath = dispatchResult.AttemptPath
	result.Attestation = dispatchResult.Attestation
	return result
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
		recoveryPath := state.RecoveryBriefPath(opts.RepoPath, opts.RunID, latest.JobID)
		if info, err := filepath.Abs(recoveryPath); err == nil {
			result.RecoveryContextPath = info
		} else {
			result.RecoveryContextPath = recoveryPath
		}
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
