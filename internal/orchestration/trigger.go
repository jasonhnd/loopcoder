package orchestration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/state"
)

const (
	TriggerReportVersion = 1

	TriggerKindCron     = "cron"
	TriggerKindGoalLoop = "goal-loop"
	TriggerKindHook     = "hook"

	TriggerStatusSucceeded  = TickStatusSucceeded
	TriggerStatusFailed     = TickStatusFailed
	TriggerStatusNeedsHuman = TickStatusNeedsHuman

	TriggerStopTickCompleted = "tick-completed"
	TriggerStopGoalReached   = "goal-reached"
	TriggerStopNeedsHuman    = "tick-needs-human"
	TriggerStopTickFailed    = "tick-failed"
	TriggerStopTickError     = "tick-error"
	TriggerStopMaxIterations = "max-iterations"
)

type TriggerTickFunc func(ctx context.Context, opts TickOptions) (TickReport, error)
type TriggerGoalFunc func(report TickReport) bool

type TriggerOptions struct {
	Kind          string
	RepoPath      string
	Schedule      string
	Event         string
	Goal          string
	MaxIterations int
	TickOptions   TickOptions
	Tick          TriggerTickFunc
	GoalReached   TriggerGoalFunc
	Clock         func() time.Time
}

type TriggerReport struct {
	Version       int          `json:"version"`
	Kind          string       `json:"kind"`
	RepoPath      string       `json:"repo_path"`
	Schedule      string       `json:"schedule,omitempty"`
	Event         string       `json:"event,omitempty"`
	Goal          string       `json:"goal,omitempty"`
	MaxIterations int          `json:"max_iterations,omitempty"`
	Iterations    int          `json:"iterations"`
	Status        string       `json:"status"`
	StopReason    string       `json:"stop_reason"`
	StartedAt     string       `json:"started_at"`
	FinishedAt    string       `json:"finished_at"`
	Error         string       `json:"error,omitempty"`
	Ticks         []TickReport `json:"ticks"`
}

func RunTrigger(ctx context.Context, opts TriggerOptions) (TriggerReport, error) {
	opts = withTriggerDefaults(opts)
	if err := validateTriggerOptions(opts); err != nil {
		return TriggerReport{}, err
	}

	started := opts.Clock().UTC()
	report := TriggerReport{
		Version:       TriggerReportVersion,
		Kind:          strings.TrimSpace(opts.Kind),
		RepoPath:      filepath.ToSlash(strings.TrimSpace(opts.RepoPath)),
		Schedule:      strings.TrimSpace(opts.Schedule),
		Event:         strings.TrimSpace(opts.Event),
		Goal:          triggerGoal(opts.Goal),
		MaxIterations: opts.MaxIterations,
		StartedAt:     state.FormatTimestamp(started),
		Ticks:         []TickReport{},
	}
	finish := func(status, stopReason string, err error) (TriggerReport, error) {
		report.Status = status
		report.StopReason = stopReason
		report.FinishedAt = state.FormatTimestamp(opts.Clock().UTC())
		report.Iterations = len(report.Ticks)
		if err != nil {
			report.Error = err.Error()
		}
		return normalizeTriggerReport(report), nil
	}

	switch opts.Kind {
	case TriggerKindCron, TriggerKindHook:
		tickReport, err := opts.Tick(ctx, triggerTickOptions(opts, started, 1, false))
		if err != nil {
			return finish(TriggerStatusFailed, TriggerStopTickError, err)
		}
		report.Ticks = append(report.Ticks, tickReport)
		return finish(triggerStatusFromTick(tickReport), triggerStopFromTick(tickReport), nil)
	case TriggerKindGoalLoop:
		for iteration := 1; iteration <= opts.MaxIterations; iteration++ {
			tickReport, err := opts.Tick(ctx, triggerTickOptions(opts, started, iteration, true))
			if err != nil {
				return finish(TriggerStatusFailed, TriggerStopTickError, err)
			}
			report.Ticks = append(report.Ticks, tickReport)
			if tickReport.Status == TickStatusNeedsHuman {
				return finish(TriggerStatusNeedsHuman, TriggerStopNeedsHuman, nil)
			}
			if tickReport.Status == TickStatusFailed {
				return finish(TriggerStatusFailed, TriggerStopTickFailed, nil)
			}
			if opts.GoalReached(tickReport) {
				return finish(TriggerStatusSucceeded, TriggerStopGoalReached, nil)
			}
		}
		return finish(TriggerStatusNeedsHuman, TriggerStopMaxIterations, nil)
	default:
		return TriggerReport{}, fmt.Errorf("unsupported trigger kind %q", opts.Kind)
	}
}

func withTriggerDefaults(opts TriggerOptions) TriggerOptions {
	opts.Kind = strings.TrimSpace(opts.Kind)
	if strings.TrimSpace(opts.Goal) == "" {
		opts.Goal = "roadmap-exhausted"
	}
	if opts.Tick == nil {
		opts.Tick = Tick
	}
	if opts.GoalReached == nil {
		opts.GoalReached = DefaultTriggerGoalReached
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time {
			return time.Now().UTC()
		}
	}
	return opts
}

