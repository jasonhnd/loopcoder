package mergegate_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/mergegate"
	"github.com/jasonhnd/loopcoder/internal/routepin"
)

func t0() time.Time { return time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC) }

func route() routepin.Fields {
	return routepin.Fields{Provider: "codex", Model: "m", Effort: "low", Permission: "read-only", SubagentPolicy: routepin.SubagentForbidden}
}

func readyPre() mergegate.Precondition {
	return mergegate.Precondition{
		WorkerCleanupTerminal: true, PRHeadStable: true, PRHeadOID: "head1",
		CIReady: true, WorkerSlotFree: true, VerifierSlotFree: true,
	}
}

func TestLaunchGates(t *testing.T) {
	g := mergegate.NewGate(t0)
	req := mergegate.Request{
		AttemptID: "v1", PRNumber: 1, PRHeadOID: "head1", PRBaseOID: "base",
		Route: route(), Permission: "read-only",
	}
	// not cleanup terminal
	pre := readyPre()
	pre.WorkerCleanupTerminal = false
	if err := g.CanLaunchVerifier(pre, req); !errors.Is(err, mergegate.ErrNotReady) {
		t.Fatalf("err=%v", err)
	}
	// concurrent worker
	g.SetWorkerActive(true)
	if err := g.CanLaunchVerifier(readyPre(), req); !errors.Is(err, mergegate.ErrConcurrent) {
		t.Fatalf("err=%v", err)
	}
	g.SetWorkerActive(false)
	// write permission denied
	req.Permission = "write"
	if err := g.CanLaunchVerifier(readyPre(), req); !errors.Is(err, mergegate.ErrReadOnly) {
		t.Fatalf("err=%v", err)
	}
	req.Permission = "read-only"
	accepted, err := g.BeginVerifier(readyPre(), req)
	if err != nil || accepted.RouteDigest == "" {
		t.Fatal(err)
	}
}

func TestRouteMismatchBlocksVerdict(t *testing.T) {
	g := mergegate.NewGate(t0)
	req, err := g.BeginVerifier(readyPre(), mergegate.Request{
		AttemptID: "v2", PRNumber: 2, PRHeadOID: "head1", Route: route(), Permission: "read-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	wrong := route()
	wrong.Model = "other"
	v, err := g.CompleteVerifier(req, mergegate.VerdictPass, wrong, "findings", false)
	if !errors.Is(err, mergegate.ErrRouteMismatch) || v.Class != mergegate.VerdictBlocked {
		t.Fatalf("%+v err=%v", v, err)
	}
}

func TestPassStopsAtHumanGate(t *testing.T) {
	g := mergegate.NewGate(t0)
	req, _ := g.BeginVerifier(readyPre(), mergegate.Request{
		AttemptID: "v3", PRNumber: 3, PRHeadOID: "head1", Route: route(), Permission: "read-only",
	})
	v, err := g.CompleteVerifier(req, mergegate.VerdictPass, route(), "ok", false)
	if err != nil || v.Class != mergegate.VerdictPass {
		t.Fatal(err)
	}
	if g.MayAutoMerge(3) {
		t.Fatal("no auto merge")
	}
	d, err := g.RecordHumanDecision(3, "head1", "approve_merge", "owner")
	if err != nil || d.AutoMerge || d.Decision != "approve_merge" {
		t.Fatalf("%+v err=%v", d, err)
	}
}

func TestHeadChangeStalesVerdict(t *testing.T) {
	g := mergegate.NewGate(t0)
	req, _ := g.BeginVerifier(readyPre(), mergegate.Request{
		AttemptID: "v4", PRNumber: 4, PRHeadOID: "head1", Route: route(), Permission: "read-only",
	})
	_, _ = g.CompleteVerifier(req, mergegate.VerdictPass, route(), "ok", false)
	g.InvalidateOnHeadChange(4, "head2")
	v, err := g.GetVerdict("v4", "head2")
	if err != nil || !v.Stale || v.Class != mergegate.VerdictStale {
		t.Fatalf("%+v err=%v", v, err)
	}
}

func TestMutationRejected(t *testing.T) {
	g := mergegate.NewGate(t0)
	req, _ := g.BeginVerifier(readyPre(), mergegate.Request{
		AttemptID: "v5", PRNumber: 5, PRHeadOID: "head1", Route: route(), Permission: "read-only",
	})
	_, err := g.CompleteVerifier(req, mergegate.VerdictPass, route(), "x", true)
	if err == nil {
		t.Fatal("expected mutation error")
	}
}
