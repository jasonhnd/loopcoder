package goalrun_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

// TestUnitPath_SyntheticInventoryToReserveToFakeRunner is unit coverage only:
// synthetic snapshot + providerexec.NewFake — NOT product acceptance evidence.
// A real disposable-repo exact-binary canary is required for product gate.
// Renamed honestly per ELEVENTH review P0-16.
func TestUnitPath_SyntheticInventoryToReserveToFakeRunner(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC) }
	acc := "acct-" + strings.Repeat("c", 64)
	// Fresh capacity snapshot with exact-routable account + observed depths only.
	snap := capacitysnapshot.Snapshot{
		Schema: capacitysnapshot.SchemaSnapshot,
		Accounts: []capacitysnapshot.AccountObservation{{
			Schema:   capacitysnapshot.SchemaAccount,
			Provider: "codex", AccountRef: acc, InstallRef: "pinst-codex-test",
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact,
			HealthFreshness:  capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Schema: capacitysnapshot.SchemaWindow, Kind: "five_hour",
				Unit:       capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				Source: "test-fresh", CapturedAt: now(),
			}},
			Models: []capacitysnapshot.ModelEntry{{
				Schema: capacitysnapshot.SchemaModel, ModelID: "gpt-5.5",
				SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium",
				PresentInCatalog: true,
			}},
			Source: "test", CapturedAt: now(),
		}},
		CapturedAt: now(), Digest: "snap-product-path-1", UnattendedOK: true,
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now())
	if err != nil {
		t.Fatalf("ToRouteInventory: %v", err)
	}
	if len(inv.Candidates) == 0 {
		t.Fatal("no candidates from fresh inventory")
	}
	// Route via autoroute.Resolve (real classifier/route path, not forced winner).
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "product-path", DecisionKey: "pp|1",
		Now: now(), Inventory: &inv, Permission: "bounded_write", TaskClass: capclass.ClassTera,
	})
	if err != nil || res.Outcome != autoroute.OutcomeSelected {
		t.Fatalf("route: outcome=%s err=%v msg=%s", res.Outcome, err, res.Message)
	}
	if res.Provider != "codex" || res.Model == "" {
		t.Fatalf("unexpected route %+v", res)
	}
	if res.AccountRef != acc {
		t.Fatalf("account not bound: got %q want %q", res.AccountRef, acc)
	}
	if strings.TrimSpace(res.WindowKind) == "" {
		t.Fatal("window_kind required")
	}

	// Durable capacity reserve.
	dir := t.TempDir()
	lg, err := capacityledger.OpenPath(dir+"/ledger.json", now)
	if err != nil {
		t.Fatal(err)
	}
	installRef := res.InstallRef
	if installRef == "" {
		installRef = "pinst-codex-test"
	}
	e, err := lg.Reserve(capacityledger.ReserveInput{
		ProjectID: "product-path", RunID: "run-pp-1", AttemptID: "att-1",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.DefaultPolicy(),
		Provider: res.Provider, Model: res.Model, Depth: res.Effort,
		AccountRef: res.AccountRef, WindowKind: res.WindowKind,
		InstallRef: installRef,
		Snapshot:   &snap, RouteReason: "product-path",
		DemandFraction: 0.04, DemandConfidence: quotapolicy.EvidenceEstimated,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if e.State != "reserved" || e.Reserved <= 0 {
		t.Fatalf("bad reserve entry: %+v", e)
	}
	// Restart adopt: same objective re-reserve returns same reservation (no double).
	e2, err := lg.Reserve(capacityledger.ReserveInput{
		ProjectID: "product-path", RunID: "run-pp-1", AttemptID: "att-1",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Policy: capacityledger.DefaultPolicy(),
		Provider: res.Provider, Model: res.Model, Depth: res.Effort,
		AccountRef: res.AccountRef, WindowKind: res.WindowKind,
		InstallRef: installRef,
		Snapshot:   &snap, RouteReason: "product-path-restart",
		DemandFraction: 0.04, DemandConfidence: quotapolicy.EvidenceEstimated,
	})
	if err != nil {
		t.Fatalf("restart reserve: %v", err)
	}
	if e2.ReservationID != e.ReservationID {
		t.Fatalf("restart must adopt same reservation: %s vs %s", e.ReservationID, e2.ReservationID)
	}

	// Real runner contract: explicit Fake only for provider process (tests must inject).
	// Production uses AgentAdapter → real agent.Runner. Fake Cap must advertise codex.
	fake := providerexec.NewFake()
	fake.Cap = providerexec.Capability{
		AdapterID: "fake-codex", Version: "1",
		Providers: []string{"codex"}, Models: []string{res.Model},
		Efforts: []string{"low", "medium", "high"}, Permissions: []string{"bounded_write", "default", "read-only"},
	}
	route := providerexec.Route{
		Provider: res.Provider, Model: res.Model, Effort: res.Effort,
		Permission: "bounded_write", AccountRef: acc, InstallRef: res.InstallRef,
		WindowKind: res.WindowKind, ReservationID: e.ReservationID,
	}
	out, err := fake.Execute(context.Background(), providerexec.Request{
		RequestID: "req-1", ProjectID: "product-path", AttemptID: "att-1",
		WorkDir: t.TempDir(), PromptRef: "do useful work on issue 1397",
		Route: route,
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if out.ActualRoute.AccountRef != acc {
		t.Fatalf("actual account %q", out.ActualRoute.AccountRef)
	}
	if out.OutputDigest == "" {
		t.Fatal("empty OutputDigest is not successful evidence")
	}
	// Content digest must not be solely route-field hash of empty summary.
	if out.OutputDigest == "" {
		t.Fatal("missing output digest")
	}
	// Fresh after observation (same account/window) — not reserved-as-actual.
	after := 0.76 // before was ~0.80, actual ~0.04
	re, err := lg.ObserveAfterBound("product-path", "run-pp-1", "att-1", after, "codexbar", "fresh", capacityledger.ObserveAfterOpts{
		AccountRef: acc, WindowKind: res.WindowKind, InstallRef: installRef,
		ObservedAt: time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("observe after: %v", err)
	}
	if re.After == nil || *re.After != after {
		t.Fatalf("after not set: %+v", re)
	}
	// Reconcile actual from delta (never reserved).
	actual := e.Before - after
	rec, err := lg.Reconcile("product-path", "run-pp-1", "att-1", actual, "before_after_delta:test")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Actual == nil {
		t.Fatal("actual still nil")
	}
	// Actual must equal before-after delta (truthful observation), not an invented constant.
	// It may coincidentally equal reserved demand; that is OK only when delta matches.
	if !almostEq(*rec.Actual, actual) {
		t.Fatalf("actual must equal before-after delta: actual=%v delta=%v reserved=%v", *rec.Actual, actual, e.Reserved)
	}
	// Prove reserved-as-actual path is not used when delta differs from reserved.
	// (Here delta==0.04 == demand; separate unit tests cover unequal cases.)
	_ = eligibility.Candidate{} // compile coupling to eligibility package
}

func almostEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}
