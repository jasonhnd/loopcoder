package worker

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/progresshost"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const progressPersistTimeout = 5 * time.Second

type progressRecorder struct {
	emitter  *progress.Emitter
	store    storage.Store
	now      func() time.Time
	warnings io.Writer
	stopOnce sync.Once
}

func newProgressRecorder(ctx context.Context, opts Options, deps Deps, roots runtimepath.Roots, jobID string, warnings io.Writer, validateOwnership func(context.Context) error) (*progressRecorder, error) {
	if !roots.Registered || strings.TrimSpace(roots.ProjectID) == "" || strings.TrimSpace(roots.DatabasePath) == "" {
		return nil, nil
	}
	openStore := deps.OpenProgressStore
	if openStore == nil {
		openStore = storage.Open
	}
	store, err := openStore(ctx, storage.Options{Path: roots.DatabasePath, Now: deps.Now})
	if err != nil {
		return nil, fmt.Errorf("open progress receipt store: %w", err)
	}
	progressStore := storage.Store(store)
	if validateOwnership != nil {
		progressStore = ownershipValidatedProgressStore{Store: store, validate: validateOwnership}
	}
	// Negotiate an active foreground sink for the current host profile.
	// Human-readable progress uses warnings/stderr so strict machine JSON
	// stdout stays clean. Durable outbox remains always-on via the store path.
	preferJSONL := strings.EqualFold(strings.TrimSpace(os.Getenv("LOOPCODER_PROGRESS_JSONL")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("LOOPCODER_PROGRESS_JSONL")), "true")
	sink, negotiation, contract := progresshost.NegotiateActiveSink(progresshost.ActiveSinkOptions{
		HumanWriter: warnings,
		PreferJSONL: preferJSONL,
		Getenv:      os.Getenv,
		Now:         deps.Now,
	})
	if warnings != nil {
		fmt.Fprintf(warnings, "[loopcoder] progress sink negotiated: %s host=%s (%s)\n", negotiation.Selected, contract.Profile, negotiation.Reason)
	}
	emitter, err := progress.NewEmitter(progress.EmitterOptions{
		Store:              progressStore,
		ProjectID:          roots.ProjectID,
		DeliveryRunID:      opts.RunID,
		RunID:              opts.RunID,
		CorrelationID:      jobID,
		MaxSilenceInterval: deps.ProgressMaxSilence,
		Clock:              deps.ProgressClock,
		Deliver:            progress.DeliveryFuncFromSink(sink),
		DeliveryObligation: progresshost.CurrentObligationFactory(),
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	recorder := &progressRecorder{
		emitter:  emitter,
		store:    store,
		now:      deps.Now,
		warnings: warnings,
	}
	if _, err := emitter.Start(context.Background()); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("start progress receipt emitter: %w", err)
	}
	return recorder, nil
}

type ownershipValidatedProgressStore struct {
	storage.Store
	validate func(context.Context) error
}

func (s ownershipValidatedProgressStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	if s.validate != nil {
		if err := s.validate(ctx); err != nil {
			return err
		}
	}
	return s.Store.WithWriteTx(ctx, fn)
}

func (r *progressRecorder) RecordAttempt(attempt state.AttemptRecord, terminal bool) {
	if r == nil || r.emitter == nil {
		return
	}
	observation := r.observation(attempt)
	observation.Terminal = terminal
	ctx, cancel := context.WithTimeout(context.Background(), progressPersistTimeout)
	defer cancel()
	var err error
	if terminal {
		_, err = r.emitter.Terminal(ctx, observation)
	} else {
		_, err = r.emitter.Emit(ctx, observation)
	}
	if err != nil && !strings.Contains(err.Error(), progress.ErrEmitterClosed.Error()) {
		fmt.Fprintf(r.warnings, "[loopcoder] warning: failed to persist progress receipt for %s/%s: %v\n", attempt.JobID, attempt.Phase, err)
	}
}

func (r *progressRecorder) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), progressPersistTimeout)
		defer cancel()
		if r.emitter != nil {
			if err := r.emitter.Stop(ctx); err != nil {
				fmt.Fprintf(r.warnings, "[loopcoder] warning: failed to stop progress receipt ticker: %v\n", err)
			}
		}
		if r.store != nil {
			if err := r.store.Close(); err != nil {
				fmt.Fprintf(r.warnings, "[loopcoder] warning: failed to close progress receipt store: %v\n", err)
			}
		}
	})
}

