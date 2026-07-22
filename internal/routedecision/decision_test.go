package routedecision

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/quotamode"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

func okFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
}
func falseFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
}

func healthy(provider, model string, class capclass.Class) eligibility.Candidate {
	return eligibility.Candidate{
		Provider: provider, Model: model, Effort: "high", Permission: "bounded_write",
		ModelClass: class,
		Installed:  okFact(provider + "-i"), Authenticated: okFact(provider + "-a"),
		ModelPresent: okFact(provider + "-m"), PermissionOK: okFact(provider + "-p"),
		EffortOK: okFact(provider + "-e"), Healthy: okFact(provider + "-h"),
		CooldownActive: falseFact(provider + "-cd"), ResourceFit: okFact(provider + "-r"),
		QuotaRemaining: 1000,
	}
}

func soft(provider, model string, rem float64, ttr time.Duration) quotapolicy.Candidate {
	rf := rem
	d := ttr
	return quotapolicy.Candidate{
		Provider: provider, Model: model,
		Windows: []quotapolicy.Window{{
			Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
			Evidence: quotapolicy.EvidenceExact, TimeToReset: &d,
		}},
		Reliability: func() *float64 { v := 0.9; return &v }(), ReliabilityEvidence: quotapolicy.EvidenceExact,
	}
}

func baseReq(cands ...eligibility.Candidate) Request {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	return Request{
		DecisionKey: "dk-1", ProjectID: "proj-1", EvidenceDigest: "ev-sha-1",
		TaskClass: capclass.ClassTera,
		Eligibility: eligibility.Snapshot{
			TaskRequiredClass: capclass.ClassTera,
			Candidates:        cands,
			Machine:           eligibility.MachineAdmission{CapacityOK: okFact("mach")},
			CapturedAt:        now,
		},
		Mode: quotamode.DefaultModeConfig(quotamode.ModeBalanced),
		Now:  now,
	}
}

func TestDecideSelectsWinnerDeterministic(t *testing.T) {
	req := baseReq(
		healthy("codex", "gpt-5.2-codex", capclass.ClassTera),
		healthy("claude", "claude-sonnet-4-5", capclass.ClassTera),
	)
	req.SoftCandidates = []quotapolicy.Candidate{
		soft("codex", "gpt-5.2-codex", 0.8, 30*time.Minute),
		soft("claude", "claude-sonnet-4-5", 0.8, 48*time.Hour),
	}
	d1, err := Evaluate(req)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Outcome != OutcomeSelected || d1.Winner == nil {
		t.Fatalf("%+v", d1)
	}
	if d1.Winner.Provider != "codex" {
		t.Fatalf("near-reset should win: %+v", d1.Winner)
	}
	d2, err := Evaluate(req)
	if err != nil {
		t.Fatal(err)
	}
	if d1.Digest != d2.Digest {
		t.Fatalf("digest drift %s vs %s", d1.Digest, d2.Digest)
	}
	// digests present for replay
	if d1.EligibilityDigest == "" || d1.SoftRankingDigest == "" || d1.ModeRankingDigest == "" {
		t.Fatalf("missing digests %+v", d1)
	}
}

func TestPinFailClosedNoFallback(t *testing.T) {
	claude := healthy("claude", "claude-sonnet-4-5", capclass.ClassTera)
	claude.Authenticated = falseFact("bad-auth")
	codex := healthy("codex", "gpt-5.3-codex", capclass.ClassSoul)
	req := baseReq(codex, claude)
	pin := eligibility.PinFields{Provider: "claude", Model: "claude-sonnet-4-5"}
	req.Eligibility.ExplicitPin = &pin
	d, err := Evaluate(req)
	if !errors.Is(err, ErrPinFailed) {
		t.Fatalf("err %v", err)
	}
	if d.Outcome != OutcomePinFail || d.Winner != nil {
		t.Fatalf("%+v", d)
	}
	if !d.PinFailClosed {
		t.Fatal("pin fail closed flag")
	}
}

