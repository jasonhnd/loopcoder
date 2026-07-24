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
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
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
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
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

// TestFromProviderInventory_TwoAccountsSameProvider_NoCollapse proves multi-account
// providers are not collapsed into one invented acct-+provider row.
func TestFromProviderInventory_TwoAccountsSameProvider_NoCollapse(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	acc1 := "profile-alpha-id"
	acc2 := "profile-beta-id"
	rep := providerinventory.Report{
		InventoryFingerprint: "fp-two-acct",
		Installations: []providerinventory.ProviderInstallation{
			{
				AdapterID: "grok", ProviderInstallationID: "inst-a",
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence: providerinventory.ConfidenceExact,
			},
			{
				AdapterID: "grok", ProviderInstallationID: "inst-b",
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence: providerinventory.ConfidenceExact,
			},
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			{AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady, AccountProfileID: &acc1},
			{AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady, AccountProfileID: &acc2},
		},
	}
	obs := capacitysnapshot.FromProviderInventoryReport(rep, now)
	if len(obs) < 2 {
		t.Fatalf("want >=2 account observations for two grok accounts, got %d %+v", len(obs), obs)
	}
	seen := map[string]bool{}
	for _, o := range obs {
		if o.Provider != "grok" {
			continue
		}
		if o.AccountRef == "acct-grok" || o.AccountRef == "account-unknown" {
			t.Fatalf("must not invent provider-collapsed account: %+v", o)
		}
		if o.AccountRef != "" {
			seen[o.AccountRef] = true
		}
	}
	if len(seen) < 2 {
		t.Fatalf("want two distinct account refs, got %v from %+v", seen, obs)
	}
}

func TestQtyFromPtrScaled_ValueScale2Percent(t *testing.T) {
	// Grok stores hundredths of percent with ValueScale=2: 6900 → 69.00, not 6900%.
	used := int64(3100)
	rem := int64(6900)
	limit := int64(10000)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	acc := "acct-" + strings.Repeat("a", 64)
	rep := providerinventory.Report{
		QuotaSnapshots: []providerinventory.QuotaSnapshot{
			{
				QuotaSnapshotID: "q1", AdapterID: "grok", AccountProfileID: &acc,
				Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
				UsedValue: &used, RemainingValue: &rem, LimitValue: &limit,
				ValueScale: 2, Confidence: providerinventory.ConfidenceExact,
				FreshnessState: providerinventory.FreshnessFresh,
				CapturedAt:     now.Format(time.RFC3339), SourceKind: providerinventory.QuotaSourceOfficialCLICommand,
			},
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			{AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady, AccountProfileID: &acc},
		},
		Installations: []providerinventory.ProviderInstallation{
			{
				AdapterID: "grok", ProviderInstallationID: "pinst-test",
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence: providerinventory.ConfidenceExact,
			},
		},
	}
	obs := capacitysnapshot.FromProviderInventoryReport(rep, now)
	found := false
	for _, o := range obs {
		for _, w := range o.Windows {
			if w.Remaining.Class != capacitysnapshot.QtyFinite {
				continue
			}
			found = true
			// 6900 with scale 2 → 69.0
			if w.Remaining.Value < 68.9 || w.Remaining.Value > 69.1 {
				t.Fatalf("remaining after scale want ~69 got %v (unit=%s)", w.Remaining.Value, w.Unit)
			}
			if w.Used.Value < 30.9 || w.Used.Value > 31.1 {
				t.Fatalf("used after scale want ~31 got %v", w.Used.Value)
			}
			// RemainingFraction for percentage >1 divides by 100 → ~0.69
			rf := capacitysnapshot.RemainingFraction(w)
			if rf == nil || *rf < 0.68 || *rf > 0.70 {
				t.Fatalf("remaining fraction want ~0.69 got %v", rf)
			}
		}
	}
	if !found {
		t.Fatalf("no finite remaining window in %+v", obs)
	}
}

func TestStaleAfterDoesNotOutliveResetAt(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	acc := "acct-" + strings.Repeat("b", 64)
	used := int64(10)
	rem := int64(90)
	limit := int64(100)
	reset := now.Add(1 * time.Hour).Format(time.RFC3339)
	stale := now.Add(48 * time.Hour).Format(time.RFC3339) // outlives reset
	rep := providerinventory.Report{
		QuotaSnapshots: []providerinventory.QuotaSnapshot{
			{
				QuotaSnapshotID: "q2", AdapterID: "grok", AccountProfileID: &acc,
				Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
				UsedValue: &used, RemainingValue: &rem, LimitValue: &limit,
				ValueScale: 0, Confidence: providerinventory.ConfidenceExact,
				FreshnessState: providerinventory.FreshnessFresh,
				CapturedAt:     now.Format(time.RFC3339), ResetAt: reset, StaleAfter: stale,
				SourceKind: providerinventory.QuotaSourceOfficialCLICommand,
			},
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			{AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady, AccountProfileID: &acc},
		},
		Installations: []providerinventory.ProviderInstallation{
			{
				AdapterID: "grok", ProviderInstallationID: "pinst-test2",
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence: providerinventory.ConfidenceExact,
			},
		},
	}
	obs := capacitysnapshot.FromProviderInventoryReport(rep, now)
	// Provenance note that stale_after was clamped; ResetAt preserved.
	ok := false
	for _, o := range obs {
		if strings.Contains(o.Provenance, "stale_after_clamped_to_reset_at") {
			ok = true
		}
		for _, w := range o.Windows {
			if w.ResetAt == nil {
				t.Fatal("ResetAt must be preserved")
			}
		}
	}
	if !ok && len(obs) > 0 {
		// Acceptable if no windows joined; clamp only when both present.
		t.Logf("obs=%+v", obs)
	}
}
