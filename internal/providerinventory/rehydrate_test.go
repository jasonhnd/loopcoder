package providerinventory_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func TestRehydrateForAutoRoutePrefersDurableFreshQuota(t *testing.T) {
	now := time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
	rem := int64(97)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh,
			Confidence:     providerinventory.ConfidenceExact,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
		}},
		// Discover without grant: unavailable not-collected
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "live-unavail", QuotaSourceID: "src-live",
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			Confidence: providerinventory.ConfidenceUnavailable, FreshnessState: providerinventory.FreshnessNotApplicable,
			GapReasons: []string{"quota-collection-not-granted"},
			CapturedAt: now.Format(time.RFC3339),
		}},
		QuotaTelemetrySources: []providerinventory.QuotaTelemetrySource{{
			AdapterID: "codex", QuotaSourceID: "src-live",
		}},
		GapReasons: []string{"provider-codex-quota-unsupported"},
	}
	durable := providerinventory.Report{
		QuotaTelemetrySources: []providerinventory.QuotaTelemetrySource{{
			AdapterID: "codex", QuotaSourceID: "src-durable",
			SourceKind: providerinventory.QuotaSourceOfficialCLICommand,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "dur-primary", QuotaSourceID: "src-durable",
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState:       providerinventory.FreshnessFresh,
			CapturedAt:           now.Format(time.RFC3339),
			StaleAfter:           now.Add(24 * time.Hour).Format(time.RFC3339),
			ResetAt:              now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			ProviderQuantityName: "primary_used_percent",
		}},
	}

	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	if len(merged.QuotaSnapshots) != 1 {
		t.Fatalf("want 1 rehydrated snap, got %#v", merged.QuotaSnapshots)
	}
	got := merged.QuotaSnapshots[0]
	if got.QuotaSnapshotID != "dur-primary" {
		t.Fatalf("want durable snap, got %#v", got)
	}
	if got.Confidence != providerinventory.ConfidenceExact || got.FreshnessState != providerinventory.FreshnessFresh {
		t.Fatalf("want exact/fresh, got conf=%s fresh=%s", got.Confidence, got.FreshnessState)
	}
	if got.RemainingValue == nil || *got.RemainingValue != 97 {
		t.Fatalf("remaining=%v", got.RemainingValue)
	}
	if got.ResetAt == "" {
		t.Fatal("reset_at must rehydrate")
	}
	// live install/auth preserved
	if len(merged.Installations) != 1 || merged.AuthReadiness[0].ReadinessState != providerinventory.ReadinessReady {
		t.Fatalf("live install/auth lost: %#v %#v", merged.Installations, merged.AuthReadiness)
	}
	for _, g := range merged.GapReasons {
		if g == "provider-codex-quota-unsupported" {
			t.Fatalf("gap should drop after rehydrate: %v", merged.GapReasons)
		}
	}
}

func TestRehydrateForAutoRouteDoesNotUseStaleOrUnknownDurable(t *testing.T) {
	now := time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
	rem := int64(50)
	live := providerinventory.Report{
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "live-unavail",
			Confidence:     providerinventory.ConfidenceUnavailable,
			FreshnessState: providerinventory.FreshnessNotApplicable,
			GapReasons:     []string{"quota-collection-not-granted"},
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	// Stale durable (stale_after in the past)
	durable := providerinventory.Report{
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "dur-stale", QuotaSourceID: "src",
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh, // will be marked stale by stale_after
			CapturedAt:     now.Add(-2 * time.Hour).Format(time.RFC3339),
			StaleAfter:     now.Add(-time.Minute).Format(time.RFC3339),
		}},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	// Must keep live unusable — not promote stale durable as usable.
	if len(merged.QuotaSnapshots) != 1 || merged.QuotaSnapshots[0].QuotaSnapshotID != "live-unavail" {
		t.Fatalf("stale durable must not win: %#v", merged.QuotaSnapshots)
	}
}

func TestRehydrateKeepsLiveWhenAlreadyTrustworthy(t *testing.T) {
	now := time.Date(2026, 7, 22, 16, 0, 0, 0, time.UTC)
	liveRem := int64(80)
	durRem := int64(10)
	live := providerinventory.Report{
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "live-ok", QuotaSourceID: "src-live",
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &liveRem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339),
			StaleAfter:     now.Add(time.Hour).Format(time.RFC3339),
		}},
	}
	durable := providerinventory.Report{
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", QuotaSnapshotID: "dur-ok", QuotaSourceID: "src-dur",
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &durRem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339),
			StaleAfter:     now.Add(time.Hour).Format(time.RFC3339),
		}},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	if len(merged.QuotaSnapshots) != 1 || merged.QuotaSnapshots[0].QuotaSnapshotID != "live-ok" {
		t.Fatalf("live trustworthy must win: %#v", merged.QuotaSnapshots)
	}
}

