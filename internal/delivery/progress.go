package delivery

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/progress"
)

func emitDeliveryRunProgress(ctx context.Context, emitter *progress.Emitter, run DeliveryRun, event string, now time.Time) {
	if emitter == nil {
		return
	}
	state := strings.TrimSpace(run.State)
	observation := progress.Observation{
		ProjectID:     run.ProjectID,
		DeliveryRunID: run.DeliveryRunID,
		RunID:         firstNonEmpty(run.RunID, run.DeliveryRunID),
		CorrelationID: "delivery-run:" + run.DeliveryRunID,
		Phase:         "delivery-run",
		Status:        state,
		KnownState:    knownRunProgressState(state),
		Reason:        progress.ReasonStateChange,
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "delivery-run",
			RecordID:       run.DeliveryRunID,
			Summary:        "delivery run state changed via " + strings.TrimSpace(event),
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		OccurredAt: now,
		Terminal:   RunTerminal(state),
	}
	emitProgressObservation(ctx, emitter, observation)
}

func emitTaskProgress(ctx context.Context, emitter *progress.Emitter, task Task, event string, now time.Time) {
	if emitter == nil {
		return
	}
	state := strings.TrimSpace(task.State)
	observation := progress.Observation{
		ProjectID:     task.ProjectID,
		DeliveryRunID: task.DeliveryRunID,
		RunID:         task.DeliveryRunID,
		TaskID:        task.TaskID,
		CorrelationID: "task:" + task.TaskID,
		Phase:         "task",
		Status:        state,
		KnownState:    knownTaskProgressState(state),
		Reason:        progress.ReasonStateChange,
		TaskCounts:    taskProgressCounts(state),
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "delivery-task",
			RecordID:       task.TaskID,
			Summary:        "task state changed via " + strings.TrimSpace(event),
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		OccurredAt: now,
		Terminal:   TaskTerminal(state),
	}
	emitProgressObservation(ctx, emitter, observation)
}

func emitAttemptProgress(ctx context.Context, emitter *progress.Emitter, attempt Attempt, event string, now time.Time) {
	if emitter == nil {
		return
	}
	state := strings.TrimSpace(attempt.State)
	observation := progress.Observation{
		ProjectID:      attempt.ProjectID,
		DeliveryRunID:  attempt.DeliveryRunID,
		RunID:          attempt.DeliveryRunID,
		TaskID:         attempt.TaskID,
		AttemptID:      attempt.AttemptID,
		AttemptOrdinal: attempt.AttemptOrdinal,
		CorrelationID:  "attempt:" + attempt.AttemptID,
		Phase:          "attempt",
		Status:         state,
		KnownState:     knownAttemptProgressState(state),
		Reason:         progress.ReasonStateChange,
		TaskCounts:     attemptProgressCounts(state),
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "delivery-attempt",
			RecordID:       attempt.AttemptID,
			Summary:        "attempt state changed via " + strings.TrimSpace(event),
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		OccurredAt: now,
		Terminal:   attemptTerminal(state),
	}
	emitProgressObservation(ctx, emitter, observation)
}

func emitApprovalProgress(ctx context.Context, emitter *progress.Emitter, approval Approval, event string, now time.Time) {
	if emitter == nil {
		return
	}
	state := strings.TrimSpace(approval.Status)
	observation := progress.Observation{
		ProjectID:     approval.ProjectID,
		DeliveryRunID: approval.DeliveryRunID,
		RunID:         approval.DeliveryRunID,
		CorrelationID: "approval:" + approval.ApprovalID,
		Phase:         "approval",
		Status:        state,
		Reason:        progress.ReasonStateChange,
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "delivery-approval",
			RecordID:       approval.ApprovalID,
			Summary:        "approval state changed via " + strings.TrimSpace(event),
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		OccurredAt: now,
	}
	if state != "active" {
		observation.KnownState = progress.KnownWaitingApproval
	}
	emitProgressObservation(ctx, emitter, observation)
}

func emitProgressObservation(ctx context.Context, emitter *progress.Emitter, observation progress.Observation) {
	var err error
	if observation.Terminal {
		_, err = emitter.Terminal(ctx, observation)
	} else {
		_, err = emitter.Emit(ctx, observation)
	}
	if err != nil && !errors.Is(err, progress.ErrEmitterClosed) {
		// Receipt generation is best-effort at this layer. Durable delivery state
		// is already committed and must not be rolled back by a reporting failure.
		return
	}
}

func knownRunProgressState(state string) string {
	switch strings.TrimSpace(state) {
	case RunAwaitingApproval:
		return progress.KnownWaitingApproval
	case RunQueued, RunRunning, RunApproved:
		return progress.KnownDeliveryPending
	case RunCancelling, RunCancelled:
		return progress.KnownCancellationInProgress
	case RunNeedsHuman, RunPaused:
		return progress.KnownBlocked
	case RunSucceeded, RunFailed, RunAbandoned:
		return progress.KnownTerminal
	default:
		return ""
	}
}

func knownTaskProgressState(state string) string {
	switch strings.TrimSpace(state) {
	case TaskAwaitingApproval:
		return progress.KnownWaitingApproval
	case TaskBlocked, TaskNeedsHuman, TaskPaused:
		return progress.KnownBlocked
	case TaskReady, TaskClaimed, TaskRunning:
		return progress.KnownDeliveryPending
	case TaskCancelling, TaskCancelled:
		return progress.KnownCancellationInProgress
	case TaskSucceeded, TaskFailed, TaskSkipped:
		return progress.KnownTerminal
	default:
		return ""
	}
}

func knownAttemptProgressState(state string) string {
	switch strings.TrimSpace(state) {
	case AttemptClaimed, AttemptLaunching, AttemptRunning:
		return progress.KnownDeliveryPending
	case AttemptFailed, AttemptNeedsHuman, AttemptStale, AttemptSuperseded:
		return progress.KnownBlocked
	case AttemptCancelled:
		return progress.KnownCancellationInProgress
	case AttemptSucceeded:
		return progress.KnownTerminal
	default:
		return ""
	}
}

func taskProgressCounts(state string) progress.TaskCounts {
	counts := progress.TaskCounts{Total: 1}
	switch strings.TrimSpace(state) {
	case TaskPending, TaskReady:
		counts.Ready = 1
	case TaskClaimed, TaskRunning:
		counts.Running = 1
	case TaskSucceeded:
		counts.Succeeded = 1
	case TaskFailed, TaskCancelled, TaskSkipped:
		counts.Failed = 1
	case TaskBlocked, TaskAwaitingApproval, TaskPaused, TaskCancelling, TaskNeedsHuman:
		counts.Blocked = 1
	default:
		counts.Unknown = 1
	}
	return counts
}

func attemptProgressCounts(state string) progress.TaskCounts {
	counts := progress.TaskCounts{Total: 1}
	switch strings.TrimSpace(state) {
	case AttemptPlanned, AttemptClaimed, AttemptLaunching:
		counts.Ready = 1
	case AttemptRunning:
		counts.Running = 1
	case AttemptSucceeded:
		counts.Succeeded = 1
	case AttemptFailed, AttemptCancelled, AttemptStale, AttemptSuperseded:
		counts.Failed = 1
	case AttemptNeedsHuman:
		counts.Blocked = 1
	default:
		counts.Unknown = 1
	}
	return counts
}

func attemptTerminal(state string) bool {
	switch strings.TrimSpace(state) {
	case AttemptSucceeded, AttemptFailed, AttemptCancelled, AttemptNeedsHuman, AttemptStale, AttemptSuperseded:
		return true
	default:
		return false
	}
}
