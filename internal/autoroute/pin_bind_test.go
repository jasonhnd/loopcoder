package autoroute_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
)

func okFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
}

func falseFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
}

func unknownFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactUnknown, EvidenceID: id, Freshness: eligibility.FreshUnknown}
}

func staleTrue(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshStale}
}

func expiredTrue(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshExpired}
}

func missingTrue(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshMissing}
}

// trueUnknown is State=true + FreshUnknown — KnownTrue alone still passes this;
// exactHardEligible must reject via !IsUnknown().
func trueUnknown(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshUnknown}
}

func pinMachineOK() eligibility.MachineAdmission {
	return eligibility.MachineAdmission{CapacityOK: okFact("mach"), ConcurrentSlots: 4}
}

func pinHealthyCodex() eligibility.Candidate {
	return eligibility.Candidate{
		Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write",
		ModelClass: capclass.ClassSoul,
		AccountRef: "acct-" + strings.Repeat("c", 64), InstallRef: "install-codex", WindowKind: "five_hour",
		Installed: okFact("i"), Authenticated: okFact("a"), ModelPresent: okFact("m"),
		PermissionOK: okFact("p"), EffortOK: okFact("e"), Healthy: okFact("h"),
		CooldownActive: falseFact("cd"), ResourceFit: okFact("r"),
	}
}

func pinInv(cands ...eligibility.Candidate) *autoroute.Inventory {
	return &autoroute.Inventory{
		EvidenceDigest: "test-pin-bind",
		Candidates:     cands,
		Machine:        pinMachineOK(),
	}
}

func TestExplicitPinFixtureFailsClosed(t *testing.T) {
	_, err := autoroute.Resolve(autoroute.Input{
		Provider: "fixture", Model: "fixture-model", Effort: "medium", Permission: "bounded_write",
		TaskClass: capclass.ClassTera,
	})
	if err == nil {
		t.Fatal("fixture pin must fail closed")
	}
	if !strings.Contains(err.Error(), "test-only") && !strings.Contains(err.Error(), "production") {
		t.Fatalf("err=%v", err)
	}
}

func TestExplicitPinUnknownProviderFails(t *testing.T) {
	_, err := autoroute.Resolve(autoroute.Input{
		Provider: "not-a-provider", Model: "m", Effort: "medium", Permission: "bounded_write",
		TaskClass: capclass.ClassTera,
	})
	if err == nil {
		t.Fatal("unknown provider pin must fail closed")
	}
}

func TestExplicitPinGeminiDepthFails(t *testing.T) {
	// Gemini cannot affirm depth.
	_, err := autoroute.Resolve(autoroute.Input{
		Provider: "gemini", Model: "gemini-2.5-flash", Effort: "medium", Permission: "bounded_write",
		TaskClass: capclass.ClassTera,
	})
	if err == nil {
		t.Fatal("gemini exact depth pin must fail closed")
	}
}

func TestExplicitPinClaudeAccountCapabilityOK(t *testing.T) {
	account := "acct-" + strings.Repeat("a", 64)
	result, err := autoroute.Resolve(autoroute.Input{
		Provider: "claude", Model: "claude-sonnet-4-5", Effort: "medium", Permission: "bounded_write",
		AccountRef: account,
		TaskClass:  capclass.ClassTera,
	})
	if err != nil {
		t.Fatalf("claude exact account capability: %v", err)
	}
	if result.Outcome != autoroute.OutcomeExplicitPin ||
		result.Provider != "claude" ||
		result.Model != "claude-sonnet-4-5" {
		t.Fatalf("claude explicit pin changed: %+v", result)
	}
}

func TestExplicitPinCodexCapabilityOK(t *testing.T) {
	res, err := autoroute.Resolve(autoroute.Input{
		Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write",
		TaskClass: capclass.ClassTera,
	})
	if err != nil {
		t.Fatalf("codex pin capability: %v", err)
	}
	if res.Outcome != autoroute.OutcomeExplicitPin {
		t.Fatalf("outcome=%s", res.Outcome)
	}
	if res.Provider != "codex" || res.Model != "gpt-5.5" {
		t.Fatalf("pin overridden: %+v", res)
	}
}

