package evidencecollect_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/evidencecollect"
)

func t0() time.Time { return time.Date(2026, 7, 22, 2, 0, 0, 0, time.UTC) }

func TestAcceptRequiredFieldsAndProgressFlags(t *testing.T) {
	s := evidencecollect.NewStore(t0)
	ev, ok, err := s.Accept(evidencecollect.ProcessState("att-1", "alive", false, t0()))
	if err != nil || !ok {
		t.Fatalf("%v ok=%v", err, ok)
	}
	if ev.Digest == "" || ev.Source == "" || ev.Subject == "" || ev.ObservedAt.IsZero() {
		t.Fatalf("%#v", ev)
	}
	if ev.IsProgress || !ev.IsHeartbeat {
		t.Fatalf("heartbeat flags %#v", ev)
	}
	// Progress output
	ev2, ok, err := s.Accept(evidencecollect.OutputMovement("att-1", 12, t0().Add(time.Second)))
	if err != nil || !ok || !ev2.IsProgress || ev2.IsHeartbeat {
		t.Fatalf("%#v ok=%v err=%v", ev2, ok, err)
	}
}

func TestHeartbeatDedupNoUnboundedGrowth(t *testing.T) {
	s := evidencecollect.NewStore(t0)
	o := evidencecollect.ProcessState("att-1", "alive", false, t0())
	if _, ok, err := s.Accept(o); err != nil || !ok {
		t.Fatal(err)
	}
	if _, ok, err := s.Accept(o); err != nil || ok {
		t.Fatalf("expected dedup ok=%v err=%v", ok, err)
	}
	if len(s.Events()) != 1 {
		t.Fatalf("len=%d", len(s.Events()))
	}
	// State transition still visible
	o2 := evidencecollect.ProcessState("att-1", "exited", true, t0().Add(time.Second))
	if _, ok, err := s.Accept(o2); err != nil || !ok {
		t.Fatal(err)
	}
	if len(s.Events()) != 2 {
		t.Fatalf("len=%d", len(s.Events()))
	}
}

func TestProviderProseCannotSetLifecycle(t *testing.T) {
	s := evidencecollect.NewStore(t0)
	o := evidencecollect.ProviderProse("att-1", "I finished the task", t0())
	o.LifecycleAuthority = true
	if _, _, err := s.Accept(o); !errors.Is(err, evidencecollect.ErrProviderLifecycle) {
		t.Fatalf("err=%v", err)
	}
	o2 := evidencecollect.ProviderProse("att-1", "still working", t0())
	o2.Fields = map[string]string{"terminal_state": "success"}
	if _, _, err := s.Accept(o2); !errors.Is(err, evidencecollect.ErrProviderLifecycle) {
		t.Fatalf("err=%v", err)
	}
	// Content-only allowed
	if _, ok, err := s.Accept(evidencecollect.ProviderProse("att-1", "hello world", t0())); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestDeterministicFixtureSequence(t *testing.T) {
	s := evidencecollect.NewStore(t0)
	at := t0()
	inputs := []evidencecollect.Observation{
		evidencecollect.ProcessState("att-1", "alive", false, at),
		evidencecollect.ResourceSample("att-1", 0.2, 1000, 3, false, at.Add(time.Second)),
		evidencecollect.OutputMovement("att-1", 40, at.Add(2*time.Second)),
		evidencecollect.GitCommitObserved("att-1", "abc123", at.Add(3*time.Second)),
		evidencecollect.GitHubCheckChanged("att-1", "verify", "success", at.Add(4*time.Second)),
		evidencecollect.OperatorAction("att-1", "ack_blocker", at.Add(5*time.Second)),
		// duplicate heartbeat
		evidencecollect.ProcessState("att-1", "alive", false, at.Add(6*time.Second)),
	}
	// Force same digest for last heartbeat by same fields (state alive)
	// ProcessState with same state yields same digest even with different ObservedAt
	// because DigestOf does not include ObservedAt — good for dedup.
	for _, o := range inputs {
		_, _, _ = s.Accept(o)
	}
	evs := s.Events()
	if len(evs) != 6 {
		t.Fatalf("want 6 events (dedup heartbeat), got %d", len(evs))
	}
	// Re-run same sequence into new store → same digests order
	s2 := evidencecollect.NewStore(t0)
	var digs []string
	for _, o := range inputs {
		if ev, ok, _ := s2.Accept(o); ok {
			digs = append(digs, ev.Digest)
		}
	}
	for i, dig := range digs {
		if dig != evs[i].Digest {
			t.Fatalf("digest[%d] %s vs %s", i, dig, evs[i].Digest)
		}
	}
}

func TestSecretPrivacyRejected(t *testing.T) {
	s := evidencecollect.NewStore(t0)
	o := evidencecollect.OutputMovement("att-1", 1, t0())
	o.Privacy = evidencecollect.PrivacySecret
	if _, _, err := s.Accept(o); !errors.Is(err, evidencecollect.ErrSecretNotPersistable) {
		t.Fatalf("err=%v", err)
	}
}

func TestRedactExcerpt(t *testing.T) {
	if evidencecollect.RedactExcerpt("token sk-abc", 100) != "[redacted]" {
		t.Fatal("expected redaction")
	}
}
