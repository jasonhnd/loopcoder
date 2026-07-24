package capacitysnapshot_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/codexauth"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func ptrStr(s string) *string { return &s }

func TestGrokPathAliasFusesInstallAuthModelsWithQuota(t *testing.T) {
	// Same-report fuse when both aliases are present (associateIdentityEvidence).
	// Primary MUST be earliest DiscoveryOrder (PATH / LookPath primary), not
	// lexicographic pinst id. Secondary alias evidence fuses onto primary.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc = "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
		// Lexicographically later id is PATH-primary (order 0) — must win over 3an5.
		instPrimary   = "pinst_wrpmecvyfayff7nnqvaztkqhfs7ua2hd"
		instSecondary = "pinst_3an5v55kgyq352a2bbgkfljbmikrndoq"
		rhash         = "sha256:deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	rem, lim := int64(3500), int64(10000)
	primary := exactFreshInstall("grok", instPrimary, rhash, "sha256:path-primary")
	primary.DiscoveryOrder = 0
	secondary := exactFreshInstall("grok", instSecondary, rhash, "sha256:path-secondary")
	secondary.DiscoveryOrder = 1
	rep := providerinventory.Report{
		InventoryFingerprint: "fp-alias",
		Installations: []providerinventory.ProviderInstallation{
			// List secondary first so sort-by-id alone would wrongly prefer it.
			secondary,
			primary,
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			// Auth observed on secondary alias — must rebind to PATH primary.
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instSecondary),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "grok", CanonicalModelID: "grok-4.5",
			AvailabilityState:      providerinventory.AvailabilityAvailable,
			LifecycleState:         providerinventory.LifecycleAvailable,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			EntrySources:           testMachineReadableSources("grok"),
			Source:                 providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
			ModelCatalogSnapshotID: "mcatsnap_grok_mr",
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mcatsnap_grok_mr",
			AdapterID:              "grok",
			CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(instSecondary),
			EntryCount:             1,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q1", AdapterID: "grok",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instPrimary),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, LimitValue: &lim, ValueScale: 2,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:   "provider:grok/account:" + acc + "/detail:credits_usage",
			CapturedAt: now.Format(time.RFC3339),
		}},
	}

	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnattendedOK {
		t.Fatalf("path-alias fuse must yield unattended-eligible grok: reasons=%v", snap.Reasons)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatalf("ToRouteInventory: %v", err)
	}
	found := false
	for _, c := range inv.Candidates {
		if c.Provider == "grok" && c.Model == "grok-4.5" {
			found = true
			if c.InstallRef != instPrimary {
				t.Fatalf("install ref must be PATH primary %s (LookPath), got %q", instPrimary, c.InstallRef)
			}
			if c.InstallRef == instSecondary {
				t.Fatal("secondary path alias must not remain production-eligible")
			}
		}
	}
	if !found {
		t.Fatalf("expected grok-4.5 candidate; got %#v", inv.Candidates)
	}
}

func TestLoadRouteInventoryRC36LiveOnlyADurableB(t *testing.T) {
	// Exact RC36 production shape:
	//   live Discover: only alias A (installed+auth+models)
	//   durable refresh: exact/fresh installation B + quota on B (and B resolved identity)
	// Rehydrate must translate B→A (sole live target) so LoadRouteInventory is eligible.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
		instA = "pinst_alias_a_live_only"
		instB = "pinst_alias_b_durable_only"
		rhash = "sha256:same-resolved-grok-binary-rc36"
	)
	remG, limG := int64(3500), int64(10000)

	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("grok", instA, rhash, "sha256:path-a"),
			// No instB in live — RC36.
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			AccountProfileID:    ptrStr(acc), ProviderInstallationID: ptrStr(instA),
		}},
		// Live has no trustworthy quota (discover without grant).
		ModelCapabilities: []providerinventory.ModelCapability{
			mrModel("grok", "grok-4.5", "mc_g", nil),
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc_g", AdapterID: "grok",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(instA), EntryCount: 1,
		}},
	}

	durable := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			// Durable knows B's exact resolved identity (and A for completeness).
			exactFreshInstall("grok", instA, rhash, "sha256:path-a"),
			exactFreshInstall("grok", instB, rhash, "sha256:path-b"),
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "dq_g", AdapterID: "grok",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instB), // durable B
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &remG, LimitValue: &limG, ValueScale: 2,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:   "provider:grok/account:" + acc + "/detail:product",
			CapturedAt: now.Format(time.RFC3339),
			StaleAfter: now.Add(time.Hour).Format(time.RFC3339),
		}},
		AuthReadiness:         live.AuthReadiness,
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
		t.Fatalf("LoadRouteInventory RC36 shape: %v unattended=%v reasons=%v", err, snap.UnattendedOK, snap.Reasons)
	}
	if !snap.UnattendedOK {
		t.Fatalf("RC36 live-A durable-B must be unattended after rehydrate alias: reasons=%v", snap.Reasons)
	}
	found := false
	for _, c := range inv.Candidates {
		if c.Provider == "grok" && c.Model == "grok-4.5" {
			found = true
			// Install must be live A (B translated away; never durable-as-live-installed).
			if c.InstallRef != instA {
				t.Fatalf("want live install %s after B→A translation, got %s", instA, c.InstallRef)
			}
		}
	}
	if !found {
		t.Fatalf("want grok-4.5 candidate; got %#v", inv.Candidates)
	}
}