func TestBindExplicitPinRequiresExactInventoryCandidate(t *testing.T) {
	// Capability OK but no inventory → fail.
	_, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, nil)
	if err == nil {
		t.Fatal("nil inventory must fail")
	}
	// Inventory with exact hard-eligible row → bind account/install/window.
	inv := pinInv(pinHealthyCodex())
	bound, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, inv)
	if err != nil {
		t.Fatal(err)
	}
	if bound.AccountRef == "" || bound.InstallRef != "install-codex" || bound.WindowKind != "five_hour" {
		t.Fatalf("bind incomplete: %+v", bound)
	}
	if bound.Provider != "codex" || bound.Model != "gpt-5.5" {
		t.Fatalf("owner pin identity changed: %+v", bound)
	}
	// Wrong model → fail.
	_, err = autoroute.BindExplicitPinWithClass("codex", "other-model", "medium", "bounded_write", capclass.ClassTera, inv)
	if err == nil {
		t.Fatal("wrong model must fail bind")
	}
	// Claude now affirms exact account/install through the same executable's
	// pre/post machine-readable auth observations. Binding still requires an
	// exact hard-eligible inventory row.
	claudeAccount := "acct-" + strings.Repeat("a", 64)
	invClaude := pinInv(eligibility.Candidate{
		Provider: "claude", Model: "claude-sonnet-5", Effort: "low", Permission: "bounded_write",
		ModelClass: capclass.ClassTera,
		AccountRef: claudeAccount, InstallRef: "pinst_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WindowKind: "five_hour",
		Installed: okFact("i"), Authenticated: okFact("a"), ModelPresent: okFact("m"),
		PermissionOK: okFact("p"), EffortOK: okFact("e"), Healthy: okFact("h"),
		CooldownActive: falseFact("cd"), ResourceFit: okFact("r"),
	})
	claudeBound, err := autoroute.BindExplicitPinWithClass("claude", "claude-sonnet-5", "low", "bounded_write", capclass.ClassTera, invClaude)
	if err != nil {
		t.Fatal(err)
	}
	if claudeBound.AccountRef != claudeAccount || claudeBound.InstallRef != "pinst_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("claude exact binding incomplete: %+v", claudeBound)
	}
	// Antigravity cannot account-affirm even if inventory row present.
	invAG := pinInv(eligibility.Candidate{
		Provider: "antigravity", Model: "GPT-OSS 120B", Effort: "medium", Permission: "bounded_write",
		ModelClass: capclass.ClassSoul,
		AccountRef: "acct-ag", InstallRef: "i-ag", WindowKind: "five_hour",
		Installed: okFact("i"), Authenticated: okFact("a"), ModelPresent: okFact("m"),
		PermissionOK: okFact("p"), EffortOK: okFact("e"), Healthy: okFact("h"),
		CooldownActive: falseFact("cd"), ResourceFit: okFact("r"),
	})
	_, err = autoroute.BindExplicitPinWithClass("antigravity", "GPT-OSS 120B", "medium", "bounded_write", capclass.ClassTera, invAG)
	if err == nil {
		t.Fatal("antigravity pin bind must fail (no account affirm)")
	}
	_ = time.Now
}

