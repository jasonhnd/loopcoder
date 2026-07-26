package processrecovery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Reconciler classifies and applies recovery decisions.
type Reconciler struct {
	prober  LiveProber
	events  EventSink
	release ReservationReleaser
	now     func() time.Time
	// applied DecisionIDs for in-memory idempotency when sink is mem.
	seen map[string]Decision
}

// Options configures Reconciler.
type Options struct {
	Prober  LiveProber
	Events  EventSink
	Release ReservationReleaser
	Now     func() time.Time
}

// New builds a Reconciler. Prober is required.
func New(opts Options) (*Reconciler, error) {
	if opts.Prober == nil {
		return nil, fmt.Errorf("processrecovery: prober required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ev := opts.Events
	if ev == nil {
		ev = &memSink{}
	}
	rel := opts.Release
	if rel == nil {
		rel = nopRelease{}
	}
	return &Reconciler{
		prober:  opts.Prober,
		events:  ev,
		release: rel,
		now:     now,
		seen:    map[string]Decision{},
	}, nil
}

type nopRelease struct{}

func (nopRelease) Release(string, int64) (bool, error) { return false, nil }

type memSink struct {
	terminals map[string]bool
	decisions map[string]Decision
}

func (m *memSink) EmitTerminal(attemptID string, generation int64, _ int, _ string) (bool, error) {
	if m.terminals == nil {
		m.terminals = map[string]bool{}
	}
	k := fmt.Sprintf("%s:%d", attemptID, generation)
	if m.terminals[k] {
		return false, nil
	}
	m.terminals[k] = true
	return true, nil
}

func (m *memSink) EmitDecision(d Decision) (bool, error) {
	if m.decisions == nil {
		m.decisions = map[string]Decision{}
	}
	if _, ok := m.decisions[d.DecisionID]; ok {
		return false, nil
	}
	m.decisions[d.DecisionID] = d
	return true, nil
}

// Classify is pure policy from evidence + live observation.
func Classify(ev PersistedEvidence, live LiveObservation) Decision {
	d := Decision{
		SchemaVersion: SchemaVersion,
		AttemptID:     ev.AttemptID,
		Generation:    ev.Generation,
		At:            time.Time{}, // filled by Apply
	}
	if ev.AttemptID == "" || ev.Generation <= 0 {
		d.Kind = DecisionAttentionRequired
		d.OperatorAction = ActionHumanAttention
		d.Reasons = []string{"invalid_evidence"}
		return d
	}
	if ev.TerminalRecorded {
		d.Kind = DecisionTerminalClean
		d.OperatorAction = ActionAlreadyTerminal
		d.Reasons = []string{"terminal_already_recorded"}
		return d
	}
	if !ev.LaunchRecorded {
		d.Kind = DecisionNeverStarted
		d.OperatorAction = ActionNewAttempt
		d.RelaunchAllowed = true
		d.Reasons = []string{"never_started"}
		return d
	}
	if live.ObservationIncomplete {
		d.Kind = DecisionUnknown
		d.OperatorAction = ActionHumanAttention
		d.Reasons = []string{"observation_incomplete"}
		return d
	}
	// Launch recorded, check live.
	if live.PIDAlive {
		if live.BirthMatches && live.ExecMatches && (ev.PGID == 0 || live.PGIDMatches) {
			if ev.ProcessBirthIdentity == "" || ev.ExecutableIdentity == "" {
				d.Kind = DecisionAttentionRequired
				d.OperatorAction = ActionHumanAttention
				d.Reasons = []string{"incomplete_launch_identity"}
				return d
			}
			d.Kind = DecisionAdopt
			d.OperatorAction = ActionContinueObserve
			d.Adopted = true
			d.Reasons = []string{"exact_live_match"}
			return d
		}
		// Alive but identity mismatch → PID reuse.
		d.Kind = DecisionPIDReused
		d.OperatorAction = ActionHumanAttention
		d.Reasons = []string{"pid_alive_identity_mismatch"}
		return d
	}
	// Root not alive.
	if live.OwnedDescendantsAlive > 0 {
		d.Kind = DecisionDescendantsOnly
		d.OperatorAction = ActionHumanAttention
		d.Reasons = []string{"descendants_only"}
		return d
	}
	if ev.ExitObserved {
		d.Kind = DecisionExitedUnrecorded
		d.OperatorAction = ActionJoinFinalize
		d.Reasons = []string{"exited_unrecorded"}
		return d
	}
	// Launch recorded, root dead, no exit evidence, no descendants.
	d.Kind = DecisionUnknown
	d.OperatorAction = ActionHumanAttention
	d.Reasons = []string{"launch_recorded_root_gone_no_exit"}
	return d
}

// Apply classifies, persists decision idempotently, and performs safe side effects
// (terminal finalize / reservation release) without launching work.
func (r *Reconciler) Apply(ev PersistedEvidence) (Decision, error) {
	if ev.AttemptID == "" || ev.Generation <= 0 {
		return Decision{}, ErrInvalidEvidence
	}
	// Stable decision id from evidence fingerprint (crash-window idempotent).
	id := decisionID(ev)
	if prev, ok := r.seen[id]; ok {
		prev.Replay = true
		return prev, nil
	}

	live, err := r.prober.Observe(ev)
	if err != nil {
		return Decision{}, err
	}
	d := Classify(ev, live)
	d.DecisionID = id
	d.At = r.now().UTC()

	// If LastDecisionID matches, treat as replay without side effects.
	if ev.LastDecisionID != "" && ev.LastDecisionID == id {
		d.Replay = true
		r.seen[id] = d
		return d, nil
	}

	emitted, err := r.events.EmitDecision(d)
	if err != nil {
		return Decision{}, err
	}
	if !emitted {
		d.Replay = true
		r.seen[id] = d
		return d, nil
	}

	switch d.Kind {
	case DecisionAdopt:
		// No launch; observation continues under adopted authority.
	case DecisionNeverStarted:
		// No silent execute.
	case DecisionExitedUnrecorded:
		termEmitted, err := r.events.EmitTerminal(ev.AttemptID, ev.Generation, ev.ExitCode, "recovery_join_finalize")
		if err != nil {
			return Decision{}, err
		}
		d.TerminalEventEmitted = termEmitted
		if ev.ReservationID != "" && !ev.ReservationReleased {
			released, err := r.release.Release(ev.ReservationID, ev.ReservationGeneration)
			if err != nil {
				return Decision{}, err
			}
			d.ReservationReleased = released
		}
	case DecisionTerminalClean:
		// already done
	case DecisionPIDReused, DecisionDescendantsOnly, DecisionUnknown, DecisionAttentionRequired:
		// Retain evidence; no reservation release (ambiguous).
	}

	r.seen[id] = d
	return d, nil
}

func decisionID(ev PersistedEvidence) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%d|%s|%v|%v|%v",
		ev.ProjectID, ev.AttemptID, ev.Generation, ev.RootPID,
		ev.ProcessBirthIdentity, ev.LaunchRecorded, ev.TerminalRecorded, ev.ExitObserved)
	return "rec_" + hex.EncodeToString(h.Sum(nil))[:24]
}
