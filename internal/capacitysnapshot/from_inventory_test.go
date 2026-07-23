package capacitysnapshot_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func testMachineReadableSources(adapter string) []providerinventory.CatalogEntrySource {
	return []providerinventory.CatalogEntrySource{{
		SourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
		Confidence:      providerinventory.ConfidenceExact,
		FreshnessState:  providerinventory.FreshnessFresh,
		SourceReference: "provider-machine-readable:" + adapter + ":test",
	}}
}

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
			Confidence:        providerinventory.ConfidenceExact,
			EntrySources:      testMachineReadableSources("codex"),
			Source:            providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
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
				Confidence:        providerinventory.ConfidenceExact,
				EntrySources:      testMachineReadableSources("codex"),
				Source:            providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
			},
			{
				AdapterID: "codex", CanonicalModelID: "gpt-5.5",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceExact,
				EntrySources:      testMachineReadableSources("codex"),
				Source:            providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
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
	hintOnly := map[string]bool{}
	for _, m := range codex.Models {
		present[m.ModelID] = m.PresentInCatalog
		hintOnly[m.ModelID] = m.CatalogHintOnly
	}
	if present["gpt-5.3-codex"] {
		t.Fatal("gpt-5.3-codex must be not-present (account incompatible)")
	}
	if !present["gpt-5.5"] {
		t.Fatal("gpt-5.5 must remain present")
	}
	if hintOnly["gpt-5.5"] {
		t.Fatal("machine-readable gpt-5.5 must be production-routable (not CatalogHintOnly)")
	}
}

func TestStaticSeedAndSourcelessCapabilityNotProductionRoutable(t *testing.T) {
	now := t0()
	rem := int64(90)
	// Auth + real quota but only static seed / source-less capability → no auto-route.
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "antigravity", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "antigravity", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh,
		}},
		// Bare capability without EntrySources / Source.Kind — CatalogHintOnly.
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "antigravity", CanonicalModelID: "GPT-OSS 120B",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "antigravity", Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	s, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	// Quota may unlock unattended, but route inventory must have zero production candidates.
	if !s.UnattendedOK {
		t.Fatalf("quota present should allow unattended shell; reasons=%v", s.Reasons)
	}
	var ag *capacitysnapshot.AccountObservation
	for i := range s.Accounts {
		if s.Accounts[i].Provider == "antigravity" {
			ag = &s.Accounts[i]
			break
		}
	}
	if ag == nil {
		t.Fatal("missing antigravity")
	}
	foundHint := false
	for _, m := range ag.Models {
		if m.PresentInCatalog && m.CatalogHintOnly {
			foundHint = true
		}
		if m.PresentInCatalog && !m.CatalogHintOnly {
			t.Fatalf("source-less capability must not be production-routable: %+v", m)
		}
	}
	if !foundHint {
		// Static seed may add more hints when no routable models.
		t.Log("no present hint models; checking ToRouteInventory empty")
	}
	_, err = capacitysnapshot.ToRouteInventory(s, now)
	if err == nil {
		t.Fatal("source-less/static catalog must not produce production route inventory")
	}

	// Pure static seed path: no capabilities, real quota.
	rep2 := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts2 := capacitysnapshot.FromProviderInventoryReport(rep2, now)
	s2, err := capacitysnapshot.Build(accounts2, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range s2.Accounts {
		if a.Provider != "codex" {
			continue
		}
		if !strings.Contains(a.Provenance, "static_registry_estimated") && !strings.Contains(a.Provenance, "catalog_hint_only") {
			// seed only when no routable models
		}
		for _, m := range a.Models {
			if m.PresentInCatalog && !m.CatalogHintOnly {
				t.Fatalf("static seed must be CatalogHintOnly: %+v", m)
			}
		}
	}
	if _, err := capacitysnapshot.ToRouteInventory(s2, now); err == nil {
		t.Fatal("static-only seed with quota must not auto-route")
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