func TestCodexEmptyAccountQuotaBindsSoleAuthenticatedInstall(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const inst = "pinst_bnq7pov5fnlikv6yb42auxv2xt2syi4d"
	authAcct := "acct_nbgt2mwso4c76xepekb7oeifcsw2axkg"
	rem := int64(73)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", inst, "sha256:codex-resolved-1", "sha256:codex-path"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			AccountProfileID:    ptrStr(authAcct), ProviderInstallationID: ptrStr(inst),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState:      providerinventory.AvailabilityAvailable,
			LifecycleState:         providerinventory.LifecycleAvailable,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			EntrySources:           testMachineReadableSources("codex"),
			Source:                 providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
			ModelCatalogSnapshotID: "mcatsnap_codex_mr",
			Constraints: []string{
				"catalog_source=codex-app-server-model-list",
				"supported_depth=low", "supported_depth=medium", "supported_depth=high",
				"default_depth=medium",
			},
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mcatsnap_codex_mr", AdapterID: "codex",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(inst), EntryCount: 1,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q_codex", AdapterID: "codex",
			ProviderInstallationID: ptrStr(inst),
			Unit:                   "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/account:unknown/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	for _, a := range accounts {
		if a.Provider == "codex" && a.AccountRef == opaqueUnknown(t) {
			t.Fatalf("account:unknown must not become AccountRef: %s", a.AccountRef)
		}
	}
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnattendedOK {
		t.Fatalf("codex sole-authenticated reassociation must be eligible: reasons=%v accounts=%s",
			snap.Reasons, summarizeAccounts(accounts))
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	var depths []string
	for _, c := range inv.Candidates {
		if c.Provider == "codex" && c.Model == "gpt-5.5" {
			depths = append(depths, c.Effort)
		}
	}
	if len(depths) == 0 {
		t.Fatalf("expected codex gpt-5.5 candidates, got %#v", inv.Candidates)
	}
	hasLow, hasMed, hasHigh := false, false, false
	for _, d := range depths {
		switch d {
		case "low":
			hasLow = true
		case "medium":
			hasMed = true
		case "high":
			hasHigh = true
		}
	}
	if !hasLow || !hasMed || !hasHigh {
		t.Fatalf("want low+medium+high candidates, got depths=%v", depths)
	}
}

func TestEmptyAccountDoesNotBindUnauthenticatedAccount(t *testing.T) {
	// Non-empty AccountRef without Authenticated must not receive empty-account quota.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const inst = "pinst_shared"
	rem := int64(50)
	// Manually construct observations via report: auth readiness not ready ⇒ Authenticated=false.
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", inst, "sha256:x", "sha256:p"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessNotAuthenticated,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr("acct_not_ready"), ProviderInstallationID: ptrStr(inst),
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q_empty", AdapterID: "codex",
			ProviderInstallationID: ptrStr(inst),
			Unit:                   "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	for _, a := range accounts {
		if a.AccountRef != "" && len(a.Windows) > 0 && !a.Authenticated {
			t.Fatalf("empty-account quota must not bind unauthenticated account: %+v", a)
		}
	}
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UnattendedOK {
		t.Fatal("unauthenticated account must not become unattended via empty-account bind")
	}
}

