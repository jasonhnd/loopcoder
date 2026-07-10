package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const LifecycleVersion = 1

type LifecycleState string

const (
	StatePlanned    LifecycleState = "planned"
	StateQueued     LifecycleState = "queued"
	StateRunning    LifecycleState = "running"
	StateWaiting    LifecycleState = "waiting"
	StateSucceeded  LifecycleState = "succeeded"
	StateFailed     LifecycleState = "failed"
	StateCancelled  LifecycleState = "cancelled"
	StateAbandoned  LifecycleState = "abandoned"
	StateNeedsHuman LifecycleState = "needs-human"
)

type LifecycleTransition struct {
	Version       int            `json:"version"`
	Timestamp     string         `json:"ts"`
	RunID         string         `json:"run_id"`
	ParentRunID   string         `json:"parent_run_id,omitempty"`
	PreviousState LifecycleState `json:"previous_state,omitempty"`
	State         LifecycleState `json:"state"`
	Reason        string         `json:"reason,omitempty"`
	Source        string         `json:"source,omitempty"`
	Issue         int            `json:"issue,omitempty"`
	JobID         string         `json:"job_id,omitempty"`
	ChildRunID    string         `json:"child_run_id,omitempty"`
	Details       any            `json:"details,omitempty"`
}

type Lifecycle struct {
	RunID       string
	ParentRunID string
	State       LifecycleState
	History     []LifecycleTransition
	ChildRunIDs []string
	Source      string
}

func LifecyclePath(repoPath, runID string) string {
	return filepath.Join(RunPath(repoPath, runID), "lifecycle.jsonl")
}

func ValidateLifecycleTransition(from, to LifecycleState) error {
	if !validLifecycleState(to) {
		return fmt.Errorf("invalid lifecycle state %q", to)
	}
	if from == "" {
		return nil
	}
	if !validLifecycleState(from) {
		return fmt.Errorf("invalid previous lifecycle state %q", from)
	}
	for _, allowed := range allowedLifecycleTransitions[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("invalid lifecycle transition %s -> %s", from, to)
}

func AppendLifecycleTransition(repoPath string, transition LifecycleTransition) error {
	runID := strings.TrimSpace(transition.RunID)
	if runID == "" {
		return fmt.Errorf("append lifecycle transition: run_id is required")
	}
	if transition.Version == 0 {
		transition.Version = LifecycleVersion
	}
	if transition.Version != LifecycleVersion {
		return fmt.Errorf("append lifecycle transition: unsupported version %d", transition.Version)
	}
	if strings.TrimSpace(transition.Timestamp) == "" {
		transition.Timestamp = FormatTimestamp(time.Now().UTC())
	}
	if _, err := ParseTimestamp(transition.Timestamp); err != nil {
		return fmt.Errorf("append lifecycle transition: %w", err)
	}

	history, err := LoadLifecycleHistory(repoPath, runID)
	if err != nil {
		return fmt.Errorf("append lifecycle transition: %w", err)
	}
	var previous LifecycleState
	if len(history) > 0 {
		previous = history[len(history)-1].State
	}
	if err := ValidateLifecycleTransition(previous, transition.State); err != nil {
		return fmt.Errorf("append lifecycle transition: %w", err)
	}
	transition.PreviousState = previous

	path := LifecyclePath(repoPath, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create lifecycle directory: %w", err)
	}
	data, err := json.Marshal(transition)
	if err != nil {
		return fmt.Errorf("marshal lifecycle transition: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open lifecycle file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append lifecycle transition: %w", err)
	}
	return nil
}

func LoadLifecycle(repoPath, runID string) (Lifecycle, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return Lifecycle{}, nil
	}
	history, err := LoadLifecycleHistory(repoPath, runID)
	if err != nil {
		return Lifecycle{}, err
	}
	if len(history) == 0 {
		legacy, err := ImportLegacyLifecycle(repoPath, runID)
		if err != nil {
			return Lifecycle{}, err
		}
		return lifecycleFromHistory(runID, legacy, "legacy"), nil
	}
	return lifecycleFromHistory(runID, history, "lifecycle"), nil
}