func TestNoRoute(t *testing.T) {
	c := healthy("gemini", "gemini-2.5-flash", capclass.ClassTera)
	c.Installed = falseFact("missing")
	req := baseReq(c)
	d, err := Evaluate(req)
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("err %v d=%+v", err, d)
	}
	if d.Outcome != OutcomeNoRoute {
		t.Fatalf("%s", d.Outcome)
	}
}

func TestStoreIdempotentAndConflict(t *testing.T) {
	s := NewStore()
	req := baseReq(healthy("grok", "grok-4.5", capclass.ClassSoul))
	req.SoftCandidates = []quotapolicy.Candidate{soft("grok", "grok-4.5", 0.6, 2*time.Hour)}
	d1, err := s.Decide(req)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.Decide(req)
	if err != nil {
		t.Fatal(err)
	}
	if d1.DecisionID != d2.DecisionID || d1.Digest != d2.Digest {
		t.Fatalf("idempotent fail")
	}
	// conflict: same key different evidence
	req2 := req
	req2.EvidenceDigest = "ev-other"
	_, err = s.Decide(req2)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict got %v", err)
	}
	got, err := s.GetByKey("dk-1")
	if err != nil || got.Digest != d1.Digest {
		t.Fatal(err)
	}
	got2, err := s.GetByID(d1.DecisionID)
	if err != nil || got2.DecisionKey != "dk-1" {
		t.Fatal(err)
	}
}

func TestExplainHumanAndJSON(t *testing.T) {
	req := baseReq(
		healthy("codex", "gpt-5.2-codex", capclass.ClassTera),
		healthy("claude", "claude-haiku-4-5", capclass.ClassLuna), // fails class if required tera - actually luna model for tera task fails hard class
	)
	// fix: use tera models
	req = baseReq(
		healthy("codex", "gpt-5.2-codex", capclass.ClassTera),
		healthy("claude", "claude-sonnet-4-5", capclass.ClassTera),
	)
	req.SoftCandidates = []quotapolicy.Candidate{
		soft("codex", "gpt-5.2-codex", 0.7, 1*time.Hour),
		soft("claude", "claude-sonnet-4-5", 0.7, 1*time.Hour),
	}
	s := NewStore()
	d, err := s.Decide(req)
	if err != nil {
		t.Fatal(err)
	}
	ex := Explain(d)
	if ex.Human == "" || !strings.Contains(ex.Human, "Winner:") {
		t.Fatalf("human %q", ex.Human)
	}
	if !strings.Contains(ex.Human, "Redaction:") {
		t.Fatal("redaction note missing")
	}
	// no credential-like keys
	if strings.Contains(ex.Human, "token") || strings.Contains(ex.Human, "password") {
		t.Fatal("leaked secrets?")
	}
	b, err := ExplainJSON(d)
	if err != nil || !strings.Contains(string(b), d.Digest) {
		t.Fatalf("json %v %s", err, b)
	}
	// sections cover pin/hard/soft
	found := false
	for _, s := range ex.Sections {
		if s == "winner" || s == "candidates" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sections %v", ex.Sections)
	}
}

func TestExplainListsHardExclusions(t *testing.T) {
	bad := healthy("gemini", "gemini-2.5-flash", capclass.ClassTera)
	bad.Authenticated = falseFact("noauth")
	ok := healthy("claude", "claude-sonnet-4-5", capclass.ClassTera)
	req := baseReq(bad, ok)
	req.SoftCandidates = []quotapolicy.Candidate{
		soft("claude", "claude-sonnet-4-5", 0.5, 3*time.Hour),
		soft("gemini", "gemini-2.5-flash", 0.9, 30*time.Minute),
	}
	d, err := Evaluate(req)
	if err != nil {
		t.Fatal(err)
	}
	ex := Explain(d)
	if !strings.Contains(ex.Human, "Hard exclusions:") || !strings.Contains(ex.Human, "gemini") {
		t.Fatalf("%s", ex.Human)
	}
}