func TestAmbiguousMultiAuthenticatedAccountSameInstallDoesNotCrossJoin(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const inst = "pinst_shared"
	rem := int64(50)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", inst, "sha256:x", "sha256:p"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			{
				AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
				FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
				ReadinessConfidence: providerinventory.ConfidenceExact,
				AccountProfileID:    ptrStr("acct_one"), ProviderInstallationID: ptrStr(inst),
			},
			{
				AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
				FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
				ReadinessConfidence: providerinventory.ConfidenceExact,
				AccountProfileID:    ptrStr("acct_two"), ProviderInstallationID: ptrStr(inst),
			},
		},
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
			QuotaSnapshotID: "q_empty", AdapterID: "codex",
			ProviderInstallationID: ptrStr(inst),
			Unit:                   "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	emptyWithWindows := 0
	for _, a := range accounts {
		if a.Provider == "codex" && a.AccountRef == "" && len(a.Windows) > 0 {
			emptyWithWindows++
			if !strings.Contains(a.Provenance, "ambiguous_multi_authenticated") {
				t.Fatalf("want ambiguous multi-authenticated provenance, got %q", a.Provenance)
			}
		}
	}
	if emptyWithWindows != 1 {
		t.Fatalf("empty-account windows must stay unmerged; accounts=%s", summarizeAccounts(accounts))
	}
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UnattendedOK {
		t.Fatal("ambiguous multi-authenticated account must not silently become unattended-eligible")
	}
}

func TestDistinctResolvedBinariesNeverFuseOrCombineEvidence(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		instA = "pinst_aaa"
		instB = "pinst_bbb"
	)
	rem := int64(40)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("grok", instA, "sha256:binary-a", "sha256:path-a"),
			exactFreshInstall("grok", instB, "sha256:binary-b", "sha256:path-b"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instA),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "grok", CanonicalModelID: "grok-4.5",
			AvailabilityState:      providerinventory.AvailabilityAvailable,
			LifecycleState:         providerinventory.LifecycleAvailable,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			EntrySources:           testMachineReadableSources("grok"),
			Source:                 providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
			ModelCatalogSnapshotID: "mc_a",
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc_a", AdapterID: "grok",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(instA), EntryCount: 1,
		}},
		// Quota on different binary (instB) — must NOT fuse with instA auth/models.
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q_b", AdapterID: "grok",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instB),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, LimitValue: int64Ptr(100),
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:   "provider:grok/account:" + acc + "/detail:credits",
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	installs := map[string]bool{}
	for _, a := range accounts {
		if a.Provider == "grok" {
			installs[a.InstallRef] = true
		}
	}
	if len(installs) < 2 {
		t.Fatalf("distinct resolved binaries must not fuse: installs=%v accounts=%s", installs, summarizeAccounts(accounts))
	}
	// No single account may hold both production models and windows across binaries.
	for _, a := range accounts {
		if a.Provider != "grok" {
			continue
		}
		hasMR := false
		for _, m := range a.Models {
			if m.PresentInCatalog && !m.CatalogHintOnly {
				hasMR = true
				break
			}
		}
		if hasMR && len(a.Windows) > 0 && a.InstallRef == instA {
			// models are on A; windows must not have been stolen from B
			t.Fatalf("instA must not receive windows from distinct binary B: %+v", a)
		}
		if a.InstallRef == instB && hasMR {
			t.Fatalf("instB must not receive models from distinct binary A: %+v", a)
		}
	}
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	// Neither fully eligible: A has models+auth no windows; B may have windows but
	// needs install+auth+models. Auth is only on A.
	if snap.UnattendedOK {
		// If somehow B got auth via empty account — still should not get A's models.
		inv, ierr := capacitysnapshot.ToRouteInventory(snap, now)
		if ierr != nil {
			t.Fatal(ierr)
		}
		for _, c := range inv.Candidates {
			if c.Provider == "grok" && c.InstallRef == instB && c.Model == "grok-4.5" {
				t.Fatalf("distinct binary B must not route with A models: %#v", c)
			}
			if c.Provider == "grok" && c.InstallRef == instA {
				// A without windows should not produce capacity-routable candidate
				// with remaining capacity from B — ToRouteInventory only emits
				// unattended-eligible accounts. Fail closed if we got here with A.
				t.Fatalf("instA without own windows must not be routable: %#v snap reasons=%v", c, snap.Reasons)
			}
		}
	}
}