func validateTriggerOptions(opts TriggerOptions) error {
	if strings.TrimSpace(opts.RepoPath) == "" {
		return errors.New("repo path is required")
	}
	if strings.TrimSpace(opts.TickOptions.RepoPath) != "" && !sameTriggerRepo(opts.TickOptions.RepoPath, opts.RepoPath) {
		return fmt.Errorf("tick repo path %q does not match trigger repo path %q", opts.TickOptions.RepoPath, opts.RepoPath)
	}
	switch opts.Kind {
	case TriggerKindCron:
		if strings.TrimSpace(opts.Schedule) == "" {
			return errors.New("cron schedule is required")
		}
	case TriggerKindGoalLoop:
		if opts.MaxIterations <= 0 {
			return errors.New("max_iterations must be greater than zero")
		}
		if goal := triggerGoal(opts.Goal); goal != "roadmap-exhausted" && goal != "no-ready-work" {
			return fmt.Errorf("unsupported goal %q", opts.Goal)
		}
	case TriggerKindHook:
		if strings.TrimSpace(opts.Event) == "" {
			return errors.New("hook event is required")
		}
	default:
		return fmt.Errorf("unsupported trigger kind %q", opts.Kind)
	}
	return nil
}

func triggerTickOptions(opts TriggerOptions, started time.Time, iteration int, forceIndependent bool) TickOptions {
	tickOpts := opts.TickOptions
	tickOpts.RepoPath = strings.TrimSpace(opts.RepoPath)
	if tickOpts.Clock == nil {
		iterationStarted := started.Add(time.Duration(iteration-1) * time.Second)
		tickOpts.Clock = func() time.Time {
			return iterationStarted
		}
	}
	if forceIndependent {
		tickOpts.RunID = state.RunIDForWave(started.Add(time.Duration(iteration-1) * time.Second))
	} else if strings.TrimSpace(tickOpts.RunID) == "" {
		tickOpts.RunID = state.RunIDForWave(started.Add(time.Duration(iteration-1) * time.Second))
	}
	return tickOpts
}

func DefaultTriggerGoalReached(report TickReport) bool {
	return report.Status == TickStatusNoReadyWork &&
		len(report.NeedsHuman) == 0 &&
		len(report.Failures) == 0
}

func triggerGoal(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "roadmap-exhausted"
	}
	return value
}

func triggerStatusFromTick(report TickReport) string {
	switch report.Status {
	case TickStatusNeedsHuman:
		return TriggerStatusNeedsHuman
	case TickStatusFailed:
		return TriggerStatusFailed
	default:
		return TriggerStatusSucceeded
	}
}

func triggerStopFromTick(report TickReport) string {
	switch report.Status {
	case TickStatusNeedsHuman:
		return TriggerStopNeedsHuman
	case TickStatusFailed:
		return TriggerStopTickFailed
	default:
		return TriggerStopTickCompleted
	}
}

func sameTriggerRepo(left, right string) bool {
	left = filepath.Clean(strings.TrimSpace(left))
	right = filepath.Clean(strings.TrimSpace(right))
	return strings.EqualFold(left, right)
}

func MarshalTriggerJSON(report TriggerReport) ([]byte, error) {
	report = normalizeTriggerReport(report)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal trigger JSON: %w", err)
	}
	return append(data, '\n'), nil
}

func RenderTriggerText(report TriggerReport) string {
	report = normalizeTriggerReport(report)
	var out bytes.Buffer
	fmt.Fprintln(&out, "TRIGGER")
	fmt.Fprintf(&out, "Kind: %s\n", report.Kind)
	fmt.Fprintf(&out, "Repo path: %s\n", report.RepoPath)
	if report.Schedule != "" {
		fmt.Fprintf(&out, "Schedule: %s\n", report.Schedule)
	}
	if report.Event != "" {
		fmt.Fprintf(&out, "Event: %s\n", report.Event)
	}
	if report.Goal != "" {
		fmt.Fprintf(&out, "Goal: %s\n", report.Goal)
	}
	if report.MaxIterations > 0 {
		fmt.Fprintf(&out, "Max iterations: %d\n", report.MaxIterations)
	}
	fmt.Fprintf(&out, "Iterations: %d\n", report.Iterations)
	fmt.Fprintf(&out, "Status: %s\n", report.Status)
	fmt.Fprintf(&out, "Stop reason: %s\n", report.StopReason)
	if report.Error != "" {
		fmt.Fprintf(&out, "Error: %s\n", report.Error)
	}

	fmt.Fprintln(&out)
	fmt.Fprintln(&out, "Ticks")
	if len(report.Ticks) == 0 {
		fmt.Fprintln(&out, "- none")
		return out.String()
	}
	for index, tick := range report.Ticks {
		fmt.Fprintf(&out, "- %d run_id=%s status=%s stop_reason=%s\n", index+1, tick.RunID, tick.Status, tick.StopReason)
	}
	return out.String()
}

func TriggerExitCode(report TriggerReport) int {
	switch report.Status {
	case TriggerStatusSucceeded:
		return 0
	case TriggerStatusNeedsHuman:
		return 2
	default:
		return 1
	}
}

func normalizeTriggerReport(report TriggerReport) TriggerReport {
	report.Kind = strings.TrimSpace(report.Kind)
	report.RepoPath = filepath.ToSlash(strings.TrimSpace(report.RepoPath))
	report.Schedule = strings.TrimSpace(report.Schedule)
	report.Event = strings.TrimSpace(report.Event)
	report.Goal = strings.TrimSpace(report.Goal)
	report.Error = strings.TrimSpace(report.Error)
	if report.Ticks == nil {
		report.Ticks = []TickReport{}
	}
	for i := range report.Ticks {
		report.Ticks[i] = normalizeTickReport(report.Ticks[i])
	}
	report.Iterations = len(report.Ticks)
	return report
}
