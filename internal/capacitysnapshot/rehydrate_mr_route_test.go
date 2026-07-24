package capacitysnapshot_test

import (
	"context"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// Permanent RC37 Stage D regression: production LoadRouteInventory must rehydrate
// durable exact+fresh provider-machine-readable models (and empty-account quota)
// when live Discover only has adapter-declared static models + unavailable quota.
// Proves Codex multi-depth (low/high) candidates appear on the full product path.
func TestLoadRouteInventory_LiveStaticModels_DurableMREmptyQuota_CodexMultiDepth(t *testing.T) {
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	const (
		inst = "pinst_codex_live_static"
		acc  = "acct_nbgt2mwso4c76xepekb7oeifcsw2axkg"
		snap = "mcatsnap_codex_mr_live_static"
	)
	ptr := func(s string) *string { return &s }
	rem, lim := int64(71), int64(100)

	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", ProviderInstallationID: inst,
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes",
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{
				ResolvedPathHash: "sha256:codex-resolved-rc37",
				PathHash:         "sha256:codex-path-rc37",
			},
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			AccountProfileID: ptr(acc), ProviderInstallationID: ptr(inst),
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			CapturedAt:          now.Format(time.RFC3339),
		}},
		// Live Discover without MR catalog grant: adapter-declared only.
		ModelCapabilities: []providerinventory.ModelCapability{{
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
			// Conflicting top-level Source.Kind must not make this production MR.
			Source: providerinventory.SourceDescriptor{
				Kind: string(providerinventory.CatalogSourceProviderMachineReadable),
			},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "live-unavail",
			Confidence:     providerinventory.ConfidenceUnavailable,
			FreshnessState: providerinventory.FreshnessNotApplicable,
			GapReasons:     []string{"quota-collection-not-granted"},
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}

	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: live.AuthReadiness,
		// Empty-account exact+fresh quota on same install (Codex production shape).
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "dur-primary", AdapterID: "codex",
			ProviderInstallationID: ptr(inst),
			// AccountProfileID intentionally empty — sole Ready auth rebinds.
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, LimitValue: &lim,
			Confidence:     providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
			StaleAfter:     now.Add(time.Hour).Format(time.RFC3339),
			ResetAt:        now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: snap, AdapterID: "codex",
			CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			CatalogSourceReference: "codex-app-server:model-list#sha256:rc37",
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			ProviderInstallationID: ptr(inst),
			EntryCount:             1,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			ModelCapabilityID: "mcap_gpt55_mr", ModelCatalogSnapshotID: snap,
			AdapterID: "codex", CanonicalModelID: "gpt-5.5", DisplayName: "GPT-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			Constraints: []string{
				"supported_depth=low",
				"supported_depth=medium",
				"supported_depth=high",
				"default_depth=medium",
			},
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
				SourceReference: "codex-app-server:model-list#sha256:rc37",
				Confidence:      providerinventory.ConfidenceExact,
				FreshnessState:  providerinventory.FreshnessFresh,
			}},
			Source: providerinventory.SourceDescriptor{
				Kind: string(providerinventory.CatalogSourceProviderMachineReadable),
			},
		}},
	}

	inv, snapOut, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now: now,
		Discover: func(context.Context, providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
		LoadDurable: func(context.Context) (providerinventory.Report, error) {
			return durable, nil
		},
	})
	if err != nil {
		t.Fatalf("LoadRouteInventory: %v unattended=%v reasons=%v", err, snapOut.UnattendedOK, snapOut.Reasons)
	}
	if !snapOut.UnattendedOK {
		t.Fatalf("want UnattendedOK after durable MR+quota rehydrate; reasons=%v", snapOut.Reasons)
	}

	var hasLow, hasMed, hasHigh bool
	for _, c := range inv.Candidates {
		if c.Provider != "codex" || c.Model != "gpt-5.5" {
			continue
		}
		switch c.Effort {
		case "low":
			hasLow = true
		case "medium":
			hasMed = true
		case "high":
			hasHigh = true
		}
	}
	if !hasLow || !hasMed || !hasHigh {
		t.Fatalf("want codex/gpt-5.5 low+medium+high candidates after durable MR overlay; got %#v", inv.Candidates)
	}
}

