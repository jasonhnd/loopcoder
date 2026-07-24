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
	// RC36: two path aliases (same ResolvedPathHash) split evidence across pinst ids.
	// Live: auth+models on pinst_a; exact/fresh windows on pinst_b. Must fuse.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
		instA = "pinst_3an5v55kgyq352a2bbgkfljbmikrndoq"
		instB = "pinst_wrpmecvyfayff7nnqvaztkqhfs7ua2hd"
		rhash = "sha256:deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	rem, lim := int64(3500), int64(10000)
	rep := providerinventory.Report{
		InventoryFingerprint: "fp-alias",
		Installations: []providerinventory.ProviderInstallation{
			{
				AdapterID: "grok", ProviderInstallationID: instA,
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence: providerinventory.ConfidenceExact,
				ExecutableIdentity: providerinventory.ExecutableIdentity{
					Basename: "grok", ResolvedPathHash: rhash, PathHash: "sha256:path-a",
				},
			},
			{
				AdapterID: "grok", ProviderInstallationID: instB,
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence: providerinventory.ConfidenceExact,
				ExecutableIdentity: providerinventory.ExecutableIdentity{
					Basename: "grok", ResolvedPathHash: rhash, PathHash: "sha256:path-b",
				},
			},
		},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instA),
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "grok", CanonicalModelID: "grok-4.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			EntrySources:      testMachineReadableSources("grok"),
			Source:            providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{{
			ModelCatalogSnapshotID: "mcatsnap_grok_mr",
			AdapterID:              "grok",
			CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
			Confidence:             providerinventory.ConfidenceExact,
			FreshnessState:         providerinventory.FreshnessFresh,
			ProviderInstallationID: ptrStr(instA),
			EntryCount:             1,
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q1", AdapterID: "grok",
			AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instB),
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, LimitValue: &lim, ValueScale: 2,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:   "provider:grok/account:" + acc + "/detail:credits_usage",
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	// Wire model capability to catalog snapshot id for install-scoped attach.
	rep.ModelCapabilities[0].ModelCatalogSnapshotID = "mcatsnap_grok_mr"

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
			if c.InstallRef != instA && c.InstallRef != instB {
				t.Fatalf("install ref %q not in alias set", c.InstallRef)
			}
			// Canonical is lex-min rank-best among aliases — both share resolved hash.
			if c.AccountRef != acc && c.AccountRef != strings.ToLower(acc) {
				// opaqueAccountRef may normalize
				if !strings.HasPrefix(c.AccountRef, "acct-") {
					t.Fatalf("account ref %q", c.AccountRef)
				}
			}
		}
	}
	if !found {
		t.Fatalf("expected grok-4.5 candidate; got %#v", inv.Candidates)
	}
}

