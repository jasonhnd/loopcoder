package routepin_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/routepin"
)

func t0() time.Time { return time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC) }

func TestPersistAckLaunchMatch(t *testing.T) {
	s := routepin.NewStore(t0, func(p, m string) bool { return p == "grok" && m == "grok-4.5" })
	pin, err := s.Persist("proj", "att1", routepin.Fields{
		Provider: "Grok", Model: "grok-4.5", Effort: "high", Permission: "default",
		SubagentPolicy: routepin.SubagentForbidden,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pin.ReadyForLaunch() {
		t.Fatal("must ack first")
	}
	pin, err = s.Acknowledge(pin.PinID)
	if err != nil || !pin.ReadyForLaunch() {
		t.Fatal(err)
	}
	// exact actual
	ev, err := s.VerifyActual("att1", routepin.Fields{
		Provider: "grok", Model: "grok-4.5", Effort: "high", Permission: "default",
		SubagentPolicy: routepin.SubagentForbidden,
	})
	if err != nil || !ev.Match || ev.PinDigest != pin.Digest {
		t.Fatalf("%+v err=%v", ev, err)
	}
	// mismatch gpt substitution blocked
	_, err = s.VerifyActual("att1", routepin.Fields{
		Provider: "codex", Model: "gpt", Effort: "high", Permission: "default",
		SubagentPolicy: routepin.SubagentForbidden,
	})
	if !errors.Is(err, routepin.ErrMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestUnavailableNoFallback(t *testing.T) {
	s := routepin.NewStore(t0, func(p, m string) bool { return false })
	_, err := s.Persist("p", "a", routepin.Fields{Provider: "grok", Model: "x"})
	if !errors.Is(err, routepin.ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestImmutableAndSuccessor(t *testing.T) {
	s := routepin.NewStore(t0, func(string, string) bool { return true })
	p1, err := s.Persist("p", "a1", routepin.Fields{Provider: "grok", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Acknowledge(p1.PinID)
	if err := s.MutateActive("a1", routepin.Fields{Provider: "codex", Model: "m2"}); !errors.Is(err, routepin.ErrImmutable) {
		t.Fatalf("err=%v", err)
	}
	// second persist same attempt rejected
	_, err = s.Persist("p", "a1", routepin.Fields{Provider: "codex", Model: "m2"})
	if !errors.Is(err, routepin.ErrImmutable) {
		t.Fatalf("err=%v", err)
	}
	p2, err := s.Successor(p1.PinID, "a2", routepin.Fields{Provider: "codex", Model: "m2"})
	if err != nil {
		t.Fatal(err)
	}
	if p2.SuccessorOf != p1.PinID || p2.AttemptID != "a2" {
		t.Fatalf("%+v", p2)
	}
	// old no longer active
	if _, err := s.GetActive("a1"); !errors.Is(err, routepin.ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	act, _ := s.GetActive("a2")
	if act.PinID != p2.PinID {
		t.Fatal("new active")
	}
}

func TestStatusNoAuth(t *testing.T) {
	s := routepin.NewStore(t0, func(string, string) bool { return true })
	p, _ := s.Persist("p", "a", routepin.Fields{Provider: "grok", Model: "m", Effort: "low"})
	p, _ = s.Acknowledge(p.PinID)
	actual := routepin.Fields{Provider: "grok", Model: "m", Effort: "low"}
	v := routepin.Status(p, &actual)
	if v.Digest != p.Digest || !v.Ready || v.Requested == "" || v.Actual == "" {
		t.Fatalf("%+v", v)
	}
}

func TestExecRouteDigestAligns(t *testing.T) {
	f := routepin.Fields{Provider: "fixture", Model: "m0", Effort: "low", Permission: "default", SubagentPolicy: routepin.SubagentForbidden}
	n, err := f.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	r := n.ToExecRoute()
	// providerexec digest path must be usable
	if r.Provider != "fixture" || r.Model != "m0" {
		t.Fatalf("%+v", r)
	}
	if n.Digest() == "" {
		t.Fatal("empty digest")
	}
}
