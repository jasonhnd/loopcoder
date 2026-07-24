package capacitysnapshot_test

import (
	"strings"
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
		TaskClass: "luna", DecisionKey: "ro-1", Inventory: &inv, Now: now,
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
	// Write resolve must select account-affirmable provider (codex), not antigravity.
	resW, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "bounded_write", ProjectID: "p",
		TaskClass: "tera", DecisionKey: "w-1", Inventory: &inv, Now: now,
	})
	if err != nil {
		t.Fatalf("write resolve: %v", err)
	}
	if resW.Provider == "antigravity" {
		t.Fatalf("write must not select antigravity (no account affirm): %+v", resW)
	}
	if resW.Provider != "codex" {
		t.Fatalf("write want codex, got %+v", resW)
	}
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
		TaskClass: "tera", ProjectID: "p", DecisionKey: "excl-1", Inventory: &inv, Now: now,
	})
	// Exhausted codex + stale gemini + AG (no account affirm) → no eligible route.
	if err == nil && res.Outcome == autoroute.OutcomeSelected {
		if res.Provider == "codex" {
			t.Fatalf("exhausted codex must not win: %+v", res)
		}
		if res.Provider == "gemini" {
			t.Fatalf("stale gemini must not win: %+v", res)
		}
		if res.Provider == "antigravity" {
			t.Fatalf("antigravity must not win (no account affirm): %+v", res)
		}
	}
	// Prefer fail-closed no_route over selecting non-affirmable AG.
	if err != nil && !strings.Contains(err.Error(), "no route") {
		t.Fatalf("resolve: %v", err)
	}
	if res.Provider == "antigravity" {
		t.Fatalf("antigravity must not be hard-eligible write winner: %+v", res)
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
			TaskClass: "luna", ProjectID: "p", DecisionKey: "d-" + depth, Inventory: &inv, Now: now,
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
		TaskClass: "luna", ProjectID: "p", DecisionKey: "d-xhigh", Inventory: &inv, Now: now,
	})
	if err == nil {
		t.Fatal("expected fail-closed for unsupported xhigh")
	}
}

func TestToRouteInventorySoftBindsHighestRemainingWindow(t *testing.T) {
	// Regression: Antigravity multi-window (primary≈98%, secondary/3p≈11%).
	// First-window binding soft-excluded the whole provider under Luna reserve.
	now := time.Date(2026, 7, 23, 4, 0, 0, 0, time.UTC)
	pct := func(rem float64) capacitysnapshot.Window {
		return capacitysnapshot.Window{
			Kind: "provider-defined", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100 - rem, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: rem, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			CapturedAt: now, Source: "test",
		}
	}
	// Scarce secondary listed first — must not bind soft remaining to 11%.
	ag := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "antigravity", AccountRef: "acct-ag", InstallRef: "i-ag",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{
			pct(11), // secondary / 3p — scarce
			pct(98), // primary — abundant
		},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "Gemini 3.1 Pro", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{pct(95)},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{ag, codex}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	var agSoftRem float64 = -1
	for _, s := range inv.Soft {
		if s.Provider != "antigravity" {
			continue
		}
		if len(s.Windows) == 0 || s.Windows[0].RemainingFraction == nil {
			t.Fatalf("antigravity soft missing remaining: %+v", s)
		}
		agSoftRem = *s.Windows[0].RemainingFraction
		break
	}
	if agSoftRem < 0.9 {
		t.Fatalf("soft remaining bound to scarce window: rem=%v want ~0.98", agSoftRem)
	}
	// Soft bind still uses highest remaining for AG display/ranking.
	// Runtime hard-eligibility: AG cannot affirm AccountRef → not hard-eligible
	// for capacity-bound product routes (codex should win write instead).
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "bounded_write", Effort: "medium",
		TaskClass: "tera", ProjectID: "p", DecisionKey: "mp-soft-1", Inventory: &inv, Now: now,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Decision == nil {
		t.Fatal("nil decision")
	}
	for _, cv := range res.Decision.Candidates {
		if cv.Provider != "antigravity" {
			continue
		}
		if cv.HardEligible {
			t.Fatalf("antigravity must not be hard-eligible (no account affirm): %+v", cv)
		}
		break
	}
	if res.Outcome == autoroute.OutcomeSelected && res.Provider == "antigravity" {
		t.Fatalf("antigravity must not win capacity-bound write route: %+v", res)
	}
	if res.Outcome == autoroute.OutcomeSelected && res.Provider != "codex" {
		t.Fatalf("want codex write winner (account-affirmable), got %+v", res)
	}
}
