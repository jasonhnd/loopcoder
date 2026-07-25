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
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-test",
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
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.PolicyUseBeforeReset,
		Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex", WindowKind: "five_hour",
		InstallRef: "i-test",
		Snapshot:   &snap, RouteReason: "use-before-reset winner",
		DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceEstimated,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e1.State != "reserved" || e1.Reserved <= 0 || e1.Before <= 0 {
		t.Fatalf("%+v", e1)
	}
	// Idempotent restart — full identity required.
	e2, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "run1", AttemptID: "att1",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.PolicyUseBeforeReset,
		Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: e1.AccountRef, WindowKind: e1.WindowKind,
		InstallRef: "i-test",
		Snapshot:   &snap,
	})
	if err != nil {
		t.Fatal(err)
	}
	if e2.ReservationID != e1.ReservationID || e2.Reserved != e1.Reserved {
		t.Fatalf("double reserve: %+v vs %+v", e1, e2)
	}
	// Reconcile persists ActualSource.
	e3, err := l.Reconcile("p", "run1", "att1", 0.03, "local_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if e3.State != "reconciled" || e3.Actual == nil || e3.After == nil {
		t.Fatalf("%+v", e3)
	}
	if e3.ActualSource != "local_tokens" {
		t.Fatalf("ActualSource=%q", e3.ActualSource)
	}
	// Same actual+source is idempotent.
	e4, err := l.Reconcile("p", "run1", "att1", 0.03, "local_tokens")
	if err != nil {
		t.Fatal(err)
	}
	if *e4.Actual != *e3.Actual || e4.ActualSource != e3.ActualSource {
		t.Fatalf("reconcile not idempotent: %+v vs %+v", e3, e4)
	}
	// Different actual or source conflicts.
	if _, err := l.Reconcile("p", "run1", "att1", 0.99, "local_tokens"); err == nil {
		t.Fatal("want conflict on different actual")
	} else if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if _, err := l.Reconcile("p", "run1", "att1", 0.03, "other_source"); err == nil {
		t.Fatal("want conflict on different source")
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
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.PolicyBalanced, Provider: "codex", AccountRef: "acct-codex", WindowKind: "five_hour", Model: "gpt-5.5",
		InstallRef: "i-test",
		Snapshot:   &snap, DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceExact,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Release("p", "run-obs", "att-obs", "executed_usage_unknown"); err != nil {
		t.Fatal(err)
	}
	// after must be <= before unless reset evidence is supplied
	e, err := l.ObserveAfter("p", "run-obs", "att-obs", 0.75, "codexbar", "fresh", t0())
	if err != nil {
		t.Fatal(err)
	}
	if e.Actual != nil {
		t.Fatalf("must not invent actual: %+v", e)
	}
	if e.After == nil || *e.After != 0.75 {
		t.Fatalf("after=%v want 0.75", e.After)
	}
	if e.AfterState != capacityledger.AfterStateObserved {
		t.Fatalf("AfterState=%q want observed", e.AfterState)
	}
	if e.AfterSource != "codexbar" || e.AfterFreshness != "fresh" {
		t.Fatalf("after meta source=%q fresh=%q", e.AfterSource, e.AfterFreshness)
	}
	if e.AfterObservedAt == nil || e.AfterObservedAt.IsZero() {
		t.Fatal("AfterObservedAt required for observed after")
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
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.PolicyBalanced,
		Provider: "codex", AccountRef: "acct-codex", WindowKind: "five_hour", Model: "gpt-5.5", Snapshot: &snap,
		InstallRef:     "i-test",
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
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "grok", AccountRef: "acct-g", WindowKind: "five_hour", Model: "grok-4.5", Snapshot: &s,
		InstallRef: "i-test",
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

func TestReserveIdempotency_ReservedOK_ReleasedRefusesRelaunch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capacity-ledger.json")
	l, err := capacityledger.OpenPath(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnap()
	e1, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-idem",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex", WindowKind: "five_hour", Snapshot: &snap,
		InstallRef: "i-test",
	})
	if err != nil || e1.State != "reserved" {
		t.Fatalf("%+v %v", e1, err)
	}
	// Same reserved attempt is idempotent — full identity required.
	e2, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-idem",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: e1.AccountRef, WindowKind: e1.WindowKind,
		InstallRef: "i-test",
		Snapshot:   &snap,
	})
	if err != nil || e2.ReservationID != e1.ReservationID || e2.State != "reserved" {
		t.Fatalf("idempotent reserved: %+v %v", e2, err)
	}
	if _, err := l.Release("p", "r", "att-idem", "done"); err != nil {
		t.Fatal(err)
	}
	// Released key must fail closed on relaunch.
	if _, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-idem",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", AccountRef: "acct-codex", WindowKind: "five_hour", Model: "gpt-5.5", Snapshot: &snap,
		InstallRef: "i-test",
	}); err == nil {
		t.Fatal("want refuse relaunch after released")
	}
	// Reconciled similarly.
	e3, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-recon",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", AccountRef: "acct-codex", WindowKind: "five_hour", Model: "gpt-5.5", Snapshot: &snap,
		InstallRef: "i-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reconcile("p", "r", "att-recon", 0.02, "src"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-recon",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", AccountRef: "acct-codex", WindowKind: "five_hour", Model: "gpt-5.5", Snapshot: &snap,
		InstallRef: "i-test",
	}); err == nil {
		t.Fatal("want refuse relaunch after reconciled")
	}
	_ = e3
}

