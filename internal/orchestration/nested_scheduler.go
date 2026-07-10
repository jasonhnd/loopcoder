package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/guardrails"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const (
	NestedStatusSucceeded  = "succeeded"
	NestedStatusFailed     = "failed"
	NestedStatusNeedsHuman = "needs-human"
	NestedStatusSkipped    = "skipped"
	NestedStatusCancelled  = "cancelled"
	NestedStatusTimedOut   = "timed_out"
	NestedStatusAbandoned  = "abandoned"
	NestedStatusQueued     = "queued"

	NestedEventChildQueued   = "nested.child.queued"
	NestedEventChildRunning  = "nested.child.running"
	NestedEventChildFinished = "nested.child.finished"
	NestedEventParentDone    = "nested.parent.finished"
)

type ChildRunExecutor func(ctx context.Context, child ChildRunPlan) (ChildRunResult, error)
type RecordNestedEventFunc func(repoPath, runID string, event state.Event) error

type NestedScheduleOptions struct {
	RepoPath         string
	ParentRunID      string
	BaseBranch       string
	ParentDepth      int
	MaxDepth         int
	MaxChildren      int
	ConcurrencyLimit int
	Children         []ChildRunPlan
	Budget           config.GuardrailBudget
	CircuitBreaker   config.GuardrailCircuitBreaker
	Now              time.Time
	Clock            func() time.Time

	Execute     ChildRunExecutor
	RecordEvent RecordNestedEventFunc
}

