package capacitysnapshot_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/codexquota"
)

func t0() time.Time { return time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC) }

func healthyAccount(provider, model string) capacitysnapshot.AccountInput {
	reset := t0().Add(2 * time.Hour)
	return capacitysnapshot.AccountInput{
		Provider: provider, AccountRef: "acct-" + provider, InstallRef: "install-1",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact,
		HealthFreshness:  capacitysnapshot.FreshnessFresh,
		Source:           "fixture",
		CapturedAt:       t0(),
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 40, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 60, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			ResetAt:    &reset,
			Confidence: capacitysnapshot.ConfidenceEstimated,
			Freshness:  capacitysnapshot.FreshnessFresh,
			Source:     "fixture",
			CapturedAt: t0(),
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: model, SupportedDepths: []string{"low", "medium", "high"},
			DefaultDepth: "medium", ClassHint: "tera", Present: true,
		}},
	}
}

func TestBuildDigestStableAndUnattendedOK(t *testing.T) {
	a := capacitysnapshot.FromAccountInput(healthyAccount("codex", "gpt-5.5"))
	b := capacitysnapshot.FromAccountInput(healthyAccount("claude", "claude-sonnet-4-5"))
	s1, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{a, b}, t0())
	if err != nil {
		t.Fatal(err)
	}
	s2, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{b, a}, t0())
	if err != nil {
		t.Fatal(err)
	}
	if !s1.UnattendedOK || s1.Digest == "" {
		t.Fatalf("%+v", s1)
	}
	if s1.Digest != s2.Digest {
		t.Fatalf("digest not order-stable: %s vs %s", s1.Digest, s2.Digest)
	}
}

func TestUnknownRemainingNeverZeroOrFull(t *testing.T) {
	in := healthyAccount("grok", "grok-4.5")
	in.Windows = []capacitysnapshot.Window{{
		Kind: "five_hour", Unit: capacitysnapshot.UnitUnknown,
		Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyUnknown},
		Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyUnknown, Value: 999}, // poison numeric
		Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyUnknown},
		Confidence: capacitysnapshot.ConfidenceUnknown,
		Freshness:  capacitysnapshot.FreshnessFresh,
		Source:     "fixture",
		CapturedAt: t0(),
	}}
	obs := capacitysnapshot.FromAccountInput(in)
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{obs}, t0())
	if err != nil {
		t.Fatal(err)
	}
	// Unknown-only → not unattended eligible
	if s.UnattendedOK {
		t.Fatal("unknown-only capacity must not satisfy unattended routing")
	}
	w := s.Accounts[0].Windows[0]
	if w.Remaining.Class != capacitysnapshot.QtyUnknown {
		t.Fatalf("remaining class=%s", w.Remaining.Class)
	}
	if w.Remaining.Value != 0 {
		t.Fatalf("unknown remaining must not keep fabricated numeric: %v", w.Remaining.Value)
	}
	if f := capacitysnapshot.RemainingFraction(w); f != nil {
		t.Fatalf("unknown remaining fraction must be nil, got %v", *f)
	}
}

func TestStaleObservationFailsUnattended(t *testing.T) {
	in := healthyAccount("claude", "claude-haiku-4-5")
	in.HealthFreshness = capacitysnapshot.FreshnessStale
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		capacitysnapshot.FromAccountInput(in),
	}, t0())
	if err != nil {
		t.Fatal(err)
	}
	if s.UnattendedOK {
		t.Fatal("stale health must not be unattended-ok")
	}
	_, err = capacitysnapshot.ToRouteInventory(s, t0())
	if err == nil {
		t.Fatal("ToRouteInventory must fail closed on non-unattended snapshot")
	}
}

func TestAbsentCatalogModelNotSelectable(t *testing.T) {
	in := healthyAccount("codex", "ghost-model")
	in.Models = []capacitysnapshot.ModelSpec{
		{ModelID: "ghost-model", Present: false, SupportedDepths: []string{"high"}},
		{ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"medium"}, DefaultDepth: "medium", ClassHint: "tera"},
	}
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		capacitysnapshot.FromAccountInput(in),
	}, t0())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Accounts[0].Models {
		if m.ModelID == "ghost-model" {
			t.Fatal("absent catalog model must be dropped")
		}
	}
	inv, err := capacitysnapshot.ToRouteInventory(s, t0())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range inv.Candidates {
		if c.Model == "ghost-model" {
			t.Fatal("ghost model must not be a route candidate")
		}
	}
}

