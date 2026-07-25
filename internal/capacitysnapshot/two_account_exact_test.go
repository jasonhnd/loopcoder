package capacitysnapshot_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

// TestFromProviderInventory_TwoGrokAccountsOnePATHInstallSameModel proves that two
// accounts for one provider share the single LookPath-primary CLI installation,
// keep distinct AccountRefs/auth/quota windows (no cross-account merge), and both
// remain production-routable against that one install. Distinct physical installs
// of the same runner command are not simultaneously routable (first-LookPath-wins).
func TestFromProviderInventory_TwoGrokAccountsOnePATHInstallSameModel(t *testing.T) {
	now := time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC)
	acc1, acc2 := "billing-alice-uuid-aaaaaaaa", "billing-bob-uuid-bbbbbbbb"
	// Single actual PATH-primary install for runner command "grok".
	const inst = "pinst_grok_path_primary_shared"
	modelID := "grok-4"
	capID := "mc-grok-4-shared"
	snapID := "mcs-grok-dyn"

	ptr := func(s string) *string { return &s }
	i64 := func(v int64) *int64 { return &v }

	rep := providerinventory.Report{
		InventoryFingerprint: "fp-two-grok",
		Installations: []providerinventory.ProviderInstallation{
			exactFreshInstall("grok", inst, "sha256:grok-primary-resolved", "sha256:grok-primary-path"),
		},
		AuthReadiness: []providerinventory.AuthReadiness{
			{
				AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
				AccountProfileID: ptr(acc1), ProviderInstallationID: ptr(inst),
				FreshnessState:      providerinventory.FreshnessFresh,
				Confidence:          providerinventory.ConfidenceExact,
				ReadinessConfidence: providerinventory.ConfidenceExact,
			},
			{
				AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
				AccountProfileID: ptr(acc2), ProviderInstallationID: ptr(inst),
				FreshnessState:      providerinventory.FreshnessFresh,
				Confidence:          providerinventory.ConfidenceExact,
				ReadinessConfidence: providerinventory.ConfidenceExact,
			},
		},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			{
				ModelCatalogSnapshotID: snapID, AdapterID: "grok",
				CatalogSourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
				Confidence:             providerinventory.ConfidenceExact,
				FreshnessState:         providerinventory.FreshnessFresh,
				ProviderInstallationID: ptr(inst),
			},
		},
		ModelCapabilities: []providerinventory.ModelCapability{
			{
				ModelCapabilityID: capID, ModelCatalogSnapshotID: snapID,
				AdapterID: "grok", CanonicalModelID: modelID,
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceExact,
				EntrySources: []providerinventory.CatalogEntrySource{{
					SourceKind:     providerinventory.CatalogSourceProviderMachineReadable,
					Confidence:     providerinventory.ConfidenceExact,
					FreshnessState: providerinventory.FreshnessFresh,
				}},
			},
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{
			{
				QuotaSnapshotID: "q-alice-5h", AdapterID: "grok",
				AccountProfileID: ptr(acc1), ProviderInstallationID: ptr(inst),
				ScopeKey:   "provider:grok/account:" + acc1 + "/detail:credits_usage",
				WindowKind: providerinventory.WindowFixedHour,
				Unit:       "percent", QuantityKind: providerinventory.QuantityProviderDefined,
				LimitValue: i64(100), UsedValue: i64(10), RemainingValue: i64(90),
				Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				CapturedAt: now.Format(time.RFC3339),
			},
			{
				QuotaSnapshotID: "q-bob-week", AdapterID: "grok",
				AccountProfileID: ptr(acc2), ProviderInstallationID: ptr(inst),
				ScopeKey:   "provider:grok/account:" + acc2 + "/detail:credits_usage",
				WindowKind: providerinventory.WindowFixedWeek,
				Unit:       "percent", QuantityKind: providerinventory.QuantityProviderDefined,
				LimitValue: i64(100), UsedValue: i64(70), RemainingValue: i64(30),
				Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				CapturedAt: now.Format(time.RFC3339),
			},
		},
	}

	// Determinism: two runs identical.
	a1 := capacitysnapshot.FromProviderInventoryReport(rep, now)
	a2 := capacitysnapshot.FromProviderInventoryReport(rep, now)
	if len(a1) != len(a2) {
		t.Fatalf("nondeterministic length %d vs %d", len(a1), len(a2))
	}
	for i := range a1 {
		if a1[i].AccountRef != a2[i].AccountRef || a1[i].InstallRef != a2[i].InstallRef {
			t.Fatalf("nondeterministic order/content at %d", i)
		}
	}

	// Exactly two Grok accounts on the single PATH-primary install (no truncation).
	type key struct{ acc, inst string }
	seen := map[key]capacitysnapshot.AccountObservation{}
	for _, o := range a1 {
		if !strings.EqualFold(o.Provider, "grok") {
			continue
		}
		if o.AccountRef == "" || o.InstallRef == "" {
			continue
		}
		if o.InstallRef != inst {
			t.Fatalf("install must be shared PATH primary %q, got %q", inst, o.InstallRef)
		}
		// Account must be full opaque (acct-+64hex), not shortRef 12-char.
		if !strings.HasPrefix(o.AccountRef, "acct-") || len(o.AccountRef) != 5+64 {
			t.Fatalf("account not full opaque: %q len=%d", o.AccountRef, len(o.AccountRef))
		}
		seen[key{o.AccountRef, o.InstallRef}] = o
	}
	if len(seen) != 2 {
		var ks []string
		for k := range seen {
			ks = append(ks, k.acc+"|"+k.inst)
		}
		t.Fatalf("want 2 exact account×install rows on one install got %d (%+v)", len(seen), ks)
	}

	// Distinct windows and quota — no cross-wire / no cross-account merge.
	var winKinds []string
	var rems []float64
	accInstalls := map[string]string{}
	for _, o := range seen {
		accInstalls[o.AccountRef] = o.InstallRef
		if len(o.Windows) == 0 {
			t.Fatalf("account %s missing windows", o.AccountRef)
		}
		for _, w := range o.Windows {
			winKinds = append(winKinds, string(w.Kind))
			if f := capacitysnapshot.RemainingFraction(w); f != nil {
				rems = append(rems, *f)
			}
		}
		// Same model present on both without collapse.
		foundModel := false
		for _, m := range o.Models {
			if strings.EqualFold(m.ModelID, modelID) && !m.CatalogHintOnly {
				foundModel = true
			}
		}
		if !foundModel {
			t.Fatalf("account %s missing model %s models=%+v", o.AccountRef, modelID, o.Models)
		}
	}
	// Both accounts must share the same production install.
	var sharedInst string
	for _, ir := range accInstalls {
		if sharedInst == "" {
			sharedInst = ir
		} else if ir != sharedInst {
			t.Fatalf("accounts must share one PATH install; got %v", accInstalls)
		}
	}
	// Distinct window kinds (fixed_hour vs fixed_week / weekly).
	kindSet := map[string]bool{}
	for _, k := range winKinds {
		kindSet[strings.ToLower(k)] = true
	}
	if len(kindSet) < 2 {
		t.Fatalf("windows collapsed: %v", winKinds)
	}
	// Distinct remaining fractions (0.9 vs 0.3).
	if len(rems) < 2 || rems[0] == rems[1] {
		t.Fatalf("quota collapsed: %v", rems)
	}

	// Build → ToRouteInventory → two exact route candidates (two accounts, one install).
	snap, err := capacitysnapshot.Build(a1, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatalf("ToRouteInventory: %v unattended=%v reasons=%v", err, snap.UnattendedOK, snap.Reasons)
	}
	cands := 0
	accs := map[string]bool{}
	installs := map[string]bool{}
	for _, c := range inv.Candidates {
		if !strings.EqualFold(c.Provider, "grok") || !strings.EqualFold(c.Model, modelID) {
			continue
		}
		if c.AccountRef == "" || c.WindowKind == "" {
			continue
		}
		cands++
		accs[c.AccountRef] = true
		if c.InstallRef != "" {
			installs[c.InstallRef] = true
		}
	}
	if cands < 2 || len(accs) < 2 {
		t.Fatalf("want ≥2 exact route candidates across 2 accounts; cands=%d accs=%d", cands, len(accs))
	}
	if len(installs) != 1 || !installs[inst] {
		t.Fatalf("want single PATH-primary install %q on candidates; installs=%v", inst, installs)
	}
}