func TestRehydrateTranslatesDurableAliasInstallToSoleLiveTarget(t *testing.T) {
	// RC36: live only A; durable B + quota on B; same exact resolved hash.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		instA = "pinst_live_a"
		instB = "pinst_durable_b"
		rhash = "sha256:resolved-same"
	)
	rem := int64(35)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "grok", ProviderInstallationID: instA,
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes",
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{
				ResolvedPathHash: rhash, PathHash: "sha256:path-a",
			},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "grok", QuotaSnapshotID: "live-unavail", QuotaSourceID: "src-live",
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			Confidence: providerinventory.ConfidenceUnavailable, FreshnessState: providerinventory.FreshnessNotApplicable,
			GapReasons: []string{"quota-collection-not-granted"},
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	durable := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "grok", ProviderInstallationID: instB,
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes",
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{
				ResolvedPathHash: rhash, PathHash: "sha256:path-b",
			},
		}},
		QuotaTelemetrySources: []providerinventory.QuotaTelemetrySource{{
			AdapterID: "grok", QuotaSourceID: "src-durable",
			SourceKind: providerinventory.QuotaSourceOfficialCLICommand,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "grok", QuotaSnapshotID: "dur-q", QuotaSourceID: "src-durable",
			ProviderInstallationID: strPtr(instB),
			Unit:                   "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339),
			StaleAfter:     now.Add(time.Hour).Format(time.RFC3339),
		}},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	if len(merged.QuotaSnapshots) != 1 {
		t.Fatalf("want 1 durable snap, got %#v", merged.QuotaSnapshots)
	}
	q := merged.QuotaSnapshots[0]
	if q.ProviderInstallationID == nil || *q.ProviderInstallationID != instA {
		t.Fatalf("durable B must translate to live A, got %#v", q.ProviderInstallationID)
	}
	// Live installations must still be only A — never promote durable B as live-installed.
	if len(merged.Installations) != 1 || merged.Installations[0].ProviderInstallationID != instA {
		t.Fatalf("live installs must remain sole host truth: %#v", merged.Installations)
	}
	if merged.Installations[0].InstallationState != providerinventory.InstallationInstalled {
		t.Fatal("live A must stay installed")
	}
}

func TestRehydrateAmbiguousLiveAliasesNoTranslate(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const rhash = "sha256:ambig"
	rem := int64(10)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			{
				AdapterID: "grok", ProviderInstallationID: "pinst_l1",
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence:         providerinventory.ConfidenceExact,
				ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: rhash},
			},
			{
				AdapterID: "grok", ProviderInstallationID: "pinst_l2",
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence:         providerinventory.ConfidenceExact,
				ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: rhash},
			},
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "grok", QuotaSnapshotID: "live-unavail", QuotaSourceID: "src-live",
			Confidence: providerinventory.ConfidenceUnavailable, FreshnessState: providerinventory.FreshnessNotApplicable,
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	durable := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "grok", ProviderInstallationID: "pinst_db",
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence:         providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: rhash},
		}},
		QuotaTelemetrySources: []providerinventory.QuotaTelemetrySource{{
			AdapterID: "grok", QuotaSourceID: "src-d",
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "grok", QuotaSnapshotID: "dq", QuotaSourceID: "src-d",
			ProviderInstallationID: strPtr("pinst_db"),
			RemainingValue:         &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339), StaleAfter: now.Add(time.Hour).Format(time.RFC3339),
		}},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	if len(merged.QuotaSnapshots) != 1 {
		t.Fatalf("%#v", merged.QuotaSnapshots)
	}
	// Ambiguous: keep durable id (fail closed translation).
	if merged.QuotaSnapshots[0].ProviderInstallationID == nil ||
		*merged.QuotaSnapshots[0].ProviderInstallationID != "pinst_db" {
		t.Fatalf("ambiguous multi-live must not translate, got %#v", merged.QuotaSnapshots[0].ProviderInstallationID)
	}
}