// Reopen-from-disk: Actual + ActualSource survive for prior and alternate
// exact attempt IDs; ReleaseReason survives with Actual nil / source empty.
func TestReopenFromDisk_ActualSourceAndReleaseReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capacity-ledger.json")
	now := t0()
	l, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnap()
	priorAtt := "att-only-deadbeef-g0"
	altAtt := "att-only-deadbeef-g1"
	if _, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: priorAtt,
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex", WindowKind: "five_hour", Snapshot: &snap,
		InstallRef: "i-test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Release("p", "r", priorAtt, "model_unavailable_supersede"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: altAtt,
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "acct-codex", WindowKind: "five_hour", Snapshot: &snap,
		InstallRef: "i-test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Reconcile("p", "r", altAtt, 0.04, "provider_usage"); err != nil {
		t.Fatal(err)
	}

	// Reopen process-equivalent.
	l2, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	prior, ok := l2.Get("p", "r", priorAtt)
	if !ok {
		t.Fatal("missing prior")
	}
	if prior.State != "released" {
		t.Fatalf("prior state=%s", prior.State)
	}
	if prior.Actual != nil {
		t.Fatalf("released prior Actual must stay nil: %+v", prior)
	}
	if prior.ActualSource != "" {
		t.Fatalf("released ActualSource empty, got %q", prior.ActualSource)
	}
	if prior.ReleaseReason != "model_unavailable_supersede" {
		t.Fatalf("ReleaseReason=%q", prior.ReleaseReason)
	}
	alt, ok := l2.Get("p", "r", altAtt)
	if !ok {
		t.Fatal("missing alternate")
	}
	if alt.State != "reconciled" || alt.Actual == nil || *alt.Actual != 0.04 {
		t.Fatalf("alt: %+v", alt)
	}
	if alt.ActualSource != "provider_usage" {
		t.Fatalf("alt ActualSource=%q", alt.ActualSource)
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

func TestObserveAfterRejectsRiseWithoutReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap.json")
	l, err := capacityledger.OpenPath(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnap() // before remaining ~0.80
	e, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "a1",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", AccountRef: "acct-codex", WindowKind: "five_hour", Model: "gpt-5.5", Snapshot: &snap,
		InstallRef:     "i-test",
		DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceExact,
	})
	if err != nil {
		t.Fatal(err)
	}
	// after 0.98 > before without reset → fail closed
	_, err = l.ObserveAfterBound("p", "r", "a1", 0.98, "cli", "fresh", capacityledger.ObserveAfterOpts{
		AccountRef: e.AccountRef, WindowKind: e.WindowKind,
		InstallRef: "i-test", ObservedAt: t0(),
	})
	if err == nil {
		t.Fatal("want reject rise without reset")
	}
	// with reset evidence OK
	e2, err := l.ObserveAfterBound("p", "r", "a1", 0.98, "cli", "fresh", capacityledger.ObserveAfterOpts{
		AccountRef: e.AccountRef, WindowKind: e.WindowKind,
		InstallRef:    "i-test", ObservedAt: t0(),
		ResetObserved: true, ResetEvidence: "window_reset_observed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if e2.After == nil || *e2.After != 0.98 {
		t.Fatalf("%+v", e2)
	}
	if e2.AfterState != capacityledger.AfterStateObserved {
		t.Fatalf("AfterState=%q", e2.AfterState)
	}
}

func TestObserveAfterRejectsWindowMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cap2.json")
	l, err := capacityledger.OpenPath(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	snap := testSnap()
	e, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "a2",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", AccountRef: "acct-codex", WindowKind: "five_hour", Model: "gpt-5.5", Snapshot: &snap,
		InstallRef:     "i-test",
		DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceExact,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.ObserveAfterBound("p", "r", "a2", 0.75, "cli", "fresh", capacityledger.ObserveAfterOpts{
		AccountRef: e.AccountRef, WindowKind: "daily", // wrong window
		InstallRef: "i-test", ObservedAt: t0(),
	})
	if err == nil {
		t.Fatal("want window mismatch")
	}
}

func TestReservePrefersHighestRemainingMultiWindow(t *testing.T) {
	// Scarce secondary resets sooner; primary abundant. Reserve must bind ~0.98 not 0.11.
	now := time.Date(2026, 7, 23, 5, 0, 0, 0, time.UTC)
	soon := now.Add(30 * time.Minute)
	later := now.Add(4 * time.Hour)
	pct := func(rem float64, reset time.Time) capacitysnapshot.Window {
		return capacitysnapshot.Window{
			Kind: "provider-defined", Unit: capacitysnapshot.UnitPercentage,
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: rem, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: &reset, CapturedAt: now, Source: "test",
		}
	}
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "antigravity", AccountRef: "acct-ag", InstallRef: "i-test",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{
			pct(11, soon),
			pct(98, later),
		},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "GPT-OSS 120B", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, now)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "capacity-ledger.json")
	l, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	e, err := l.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "run-ag", AttemptID: "att1",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.PolicyUseBeforeReset,
		Provider: "antigravity", Model: "GPT-OSS 120B", Depth: "medium",
		AccountRef: "acct-ag", WindowKind: "provider-defined",
		InstallRef: "i-test",
		Snapshot:   &snap, RouteReason: "multi-window",
		DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceEstimated,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if e.State != "reserved" {
		t.Fatalf("state=%s reason=%s", e.State, e.RouteReason)
	}
	if e.Before < 0.9 {
		t.Fatalf("bound scarce window: before=%v want >=0.9", e.Before)
	}
}
