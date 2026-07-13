package inspect

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
)

func TestInspectRecognizesEveryDeliveryLifecycleState(t *testing.T) {
	for _, state := range allRunLifecycleStates() {
		run := RunSummary{DeliveryRunID: "run_" + strings.ReplaceAll(state, "-", "_"), State: state}
		diagnostics := validateRun(run, delivery.SchemaDeliveryRun, 1)
		if diagnosticCodePresent(diagnostics, "unknown-run-state") {
			t.Fatalf("run state %q produced unknown diagnostic: %#v", state, diagnostics)
		}
		if !knownRunState(state) {
			t.Fatalf("knownRunState(%q) = false", state)
		}
	}
	for _, state := range allTaskLifecycleStates() {
		task := TaskSummary{TaskID: "task_" + strings.ReplaceAll(state, "-", "_"), State: state}
		diagnostics := validateTask(task, delivery.SchemaTask, 1)
		if diagnosticCodePresent(diagnostics, "unknown-task-state") {
			t.Fatalf("task state %q produced unknown diagnostic: %#v", state, diagnostics)
		}
		if !knownTaskState(state) {
			t.Fatalf("knownTaskState(%q) = false", state)
		}
	}
	for _, state := range allAttemptLifecycleStates() {
		attempt := AttemptSummary{AttemptID: "att_" + strings.ReplaceAll(state, "-", "_"), State: state}
		diagnostics := validateAttempt(attempt, delivery.SchemaAttempt, 1)
		if diagnosticCodePresent(diagnostics, "unknown-attempt-state") {
			t.Fatalf("attempt state %q produced unknown diagnostic: %#v", state, diagnostics)
		}
		if !knownAttemptState(state) {
			t.Fatalf("knownAttemptState(%q) = false", state)
		}
	}
}

func TestInspectTreatsLegacyHungAttemptAsUnknown(t *testing.T) {
	attempt := AttemptSummary{AttemptID: "att_hung", State: "hung"}
	diagnostics := validateAttempt(attempt, delivery.SchemaAttempt, 1)
	if !diagnosticCodePresent(diagnostics, "unknown-attempt-state") {
		t.Fatalf("hung attempt diagnostics = %#v, want unknown-attempt-state", diagnostics)
	}
	if knownAttemptState("hung") {
		t.Fatal("knownAttemptState(\"hung\") = true, want false for non-0801 Attempt state")
	}
}

func TestClassifyRunLifecycleStates(t *testing.T) {
	opts := classifierTestOptions()
	cases := map[string]struct {
		blocker string
		next    string
	}{
		delivery.RunDraft:            {"", "start planning or abandon delivery run"},
		delivery.RunPlanning:         {"", "wait for plan proposal or inspect planner progress"},
		delivery.RunAwaitingApproval: {"approval-required", "review delivery plan and decide approve, reject, or edit"},
		delivery.RunApproved:         {"", "queue delivery run or start execution"},
		delivery.RunQueued:           {"", "start eligible tasks"},
		delivery.RunRunning:          {"", "wait for active tasks or inspect attempts"},
		delivery.RunPaused:           {"paused", "resume or cancel delivery run"},
		delivery.RunCancelling:       {"cancelling", "wait for cancellation to finish or inspect active task cleanup"},
		delivery.RunSucceeded:        {"", "none"},
		delivery.RunFailed:           {"failed", "inspect failure and decide retry, edit, or abandon"},
		delivery.RunCancelled:        {"", "none"},
		delivery.RunNeedsHuman:       {"needs-human", "inspect blocker and choose the next delivery action"},
		delivery.RunAbandoned:        {"", "none"},
	}
	if len(cases) != len(allRunLifecycleStates()) {
		t.Fatalf("run classifier cases = %d, want %d", len(cases), len(allRunLifecycleStates()))
	}
	for _, state := range allRunLifecycleStates() {
		want := cases[state]
		gotBlocker, gotNext := classifyRun(RunSummary{
			DeliveryRunID:       "run_" + strings.ReplaceAll(state, "-", "_"),
			State:               state,
			UpdatedAt:           "2026-07-13T00:00:00Z",
			LastDurableProgress: Progress{At: "2026-07-13T00:00:00Z", Source: "delivery_run"},
		}, opts)
		if gotBlocker != want.blocker || gotNext != want.next {
			t.Fatalf("classifyRun(%q) = (%q, %q), want (%q, %q)", state, gotBlocker, gotNext, want.blocker, want.next)
		}
	}
}

