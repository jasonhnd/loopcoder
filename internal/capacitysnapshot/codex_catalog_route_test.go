package capacitysnapshot_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// #1397: observed Codex model/list catalog (MR exact/fresh) must route Soul/read-only/high
// and expose low — without making static adapter-declared rows routable.
func TestCodexMachineReadableCatalog_RoutesHighAndLow(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	reset := now.Add(7 * 24 * time.Hour)
	ptr := func(s string) *string { return &s }
	i64 := func(v int64) *int64 { return &v }
	acct := "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
	inst := "pinst_codex_exact_install_001"
	snapID := "mcatsnap_codex_mr_list"

	rep := providerinventory.Report{
		InventoryFingerprint: "fp-codex-mr",
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", inst, "sha256:codex-mr-resolved", "sha256:codex-mr-path"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			AccountProfileID: ptr(acct), ProviderInstallationID: ptr(inst),
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: snapID, AdapterID: "codex",
			ProviderInstallationID: ptr(inst), AccountProfileID: ptr(acct),
			CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			CatalogSourceReference: "codex-app-server:model-list#sha256:deadbeef",
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
		}},
		// Static adapter-declared snapshot must not route.
		// (Also include one static capability that would be hint-only.)
		ModelCapabilities: []providerinventory.ModelCapability{
			{
				ModelCapabilityID: "mcap_gpt55_mr", ModelCatalogSnapshotID: snapID,
				AdapterID: "codex", CanonicalModelID: "gpt-5.5", DisplayName: "GPT-5.5",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceExact,
				Constraints: []string{
					"cli_model=gpt-5.5",
					"supported_depth=low",
					"supported_depth=medium",
					"supported_depth=high",
					"supported_depth=xhigh",
					"default_depth=medium",
				},
				EntrySources: []providerinventory.CatalogEntrySource{{
					SourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
					SourceReference: "codex-app-server:model-list#sha256:deadbeef",
					Confidence:      providerinventory.ConfidenceExact,
					FreshnessState:  providerinventory.FreshnessFresh,
				}},
			},
			// Adapter-declared static-looking row for same model id should not make routes
			// if MR present; if only this exists it would be hint-only.
			{
				ModelCapabilityID: "mcap_static_hint", ModelCatalogSnapshotID: "mcatsnap_static",
				AdapterID: "codex", CanonicalModelID: "gpt-5.3-codex",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceExact,
				EntrySources: []providerinventory.CatalogEntrySource{{
					SourceKind:     providerinventory.CatalogSourceAdapterDeclared,
					Confidence:     providerinventory.ConfidenceExact,
					FreshnessState: providerinventory.FreshnessFresh,
				}},
			},
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q_codex_week", AdapterID: "codex",
			AccountProfileID: ptr(acct), ProviderInstallationID: ptr(inst),
			ScopeKey:   "provider:codex/account:" + acct + "/detail:secondary",
			WindowKind: providerinventory.WindowFixedWeek, Unit: "percent",
			QuantityKind: providerinventory.QuantityProviderDefined,
			LimitValue:   i64(100), UsedValue: i64(20), RemainingValue: i64(80),
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ResetAt: reset.Format(time.RFC3339), CapturedAt: now.Format(time.RFC3339),
		}},
	}

	obs := capacitysnapshot.FromProviderInventoryReport(rep, now)
	// Find codex account with model gpt-5.5 not hint-only.
	var found bool
	for _, o := range obs {
		if o.Provider != "codex" {
			continue
		}
		for _, m := range o.Models {
			if m.ModelID == "gpt-5.5" && m.PresentInCatalog && !m.CatalogHintOnly {
				found = true
				depthSet := map[string]bool{}
				for _, d := range m.SupportedDepths {
					depthSet[d] = true
				}
				for _, need := range []string{"low", "medium", "high", "xhigh"} {
					if !depthSet[need] {
						t.Fatalf("gpt-5.5 depths=%v missing %s", m.SupportedDepths, need)
					}
				}
			}
			if m.ModelID == "gpt-5.3-codex" && m.PresentInCatalog && !m.CatalogHintOnly {
				t.Fatalf("static adapter-declared gpt-5.3-codex must be CatalogHintOnly: %+v", m)
			}
		}
	}
	if !found {
		t.Fatalf("want production-routable gpt-5.5; accounts=%+v", obs)
	}

	snap, err := capacitysnapshot.Build(obs, now)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnattendedOK {
		t.Fatalf("unattended: %v", snap.Reasons)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}

	// High soul read-only → codex gpt-5.5
	resHigh, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "read-only", Effort: "high",
		TaskClass: "soul", ProjectID: "p-codex-high", DecisionKey: "dk-high",
		Inventory: &inv, Now: now,
	})
	if err != nil {
		t.Fatalf("high resolve: %v", err)
	}
	if resHigh.Outcome != autoroute.OutcomeSelected || resHigh.Provider != "codex" || resHigh.Model != "gpt-5.5" {
		t.Fatalf("want codex/gpt-5.5 high selected, got outcome=%s %s/%s", resHigh.Outcome, resHigh.Provider, resHigh.Model)
	}
	if resHigh.AccountRef == "" || resHigh.InstallRef == "" || resHigh.WindowKind == "" {
		t.Fatalf("missing exact identity: acct=%q inst=%q win=%q", resHigh.AccountRef, resHigh.InstallRef, resHigh.WindowKind)
	}
	if resHigh.Effort != "high" {
		t.Fatalf("effort=%q want high", resHigh.Effort)
	}

	// Low luna path exists
	resLow, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, Permission: "read-only", Effort: "low",
		TaskClass: "luna", ProjectID: "p-codex-low", DecisionKey: "dk-low",
		Inventory: &inv, Now: now,
	})
	if err != nil {
		t.Fatalf("low resolve: %v", err)
	}
	if resLow.Outcome != autoroute.OutcomeSelected || resLow.Provider != "codex" {
		t.Fatalf("want codex low selected, got %+v", resLow)
	}
	if resLow.Effort != "low" {
		t.Fatalf("effort=%q want low", resLow.Effort)
	}
}