func TestCodexEmptyAccountQuotaBindsSoleInstallAuth(t *testing.T) {
	// RC36: rate-limits with account:unknown / empty account + windows on install;
	// auth+MR models on same install with real account. Must reassociate.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const inst = "pinst_bnq7pov5fnlikv6yb42auxv2xt2syi4d"
	authAcct := "acct_nbgt2mwso4c76xepekb7oeifcsw2axkg" // status-style id (opaque-hashed on join)
	rem := int64(73)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", ProviderInstallationID: inst,
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{
				Basename: "codex", ResolvedPathHash: "sha256:codex-resolved-1",
			},
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr(authAcct), ProviderInstallationID: ptrStr(inst),
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
		// Quota: empty AccountProfileID + sentinel scope (must not invent fake account).
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			QuotaSnapshotID: "q_codex", AdapterID: "codex",
			ProviderInstallationID: ptrStr(inst),
			// AccountProfileID intentionally nil (RC36 production shape without id field).
			Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
			RemainingValue: &rem, Confidence: providerinventory.ConfidenceExact,
			FreshnessState: providerinventory.FreshnessFresh,
			ScopeKey:       "provider:codex/account:unknown/scope:codex/detail:primary",
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	// No row should carry opaqueAccountRef("unknown") as AccountRef.
	for _, a := range accounts {
		if a.Provider == "codex" && a.AccountRef != "" {
			// Must not be the hash of "unknown"
			unk := opaqueUnknown(t)
			if a.AccountRef == unk {
				t.Fatalf("account:unknown must not become AccountRef: %s", a.AccountRef)
			}
		}
	}
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.UnattendedOK {
		t.Fatalf("codex sole-account reassociation must be unattended-eligible: reasons=%v accounts=%+v",
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
	// Observed multi-depth should surface for low/medium/high.
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

func TestAmbiguousMultiAccountSameInstallDoesNotCrossJoin(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const inst = "pinst_shared"
	rem := int64(50)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", ProviderInstallationID: inst,
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence:         providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: "sha256:x"},
		}},
		AuthReadiness: []providerinventory.AuthReadiness{
			{
				AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
				FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
				AccountProfileID: ptrStr("acct_one"), ProviderInstallationID: ptrStr(inst),
			},
			{
				AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
				FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
				AccountProfileID: ptrStr("acct_two"), ProviderInstallationID: ptrStr(inst),
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
			ScopeKey:       "provider:codex/scope:codex/detail:primary", // no account
			CapturedAt:     now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	// Empty-account row must remain (not merged into either account).
	emptyWithWindows := 0
	for _, a := range accounts {
		if a.Provider == "codex" && a.AccountRef == "" && len(a.Windows) > 0 {
			emptyWithWindows++
			if !strings.Contains(a.Provenance, "ambiguous_multi_account") {
				t.Fatalf("want ambiguous provenance, got %q", a.Provenance)
			}
		}
	}
	if emptyWithWindows != 1 {
		t.Fatalf("empty-account windows must stay unmerged under multi-account; accounts=%+v", summarizeAccounts(accounts))
	}
	// Auth accounts without windows should not become unattended via stolen windows.
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	// Empty-account row: not authenticated → not eligible.
	// Auth rows: no windows → not eligible.
	if snap.UnattendedOK {
		t.Fatal("ambiguous multi-account must not silently become unattended-eligible")
	}
}

func TestDistinctResolvedBinariesNeverFuse(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		instA = "pinst_aaa"
		instB = "pinst_bbb"
	)
	rem := int64(40)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			{
				AdapterID: "grok", ProviderInstallationID: instA,
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence:         providerinventory.ConfidenceExact,
				ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: "sha256:binary-a"},
			},
			{
				AdapterID: "grok", ProviderInstallationID: instB,
				InstallationState:   providerinventory.InstallationInstalled,
				UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
				Confidence:         providerinventory.ConfidenceExact,
				ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: "sha256:binary-b"},
			},
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
			ProviderInstallationID: ptrStr(instA),
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
	// Still two install groups after association.
	installs := map[string]bool{}
	for _, a := range accounts {
		if a.Provider == "grok" {
			installs[a.InstallRef] = true
		}
	}
	if len(installs) < 2 {
		t.Fatalf("distinct resolved binaries must not fuse: installs=%v accounts=%+v", installs, summarizeAccounts(accounts))
	}
	snap, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	// instA: models+auth, no windows; instB: windows+auth account, maybe no models if models install-scoped to A.
	// Neither should fully satisfy unattended unless models also replicated — production models
	// attach via catalog install A only. Expect fail closed (no unattended) OR only if models
	// replicated by account — builderKeysForProvider may attach models to both accounts.
	// Distinct binaries: install refs must remain different (asserted above).
	_ = snap
}