// Same production path for Antigravity: live adapter-declared + durable MR ladder
// + empty-account exact quota must yield multi-depth candidates.
func TestLoadRouteInventory_LiveStaticModels_DurableMREmptyQuota_AntigravityMultiDepth(t *testing.T) {
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	const (
		inst = "pinst_agy_live_static"
		acc  = "acct_xuczrohs6bt2wowcg7ujk5docev5fvkt"
		snap = "mcatsnap_agy_mr"
	)
	ptr := func(s string) *string { return &s }
	rem := int64(90)

	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "antigravity", ProviderInstallationID: inst,
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes",
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "antigravity", ReadinessState: providerinventory.ReadinessReady,
			AccountProfileID: ptr(acc), ProviderInstallationID: ptr(inst),
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			CapturedAt:          now.Format(time.RFC3339),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "antigravity", CanonicalModelID: "GPT-OSS 120B",
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
			AdapterID: "antigravity", Confidence: providerinventory.ConfidenceUnavailable,
			FreshnessState: providerinventory.FreshnessNotApplicable,
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}

	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: live.AuthReadiness,
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "agy-q", AdapterID: "antigravity",
			ProviderInstallationID: ptr(inst),
			Unit:                   "percent", WindowKind: providerinventory.WindowFixedHour,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:antigravity|window:primary_5h",
			CapturedAt:     now.Format(time.RFC3339),
			StaleAfter:     now.Add(time.Hour).Format(time.RFC3339),
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: snap, AdapterID: "antigravity",
			CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			ProviderInstallationID: ptr(inst), EntryCount: 3,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{
			mrDepthCap("antigravity", "gemini-flash-low", snap, "low"),
			mrDepthCap("antigravity", "gemini-flash-medium", snap, "medium"),
			mrDepthCap("antigravity", "gemini-flash-high", snap, "high"),
		},
	}

	inv, snapOut, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now: now,
		Discover: func(context.Context, providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
		LoadDurable: func(context.Context) (providerinventory.Report, error) {
			return durable, nil
		},
	})
	if err != nil {
		t.Fatalf("LoadRouteInventory AG: %v reasons=%v", err, snapOut.Reasons)
	}
	if !snapOut.UnattendedOK {
		t.Fatalf("AG want UnattendedOK; reasons=%v", snapOut.Reasons)
	}
	var hasLow, hasHigh bool
	for _, c := range inv.Candidates {
		if c.Provider != "antigravity" {
			continue
		}
		switch c.Effort {
		case "low":
			hasLow = true
		case "high":
			hasHigh = true
		}
	}
	if !hasLow || !hasHigh {
		t.Fatalf("want antigravity low+high candidates; got %#v", inv.Candidates)
	}
}

func mrDepthCap(adapter, model, snapID, depth string) providerinventory.ModelCapability {
	return providerinventory.ModelCapability{
		ModelCapabilityID: "mcap_" + model, ModelCatalogSnapshotID: snapID,
		AdapterID: adapter, CanonicalModelID: model,
		AvailabilityState: providerinventory.AvailabilityAvailable,
		LifecycleState:    providerinventory.LifecycleAvailable,
		FreshnessState:    providerinventory.FreshnessFresh,
		Confidence:        providerinventory.ConfidenceExact,
		Constraints:       []string{"supported_depth=" + depth},
		EntrySources: []providerinventory.CatalogEntrySource{{
			SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:     providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
		}},
		Source: providerinventory.SourceDescriptor{
			Kind: string(providerinventory.CatalogSourceProviderMachineReadable),
		},
	}
}

// Durable estimated/static models must not become production route candidates even
// when rehydrate is asked (fail closed via rehydrate filter + mapper).
func TestLoadRouteInventory_DurableStaticEstimatedModelsNotRoutable(t *testing.T) {
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	const inst = "pinst_weak"
	ptr := func(s string) *string { return &s }
	rem := int64(50)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", ProviderInstallationID: inst,
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes",
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			AccountProfileID: ptr("acct_weak"), ProviderInstallationID: ptr(inst),
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			CapturedAt:          now.Format(time.RFC3339),
		}},
		// No live models.
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", Confidence: providerinventory.ConfidenceUnavailable,
			FreshnessState: providerinventory.FreshnessNotApplicable,
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	durable := providerinventory.Report{
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", ProviderInstallationID: ptr(inst),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339),
			StaleAfter:     now.Add(time.Hour).Format(time.RFC3339),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{
			{
				AdapterID: "codex", CanonicalModelID: "static-model",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceExact,
				Constraints:       []string{"supported_depth=low", "supported_depth=high"},
				EntrySources: []providerinventory.CatalogEntrySource{{
					SourceKind:     providerinventory.CatalogSourceAdapterDeclared,
					Confidence:     providerinventory.ConfidenceExact,
					FreshnessState: providerinventory.FreshnessFresh,
				}},
			},
			{
				AdapterID: "codex", CanonicalModelID: "estimated-mr",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceEstimated,
				Constraints:       []string{"supported_depth=high"},
				EntrySources: []providerinventory.CatalogEntrySource{{
					SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
					Confidence:     providerinventory.ConfidenceEstimated,
					FreshnessState: providerinventory.FreshnessFresh,
				}},
			},
		},
	}
	_, snap, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now: now,
		Discover: func(context.Context, providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
		LoadDurable: func(context.Context) (providerinventory.Report, error) {
			return durable, nil
		},
	})
	// No production MR models → no unattended route inventory.
	if err == nil && snap.UnattendedOK {
		t.Fatal("static/estimated durable models must not yield unattended production routes")
	}
}