func TestRehydrateEstimatedOrStaleDurableInstallNoTranslate(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const rhash = "sha256:est"
	rem := int64(10)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "grok", ProviderInstallationID: "pinst_live",
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence:         providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: rhash},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "grok", QuotaSnapshotID: "live-unavail",
			Confidence: providerinventory.ConfidenceUnavailable, FreshnessState: providerinventory.FreshnessNotApplicable,
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	// Durable estimated — must not participate in alias map.
	durable := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "grok", ProviderInstallationID: "pinst_est",
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence:         providerinventory.ConfidenceEstimated,
			ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: rhash},
		}},
		QuotaTelemetrySources: []providerinventory.QuotaTelemetrySource{{
			AdapterID: "grok", QuotaSourceID: "src-d",
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "grok", QuotaSnapshotID: "dq", QuotaSourceID: "src-d",
			ProviderInstallationID: strPtr("pinst_est"),
			RemainingValue:         &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt:     now.Format(time.RFC3339), StaleAfter: now.Add(time.Hour).Format(time.RFC3339),
		}},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	if merged.QuotaSnapshots[0].ProviderInstallationID == nil ||
		*merged.QuotaSnapshots[0].ProviderInstallationID != "pinst_est" {
		t.Fatalf("estimated durable identity must not translate: %#v", merged.QuotaSnapshots[0].ProviderInstallationID)
	}
}

// RC37 Stage D: live Discover adapter-declared models must not suppress durable
// exact+fresh machine-readable model catalogs for the same adapter.
func TestRehydrateModelsOverlaysDurableMRWhenLiveOnlyAdapterDeclared(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", ProviderInstallationID: "pinst_live",
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
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
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", Confidence: providerinventory.ConfidenceUnavailable,
			FreshnessState: providerinventory.FreshnessNotApplicable,
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	durable := providerinventory.Report{
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			Constraints:       []string{"supported_depth=low", "supported_depth=medium", "supported_depth=high"},
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
				SourceReference: "codex-app-server:model-list#test",
				Confidence:      providerinventory.ConfidenceExact,
				FreshnessState:  providerinventory.FreshnessFresh,
			}},
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc_mr", AdapterID: "codex",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:        providerinventory.ConfidenceExact,
			FreshnessState:    providerinventory.FreshnessFresh,
			EntryCount:        1,
		}},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	var mr, declared int
	for _, m := range merged.ModelCapabilities {
		if m.AdapterID != "codex" {
			continue
		}
		for _, s := range m.EntrySources {
			switch s.SourceKind {
			case providerinventory.CatalogSourceProviderMachineReadable:
				mr++
			case providerinventory.CatalogSourceAdapterDeclared:
				declared++
			}
		}
	}
	if mr == 0 {
		t.Fatalf("durable exact+fresh MR model must rehydrate over live adapter-declared only; models=%#v", merged.ModelCapabilities)
	}
	if declared == 0 {
		t.Fatal("live adapter-declared row should remain (capacity mapper marks CatalogHintOnly when MR present)")
	}
}

// EntrySources is authoritative: adapter-declared EntrySources must not count as
// live MR even when top-level Source.Kind falsely claims machine-readable.
func TestRehydrateModels_EntrySourcesAuthority_ConflictingSourceKindDoesNotBlockDurableMR(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	live := providerinventory.Report{
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			// Conflicting shape that previously false-positived production MR.
			Source: providerinventory.SourceDescriptor{
				Kind: string(providerinventory.CatalogSourceProviderMachineReadable),
			},
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:     providerinventory.CatalogSourceAdapterDeclared,
				Confidence:     providerinventory.ConfidenceExact,
				FreshnessState: providerinventory.FreshnessFresh,
			}},
		}},
	}
	durable := providerinventory.Report{
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			Constraints:       []string{"supported_depth=low", "supported_depth=high"},
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:     providerinventory.ConfidenceExact,
				FreshnessState: providerinventory.FreshnessFresh,
			}},
		}},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	var hasDurableMR bool
	for _, m := range merged.ModelCapabilities {
		for _, s := range m.EntrySources {
			if s.SourceKind == providerinventory.CatalogSourceProviderMachineReadable {
				hasDurableMR = true
			}
		}
	}
	if !hasDurableMR {
		t.Fatalf("EntrySources=adapter-declared must not block durable MR despite Source.Kind=MR; got %#v", merged.ModelCapabilities)
	}
	_ = now
}

func TestRehydrateModelsSkipsDurableWhenLiveAlreadyHasProductionMR(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	liveMR := providerinventory.ModelCapability{
		AdapterID: "codex", CanonicalModelID: "gpt-5.5",
		AvailabilityState: providerinventory.AvailabilityAvailable,
		LifecycleState:    providerinventory.LifecycleAvailable,
		FreshnessState:    providerinventory.FreshnessFresh,
		Confidence:        providerinventory.ConfidenceExact,
		Constraints:       []string{"supported_depth=medium"},
		EntrySources: []providerinventory.CatalogEntrySource{{
			SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:     providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
		}},
	}
	live := providerinventory.Report{
		ModelCapabilities: []providerinventory.ModelCapability{liveMR},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "live_mc", AdapterID: "codex",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:        providerinventory.ConfidenceExact,
			FreshnessState:    providerinventory.FreshnessFresh,
			EntryCount:        1,
		}},
	}
	durable := providerinventory.Report{
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.4",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:     providerinventory.ConfidenceExact,
				FreshnessState: providerinventory.FreshnessFresh,
			}},
		}},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	for _, m := range merged.ModelCapabilities {
		if m.CanonicalModelID == "gpt-5.4" {
			t.Fatalf("when live already has production MR, durable MR must not inject another model set: %#v", merged.ModelCapabilities)
		}
	}
	if len(merged.ModelCapabilities) != 1 || merged.ModelCapabilities[0].CanonicalModelID != "gpt-5.5" {
		t.Fatalf("want sole live MR gpt-5.5, got %#v", merged.ModelCapabilities)
	}
	_ = now
}