func TestCredentialMaterialRejected(t *testing.T) {
	in := healthyAccount("codex", "gpt-5.5")
	in.AccountRef = "sk-abc123secret"
	_, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		capacitysnapshot.FromAccountInput(in),
	}, t0())
	if err == nil {
		t.Fatal("expected credential rejection")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Fatalf("err=%v", err)
	}
}

func TestToRouteInventoryFeedsAutoroute(t *testing.T) {
	a := capacitysnapshot.FromAccountInput(healthyAccount("codex", "gpt-5.5"))
	b := capacitysnapshot.FromAccountInput(healthyAccount("grok", "grok-4.5"))
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{a, b}, t0())
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(s, t0())
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(inv.EvidenceDigest, "default-official-fake") {
		t.Fatal("must not use historical fake digest")
	}
	if !strings.HasPrefix(inv.EvidenceDigest, "capacity-") {
		t.Fatalf("digest=%s", inv.EvidenceDigest)
	}
	res, err := autoroute.Resolve(autoroute.Input{
		AutoRoute: true, ProjectID: "p", DecisionKey: "cap1", Now: t0(), Inventory: &inv, TaskClass: capclass.ClassTera,
	})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Outcome != autoroute.OutcomeSelected {
		t.Fatalf("%+v", res)
	}
	// Inventory exposes the full supported depth ladder; unsolicited resolve
	// without Effort must not force high (CRO-005). Medium must still be present.
	foundMediumDefault := false
	foundHigh := false
	for _, c := range inv.Candidates {
		if c.Model == "gpt-5.5" && c.Effort == "medium" {
			foundMediumDefault = true
		}
		if c.Model == "gpt-5.5" && c.Effort == "high" {
			foundHigh = true
		}
	}
	if !foundMediumDefault {
		t.Fatal("expected medium depth candidate from catalog")
	}
	if !foundHigh {
		t.Fatal("expected high depth candidate so per-child high requirements can bind")
	}
	// Auto-route without required Effort should not silently force high.
	if res.Effort == "high" {
		t.Fatalf("unsolicited resolve must not force high, got %+v", res)
	}
}

func TestFromCodexQuotaAdapter(t *testing.T) {
	raw := []byte(`[
	  {"kind":"five_hour","scope":"account","remaining":55,"limit":100,"unit":"percent","remaining_class":"","limit_class":""}
	]`)
	cq, err := codexquota.ParseJSONFixture(raw, codexquota.ParseOptions{Now: t0(), Source: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	// Force freshness/confidence as the parser sets them.
	for i := range cq.Windows {
		cq.Windows[i].Freshness = "fresh"
		cq.Windows[i].Confidence = "high"
	}
	obs := capacitysnapshot.FromCodexQuota(cq, capacitysnapshot.AccountInput{
		AccountRef: "acct-codex", Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium",
		}},
	})
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{obs}, t0())
	if err != nil {
		t.Fatal(err)
	}
	if !s.UnattendedOK {
		t.Fatalf("expected unattended ok: %+v", s)
	}
	if s.Accounts[0].Provider != "codex" {
		t.Fatalf("%+v", s.Accounts[0])
	}
}

func TestOfficialObservationPlansCoverFourProviders(t *testing.T) {
	plans := capacitysnapshot.OfficialObservationPlans()
	want := map[string]bool{"codex": false, "claude": false, "grok": false, "antigravity": false}
	for _, p := range plans {
		if _, ok := want[p.Provider]; ok {
			want[p.Provider] = true
		}
	}
	for p, ok := range want {
		if !ok {
			t.Fatalf("missing plan for %s", p)
		}
	}
}

func TestRateLimitedExcluded(t *testing.T) {
	in := healthyAccount("claude", "claude-sonnet-4-5")
	in.RateLimited = true
	s, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		capacitysnapshot.FromAccountInput(in),
	}, t0())
	if err != nil {
		t.Fatal(err)
	}
	if s.UnattendedOK {
		t.Fatal("rate limited must not be unattended-ok")
	}
}