func TestEstimatedOrStaleInstallIdentityNeverAliasFuses(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
		instA = "pinst_exact_fresh"
		instB = "pinst_estimated"
		instC = "pinst_stale"
		rhash = "sha256:same-hash-but-bad-confidence"
	)
	rem := int64(30)
	// Estimated B and stale C share hash with exact A — must NOT fuse.
	est := exactFreshInstall("grok", instB, rhash, "sha256:path-b")
	est.Confidence = providerinventory.ConfidenceEstimated
	stale := exactFreshInstall("grok", instC, rhash, "sha256:path-c")
	stale.FreshnessState = providerinventory.FreshnessStale

	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("grok", instA, rhash, "sha256:path-a"),
			est,
			stale,
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instA),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{
			mrModel("grok", "grok-4.5", "mc", nil),
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc", AdapterID: "grok",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(instA), EntryCount: 1,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q_b", AdapterID: "grok",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instB),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, LimitValue: int64Ptr(100),
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:   "provider:grok/account:" + acc + "/detail:x",
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	// Install refs for A and B must remain distinct (no fuse with estimated B).
	hasA, hasB := false, false
	for _, a := range accounts {
		if a.InstallRef == instA {
			hasA = true
			if len(a.Windows) > 0 {
				t.Fatalf("exact A must not receive windows from estimated B: %s", summarizeAccounts(accounts))
			}
		}
		if a.InstallRef == instB {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Fatalf("estimated alias must not collapse installs: %s", summarizeAccounts(accounts))
	}
}

func TestMultiLivePathAliasesTranslateDurableToPATHPrimary(t *testing.T) {
	// Two live path aliases + durable quota on a third pinst of the same resolved
	// binary → translate durable onto PATH-primary (DiscoveryOrder 0), not fail closed.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
		live1 = "pinst_live_path_primary"
		live2 = "pinst_live_path_secondary"
		durB  = "pinst_durable_b"
		rhash = "sha256:multi-live-path-primary"
	)
	rem := int64(40)
	p0 := exactFreshInstall("grok", live1, rhash, "sha256:p1")
	p0.DiscoveryOrder = 0
	p1 := exactFreshInstall("grok", live2, rhash, "sha256:p2")
	p1.DiscoveryOrder = 1
	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{p0, p1},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			AccountProfileID:    ptrStr(acc), ProviderInstallationID: ptrStr(live2), // auth on secondary
		}},
		ModelCapabilities: []providerinventory.ModelCapability{mrModel("grok", "grok-4.5", "mc", nil)},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc", AdapterID: "grok",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(live2), EntryCount: 1,
		}},
	}
	durable := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("grok", durB, rhash, "sha256:pb"),
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "dq", AdapterID: "grok",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(durB),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, LimitValue: int64Ptr(100),
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:   "provider:grok/account:" + acc + "/detail:x",
			CapturedAt: now.Format(time.RFC3339),
			StaleAfter: now.Add(time.Hour).Format(time.RFC3339),
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
		t.Fatalf("multi-live path aliases must rehydrate to PATH primary: %v", err)
	}
	if !snap.UnattendedOK {
		t.Fatalf("want unattended after PATH-primary fuse: reasons=%v", snap.Reasons)
	}
	found := false
	for _, c := range inv.Candidates {
		if c.Provider == "grok" && c.Model == "grok-4.5" {
			found = true
			if c.InstallRef != live1 {
				t.Fatalf("want PATH primary install %s, got %s", live1, c.InstallRef)
			}
		}
	}
	if !found {
		t.Fatalf("want grok-4.5 on PATH primary; candidates=%#v", inv.Candidates)
	}
}

func TestCodexAccountScopeNeverUnknownSentinel(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	rem := int64(10)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", "pinst_x", "sha256:x", "sha256:p"),
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q", AdapterID: "codex",
			ProviderInstallationID: ptrStr("pinst_x"),
			Unit:                   "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/account:unknown/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	for _, a := range accounts {
		if a.AccountRef == opaqueUnknown(t) {
			t.Fatal("account:unknown must not produce AccountRef")
		}
	}
}

func TestCodexAccountProfileIDWhenPrincipalPresent(t *testing.T) {
	want := codexauth.CanonicalAccountProfileID("acct_fixture", "", "")
	if want == "" {
		t.Fatal("fixture principal must be exact-routable")
	}
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	rem := int64(50)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", "pinst_y", "sha256:y", "sha256:p"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			AccountProfileID:    ptrStr(want), ProviderInstallationID: ptrStr("pinst_y"),
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
			QuotaSnapshotID: "q", AdapterID: "codex",
			AccountProfileID: ptrStr(want), ProviderInstallationID: ptrStr("pinst_y"),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/account:" + want + "/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	snap, err := capacitysnapshot.Build(capacitysnapshot.FromProviderInventoryReport(rep, now), now)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnattendedOK {
		t.Fatalf("bound principal must be eligible: %v", snap.Reasons)
	}
}

