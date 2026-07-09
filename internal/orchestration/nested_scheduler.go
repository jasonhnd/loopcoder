package orchestration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/report"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const (
	NestedScheduleStatusSucceeded  = "succeeded"
	NestedScheduleStatusFailed     = "failed"
	NestedScheduleStatusNeedsHuman = "needs-human"

	NestedScheduleStopCompleted       = "completed"
	NestedScheduleStopChildFailed     = "child-failed"
	NestedScheduleStopChildNeedsHuman = "child-needs-human"

	nestedChildScheduledEvent = "nested.child_scheduled"
	nestedChildCompletedEvent = "nested.child_completed"
)

type AppendEventFunc func(repoPath, runID string, event state.Event) error

type ChildRunSpec struct {
	ID            string
	RunID         string
	IssueNumbers  []int
	ReadySet      *report.ReadySetReport
	Required      bool
	Optional      bool
	ThrottleLimit int
}

type NestedScheduleOptions struct {
	Reader           GitHubReader
	RepoPath         string
	BaseBranch       string
	ParentRunID      string
	Children         []ChildRunSpec
	ConcurrencyLimit int
	Provider         string
	Model            string
	Effort           string
	ConfigFromBase   bool
	ThrottleLimit    int
	Thresholds       config.ResilienceWorker
	Budget           config.GuardrailBudget
	CircuitBreaker   config.GuardrailCircuitBreaker
	ProcessAlive     ProcessAliveFunc
	Now              time.Time
	Stderr           io.Writer

	ComputeReadySet ReadySetFunc
	DispatchWave    DispatchWaveFunc
	Dispatch        WorkerDispatchFunc
	LoadAttempts    LoadAttemptsFunc
	AppendEvent     AppendEventFunc
}

type NestedScheduleReport struct {
	Repo        string                `json:"repo,omitempty"`
	RepoPath    string                `json:"repo_path,omitempty"`
	BaseBranch  string                `json:"base_branch"`
	ParentRunID string                `json:"parent_run_id"`
	Status      string                `json:"status"`
	StopReason  string                `json:"stop_reason"`
	StartedAt   string                `json:"started_at"`
	FinishedAt  string                `json:"finished_at"`
	Children    []ChildRunResult      `json:"children"`
	Summary     NestedScheduleSummary `json:"summary"`
}

type ChildRunResult struct {
	ID           string              `json:"id"`
	RunID        string              `json:"run_id"`
	Required     bool                `json:"required"`
	IssueNumbers []int               `json:"issue_numbers"`
	Status       string              `json:"status"`
	Error        string              `json:"error,omitempty"`
	Wave         *DispatchWaveReport `json:"dispatch_wave,omitempty"`
}

type NestedScheduleSummary struct {
	ChildCount              int `json:"child_count"`
	RequiredChildCount      int `json:"required_child_count"`
	OptionalChildCount      int `json:"optional_child_count"`
	ChildSucceededCount     int `json:"child_succeeded_count"`
	ChildFailedCount        int `json:"child_failed_count"`
	ChildNeedsHumanCount    int `json:"child_needs_human_count"`
	RequiredFailedCount     int `json:"required_failed_count"`
	RequiredNeedsHumanCount int `json:"required_needs_human_count"`
}

