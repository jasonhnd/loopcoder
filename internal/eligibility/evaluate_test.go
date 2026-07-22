package eligibility

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
)

func okFact(id string) Fact {
	return Fact{State: FactTrue, EvidenceID: id, Freshness: FreshFresh}
}

func falseFact(id string) Fact {
	return Fact{State: FactFalse, EvidenceID: id, Freshness: FreshFresh}
}

func unknownFact(id string) Fact {
	return Fact{State: FactUnknown, EvidenceID: id, Freshness: FreshUnknown}
}

func staleFact(id string) Fact {
	return Fact{State: FactTrue, EvidenceID: id, Freshness: FreshStale}
}

func healthyCandidate(provider, model string, class capclass.Class) Candidate {
	return Candidate{
		Provider: provider, Model: model, Effort: "high", Permission: "bounded_write",
		ModelClass:     class,
		Installed:      okFact(provider + "-install"),
		Authenticated:  okFact(provider + "-auth"),
		ModelPresent:   okFact(provider + "-model"),
		PermissionOK:   okFact(provider + "-perm"),
		EffortOK:       okFact(provider + "-effort"),
		Healthy:        okFact(provider + "-health"),
		CooldownActive: falseFact(provider + "-cd"),
		ResourceFit:    okFact(provider + "-res"),
		QuotaRemaining: 99999, // high quota must not compensate hard failures
	}
}

func baseSnap(cands ...Candidate) Snapshot {
	return Snapshot{
		TaskRequiredClass: capclass.ClassTera,
		Candidates:        cands,
		Machine:           MachineAdmission{CapacityOK: okFact("machine-cap"), ConcurrentSlots: 4},
		CapturedAt:        time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC),
	}
}

func TestExplicitPinEligibleUnchanged(t *testing.T) {
	codex := healthyCandidate("codex", "gpt-5.3-codex", capclass.ClassSoul)
	claude := healthyCandidate("claude", "claude-sonnet-4-5", capclass.ClassTera)
	pin := PinFields{Provider: "claude", Model: "claude-sonnet-4-5", Effort: "high"}
	snap := baseSnap(codex, claude)
	snap.ExplicitPin = &pin

	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if d.Mode != ModeExplicitPin || d.FailClosed {
		t.Fatalf("decision %+v", d)
	}
	if d.PinSelected == nil || d.PinSelected.Provider != "claude" || d.PinSelected.Model != "claude-sonnet-4-5" {
		t.Fatalf("pin selected %+v", d.PinSelected)
	}
	if len(d.Eligible) != 1 {
		t.Fatalf("eligible %d", len(d.Eligible))
	}
	// codex excluded solely due to pin mismatch (not evaluated as competitor)
	if len(d.Excluded) != 1 || d.Excluded[0].Provider != "codex" {
		t.Fatalf("excluded %+v", d.Excluded)
	}
	if d.Excluded[0].Reasons[0] != ReasonPinMismatch {
		t.Fatalf("reasons %v", d.Excluded[0].Reasons)
	}
}

func TestExplicitPinIneligibleNoFallback(t *testing.T) {
	// Pin to claude but auth missing — fail closed even though codex is healthy with huge quota.
	codex := healthyCandidate("codex", "gpt-5.3-codex", capclass.ClassSoul)
	claude := healthyCandidate("claude", "claude-sonnet-4-5", capclass.ClassTera)
	claude.Authenticated = falseFact("claude-auth-bad")
	claude.QuotaRemaining = 1_000_000
	pin := PinFields{Provider: "claude", Model: "claude-sonnet-4-5"}
	snap := baseSnap(codex, claude)
	snap.ExplicitPin = &pin

	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !d.FailClosed || d.PinSelected != nil || len(d.Eligible) != 0 {
		t.Fatalf("expected fail-closed, got %+v", d)
	}
	// Must not silently select codex
	for _, e := range d.Eligible {
		if e.Provider == "codex" {
			t.Fatal("fallback to codex forbidden")
		}
	}
	found := false
	for _, r := range d.Reasons {
		if r == ReasonPinIneligible || r == ReasonAuthMissing {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons %v", d.Reasons)
	}
}

func TestQuotaCannotCompensateHardFailure(t *testing.T) {
	c := healthyCandidate("grok", "grok-4.5", capclass.ClassSoul)
	c.Installed = falseFact("grok-missing")
	c.QuotaRemaining = 9_999_999
	snap := baseSnap(c)
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Eligible) != 0 {
		t.Fatalf("eligible despite missing install: %+v", d.Eligible)
	}
	if len(d.Excluded) != 1 || d.Excluded[0].Reasons[0] != ReasonNotInstalled {
		t.Fatalf("excluded %+v", d.Excluded)
	}
}

func TestUnknownEvidenceIneligible(t *testing.T) {
	c := healthyCandidate("gemini", "gemini-2.5-pro", capclass.ClassSoul)
	c.Authenticated = unknownFact("gem-auth")
	snap := baseSnap(c)
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Eligible) != 0 {
		t.Fatal("unknown auth must not be eligible")
	}
	if d.Excluded[0].Reasons[0] != ReasonAuthUnknown {
		t.Fatalf("reasons %v", d.Excluded[0].Reasons)
	}
}