func TestExplicitPinNotRerouted(t *testing.T) {
	// Pin remains exact provider/model even when inventory has other winners.
	res, err := autoroute.Resolve(autoroute.Input{
		Provider: "grok", Model: "grok-4.5", Effort: "medium", Permission: "bounded_write",
		TaskClass: capclass.ClassTera,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider != "grok" || res.Model != "grok-4.5" {
		t.Fatalf("pin overridden to %+v", res)
	}
}

// TestBindExplicitPinHardFactsKnownTrueAdversarial mutates every hard fact to
// unknown/stale/expired/missing/false and proves bind fails closed: no alternate
// provider/model, no capacity identity returned for reserve/spend.
func TestBindExplicitPinHardFactsKnownTrueAdversarial(t *testing.T) {
	ownerP, ownerM := "codex", "gpt-5.5"
	// Healthy alternate that must never be selected when pin is ineligible.
	alt := eligibility.Candidate{
		Provider: "grok", Model: "grok-4.5", Effort: "medium", Permission: "bounded_write",
		ModelClass: capclass.ClassSoul,
		AccountRef: "acct-" + strings.Repeat("g", 64), InstallRef: "install-grok", WindowKind: "five_hour",
		Installed: okFact("gi"), Authenticated: okFact("ga"), ModelPresent: okFact("gm"),
		PermissionOK: okFact("gp"), EffortOK: okFact("ge"), Healthy: okFact("gh"),
		CooldownActive: falseFact("gcd"), ResourceFit: okFact("gr"),
	}

	type mut struct {
		name string
		fn   func(*eligibility.Candidate)
	}
	// Positive hard facts that must be KnownTrue with usable freshness.
	positive := []struct {
		name string
		set  func(*eligibility.Candidate, eligibility.Fact)
	}{
		{"Installed", func(c *eligibility.Candidate, f eligibility.Fact) { c.Installed = f }},
		{"Authenticated", func(c *eligibility.Candidate, f eligibility.Fact) { c.Authenticated = f }},
		{"ModelPresent", func(c *eligibility.Candidate, f eligibility.Fact) { c.ModelPresent = f }},
		{"PermissionOK", func(c *eligibility.Candidate, f eligibility.Fact) { c.PermissionOK = f }},
		{"EffortOK", func(c *eligibility.Candidate, f eligibility.Fact) { c.EffortOK = f }},
		{"Healthy", func(c *eligibility.Candidate, f eligibility.Fact) { c.Healthy = f }},
		{"ResourceFit", func(c *eligibility.Candidate, f eligibility.Fact) { c.ResourceFit = f }},
	}
	badFacts := []struct {
		label string
		fact  func(string) eligibility.Fact
	}{
		{"state_unknown", unknownFact},
		{"true_fresh_unknown", trueUnknown}, // State=true + FreshUnknown hole
		{"stale", staleTrue},
		{"expired", expiredTrue},
		{"missing", missingTrue},
		{"false", falseFact},
	}

	var cases []mut
	for _, p := range positive {
		p := p
		for _, bf := range badFacts {
			bf := bf
			cases = append(cases, mut{
				name: p.name + "_" + bf.label,
				fn: func(c *eligibility.Candidate) {
					p.set(c, bf.fact(p.name+"-"+bf.label))
				},
			})
		}
	}
	// CooldownActive must be exactly known false and !IsUnknown.
	for _, bf := range []struct {
		label string
		fact  eligibility.Fact
	}{
		{"true", okFact("cd-true")}, // KnownTrue cooldown = ineligible
		{"state_unknown", unknownFact("cd-unk")},
		{"true_fresh_unknown", trueUnknown("cd-true-unk")}, // State=true + FreshUnknown
		{"false_fresh_unknown", eligibility.Fact{State: eligibility.FactFalse, EvidenceID: "cd-false-unk", Freshness: eligibility.FreshUnknown}},
		{"stale_false", eligibility.Fact{State: eligibility.FactFalse, EvidenceID: "cd-stale", Freshness: eligibility.FreshStale}},
		{"expired_false", eligibility.Fact{State: eligibility.FactFalse, EvidenceID: "cd-exp", Freshness: eligibility.FreshExpired}},
		{"missing_false", eligibility.Fact{State: eligibility.FactFalse, EvidenceID: "cd-miss", Freshness: eligibility.FreshMissing}},
		{"empty", eligibility.Fact{}},
	} {
		bf := bf
		cases = append(cases, mut{
			name: "CooldownActive_" + bf.label,
			fn:   func(c *eligibility.Candidate) { c.CooldownActive = bf.fact },
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := pinHealthyCodex()
			tc.fn(&c)
			inv := pinInv(c, alt)
			bound, err := autoroute.BindExplicitPinWithClass(ownerP, ownerM, "medium", "bounded_write", capclass.ClassTera, inv)
			if err == nil {
				t.Fatalf("expected fail closed on %s; got bind %+v", tc.name, bound)
			}
			if !errors.Is(err, autoroute.ErrPinFail) && !strings.Contains(err.Error(), "pin") {
				t.Fatalf("want pin fail, got %v", err)
			}
			// No capacity identity for reserve/spend.
			if bound.AccountRef != "" || bound.InstallRef != "" || bound.WindowKind != "" || bound.Candidate != nil {
				t.Fatalf("fail closed must not emit capacity identity: %+v", bound)
			}
			// Owner provider/model never rewritten to alternate (empty bind or exact owner only).
			if bound.Provider != "" && bound.Provider != ownerP {
				t.Fatalf("provider overridden to %q", bound.Provider)
			}
			if bound.Model != "" && bound.Model != ownerM {
				t.Fatalf("model overridden to %q", bound.Model)
			}
			if bound.Provider == "grok" || bound.Model == "grok-4.5" {
				t.Fatal("fallback to alternate provider/model forbidden")
			}
		})
	}
}

func TestBindExplicitPinMachineAdmissionRequired(t *testing.T) {
	c := pinHealthyCodex()
	// Missing / unknown machine capacity → fail closed even with healthy pin row.
	for _, name := range []string{"zero", "unknown", "false", "stale"} {
		t.Run("machine_"+name, func(t *testing.T) {
			inv := pinInv(c)
			switch name {
			case "zero":
				inv.Machine = eligibility.MachineAdmission{}
			case "unknown":
				inv.Machine = eligibility.MachineAdmission{CapacityOK: unknownFact("mach")}
			case "false":
				inv.Machine = eligibility.MachineAdmission{CapacityOK: falseFact("mach")}
			case "stale":
				inv.Machine = eligibility.MachineAdmission{CapacityOK: staleTrue("mach")}
			}
			bound, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, inv)
			if err == nil {
				t.Fatalf("machine %s must fail bind; got %+v", name, bound)
			}
			if bound.AccountRef != "" || bound.Candidate != nil {
				t.Fatalf("no reserve identity on machine fail: %+v", bound)
			}
		})
	}
}

func TestBindExplicitPinTaskClassQualityGate(t *testing.T) {
	// Luna model cannot meet Soul task class; healthy Soul alternate must not win.
	cLuna := pinHealthyCodex()
	cLuna.ModelClass = capclass.ClassLuna
	alt := eligibility.Candidate{
		Provider: "grok", Model: "grok-4.5", Effort: "medium", Permission: "bounded_write",
		ModelClass: capclass.ClassSoul,
		AccountRef: "acct-" + strings.Repeat("g", 64), InstallRef: "install-grok", WindowKind: "five_hour",
		Installed: okFact("gi"), Authenticated: okFact("ga"), ModelPresent: okFact("gm"),
		PermissionOK: okFact("gp"), EffortOK: okFact("ge"), Healthy: okFact("gh"),
		CooldownActive: falseFact("gcd"), ResourceFit: okFact("gr"),
	}
	bound, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassSoul, pinInv(cLuna, alt))
	if err == nil {
		t.Fatalf("task class unmet must fail closed; got %+v", bound)
	}
	if bound.Provider == "grok" || bound.InstallRef == "install-grok" {
		t.Fatal("must not fallback to grok when pin task class fails")
	}
	// Soul model meets Tera → bind OK; owner identity preserved.
	boundOK, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, pinInv(pinHealthyCodex()))
	if err != nil {
		t.Fatal(err)
	}
	if boundOK.Provider != "codex" || boundOK.Model != "gpt-5.5" {
		t.Fatalf("owner pin changed: %+v", boundOK)
	}
}

