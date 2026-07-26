package capacitysnapshot_test

import (
	"context"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// Auth quality regressions for exact+fresh Ready gating and durable auth
// recovery through LoadRouteInventory (live Discover + optional durable overlay).

func aqPtr(s string) *string { return &s }

func aqExactFreshInstall(adapter, id, resolved, pathHash string) providerinventory.ProviderInstallation {
	return providerinventory.ProviderInstallation{
		AdapterID: adapter, ProviderInstallationID: id, ExecutableName: adapter,
		DiscoverySource:     providerinventory.DiscoveryPath,
		InstallationState:   providerinventory.InstallationInstalled,
		UsableForInvocation: "yes",
		FreshnessState:      providerinventory.FreshnessFresh,
		Confidence:          providerinventory.ConfidenceExact,
		ExecutableIdentity: providerinventory.ExecutableIdentity{
			Basename: adapter, ResolvedPathHash: resolved, PathHash: pathHash,
		},
	}
}

func aqExactFreshReadyAuth(adapter, acc, inst string, now time.Time) providerinventory.AuthReadiness {
	return providerinventory.AuthReadiness{
		AdapterID:              adapter,
		ReadinessState:         providerinventory.ReadinessReady,
		ReadinessConfidence:    providerinventory.ConfidenceExact,
		Confidence:             providerinventory.ConfidenceExact,
		FreshnessState:         providerinventory.FreshnessFresh,
		AccountProfileID:       aqPtr(acc),
		ProviderInstallationID: aqPtr(inst),
		CapturedAt:             now.Format(time.RFC3339),
	}
}

func aqExactFreshAuth(adapter, acc, inst string, state providerinventory.ReadinessState, now time.Time) providerinventory.AuthReadiness {
	a := aqExactFreshReadyAuth(adapter, acc, inst, now)
	a.ReadinessState = state
	return a
}

func aqMRModel(adapter, model, snapID string) providerinventory.ModelCapability {
	return providerinventory.ModelCapability{
		AdapterID: adapter, CanonicalModelID: model,
		AvailabilityState:      providerinventory.AvailabilityAvailable,
		LifecycleState:         providerinventory.LifecycleAvailable,
		FreshnessState:         providerinventory.FreshnessFresh,
		Confidence:             providerinventory.ConfidenceExact,
		ModelCatalogSnapshotID: snapID,
		EntrySources: []providerinventory.CatalogEntrySource{{
			SourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:      providerinventory.ConfidenceExact,
			FreshnessState:  providerinventory.FreshnessFresh,
			SourceReference: "provider-machine-readable:" + adapter + ":test",
		}},
		Source: providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
	}
}

func aqCatalog(adapter, snapID, inst string) providerinventory.ModelCatalogSnapshot {
	return providerinventory.ModelCatalogSnapshot{
		ModelCatalogSnapshotID: snapID,
		AdapterID:              adapter,
		CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
		Confidence:             providerinventory.ConfidenceExact,
		FreshnessState:         providerinventory.FreshnessFresh,
		ProviderInstallationID: aqPtr(inst),
		EntryCount:             1,
	}
}

func aqExactQuota(adapter, acc, inst string, rem int64, now time.Time) providerinventory.QuotaSnapshot {
	lim := int64(100)
	q := providerinventory.QuotaSnapshot{
		QuotaSnapshotID:        "q-" + adapter + "-" + inst,
		AdapterID:              adapter,
		ProviderInstallationID: aqPtr(inst),
		Unit:                   "percent",
		WindowKind:             providerinventory.WindowFixedWeek,
		RemainingValue:         &rem,
		LimitValue:             &lim,
		Confidence:             providerinventory.ConfidenceExact,
		FreshnessState:         providerinventory.FreshnessFresh,
		CapturedAt:             now.Format(time.RFC3339),
		StaleAfter:             now.Add(time.Hour).Format(time.RFC3339),
		ScopeKey:               "provider:" + adapter + "/detail:primary",
	}
	if acc != "" {
		a := acc
		q.AccountProfileID = &a
		q.ScopeKey = "provider:" + adapter + "/account:" + acc + "/detail:primary"
	}
	return q
}

// loadLiveOnly exercises the full production load path with no durable overlay.
func loadLiveOnly(t *testing.T, now time.Time, live providerinventory.Report) (capacitysnapshot.Snapshot, error) {
	t.Helper()
	_, snap, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now:                     now,
		SkipDefaultDurableStore: true,
		Discover: func(context.Context, providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
	})
	return snap, err
}

