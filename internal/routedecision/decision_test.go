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
		AccountRef: "acct-" + provider, WindowKind: "five_hour",
		ModelClass: class,
		Installed:  okFact(provider + "-i"), Authenticated: okFact(provider + "-a"),
		ModelPresent: okFact(provider + "-m"), PermissionOK: okFact(provider + "-p"),
		EffortOK: okFact(provider + "-e"), Healthy: okFact(provider + "-h"),
		CooldownActive: falseFact(provider + "-cd"), ResourceFit: okFact(provider + "-r"),
		QuotaRemaining: 1000,
	}
}

func soft(provider, model string, rem float64, ttr time.Duration) quotapolicy.Candidate {
	// Default account/window match healthy() rows so production auto-route
	// exact-identity winner selection can bind.
	return softAcct(provider, model, "acct-"+provider, string(quotapolicy.WindowFiveHour), rem, ttr)
}

func softAcct(provider, model, account, windowKind string, rem float64, ttr time.Duration) quotapolicy.Candidate {
	rf := rem
	d := ttr
	wk := quotapolicy.WindowFiveHour
	switch strings.ToLower(strings.TrimSpace(windowKind)) {
	case "weekly":
		wk = quotapolicy.WindowWeekly
	case "credit":
		wk = quotapolicy.WindowCredit
	case "five_hour", "":
		wk = quotapolicy.WindowFiveHour
	default:
		if windowKind != "" {
			wk = quotapolicy.WindowKind(windowKind)
		}
	}
	return quotapolicy.Candidate{
		Provider: provider, Model: model,
		AccountRef: account, WindowKind: windowKind,
		Windows: []quotapolicy.Window{{
			Kind: wk, RemainingFraction: &rf,
			Evidence: quotapolicy.EvidenceExact, TimeToReset: &d,
		}},
		Reliability: func() *float64 { v := 0.9; return &v }(), ReliabilityEvidence: quotapolicy.EvidenceExact,
	}
}