func LoadLifecycleHistory(repoPath, runID string) ([]LifecycleTransition, error) {
	path := LifecyclePath(repoPath, runID)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lifecycle file: %w", err)
	}
	defer file.Close()

	var history []LifecycleTransition
	var previous LifecycleState
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var transition LifecycleTransition
		if err := json.Unmarshal([]byte(line), &transition); err != nil {
			return nil, fmt.Errorf("read lifecycle file line %d: %w", lineNumber, err)
		}
		transition.RunID = strings.TrimSpace(transition.RunID)
		if transition.RunID == "" {
			transition.RunID = runID
		}
		if transition.RunID != runID {
			return nil, fmt.Errorf("read lifecycle file line %d: run_id %q does not match %q", lineNumber, transition.RunID, runID)
		}
		if transition.Version == 0 {
			transition.Version = LifecycleVersion
		}
		if transition.Version != LifecycleVersion {
			return nil, fmt.Errorf("read lifecycle file line %d: unsupported version %d", lineNumber, transition.Version)
		}
		if _, err := ParseTimestamp(transition.Timestamp); err != nil {
			return nil, fmt.Errorf("read lifecycle file line %d: %w", lineNumber, err)
		}
		if transition.PreviousState != "" && transition.PreviousState != previous {
			return nil, fmt.Errorf("read lifecycle file line %d: previous_state %q does not match current state %q", lineNumber, transition.PreviousState, previous)
		}
		if err := ValidateLifecycleTransition(previous, transition.State); err != nil {
			return nil, fmt.Errorf("read lifecycle file line %d: %w", lineNumber, err)
		}
		history = append(history, transition)
		previous = transition.State
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read lifecycle file: %w", err)
	}
	return history, nil
}

func ImportLegacyLifecycle(repoPath, runID string) ([]LifecycleTransition, error) {
	var history []LifecycleTransition
	events, err := loadLegacyEvents(repoPath, runID)
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		state, ok := LegacyLifecycleState(event.Status, event.Phase, event.ExitCode)
		if !ok {
			continue
		}
		history = appendLegacyTransition(history, LifecycleTransition{
			Version:   LifecycleVersion,
			Timestamp: event.Timestamp,
			RunID:     runID,
			State:     state,
			Reason:    "mapped from v0.6 events.jsonl",
			Source:    "legacy-event",
			Issue:     event.Issue,
			JobID:     event.JobID,
		})
	}

	attempts, err := LoadAttemptsFromWorkersDir(LegacyWorkersPath(repoPath, runID))
	if err != nil {
		return nil, err
	}
	for _, attempt := range attempts {
		state, ok := LegacyLifecycleState(attempt.Status, attempt.Phase, attempt.ExitCode)
		if !ok {
			continue
		}
		ts := firstNonEmptyLifecycle(attempt.LastProgressAt, attempt.HeartbeatAt, attempt.StartedAt)
		if ts == "" && !attempt.LastWriteUTC.IsZero() {
			ts = FormatTimestamp(attempt.LastWriteUTC)
		}
		if ts == "" {
			ts = FormatTimestamp(time.Unix(0, 0).UTC())
		}
		history = appendLegacyTransition(history, LifecycleTransition{
			Version:   LifecycleVersion,
			Timestamp: ts,
			RunID:     runID,
			State:     state,
			Reason:    "mapped from v0.6 attempt sidecar",
			Source:    "legacy-attempt",
			Issue:     attempt.Issue,
			JobID:     attempt.JobID,
		})
	}
	sort.SliceStable(history, func(i, j int) bool {
		left, leftErr := ParseTimestamp(history[i].Timestamp)
		right, rightErr := ParseTimestamp(history[j].Timestamp)
		if leftErr == nil && rightErr == nil && !left.Equal(right) {
			return left.Before(right)
		}
		return history[i].Source < history[j].Source
	})
	return compressLegacyHistory(history), nil
}