func loadLiveDurable(t *testing.T, now time.Time, live, durable providerinventory.Report) (capacitysnapshot.Snapshot, error) {
	t.Helper()
	_, snap, err := capacitysnapshot.LoadRouteInventory(context.Background(), capacitysnapshot.LoadOptions{
		Now: now,
		Discover: func(context.Context, providerinventory.Options) (providerinventory.Report, error) {
			return live, nil
		},
		LoadDurable: func(context.Context) (providerinventory.Report, error) {
			return durable, nil
		},
	})
	return snap, err
}

func TestAuthQuality_StaleReadyNotUnattended(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const acc = "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const inst = "pinst_stale_auth"
	auth := aqExactFreshReadyAuth("grok", acc, inst, now)
	auth.FreshnessState = providerinventory.FreshnessStale
	rep := providerinventory.Report{
		Installations:         []providerinventory.ProviderInstallation{aqExactFreshInstall("grok", inst, "sha256:stale-a", "sha256:p")},
		AuthReadiness:         []providerinventory.AuthReadiness{auth},
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("grok", "grok-4.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("grok", "mc", inst)},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("grok", acc, inst, 50, now)},
	}
	snap, err := loadLiveOnly(t, now, rep)
	if err == nil && snap.UnattendedOK {
		t.Fatal("stale Ready auth + exact install/quota/model must not be unattended-eligible")
	}
	if snap.UnattendedOK {
		t.Fatal("stale Ready auth must not be unattended-eligible")
	}
}

func TestAuthQuality_EstimatedAndUnknownReadyNotUnattended(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	for _, conf := range []providerinventory.Confidence{
		providerinventory.ConfidenceEstimated,
		providerinventory.ConfidenceUnknown,
	} {
		t.Run(string(conf), func(t *testing.T) {
			acc := "acct-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			inst := "pinst_" + string(conf)
			auth := aqExactFreshReadyAuth("grok", acc, inst, now)
			auth.Confidence = conf
			auth.ReadinessConfidence = conf
			rep := providerinventory.Report{
				Installations:         []providerinventory.ProviderInstallation{aqExactFreshInstall("grok", inst, "sha256:"+string(conf), "sha256:p")},
				AuthReadiness:         []providerinventory.AuthReadiness{auth},
				ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("grok", "grok-4.5", "mc")},
				ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("grok", "mc", inst)},
				QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("grok", acc, inst, 50, now)},
			}
			snap, err := loadLiveOnly(t, now, rep)
			if err == nil && snap.UnattendedOK {
				t.Fatalf("%s Ready auth must not be unattended-eligible", conf)
			}
			if snap.UnattendedOK {
				t.Fatalf("%s Ready auth must not be unattended-eligible", conf)
			}
		})
	}
}

func TestAuthQuality_LoadRoute_TwoDurableExactReadySameInstall_FailClosed(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const inst = "pinst_live_multi_auth"
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("codex", inst, "sha256:codex-live", "sha256:p"),
		},
		// No live Ready auth.
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("codex", "gpt-5.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("codex", "mc", inst)},
	}
	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: []providerinventory.AuthReadiness{
			aqExactFreshReadyAuth("codex", "acct_one_status", inst, now),
			aqExactFreshReadyAuth("codex", "acct_two_status", inst, now),
		},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("codex", "", inst, 40, now)},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
	}
	snap, err := loadLiveDurable(t, now, live, durable)
	if err == nil && snap.UnattendedOK {
		t.Fatal("two durable exact Ready accounts on same install must not select one / not unattended")
	}
	if snap.UnattendedOK {
		t.Fatal("two durable exact Ready accounts on same install must not be unattended")
	}
}

