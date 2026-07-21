package statusproj_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/evidencecollect"
	"github.com/jasonhnd/loopcoder/internal/statusproj"
)

func t0() time.Time { return time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC) }

func TestRebuildDigestMatches(t *testing.T) {
	s := statusproj.NewStream("proj", "att-1")
	at := t0()
	events := []evidencecollect.Observation{
		evidencecollect.ProcessState("att-1", "alive", false, at),
		evidencecollect.OutputMovement("att-1", 10, at.Add(time.Second)),
		evidencecollect.GitHubCheckChanged("att-1", "verify", "success", at.Add(2*time.Second)),
	}
	store := evidencecollect.NewStore(t0)
	for _, o := range events {
		ev, ok, err := store.Accept(o)
		if err != nil || !ok {
			t.Fatal(err)
		}
		if _, err := s.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	reb, err := s.Rebuild()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Digest != reb.Digest {
		t.Fatalf("digest mismatch %s vs %s", snap.Digest, reb.Digest)
	}
	if snap.Liveness != "alive" || !snap.ConcreteProgress {
		t.Fatalf("%#v", snap)
	}
	if snap.DeliveryGate != "success" {
		t.Fatalf("gate=%s", snap.DeliveryGate)
	}
}

func TestFollowCursorOrderAndResume(t *testing.T) {
	s := statusproj.NewStream("proj", "att-1")
	store := evidencecollect.NewStore(t0)
	at := t0()
	var cursors []statusproj.Cursor
	for i, o := range []evidencecollect.Observation{
		evidencecollect.ProcessState("att-1", "alive", false, at),
		evidencecollect.OutputMovement("att-1", 1, at.Add(time.Second)),
		evidencecollect.OperatorAction("att-1", "ack", at.Add(2*time.Second)),
	} {
		ev, ok, err := store.Accept(o)
		if err != nil || !ok {
			t.Fatal(err)
		}
		c, err := s.Append(ev)
		if err != nil {
			t.Fatal(err)
		}
		cursors = append(cursors, c)
		_ = i
	}
	// After 0: all three
	all := s.Follow(0)
	if len(all) != 3 {
		t.Fatalf("len=%d", len(all))
	}
	// After first: two remaining
	rest := s.Follow(cursors[0])
	if len(rest) != 2 {
		t.Fatalf("resume len=%d", len(rest))
	}
	// After end: empty
	if len(s.Follow(s.Cursor())) != 0 {
		t.Fatal("expected empty")
	}
}

func TestProviderProseDoesNotSetLifecycle(t *testing.T) {
	s := statusproj.NewStream("proj", "att-1")
	store := evidencecollect.NewStore(t0)
	ev, ok, err := store.Accept(evidencecollect.ProviderProse("att-1", "done!", t0()))
	if err != nil || !ok {
		t.Fatal(err)
	}
	_, _ = s.Append(ev)
	snap, _ := s.Snapshot()
	if snap.Liveness != "not_started" || snap.Stage != "not_started" {
		t.Fatalf("provider must not set lifecycle: %#v", snap)
	}
}

func TestUnknownNotSuccessByOmission(t *testing.T) {
	s := statusproj.NewStream("proj", "att-1")
	snap, _ := s.Snapshot()
	if snap.ResourceState != "unknown" || !snap.ResourceUnknown {
		t.Fatalf("%#v", snap)
	}
	if snap.DeliveryGate == "success" {
		t.Fatal("must not default delivery to success")
	}
}

func TestHeartbeatDistinctFromProgress(t *testing.T) {
	s := statusproj.NewStream("proj", "att-1")
	store := evidencecollect.NewStore(t0)
	ev, _, _ := store.Accept(evidencecollect.ProcessState("att-1", "alive", false, t0()))
	_, _ = s.Append(ev)
	snap, _ := s.Snapshot()
	if !snap.Heartbeat || snap.ConcreteProgress {
		t.Fatalf("%#v", snap)
	}
}