func healthyAcct(provider, model, account, windowKind string, class capclass.Class) eligibility.Candidate {
	c := healthy(provider, model, class)
	c.AccountRef = account
	c.WindowKind = windowKind
	return c
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

// Score with empty account/window must not win by filling identity from a bound
// CandidateView (forbidden PM fallback / post-hoc identity fill).
func TestDecide_EmptyScoreIdentity_NoBorrowedWinner(t *testing.T) {
	req := baseReq(
		healthyAcct("codex", "gpt-5.5", "acct-a", "five_hour", capclass.ClassTera),
		healthyAcct("codex", "gpt-5.5", "acct-b", "weekly", capclass.ClassTera),
	)
	// Soft score has provider/model only — empty account/window.
	rf := 0.99
	d := 10 * time.Minute
	req.SoftCandidates = []quotapolicy.Candidate{{
		Provider: "codex", Model: "gpt-5.5",
		// AccountRef/WindowKind intentionally empty
		Windows: []quotapolicy.Window{{
			Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
			Evidence: quotapolicy.EvidenceExact, TimeToReset: &d,
		}},
		Reliability: func() *float64 { v := 0.9; return &v }(), ReliabilityEvidence: quotapolicy.EvidenceExact,
	}}
	dec, err := Evaluate(req)
	// no_route is acceptable when no exact-identity score qualifies.
	if err != nil && !strings.Contains(err.Error(), "no route") {
		t.Fatal(err)
	}
	if dec.Winner != nil && (dec.Winner.AccountRef == "acct-a" || dec.Winner.AccountRef == "acct-b") {
		t.Fatalf("empty score identity must not borrow bound account: %+v", dec.Winner)
	}
	if dec.Winner != nil && dec.Winner.AccountRef != "" && dec.Outcome == OutcomeSelected {
		t.Fatalf("must not select with fabricated account: %+v", dec.Winner)
	}
}

// Bound hard row missing exact account/window score must soft-exclude (never
// borrow another account's provider/model score).
func TestDecide_MissingExactAccountScore_FailClosed(t *testing.T) {
	req := baseReq(
		healthyAcct("codex", "gpt-5.5", "acct-a", "five_hour", capclass.ClassTera),
		healthyAcct("codex", "gpt-5.5", "acct-b", "weekly", capclass.ClassTera),
	)
	// Only acct-a has a soft score. acct-b is bound but missing exact score → excluded.
	req.SoftCandidates = []quotapolicy.Candidate{
		softAcct("codex", "gpt-5.5", "acct-a", "five_hour", 0.90, 20*time.Minute),
	}
	d, err := Evaluate(req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Winner == nil {
		t.Fatalf("want winner from scored acct-a: %+v", d)
	}
	if d.Winner.AccountRef != "acct-a" {
		t.Fatalf("winner account=%q want acct-a (not borrow PM score for acct-b)", d.Winner.AccountRef)
	}
	// Reverse soft order: only acct-b scored.
	req2 := baseReq(
		healthyAcct("codex", "gpt-5.5", "acct-a", "five_hour", capclass.ClassTera),
		healthyAcct("codex", "gpt-5.5", "acct-b", "weekly", capclass.ClassTera),
	)
	req2.SoftCandidates = []quotapolicy.Candidate{
		softAcct("codex", "gpt-5.5", "acct-b", "weekly", 0.85, 15*time.Minute),
	}
	d2, err := Evaluate(req2)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Winner == nil || d2.Winner.AccountRef != "acct-b" {
		t.Fatalf("want acct-b winner: %+v", d2.Winner)
	}
	// Views: acct-a must be soft-excluded for missing exact score.
	for _, cv := range d2.Candidates {
		if cv.AccountRef == "acct-a" && !cv.SoftExcluded {
			t.Fatalf("acct-a missing exact score must SoftExcluded: %+v", cv)
		}
	}
}

// Two same provider/model accounts with different windows/remaining: winner must
// be the exact scored account+window row (no cross-wire to the other account).
func TestDecide_TwoAccountsSameProviderModel_ExactAccountWindowWinner(t *testing.T) {
	req := baseReq(
		healthyAcct("codex", "gpt-5.5", "acct-primary", "five_hour", capclass.ClassTera),
		healthyAcct("codex", "gpt-5.5", "acct-secondary", "weekly", capclass.ClassTera),
	)
	// Secondary weekly near-reset + low remaining still loses to primary high remaining near-reset,
	// or we force primary to win via rem+ttr. Primary: rem 0.9, ttr 20m. Secondary: rem 0.15, ttr 20m.
	// Soft ranking prefers higher remaining when both near reset (balanced). Primary must win.
	req.SoftCandidates = []quotapolicy.Candidate{
		softAcct("codex", "gpt-5.5", "acct-primary", "five_hour", 0.90, 20*time.Minute),
		softAcct("codex", "gpt-5.5", "acct-secondary", "weekly", 0.15, 20*time.Minute),
	}
	d, err := Evaluate(req)
	if err != nil {
		t.Fatal(err)
	}
	if d.Outcome != OutcomeSelected || d.Winner == nil {
		t.Fatalf("want selected: %+v", d)
	}
	if d.Winner.Provider != "codex" || d.Winner.Model != "gpt-5.5" {
		t.Fatalf("provider/model: %+v", d.Winner)
	}
	if d.Winner.AccountRef != "acct-primary" {
		t.Fatalf("winner account=%q want acct-primary (exact scored row)", d.Winner.AccountRef)
	}
	if d.Winner.WindowKind != "five_hour" && !strings.Contains(d.Winner.WindowKind, "five") {
		t.Fatalf("winner window=%q want five_hour from scored row", d.Winner.WindowKind)
	}
	// Invert: secondary weekly with higher remaining near-reset wins; account/window must match secondary.
	req2 := baseReq(
		healthyAcct("codex", "gpt-5.5", "acct-primary", "five_hour", capclass.ClassTera),
		healthyAcct("codex", "gpt-5.5", "acct-secondary", "weekly", capclass.ClassTera),
	)
	req2.SoftCandidates = []quotapolicy.Candidate{
		softAcct("codex", "gpt-5.5", "acct-primary", "five_hour", 0.10, 40*time.Hour),
		softAcct("codex", "gpt-5.5", "acct-secondary", "weekly", 0.85, 15*time.Minute),
	}
	d2, err := Evaluate(req2)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Winner == nil {
		t.Fatalf("want winner: %+v", d2)
	}
	if d2.Winner.AccountRef != "acct-secondary" {
		t.Fatalf("winner account=%q want acct-secondary", d2.Winner.AccountRef)
	}
	if d2.Winner.WindowKind != "weekly" && !strings.Contains(strings.ToLower(d2.Winner.WindowKind), "week") {
		t.Fatalf("winner window=%q want weekly", d2.Winner.WindowKind)
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

func TestCandidateViewsCarryEffortPermissionDistinctDepths(t *testing.T) {
	// Two hard-eligible rows same provider/model, different efforts must both appear.
	hard := eligibility.Decision{
		Eligible: []eligibility.CandidateView{
			{Provider: "codex", Model: "gpt-5.5", Effort: "low", Permission: "bounded_write", Eligible: true},
			{Provider: "codex", Model: "gpt-5.5", Effort: "high", Permission: "bounded_write", Eligible: true},
			{Provider: "claude", Model: "claude-sonnet-4-5", Effort: "medium", Permission: "read-only", Eligible: true},
		},
	}
	views := candidateViewsFromHard(hard, nil, nil)
	if len(views) < 3 {
		t.Fatalf("want 3 distinct identities, got %+v", views)
	}
	seen := map[string]bool{}
	for _, v := range views {
		if v.Effort == "" && v.HardEligible {
			t.Fatalf("hard-eligible must carry observed effort: %+v", v)
		}
		key := v.Provider + "|" + v.Model + "|" + v.Effort + "|" + v.Permission
		if seen[key] {
			t.Fatalf("collapsed identity: %s", key)
		}
		seen[key] = true
	}
	if !seen["codex|gpt-5.5|low|bounded_write"] || !seen["codex|gpt-5.5|high|bounded_write"] {
		t.Fatalf("depths collapsed: %v", seen)
	}
}
