package capacitysnapshot_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
)

func entryMachineReadable() []providerinventory.CatalogEntrySource {
	return []providerinventory.CatalogEntrySource{{
		SourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
		SourceReference: "provider-machine-readable:antigravity:agy-models",
		Precedence:      200,
		Confidence:      providerinventory.ConfidenceExact,
		FreshnessState:  providerinventory.FreshnessFresh,
	}}
}

func entryAdapterDeclared() []providerinventory.CatalogEntrySource {
	return []providerinventory.CatalogEntrySource{{
		SourceKind:      providerinventory.CatalogSourceAdapterDeclared,
		SourceReference: "loopcoder-static-registry:antigravity",
		Precedence:      100,
		Confidence:      providerinventory.ConfidenceExact, // production static is also exact+fresh
		FreshnessState:  providerinventory.FreshnessFresh,
	}}
}

func TestFromInventory_DynamicMachineReadableOnly_ExactToken(t *testing.T) {
	now := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	rem, lim := int64(50), int64(100)
	// Real production shape: static adapter-declared is also ConfidenceExact+Fresh,
	// plus dynamic machine-readable exact. Only dynamic must route.
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "antigravity", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "antigravity", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
		}},
		ModelCatalogSnapshots: []providerinventory.ModelCatalogSnapshot{
			{
				AdapterID: "antigravity", CatalogSourceKind: providerinventory.CatalogSourceAdapterDeclared,
				CatalogSourceReference: "loopcoder-static-registry:antigravity",
				Confidence:             providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			},
			{
				AdapterID: "antigravity", CatalogSourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				CatalogSourceReference: "provider-machine-readable:antigravity:agy-models",
				Confidence:             providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			},
		},
		ModelCapabilities: []providerinventory.ModelCapability{
			{
				AdapterID: "antigravity", CanonicalModelID: "GPT-OSS 120B",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceExact,
				EntrySources:      entryAdapterDeclared(),
				Source:            providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceAdapterDeclared)},
			},
			{
				AdapterID: "antigravity", CanonicalModelID: "gpt-oss-120b-medium",
				DisplayName:       "GPT-OSS 120B (Medium)",
				AvailabilityState: providerinventory.AvailabilityAvailable,
				LifecycleState:    providerinventory.LifecycleAvailable,
				FreshnessState:    providerinventory.FreshnessFresh,
				Confidence:        providerinventory.ConfidenceExact,
				Constraints:       []string{"provider=antigravity", "supported_depth=medium", "cli_model=gpt-oss-120b-medium"},
				EntrySources:      entryMachineReadable(),
				Source:            providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
			},
		},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "antigravity", Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
			RemainingValue: &rem, LimitValue: &lim,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	inv, err := capacitysnapshot.ToRouteInventory(mustBuild(t, accounts, now), now)
	if err != nil {
		t.Fatal(err)
	}
	var gptRoutes int
	for _, c := range inv.Candidates {
		if c.Provider != "antigravity" {
			continue
		}
		// Static base GPT-OSS 120B must not appear when dynamic exact is present.
		if c.Model == "GPT-OSS 120B" {
			t.Fatalf("adapter-declared static must not route when dynamic present: %+v", c)
		}
		if c.Model == "gpt-oss-120b-medium" {
			gptRoutes++
			if c.Effort != "medium" {
				t.Fatalf("depth=%q want medium only: %+v", c.Effort, c)
			}
		}
		if c.Effort == "low" || c.Effort == "high" {
			if c.Model == "gpt-oss-120b-medium" || c.Model == "GPT-OSS 120B" {
				t.Fatalf("invented depth: %+v", c)
			}
		}
	}
	if gptRoutes == 0 {
		t.Fatalf("expected exact slug route, candidates=%+v", inv.Candidates)
	}
	// Same family only one exact token route (one model id × one depth).
	if gptRoutes != 1 && gptRoutes != 2 { // 2 if write+read candidates for same model/depth
		// Count unique model|effort
		seen := map[string]bool{}
		for _, c := range inv.Candidates {
			if c.Model == "gpt-oss-120b-medium" {
				seen[c.Model+"|"+c.Effort+"|"+c.Permission] = true
			}
		}
		// Only medium depth for this model.
		for k := range seen {
			if !(k == "gpt-oss-120b-medium|medium|bounded_write" || k == "gpt-oss-120b-medium|medium|read-only") {
				t.Fatalf("unexpected route key %s in %v", k, seen)
			}
		}
	}
}