func TestStaleEvidenceIneligible(t *testing.T) {
	c := healthyCandidate("antigravity", "gemini-2.5-flash", capclass.ClassTera)
	c.Healthy = staleFact("ag-health")
	snap := baseSnap(c)
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Eligible) != 0 {
		t.Fatal("stale health ineligible")
	}
	if d.Excluded[0].Reasons[0] != ReasonStaleEvidence {
		t.Fatalf("reasons %v", d.Excluded[0].Reasons)
	}
}

func TestTaskClassGate(t *testing.T) {
	// Luna model cannot meet Soul task
	c := healthyCandidate("claude", "claude-haiku-4-5", capclass.ClassLuna)
	snap := baseSnap(c)
	snap.TaskRequiredClass = capclass.ClassSoul
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Eligible) != 0 {
		t.Fatal("luna must not meet soul")
	}
	found := false
	for _, r := range d.Excluded[0].Reasons {
		if r == ReasonTaskClass {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons %v", d.Excluded[0].Reasons)
	}
}

func TestNeedsHumanBlocksAll(t *testing.T) {
	c := healthyCandidate("codex", "gpt-5.3-codex", capclass.ClassSoul)
	snap := baseSnap(c)
	snap.TaskRequiredClass = capclass.ClassNeedsHuman
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Eligible) != 0 {
		t.Fatal("needs_human blocks routing")
	}
}

func TestCooldownAndMachine(t *testing.T) {
	c := healthyCandidate("codex", "gpt-5.2-codex", capclass.ClassTera)
	c.CooldownActive = okFact("cd") // true = on cooldown
	snap := baseSnap(c)
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Eligible) != 0 {
		t.Fatal("cooldown ineligible")
	}

	c2 := healthyCandidate("codex", "gpt-5.2-codex", capclass.ClassTera)
	snap2 := baseSnap(c2)
	snap2.Machine.CapacityOK = falseFact("full")
	d2, err := Evaluate(snap2)
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.Eligible) != 0 {
		t.Fatal("machine full ineligible")
	}
}

func TestPolicyDeny(t *testing.T) {
	c := healthyCandidate("grok", "grok-4", capclass.ClassTera)
	snap := baseSnap(c)
	snap.Policy.DenyProvider = []string{"grok"}
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Eligible) != 0 || d.Excluded[0].Reasons[0] != ReasonPolicyDenyProvider {
		t.Fatalf("%+v", d)
	}
}

func TestOfficialProviderMatrix(t *testing.T) {
	// All official providers present; only those meeting class + healthy pass.
	cands := []Candidate{
		healthyCandidate("codex", "gpt-5.3-codex", capclass.ClassSoul),
		healthyCandidate("claude", "claude-sonnet-4-5", capclass.ClassTera),
		healthyCandidate("gemini", "gemini-2.5-flash", capclass.ClassTera),
		healthyCandidate("antigravity", "gemini-2.5-pro", capclass.ClassSoul),
		healthyCandidate("grok", "grok-4.5", capclass.ClassSoul),
	}
	// Break gemini auth
	cands[2].Authenticated = falseFact("gem-auth")
	snap := baseSnap(cands...)
	snap.TaskRequiredClass = capclass.ClassTera
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Eligible) != 4 {
		t.Fatalf("eligible=%d want 4: %+v", len(d.Eligible), d.Eligible)
	}
	if len(d.Excluded) != 1 || d.Excluded[0].Provider != "gemini" {
		t.Fatalf("excluded %+v", d.Excluded)
	}
	// Deterministic digest replay
	d2, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if d.Digest == "" || d.Digest != d2.Digest {
		t.Fatalf("digest %s vs %s", d.Digest, d2.Digest)
	}
	// Eligible set identical
	if len(d.Eligible) != len(d2.Eligible) {
		t.Fatal("eligible set drift")
	}
}

func TestEveryCandidateHasResult(t *testing.T) {
	cands := []Candidate{
		healthyCandidate("codex", "gpt-5.1-codex-mini", capclass.ClassLuna),
		healthyCandidate("claude", "claude-opus-4-5", capclass.ClassSoul),
	}
	// luna fails tera class; opus passes
	snap := baseSnap(cands...)
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	total := len(d.Eligible) + len(d.Excluded)
	if total != 2 {
		t.Fatalf("total %d", total)
	}
	// every exclusion has ordered reasons + evidence ids
	for _, ex := range d.Excluded {
		if len(ex.Reasons) == 0 {
			t.Fatalf("no reasons for %s", ex.Provider)
		}
		if len(ex.EvidenceID) == 0 {
			t.Fatalf("no evidence ids for %s", ex.Provider)
		}
	}
}

func TestPinMissingCandidateFailClosed(t *testing.T) {
	c := healthyCandidate("codex", "gpt-5.3-codex", capclass.ClassSoul)
	snap := baseSnap(c)
	pin := PinFields{Provider: "grok", Model: "grok-4.5"}
	snap.ExplicitPin = &pin
	d, err := Evaluate(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !d.FailClosed || len(d.Eligible) != 0 {
		t.Fatalf("%+v", d)
	}
}
