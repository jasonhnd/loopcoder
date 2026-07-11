package delivery

import "strings"

const (
	RunDraft            = "draft"
	RunPlanning         = "planning"
	RunAwaitingApproval = "awaiting-approval"
	RunApproved         = "approved"
	RunQueued           = "queued"
	RunRunning          = "running"
	RunPaused           = "paused"
	RunCancelling       = "cancelling"
	RunSucceeded        = "succeeded"
	RunFailed           = "failed"
	RunCancelled        = "cancelled"
	RunNeedsHuman       = "needs-human"
	RunAbandoned        = "abandoned"

	TaskPending          = "pending"
	TaskBlocked          = "blocked"
	TaskAwaitingApproval = "awaiting-approval"
	TaskReady            = "ready"
	TaskClaimed          = "claimed"
	TaskRunning          = "running"
	TaskPaused           = "paused"
	TaskCancelling       = "cancelling"
	TaskSucceeded        = "succeeded"
	TaskFailed           = "failed"
	TaskSkipped          = "skipped"
	TaskCancelled        = "cancelled"
	TaskNeedsHuman       = "needs-human"
)

var runTransitions = map[string]map[string]string{
	RunDraft: {
		"start_planning": RunPlanning, "reject": RunAbandoned, "cancel": RunCancelled, "finish_failure": RunFailed, "escalate": RunNeedsHuman, "abandon": RunAbandoned, "replay_same": RunDraft,
		"approve": "ErrApprovalRequired", "queue": "ErrApprovalRequired", "start_execution": "ErrApprovalRequired", "approve_stale": "ErrStaleApproval",
	},
	RunPlanning: {
		"start_planning": RunPlanning, "plan_ready_requires_approval": RunAwaitingApproval, "plan_ready_no_approval": RunApproved, "reject": RunAbandoned, "pause": RunPaused, "cancel": RunCancelling, "finish_failure": RunFailed, "escalate": RunNeedsHuman, "abandon": RunAbandoned, "replay_same": RunPlanning,
		"approve": "ErrApprovalRequired", "queue": "ErrApprovalRequired", "start_execution": "ErrApprovalRequired", "approve_stale": "ErrStaleApproval",
	},
	RunAwaitingApproval: {
		"approve": RunApproved, "reject": RunAbandoned, "pause": RunPaused, "cancel": RunCancelling, "finish_failure": RunFailed, "escalate": RunNeedsHuman, "abandon": RunAbandoned, "replay_same": RunAwaitingApproval,
		"start_planning": "ErrInvalidTransition", "plan_ready_requires_approval": "ErrDuplicateRecord", "plan_ready_no_approval": "ErrDuplicateRecord", "queue": "ErrApprovalRequired", "start_execution": "ErrApprovalRequired", "approve_stale": "ErrStaleApproval",
	},
	RunApproved: {
		"approve": RunApproved, "reject": RunAbandoned, "queue": RunQueued, "start_execution": RunRunning, "pause": RunPaused, "cancel": RunCancelling, "finish_failure": RunFailed, "escalate": RunNeedsHuman, "abandon": RunAbandoned, "replay_same": RunApproved,
		"plan_ready_requires_approval": "ErrDuplicateRecord", "plan_ready_no_approval": "ErrDuplicateRecord", "approve_stale": "ErrStaleApproval",
	},
	RunQueued: {
		"approve": RunQueued, "queue": RunQueued, "start_execution": RunRunning, "pause": RunPaused, "cancel": RunCancelling, "finish_success": RunSucceeded, "finish_failure": RunFailed, "escalate": RunNeedsHuman, "abandon": RunAbandoned, "replay_same": RunQueued,
		"plan_ready_requires_approval": "ErrDuplicateRecord", "plan_ready_no_approval": "ErrDuplicateRecord", "approve_stale": "ErrStaleApproval",
	},
	RunRunning: {
		"approve": RunRunning, "queue": RunRunning, "start_execution": RunRunning, "pause": RunPaused, "cancel": RunCancelling, "finish_success": RunSucceeded, "finish_failure": RunFailed, "escalate": RunNeedsHuman, "abandon": RunAbandoned, "replay_same": RunRunning,
		"plan_ready_requires_approval": "ErrDuplicateRecord", "plan_ready_no_approval": "ErrDuplicateRecord", "approve_stale": "ErrStaleApproval",
	},
	RunPaused: {
		"approve": RunPaused, "pause": RunPaused, "resume": RunQueued, "cancel": RunCancelling, "finish_success": RunSucceeded, "finish_failure": RunFailed, "escalate": RunNeedsHuman, "abandon": RunAbandoned, "replay_same": RunPaused,
		"plan_ready_requires_approval": "ErrDuplicateRecord", "plan_ready_no_approval": "ErrDuplicateRecord", "approve_stale": "ErrStaleApproval",
	},
	RunCancelling: {
		"cancel": RunCancelling, "finish_failure": RunCancelled, "escalate": RunNeedsHuman, "abandon": RunAbandoned, "replay_same": RunCancelling, "approve_stale": "ErrStaleApproval",
		"plan_ready_requires_approval": "ErrDuplicateRecord", "plan_ready_no_approval": "ErrDuplicateRecord",
	},
	RunSucceeded:  terminalRunEvents(RunSucceeded, "finish_success", "replay_same"),
	RunFailed:     terminalRunEvents(RunFailed, "finish_failure", "replay_same"),
	RunCancelled:  terminalRunEvents(RunCancelled, "cancel", "replay_same"),
	RunNeedsHuman: terminalRunEvents(RunNeedsHuman, "escalate", "replay_same"),
	RunAbandoned:  terminalRunEvents(RunAbandoned, "abandon", "replay_same"),
}

