package capacitysnapshot_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// Production-shaped regression: providers refresh stores durable exact+fresh Grok
// Ready auth + quota, then grantless Discover emits not-run / network-not-granted
// auth that must NOT suppress durable recovery. Path-alias dual installs + shared
// account must still yield unattended hard-eligible grok/grok-4.5 medium.

func grokGrantlessLiveAuth(adapter, inst string, now time.Time) providerinventory.AuthReadiness {
	// Shape emitted by inspectAuthCommand when NetworkDeclared && !NetworkGranted:
	// EvidenceNotRun, unknown readiness/confidence, network-permission-denied gaps.
	// Must NOT dominate durable exact+fresh Ready recovery.
	return providerinventory.AuthReadiness{
		AdapterID:              adapter,
		ReadinessState:         providerinventory.ReadinessUnknown,
		ReadinessConfidence:    providerinventory.ConfidenceUnknown,
		Confidence:             providerinventory.ConfidenceUnknown,
		FreshnessState:         providerinventory.FreshnessFresh,
		EvidenceKind:           providerinventory.EvidenceNotRun,
		ProviderInstallationID: aqPtr(inst),
		// No AccountProfileID — grantless not-run has no collected identity.
		GapReasons:        []string{"network-permission-denied", "not-collected"},
		TerminalErrorCode: "ErrNetworkPermissionDenied",
		CapturedAt:        now.Format(time.RFC3339),
		SideEffectClass:   "not-run",
	}
}

func TestGrokDurableAuth_LoadRoute_GrantlessLiveDoesNotSuppressReady(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	const (
		acc   = "acct_grok_shared_exact"
		instA = "pinst_grok_live_primary" // LookPath-primary
		instB = "pinst_grok_live_alias"   // second PATH alias, same resolved binary
		rhash = "sha256:grok-resolved-binary-exact"
		// Durable may have been captured under a different pinst id for the same resolved hash.
		instDurable = "pinst_grok_refresh_alias"
	)
	// Two live PATH aliases of the same exact/fresh resolved Grok binary.
	// DiscoveryOrder 0 = LookPath primary.
	// Real grantless Discover shape: installed exact+fresh but usable still unknown
	// because promoteUsableInstallations ran before durable Ready rehydrate.
	liveInstA := aqExactFreshInstall("grok", instA, rhash, "sha256:path-a")
	liveInstA.DiscoveryOrder = 0
	liveInstA.UsableForInvocation = "unknown"
	liveInstB := aqExactFreshInstall("grok", instB, rhash, "sha256:path-b")
	liveInstB.DiscoveryOrder = 1
	liveInstB.UsableForInvocation = "unknown"
	// Grantless Discover auth on primary: not-run / network-not-granted / not-collected.
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{liveInstA, liveInstB},
		AuthReadiness: []providerinventory.AuthReadiness{
			grokGrantlessLiveAuth("grok", instA, now),
		},
		// Live catalog may be grant-denied unavailable; durable MR exact+fresh fills.
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc-live-denied",
			AdapterID:              "grok",
			CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:             providerinventory.ConfidenceUnavailable,
			FreshnessState:         providerinventory.FreshnessNotApplicable,
			ProviderInstallationID: aqPtr(instA),
			GapReasons:             []string{"network-permission-denied", "not-collected"},
			TerminalErrorCode:      "ErrNetworkPermissionDenied",
		}},
		// Live quota grantless unavailable — durable exact+fresh fixed-week recovers.
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID:        "q-live-grantless",
			AdapterID:              "grok",
			ProviderInstallationID: aqPtr(instA),
			Unit:                   "percent",
			WindowKind:             providerinventory.WindowFixedWeek,
			Confidence:             providerinventory.ConfidenceUnavailable,
			FreshnessState:         providerinventory.FreshnessNotApplicable,
			GapReasons:             []string{"quota-collection-not-granted", "not-collected"},
			TerminalErrorCode:      "ErrQuotaCollectionGrantRequired",
			CapturedAt:             now.Format(time.RFC3339),
		}},
	}

	// Durable from providers refresh: Ready auth on both PATH aliases (real shape),
	// exact+fresh grok-4.5 catalog + fixed-week quota bound to primary install.
	rem := int64(23)
	lim := int64(100)
	// Real refresh stamps Ready on each discovered PATH install (shared account).
	durAuthPrimary := aqExactFreshReadyAuth("grok", acc, instA, now.Add(-3*time.Minute))
	durAuthAlias := aqExactFreshReadyAuth("grok", acc, instDurable, now.Add(-3*time.Minute))
	durable := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			func() providerinventory.ProviderInstallation {
				i := aqExactFreshInstall("grok", instA, rhash, "sha256:path-a")
				i.DiscoveryOrder = 0
				i.UsableForInvocation = "yes" // durable snapshot after refresh promote
				return i
			}(),
			func() providerinventory.ProviderInstallation {
				i := aqExactFreshInstall("grok", instDurable, rhash, "sha256:path-d")
				i.DiscoveryOrder = 1
				i.UsableForInvocation = "yes"
				return i
			}(),
		},
		AuthReadiness: []providerinventory.AuthReadiness{durAuthPrimary, durAuthAlias},
		ModelCapabilities: []providerinventory.ModelCapability{
			aqMRModel("grok", "grok-4.5", "mc-grok-refresh"),
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			aqCatalog("grok", "mc-grok-refresh", instA),
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID:        "q-grok-refresh",
			QuotaSourceID:          "src-grok-refresh",
			AdapterID:              "grok",
			ProviderInstallationID: aqPtr(instA),
			AccountProfileID:       aqPtr(acc),
			Unit:                   "percent",
			WindowKind:             providerinventory.WindowFixedWeek,
			RemainingValue:         &rem,
			LimitValue:             &lim,
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			CapturedAt:             now.Add(-3 * time.Minute).Format(time.RFC3339),
			StaleAfter:             now.Add(time.Hour).Format(time.RFC3339),
			ResetAt:                now.Add(48 * time.Hour).Format(time.RFC3339),
			ScopeKey:               "provider:grok/account:" + acc + "/detail:primary",
		}},
		QuotaTelemetrySources: []providerinventory.QuotaTelemetrySource{{
			AdapterID:     "grok",
			QuotaSourceID: "src-grok-refresh",
			SourceKind:    providerinventory.QuotaSourceOfficialCLICommand,
		}},
	}

	inv, snap, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now: now,
		Discover: func(context.Context, providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
		LoadDurable: func(context.Context) (providerinventory.Report, error) {
			return durable, nil
		},
	})
	if err != nil {
		t.Fatalf("LoadRouteInventory: %v reasons=%v", err, snap.Reasons)
	}
	if !snap.UnattendedOK {
		t.Fatalf("want unattended hard-eligible after durable auth recovery; reasons=%v accounts=%d",
			snap.Reasons, len(snap.Accounts))
	}
	// PATH-primary account must be Authenticated (recovered Ready + re-promote usable).
	var primaryAuth, aliasAuth bool
	for _, a := range snap.Accounts {
		if a.Provider != "grok" {
			continue
		}
		if a.InstallRef == instA && a.Authenticated {
			primaryAuth = true
		}
		// Alias must not become an independent unattended production route.
		if a.InstallRef == instB && a.Authenticated && productionEligibleAccount(a) {
			t.Fatalf("alias install must not be production-authenticated independently: %+v", a)
		}
		if a.InstallRef == instB {
			aliasAuth = a.Authenticated
		}
	}
	if !primaryAuth {
		t.Fatalf("PATH-primary install must authenticate after re-promote; accounts=%+v", snap.Accounts)
	}
	_ = aliasAuth
	found := false
	for _, c := range inv.Candidates {
		if c.Provider != "grok" || c.Model != "grok-4.5" {
			continue
		}
		found = true
		if c.InstallRef != instA {
			t.Fatalf("want primary install %s after alias rewrite, got %s", instA, c.InstallRef)
		}
		if strings.TrimSpace(c.AccountRef) == "" {
			t.Fatal("want exact-routable account identity on candidate")
		}
	}
	if !found {
		for _, s := range inv.Soft {
			if s.Provider == "grok" && s.Model == "grok-4.5" {
				found = true
				if s.InstallRef != instA {
					t.Fatalf("soft install want %s got %s", instA, s.InstallRef)
				}
				break
			}
		}
	}
	if !found {
		t.Fatalf("want unattended grok/grok-4.5 candidate; candidates=%#v soft=%#v", inv.Candidates, inv.Soft)
	}
}