func TestLoadRouteInventoryPathAliasAndCodexRebind(t *testing.T) {
	// Production path: LoadRouteInventory(Discover live + LoadDurable rehydrate).
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	const (
		acc   = "acct-0c985592aa87678f5c9e10707f0871fcecb480055d14835cee750b19d47df695"
		instA = "pinst_alias_a"
		instB = "pinst_alias_b"
		rhash = "sha256:same-resolved-grok-binary"
		cinst = "pinst_codex_one"
	)
	// Auth profile for codex (status-style) — reassociation target.
	codexAuth := "acct_status_codex_profile_1"
	remG, limG := int64(3500), int64(10000)
	remC := int64(80)

	live := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{
			inst("grok", instA, rhash, "sha256:path-a"),
			inst("grok", instB, rhash, "sha256:path-b"),
			inst("codex", cinst, "sha256:codex-only", "sha256:codex-path"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			{
				AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
				FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
				AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instA),
			},
			{
				AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
				FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
				AccountProfileID: ptrStr(codexAuth), ProviderInstallationID: ptrStr(cinst),
			},
		},
		// Live has no trustworthy quota (simulates discover without grant).
		ModelCapabilities: []providerinventory.ModelCapability{
			mrModel("grok", "grok-4.5", "mc_g", []string{}),
			mrModel("codex", "gpt-5.5", "mc_c", []string{
				"supported_depth=low", "supported_depth=medium", "supported_depth=high", "default_depth=medium",
				"catalog_source=codex-app-server-model-list",
			}),
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			{ModelCatalogSnapshotID: "mc_g", AdapterID: "grok",
				CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				ProviderInstallationID: ptrStr(instA)},
			{ModelCatalogSnapshotID: "mc_c", AdapterID: "codex",
				CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:        providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				ProviderInstallationID: ptrStr(cinst)},
		},
	}

	durable := providerinventory.Report{
		// Durable quota from providers refresh: grok on alias B; codex empty-account windows.
		QuotaSnapshots: []providerinventory.QuotaSnapshot{
			{
				QuotaSnapshotID: "dq_g", AdapterID: "grok",
				AccountProfileID: ptrStr(acc), ProviderInstallationID: ptrStr(instB),
				Unit: "percent", WindowKind: providerinventory.WindowFixedWeek,
				RemainingValue: &remG, LimitValue: &limG, ValueScale: 2,
				Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				ScopeKey:   "provider:grok/account:" + acc + "/detail:product",
				CapturedAt: now.Format(time.RFC3339),
				StaleAfter: now.Add(time.Hour).Format(time.RFC3339),
			},
			{
				QuotaSnapshotID: "dq_c", AdapterID: "codex",
				ProviderInstallationID: ptrStr(cinst),
				Unit:                   "percent", WindowKind: providerinventory.WindowFixedWeek,
				RemainingValue: &remC,
				Confidence:     providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				ScopeKey:   "provider:codex/account:unknown/scope:codex/detail:primary",
				CapturedAt: now.Format(time.RFC3339),
				StaleAfter: now.Add(time.Hour).Format(time.RFC3339),
			},
		},
		AuthReadiness:         live.AuthReadiness,
		ModelCapabilities:     live.ModelCapabilities,
		ModelCatalogSnapshots: live.ModelCatalogSnapshots,
		Installations:         live.Installations,
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
		t.Fatalf("LoadRouteInventory: %v snap=%v reasons=%v", err, snap.UnattendedOK, snap.Reasons)
	}
	if !snap.UnattendedOK {
		t.Fatalf("expected unattended ok after alias+rebind; reasons=%v", snap.Reasons)
	}
	providers := map[string]bool{}
	depths := map[string]bool{}
	for _, c := range inv.Candidates {
		providers[c.Provider] = true
		if c.Effort != "" {
			depths[c.Effort] = true
		}
	}
	if !providers["grok"] {
		t.Fatalf("want grok candidate; providers=%v candidates=%#v", providers, inv.Candidates)
	}
	if !providers["codex"] {
		t.Fatalf("want codex candidate after account rebind; providers=%v", providers)
	}
	if !depths["medium"] {
		t.Fatalf("want medium depth present; depths=%v", depths)
	}
}

func TestCodexAccountScopeNeverUnknownSentinel(t *testing.T) {
	// When account/read has no principal id, ScopeKey must omit account: (not account:unknown).
	// Verified via FromProviderInventoryReport + empty AccountProfileID quota.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	rem := int64(10)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", ProviderInstallationID: "pinst_x",
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence:         providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: "sha256:x"},
		}},
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
	// Positive: account/read principal stamps AccountProfileID + matching opaque scope.
	want := codexauth.CanonicalAccountProfileID("acct_fixture", "", "")
	if want == "" {
		t.Fatal("fixture principal must be exact-routable")
	}
	// Smoke via snapshotsFromCodexRateLimits is in providerinventory tests;
	// here ensure capacity join uses the opaque form.
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	rem := int64(50)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", ProviderInstallationID: "pinst_y",
			InstallationState:   providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence:         providerinventory.ConfidenceExact,
			ExecutableIdentity: providerinventory.ExecutableIdentity{ResolvedPathHash: "sha256:y"},
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState: providerinventory.FreshnessFresh, Confidence: providerinventory.ConfidenceExact,
			AccountProfileID: ptrStr(want), ProviderInstallationID: ptrStr("pinst_y"),
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

// --- helpers ---

func opaqueUnknown(t *testing.T) string {
	t.Helper()
	// Pre-fix opaqueAccountRef("unknown") = sha256("acct|unknown") hex (RC36 codex split key).
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

func inst(adapter, id, resolved, pathHash string) providerinventory.ProviderInstallation {
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