func TestFromInventory_GrokDoesNotUseAgySlugParse(t *testing.T) {
	now := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	rem, lim := int64(40), int64(100)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "grok", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "grok", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
		}},
		// Fresh `grok models` presence: model id only, no observed depth tokens
		// (production shape). Must not backfill static registry low/high/xhigh.
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "grok", CanonicalModelID: "grok-4.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind: providerinventory.CatalogSourceProviderMachineReadable,
				Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
				SourceReference: "provider-machine-readable:grok:models",
			}},
			Source: providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "grok", Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
			RemainingValue: &rem, LimitValue: &lim,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	var grok *capacitysnapshot.AccountObservation
	for i := range accounts {
		if accounts[i].Provider == "grok" {
			grok = &accounts[i]
			break
		}
	}
	if grok == nil {
		t.Fatal("missing grok account")
	}
	found := false
	for _, m := range grok.Models {
		if m.ModelID != "grok-4.5" || !m.PresentInCatalog || m.CatalogHintOnly {
			continue
		}
		found = true
		if len(m.SupportedDepths) != 1 || m.SupportedDepths[0] != "medium" {
			t.Fatalf("live grok-4.5 must be medium-only (no static ladder), depths=%v", m.SupportedDepths)
		}
		if m.DefaultDepth != "medium" {
			t.Fatalf("DefaultDepth=%q want medium", m.DefaultDepth)
		}
	}
	if !found {
		t.Fatalf("want production-routable grok-4.5 present models=%+v", grok.Models)
	}
	inv, err := capacitysnapshot.ToRouteInventory(mustBuild(t, accounts, now), now)
	if err != nil {
		t.Fatal(err)
	}
	depths := map[string]bool{}
	for _, c := range inv.Candidates {
		if c.Provider == "grok" && (c.Model == "GPT-OSS 120B" || c.Model == "GPT-OSS 120B (Medium)") {
			t.Fatalf("agy parse leaked onto grok: %+v", c)
		}
		if c.Provider == "grok" && c.Model == "grok-4.5" {
			depths[c.Effort] = true
		}
	}
	if !depths["medium"] {
		t.Fatal("want medium effort candidate for grok-4.5")
	}
	for d := range depths {
		if d != "medium" {
			t.Fatalf("static depth %q must not be production-routable for live grok-4.5: %v", d, depths)
		}
	}
}

// Live machine-readable row for a model that *does* exist in the static registry
// (gpt-5.5 → low/medium/high/xhigh). Without observed depth tokens, production
// must not backfill that ladder — only medium-only (or later explicit observed
// depths). Static fill remains CatalogHintOnly / seed display only.
func TestFromInventory_LiveMachineReadableNoDepth_NoStaticLadder(t *testing.T) {
	now := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	rem, lim := int64(40), int64(100)
	rep := providerinventory.Report{
		Installations: []providerinventory.ProviderInstallation{{
			AdapterID: "codex", InstallationState: providerinventory.InstallationInstalled,
			UsableForInvocation: "yes", FreshnessState: providerinventory.FreshnessFresh,
			Confidence: providerinventory.ConfidenceExact,
		}},
		AuthReadiness: []providerinventory.AuthReadiness{{
			AdapterID: "codex", ReadinessState: providerinventory.ReadinessReady,
			FreshnessState:      providerinventory.FreshnessFresh,
			Confidence:          providerinventory.ConfidenceExact,
			ReadinessConfidence: providerinventory.ConfidenceExact,
		}},
		ModelCapabilities: []providerinventory.ModelCapability{{
			AdapterID: "codex", CanonicalModelID: "gpt-5.5",
			AvailabilityState: providerinventory.AvailabilityAvailable,
			LifecycleState:    providerinventory.LifecycleAvailable,
			FreshnessState:    providerinventory.FreshnessFresh,
			Confidence:        providerinventory.ConfidenceExact,
			// No Constraints, no depth tokens — same shape as providers that only
			// list model ids (Codex supportedReasoningEfforts lands in a later PR).
			EntrySources: []providerinventory.CatalogEntrySource{{
				SourceKind:      providerinventory.CatalogSourceProviderMachineReadable,
				SourceReference: "provider-machine-readable:codex:models",
				Confidence:      providerinventory.ConfidenceExact,
				FreshnessState:  providerinventory.FreshnessFresh,
			}},
			Source: providerinventory.SourceDescriptor{Kind: string(providerinventory.CatalogSourceProviderMachineReadable)},
		}},
		QuotaSnapshots: []providerinventory.QuotaSnapshot{{
			AdapterID: "codex", Unit: "percent", WindowKind: providerinventory.WindowFixedHour,
			RemainingValue: &rem, LimitValue: &lim,
			Confidence: providerinventory.ConfidenceExact, FreshnessState: providerinventory.FreshnessFresh,
			CapturedAt: now.Format(time.RFC3339),
		}},
	}
	accounts := capacitysnapshot.FromProviderInventoryReport(rep, now)
	var codex *capacitysnapshot.AccountObservation
	for i := range accounts {
		if accounts[i].Provider == "codex" {
			codex = &accounts[i]
			break
		}
	}
	if codex == nil {
		t.Fatal("missing codex account")
	}
	found := false
	for _, m := range codex.Models {
		if m.ModelID != "gpt-5.5" || !m.PresentInCatalog || m.CatalogHintOnly {
			continue
		}
		found = true
		if len(m.SupportedDepths) != 1 || m.SupportedDepths[0] != "medium" {
			t.Fatalf("live gpt-5.5 with no observed depth must be medium-only (not static low/high/xhigh), depths=%v", m.SupportedDepths)
		}
	}
	if !found {
		t.Fatalf("want production-routable live gpt-5.5 models=%+v", codex.Models)
	}
	inv, err := capacitysnapshot.ToRouteInventory(mustBuild(t, accounts, now), now)
	if err != nil {
		t.Fatal(err)
	}
	depths := map[string]bool{}
	for _, c := range inv.Candidates {
		if c.Provider == "codex" && c.Model == "gpt-5.5" {
			depths[c.Effort] = true
		}
	}
	if !depths["medium"] {
		t.Fatal("want medium effort candidate for live gpt-5.5")
	}
	for d := range depths {
		if d != "medium" {
			t.Fatalf("static depth %q must not route for live gpt-5.5 with no observed depths: %v", d, depths)
		}
	}
}

func mustBuild(t *testing.T, accounts []capacitysnapshot.AccountObservation, now time.Time) capacitysnapshot.Snapshot {
	t.Helper()
	s, err := capacitysnapshot.Build(accounts, now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