func TestCodexAdapterDeclaredOnly_CannotRoute(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	ptr := func(s string) *string { return &s }
	i64 := func(v int64) *int64 { return &v }
	acct := "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	inst := "pinst_codex_static_only"
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			ProviderInstallationID: inst, AdapterID: "codex",
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			AccountProfileID: ptr(acct), ProviderInstallationID: ptr(inst),
			FreshnessState: providerinventory.FreshnessFresh,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			ModelCapabilityID: "m_static", ModelCatalogSnapshotID: "mcs_static",
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:     providerinventory.CatalogSourceAdapterDeclared,
				Confidence:     providerinventory.ConfidenceExact,
				FreshnessState: providerinventory.FreshnessFresh,
			}},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q1", AdapterID: "codex",
			AccountProfileID: ptr(acct), ProviderInstallationID: ptr(inst),
			WindowKind: providerinventory.WindowFixedWeek, Unit: "percent",
			LimitValue: i64(100), RemainingValue: i64(90), UsedValue: i64(10),
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	obs := capacitysnapshot.FromProviderInventoryReport(rep, now)
	for _, o := range obs {
		for _, m := range o.Models {
			if m.ModelID == "gpt-5.5" && m.PresentInCatalog && !m.CatalogHintOnly {
				t.Fatalf("adapter-declared must be CatalogHintOnly: %+v", m)
			}
		}
	}
	snap, err := capacitysnapshot.Build(obs, now)
	if err != nil {
		// May fail unattended if no production-routable model — also acceptable.
		if strings.Contains(err.Error(), "no unattended") || strings.Contains(err.Error(), "invalid") {
			return
		}
		t.Fatal(err)
	}
	_, err = capacitysnapshot.ToRouteInventory(snap, now)
	if err == nil {
		// If inventory maps somehow, Resolve must not select pure-hint codex.
		// ToRouteInventory should fail closed with no candidates.
		t.Fatal("ToRouteInventory must fail closed when only CatalogHintOnly models present")
	}
}