type nestedChildTransitionDetails struct {
	ParentRunID  string `json:"parent_run_id"`
	ChildID      string `json:"child_id"`
	ChildRunID   string `json:"child_run_id"`
	Required     bool   `json:"required"`
	IssueNumbers []int  `json:"issue_numbers,omitempty"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
}

func NestedSchedule(ctx context.Context, opts NestedScheduleOptions) (NestedScheduleReport, error) {
	opts = withNestedScheduleDefaults(opts)
	children, err := normalizeChildRunSpecs(opts.ParentRunID, opts.Children)
	if err != nil {
		return NestedScheduleReport{}, err
	}
	started := opts.Now
	if started.IsZero() {
		started = time.Now().UTC()
	} else {
		started = started.UTC()
	}

	results := make([]ChildRunResult, len(children))
	sem := make(chan struct{}, opts.ConcurrencyLimit)
	var wg sync.WaitGroup
	for index, child := range children {
		select {
		case <-ctx.Done():
			results[index] = childResultFromError(child, NestedScheduleStatusFailed, ctx.Err().Error())
			continue
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(index int, child ChildRunSpec) {
			defer wg.Done()
			defer func() { <-sem }()
			results[index] = runNestedChild(ctx, opts, child, started)
		}(index, child)
	}
	wg.Wait()

	finished := time.Now().UTC()
	if !opts.Now.IsZero() {
		finished = opts.Now.UTC()
	}
	nestedReport := NestedScheduleReport{
		RepoPath:    filepath.ToSlash(opts.RepoPath),
		BaseBranch:  opts.BaseBranch,
		ParentRunID: opts.ParentRunID,
		StartedAt:   state.FormatTimestamp(started),
		FinishedAt:  state.FormatTimestamp(finished),
		Children:    results,
	}
	for _, result := range results {
		if result.Wave != nil {
			nestedReport.Repo = firstNonEmpty(nestedReport.Repo, result.Wave.Repo)
		}
	}
	nestedReport.Repo = firstNonEmpty(nestedReport.Repo, filepath.ToSlash(opts.RepoPath))
	nestedReport.Summary = summarizeNestedChildren(results)
	nestedReport.Status, nestedReport.StopReason = nestedScheduleStatus(nestedReport.Summary)
	return nestedReport, nil
}

func withNestedScheduleDefaults(opts NestedScheduleOptions) NestedScheduleOptions {
	if strings.TrimSpace(opts.BaseBranch) == "" {
		opts.BaseBranch = lcdefaults.BaseBranch
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	} else {
		opts.Now = opts.Now.UTC()
	}
	if strings.TrimSpace(opts.ParentRunID) == "" {
		opts.ParentRunID = state.RunIDForWave(opts.Now)
	}
	if opts.ConcurrencyLimit <= 0 {
		opts.ConcurrencyLimit = lcdefaults.DispatchWaveThrottleLimit
	}
	if opts.ThrottleLimit <= 0 {
		opts.ThrottleLimit = lcdefaults.DispatchWaveThrottleLimit
	}
	if opts.DispatchWave == nil {
		opts.DispatchWave = DispatchWave
	}
	if opts.LoadAttempts == nil {
		opts.LoadAttempts = state.LoadAttempts
	}
	if opts.AppendEvent == nil {
		opts.AppendEvent = state.AppendEvent
	}
	return opts
}

func normalizeChildRunSpecs(parentRunID string, children []ChildRunSpec) ([]ChildRunSpec, error) {
	out := make([]ChildRunSpec, 0, len(children))
	seen := map[string]bool{}
	for index, child := range children {
		child.ID = strings.TrimSpace(child.ID)
		if child.ID == "" {
			child.ID = fmt.Sprintf("child-%d", index+1)
		}
		key := strings.ToLower(child.ID)
		if seen[key] {
			return nil, fmt.Errorf("duplicate child run id %q", child.ID)
		}
		seen[key] = true
		if child.Required == child.Optional {
			return nil, fmt.Errorf("child %q must set exactly one of Required or Optional", child.ID)
		}
		child.IssueNumbers = normalizeIssueNumbers(child.IssueNumbers)
		if len(child.IssueNumbers) == 0 && child.ReadySet == nil {
			return nil, fmt.Errorf("child %q must include issue numbers or a ready set", child.ID)
		}
		child.RunID = strings.TrimSpace(child.RunID)
		if child.RunID == "" {
			child.RunID = state.RunIDForChild(parentRunID, child.ID)
		}
		out = append(out, child)
	}
	return out, nil
}

func runNestedChild(ctx context.Context, opts NestedScheduleOptions, child ChildRunSpec, now time.Time) ChildRunResult {
	result := ChildRunResult{
		ID:           child.ID,
		RunID:        child.RunID,
		Required:     child.Required,
		IssueNumbers: append([]int(nil), child.IssueNumbers...),
	}
	if err := recordNestedChildTransition(opts, child, now, nestedChildScheduledEvent, "scheduled", ""); err != nil {
		result.Status = NestedScheduleStatusNeedsHuman
		result.Error = fmt.Sprintf("record child scheduled transition: %v", err)
		return result
	}

	childThrottle := child.ThrottleLimit
	if childThrottle <= 0 {
		childThrottle = opts.ThrottleLimit
	}
	wave, err := opts.DispatchWave(ctx, DispatchWaveOptions{
		Reader:          opts.Reader,
		RepoPath:        opts.RepoPath,
		BaseBranch:      opts.BaseBranch,
		RunID:           child.RunID,
		IssueNumbers:    child.IssueNumbers,
		ReadySet:        child.ReadySet,
		Provider:        opts.Provider,
		Model:           opts.Model,
		Effort:          opts.Effort,
		ConfigFromBase:  opts.ConfigFromBase,
		ThrottleLimit:   childThrottle,
		Thresholds:      opts.Thresholds,
		Budget:          opts.Budget,
		CircuitBreaker:  opts.CircuitBreaker,
		ProcessAlive:    opts.ProcessAlive,
		Now:             now,
		Stderr:          opts.Stderr,
		ComputeReadySet: opts.ComputeReadySet,
		Dispatch:        opts.Dispatch,
		LoadAttempts:    opts.LoadAttempts,
	})
	result.Wave = &wave
	if len(result.IssueNumbers) == 0 {
		result.IssueNumbers = append([]int(nil), wave.IssuesRequested...)
	}
	result.Status = childStatusFromWave(wave, err)
	if err != nil {
		result.Error = err.Error()
	}
	if result.Error == "" && waveNeedsHumanError(wave) != "" {
		result.Error = waveNeedsHumanError(wave)
	}
	if recordErr := recordNestedChildTransition(opts, child, now, nestedChildCompletedEvent, result.Status, result.Error); recordErr != nil {
		result.Status = NestedScheduleStatusNeedsHuman
		if strings.TrimSpace(result.Error) != "" {
			result.Error += "; "
		}
		result.Error += fmt.Sprintf("record child completed transition: %v", recordErr)
	}
	return result
}

func recordNestedChildTransition(opts NestedScheduleOptions, child ChildRunSpec, now time.Time, eventName, status, errText string) error {
	if opts.AppendEvent == nil {
		return errors.New("append event is required")
	}
	return opts.AppendEvent(opts.RepoPath, opts.ParentRunID, state.Event{
		Timestamp: state.FormatTimestamp(now),
		RunID:     opts.ParentRunID,
		JobID:     "nested-" + child.ID,
		Phase:     "nested-scheduler",
		Status:    status,
		LogBytes:  0,
		Event:     eventName,
		Outcome:   status,
		Details: nestedChildTransitionDetails{
			ParentRunID:  opts.ParentRunID,
			ChildID:      child.ID,
			ChildRunID:   child.RunID,
			Required:     child.Required,
			IssueNumbers: append([]int(nil), child.IssueNumbers...),
			Status:       status,
			Error:        errText,
		},
	})
}

func childStatusFromWave(wave DispatchWaveReport, err error) string {
	if err != nil {
		return NestedScheduleStatusFailed
	}
	for _, result := range wave.Results {
		if result.Status == DispatchWaveStatusFailed {
			return NestedScheduleStatusFailed
		}
	}
	for _, result := range wave.Results {
		if result.Status == DispatchWaveStatusNeedsHuman {
			return NestedScheduleStatusNeedsHuman
		}
	}
	return NestedScheduleStatusSucceeded
}

func waveNeedsHumanError(wave DispatchWaveReport) string {
	for _, result := range wave.Results {
		if result.Status == DispatchWaveStatusNeedsHuman && strings.TrimSpace(result.Error) != "" {
			return result.Error
		}
	}
	return ""
}

func childResultFromError(child ChildRunSpec, status, errText string) ChildRunResult {
	return ChildRunResult{
		ID:           child.ID,
		RunID:        child.RunID,
		Required:     child.Required,
		IssueNumbers: append([]int(nil), child.IssueNumbers...),
		Status:       status,
		Error:        errText,
	}
}

func summarizeNestedChildren(results []ChildRunResult) NestedScheduleSummary {
	summary := NestedScheduleSummary{ChildCount: len(results)}
	for _, result := range results {
		if result.Required {
			summary.RequiredChildCount++
		} else {
			summary.OptionalChildCount++
		}
		switch result.Status {
		case NestedScheduleStatusSucceeded:
			summary.ChildSucceededCount++
		case NestedScheduleStatusFailed:
			summary.ChildFailedCount++
			if result.Required {
				summary.RequiredFailedCount++
			}
		case NestedScheduleStatusNeedsHuman:
			summary.ChildNeedsHumanCount++
			if result.Required {
				summary.RequiredNeedsHumanCount++
			}
		}
	}
	return summary
}

func nestedScheduleStatus(summary NestedScheduleSummary) (string, string) {
	if summary.RequiredFailedCount > 0 {
		return NestedScheduleStatusFailed, NestedScheduleStopChildFailed
	}
	if summary.RequiredNeedsHumanCount > 0 {
		return NestedScheduleStatusNeedsHuman, NestedScheduleStopChildNeedsHuman
	}
	return NestedScheduleStatusSucceeded, NestedScheduleStopCompleted
}
