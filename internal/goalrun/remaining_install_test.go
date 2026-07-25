package goalrun

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
)

// Wrong install first must not satisfy remainingForProviderWindow for the reserved install.
func TestRemainingForProviderWindow_ExactInstallBinding(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	remWrong, remRight := 0.99, 0.40
	// Wrong install listed first with same provider/account/window.
	wrong := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "grok", AccountRef: "acct-same", InstallRef: "pinst_wrong_first",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "fixed_week", Unit: capacitysnapshot.UnitPercentage,
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: remWrong * 100, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			Source: "codexbar", CapturedAt: now,
		}},
	})
	right := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "grok", AccountRef: "acct-same", InstallRef: "pinst_right",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "fixed_week", Unit: capacitysnapshot.UnitPercentage,
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: remRight * 100, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			Source: "codexbar", CapturedAt: now,
		}},
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{wrong, right}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Empty install must fail closed.
	if _, _, _, _, _, _, ok := remainingForProviderWindow(&snap, "grok", "acct-same", "", "fixed_week"); ok {
		t.Fatal("empty install must not match")
	}
	// Wrong install first in snapshot: lookup for right install must return right remaining.
	rem, src, _, _, _, _, ok := remainingForProviderWindow(&snap, "grok", "acct-same", "pinst_right", "fixed_week")
	if !ok || rem == nil {
		t.Fatal("want right install match")
	}
	if *rem < 0.35 || *rem > 0.45 {
		t.Fatalf("remaining=%v want ~0.40 for right install (not wrong 0.99)", *rem)
	}
	if src != "codexbar" {
		t.Fatalf("source=%q", src)
	}
	// Explicit wrong install returns wrong remaining — caller must not use it for right reservation.
	rem2, _, _, _, _, _, ok2 := remainingForProviderWindow(&snap, "grok", "acct-same", "pinst_wrong_first", "fixed_week")
	if !ok2 || rem2 == nil || *rem2 < 0.9 {
		t.Fatalf("wrong install lookup remaining=%v", rem2)
	}
	if *rem2 == *rem {
		t.Fatal("wrong and right installs must not share remaining")
	}
}
