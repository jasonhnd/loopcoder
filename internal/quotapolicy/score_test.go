package quotapolicy

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
)

func fptr(v float64) *float64             { return &v }
func dptr(d time.Duration) *time.Duration { return &d }

func TestNearResetPreferred(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	// A: abundant, resets in 30m
	a := Candidate{
		Provider: "codex", Model: "gpt-5.2-codex",
		Windows: []Window{{
			Kind: WindowFiveHour, RemainingFraction: fptr(0.8), Evidence: EvidenceExact,
			TimeToReset: dptr(30 * time.Minute),
		}},
		Reliability: fptr(0.9), ReliabilityEvidence: EvidenceExact,
	}
	// B: same remaining, resets in 3 days
	b := Candidate{
		Provider: "claude", Model: "claude-sonnet-4-5",
		Windows: []Window{{
			Kind: WindowFiveHour, RemainingFraction: fptr(0.8), Evidence: EvidenceExact,
			TimeToReset: dptr(72 * time.Hour),
		}},
		Reliability: fptr(0.9), ReliabilityEvidence: EvidenceExact,
	}
	r, err := Rank(Input{
		Policy: DefaultPolicy(), TaskClass: capclass.ClassTera,
		Candidates: []Candidate{b, a}, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.Scores[0].Provider != "codex" {
		t.Fatalf("want codex first (near reset), got %+v", r.Scores)
	}
	if r.Scores[0].BurnUrgency <= r.Scores[1].BurnUrgency {
		t.Fatalf("burn urgency %+v", r.Scores)
	}
	found := false
	for _, reason := range r.Scores[0].Reasons {
		if reason == ReasonNearResetPrefer {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons %v", r.Scores[0].Reasons)
	}
}

func TestExhaustedAndRateLimitExcluded(t *testing.T) {
	now := time.Now().UTC()
	ex := Candidate{
		Provider: "gemini", Model: "gemini-2.5-flash",
		Windows: []Window{{
			Kind: WindowWeekly, RemainingFraction: fptr(0), Evidence: EvidenceExact, Exhausted: true,
		}},
		ReliabilityEvidence: EvidenceMissing,
	}
	rl := Candidate{
		Provider: "grok", Model: "grok-4",
		Windows: []Window{{
			Kind: WindowRateLimit, Evidence: EvidenceExact, RateLimited: true, RemainingFraction: fptr(0.9),
		}},
		ReliabilityEvidence: EvidenceMissing,
	}
	ok := Candidate{
		Provider: "claude", Model: "claude-sonnet-4-5",
		Windows: []Window{{
			Kind: WindowFiveHour, RemainingFraction: fptr(0.5), Evidence: EvidenceExact,
			TimeToReset: dptr(3 * time.Hour),
		}},
		Reliability: fptr(0.8), ReliabilityEvidence: EvidenceExact,
	}
	r, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassTera, Candidates: []Candidate{ex, rl, ok}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if r.Scores[0].Provider != "claude" || r.Scores[0].SoftExcluded {
		t.Fatalf("top %+v", r.Scores[0])
	}
	for _, s := range r.Scores[1:] {
		if !s.SoftExcluded {
			t.Fatalf("expected excluded %+v", s)
		}
	}
}

func TestReserveBreachForLowerClass(t *testing.T) {
	now := time.Now().UTC()
	// Only 15% remaining; soul reserve 20% → Luna task cannot use it
	c := Candidate{
		Provider: "codex", Model: "gpt-5.3-codex",
		Windows: []Window{{
			Kind: WindowFiveHour, RemainingFraction: fptr(0.15), Evidence: EvidenceExact,
			TimeToReset: dptr(4 * time.Hour),
		}},
		Reliability: fptr(0.95), ReliabilityEvidence: EvidenceExact,
	}
	r, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassLuna, Candidates: []Candidate{c}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Scores[0].SoftExcluded {
		t.Fatalf("expected reserve breach %+v", r.Scores[0])
	}
	found := false
	for _, x := range r.Scores[0].ExcludeReasons {
		if x == ReasonReserveBreach {
			found = true
		}
	}
	if !found {
		t.Fatalf("exclude %v", r.Scores[0].ExcludeReasons)
	}

	// Soul task may use the same capacity
	r2, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassSoul, Candidates: []Candidate{c}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Scores[0].SoftExcluded {
		t.Fatalf("soul should use capacity %+v", r2.Scores[0])
	}
}

func TestUnknownNotNumericZero(t *testing.T) {
	now := time.Now().UTC()
	unknown := Candidate{
		Provider: "antigravity", Model: "gemini-2.5-pro",
		Windows:             []Window{{Kind: WindowFiveHour, Evidence: EvidenceUnknown}},
		ReliabilityEvidence: EvidenceUnknown,
	}
	// Zero remaining exact is exhausted; unknown must not equal that.
	zero := Candidate{
		Provider: "gemini", Model: "gemini-2.5-flash",
		Windows: []Window{{
			Kind: WindowFiveHour, RemainingFraction: fptr(0), Evidence: EvidenceExact, Exhausted: true,
		}},
		ReliabilityEvidence: EvidenceMissing,
	}
	r, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassTera, Candidates: []Candidate{unknown, zero}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	// zero exhausted → soft excluded; unknown may score (low) but not excluded as exhausted
	var u, z *Score
	for i := range r.Scores {
		if r.Scores[i].Provider == "antigravity" {
			u = &r.Scores[i]
		}
		if r.Scores[i].Provider == "gemini" {
			z = &r.Scores[i]
		}
	}
	if u == nil || z == nil {
		t.Fatal("missing scores")
	}
	if !z.SoftExcluded {
		t.Fatal("zero must exclude")
	}
	if u.SoftExcluded && has(u.ExcludeReasons, ReasonExhausted) {
		t.Fatal("unknown must not be treated as exhausted zero")
	}
	if u.SoftScore <= 0 {
		// unknown gets a small non-zero soft score unless otherwise excluded
		// (may be low after penalty but structure should not force exact 0 unless excluded)
		t.Logf("unknown score=%v reasons=%v", u.SoftScore, u.Reasons)
	}
	found := false
	for _, reason := range u.Reasons {
		if reason == ReasonUnknownEvidence || reason == ReasonNoTelemetry {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unknown reason %v", u.Reasons)
	}
}

func TestNoTelemetry(t *testing.T) {
	now := time.Now().UTC()
	c := Candidate{
		Provider: "codex", Model: "gpt-5.1-codex-mini",
		Windows:             nil,
		ReliabilityEvidence: EvidenceMissing,
	}
	r, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassLuna, Candidates: []Candidate{c}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !has(r.Scores[0].Reasons, ReasonNoTelemetry) {
		t.Fatalf("reasons %v", r.Scores[0].Reasons)
	}
}

func TestReliabilityAndConcurrencySoft(t *testing.T) {
	now := time.Now().UTC()
	base := func(p, m string, rel, load float64) Candidate {
		return Candidate{
			Provider: p, Model: m,
			Windows: []Window{{
				Kind: WindowFiveHour, RemainingFraction: fptr(0.6), Evidence: EvidenceExact,
				TimeToReset: dptr(5 * time.Hour),
			}},
			Reliability: fptr(rel), ReliabilityEvidence: EvidenceExact,
			ConcurrencyLoad: load,
		}
	}
	hi := base("claude", "claude-opus-4-5", 0.95, 0.1)
	lo := base("grok", "grok-4.5", 0.3, 0.9)
	r, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassSoul, Candidates: []Candidate{lo, hi}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if r.Scores[0].Provider != "claude" {
		t.Fatalf("want high reliability first %+v", r.Scores)
	}
}

func TestWeeklyScarcityBinds(t *testing.T) {
	now := time.Now().UTC()
	// Five-hour abundant but weekly scarce → weekly binds
	c := Candidate{
		Provider: "codex", Model: "gpt-5.3-codex",
		Windows: []Window{
			{Kind: WindowFiveHour, RemainingFraction: fptr(0.9), Evidence: EvidenceExact, TimeToReset: dptr(1 * time.Hour)},
			{Kind: WindowWeekly, RemainingFraction: fptr(0.1), Evidence: EvidenceExact, TimeToReset: dptr(48 * time.Hour)},
		},
		Reliability: fptr(0.9), ReliabilityEvidence: EvidenceExact,
	}
	r, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassSoul, Candidates: []Candidate{c}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if r.Scores[0].BindingWindow != WindowWeekly {
		t.Fatalf("binding %s", r.Scores[0].BindingWindow)
	}
	if !has(r.Scores[0].Reasons, ReasonScarceWeekly) {
		t.Fatalf("reasons %v", r.Scores[0].Reasons)
	}
}

func TestUnrelatedSurplusCannotMaskExhausted(t *testing.T) {
	now := time.Now().UTC()
	c := Candidate{
		Provider: "claude", Model: "claude-sonnet-4-5",
		Windows: []Window{
			{Kind: WindowFiveHour, RemainingFraction: fptr(0), Evidence: EvidenceExact, Exhausted: true},
			{Kind: WindowCredit, RemainingFraction: fptr(0.99), Evidence: EvidenceExact},
		},
		ReliabilityEvidence: EvidenceMissing,
	}
	r, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassTera, Candidates: []Candidate{c}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Scores[0].SoftExcluded || !has(r.Scores[0].ExcludeReasons, ReasonExhausted) {
		t.Fatalf("%+v", r.Scores[0])
	}
}

func TestDeterministicReplayAndBreakdown(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	cands := []Candidate{
		{
			Provider: "codex", Model: "gpt-5.2-codex",
			Windows:     []Window{{Kind: WindowFiveHour, RemainingFraction: fptr(0.7), Evidence: EvidenceExact, TimeToReset: dptr(40 * time.Minute)}},
			Reliability: fptr(0.85), ReliabilityEvidence: EvidenceExact,
		},
		{
			Provider: "claude", Model: "claude-haiku-4-5",
			Windows:     []Window{{Kind: WindowFiveHour, RemainingFraction: fptr(0.7), Evidence: EvidenceExact, TimeToReset: dptr(40 * time.Minute)}},
			Reliability: fptr(0.85), ReliabilityEvidence: EvidenceExact,
		},
	}
	in := Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassTera, Candidates: cands, Now: now}
	r1, err := Rank(in)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Rank(in)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Digest != r2.Digest || r1.Digest == "" {
		t.Fatalf("digest %s vs %s", r1.Digest, r2.Digest)
	}
	// Tie → provider order (claude before codex)
	if r1.Scores[0].SoftScore != r1.Scores[1].SoftScore {
		t.Fatalf("expected tie scores")
	}
	if r1.Scores[0].Provider != "claude" {
		t.Fatalf("tie break want claude first got %s", r1.Scores[0].Provider)
	}
	// Breakdown present for V090-053
	if len(r1.Scores[0].Components) < 4 {
		t.Fatalf("components %+v", r1.Scores[0].Components)
	}
	for _, c := range r1.Scores[0].Components {
		if c.Name == "" || c.Weight < 0 {
			t.Fatalf("component %+v", c)
		}
	}
}

func TestCooldownSoftExclude(t *testing.T) {
	now := time.Now().UTC()
	c := Candidate{
		Provider: "grok", Model: "grok-4.5",
		Windows:     []Window{{Kind: WindowFiveHour, RemainingFraction: fptr(0.9), Evidence: EvidenceExact}},
		Reliability: fptr(0.9), ReliabilityEvidence: EvidenceExact,
		CooldownActive: true,
	}
	r, err := Rank(Input{Policy: DefaultPolicy(), TaskClass: capclass.ClassSoul, Candidates: []Candidate{c}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Scores[0].SoftExcluded {
		t.Fatal("cooldown")
	}
}

func has(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}