func TestAuthQuality_LoadRoute_SoleDurableExactReadyRecoversWithAlias(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct_status_sole_recover"
		instA = "pinst_live_a_recover"
		instB = "pinst_durable_b_recover"
		rhash = "sha256:recover-alias"
	)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("grok", instA, rhash, "sha256:pa"),
		},
		// No live auth — recovery must come from durable B after alias rewrite.
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("grok", "grok-4.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("grok", "mc", instA)},
	}
	durable := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("grok", instA, rhash, "sha256:pa"),
			aqExactFreshInstall("grok", instB, rhash, "sha256:pb"),
		},
		// Sole durable auth on instB so rewriteAuthInstallIDs B→live A is exercised.
		AuthReadiness: []providerinventory.AuthReadiness{
			aqExactFreshReadyAuth("grok", acc, instB, now),
		},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("grok", acc, instB, 55, now)},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
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
		t.Fatalf("sole durable exact Ready + alias should recover: %v reasons=%v", err, snap.Reasons)
	}
	if !snap.UnattendedOK {
		t.Fatalf("want unattended ok; reasons=%v", snap.Reasons)
	}
	found := false
	for _, c := range inv.Candidates {
		if c.Provider == "grok" && c.Model == "grok-4.5" {
			found = true
			if c.InstallRef != instA {
				t.Fatalf("want live A after alias, got %s", c.InstallRef)
			}
		}
	}
	if !found {
		t.Fatalf("want grok candidate: %#v", inv.Candidates)
	}
}

// --- Durable auth truth (Load does not recompute auth FreshnessState) ---

func TestAuthQuality_LoadRoute_LiveExactFreshNotAuthenticatedBlocksDurableReady(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_live_neg_blocks"
		inst = "pinst_live_neg"
	)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("codex", inst, "sha256:live-neg", "sha256:p"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			aqExactFreshAuth("codex", acc, inst, providerinventory.ReadinessNotAuthenticated, now),
		},
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("codex", "gpt-5.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("codex", "mc", inst)},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("codex", acc, inst, 60, now)},
	}
	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: []providerinventory.AuthReadiness{
			aqExactFreshReadyAuth("codex", acc, inst, now.Add(-5*time.Minute)),
		},
		QuotaSnapshots:        live.QuotaSnapshots,
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
	}
	snap, _ := loadLiveDurable(t, now, live, durable)
	if snap.UnattendedOK {
		t.Fatal("live exact+fresh NotAuthenticated must block durable Ready recovery")
	}
}

func TestAuthQuality_LoadRoute_OldDurableReadyBeyondHorizonNotRecovered(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_old_durable"
		inst = "pinst_old_dur"
	)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("codex", inst, "sha256:old-dur", "sha256:p"),
		},
		// No live auth.
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("codex", "gpt-5.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("codex", "mc", inst)},
	}
	oldReady := aqExactFreshReadyAuth("codex", acc, inst, now.Add(-31*time.Minute))
	durable := providerinventory.Report{
		Installations:         live.Installations,
		AuthReadiness:         []providerinventory.AuthReadiness{oldReady},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("codex", acc, inst, 60, now)},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
	}
	snap, _ := loadLiveDurable(t, now, live, durable)
	if snap.UnattendedOK {
		t.Fatal("durable Ready older than 30m recovery horizon must not be unattended")
	}
}

func TestAuthQuality_LoadRoute_NewerDurableNotAuthenticatedBlocksOlderReady(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_newer_not_auth"
		inst = "pinst_newer_na"
	)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("codex", inst, "sha256:newer-na", "sha256:p"),
		},
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("codex", "gpt-5.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("codex", "mc", inst)},
	}
	olderReady := aqExactFreshReadyAuth("codex", acc, inst, now.Add(-20*time.Minute))
	newerNA := aqExactFreshAuth("codex", acc, inst, providerinventory.ReadinessNotAuthenticated, now.Add(-2*time.Minute))
	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: []providerinventory.AuthReadiness{
			olderReady,
			newerNA,
		},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("codex", acc, inst, 60, now)},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
	}
	snap, _ := loadLiveDurable(t, now, live, durable)
	if snap.UnattendedOK {
		t.Fatal("newer durable NotAuthenticated must block older Ready recovery")
	}
}