func LegacyLifecycleState(status, phase string, exitCode *int) (LifecycleState, bool) {
	status = strings.ToLower(strings.TrimSpace(status))
	phase = strings.ToLower(strings.TrimSpace(phase))
	switch status {
	case string(StatePlanned), string(StateQueued), string(StateRunning), string(StateWaiting),
		string(StateSucceeded), string(StateFailed), string(StateCancelled), string(StateAbandoned),
		string(StateNeedsHuman):
		return LifecycleState(status), true
	case "success", "passed", "pass", "done", "completed", "promoted":
		return StateSucceeded, true
	case "error", "errored", "hung", "idle":
		return StateFailed, true
	case "blocked", "guardrail-frozen", "needs:human":
		return StateNeedsHuman, true
	case "stale", "in-review", "pending":
		return StateWaiting, true
	}
	if exitCode != nil {
		if *exitCode == 0 {
			return StateSucceeded, true
		}
		return StateFailed, true
	}
	if strings.Contains(phase, "start") || strings.Contains(phase, "running") || strings.Contains(phase, "codex") ||
		strings.Contains(phase, "claude") || strings.Contains(phase, "gemini") || strings.Contains(phase, "worker") {
		return StateRunning, true
	}
	return "", false
}

var allowedLifecycleTransitions = map[LifecycleState][]LifecycleState{
	StatePlanned:    {StateQueued, StateRunning, StateWaiting, StateCancelled, StateAbandoned, StateNeedsHuman},
	StateQueued:     {StateRunning, StateWaiting, StateCancelled, StateAbandoned, StateNeedsHuman},
	StateRunning:    {StateWaiting, StateSucceeded, StateFailed, StateCancelled, StateAbandoned, StateNeedsHuman},
	StateWaiting:    {StateQueued, StateRunning, StateSucceeded, StateFailed, StateCancelled, StateAbandoned, StateNeedsHuman},
	StateSucceeded:  nil,
	StateFailed:     nil,
	StateCancelled:  nil,
	StateAbandoned:  nil,
	StateNeedsHuman: nil,
}

func validLifecycleState(state LifecycleState) bool {
	_, ok := allowedLifecycleTransitions[state]
	return ok
}

func lifecycleFromHistory(runID string, history []LifecycleTransition, source string) Lifecycle {
	current := Lifecycle{RunID: runID, History: append([]LifecycleTransition(nil), history...), Source: source}
	children := map[string]bool{}
	for _, transition := range history {
		current.State = transition.State
		if strings.TrimSpace(transition.ParentRunID) != "" {
			current.ParentRunID = strings.TrimSpace(transition.ParentRunID)
		}
		if strings.TrimSpace(transition.ChildRunID) != "" {
			children[strings.TrimSpace(transition.ChildRunID)] = true
		}
	}
	for child := range children {
		current.ChildRunIDs = append(current.ChildRunIDs, child)
	}
	sort.Strings(current.ChildRunIDs)
	return current
}

func appendLegacyTransition(history []LifecycleTransition, transition LifecycleTransition) []LifecycleTransition {
	if transition.Timestamp == "" {
		transition.Timestamp = FormatTimestamp(time.Unix(0, 0).UTC())
	}
	if _, err := ParseTimestamp(transition.Timestamp); err != nil {
		transition.Timestamp = FormatTimestamp(time.Unix(0, 0).UTC())
	}
	return append(history, transition)
}

func compressLegacyHistory(history []LifecycleTransition) []LifecycleTransition {
	var out []LifecycleTransition
	var previous LifecycleState
	for _, transition := range history {
		if transition.State == previous {
			continue
		}
		if err := ValidateLifecycleTransition(previous, transition.State); err != nil {
			transition.State = conservativeLegacyState(previous, transition.State)
			if transition.State == "" || transition.State == previous {
				continue
			}
		}
		transition.PreviousState = previous
		out = append(out, transition)
		previous = transition.State
	}
	return out
}

func conservativeLegacyState(previous, candidate LifecycleState) LifecycleState {
	if previous == "" {
		return candidate
	}
	if len(allowedLifecycleTransitions[previous]) == 0 {
		return ""
	}
	switch candidate {
	case StateSucceeded, StateFailed, StateCancelled, StateAbandoned, StateNeedsHuman:
		return candidate
	default:
		return StateWaiting
	}
}

func loadLegacyEvents(repoPath, runID string) ([]Event, error) {
	path := LegacyEventsPath(repoPath, runID)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read legacy events: %w", err)
	}
	defer file.Close()

	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, fmt.Errorf("read legacy events line %d: %w", lineNumber, err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read legacy events: %w", err)
	}
	return events, nil
}

func firstNonEmptyLifecycle(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
