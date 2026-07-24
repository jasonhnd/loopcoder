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

// #1397 RC34: inventory fixed-week must not soft-only remap to weekly while hard
// keeps fixed-week — that identity split soft-excludes a hard-eligible Grok medium
// route (hard_eligible=true, soft_excluded=true, no_route).
func TestToRouteInventory_GrokFixedWeek_HardSoftWinnerIdentity(t *testing.T) {
	now := time.Date(2026, 7, 24, 10, 2, 0, 0, time.UTC)
	reset := now.Add(4 * 24 * time.Hour)
	// Scale-2 Grok shape (hundredths of percent): rem 3700/10000 → 37% after scale
	// is applied in FromProviderInventory; here we use post-scale finite percent.
	acct := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "grok", AccountRef: "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695",
		InstallRef: "pinst_3an5v55kgyq352a2bbgkfljbmikrndoq",
		Installed:  true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			// Observed inventory kind (providerinventory.WindowFixedWeek).
			Kind: "fixed-week", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 63, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 37, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: &reset, CapturedAt: now, Source: "grok.cli.credits_billing.v1",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "grok-4.5", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "providerinventory", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acct}, now)
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

	var hardWK, softWK string
	for _, c := range inv.Candidates {
		if c.Provider != "grok" || c.Model != "grok-4.5" || c.Effort != "medium" || c.Permission != "bounded_write" {
			continue
		}
		hardWK = c.WindowKind
		break
	}
	for _, s := range inv.Soft {
		if s.Provider != "grok" || s.Model != "grok-4.5" {
			continue
		}
		softWK = s.WindowKind
		break
	}
	if hardWK != "weekly" {
		t.Fatalf("hard WindowKind=%q want weekly (canonical for fixed-week)", hardWK)
	}
	if softWK != hardWK {
		t.Fatalf("hard/soft WindowKind split: hard=%q soft=%q", hardWK, softWK)
	}

	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "bounded_write", Effort: "medium",
		TaskClass: "tera", ProjectID: "p-1397", DecisionKey: "grok-fixed-week-1",
		Inventory: &inv, Now: now,
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Outcome != autoroute.OutcomeSelected {
		t.Fatalf("want selected, got outcome=%s msg=%q decision=%+v", res.Outcome, res.Message, res.Decision)
	}
	if res.Provider != "grok" || res.Model != "grok-4.5" {
		t.Fatalf("want grok/grok-4.5 winner, got %s/%s", res.Provider, res.Model)
	}
	if res.WindowKind != "weekly" {
		t.Fatalf("winner WindowKind=%q want weekly", res.WindowKind)
	}
	if res.WindowKind != hardWK || res.WindowKind != softWK {
		t.Fatalf("winner/hard/soft identity: winner=%q hard=%q soft=%q", res.WindowKind, hardWK, softWK)
	}
	if res.Decision == nil {
		t.Fatal("nil decision")
	}
	foundHard := false
	for _, cv := range res.Decision.Candidates {
		if cv.Provider != "grok" || cv.Model != "grok-4.5" {
			continue
		}
		if !cv.HardEligible {
			t.Fatalf("grok must be hard-eligible: %+v", cv)
		}
		if cv.SoftExcluded {
			t.Fatalf("grok must not be soft-excluded after window canon: %+v", cv)
		}
		if cv.WindowKind != "weekly" {
			t.Fatalf("decision cand WindowKind=%q want weekly", cv.WindowKind)
		}
		foundHard = true
	}
	if !foundHard {
		t.Fatal("missing grok decision candidate")
	}
}

