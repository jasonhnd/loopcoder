package processrecovery_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/processrecovery"
)

type staticProber struct {
	live processrecovery.LiveObservation
	err  error
}

func (s staticProber) Observe(processrecovery.PersistedEvidence) (processrecovery.LiveObservation, error) {
	return s.live, s.err
}

type countRelease struct {
	n int
}

func (c *countRelease) Release(string, int64) (bool, error) {
	c.n++
	return true, nil
}

func baseEv() processrecovery.PersistedEvidence {
	return processrecovery.PersistedEvidence{
		ProjectID:            "proj-a",
		AttemptID:            "att-1",
		Generation:           1,
		RootPID:              99,
		PGID:                 99,
		ProcessBirthIdentity: "birth-abc",
		ExecutableIdentity:   "exec-xyz",
		LaunchRecorded:       true,
	}
}

func newRec(t *testing.T, live processrecovery.LiveObservation, rel processrecovery.ReservationReleaser) *processrecovery.Reconciler {
	t.Helper()
	r, err := processrecovery.New(processrecovery.Options{
		Prober:  staticProber{live: live},
		Release: rel,
		Now:     func() time.Time { return time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAdoptExactLiveMatch(t *testing.T) {
	r := newRec(t, processrecovery.LiveObservation{
		PIDAlive: true, BirthMatches: true, ExecMatches: true, PGIDMatches: true,
	}, nil)
	d, err := r.Apply(baseEv())
	if err != nil || !d.Adopted || d.Kind != processrecovery.DecisionAdopt {
		t.Fatalf("%#v err=%v", d, err)
	}
	if d.OperatorAction != processrecovery.ActionContinueObserve {
		t.Fatalf("action=%s", d.OperatorAction)
	}
	// Idempotent
	d2, err := r.Apply(baseEv())
	if err != nil || !d2.Replay || d2.DecisionID != d.DecisionID {
		t.Fatalf("replay %#v err=%v", d2, err)
	}
}

func TestNeverStartedNoSilentLaunch(t *testing.T) {
	ev := baseEv()
	ev.LaunchRecorded = false
	r := newRec(t, processrecovery.LiveObservation{}, nil)
	d, err := r.Apply(ev)
	if err != nil || d.Kind != processrecovery.DecisionNeverStarted || !d.RelaunchAllowed {
		t.Fatalf("%#v err=%v", d, err)
	}
	if d.OperatorAction != processrecovery.ActionNewAttempt {
		t.Fatal("must require new attempt")
	}
	if d.Adopted {
		t.Fatal("must not adopt")
	}
}

func TestPIDReuseAttention(t *testing.T) {
	r := newRec(t, processrecovery.LiveObservation{
		PIDAlive: true, BirthMatches: false, ExecMatches: true, PGIDMatches: true,
	}, nil)
	d, err := r.Apply(baseEv())
	if err != nil || d.Kind != processrecovery.DecisionPIDReused {
		t.Fatalf("%#v err=%v", d, err)
	}
	if d.OperatorAction != processrecovery.ActionHumanAttention || d.Adopted {
		t.Fatalf("%#v", d)
	}
}

func TestDescendantsOnlyAttention(t *testing.T) {
	r := newRec(t, processrecovery.LiveObservation{
		PIDAlive: false, OwnedDescendantsAlive: 2,
	}, nil)
	d, err := r.Apply(baseEv())
	if err != nil || d.Kind != processrecovery.DecisionDescendantsOnly {
		t.Fatalf("%#v err=%v", d, err)
	}
}

func TestExitedUnrecordedFinalizeOnce(t *testing.T) {
	rel := &countRelease{}
	ev := baseEv()
	ev.ExitObserved = true
	ev.ExitCode = 0
	ev.ReservationID = "res-1"
	ev.ReservationGeneration = 3
	r := newRec(t, processrecovery.LiveObservation{PIDAlive: false}, rel)
	d, err := r.Apply(ev)
	if err != nil || d.Kind != processrecovery.DecisionExitedUnrecorded {
		t.Fatalf("%#v err=%v", d, err)
	}
	if !d.TerminalEventEmitted || !d.ReservationReleased || rel.n != 1 {
		t.Fatalf("%#v rel=%d", d, rel.n)
	}
	d2, err := r.Apply(ev)
	if err != nil || !d2.Replay {
		t.Fatalf("replay %#v err=%v", d2, err)
	}
	if rel.n != 1 {
		t.Fatal("duplicate reservation release")
	}
}

func TestIncompleteObservationUnknown(t *testing.T) {
	r := newRec(t, processrecovery.LiveObservation{ObservationIncomplete: true}, nil)
	d, err := r.Apply(baseEv())
	if err != nil || d.Kind != processrecovery.DecisionUnknown {
		t.Fatalf("%#v err=%v", d, err)
	}
}

func TestTerminalAlreadyRecorded(t *testing.T) {
	ev := baseEv()
	ev.TerminalRecorded = true
	r := newRec(t, processrecovery.LiveObservation{}, nil)
	d, err := r.Apply(ev)
	if err != nil || d.Kind != processrecovery.DecisionTerminalClean {
		t.Fatalf("%#v err=%v", d, err)
	}
}

func TestIncompleteLaunchIdentityNoAdopt(t *testing.T) {
	ev := baseEv()
	ev.ProcessBirthIdentity = ""
	r := newRec(t, processrecovery.LiveObservation{
		PIDAlive: true, BirthMatches: true, ExecMatches: true, PGIDMatches: true,
	}, nil)
	d, err := r.Apply(ev)
	if err != nil || d.Kind != processrecovery.DecisionAttentionRequired || d.Adopted {
		t.Fatalf("%#v err=%v", d, err)
	}
}

func TestInvalidEvidence(t *testing.T) {
	r := newRec(t, processrecovery.LiveObservation{}, nil)
	_, err := r.Apply(processrecovery.PersistedEvidence{})
	if !errors.Is(err, processrecovery.ErrInvalidEvidence) {
		t.Fatalf("err=%v", err)
	}
}