func TestAuthQuality_LoadRoute_EqualTimeConflictingDurableAuthFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_equal_conflict"
		inst = "pinst_eq_conflict"
	)
	captured := now.Add(-3 * time.Minute)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("codex", inst, "sha256:eq-conflict", "sha256:p"),
		},
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("codex", "gpt-5.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("codex", "mc", inst)},
	}
	ready := aqExactFreshReadyAuth("codex", acc, inst, captured)
	notAuth := aqExactFreshAuth("codex", acc, inst, providerinventory.ReadinessNotAuthenticated, captured)
	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: []providerinventory.AuthReadiness{
			ready,
			notAuth,
		},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("codex", acc, inst, 60, now)},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
	}
	snap, _ := loadLiveDurable(t, now, live, durable)
	if snap.UnattendedOK {
		t.Fatal("equal-time Ready vs NotAuthenticated must fail closed")
	}
}

func TestAuthQuality_LoadRoute_RecentSoleDurableReadyStillRecovers(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_recent_sole"
		inst = "pinst_recent_sole"
	)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("codex", inst, "sha256:recent-sole", "sha256:p"),
		},
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("codex", "gpt-5.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("codex", "mc", inst)},
	}
	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: []providerinventory.AuthReadiness{
			aqExactFreshReadyAuth("codex", acc, inst, now.Add(-10*time.Minute)),
		},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("codex", acc, inst, 60, now)},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
	}
	snap, err := loadLiveDurable(t, now, live, durable)
	if err != nil {
		t.Fatalf("recent sole durable Ready should recover: %v reasons=%v", err, snap.Reasons)
	}
	if !snap.UnattendedOK {
		t.Fatalf("want unattended ok; reasons=%v", snap.Reasons)
	}
}

func TestAuthQuality_LoadRoute_InvalidCapturedAtBlocksOlderReady(t *testing.T) {
	// Same identity: recent exact+fresh Ready plus a durable row with empty
	// CapturedAt must not recover (no fallback past invalid history).
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_invalid_cap"
		inst = "pinst_invalid_cap"
	)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("codex", inst, "sha256:invalid-cap", "sha256:p"),
		},
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("codex", "gpt-5.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("codex", "mc", inst)},
	}
	olderReady := aqExactFreshReadyAuth("codex", acc, inst, now.Add(-5*time.Minute))
	invalidCap := aqExactFreshReadyAuth("codex", acc, inst, now)
	invalidCap.CapturedAt = ""
	durable := providerinventory.Report{
		Installations: live.Installations,
		AuthReadiness: []providerinventory.AuthReadiness{
			olderReady,
			invalidCap,
		},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("codex", acc, inst, 60, now)},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
	}
	snap, _ := loadLiveDurable(t, now, live, durable)
	if snap.UnattendedOK {
		t.Fatal("invalid/empty CapturedAt on same identity must block older Ready recovery")
	}
}

func TestAuthQuality_LoadRoute_MateriallyFutureCapturedAtNotRecovered(t *testing.T) {
	// Sole durable exact+fresh Ready with CapturedAt beyond authCaptureFutureSkew
	// (2m) must not recover.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct_future_cap"
		inst = "pinst_future_cap"
	)
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			aqExactFreshInstall("codex", inst, "sha256:future-cap", "sha256:p"),
		},
		ModelCapabilities:     []providerinventory.ModelCapability{aqMRModel("codex", "gpt-5.5", "mc")},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{aqCatalog("codex", "mc", inst)},
	}
	// 3m ahead of now exceeds the 2m material-future skew.
	futureReady := aqExactFreshReadyAuth("codex", acc, inst, now.Add(3*time.Minute))
	durable := providerinventory.Report{
		Installations:         live.Installations,
		AuthReadiness:         []providerinventory.AuthReadiness{futureReady},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{aqExactQuota("codex", acc, inst, 60, now)},
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
	}
	snap, _ := loadLiveDurable(t, now, live, durable)
	if snap.UnattendedOK {
		t.Fatal("CapturedAt materially future of now must not recover durable Ready")
	}
}