type ChildRunPlan struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	Issue      int             `json:"issue,omitempty"`
	Scope      []int           `json:"scope,omitempty"`
	Permission string          `json:"permission"`
	Required   bool            `json:"required,omitempty"`
	Optional   bool            `json:"optional,omitempty"`
	Depth      int             `json:"depth,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
}

type NestedScheduleReport struct {
	Version          int              `json:"version"`
	RepoPath         string           `json:"repo_path"`
	BaseBranch       string           `json:"base_branch"`
	ParentRunID      string           `json:"parent_run_id"`
	Status           string           `json:"status"`
	StartedAt        string           `json:"started_at"`
	FinishedAt       string           `json:"finished_at"`
	ConcurrencyLimit int              `json:"concurrency_limit"`
	Children         []ChildRunResult `json:"children"`
	Summary          NestedSummary    `json:"summary"`
}

type ChildRunResult struct {
	ID                  string           `json:"id"`
	RunID               string           `json:"run_id"`
	Issue               int              `json:"issue,omitempty"`
	Scope               []int            `json:"scope,omitempty"`
	Permission          string           `json:"permission"`
	Required            bool             `json:"required,omitempty"`
	Optional            bool             `json:"optional,omitempty"`
	Depth               int              `json:"depth"`
	Status              string           `json:"status"`
	StartedAt           string           `json:"started_at,omitempty"`
	FinishedAt          string           `json:"finished_at,omitempty"`
	Error               string           `json:"error,omitempty"`
	AttemptPath         string           `json:"attempt_path,omitempty"`
	RecoveryContextPath string           `json:"recovery_context_path,omitempty"`
	Report              *reporter.Report `json:"report,omitempty"`
}

type NestedSummary struct {
	RequiredCount   int `json:"required_count"`
	OptionalCount   int `json:"optional_count"`
	SucceededCount  int `json:"succeeded_count"`
	FailedCount     int `json:"failed_count"`
	NeedsHumanCount int `json:"needs_human_count"`
	SkippedCount    int `json:"skipped_count"`
	CancelledCount  int `json:"cancelled_count"`
}

func ScheduleNestedRuns(ctx context.Context, opts NestedScheduleOptions) (NestedScheduleReport, error) {
	if opts.Execute == nil {
		return NestedScheduleReport{}, fmt.Errorf("child run executor is required")
	}
	if strings.TrimSpace(opts.RepoPath) == "" {
		opts.RepoPath = "."
	}
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
	}
	if opts.ConcurrencyLimit <= 0 {
		opts.ConcurrencyLimit = lcdefaults.DispatchWaveThrottleLimit
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = lcdefaults.NestedSchedulerMaxDepth
	}
	if opts.MaxChildren <= 0 {
		opts.MaxChildren = lcdefaults.NestedSchedulerMaxChildren
	}
	if opts.RecordEvent == nil {
		opts.RecordEvent = state.AppendEvent
	}
	clock := opts.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	started := opts.Now
	if started.IsZero() {
		started = clock()
	}
	started = started.UTC()
	if strings.TrimSpace(opts.ParentRunID) == "" {
		opts.ParentRunID = state.RunIDForWave(started)
	}

	children, err := normalizeChildRunPlans(opts.Children, opts, started)
	if err != nil {
		return NestedScheduleReport{}, err
	}
	results := make([]ChildRunResult, len(children))
	dispatchJobs := make([]int, 0, len(children))
	plannedAttempts := 0
	for i, child := range children {
		result := childResultFromPlan(child)
		if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildQueued, started); err != nil {
			return NestedScheduleReport{}, err
		}
		if blocked, ok, err := evaluateNestedBudget(opts, child, plannedAttempts, started); err != nil {
			result.Status = NestedStatusNeedsHuman
			result.Error = err.Error()
			result.FinishedAt = state.FormatTimestamp(started)
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			results[i] = result
			continue
		} else if ok {
			result.Status = NestedStatusNeedsHuman
			result.Error = blocked
			result.FinishedAt = state.FormatTimestamp(started)
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			results[i] = result
			continue
		}
		plannedAttempts++
		if blocked, ok, err := evaluateNestedCircuit(opts, child, nil, started); err != nil {
			result.Status = NestedStatusNeedsHuman
			result.Error = err.Error()
			result.FinishedAt = state.FormatTimestamp(started)
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			results[i] = result
			continue
		} else if ok {
			result.Status = NestedStatusNeedsHuman
			result.Error = blocked
			result.FinishedAt = state.FormatTimestamp(started)
			if err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, started); err != nil {
				return NestedScheduleReport{}, err
			}
			results[i] = result
			continue
		}
		dispatchJobs = append(dispatchJobs, i)
	}

	var wg sync.WaitGroup
	var eventMu sync.Mutex
	var completeErr error
	setCompleteErr := func(err error) {
		if err == nil {
			return
		}
		eventMu.Lock()
		defer eventMu.Unlock()
		if completeErr == nil {
			completeErr = err
		}
	}
	sem := make(chan struct{}, opts.ConcurrencyLimit)
	for _, index := range dispatchJobs {
		select {
		case <-ctx.Done():
			result := childResultFromPlan(children[index])
			result.Status = normalizeNestedStatus(state.FailureStatus(ctx.Err()))
			if result.Status == "" {
				result.Status = NestedStatusCancelled
			}
			result.Error = ctx.Err().Error()
			finishedAt := clock().UTC()
			result.FinishedAt = state.FormatTimestamp(finishedAt)
			eventMu.Lock()
			err := recordNestedEvent(opts, opts.ParentRunID, children[index], result, NestedEventChildFinished, finishedAt)
			eventMu.Unlock()
			setCompleteErr(err)
			if err := recordNestedEvent(opts, children[index].RunID, children[index], result, NestedEventChildFinished, finishedAt); err != nil {
				setCompleteErr(err)
			}
			results[index] = result
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			defer func() { <-sem }()
			child := children[index]
			result := childResultFromPlan(child)
			result.Status = NestedStatusFailed
			result.StartedAt = state.FormatTimestamp(clock())
			eventMu.Lock()
			err := recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildRunning, parseOrClock(result.StartedAt, clock))
			eventMu.Unlock()
			setCompleteErr(err)
			if err := recordNestedEvent(opts, child.RunID, child, result, NestedEventChildRunning, parseOrClock(result.StartedAt, clock)); err != nil {
				setCompleteErr(err)
			}

			executed, err := opts.Execute(ctx, child)
			result = mergeChildResult(result, executed)
			if err != nil {
				result.Status = normalizeNestedStatus(state.FailureStatus(err))
				result.Error = err.Error()
			}
			result.Status = normalizeNestedStatus(result.Status)
			if result.FinishedAt == "" {
				result.FinishedAt = state.FormatTimestamp(clock())
			}
			result = applyNestedCircuitOutcome(opts, child, result, parseOrClock(result.FinishedAt, clock))
			results[index] = result

			finishedAt := parseOrClock(result.FinishedAt, clock)
			eventMu.Lock()
			err = recordNestedEvent(opts, opts.ParentRunID, child, result, NestedEventChildFinished, finishedAt)
			eventMu.Unlock()
			setCompleteErr(err)
			if err := recordNestedEvent(opts, child.RunID, child, result, NestedEventChildFinished, finishedAt); err != nil {
				setCompleteErr(err)
			}
		}(index)
	}
	wg.Wait()

	finished := clock().UTC()
	report := NestedScheduleReport{
		Version:          1,
		RepoPath:         filepath.ToSlash(opts.RepoPath),
		BaseBranch:       opts.BaseBranch,
		ParentRunID:      opts.ParentRunID,
		StartedAt:        state.FormatTimestamp(started),
		FinishedAt:       state.FormatTimestamp(finished),
		ConcurrencyLimit: opts.ConcurrencyLimit,
		Children:         results,
	}
	report.Summary = nestedSummary(results)
	report.Status = nestedParentStatus(results)
	if err := recordNestedParentDone(opts, report, finished); err != nil && completeErr == nil {
		completeErr = err
	}
	if completeErr != nil {
		return report, completeErr
	}
	return report, nil
}

func normalizeChildRunPlans(children []ChildRunPlan, opts NestedScheduleOptions, started time.Time) ([]ChildRunPlan, error) {
	if len(children) == 0 {
		return nil, nil
	}
	if len(children) > opts.MaxChildren {
		return nil, fmt.Errorf("child run count %d exceeds max children %d", len(children), opts.MaxChildren)
	}
	out := make([]ChildRunPlan, 0, len(children))
	seenRunIDs := map[string]bool{}
	for index, child := range children {
		child.ID = strings.TrimSpace(child.ID)
		if child.ID == "" {
			return nil, fmt.Errorf("child[%d].id is required", index)
		}
		if child.Required == child.Optional {
			return nil, fmt.Errorf("child %q must set exactly one of required or optional", child.ID)
		}
		child.Permission = strings.TrimSpace(child.Permission)
		if child.Permission == "" {
			return nil, fmt.Errorf("child %q permission is required", child.ID)
		}
		if child.Depth <= 0 {
			child.Depth = opts.ParentDepth + 1
		}
		if child.Depth > opts.MaxDepth {
			return nil, fmt.Errorf("child %q depth %d exceeds max depth %d", child.ID, child.Depth, opts.MaxDepth)
		}
		child.Scope = normalizeChildScope(child.Scope)
		if child.RunID == "" {
			child.RunID = state.RunIDForChild(child.ID, index, started)
		}
		if !state.IsRunID(child.RunID) {
			return nil, fmt.Errorf("child %q run id %q is invalid", child.ID, child.RunID)
		}
		if seenRunIDs[child.RunID] {
			return nil, fmt.Errorf("duplicate child run id %q", child.RunID)
		}
		seenRunIDs[child.RunID] = true
		out = append(out, child)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func childResultFromPlan(child ChildRunPlan) ChildRunResult {
	return ChildRunResult{
		ID:         child.ID,
		RunID:      child.RunID,
		Issue:      child.Issue,
		Scope:      append([]int(nil), child.Scope...),
		Permission: child.Permission,
		Required:   child.Required,
		Optional:   child.Optional,
		Depth:      child.Depth,
		Status:     NestedStatusQueued,
	}
}

func mergeChildResult(base, result ChildRunResult) ChildRunResult {
	if strings.TrimSpace(result.ID) != "" {
		base.ID = strings.TrimSpace(result.ID)
	}
	if strings.TrimSpace(result.RunID) != "" {
		base.RunID = strings.TrimSpace(result.RunID)
	}
	if result.Issue > 0 {
		base.Issue = result.Issue
	}
	if len(result.Scope) > 0 {
		base.Scope = normalizeChildScope(result.Scope)
	}
	if strings.TrimSpace(result.Permission) != "" {
		base.Permission = strings.TrimSpace(result.Permission)
	}
	if result.Required || result.Optional {
		base.Required = result.Required
		base.Optional = result.Optional
	}
	if result.Depth > 0 {
		base.Depth = result.Depth
	}
	if strings.TrimSpace(result.Status) != "" {
		base.Status = strings.TrimSpace(result.Status)
	}
	base.Error = strings.TrimSpace(result.Error)
	base.AttemptPath = strings.TrimSpace(result.AttemptPath)
	base.RecoveryContextPath = strings.TrimSpace(result.RecoveryContextPath)
	base.Report = result.Report
	if strings.TrimSpace(result.StartedAt) != "" {
		base.StartedAt = strings.TrimSpace(result.StartedAt)
	}
	if strings.TrimSpace(result.FinishedAt) != "" {
		base.FinishedAt = strings.TrimSpace(result.FinishedAt)
	}
	return base
}

func evaluateNestedBudget(opts NestedScheduleOptions, child ChildRunPlan, plannedAttempts int, now time.Time) (string, bool, error) {
	if !opts.Budget.Enabled() {
		return "", false, nil
	}
	decision := guardrails.EvaluateBudget(guardrails.BudgetOptions{
		RepoPath:         opts.RepoPath,
		RunID:            opts.ParentRunID,
		BaseBranch:       opts.BaseBranch,
		Issue:            childGuardrailIssue(child),
		ScopeIssues:      childGuardrailScope(child),
		Budget:           opts.Budget,
		PlannedAttempts:  plannedAttempts,
		ProposedAttempts: 1,
		Now:              now,
	})
	if _, err := guardrails.RecordDecision(opts.RepoPath, decision); err != nil {
		return "", false, fmt.Errorf("needs-human: guardrails.budget ledger write failed: %v", err)
	}
	if !decision.Allowed {
		return decision.Message, true, nil
	}
	return "", false, nil
}

func evaluateNestedCircuit(opts NestedScheduleOptions, child ChildRunPlan, outcome *guardrails.CircuitOutcome, now time.Time) (string, bool, error) {
	if !opts.CircuitBreaker.Enabled() {
		return "", false, nil
	}
	decision := guardrails.EvaluateCircuitBreaker(guardrails.CircuitOptions{
		RepoPath:       opts.RepoPath,
		RunID:          opts.ParentRunID,
		BaseBranch:     opts.BaseBranch,
		Issue:          childGuardrailIssue(child),
		ScopeIssues:    childGuardrailScope(child),
		CircuitBreaker: opts.CircuitBreaker,
		Outcome:        outcome,
		Now:            now,
	})
	if _, err := guardrails.RecordDecision(opts.RepoPath, decision); err != nil {
		return "", false, fmt.Errorf("needs-human: guardrails.circuit_breaker ledger write failed: %v", err)
	}
	if !decision.Allowed {
		return decision.Message, true, nil
	}
	return "", false, nil
}

func applyNestedCircuitOutcome(opts NestedScheduleOptions, child ChildRunPlan, result ChildRunResult, now time.Time) ChildRunResult {
	if !opts.CircuitBreaker.Enabled() || result.Status == NestedStatusNeedsHuman {
		return result
	}
	blockedMessage, blocked, err := evaluateNestedCircuit(opts, child, &guardrails.CircuitOutcome{
		Kind:             guardrails.CircuitOutcomeWave,
		MaterialProgress: nestedResultMaterialProgress(result),
		Detail:           result.Status,
	}, now)
	if err != nil {
		result.Status = NestedStatusNeedsHuman
		result.Error = err.Error()
		return result
	}
	if blocked {
		result.Status = NestedStatusNeedsHuman
		result.Error = blockedMessage
	}
	return result
}

func nestedResultMaterialProgress(result ChildRunResult) bool {
	if result.Status == NestedStatusSucceeded {
		return true
	}
	return strings.TrimSpace(result.AttemptPath) != "" || strings.TrimSpace(result.ReportHeader()) != ""
}

func (r ChildRunResult) ReportHeader() string {
	if r.Report == nil {
		return ""
	}
	return r.Report.Header()
}

func childGuardrailIssue(child ChildRunPlan) int {
	if child.Issue > 0 {
		return child.Issue
	}
	for _, issue := range child.Scope {
		if issue > 0 {
			return issue
		}
	}
	return 1
}

func childGuardrailScope(child ChildRunPlan) []int {
	if len(child.Scope) > 0 {
		return append([]int(nil), child.Scope...)
	}
	if child.Issue > 0 {
		return []int{child.Issue}
	}
	return []int{childGuardrailIssue(child)}
}

func normalizeNestedStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case NestedStatusSucceeded, "success", "completed", "complete", "done":
		return NestedStatusSucceeded
	case NestedStatusNeedsHuman, "needs_human", "needs human":
		return NestedStatusNeedsHuman
	case NestedStatusSkipped:
		return NestedStatusSkipped
	case NestedStatusCancelled, "canceled":
		return NestedStatusCancelled
	case NestedStatusTimedOut, "timeout", "timed-out":
		return NestedStatusTimedOut
	case NestedStatusAbandoned:
		return NestedStatusAbandoned
	case "", NestedStatusFailed, "failure", "error":
		return NestedStatusFailed
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

func nestedParentStatus(results []ChildRunResult) string {
	needsHuman := false
	for _, result := range results {
		if !result.Required {
			continue
		}
		switch result.Status {
		case NestedStatusFailed, NestedStatusCancelled, NestedStatusTimedOut, NestedStatusAbandoned, NestedStatusSkipped:
			return NestedStatusFailed
		case NestedStatusNeedsHuman:
			needsHuman = true
		}
	}
	if needsHuman {
		return NestedStatusNeedsHuman
	}
	return NestedStatusSucceeded
}

func nestedSummary(results []ChildRunResult) NestedSummary {
	var summary NestedSummary
	for _, result := range results {
		if result.Required {
			summary.RequiredCount++
		}
		if result.Optional {
			summary.OptionalCount++
		}
		switch result.Status {
		case NestedStatusSucceeded:
			summary.SucceededCount++
		case NestedStatusFailed:
			summary.FailedCount++
		case NestedStatusNeedsHuman:
			summary.NeedsHumanCount++
		case NestedStatusSkipped:
			summary.SkippedCount++
		case NestedStatusCancelled:
			summary.CancelledCount++
		}
	}
	return summary
}

func recordNestedEvent(opts NestedScheduleOptions, runID string, child ChildRunPlan, result ChildRunResult, eventName string, at time.Time) error {
	details, err := json.Marshal(nestedChildEventDetails{
		ParentRunID: opts.ParentRunID,
		Child:       child,
		Result:      result,
	})
	if err != nil {
		return fmt.Errorf("marshal nested child event details: %w", err)
	}
	return opts.RecordEvent(opts.RepoPath, runID, state.Event{
		Timestamp: state.FormatTimestamp(at),
		RunID:     runID,
		JobID:     "nested-scheduler",
		Issue:     child.Issue,
		Phase:     "nested-scheduler",
		Status:    result.Status,
		LogBytes:  0,
		Event:     eventName,
		Outcome:   result.Status,
		Details:   json.RawMessage(details),
	})
}

func recordNestedParentDone(opts NestedScheduleOptions, report NestedScheduleReport, at time.Time) error {
	details, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal nested parent event details: %w", err)
	}
	return opts.RecordEvent(opts.RepoPath, opts.ParentRunID, state.Event{
		Timestamp: state.FormatTimestamp(at),
		RunID:     opts.ParentRunID,
		JobID:     "nested-scheduler",
		Phase:     "nested-scheduler",
		Status:    report.Status,
		LogBytes:  0,
		Event:     NestedEventParentDone,
		Outcome:   report.Status,
		Details:   json.RawMessage(details),
	})
}

type nestedChildEventDetails struct {
	ParentRunID string         `json:"parent_run_id"`
	Child       ChildRunPlan   `json:"child"`
	Result      ChildRunResult `json:"result"`
}

func normalizeChildScope(scope []int) []int {
	if len(scope) == 0 {
		return nil
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(scope))
	for _, issue := range scope {
		if issue <= 0 || seen[issue] {
			continue
		}
		seen[issue] = true
		out = append(out, issue)
	}
	sort.Ints(out)
	return out
}

func parseOrClock(value string, clock func() time.Time) time.Time {
	if parsed, err := state.ParseTimestamp(value); err == nil {
		return parsed
	}
	return clock().UTC()
}
