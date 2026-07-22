package rehydrate_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/rehydrate"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC)
}

func TestClassifyMerged(t *testing.T) {
	st := rehydrate.Classify(rehydrate.FixtureTerminalMerged())
	if st != rehydrate.StateMerged {
		t.Fatalf("got %s", st)
	}
	if !st.Terminal() {
		t.Fatal("merged should be terminal")
	}
}

func TestClassifyInFlight(t *testing.T) {
	st := rehydrate.Classify(rehydrate.FixtureInFlight())
	if st != rehydrate.StateInFlight {
		t.Fatalf("got %s", st)
	}
	if st.Terminal() {
		t.Fatal("in_flight must not be terminal")
	}
}

func TestClassifyAmbiguous(t *testing.T) {
	st := rehydrate.Classify(rehydrate.FixtureAmbiguous())
	if st != rehydrate.StateAmbiguous {
		t.Fatalf("got %s", st)
	}
}

func TestValidateEvidenceFailClosed(t *testing.T) {
	ev := rehydrate.FixtureTerminalMerged()
	ev.Repo.Visibility = "unknown"
	if err := rehydrate.ValidateEvidence(ev); err == nil {
		t.Fatal("unknown visibility must fail")
	}
	ev = rehydrate.FixtureTerminalMerged()
	ev.Repo.Owner = ""
	if err := rehydrate.ValidateEvidence(ev); err == nil {
		t.Fatal("missing owner must fail")
	}
}

func TestFreshHomeReconstructsFromGitHubOnly(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	ev := rehydrate.FixtureTerminalMerged()
	r := s.Rehydrate(rehydrate.Input{
		Evidence:  ev,
		Checkout:  rehydrate.LocalCheckout{Path: "/Users/b/app", HeadSHA: ev.PR.MergeSHA, Branch: "pre-prod"},
		MachineID: "mac-b",
	})
	if !r.Allowed {
		t.Fatalf("denied: %v", r.Reasons)
	}
	if r.Project.RepoKey != "acme/app" {
		t.Fatalf("repo_key=%s", r.Project.RepoKey)
	}
	if r.Event.MergeSHA != ev.PR.MergeSHA {
		t.Fatalf("merge sha not linked")
	}
	if len(r.Event.RouteEvidenceRefs) != 2 {
		t.Fatalf("route refs=%v", r.Event.RouteEvidenceRefs)
	}
	if r.NewLocalExecutionID == "" {
		t.Fatal("missing local execution id")
	}
}

func TestRejectForeignAttempt(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	r := s.Rehydrate(rehydrate.Input{
		Evidence:            rehydrate.FixtureTerminalMerged(),
		Checkout:            rehydrate.LocalCheckout{Path: "/Users/b/app"},
		MachineID:           "mac-b",
		AttemptAdoptForeign: true,
		ForeignAttemptID:    "foreign-1",
	})
	if r.Allowed {
		t.Fatal("must reject")
	}
	if r.Event == nil || !r.Event.ForeignAttemptRejected {
		t.Fatalf("event=%#v", r.Event)
	}
}

func TestRejectInFlightRemote(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	r := s.Rehydrate(rehydrate.Input{
		Evidence:  rehydrate.FixtureInFlight(),
		Checkout:  rehydrate.LocalCheckout{Path: "/Users/b/app"},
		MachineID: "mac-b",
	})
	if r.Allowed {
		t.Fatal("in-flight must not be allowed")
	}
	if !strings.Contains(strings.Join(r.Reasons, " "), "in-flight") {
		t.Fatalf("reasons=%v", r.Reasons)
	}
}

func TestAmbiguousRequiresReconciliation(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	r := s.Rehydrate(rehydrate.Input{
		Evidence:  rehydrate.FixtureAmbiguous(),
		Checkout:  rehydrate.LocalCheckout{Path: "/Users/b/app"},
		MachineID: "mac-b",
	})
	if r.Allowed {
		t.Fatal("ambiguous must not be allowed")
	}
}