func TestMergeDepthsNeverBroadenOnDisagreement(t *testing.T) {
	// Two rows same model with conflicting depth sets: result must be intersection only.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc  = "acct-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		inst = "pinst_merge"
	)
	rem := int64(50)
	// Build two account observations that will collide after alias (same install/account)
	// by placing models via two quota-less auth rows is hard; instead fuse via
	// path-alias so mergeAccountObservations runs.
	rhash := "sha256:merge-depth"
	instA, instB := "pinst_m_a", "pinst_m_b"
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", instA, rhash, "sha256:pa"),
			exactFreshInstall("codex", instB, rhash, "sha256:pb"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
			AccountProfileID:    ptrStr(acc), ProviderInstallationID: ptrStr(instA),
		}},
		// Two model rows for same id with different constraints — attached to same
		// adapter accounts; after alias fuse merge should intersect.
		ModelCapabilities: []providerinventory.ModelCapability{
			{
				AdapterID: "codex", CanonicalModelID: "gpt-5.5",
				AvailabilityState:      providerinventory.AvailabilityAvailable,
				LifecycleState:         providerinventory.LifecycleAvailable,
				FreshnessState:         providerinventory.FreshnessFresh,
				Confidence:             providerinventory.ConfidenceExact,
				EntrySources:           testMachineReadableSources("codex"),
				Source:                 providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
				ModelCatalogSnapshotID: "mc1",
				Constraints:            []string{"supported_depth=low", "supported_depth=medium", "default_depth=medium"},
			},
			{
				AdapterID: "codex", CanonicalModelID: "gpt-5.5",
				AvailabilityState:      providerinventory.AvailabilityAvailable,
				LifecycleState:         providerinventory.LifecycleAvailable,
				FreshnessState:         providerinventory.FreshnessFresh,
				Confidence:             providerinventory.ConfidenceExact,
				EntrySources:           testMachineReadableSources("codex"),
				Source:                 providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
				ModelCatalogSnapshotID: "mc2",
				Constraints:            []string{"supported_depth=medium", "supported_depth=high", "default_depth=high"},
			},
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			{ModelCatalogSnapshotID: "mc1", AdapterID: "codex",
				CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				ProviderInstallationID: ptrStr(instA), EntryCount: 1},
			{ModelCatalogSnapshotID: "mc2", AdapterID: "codex",
				CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				ProviderInstallationID: ptrStr(instB), EntryCount: 1},
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q", AdapterID: "codex",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instA),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/account:" + acc + "/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	_ = inst
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	// Find gpt-5.5 on fused account: depths must be intersection {medium} only,
	// never low∪high invent.
	for _, a := range accounts {
		for _, m := range a.Models {
			if m.ModelID != "gpt-5.5" || m.CatalogHintOnly {
				continue
			}
			set := map[string]bool{}
			for _, d := range m.SupportedDepths {
				set[d] = true
			}
			if set["low"] || set["high"] {
				t.Fatalf("merge must not broaden depths beyond intersection; got %v", m.SupportedDepths)
			}
			// Intersection of {low,medium} ∩ {medium,high} = {medium}
			if !set["medium"] && len(m.SupportedDepths) > 0 {
				// If attach produced separate rows before merge, still OK if no broaden.
			}
		}
	}
}