func TestClassifyTaskLifecycleStates(t *testing.T) {
	opts := classifierTestOptions()
	cases := map[string]struct {
		blocker string
		next    string
	}{
		delivery.TaskPending:          {"dependencies-pending", "wait for upstream tasks"},
		delivery.TaskBlocked:          {"dependencies-blocked", "inspect upstream task blockers"},
		delivery.TaskAwaitingApproval: {"approval-required", "review and approve the current plan fingerprint"},
		delivery.TaskReady:            {"", "claim task"},
		delivery.TaskClaimed:          {"", "start claimed task"},
		delivery.TaskRunning:          {"", "wait for attempt progress"},
		delivery.TaskPaused:           {"paused", "resume or cancel task"},
		delivery.TaskCancelling:       {"cancelling", "wait for cancellation to finish or inspect active attempt cleanup"},
		delivery.TaskSucceeded:        {"", "none"},
		delivery.TaskFailed:           {"failed", "inspect failed attempt and decide retry or edit"},
		delivery.TaskSkipped:          {"", "none"},
		delivery.TaskCancelled:        {"", "none"},
		delivery.TaskNeedsHuman:       {"needs-human", "human decision required before continuing"},
	}
	if len(cases) != len(allTaskLifecycleStates()) {
		t.Fatalf("task classifier cases = %d, want %d", len(cases), len(allTaskLifecycleStates()))
	}
	for _, state := range allTaskLifecycleStates() {
		want := cases[state]
		gotBlocker, gotNext := classifyTask(TaskSummary{
			TaskID:              "task_" + strings.ReplaceAll(state, "-", "_"),
			State:               state,
			UpdatedAt:           "2026-07-13T00:00:00Z",
			LastDurableProgress: Progress{At: "2026-07-13T00:00:00Z", Source: "task"},
		}, opts)
		if gotBlocker != want.blocker || gotNext != want.next {
			t.Fatalf("classifyTask(%q) = (%q, %q), want (%q, %q)", state, gotBlocker, gotNext, want.blocker, want.next)
		}
	}
}

func TestClassifiersPreserveTerminalErrorBlockersAndNextActions(t *testing.T) {
	opts := classifierTestOptions()
	runBlocker, runNext := classifyRun(RunSummary{
		DeliveryRunID:     "run_failed",
		State:             delivery.RunFailed,
		UpdatedAt:         "2026-07-13T00:00:00Z",
		TerminalErrorCode: string(delivery.ErrInvalidRecordCode),
	}, opts)
	if runBlocker != string(delivery.ErrInvalidRecordCode) || runNext != "inspect failure and decide retry, edit, or abandon" {
		t.Fatalf("failed run = (%q, %q), want terminal error blocker and stable next action", runBlocker, runNext)
	}
	taskBlocker, taskNext := classifyTask(TaskSummary{
		TaskID:            "task_failed",
		State:             delivery.TaskFailed,
		UpdatedAt:         "2026-07-13T00:00:00Z",
		TerminalErrorCode: string(delivery.ErrStaleClaimCode),
	}, opts)
	if taskBlocker != string(delivery.ErrStaleClaimCode) || taskNext != "inspect failed attempt and decide retry or edit" {
		t.Fatalf("failed task = (%q, %q), want terminal error blocker and stable next action", taskBlocker, taskNext)
	}
}

func allRunLifecycleStates() []string {
	return []string{
		delivery.RunDraft,
		delivery.RunPlanning,
		delivery.RunAwaitingApproval,
		delivery.RunApproved,
		delivery.RunQueued,
		delivery.RunRunning,
		delivery.RunPaused,
		delivery.RunCancelling,
		delivery.RunSucceeded,
		delivery.RunFailed,
		delivery.RunCancelled,
		delivery.RunNeedsHuman,
		delivery.RunAbandoned,
	}
}

func allTaskLifecycleStates() []string {
	return []string{
		delivery.TaskPending,
		delivery.TaskBlocked,
		delivery.TaskAwaitingApproval,
		delivery.TaskReady,
		delivery.TaskClaimed,
		delivery.TaskRunning,
		delivery.TaskPaused,
		delivery.TaskCancelling,
		delivery.TaskSucceeded,
		delivery.TaskFailed,
		delivery.TaskSkipped,
		delivery.TaskCancelled,
		delivery.TaskNeedsHuman,
	}
}

func allAttemptLifecycleStates() []string {
	return []string{
		delivery.AttemptPlanned,
		delivery.AttemptClaimed,
		delivery.AttemptLaunching,
		delivery.AttemptRunning,
		delivery.AttemptSucceeded,
		delivery.AttemptFailed,
		delivery.AttemptCancelled,
		delivery.AttemptNeedsHuman,
		delivery.AttemptStale,
		delivery.AttemptSuperseded,
	}
}

func classifierTestOptions() Options {
	return Options{
		StaleAfter: time.Hour,
		Now: func() time.Time {
			return time.Date(2026, 7, 13, 0, 1, 0, 0, time.UTC)
		},
	}
}

func diagnosticCodePresent(diagnostics []Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
