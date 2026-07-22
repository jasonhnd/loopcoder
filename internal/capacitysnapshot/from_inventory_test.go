package capacitysnapshot_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func TestFromProviderInventoryReportMapsQuotaAndModels(t *testing.T) {
	now := time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
	rem := int64(60)
	lim := int64(100)
	rep := providerinventory.Report{
		InventoryFingerprint: "fp-test",
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
			RemainingValue: &rem, LimitValue: &lim,
			Confidence:     providerinventory.ConfidenceEstimated,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339),
			SourceKind:     providerinventory.QuotaSourceFixture,
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	s, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if !s.UnattendedOK {
		t.Fatalf("expected unattended ok reasons=%v", s.Reasons)
	}
	inv, err := capacitysnapshot.ToRouteInventory(s, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	if inv.Candidates[0].Model != "gpt-5.5" {
		t.Fatalf("%+v", inv.Candidates[0])
	}
}

func TestFromProviderInventoryUnknownQuotaNotUnattended(t *testing.T) {
	now := t0()
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "grok", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "grok", CanonicalModelID: "grok-4.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
		}},
		// no quota snapshots → unknown capacity
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	s, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.UnattendedOK {
		t.Fatal("missing quota must not be unattended-ok")
	}
}