// Live rows with MR source but not present (mapper would not produce candidates)
// must not block durable exact+fresh available MR.
func TestRehydrateModels_UnpresentLiveMRDoesNotBlockDurableMR(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	baseMRSource := func() providerinventory.ModelCapability {
		return providerinventory.ModelCapability{
			AdapterID: "codex", CanonicalModelID: "live-broken",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:     providerinventory.ConfidenceExact,
				FreshnessState: providerinventory.FreshnessFresh,
			}},
		}
	}
	durableMR := providerinventory.ModelCapability{
		AdapterID: "codex", CanonicalModelID: "gpt-5.5",
		AvailabilityState: providerinventory.AvailabilityAvailable,
		LifecycleState:    providerinventory.LifecycleAvailable,
		FreshnessState:    providerinventory.FreshnessFresh,
		Confidence:        providerinventory.ConfidenceExact,
		Constraints:       []string{"supported_depth=low", "supported_depth=high"},
		EntrySources: []providerinventory.CatalogEntrySource{{
			SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:     providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
		}},
	}

	cases := []struct {
		name string
		mut  func(*providerinventory.ModelCapability)
	}{
		{"empty_canonical_id", func(m *providerinventory.ModelCapability) { m.CanonicalModelID = "" }},
		{"unavailable", func(m *providerinventory.ModelCapability) {
			m.AvailabilityState = providerinventory.AvailabilityTemporarilyUnavailable
		}},
		{"removed", func(m *providerinventory.ModelCapability) {
			m.LifecycleState = providerinventory.LifecycleRemoved
		}},
		{"deprecated", func(m *providerinventory.ModelCapability) {
			m.LifecycleState = providerinventory.LifecycleDeprecated
		}},
		{"stale_top_level", func(m *providerinventory.ModelCapability) {
			m.FreshnessState = providerinventory.FreshnessStale
		}},
		{"expired_top_level", func(m *providerinventory.ModelCapability) {
			m.FreshnessState = providerinventory.FreshnessExpired
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			liveRow := baseMRSource()
			tc.mut(&liveRow)
			merged := providerinventory.RehydrateForAutoRoute(
				providerinventory.Report{ModelCapabilities: []providerinventory.ModelCapability{liveRow}},
				providerinventory.Report{ModelCapabilities: []providerinventory.ModelCapability{durableMR}},
				now,
			)
			var hasDurable bool
			for _, m := range merged.ModelCapabilities {
				if m.CanonicalModelID == "gpt-5.5" {
					for _, s := range m.EntrySources {
						if s.SourceKind == providerinventory.CatalogSourceProviderMachineReadable {
							hasDurable = true
						}
					}
				}
			}
			if !hasDurable {
				t.Fatalf("unpresent live MR (%s) must not block durable exact+fresh MR; got %#v", tc.name, merged.ModelCapabilities)
			}
		})
	}
}

// Durable static / estimated rows must not be injected as rehydrate catalog truth.
func TestRehydrateModelsDoesNotOverlayDurableStaticOrEstimated(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	live := providerinventory.Report{
		// No production MR live catalog.
		ModelCapabilities: nil,
	}
	durable := providerinventory.Report{
		ModelCapabilities: []providerinventory.ModelCapability{
			{
				AdapterID: "codex", CanonicalModelID: "static-only",
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
			{
				AdapterID: "codex", CanonicalModelID: "estimated-mr",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceEstimated,
				EntrySources: []providerinventory.CatalogEntrySource{{
					SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
					Confidence:     providerinventory.ConfidenceEstimated,
					FreshnessState: providerinventory.FreshnessFresh,
				}},
			},
		},
	}
	merged := providerinventory.RehydrateForAutoRoute(live, durable, now)
	if len(merged.ModelCapabilities) != 0 {
		t.Fatalf("static/estimated durable models must not rehydrate as route truth: %#v", merged.ModelCapabilities)
	}
	_ = now
}

func strPtr(s string) *string { return &s }
