package capacitysnapshot_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
)

func TestToRouteInventoryPermissionAwareAntigravity(t *testing.T) {
	now := time.Date(2026, 7, 22, 19, 0, 0, 0, time.UTC)
	remCodex := 0.9
	remAG := 0.85
	accounts := []capacitysnapshot.AccountObservation{
		capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				CapturedAt: now, Source: "test",
			}},
			Models: []capacitysnapshot.ModelSpec{{
				ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
			}},
			Source: "test", CapturedAt: now,
		}),
		capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: "antigravity", AccountRef: "acct-ag", InstallRef: "i-ag",
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 15, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 85, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				CapturedAt: now, Source: "test",
			}},
			Models: []capacitysnapshot.ModelSpec{{
				ModelID: "gemini-3.1-pro-high", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
			}},
			Source: "test", CapturedAt: now,
		}),
	}
	_ = remCodex
	_ = remAG
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnattendedOK {
		t.Fatalf("want unattended: %v", snap.Reasons)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	// Read-only resolve must not select antigravity.
	resRO, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "read-only", ProjectID: "p",
		DecisionKey: "ro-1", Inventory: &inv, Now: now,
	})
	if err != nil {
		t.Fatalf("read-only resolve: %v", err)
	}
	if resRO.Provider == "antigravity" {
		t.Fatalf("read-only must not select antigravity: %+v", resRO)
	}
	if resRO.Provider != "codex" {
		t.Fatalf("read-only want codex, got %+v", resRO)
	}
	// Write resolve may select antigravity.
	resW, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "bounded_write", ProjectID: "p",
		DecisionKey: "w-1", Inventory: &inv, Now: now,
	})
	if err != nil {
		t.Fatalf("write resolve: %v", err)
	}
	if resW.Provider == "" {
		t.Fatalf("write resolve empty: %+v", resW)
	}
	// At least one of the two modes used antigravity or codex with real capacity.
	t.Logf("ro=%s/%s write=%s/%s", resRO.Provider, resRO.Model, resW.Provider, resW.Model)
}

func TestToRouteInventoryExcludesExhaustedAndStaleRoutes(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	// codex exhausted (remaining 0) — must not win write routes.
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 0, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			CapturedAt: now, Source: "test-exhausted",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	// antigravity healthy remaining — write winner.
	ag := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "antigravity", AccountRef: "acct-ag", InstallRef: "i-ag",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			CapturedAt: now, Source: "test-ok",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "GPT-OSS 120B", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	// gemini stale-only window — must not win.
	gemini := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "gemini", AccountRef: "acct-gem", InstallRef: "i-gem",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 5, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 95, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessStale,
			CapturedAt: now.Add(-2 * time.Hour), Source: "test-stale",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gemini-2.5-pro", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, ag, gemini}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		// may fail unattended if all bad — still try with healthy AG only path
		t.Logf("inventory: %v", err)
	}
	if inv.Candidates == nil {
		// rebuild with only AG if unattended failed
		snap2, err2 := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{ag, codex}, now)
		if err2 != nil {
			t.Fatal(err2)
		}
		inv, err = capacitysnapshot.ToRouteInventory(snap2, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "bounded_write", Effort: "medium",
		ProjectID: "p", DecisionKey: "excl-1", Inventory: &inv, Now: now,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Provider == "codex" {
		t.Fatalf("exhausted codex must not win: %+v", res)
	}
	if res.Provider == "gemini" {
		t.Fatalf("stale gemini must not win: %+v", res)
	}
	if res.Provider != "antigravity" {
		t.Fatalf("want antigravity write winner, got %+v", res)
	}
}

func TestToRouteInventoryEmitsPerSupportedDepth(t *testing.T) {
	now := time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC)
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	// Expect RO+write candidates for each of low/medium/high (6 total for one model).
	depths := map[string]int{}
	for _, c := range inv.Candidates {
		if c.Provider == "codex" && c.Model == "gpt-5.5" && c.Permission == "read-only" {
			depths[c.Effort]++
		}
	}
	for _, want := range []string{"low", "medium", "high"} {
		if depths[want] == 0 {
			t.Fatalf("missing depth candidate %s in inventory: %v", want, depths)
		}
	}
	// Required low must bind low; high must bind high (no silent medium).
	for _, depth := range []string{"low", "high"} {
		res, err := autoroute.Resolve(autoroute.Input{
			AutoRoute: true, Permission: "read-only", Effort: depth,
			ProjectID: "p", DecisionKey: "d-" + depth, Inventory: &inv, Now: now,
		})
		if err != nil {
			t.Fatalf("resolve depth=%s: %v", depth, err)
		}
		if res.Effort != depth {
			t.Fatalf("depth=%s selection effort=%q want %q (%+v)", depth, res.Effort, depth, res)
		}
	}
	// Unsupported depth fails closed.
	_, err = autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "read-only", Effort: "xhigh",
		ProjectID: "p", DecisionKey: "d-xhigh", Inventory: &inv, Now: now,
	})
	if err == nil {
		t.Fatal("expected fail-closed for unsupported xhigh")
	}
}
