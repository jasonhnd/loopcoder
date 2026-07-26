package capacitysnapshot_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/codexauth"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// TestFromInventory_CodexSharedAuthID_SurvivesOpaqueAccountRef proves that when
// AuthReadiness carries shared codexauth acct-+64hex, FromProviderInventoryReport
// preserves it as AccountRef (no second hash), so route AccountRef matches agent
// preflight RequireMatch / ExactRouteAffirm.
func TestFromInventory_CodexSharedAuthID_SurvivesOpaqueAccountRef(t *testing.T) {
	principal := "rc38-align-principal-001"
	want := codexauth.CanonicalAccountProfileID(principal, "", "")
	if want == "" {
		t.Fatal("empty canonical")
	}
	inst := "pinst_bnq7pov5fnlikv6yb42auxv2xt2syi4d"
	acc := want
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	ptr := func(s string) *string { return &s }
	i64 := func(v int64) *int64 { return &v }

	rep := providerinventory.Report{
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID:              "codex",
			ReadinessState:         providerinventory.ReadinessReady,
			AccountProfileID:       &acc,
			ProviderInstallationID: ptr(inst),
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			ReadinessConfidence:    providerinventory.ConfidenceExact,
		}},
		// Empty-account quota rebinds to sole ready account on install.
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID:        "q-codex-weekly",
			AdapterID:              "codex",
			ProviderInstallationID: ptr(inst),
			// AccountProfileID intentionally nil — rebind path.
			WindowKind:     providerinventory.WindowFixedWeek,
			Unit:           "percent",
			QuantityKind:   providerinventory.QuantityProviderDefined,
			LimitValue:     i64(100),
			UsedValue:      i64(30),
			RemainingValue: i64(70),
			FreshnessState: providerinventory.FreshnessFresh,
			Confidence:     providerinventory.ConfidenceExact,
			CapturedAt:     now.Format(time.RFC3339),
			ResetAt:        now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID:         "codex",
			CanonicalModelID:  "codex-auto-review",
			LifecycleState:    providerinventory.LifecycleAvailable,
			AvailabilityState: providerinventory.AvailabilityAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			Constraints: []string{
				"supported_depth=low",
				"supported_depth=medium",
				"supported_depth=high",
			},
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:     providerinventory.ConfidenceExact,
				FreshnessState: providerinventory.FreshnessFresh,
			}},
		}},
		Installations: []providerinventory.ProviderInstallation{{
			ProviderInstallationID: inst,
			AdapterID:              "codex",
			InstallationState:      providerinventory.InstallationInstalled,
			UsableForInvocation:    "yes",
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
		}},
	}

	obs := capacitysnapshot.FromProviderInventoryReport(rep, now)
	var found bool
	for _, o := range obs {
		if !strings.EqualFold(o.Provider, "codex") {
			continue
		}
		if o.AccountRef == "" {
			continue
		}
		found = true
		if o.AccountRef != want {
			t.Fatalf("AccountRef=%q want shared codexauth %q", o.AccountRef, want)
		}
		if strings.HasPrefix(o.AccountRef, "acct_") {
			t.Fatalf("status acct_ leaked: %q", o.AccountRef)
		}
	}
	if !found {
		t.Fatalf("no codex account observation with AccountRef; obs=%+v", obs)
	}
}

// TestLegacyStatusID_DoesNotEqualCodexAuth documents the RC38 split: hashing a
// status-style acct_ id produces a different opaque ref than codexauth principal.
func TestLegacyStatusID_DoesNotEqualCodexAuth(t *testing.T) {
	principal := "537689fe-5e19-45f1-96f2-5f6b99373698"
	canon := codexauth.CanonicalAccountProfileID(principal, "", "")
	statusID := "acct_nbgt2mwso4c76xepekb7oeifcsw2axkg"
	inst := "pinst_test"
	acc := statusID
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	ptr := func(s string) *string { return &s }
	rep := providerinventory.Report{
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			AccountProfileID: &acc, ProviderInstallationID: ptr(inst),
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
		}},
	}
	obs := capacitysnapshot.FromProviderInventoryReport(rep, now)
	if len(obs) == 0 {
		t.Fatal("expected observation")
	}
	got := obs[0].AccountRef
	if got == canon {
		t.Fatalf("status-style id unexpectedly equals codexauth; dual-scheme detection broken")
	}
	if got == statusID {
		t.Fatalf("status acct_ must be hashed by opaqueAccountRef, got raw %q", got)
	}
	if !strings.HasPrefix(got, "acct-") || len(got) != 5+64 {
		t.Fatalf("expected opaque hash of status id, got %q", got)
	}
}
