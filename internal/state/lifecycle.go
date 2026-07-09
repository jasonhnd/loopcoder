package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const LifecycleTransitionEvent = "run.lifecycle.transition"

type LifecycleState string

const (
	LifecyclePlanned    LifecycleState = "planned"
	LifecycleQueued     LifecycleState = "queued"
	LifecycleRunning    LifecycleState = "running"
	LifecycleWaiting    LifecycleState = "waiting"
	LifecycleSucceeded  LifecycleState = "succeeded"
	LifecycleFailed     LifecycleState = "failed"
	LifecycleCancelled  LifecycleState = "cancelled"
	LifecycleAbandoned  LifecycleState = "abandoned"
	LifecycleNeedsHuman LifecycleState = "needs-human"
)

type LifecycleTransition struct {
	Timestamp   string          `json:"ts"`
	RunID       string          `json:"run_id"`
	ParentRunID string          `json:"parent_run_id,omitempty"`
	ChildRunID  string          `json:"child_run_id,omitempty"`
	From        LifecycleState  `json:"from"`
	To          LifecycleState  `json:"to"`
	Event       string          `json:"event"`
	Reason      string          `json:"reason,omitempty"`
	Actor       string          `json:"actor,omitempty"`
	Source      string          `json:"source,omitempty"`
	Details     json.RawMessage `json:"details,omitempty"`
}

type Lifecycle struct {
	RunID       string                `json:"run_id"`
	ParentRunID string                `json:"parent_run_id,omitempty"`
	ChildRunIDs []string              `json:"child_run_ids,omitempty"`
	State       LifecycleState        `json:"state"`
	History     []LifecycleTransition `json:"history,omitempty"`
}

// ValidLifecycleState reports whether state is one of the durable run states.
func ValidLifecycleState(state LifecycleState) bool {
	switch state {
	case LifecyclePlanned, LifecycleQueued, LifecycleRunning, LifecycleWaiting,
		LifecycleSucceeded, LifecycleFailed, LifecycleCancelled, LifecycleAbandoned,
		LifecycleNeedsHuman:
		return true
	default:
		return false
	}
}

// ValidLifecycleTransition reports whether a transition is allowed.
func ValidLifecycleTransition(from, to LifecycleState) bool {
	if !ValidLifecycleState(from) || !ValidLifecycleState(to) || from == to {
		return false
	}
	switch from {
	case LifecyclePlanned:
		return to == LifecycleQueued || to == LifecycleRunning || to == LifecycleWaiting || to == LifecycleCancelled ||
			to == LifecycleAbandoned || to == LifecycleNeedsHuman || to == LifecycleSucceeded
	case LifecycleQueued:
		return to == LifecycleRunning || to == LifecycleWaiting || to == LifecycleCancelled ||
			to == LifecycleAbandoned || to == LifecycleNeedsHuman || to == LifecycleSucceeded || to == LifecycleFailed
	case LifecycleRunning:
		return to == LifecycleWaiting || to == LifecycleSucceeded || to == LifecycleFailed ||
			to == LifecycleCancelled || to == LifecycleAbandoned || to == LifecycleNeedsHuman || to == LifecycleQueued
	case LifecycleWaiting:
		return to == LifecycleQueued || to == LifecycleRunning || to == LifecycleSucceeded ||
			to == LifecycleFailed || to == LifecycleCancelled || to == LifecycleAbandoned || to == LifecycleNeedsHuman
	case LifecycleFailed, LifecycleNeedsHuman:
		return to == LifecycleQueued || to == LifecycleRunning || to == LifecycleWaiting ||
			to == LifecycleCancelled || to == LifecycleAbandoned
	default:
		return false
	}
}

// AppendLifecycleTransition validates and persists one lifecycle transition to events.jsonl.
func AppendLifecycleTransition(repoPath, runID string, transition LifecycleTransition) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("run id is required")
	}
	current, err := LoadLifecycle(repoPath, runID)
	if err != nil {
		return err
	}

	transition.RunID = runID
	transition.Event = LifecycleTransitionEvent
	transition.Source = firstLifecycleText(transition.Source, "explicit")
	if strings.TrimSpace(transition.Timestamp) == "" {
		transition.Timestamp = FormatTimestamp(time.Now())
	}
	if transition.From == "" {
		transition.From = current.State
	}
	if transition.From != current.State {
		return fmt.Errorf("invalid lifecycle transition for %s: from %q does not match current state %q", runID, transition.From, current.State)
	}
	if !ValidLifecycleTransition(transition.From, transition.To) {
		return fmt.Errorf("invalid lifecycle transition for %s: %s -> %s", runID, transition.From, transition.To)
	}

	path := EventsPath(repoPath, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create run directory: %w", err)
	}
	data, err := json.Marshal(transition)
	if err != nil {
		return fmt.Errorf("marshal lifecycle transition: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open events file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append lifecycle transition: %w", err)
	}
	return nil
}