var taskTransitions = map[string]map[string]string{
	TaskPending: {
		"dependencies_ready": TaskReady, "dependencies_blocked": TaskBlocked, "require_approval": TaskAwaitingApproval, "approval_bound": TaskReady, "pause": TaskPaused, "cancel": TaskCancelled, "complete_failure": TaskFailed, "skip": TaskSkipped, "escalate": TaskNeedsHuman, "replay_same": TaskPending, "approval_stale": "ErrStaleApproval",
		"claim": "ErrInvalidTransition", "start": "ErrClaimRequired",
	},
	TaskBlocked: {
		"dependencies_ready": TaskReady, "dependencies_blocked": TaskBlocked, "require_approval": TaskAwaitingApproval, "approval_bound": TaskReady, "pause": TaskPaused, "cancel": TaskCancelled, "complete_failure": TaskFailed, "skip": TaskSkipped, "escalate": TaskNeedsHuman, "replay_same": TaskBlocked, "approval_stale": "ErrStaleApproval",
		"claim": "ErrInvalidTransition", "start": "ErrClaimRequired",
	},
	TaskAwaitingApproval: {
		"dependencies_ready": TaskAwaitingApproval, "dependencies_blocked": TaskBlocked, "require_approval": TaskAwaitingApproval, "approval_bound": TaskReady, "pause": TaskPaused, "cancel": TaskCancelled, "complete_failure": TaskFailed, "skip": TaskSkipped, "escalate": TaskNeedsHuman, "replay_same": TaskAwaitingApproval, "approval_stale": "ErrStaleApproval",
		"claim": "ErrApprovalRequired", "start": "ErrApprovalRequired",
	},
	TaskReady: {
		"dependencies_ready": TaskReady, "dependencies_blocked": TaskBlocked, "require_approval": TaskAwaitingApproval, "approval_bound": TaskReady, "claim": TaskClaimed, "pause": TaskPaused, "cancel": TaskCancelled, "complete_success": TaskSucceeded, "complete_failure": TaskFailed, "skip": TaskSkipped, "escalate": TaskNeedsHuman, "replay_same": TaskReady, "approval_stale": "ErrStaleApproval",
		"start": "ErrClaimRequired",
	},
	TaskClaimed: {
		"dependencies_ready": TaskClaimed, "approval_bound": TaskClaimed, "claim": TaskClaimed, "start": TaskRunning, "pause": TaskPaused, "cancel": TaskCancelling, "complete_success": TaskSucceeded, "complete_failure": TaskFailed, "escalate": TaskNeedsHuman, "replay_same": TaskClaimed, "approval_stale": "ErrStaleApproval",
	},
	TaskRunning: {
		"dependencies_ready": TaskRunning, "approval_bound": TaskRunning, "start": TaskRunning, "pause": TaskPaused, "cancel": TaskCancelling, "complete_success": TaskSucceeded, "complete_failure": TaskFailed, "escalate": TaskNeedsHuman, "replay_same": TaskRunning, "approval_stale": "ErrStaleApproval",
		"claim": "ErrClaimConflict",
	},
	TaskPaused: {
		"dependencies_ready": TaskPaused, "dependencies_blocked": TaskBlocked, "require_approval": TaskAwaitingApproval, "approval_bound": TaskPaused, "pause": TaskPaused, "resume": TaskReady, "cancel": TaskCancelling, "complete_success": TaskSucceeded, "complete_failure": TaskFailed, "skip": TaskSkipped, "escalate": TaskNeedsHuman, "replay_same": TaskPaused, "approval_stale": "ErrStaleApproval",
	},
	TaskCancelling: {
		"dependencies_ready": TaskCancelling, "dependencies_blocked": TaskCancelling, "cancel": TaskCancelling, "complete_failure": TaskCancelled, "escalate": TaskNeedsHuman, "replay_same": TaskCancelling, "approval_stale": "ErrStaleApproval",
	},
	TaskSucceeded:  terminalTaskEvents(TaskSucceeded, "complete_success", "replay_same"),
	TaskFailed:     terminalTaskEvents(TaskFailed, "complete_failure", "replay_same"),
	TaskSkipped:    terminalTaskEvents(TaskSkipped, "skip", "replay_same"),
	TaskCancelled:  terminalTaskEvents(TaskCancelled, "cancel", "replay_same"),
	TaskNeedsHuman: terminalTaskEvents(TaskNeedsHuman, "escalate", "replay_same"),
}