// productionEligibleAccount mirrors unattended requirements used in snapshot reasons.
func productionEligibleAccount(a capacitysnapshot.AccountObservation) bool {
	return a.Installed && a.Authenticated && a.Healthy
}

func TestGrokDurableAuth_LoadRoute_LiveExactFreshNotAuthenticatedBlocksReady(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_grok_live_na"
		inst = "pinst_grok_live_na"
	)
	// Genuinely collected live exact+fresh NotAuthenticated MUST dominate.
	liveNA := aqExactFreshAuth("grok", acc, inst, providerinventory.ReadinessNotAuthenticated, now)
	liveNA.EvidenceKind = providerinventory.EvidenceStatusCommand
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("grok", inst, "sha256:grok-na", "sha256:p"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{liveNA},
		ModelCapabilities: []providerinventory.ModelCapability{
			aqMRModel("grok", "grok-4.5", "mc-na"),
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			aqCatalog("grok", "mc-na", inst),
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{
			aqExactQuota("grok", acc, inst, 40, now),
		},
	}
	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: []providerinventory.AuthReadiness{
			aqExactFreshReadyAuth("grok", acc, inst, now.Add(-5*time.Minute)),
		},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
		QuotaSnapshots:        live.QuotaSnapshots,
	}
	snap, _ := loadLiveDurable(t, now, live, durable)
	if snap.UnattendedOK {
		t.Fatal("live exact+fresh NotAuthenticated must block durable Ready recovery")
	}
}

func TestGrokDurableAuth_LoadRoute_LiveExactFreshExpiredBlocksReady(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_grok_live_exp"
		inst = "pinst_grok_live_exp"
	)
	liveExp := aqExactFreshAuth("grok", acc, inst, providerinventory.ReadinessExpired, now)
	liveExp.EvidenceKind = providerinventory.EvidenceStatusCommand
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("grok", inst, "sha256:grok-exp", "sha256:p"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{liveExp},
		ModelCapabilities: []providerinventory.ModelCapability{
			aqMRModel("grok", "grok-4.5", "mc-exp"),
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			aqCatalog("grok", "mc-exp", inst),
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{
			aqExactQuota("grok", acc, inst, 40, now),
		},
	}
	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: []providerinventory.AuthReadiness{
			aqExactFreshReadyAuth("grok", acc, inst, now.Add(-4*time.Minute)),
		},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
		QuotaSnapshots:        live.QuotaSnapshots,
	}
	snap, _ := loadLiveDurable(t, now, live, durable)
	if snap.UnattendedOK {
		t.Fatal("live exact+fresh Expired must block durable Ready recovery")
	}
}
