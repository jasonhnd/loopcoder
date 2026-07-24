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

func strPtr(s string) *string { return &s }