func (r *progressRecorder) observation(attempt state.AttemptRecord) progress.Observation {
	now := r.now()
	heartbeat := ageEvidence(attempt.HeartbeatAt, now)
	meaningful := ageEvidence(attempt.LastProgressAt, now)
	status := strings.TrimSpace(attempt.Status)
	phase := strings.TrimSpace(attempt.Phase)
	blocker, nextAction, gaps := attemptTruth(status, phase)
	return progress.Observation{
		TaskID:         fmt.Sprintf("issue-%d", attempt.Issue),
		AttemptID:      attempt.JobID,
		AttemptOrdinal: attempt.Attempt,
		Phase:          phase,
		Status:         status,
		TaskCounts:     attemptTaskCounts(status),
		Provider: progress.ProviderIdentity{
			ProviderID:         attempt.Provider,
			ProviderConfidence: "exact",
		},
		Heartbeat: heartbeat,
		Progress:  meaningful,
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "attempt-sidecar",
			RecordID:       attempt.JobID,
			Summary:        "worker attempt state changed",
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		QuotaBudget: progress.QuotaBudgetState{
			State:             progress.Unknown,
			Confidence:        progress.Unknown,
			RemainingQuantity: -1,
			GapReasons:        []string{"not-collected"},
		},
		Blocker:    blocker,
		NextAction: nextAction,
		GapReasons: gaps,
		OccurredAt: now,
	}
}

func attemptTaskCounts(status string) progress.TaskCounts {
	counts := progress.TaskCounts{Total: 1}
	switch state.NormalizeStatus(status) {
	case state.StatusPlanned, state.StatusQueued, state.StatusLaunching:
		counts.Ready = 1
	case state.StatusRunning, state.StatusFinishing:
		counts.Running = 1
	case state.StatusSucceeded, state.StatusSucceededWithOptionalFailures:
		counts.Succeeded = 1
	case state.StatusFailed, state.StatusCancelled, state.StatusTimedOut, state.StatusAbandoned, state.StatusSkipped:
		counts.Failed = 1
	case state.StatusHung, state.StatusNeedsHuman, state.StatusWaiting:
		counts.Blocked = 1
	default:
		counts.Unknown = 1
	}
	return counts
}

func ageEvidence(value string, now time.Time) progress.AgeEvidence {
	value = strings.TrimSpace(value)
	if value == "" {
		return progress.AgeEvidence{State: progress.Unknown, AgeMillis: -1}
	}
	age := int64(-1)
	if parsed, err := state.ParseTimestamp(value); err == nil {
		age = now.UTC().Sub(parsed.UTC()).Milliseconds()
		if age < 0 {
			age = 0
		}
	}
	return progress.AgeEvidence{State: "exact", ObservedAt: value, AgeMillis: age}
}

func attemptTruth(status, phase string) (progress.ActionState, progress.ActionState, []string) {
	status = strings.ToLower(strings.TrimSpace(status))
	phase = strings.ToLower(strings.TrimSpace(phase))
	switch status {
	case state.StatusSucceeded:
		return progress.ActionState{State: "none"}, progress.ActionState{State: "complete", Summary: "worker attempt succeeded"}, nil
	case state.StatusCancelled:
		return progress.ActionState{State: "cancelled", Summary: "worker attempt cancelled"}, progress.ActionState{State: "stop", Summary: "terminal cancellation recorded"}, []string{"cancellation"}
	case state.StatusTimedOut:
		return progress.ActionState{State: "timeout", Summary: "worker attempt timed out"}, progress.ActionState{State: "stop", Summary: "terminal timeout recorded"}, []string{"timeout"}
	case state.StatusHung:
		return progress.ActionState{State: "stalled", Summary: "worker stall policy fired"}, progress.ActionState{State: "recover", Summary: "inspect hung worker log and recovery artifacts"}, []string{"alive-but-no-meaningful-progress"}
	case state.StatusNeedsHuman:
		return progress.ActionState{State: "needs-human", Summary: "human review required"}, progress.ActionState{State: "wait-human", Summary: "wait for human recovery or review"}, []string{"blocked"}
	case state.StatusFailed:
		return progress.ActionState{State: "failed", Summary: "worker attempt failed"}, progress.ActionState{State: "recover", Summary: "write recovery context or retry"}, []string{"recovery"}
	}
	switch {
	case strings.Contains(phase, "codex_started") || strings.Contains(phase, "claude_started") || strings.Contains(phase, "gemini_started"):
		return progress.ActionState{State: "none"}, progress.ActionState{State: "wait-provider", Summary: "worker process is alive; no model-authored progress is inferred"}, []string{"alive-but-no-meaningful-progress"}
	case strings.Contains(phase, "dirty_checked") || strings.Contains(phase, "committed") || strings.Contains(phase, "pushed") || strings.Contains(phase, "pr_opened"):
		return progress.ActionState{State: "none"}, progress.ActionState{State: "finalize", Summary: "local delivery step is in progress"}, []string{"delivery-pending"}
	default:
		return progress.ActionState{State: "none"}, progress.ActionState{State: "continue", Summary: "continue supervising worker attempt"}, nil
	}
}
