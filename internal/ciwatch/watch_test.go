package ciwatch_test

import (
	"context"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/ciwatch"
)

func TestPendingZeroProviderCallsAndReportDue(t *testing.T) {
	clock := time.Date(2026, 7, 23, 5, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	w := &ciwatch.Watcher{
		Store: ciwatch.NewStore(now), Now: now,
		MinInterval: 15 * time.Second, MaxInterval: time.Minute,
		ReportEvery: 5 * time.Minute,
	}
	st, err := w.Start(42, "head1", ciwatch.RequirementPolicy{
		RequiredChecks: []string{"verify", "test"}, RequiredApprovals: 0,
		OptionalEvidence: []string{"Greptile Review"},
	})
	if err != nil || st.ProviderCalls != 0 {
		t.Fatal(err)
	}
	// 30 minutes of pending observations
	for i := 0; i < 30; i++ {
		clock = clock.Add(time.Minute)
		ev, emitted, err := w.Observe(context.Background(), ciwatch.RemoteSnapshot{
			PRNumber: 42, HeadOID: "head1",
			Checks: []ciwatch.CheckState{
				{Name: "verify", Conclusion: "pending", Required: true},
				{Name: "test", Conclusion: "pending", Required: true},
			},
			ObservedAt: clock,
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if !emitted || ev.Class != ciwatch.ClassPending {
				t.Fatalf("first %+v emitted=%v", ev, emitted)
			}
		} else if emitted {
			t.Fatalf("duplicate flood at i=%d %+v", i, ev)
		}
		cp, _ := w.Checkpoint(42)
		if cp.ProviderCalls != 0 {
			t.Fatal("provider call")
		}
	}
	// report due after 5m from start — we advanced 30m
	if !w.DueReport(42) {
		t.Fatal("expected report due")
	}
	// restart from checkpoint
	cp, _ := w.Checkpoint(42)
	w2 := &ciwatch.Watcher{Store: ciwatch.NewStore(now), Now: now, ReportEvery: 5 * time.Minute}
	w2.Restore(cp)
	cp2, _ := w2.Checkpoint(42)
	if cp2.Class != ciwatch.ClassPending || cp2.EventsEmitted != 1 {
		t.Fatalf("%+v", cp2)
	}
}

func TestSuccessFailureMissingOptional(t *testing.T) {
	clock := time.Date(2026, 7, 23, 6, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	w := &ciwatch.Watcher{Store: ciwatch.NewStore(now), Now: now}
	_, _ = w.Start(1, "h", ciwatch.RequirementPolicy{
		RequiredChecks:   []string{"verify", "test"},
		OptionalEvidence: []string{"greptile"},
	})
	// missing greptile ok
	clock = clock.Add(time.Second)
	ev, ok, err := w.Observe(context.Background(), ciwatch.RemoteSnapshot{
		PRNumber: 1, HeadOID: "h",
		Checks: []ciwatch.CheckState{
			{Name: "verify", Conclusion: "success"},
			{Name: "test", Conclusion: "success"},
		},
	})
	if err != nil || !ok || ev.Class != ciwatch.ClassSuccess || !w.Ready(1) {
		t.Fatalf("%+v ok=%v err=%v", ev, ok, err)
	}
	// failure
	w3 := &ciwatch.Watcher{Store: ciwatch.NewStore(now), Now: now}
	_, _ = w3.Start(2, "h", ciwatch.RequirementPolicy{RequiredChecks: []string{"verify"}})
	clock = clock.Add(time.Second)
	ev, ok, _ = w3.Observe(context.Background(), ciwatch.RemoteSnapshot{
		PRNumber: 2, HeadOID: "h",
		Checks: []ciwatch.CheckState{{Name: "verify", Conclusion: "failure"}},
	})
	if !ok || ev.Class != ciwatch.ClassFailure {
		t.Fatalf("%+v", ev)
	}
	// missing required
	w4 := &ciwatch.Watcher{Store: ciwatch.NewStore(now), Now: now}
	_, _ = w4.Start(3, "h", ciwatch.RequirementPolicy{RequiredChecks: []string{"verify", "security"}})
	ev, ok, _ = w4.Observe(context.Background(), ciwatch.RemoteSnapshot{
		PRNumber: 3, HeadOID: "h",
		Checks: []ciwatch.CheckState{{Name: "verify", Conclusion: "success"}},
	})
	if !ok || ev.Class != ciwatch.ClassMissingRequired {
		t.Fatalf("%+v", ev)
	}
}

func TestHeadChangeInvalidates(t *testing.T) {
	clock := time.Date(2026, 7, 23, 7, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	w := &ciwatch.Watcher{Store: ciwatch.NewStore(now), Now: now}
	_, _ = w.Start(9, "h1", ciwatch.RequirementPolicy{RequiredChecks: []string{"verify"}})
	_, _, _ = w.Observe(context.Background(), ciwatch.RemoteSnapshot{
		PRNumber: 9, HeadOID: "h1",
		Checks: []ciwatch.CheckState{{Name: "verify", Conclusion: "success"}},
	})
	if !w.Ready(9) {
		t.Fatal("ready")
	}
	clock = clock.Add(time.Second)
	ev, ok, _ := w.Observe(context.Background(), ciwatch.RemoteSnapshot{
		PRNumber: 9, HeadOID: "h2",
		Checks: []ciwatch.CheckState{{Name: "verify", Conclusion: "success"}},
	})
	if !ok || ev.Class != ciwatch.ClassChangedHead || w.Ready(9) {
		t.Fatalf("%+v ready=%v", ev, w.Ready(9))
	}
}

func TestRateLimitBackoffNoBusyPoll(t *testing.T) {
	clock := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	now := func() time.Time { return clock }
	w := &ciwatch.Watcher{
		Store: ciwatch.NewStore(now), Now: now,
		MinInterval: 10 * time.Second, MaxInterval: time.Minute,
	}
	_, _ = w.Start(5, "h", ciwatch.RequirementPolicy{RequiredChecks: []string{"verify"}})
	ev, ok, _ := w.Observe(context.Background(), ciwatch.RemoteSnapshot{
		PRNumber: 5, HeadOID: "h", RateLimited: true,
		Checks: []ciwatch.CheckState{{Name: "verify", Conclusion: "pending"}},
	})
	if !ok || ev.Class != ciwatch.ClassRateLimited {
		t.Fatalf("%+v", ev)
	}
	wait := w.NextPollAfter(5)
	if wait < 10*time.Second {
		t.Fatalf("backoff too small %v", wait)
	}
	// immediate re-observe during backoff should not emit
	_, ok, _ = w.Observe(context.Background(), ciwatch.RemoteSnapshot{
		PRNumber: 5, HeadOID: "h", RateLimited: true,
		Checks: []ciwatch.CheckState{{Name: "verify", Conclusion: "pending"}},
	})
	if ok {
		t.Fatal("should not emit during backoff")
	}
}

func TestApprovalNeeded(t *testing.T) {
	clock := time.Now().UTC()
	now := func() time.Time { return clock }
	w := &ciwatch.Watcher{Store: ciwatch.NewStore(now), Now: now}
	_, _ = w.Start(7, "h", ciwatch.RequirementPolicy{
		RequiredChecks: []string{"verify"}, RequiredApprovals: 1,
	})
	ev, ok, _ := w.Observe(context.Background(), ciwatch.RemoteSnapshot{
		PRNumber: 7, HeadOID: "h", Approvals: 0,
		Checks: []ciwatch.CheckState{{Name: "verify", Conclusion: "success"}},
	})
	if !ok || ev.Class != ciwatch.ClassApprovalNeeded {
		t.Fatalf("%+v", ev)
	}
}

func TestNoProviderDependencyMarker(t *testing.T) {
	ciwatch.AssertNoProviderDependency()
}
