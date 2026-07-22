package capacitysnapshot_test

import (
	"strings"
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

func TestMissingOrUnsupportedQuotaNeverUnattended_AllProviders(t *testing.T) {
	now := t0()
	// Auth-ready + live catalog without real quota must stay ineligible for
	// every provider — including grok and antigravity. Unknown ≠ unlimited.
	providers := []string{"grok", "antigravity", "claude", "gemini", "codex"}
	for _, prov := range providers {
		t.Run(prov, func(t *testing.T) {
			rep := providerinventory.Report{
				Installations: []providerinventory.ProviderInstallation{{
					AdapterID: prov, InstallationState: providerinventory.InstallationInstalled,
					UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
					Confidence: providerinventory.ConfidenceExact,
				}},
				AuthReadiness: []providerinventory.AuthReadiness{{
					AdapterID: prov, ReadinessState: providerinventory.ReadinessReady,
					FreshnessState: providerinventory.FreshnessFresh,
				}},
				ModelCapabilities: []providerinventory.ModelCapability{{
					AdapterID: prov, CanonicalModelID: "model-x",
					AvailabilityState: providerinventory.AvailabilityAvailable,
					LifecycleState:    providerinventory.LifecycleAvailable,
					FreshnessState:    providerinventory.FreshnessFresh,
				}},
				// no usable quota snapshots
			}
			accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
			for _, a := range accounts {
				for _, w := range a.Windows {
					if w.Remaining.Class == capacitysnapshot.QtyUnlimited {
						t.Fatalf("%s: invented unlimited window: %+v", prov, w)
					}
					if w.Remaining.Class == capacitysnapshot.QtyFinite && w.Source == "auth_ready_quota_telemetry_unsupported" {
						t.Fatalf("%s: invented finite capacity from auth: %+v", prov, w)
					}
				}
			}
			s, err := capacitysnapshot.Build(accounts, now)
			if err != nil {
				t.Fatal(err)
			}
			if s.UnattendedOK {
				t.Fatalf("%s: missing quota must not be unattended-ok reasons=%v accounts=%+v",
					prov, s.Reasons, s.Accounts)
			}
		})
	}
}

func TestUnsupportedQuotaSnapshotDoesNotBecomeWindow(t *testing.T) {
	now := t0()
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "antigravity", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "antigravity", ReadinessState: providerinventory.ReadinessReady,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "antigravity", CanonicalModelID: "gemini-3.1-pro-high",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID:      "antigravity",
			Confidence:     providerinventory.ConfidenceUnavailable,
			FreshnessState: providerinventory.FreshnessNotApplicable,
			// no numeric values
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	s, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.UnattendedOK {
		t.Fatal("unsupported quota must not unlock unattended")
	}
	for _, a := range accounts {
		if a.Provider != "antigravity" {
			continue
		}
		if len(a.Windows) != 0 {
			t.Fatalf("unsupported snapshot must not become windows: %+v", a.Windows)
		}
		if !strings.Contains(a.Provenance, "quota_observation=unsupported_or_unavailable") {
			t.Fatalf("want provenance note, got %q", a.Provenance)
		}
	}
}

func TestCodexGpt53ExcludedAsCapabilityNotInventedCapacity(t *testing.T) {
	now := t0()
	rem := int64(80)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{
			{
				AdapterID: "codex", CanonicalModelID: "gpt-5.3-codex",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
			},
			{
				AdapterID: "codex", CanonicalModelID: "gpt-5.5",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
			},
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
			RemainingValue: &rem,
			Confidence:     providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339),
			SourceKind:     providerinventory.QuotaSourceFixture,
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	var codex *capacitysnapshot.AccountObservation
	for i := range accounts {
		if accounts[i].Provider == "codex" {
			codex = &accounts[i]
			break
		}
	}
	if codex == nil {
		t.Fatal("missing codex account")
	}
	if !strings.Contains(codex.Provenance, "model_excluded=gpt-5.3-codex") {
		t.Fatalf("want exclusion provenance: %q", codex.Provenance)
	}
	present := map[string]bool{}
	for _, m := range codex.Models {
		present[m.ModelID] = m.PresentInCatalog
	}
	if present["gpt-5.3-codex"] {
		t.Fatal("gpt-5.3-codex must be not-present (account incompatible)")
	}
	if !present["gpt-5.5"] {
		t.Fatal("gpt-5.5 must remain present")
	}
}

func TestStaticSeedDoesNotCreateCapacityWindows(t *testing.T) {
	now := t0()
	// Auth ready, no live models, no quota — static seed may add estimated models
	// but must never invent capacity windows or unattended eligibility.
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "antigravity", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "antigravity", ReadinessState: providerinventory.ReadinessReady,
		}},
		// no ModelCapabilities, no QuotaSnapshots
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	s, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.UnattendedOK {
		t.Fatal("static seed must not unlock unattended")
	}
	for _, a := range accounts {
		if a.Provider != "antigravity" {
			continue
		}
		if len(a.Windows) != 0 {
			t.Fatalf("static seed must not invent windows: %+v", a.Windows)
		}
		if !strings.Contains(a.Provenance, "models_source=static_registry_estimated") && len(a.Models) > 0 {
			t.Fatalf("seeded models must note estimated static source: %q", a.Provenance)
		}
	}
}