func TestIdempotentRehydrate(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	in := rehydrate.Input{
		Evidence:  rehydrate.FixtureTerminalMerged(),
		Checkout:  rehydrate.LocalCheckout{Path: "/Users/b/app", Branch: "pre-prod"},
		MachineID: "mac-b",
	}
	r1 := s.Rehydrate(in)
	r2 := s.Rehydrate(in)
	if !r1.Allowed || !r2.Allowed {
		t.Fatalf("r1=%v r2=%v", r1.Reasons, r2.Reasons)
	}
	if s.ProjectCount() != 1 {
		t.Fatalf("projects=%d", s.ProjectCount())
	}
	if r2.Event == nil || !r2.Event.IdempotentReplay {
		t.Fatalf("second should be idempotent: %#v", r2.Event)
	}
}

func TestSameNameDifferentOwnerIsolated(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	a := rehydrate.FixtureTerminalMerged()
	b := rehydrate.FixtureTerminalMerged()
	b.Repo.Owner = "other"
	b.Issue.Number = 1
	b.PR.Number = 1
	ra := s.Rehydrate(rehydrate.Input{Evidence: a, Checkout: rehydrate.LocalCheckout{Path: "/a"}, MachineID: "mac-b"})
	rb := s.Rehydrate(rehydrate.Input{Evidence: b, Checkout: rehydrate.LocalCheckout{Path: "/b"}, MachineID: "mac-b"})
	if !ra.Allowed || !rb.Allowed {
		t.Fatalf("ra=%v rb=%v", ra.Reasons, rb.Reasons)
	}
	if ra.Project.ProjectID == rb.Project.ProjectID {
		t.Fatal("project ids must differ")
	}
}

func TestVisibilityChangeFailClosed(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	ev := rehydrate.FixtureTerminalMerged()
	_ = s.Rehydrate(rehydrate.Input{Evidence: ev, Checkout: rehydrate.LocalCheckout{Path: "/a"}, MachineID: "mac-b"})
	ev.Repo.Visibility = "public"
	r := s.Rehydrate(rehydrate.Input{Evidence: ev, Checkout: rehydrate.LocalCheckout{Path: "/a"}, MachineID: "mac-b"})
	if r.Allowed {
		t.Fatal("visibility flip must fail closed")
	}
}

func TestDivergenceReported(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	ev := rehydrate.FixtureTerminalMerged()
	r := s.Rehydrate(rehydrate.Input{
		Evidence: ev,
		Checkout: rehydrate.LocalCheckout{
			Path: "/Users/b/app",
			// Different from both head and merge.
			HeadSHA: "cccccccccccccccccccccccccccccccccccccccc",
			Branch:  "feature-x",
		},
		MachineID: "mac-b",
	})
	if !r.Allowed {
		t.Fatalf("divergence should still allow terminal rehydrate with report: %v", r.Reasons)
	}
	if len(r.Divergences) == 0 {
		t.Fatal("expected divergence report")
	}
}

func TestRequiresExplicitCheckout(t *testing.T) {
	s := rehydrate.NewStore("mac-b", fixedNow)
	r := s.Rehydrate(rehydrate.Input{
		Evidence:  rehydrate.FixtureTerminalMerged(),
		MachineID: "mac-b",
	})
	if r.Allowed {
		t.Fatal("missing checkout must fail")
	}
}

func TestHandoffCanary(t *testing.T) {
	if err := rehydrate.RunHandoffCanary(); err != nil {
		t.Fatal(err)
	}
}

func TestTwoMachinesNoSharedState(t *testing.T) {
	ev := rehydrate.FixtureTerminalMerged()
	a := rehydrate.NewStore("mac-a", fixedNow)
	b := rehydrate.NewStore("mac-b", fixedNow)
	ra := a.Rehydrate(rehydrate.Input{Evidence: ev, Checkout: rehydrate.LocalCheckout{Path: "/a"}, MachineID: "mac-a"})
	rb := b.Rehydrate(rehydrate.Input{Evidence: ev, Checkout: rehydrate.LocalCheckout{Path: "/b"}, MachineID: "mac-b"})
	if !ra.Allowed || !rb.Allowed {
		t.Fatalf("ra=%v rb=%v", ra.Reasons, rb.Reasons)
	}
	// Distinct local execution identities; no copied process identity.
	if ra.NewLocalExecutionID == rb.NewLocalExecutionID {
		t.Fatal("execution ids collided across machines")
	}
	if ra.Project.MachineID == rb.Project.MachineID {
		t.Fatal("machine ids should differ")
	}
	// Mac B events do not appear on Mac A.
	if len(b.Events()) == 0 || len(a.Events()) == 0 {
		t.Fatal("each machine should have its own events")
	}
}
