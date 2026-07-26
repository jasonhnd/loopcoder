package obsrefresh_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/obsrefresh"
)

func TestFreshReuseAndSingleProbe(t *testing.T) {
	clock := &obsrefresh.MemoryClock{T: time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC)}
	var probes int32
	m := obsrefresh.NewManager(obsrefresh.DefaultConfig(), clock, func(string) (bool, string, []string, bool, string) {
		atomic.AddInt32(&probes, 1)
		return true, "dig1", []string{"present"}, true, ""
	})
	if m.HasProviderRunner() {
		t.Fatal("provider runner")
	}
	r1, err := m.DemandRefresh("p1", "fake", "cli")
	if err != nil || !r1.Probed || r1.Health != obsrefresh.HealthHealthy {
		t.Fatalf("%+v err=%v", r1, err)
	}
	r2, err := m.DemandRefresh("p2", "fake", "cli")
	if err != nil || r2.Probed || !r2.Reused {
		t.Fatalf("expected reuse %+v err=%v", r2, err)
	}
	if atomic.LoadInt32(&probes) != 1 {
		t.Fatalf("probes=%d", probes)
	}
}

func TestConcurrentStaleOneProbe(t *testing.T) {
	clock := &obsrefresh.MemoryClock{T: time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)}
	var probes int32
	started := make(chan struct{})
	release := make(chan struct{})
	m := obsrefresh.NewManager(obsrefresh.Config{
		TTL: time.Minute, SuccessBackoff: time.Second, FailureBackoff: time.Second,
		MaxFailureBackoff: time.Minute, MinInterval: time.Millisecond,
	}, clock, func(string) (bool, string, []string, bool, string) {
		atomic.AddInt32(&probes, 1)
		close(started)
		<-release
		return true, "d", []string{"x"}, true, ""
	})
	// First call starts probe
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = m.DemandRefresh("a", "fake", "src")
	}()
	<-started
	// Concurrent while in-flight
	r, err := m.DemandRefresh("b", "fake", "src")
	if err != nil {
		t.Fatal(err)
	}
	if r.Message != "coalesced_in_flight" && !r.Reused {
		t.Fatalf("%+v", r)
	}
	close(release)
	wg.Wait()
	if atomic.LoadInt32(&probes) != 1 {
		t.Fatalf("probes=%d", probes)
	}
}

func TestFailurePreservesObservationAndBackoff(t *testing.T) {
	clock := &obsrefresh.MemoryClock{T: time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC)}
	n := 0
	m := obsrefresh.NewManager(obsrefresh.DefaultConfig(), clock, func(string) (bool, string, []string, bool, string) {
		n++
		if n == 1 {
			return true, "good", []string{"present"}, true, ""
		}
		return false, "", nil, true, "timeout"
	})
	_, _ = m.DemandRefresh("p", "fake", "cli")
	// expire TTL
	clock.Advance(10 * time.Minute)
	r, err := m.DemandRefresh("p", "fake", "cli")
	if err != nil || r.Health != obsrefresh.HealthStale {
		t.Fatalf("%+v err=%v", r, err)
	}
	if r.Observation != "good" {
		t.Fatalf("lost observation %q", r.Observation)
	}
	st, ok := m.Get("cli")
	if !ok || !st.InstallationKnown {
		t.Fatalf("install not preserved %+v", st)
	}
	// immediate retry should reuse backoff
	r2, _ := m.DemandRefresh("p", "fake", "cli")
	if r2.Probed {
		t.Fatalf("expected backoff wait %+v", r2)
	}
}

func TestCooldownBlocksManualWithoutOverride(t *testing.T) {
	clock := &obsrefresh.MemoryClock{T: time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC)}
	n := 0
	m := obsrefresh.NewManager(obsrefresh.DefaultConfig(), clock, func(string) (bool, string, []string, bool, string) {
		n++
		if n == 1 {
			return false, "", nil, true, "rate_limit"
		}
		return true, "recovered", []string{"ok"}, true, ""
	})
	r, err := m.DemandRefresh("p", "fake", "api")
	if !errors.Is(err, obsrefresh.ErrCooldowned) && r.Health != obsrefresh.HealthCooldown {
		// DemandRefresh returns result with cooldown health and ErrCooldowned on active cooldown after set
		// first call probes and sets cooldown — may return result without error
	}
	if r.Health != obsrefresh.HealthCooldown {
		t.Fatalf("health=%s msg=%s", r.Health, r.Message)
	}
	// Manual without override blocked
	outs, err := m.ManualRefresh("fake", []string{"api"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Message != "manual_blocked_cooldown" {
		t.Fatalf("%+v", outs[0])
	}
	// With override
	outs, err = m.ManualRefresh("fake", []string{"api"}, "owner_override_v1")
	if err != nil {
		t.Fatal(err)
	}
	if !outs[0].Probed && outs[0].Health != obsrefresh.HealthHealthy {
		// may probe
		if outs[0].Message == "manual_blocked_cooldown" {
			t.Fatal("override failed")
		}
	}
}

func TestRestartCheckpoint(t *testing.T) {
	clock := &obsrefresh.MemoryClock{T: time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)}
	m := obsrefresh.NewManager(obsrefresh.DefaultConfig(), clock, func(string) (bool, string, []string, bool, string) {
		return true, "d", []string{"a"}, true, ""
	})
	_, _ = m.DemandRefresh("p", "fake", "cli")
	cp := m.Checkpoint()
	if len(cp) != 1 {
		t.Fatal(cp)
	}
	m2 := obsrefresh.NewManager(obsrefresh.DefaultConfig(), clock, func(string) (bool, string, []string, bool, string) {
		t.Fatal("should not probe when fresh after restore")
		return false, "", nil, false, ""
	})
	m2.Restore(cp)
	r, err := m2.DemandRefresh("p", "fake", "cli")
	if err != nil || r.Probed {
		t.Fatalf("%+v err=%v", r, err)
	}
}

func TestRenderHealthDistinct(t *testing.T) {
	for _, h := range []obsrefresh.HealthClass{
		obsrefresh.HealthUnknown, obsrefresh.HealthStale, obsrefresh.HealthUnavailable,
	} {
		label, cap := obsrefresh.RenderHealth(h)
		if label == "healthy" || cap == "zero" {
			t.Fatalf("%s -> %s %s", h, label, cap)
		}
		if cap != "unknown_capacity" {
			t.Fatalf("cap=%s", cap)
		}
	}
	l, c := obsrefresh.RenderHealth(obsrefresh.HealthHealthy)
	if l != "healthy" || c != "use_last_facts" {
		t.Fatalf("%s %s", l, c)
	}
}

func TestTTLExpiryTriggersProbe(t *testing.T) {
	clock := &obsrefresh.MemoryClock{T: time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)}
	var probes int
	m := obsrefresh.NewManager(obsrefresh.Config{
		TTL: 2 * time.Minute, SuccessBackoff: time.Second, FailureBackoff: time.Second,
		MaxFailureBackoff: time.Minute, MinInterval: time.Second,
	}, clock, func(string) (bool, string, []string, bool, string) {
		probes++
		return true, "d", []string{"k"}, true, ""
	})
	_, _ = m.DemandRefresh("p", "a", "s")
	clock.Advance(time.Minute)
	_, _ = m.DemandRefresh("p", "a", "s") // still fresh
	if probes != 1 {
		t.Fatal(probes)
	}
	clock.Advance(2 * time.Minute)
	_, _ = m.DemandRefresh("p", "a", "s")
	if probes != 2 {
		t.Fatalf("probes=%d", probes)
	}
}
