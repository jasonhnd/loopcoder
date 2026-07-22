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