func TestBindExplicitPinNoFallbackWhenAlternateHealthy(t *testing.T) {
	// Pin broken auth; alternate fully healthy — still pin-fail, never bind alternate.
	bad := pinHealthyCodex()
	bad.Authenticated = unknownFact("auth-stale")
	alt := eligibility.Candidate{
		Provider: "grok", Model: "grok-4.5", Effort: "medium", Permission: "bounded_write",
		ModelClass: capclass.ClassSoul,
		AccountRef: "acct-" + strings.Repeat("g", 64), InstallRef: "install-grok", WindowKind: "five_hour",
		Installed: okFact("gi"), Authenticated: okFact("ga"), ModelPresent: okFact("gm"),
		PermissionOK: okFact("gp"), EffortOK: okFact("ge"), Healthy: okFact("gh"),
		CooldownActive: falseFact("gcd"), ResourceFit: okFact("gr"),
	}
	bound, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, pinInv(bad, alt))
	if err == nil {
		t.Fatal("expected pin fail")
	}
	if bound.InstallRef == "install-grok" || bound.Provider == "grok" {
		t.Fatalf("fallback bind forbidden: %+v", bound)
	}
	// Owner identity not rewritten.
	if bound.Provider != "" && bound.Provider != "codex" {
		t.Fatalf("provider mutated: %q", bound.Provider)
	}
}