// LoadLifecycle derives the current lifecycle and inspectable history from events.jsonl.
func LoadLifecycle(repoPath, runID string) (Lifecycle, error) {
	runID = strings.TrimSpace(runID)
	lifecycle := Lifecycle{RunID: runID, State: LifecyclePlanned}
	if runID == "" {
		return lifecycle, nil
	}

	file, err := os.Open(EventsPath(repoPath, runID))
	if err != nil {
		if os.IsNotExist(err) {
			return lifecycle, nil
		}
		return Lifecycle{}, fmt.Errorf("read lifecycle events: %w", err)
	}
	defer file.Close()

	children := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		transition, ok, err := lifecycleTransitionFromLine(line, runID, lifecycle.State)
		if err != nil {
			return Lifecycle{}, err
		}
		if !ok {
			continue
		}
		lifecycle.History = append(lifecycle.History, transition)
		lifecycle.State = transition.To
		if transition.ParentRunID != "" && lifecycle.ParentRunID == "" {
			lifecycle.ParentRunID = transition.ParentRunID
		}
		if transition.ChildRunID != "" {
			children[transition.ChildRunID] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return Lifecycle{}, fmt.Errorf("read lifecycle events: %w", err)
	}
	if len(children) > 0 {
		lifecycle.ChildRunIDs = make([]string, 0, len(children))
		for child := range children {
			lifecycle.ChildRunIDs = append(lifecycle.ChildRunIDs, child)
		}
		sort.Strings(lifecycle.ChildRunIDs)
	}
	return lifecycle, nil
}

func lifecycleTransitionFromLine(line []byte, runID string, current LifecycleState) (LifecycleTransition, bool, error) {
	var probe struct {
		Timestamp   string          `json:"ts"`
		RunID       string          `json:"run_id"`
		ParentRunID string          `json:"parent_run_id"`
		ChildRunID  string          `json:"child_run_id"`
		From        LifecycleState  `json:"from"`
		To          LifecycleState  `json:"to"`
		Event       string          `json:"event"`
		Status      string          `json:"status"`
		Outcome     string          `json:"outcome"`
		Phase       string          `json:"phase"`
		Reason      string          `json:"reason"`
		Actor       string          `json:"actor"`
		Source      string          `json:"source"`
		Details     json.RawMessage `json:"details"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return LifecycleTransition{}, false, nil
	}

	if strings.TrimSpace(probe.Event) == LifecycleTransitionEvent {
		transition := LifecycleTransition{
			Timestamp:   probe.Timestamp,
			RunID:       firstLifecycleText(probe.RunID, runID),
			ParentRunID: strings.TrimSpace(probe.ParentRunID),
			ChildRunID:  strings.TrimSpace(probe.ChildRunID),
			From:        probe.From,
			To:          probe.To,
			Event:       LifecycleTransitionEvent,
			Reason:      strings.TrimSpace(probe.Reason),
			Actor:       strings.TrimSpace(probe.Actor),
			Source:      firstLifecycleText(probe.Source, "explicit"),
			Details:     cloneRawMessage(probe.Details),
		}
		if transition.From == "" {
			transition.From = current
		}
		if !ValidLifecycleTransition(transition.From, transition.To) {
			return LifecycleTransition{}, false, fmt.Errorf("invalid lifecycle event in %s: %s -> %s", runID, transition.From, transition.To)
		}
		if transition.From != current {
			return LifecycleTransition{}, false, fmt.Errorf("invalid lifecycle event in %s: from %q does not match current state %q", runID, transition.From, current)
		}
		return transition, true, nil
	}

	next, ok := legacyLifecycleState(probe.Status, probe.Outcome, probe.Phase)
	if !ok || next == current {
		return LifecycleTransition{}, false, nil
	}
	if !ValidLifecycleTransition(current, next) {
		return LifecycleTransition{}, false, nil
	}
	return LifecycleTransition{
		Timestamp:   probe.Timestamp,
		RunID:       firstLifecycleText(probe.RunID, runID),
		ParentRunID: strings.TrimSpace(probe.ParentRunID),
		ChildRunID:  strings.TrimSpace(probe.ChildRunID),
		From:        current,
		To:          next,
		Event:       firstLifecycleText(probe.Event, "legacy.status"),
		Source:      "legacy",
	}, true, nil
}

func legacyLifecycleState(values ...string) (LifecycleState, bool) {
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(value))
		switch normalized {
		case "":
			continue
		case "planned":
			return LifecyclePlanned, true
		case "queued", "pending", "pending-promotion":
			return LifecycleQueued, true
		case "running", "implementing", "in-progress", "in_progress":
			return LifecycleRunning, true
		case "waiting", "idle", "blocked":
			return LifecycleWaiting, true
		case "succeeded", "success", "completed", "complete", "done", "passed", "pass", "promoted":
			return LifecycleSucceeded, true
		case "failed", "failure", "error", "errored", "hung", "timeout", "timed_out":
			return LifecycleFailed, true
		case "cancelled", "canceled":
			return LifecycleCancelled, true
		case "abandoned":
			return LifecycleAbandoned, true
		case "needs-human", "needs_human", "needs human", "action_required":
			return LifecycleNeedsHuman, true
		}
	}
	return "", false
}

func firstLifecycleText(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func cloneRawMessage(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	out := make([]byte, len(value))
	copy(out, value)
	return out
}
