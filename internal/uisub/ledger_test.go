package uisub_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/uireport"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

func env(seq int64, project string) uireport.Envelope {
	e, err := uireport.Project(uireport.Input{
		Kind:       uireport.KindPeriodic,
		ProjectID:  project,
		AttemptID:  "att",
		Sequence:   seq,
		Stage:      "run",
		Status:     "running",
		Liveness:   "alive",
		RecordedAt: time.Date(2026, 7, 22, 11, 0, int(seq), 0, time.UTC),
	})
	if err != nil {
		panic(err)
	}
	return e
}

func TestReplayOrderAndAckMonotonic(t *testing.T) {
	l := uisub.NewLedger("proj_a", 8, nil)
	_ = l.RegisterClient(uisub.ClientIdentity{
		ClientID: "cli1", SessionID: "s1", AdapterVersion: "1", ProjectID: "proj_a", Required: true,
	})
	for i := int64(1); i <= 3; i++ {
		if err := l.Publish(env(i, "proj_a")); err != nil {
			t.Fatal(err)
		}
	}
	reps, err := l.Replay("cli1", 0)
	if err != nil || len(reps) != 3 {
		t.Fatalf("len=%d err=%v", len(reps), err)
	}
	// Advance stages
	for _, st := range []uisub.AckStage{uisub.StageAccepted, uisub.StageRendered} {
		if err := l.Acknowledge(uisub.Ack{
			ClientID: "cli1", EventID: reps[0].EventID, Digest: reps[0].ContentDigest, Stage: st,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Regressive rejected
	if err := l.Acknowledge(uisub.Ack{
		ClientID: "cli1", EventID: reps[0].EventID, Digest: reps[0].ContentDigest, Stage: uisub.StageStreamed,
	}); !errors.Is(err, uisub.ErrStaleCursor) {
		t.Fatalf("err=%v", err)
	}
	cur, err := l.LastAcceptedCursor("cli1")
	if err != nil || cur != 1 {
		t.Fatalf("cursor=%d err=%v", cur, err)
	}
	// Reconnect after cursor 1
	rest, err := l.Replay("cli1", cur)
	if err != nil || len(rest) != 2 {
		t.Fatalf("len=%d err=%v", len(rest), err)
	}
}

func TestWrongDigestAndCrossProject(t *testing.T) {
	l := uisub.NewLedger("proj_a", 8, nil)
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "cli1", SessionID: "s1", ProjectID: "proj_a"})
	e := env(1, "proj_a")
	_ = l.Publish(e)
	if err := l.Acknowledge(uisub.Ack{
		ClientID: "cli1", EventID: e.EventID, Digest: "wrong", Stage: uisub.StageAccepted,
	}); !errors.Is(err, uisub.ErrWrongDigest) {
		t.Fatalf("err=%v", err)
	}
	if err := l.RegisterClient(uisub.ClientIdentity{ClientID: "x", SessionID: "s", ProjectID: "other"}); !errors.Is(err, uisub.ErrWrongProject) {
		t.Fatalf("err=%v", err)
	}
	if err := l.Publish(env(2, "other")); !errors.Is(err, uisub.ErrWrongProject) {
		t.Fatalf("err=%v", err)
	}
}

func TestFastClientPagesWhileSlowOverflows(t *testing.T) {
	l := uisub.NewLedger("proj_a", 2, nil)
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "slow", SessionID: "s", ProjectID: "proj_a"})
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "fast", SessionID: "s2", ProjectID: "proj_a"})
	_ = l.Publish(env(1, "proj_a"))
	_ = l.Publish(env(2, "proj_a"))
	reps, err := l.Replay("fast", 0)
	if err != nil || len(reps) != 2 {
		t.Fatal(err)
	}
	for _, r := range reps {
		_ = l.Acknowledge(uisub.Ack{ClientID: "fast", EventID: r.EventID, Digest: r.ContentDigest, Stage: uisub.StageAccepted})
	}
	cur, _ := l.LastAcceptedCursor("fast")
	_ = l.Publish(env(3, "proj_a"))
	_ = l.Publish(env(4, "proj_a"))
	// fast: 2 unread ok
	reps, err = l.Replay("fast", cur)
	if err != nil || len(reps) != 2 {
		t.Fatalf("fast page len=%d err=%v", len(reps), err)
	}
	// slow never read: 4 unread > 2
	if _, err := l.Replay("slow", 0); !errors.Is(err, uisub.ErrQueueOverflow) {
		t.Fatalf("expected overflow, err=%v", err)
	}
}

func TestAckEvidenceFields(t *testing.T) {
	l := uisub.NewLedger("proj_a", 8, nil)
	_ = l.RegisterClient(uisub.ClientIdentity{
		ClientID: "cli1", SessionID: "sess", AdapterVersion: "v1", ProjectID: "proj_a",
	})
	e := env(1, "proj_a")
	_ = l.Publish(e)
	_ = l.Acknowledge(uisub.Ack{
		ClientID: "cli1", EventID: e.EventID, Digest: e.ContentDigest, Stage: uisub.StageAccepted,
	})
	a, ok := l.AckEvidence("cli1", e.EventID, uisub.StageAccepted)
	if !ok || a.SessionID != "sess" || a.Digest == "" || a.Sequence != 1 {
		t.Fatalf("%#v", a)
	}
}

func TestDuplicateAckIdempotent(t *testing.T) {
	l := uisub.NewLedger("proj_a", 8, nil)
	_ = l.RegisterClient(uisub.ClientIdentity{ClientID: "cli1", SessionID: "s", ProjectID: "proj_a"})
	e := env(1, "proj_a")
	_ = l.Publish(e)
	a := uisub.Ack{ClientID: "cli1", EventID: e.EventID, Digest: e.ContentDigest, Stage: uisub.StageAccepted}
	if err := l.Acknowledge(a); err != nil {
		t.Fatal(err)
	}
	if err := l.Acknowledge(a); err != nil {
		t.Fatal(err)
	}
}