func DeliveryRunTransition(from, event string) (string, error) {
	return transition(runTransitions, normalizeState(from), strings.TrimSpace(event))
}

func TaskTransition(from, event string) (string, error) {
	return transition(taskTransitions, normalizeState(from), strings.TrimSpace(event))
}

func RunTerminal(state string) bool {
	switch normalizeState(state) {
	case RunSucceeded, RunFailed, RunCancelled, RunNeedsHuman, RunAbandoned:
		return true
	default:
		return false
	}
}

func TaskTerminal(state string) bool {
	switch normalizeState(state) {
	case TaskSucceeded, TaskFailed, TaskSkipped, TaskCancelled, TaskNeedsHuman:
		return true
	default:
		return false
	}
}

func transition(table map[string]map[string]string, from, event string) (string, error) {
	if event == "" {
		return "", typed(ErrInvalidTransitionCode, "event is required")
	}
	events, ok := table[from]
	if !ok {
		return "", typed(ErrInvalidRecordCode, "unknown state %q", from)
	}
	next, ok := events[event]
	if !ok {
		if isTerminalTransitionTable(table, from) {
			return "", typed(ErrTerminalStateCode, "%s cannot handle %s", from, event)
		}
		return "", typed(ErrInvalidTransitionCode, "%s cannot handle %s", from, event)
	}
	if strings.HasPrefix(next, "Err") {
		return "", typed(ErrorCode(next), "%s cannot handle %s", from, event)
	}
	return next, nil
}

func terminalRunEvents(state string, sameEvents ...string) map[string]string {
	events := map[string]string{"approve_stale": "ErrStaleApproval"}
	for _, event := range []string{"start_planning", "plan_ready_requires_approval", "plan_ready_no_approval", "approve", "reject", "queue", "start_execution", "pause", "resume", "cancel", "finish_success", "finish_failure", "escalate", "abandon"} {
		events[event] = "ErrTerminalState"
	}
	for _, event := range sameEvents {
		events[event] = state
	}
	return events
}

func terminalTaskEvents(state string, sameEvents ...string) map[string]string {
	events := map[string]string{"approval_stale": "ErrStaleApproval"}
	for _, event := range []string{"dependencies_ready", "dependencies_blocked", "require_approval", "approval_bound", "claim", "start", "pause", "resume", "cancel", "complete_success", "complete_failure", "skip", "escalate"} {
		events[event] = "ErrTerminalState"
	}
	for _, event := range sameEvents {
		events[event] = state
	}
	return events
}

func isTerminalTransitionTable(table map[string]map[string]string, state string) bool {
	return RunTerminal(state) || TaskTerminal(state)
}

func normalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}