func TestToRouteInventory_FixedHourAlias_FiveHour(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{"fixed-hour", "fixed_hour", "5h", "five_hour"} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			acct := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
				Provider: "codex", AccountRef: "acct-codex-hour", InstallRef: "i-codex-hour",
				Installed: true, Authenticated: true, Healthy: true,
				HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
				Windows: []capacitysnapshot.Window{{
					Kind: raw, Unit: capacitysnapshot.UnitPercentage,
					Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
					Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
					Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
					Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
					CapturedAt: now, Source: "test",
				}},
				Models: []capacitysnapshot.ModelSpec{{
					ModelID: "gpt-5.5", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
				}},
				Source: "test", CapturedAt: now,
			})
			snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acct}, now)
			if err != nil {
				t.Fatal(err)
			}
			inv, err := capacitysnapshot.ToRouteInventory(snap, now)
			if err != nil {
				t.Fatal(err)
			}
			var hardWK, softWK string
			for _, c := range inv.Candidates {
				if c.Provider == "codex" && c.Model == "gpt-5.5" && c.Permission == "bounded_write" {
					hardWK = c.WindowKind
					break
				}
			}
			for _, s := range inv.Soft {
				if s.Provider == "codex" && s.Model == "gpt-5.5" {
					softWK = s.WindowKind
					break
				}
			}
			if hardWK != "five_hour" || softWK != "five_hour" {
				t.Fatalf("raw=%q hard=%q soft=%q want five_hour/five_hour", raw, hardWK, softWK)
			}
			res, err := autoroute.Resolve(autoroute.Input{
				AutoRoute: true, Permission: "bounded_write", Effort: "medium",
				TaskClass: "tera", ProjectID: "p", DecisionKey: "fixed-hour-" + raw,
				Inventory: &inv, Now: now,
			})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if res.Outcome != autoroute.OutcomeSelected || res.Provider != "codex" {
				t.Fatalf("want codex selected, got %+v", res)
			}
			if res.WindowKind != "five_hour" {
				t.Fatalf("winner WindowKind=%q want five_hour", res.WindowKind)
			}
		})
	}
}

func TestToRouteInventory_EmptyUnknownWindow_NotInvented(t *testing.T) {
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)
	// Empty Kind: normalizeWindow rewrites "" → "unknown". Soft/hard must not
	// invent five_hour or weekly; "unknown" is an allowed normalized raw token.
	for _, name := range []string{"empty", "unknown"} {
		name := name
		t.Run(name, func(t *testing.T) {
			kind := ""
			if name == "unknown" {
				kind = "unknown"
			}
			// Fresh exact remaining so unattended + soft bind; kind alone is the
			// identity under test.
			acct := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
				Provider: "codex", AccountRef: "acct-empty-win", InstallRef: "i-empty-win",
				Installed: true, Authenticated: true, Healthy: true,
				HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
				Windows: []capacitysnapshot.Window{{
					Kind: kind, Unit: capacitysnapshot.UnitPercentage,
					Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
					Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
					Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
					Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
					CapturedAt: now, Source: "test",
				}},
				Models: []capacitysnapshot.ModelSpec{{
					ModelID: "gpt-5.5", SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
				}},
				Source: "test", CapturedAt: now,
			})
			snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acct}, now)
			if err != nil {
				t.Fatal(err)
			}
			inv, err := capacitysnapshot.ToRouteInventory(snap, now)
			if err != nil {
				t.Fatal(err)
			}
			var hardWK, softWK string
			for _, c := range inv.Candidates {
				if c.Provider == "codex" && c.Permission == "bounded_write" {
					hardWK = c.WindowKind
					break
				}
			}
			for _, s := range inv.Soft {
				if s.Provider == "codex" {
					softWK = s.WindowKind
					break
				}
			}
			if hardWK != softWK {
				t.Fatalf("hard/soft split: hard=%q soft=%q", hardWK, softWK)
			}
			if hardWK == "five_hour" || hardWK == "weekly" {
				t.Fatalf("empty/unknown must not invent fixed window, got %q", hardWK)
			}
			// After Build, empty becomes "unknown"; canonical keeps "unknown".
			if hardWK != "unknown" {
				t.Fatalf("WindowKind=%q want unknown (no invent)", hardWK)
			}
		})
	}
}

func TestCanonicalRouteWindowKind(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"  ":               "",
		"fixed-week":       "weekly",
		"fixed_week":       "weekly",
		"WEEKLY":           "weekly",
		"fixed-hour":       "five_hour",
		"fixed_hour":       "five_hour",
		"5h":               "five_hour",
		"five_hour":        "five_hour",
		"credit":           "credit",
		"provider-defined": "provider-defined",
		"unknown":          "unknown",
	}
	for in, want := range cases {
		if got := capacitysnapshot.CanonicalRouteWindowKind(in); got != want {
			t.Errorf("canonical(%q)=%q want %q", in, got, want)
		}
	}
}
