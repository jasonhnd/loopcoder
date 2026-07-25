package capacitysnapshot_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// Explicit installed+auth+quota+model remains non-production-routable.
func TestFromInventory_ExplicitInstalledAuthQuotaNonRoutable(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
		inst = "pinst_explicit_only"
		rhash = "sha256:explicit-resolved"
	)
	rem := int64(80)
	explicit := exactFreshInstall("grok", inst, rhash, "sha256:pe")
	explicit.DiscoverySource = providerinventory.DiscoveryExplicitConfig
	explicit.DiscoveryOrder = 0
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{explicit},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(inst),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{mrModel("grok", "grok-4.5", "mc", nil)},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc", AdapterID: "grok",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(inst), EntryCount: 1,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q", AdapterID: "grok",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(inst),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, LimitValue: int64Ptr(100),
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey: "provider:grok/account:" + acc + "/detail:x",
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UnattendedOK {
		t.Fatalf("explicit-only must not be unattended-eligible; reasons=%v accounts=%s",
			snap.Reasons, summarizeAccounts(accounts))
	}
	_, rerr := capacitysnapshot.ToRouteInventory(snap, now)
	if rerr == nil {
		t.Fatal("explicit-only must fail ToRouteInventory")
	}
}

// First PATH hit unusable + second usable → neither production-routable.
func TestFromInventory_FirstPATHUnusableSecondUsableNonRoutable(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
		first = "pinst_path0_unusable"
		second = "pinst_path1_usable"
		rhash = "sha256:same"
	)
	rem := int64(70)
	p0 := exactFreshInstall("grok", first, rhash, "sha256:p0")
	p0.DiscoveryOrder = 0
	p0.UsableForInvocation = "unknown"
	p1 := exactFreshInstall("grok", second, rhash, "sha256:p1")
	p1.DiscoveryOrder = 1
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{p0, p1},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(second),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{mrModel("grok", "grok-4.5", "mc", nil)},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc", AdapterID: "grok",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(second), EntryCount: 1,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q", AdapterID: "grok",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(second),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, LimitValue: int64Ptr(100),
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey: "provider:grok/account:" + acc + "/detail:x",
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UnattendedOK {
		t.Fatalf("first PATH unusable must fail closed (no later fallback); reasons=%v", snap.Reasons)
	}
}
