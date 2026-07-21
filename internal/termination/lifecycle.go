package termination

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Lifecycle is the single stop/join path for an attempt generation.
type Lifecycle struct {
	mu      sync.Mutex
	state   State
	genSeen map[string]int64 // attemptID -> last stop generation handled to terminal
	policy  Policy
	now     func() time.Time
	ctrl    Controller
	tree    TreeView
	flush   OutputFlusher
	release ReservationReleaser
	events  EventWriter
}

// Options configures Lifecycle.
type Options struct {
	Policy  Policy
	Now     func() time.Time
	Ctrl    Controller
	Tree    TreeView
	Flush   OutputFlusher
	Release ReservationReleaser
	Events  EventWriter
}

// New builds a Lifecycle. Ctrl is required.
func New(opts Options) (*Lifecycle, error) {
	if opts.Ctrl == nil {
		return nil, fmt.Errorf("termination: controller required")
	}
	p := opts.Policy
	if p.Grace <= 0 {
		p.Grace = DefaultGrace
	}
	if p.HardAfterGrace <= 0 {
		p.HardAfterGrace = DefaultHardAfterGrace
	}
	if p.CleanupBound <= 0 {
		p.CleanupBound = DefaultCleanupBound
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	flush := opts.Flush
	if flush == nil {
		flush = NopFlush{}
	}
	rel := opts.Release
	if rel == nil {
		rel = NopRelease{}
	}
	ev := opts.Events
	if ev == nil {
		ev = NopEvents{}
	}
	tree := opts.Tree
	if tree == nil {
		tree = staticTree{}
	}
	return &Lifecycle{
		state:   StateIdle,
		genSeen: map[string]int64{},
		policy:  p,
		now:     now,
		ctrl:    opts.Ctrl,
		tree:    tree,
		flush:   flush,
		release: rel,
		events:  ev,
	}, nil
}

type staticTree struct{}

func (staticTree) Snapshot(Target) (int, int, bool, error) { return 0, 0, false, nil }

// State returns the current lifecycle state.
func (l *Lifecycle) State() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

// Stop runs the full termination path. It is idempotent for the same
// attempt+generation once terminal_clean or attention_required is reached.
// The caller context may be cancelled; cleanup uses an independent bound.
func (l *Lifecycle) Stop(ctx context.Context, target Target, reason Reason) (Result, error) {
	if err := validateTarget(target); err != nil {
		return Result{}, err
	}
	if reason == "" {
		reason = ReasonCancel
	}

	l.mu.Lock()
	// Idempotent: already finished this generation.
	if last, ok := l.genSeen[target.AttemptID]; ok && last == target.Generation {
		st := l.state
		l.mu.Unlock()
		if st == StateAttentionRequired {
			return Result{State: st, TerminalClean: false}, ErrAttentionRequired
		}
		return Result{State: StateTerminalClean, TerminalClean: true}, nil
	}
	// Wrong generation already terminal under a different gen cannot be signalled.
	if last, ok := l.genSeen[target.AttemptID]; ok && last != target.Generation {
		l.mu.Unlock()
		return Result{}, ErrGenerationMismatch
	}
	l.state = StateStopping
	l.mu.Unlock()

	var transitions []Transition
	emit := func(from, to State, sig SignalKind, note string) {
		t := Transition{
			SchemaVersion: SchemaVersion,
			AttemptID:     target.AttemptID,
			Generation:    target.Generation,
			From:          from,
			To:            to,
			Reason:        reason,
			Signal:        sig,
			Note:          note,
			At:            l.now().UTC(),
		}
		_ = l.events.Write(t)
		transitions = append(transitions, t)
	}

	// Independent cleanup context — caller cancel cannot skip it.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), l.policy.CleanupBound)
	defer cancel()

	ev := JoinEvidence{}

	// Already exited root: skip signals, still join/flush/release.
	alive, err := l.ctrl.Alive(target)
	if err != nil {
		if errors.Is(err, ErrGenerationMismatch) || errors.Is(err, ErrNotOwned) {
			return Result{}, err
		}
		return l.failAttention(target, transitions, ev, "alive_probe_failed")
	}
	if alive {
		// Graceful stop.
		if err := l.ctrl.Signal(target, SignalTerm); err != nil {
			if err == ErrGenerationMismatch || err == ErrNotOwned {
				return Result{}, err
			}
			// Signal failure may mean already gone; continue join path.
			emit(StateStopping, StateStopping, SignalTerm, "signal_term_error")
		} else {
			emit(StateStopping, StateStopping, SignalTerm, "signal_term")
		}

		graceCtx, graceCancel := context.WithTimeout(cleanupCtx, l.policy.Grace)
		waitErr := l.ctrl.Wait(graceCtx, target)
		graceCancel()

		still, _ := l.ctrl.Alive(target)
		if still || waitErr != nil {
			l.setState(StateEscalating)
			emit(StateStopping, StateEscalating, SignalKill, "grace_elapsed")
			if err := l.ctrl.Signal(target, SignalKill); err != nil && err != ErrNotOwned && err != ErrGenerationMismatch {
				emit(StateEscalating, StateEscalating, SignalKill, "signal_kill_error")
			} else {
				emit(StateEscalating, StateEscalating, SignalKill, "signal_kill")
			}
			hardCtx, hardCancel := context.WithTimeout(cleanupCtx, l.policy.HardAfterGrace)
			_ = l.ctrl.Wait(hardCtx, target)
			hardCancel()
		}
	} else {
		emit(StateStopping, StateJoining, "", "already_exited")
	}

	l.setState(StateJoining)
	ev.RootExited = true
	if alive, _ := l.ctrl.Alive(target); alive {
		ev.RootExited = false
	}

	ownedAlive, escaped, unknown, err := l.tree.Snapshot(target)
	if err != nil {
		return l.failAttention(target, transitions, ev, "tree_snapshot_failed")
	}
	ev.OwnedJoined = 0
	if ownedAlive == 0 && !unknown {
		ev.OwnedJoined = 1 // root accounted; no owned descendants remain
	}
	ev.EscapedChildren = escaped
	ev.UnknownChildren = unknown

	if escaped > 0 || unknown || !ev.RootExited || ownedAlive > 0 {
		if !ev.RootExited {
			ev.AttentionReason = "root_still_alive"
		} else if escaped > 0 {
			ev.AttentionReason = "escaped_children"
		} else if unknown {
			ev.AttentionReason = "unobservable_children"
		} else {
			ev.AttentionReason = "owned_descendants_remain"
		}
		// Do not release reservation on ambiguous or incomplete ownership.
		emit(StateJoining, StateAttentionRequired, "", ev.AttentionReason)
		l.markTerminal(target, StateAttentionRequired)
		return Result{
			State:         StateAttentionRequired,
			Evidence:      ev,
			Transitions:   transitions,
			TerminalClean: false,
		}, ErrAttentionRequired
	}

	// Flush output under cleanup bound.
	if err := l.flush.Flush(cleanupCtx, target.AttemptID); err != nil {
		return l.failAttention(target, transitions, ev, "output_flush_failed")
	}
	ev.OutputFlushed = true

	// Release reservation only when tree is clean.
	if target.ReservationID != "" {
		if err := l.release.Release(cleanupCtx, target.ReservationID, target.ReservationGeneration); err != nil {
			return l.failAttention(target, transitions, ev, "reservation_release_failed")
		}
		ev.ReservationFreed = true
	} else {
		ev.ReservationFreed = true
	}

	emit(StateJoining, StateTerminalClean, "", "terminal_clean")
	l.markTerminal(target, StateTerminalClean)
	// Caller ctx cancel must not matter — we finished under cleanupCtx.
	_ = ctx
	return Result{
		State:         StateTerminalClean,
		Evidence:      ev,
		Transitions:   transitions,
		TerminalClean: true,
	}, nil
}

func (l *Lifecycle) failAttention(target Target, transitions []Transition, ev JoinEvidence, reason string) (Result, error) {
	ev.AttentionReason = reason
	t := Transition{
		SchemaVersion: SchemaVersion,
		AttemptID:     target.AttemptID,
		Generation:    target.Generation,
		From:          StateJoining,
		To:            StateAttentionRequired,
		Note:          reason,
		At:            l.now().UTC(),
	}
	_ = l.events.Write(t)
	transitions = append(transitions, t)
	l.markTerminal(target, StateAttentionRequired)
	return Result{
		State:         StateAttentionRequired,
		Evidence:      ev,
		Transitions:   transitions,
		TerminalClean: false,
	}, ErrAttentionRequired
}

func (l *Lifecycle) setState(st State) {
	l.mu.Lock()
	l.state = st
	l.mu.Unlock()
}

func (l *Lifecycle) markTerminal(target Target, st State) {
	l.mu.Lock()
	l.state = st
	l.genSeen[target.AttemptID] = target.Generation
	l.mu.Unlock()
}

func validateTarget(t Target) error {
	if t.AttemptID == "" || t.Generation <= 0 || t.RootPID <= 0 {
		return ErrInvalidTarget
	}
	return nil
}