func TestBindExplicitPinMissingCapacityIdentityFails(t *testing.T) {
	c := pinHealthyCodex()
	c.AccountRef = ""
	_, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, pinInv(c))
	if err == nil {
		t.Fatal("missing account must fail (no reserve identity)")
	}
	c2 := pinHealthyCodex()
	c2.InstallRef = ""
	_, err = autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, pinInv(c2))
	if err == nil {
		t.Fatal("missing install must fail")
	}
	c3 := pinHealthyCodex()
	c3.WindowKind = ""
	_, err = autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, pinInv(c3))
	if err == nil {
		t.Fatal("missing window must fail")
	}
}

// TestBindExplicitPinNoCrossRowAlias: same account/install, different
// permission/effort/window — bad exact requested row fails even when a sibling
// healthy row shares account/install.
func TestBindExplicitPinNoCrossRowAlias(t *testing.T) {
	acct := "acct-" + strings.Repeat("c", 64)
	inst := "install-codex"
	// Requested: medium + bounded_write — intentionally hard-ineligible (stale auth).
	badWrite := eligibility.Candidate{
		Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write",
		ModelClass: capclass.ClassSoul,
		AccountRef: acct, InstallRef: inst, WindowKind: "five_hour",
		Installed: okFact("i"), Authenticated: staleTrue("a-stale"), ModelPresent: okFact("m"),
		PermissionOK: okFact("p"), EffortOK: okFact("e"), Healthy: okFact("h"),
		CooldownActive: falseFact("cd"), ResourceFit: okFact("r"),
	}
	// Sibling: same account/install, healthy read-only medium.
	sibRO := eligibility.Candidate{
		Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "read-only",
		ModelClass: capclass.ClassSoul,
		AccountRef: acct, InstallRef: inst, WindowKind: "five_hour",
		Installed: okFact("i2"), Authenticated: okFact("a2"), ModelPresent: okFact("m2"),
		PermissionOK: okFact("p2"), EffortOK: okFact("e2"), Healthy: okFact("h2"),
		CooldownActive: falseFact("cd2"), ResourceFit: okFact("r2"),
	}
	// Sibling: same account/install, healthy high + bounded_write (different effort).
	sibHigh := eligibility.Candidate{
		Provider: "codex", Model: "gpt-5.5", Effort: "high", Permission: "bounded_write",
		ModelClass: capclass.ClassSoul,
		AccountRef: acct, InstallRef: inst, WindowKind: "five_hour",
		Installed: okFact("i3"), Authenticated: okFact("a3"), ModelPresent: okFact("m3"),
		PermissionOK: okFact("p3"), EffortOK: okFact("e3"), Healthy: okFact("h3"),
		CooldownActive: falseFact("cd3"), ResourceFit: okFact("r3"),
	}
	// Sibling: different window, healthy medium write.
	sibWin := eligibility.Candidate{
		Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write",
		ModelClass: capclass.ClassSoul,
		AccountRef: acct, InstallRef: inst, WindowKind: "weekly",
		Installed: okFact("i4"), Authenticated: okFact("a4"), ModelPresent: okFact("m4"),
		PermissionOK: okFact("p4"), EffortOK: okFact("e4"), Healthy: okFact("h4"),
		CooldownActive: falseFact("cd4"), ResourceFit: okFact("r4"),
	}
	inv := pinInv(badWrite, sibRO, sibHigh, sibWin)
	bound, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, inv)
	if err == nil {
		t.Fatalf("must not alias to healthy sibling; got %+v", bound)
	}
	if bound.Permission == "read-only" || bound.Effort == "high" || bound.WindowKind == "weekly" {
		t.Fatalf("cross-row alias forbidden: %+v", bound)
	}
	if bound.Candidate != nil {
		t.Fatalf("no candidate on fail: %+v", bound.Candidate)
	}

	// Control: exact healthy medium+write row binds.
	good := badWrite
	good.Authenticated = okFact("a-ok")
	boundOK, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassTera, pinInv(good, sibRO))
	if err != nil {
		t.Fatal(err)
	}
	if boundOK.Permission != "bounded_write" || boundOK.Effort != "medium" || boundOK.WindowKind != "five_hour" {
		t.Fatalf("exact row identity: %+v", boundOK)
	}
}