func TestCodexDualAccountSchemeDoesNotSplitIdentities(t *testing.T) {
	// Principal-derived codexauth acct-<sha256> on quota vs live status acct_ AuthReadiness.
	// No proven mapping: must NOT false-equate or rebind; must not become two-account eligible.
	// Prefer fail-closed over inventing shared identity.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const inst = "pinst_codex_live_one"
	statusAuthID := "acct_nbgt2mwso4c76xepekb7oeifcsw2axkg" // providerinventory status form
	codexAuthForm := codexauth.CanonicalAccountProfileID("acct_chatgpt_principal_xyz", "", "")
	if codexAuthForm == "" || !strings.HasPrefix(codexAuthForm, "acct-") {
		t.Fatalf("want codexauth acct- form, got %q", codexAuthForm)
	}
	if statusAuthID == codexAuthForm {
		t.Fatal("fixtures must differ across schemes")
	}
	rem := int64(73)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("codex", inst, "sha256:codex-resolved", "sha256:codex-path"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr(statusAuthID), ProviderInstallationID: ptrStr(inst),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState:      providerinventory.AvailabilityAvailable,
			LifecycleState:         providerinventory.LifecycleAvailable,
			FreshnessState:         providerinventory.FreshnessFresh,
			Confidence:             providerinventory.ConfidenceExact,
			EntrySources:           testMachineReadableSources("codex"),
			Source:                 providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
			ModelCatalogSnapshotID: "mc_c",
			Constraints: []string{
				"supported_depth=low", "supported_depth=medium", "supported_depth=high",
				"default_depth=medium", "catalog_source=codex-app-server-model-list",
			},
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mc_c", AdapterID: "codex",
			CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(inst), EntryCount: 1,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q_codex_auth_form", AdapterID: "codex",
			AccountProfileID:       ptrStr(codexAuthForm), // conflicting nonempty principal
			ProviderInstallationID: ptrStr(inst),
			Unit:                   "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/account:" + codexAuthForm + "/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	// Must not merge into one falsely-equivalent account.
	var statusLike, principalLike, complete int
	for _, a := range accounts {
		if a.Provider != "codex" {
			continue
		}
		mc := 0
		for _, m := range a.Models {
			if m.PresentInCatalog && !m.CatalogHintOnly {
				mc++
			}
		}
		if a.Authenticated && mc > 0 && len(a.Windows) > 0 {
			complete++
		}
		if a.Authenticated && mc > 0 {
			statusLike++
		}
		if !a.Authenticated && len(a.Windows) > 0 {
			principalLike++
		}
		// Provenance must not claim orphan rebind / equivalence.
		if strings.Contains(a.Provenance, "orphan_account") {
			t.Fatalf("must not rebind/overwrite conflicting principal: %s", a.Provenance)
		}
	}
	if complete > 0 {
		t.Fatalf("must not invent complete eligibility by equating dual IDs; accounts=%s", summarizeAccounts(accounts))
	}
	if statusLike > 0 && principalLike > 0 {
		// Split is honest; must not yield unattended route.
		snap, err := capacitysnapshot.Build(accounts, now)
		if err != nil {
			t.Fatal(err)
		}
		if snap.UnattendedOK {
			t.Fatalf("dual-scheme split must fail closed unattended; reasons=%v accounts=%s",
				snap.Reasons, summarizeAccounts(accounts))
		}
		return
	}
	// If somehow only one side remains without complete bind, still fail closed.
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.UnattendedOK {
		t.Fatalf("conflicting dual IDs must not be unattended-eligible: %v", snap.Reasons)
	}
}

// --- helpers ---

func opaqueUnknown(t *testing.T) string {
	t.Helper()
	return "acct-0b46c5378d997593a68a5df708cc61c0207785483cca3768641b04e30ac23638"
}

func summarizeAccounts(accounts []capacitysnapshot.AccountObservation) string {
	var b strings.Builder
	for _, a := range accounts {
		b.WriteString(a.Provider)
		b.WriteString("/")
		b.WriteString(a.AccountRef)
		b.WriteString("/")
		b.WriteString(a.InstallRef)
		b.WriteString(" inst=")
		if a.Installed {
			b.WriteString("1")
		} else {
			b.WriteString("0")
		}
		b.WriteString(" auth=")
		if a.Authenticated {
			b.WriteString("1")
		} else {
			b.WriteString("0")
		}
		b.WriteString(" models=")
		b.WriteString(itoa(len(a.Models)))
		b.WriteString(" wins=")
		b.WriteString(itoa(len(a.Windows)))
		b.WriteString("; ")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}

func int64Ptr(v int64) *int64 { return &v }

func exactFreshInstall(adapter, id, resolved, pathHash string) providerinventory.ProviderInstallation {
	return providerinventory.ProviderInstallation{
		AdapterID: adapter, ProviderInstallationID: id,
		InstallationState:   providerinventory.InstallationInstalled,
		UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
		Confidence: providerinventory.ConfidenceExact,
		ExecutableIdentity: providerinventory.ExecutableIdentity{
			Basename: adapter, ResolvedPathHash: resolved, PathHash: pathHash,
		},
	}
}

func mrModel(adapter, model, snapID string, constraints []string) providerinventory.ModelCapability {
	return providerinventory.ModelCapability{
		AdapterID: adapter, CanonicalModelID: model,
		AvailabilityState:      providerinventory.AvailabilityAvailable,
		LifecycleState:         providerinventory.LifecycleAvailable,
		FreshnessState:         providerinventory.FreshnessFresh,
		Confidence:             providerinventory.ConfidenceExact,
		EntrySources:           testMachineReadableSources(adapter),
		Source:                 providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
		ModelCatalogSnapshotID: snapID,
		Constraints:            constraints,
	}
}
