package capacityledger_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

func t0() time.Time { return time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC) }

func testSnap() capacitysnapshot.Snapshot {
	reset := t0().Add(30 * time.Minute)
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i1",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact,
		HealthFreshness:  capacitysnapshot.FreshnessFresh,
		Source:           "fixture", CapturedAt: t0(),
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			ResetAt:    &reset,
			Confidence: capacitysnapshot.ConfidenceEstimated,
			Freshness:  capacitysnapshot.FreshnessFresh,
			Source:     "fixture", CapturedAt: t0(),
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium",
		}},
	})
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, t0())
	if err != nil {
		panic(err)
	}
	return s
}

func TestReserveReconcileIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capacity-ledger.json")
	now := t0()
	l, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnap()
	e1, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "run1", AttemptID: "att1",
		Policy:   capacityledger.PolicyUseBeforeReset,
		Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		Snapshot: &snap, RouteReason: "use-before-reset winner",
		DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceEstimated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e1.State != "reserved" || e1.Reserved <= 0 || e1.Before <= 0 {
		t.Fatalf("%+v", e1)
	}
	// Idempotent restart
	e2, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "run1", AttemptID: "att1",
		Policy:   capacityledger.PolicyUseBeforeReset,
		Provider: "codex", Model: "gpt-5.5", Snapshot: &snap,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e2.ReservationID != e1.ReservationID || e2.Reserved != e1.Reserved {
		t.Fatalf("double reserve: %+v vs %+v", e1, e2)
	}
	// Reconcile
	e3, err := l.Reconcile("p", "run1", "att1", 0.03, "local_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if e3.State != "reconciled" || e3.Actual == nil || e3.After == nil {
		t.Fatalf("%+v", e3)
	}
	// Second reconcile is no-op
	e4, err := l.Reconcile("p", "run1", "att1", 0.99, "local_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if *e4.Actual != *e3.Actual {
		t.Fatalf("reconcile not idempotent: %v vs %v", *e4.Actual, *e3.Actual)
	}
	rep := e3.HumanReport()
	if rep == "" || containsSecret(rep) {
		t.Fatalf("report=%q", rep)
	}
}

func TestObserveAfterWithoutInventingActual(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capacity-ledger.json")
	l, err := capacityledger.OpenPath(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnap()
	_, err = l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "run-obs", AttemptID: "att-obs",
		Policy: capacityledger.PolicyBalanced, Provider: "codex", Model: "gpt-5.5",
		Snapshot: &snap, DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceExact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Release("p", "run-obs", "att-obs", "executed_usage_unknown"); err != nil {
		t.Fatal(err)
	}
	e, err := l.ObserveAfter("p", "run-obs", "att-obs", 0.88, "codexbar", "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if e.Actual != nil {
		t.Fatalf("must not invent actual: %+v", e)
	}
	if e.After == nil || *e.After != 0.88 {
		t.Fatalf("after=%v want 0.88", e.After)
	}
	if e.Freshness != "fresh" {
		t.Fatalf("freshness=%q", e.Freshness)
	}
	if !strings.Contains(e.RouteReason, "after_source=codexbar") {
		t.Fatalf("route reason missing source: %s", e.RouteReason)
	}
}

func TestReleaseOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capacity-ledger.json")
	l, err := capacityledger.OpenPath(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnap()
	_, err = l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "run2", AttemptID: "att2",
		Policy:   capacityledger.PolicyBalanced,
		Provider: "codex", Model: "gpt-5.5", Snapshot: &snap,
		DemandFraction: 0.04, DemandConfidence: quotapolicy.EvidenceExact,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Release("p", "run2", "att2", "execution_failed")
	if err != nil {
		t.Fatal(err)
	}
	if e.State != "released" {
		t.Fatalf("%+v", e)
	}
	// reopen durable — release must persist
	l2, err := capacityledger.OpenPath(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := l2.Get("p", "run2", "att2")
	if !ok || got.State != "released" {
		t.Fatalf("durable state=%v ok=%v", got, ok)
	}
}

func TestUnknownQuotaRefusesWithoutFabrication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capacity-ledger.json")
	l, err := capacityledger.OpenPath(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "grok", AccountRef: "acct-g", Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Models: []capacitysnapshot.ModelSpec{{ModelID: "grok-4.5", Present: true, SupportedDepths: []string{"medium"}}},
		// no usable windows
	})
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, t0())
	if err != nil {
		t.Fatal(err)
	}
	// Build may be unattended-false; still call Reserve with snapshot
	_, err = l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "a",
		Provider: "grok", Model: "grok-4.5", Snapshot: &s,
	})
	if err == nil {
		t.Fatal("expected refuse without usable window")
	}
}

func TestDefaultPolicyIsUseBeforeReset(t *testing.T) {
	if capacityledger.DefaultPolicy() != capacityledger.PolicyUseBeforeReset {
		t.Fatal(capacityledger.DefaultPolicy())
	}
	cfg := capacityledger.ModeConfig(capacityledger.PolicyUseBeforeReset)
	if cfg.BurnBoost < 1.5 {
		t.Fatalf("use-before-reset should boost burn: %+v", cfg)
	}
}

func containsSecret(s string) bool {
	for _, n := range []string{"sk-", "api_key", "password="} {
		if len(s) > 0 && (contains(s, n)) {
			return true
		}
	}
	return false
}
func contains(s, n string) bool {
	return len(s) >= len(n) && (s == n || len(n) == 0 || (len(s) > 0 && indexOf(s, n) >= 0))
}
func indexOf(s, n string) int {
	for i := 0; i+len(n) <= len(s); i++ {
		if s[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