// TestBindExplicitPinWithClassTaskClassFailClosed: empty/invalid/needs_human
// never silently default to Tera.
func TestBindExplicitPinWithClassTaskClassFailClosed(t *testing.T) {
	inv := pinInv(pinHealthyCodex())
	for _, tc := range []struct {
		name string
		cl   capclass.Class
	}{
		{"empty", ""},
		{"invalid", "mega"},
		{"needs_human", capclass.ClassNeedsHuman},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", tc.cl, inv)
			if err == nil {
				t.Fatalf("class %q must fail closed; got %+v", tc.cl, bound)
			}
			if bound.AccountRef != "" || bound.Candidate != nil {
				t.Fatalf("no capacity identity on class fail: %+v", bound)
			}
		})
	}
	// Valid soul binds.
	if _, err := autoroute.BindExplicitPinWithClass("codex", "gpt-5.5", "medium", "bounded_write", capclass.ClassSoul, inv); err != nil {
		t.Fatal(err)
	}
}

// TestNoBindExplicitPinWithoutClass guards: no exported no-class binding path
// (BindExplicitPin was deleted; production/tests must use WithClass + explicit class).
func TestNoBindExplicitPinWithoutClass(t *testing.T) {
	src, err := os.ReadFile("pin_bind.go")
	if err != nil {
		src, err = os.ReadFile(filepath.Join("internal", "autoroute", "pin_bind.go"))
	}
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if strings.Contains(body, "func BindExplicitPin(") {
		t.Fatal("BindExplicitPin must not be re-exported (silent ClassTera path forbidden)")
	}
	if !strings.Contains(body, "func BindExplicitPinWithClass(") {
		t.Fatal("BindExplicitPinWithClass required")
	}
	// Production non-test callers must not call a no-class bind.
	// Scan sibling packages that bind pins.
	for _, rel := range []string{
		filepath.Join("..", "cli", "run_cmd.go"),
		filepath.Join("..", "goalrun", "run.go"),
		filepath.Join("internal", "cli", "run_cmd.go"),
		filepath.Join("internal", "goalrun", "run.go"),
	} {
		b, err := os.ReadFile(rel)
		if err != nil {
			continue
		}
		s := string(b)
		if strings.Contains(s, "BindExplicitPin(") && !strings.Contains(s, "BindExplicitPinWithClass(") {
			t.Fatalf("%s calls BindExplicitPin without WithClass", rel)
		}
		if strings.Contains(s, "autoroute.BindExplicitPin(") {
			t.Fatalf("%s must not call deleted BindExplicitPin", rel)
		}
	}
}
