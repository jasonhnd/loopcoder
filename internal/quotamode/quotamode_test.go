package quotamode

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

func fptr(v float64) *float64             { return &v }
func dptr(d time.Duration) *time.Duration { return &d }

func cand(p, m string, rem float64, ttr time.Duration) quotapolicy.Candidate {
	return quotapolicy.Candidate{
		Provider: p, Model: m,
		Windows: []quotapolicy.Window{{
			Kind: quotapolicy.WindowFiveHour, RemainingFraction: fptr(rem),
			Evidence: quotapolicy.EvidenceExact, TimeToReset: dptr(ttr),
		}},
		Reliability: fptr(0.9), ReliabilityEvidence: quotapolicy.EvidenceExact,
	}
}

func TestConcurrentReservationConflict(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock := now
	s := NewStore(func() time.Time { return clock })
	key := WindowKey{Provider: "codex", Model: "gpt-5.2-codex", Window: quotapolicy.WindowFiveHour}
	snap := SnapshotRemaining{Key: key, RemainingFraction: 0.30, Evidence: quotapolicy.EvidenceExact, EvidenceID: "e1"}
	cfg := DefaultModeConfig(ModeBalanced)
	cfg.CompletionHeadroom = 0.05

	// First hold 0.20 of window
	r1, err := s.Reserve(ReserveRequest{
		ProjectID: "p1", AttemptID: "a1", Key: key, Snapshot: snap,
		DemandEstimate: 0.20, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r1.State != StateActive {
		t.Fatalf("r1 %+v", r1)
	}

	// Second concurrent full claim should fail (remaining 0.30 - 0.20 active - 0.05 headroom = 0.05 < 0.20)
	_, err = s.Reserve(ReserveRequest{
		ProjectID: "p2", AttemptID: "a2", Key: key, Snapshot: snap,
		DemandEstimate: 0.20, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err == nil {
		t.Fatal("expected conflict or headroom")
	}
	if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrHeadroom) {
		t.Fatalf("err %v", err)
	}

	// Small second claim may fit
	r2, err := s.Reserve(ReserveRequest{
		ProjectID: "p2", AttemptID: "a2b", Key: key, Snapshot: snap,
		DemandEstimate: 0.04, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err != nil {
		t.Fatalf("small claim: %v", err)
	}
	if r2.State != StateActive {
		t.Fatalf("r2 %+v", r2)
	}
}

func TestRaceTwoProjectsDeterministic(t *testing.T) {
	// Serial equivalent of race: same snapshot, two equal claims, only first wins.
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	key := WindowKey{Provider: "claude", Model: "claude-sonnet-4-5", Window: quotapolicy.WindowFiveHour}
	snap := SnapshotRemaining{Key: key, RemainingFraction: 0.50, Evidence: quotapolicy.EvidenceExact}
	cfg := DefaultModeConfig(ModeBalanced)
	cfg.CompletionHeadroom = 0.05

	var mu sync.Mutex
	var wins int
	try := func(proj, att string) {
		_, err := s.Reserve(ReserveRequest{
			ProjectID: proj, AttemptID: att, Key: key, Snapshot: snap,
			DemandEstimate: 0.40, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
		})
		mu.Lock()
		if err == nil {
			wins++
		}
		mu.Unlock()
	}
	try("p1", "a1")
	try("p2", "a2")
	if wins != 1 {
		t.Fatalf("wins=%d want 1", wins)
	}
}

func TestPolicyModesReplayable(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	cands := []quotapolicy.Candidate{
		cand("codex", "gpt-5.2-codex", 0.8, 30*time.Minute),
		cand("claude", "claude-sonnet-4-5", 0.8, 48*time.Hour),
	}
	for _, mode := range []Mode{ModeBalanced, ModeBurnBeforeReset, ModePreservePremium} {
		r1, err := Rank(RankInput{Mode: DefaultModeConfig(mode), TaskClass: capclass.ClassTera, Candidates: cands, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		r2, err := Rank(RankInput{Mode: DefaultModeConfig(mode), TaskClass: capclass.ClassTera, Candidates: cands, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if r1.Digest != r2.Digest {
			t.Fatalf("mode %s non-deterministic", mode)
		}
		if r1.Mode != mode || len(r1.Scores) != 2 {
			t.Fatalf("%+v", r1)
		}
	}
	// Burn mode should prefer near-reset codex more strongly than preserve
	burn, _ := Rank(RankInput{Mode: DefaultModeConfig(ModeBurnBeforeReset), TaskClass: capclass.ClassTera, Candidates: cands, Now: now})
	if burn.Scores[0].Provider != "codex" {
		t.Fatalf("burn want codex first %+v", burn.Scores)
	}
}

func TestHeadroomReasons(t *testing.T) {
	now := time.Now().UTC()
	s := NewStore(func() time.Time { return now })
	key := WindowKey{Provider: "grok", Model: "grok-4", Window: quotapolicy.WindowWeekly}
	cfg := DefaultModeConfig(ModeBalanced)

	// exact exhaustion
	_, err := s.Reserve(ReserveRequest{
		ProjectID: "p", AttemptID: "a", Key: key,
		Snapshot:       SnapshotRemaining{Key: key, RemainingFraction: 0, Evidence: quotapolicy.EvidenceExact},
		DemandEstimate: 0.1, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err == nil || !errors.Is(err, ErrHeadroom) {
		t.Fatalf("exhausted: %v", err)
	}

	// unknown quota
	_, err = s.Reserve(ReserveRequest{
		ProjectID: "p", AttemptID: "a2", Key: key,
		Snapshot:       SnapshotRemaining{Key: key, Evidence: quotapolicy.EvidenceUnknown},
		DemandEstimate: 0.1, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err == nil || !errors.Is(err, ErrHeadroom) {
		t.Fatalf("unknown: %v", err)
	}

	// owner risk acceptance on unknown
	r, err := s.Reserve(ReserveRequest{
		ProjectID: "p", AttemptID: "a3", Key: key,
		Snapshot:       SnapshotRemaining{Key: key, Evidence: quotapolicy.EvidenceUnknown},
		DemandEstimate: 0.1, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
		RiskAccepted: true, RiskActor: "owner", RiskReason: "accept unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.RiskAccepted {
		t.Fatal("risk")
	}
}

func TestReleaseCancelExpireReconcileIdempotent(t *testing.T) {
	clock := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return clock })
	key := WindowKey{Provider: "gemini", Model: "gemini-2.5-flash", Window: quotapolicy.WindowFiveHour}
	cfg := DefaultModeConfig(ModeBalanced)
	cfg.SoftTTL = 10 * time.Minute
	r, err := s.Reserve(ReserveRequest{
		ProjectID: "p", AttemptID: "a", Key: key,
		Snapshot:       SnapshotRemaining{Key: key, RemainingFraction: 0.9, Evidence: quotapolicy.EvidenceExact},
		DemandEstimate: 0.2, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Release twice idempotent
	r1, err := s.Release(r.ID, "timeout")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Release(r.ID, "timeout-again")
	if err != nil {
		t.Fatal(err)
	}
	if r1.State != StateReleased || r2.State != StateReleased {
		t.Fatalf("%+v %+v", r1, r2)
	}

	// Fresh reservation then expire via clock
	r3, err := s.Reserve(ReserveRequest{
		ProjectID: "p", AttemptID: "a2", Key: key,
		Snapshot:       SnapshotRemaining{Key: key, RemainingFraction: 0.9, Evidence: quotapolicy.EvidenceExact},
		DemandEstimate: 0.1, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(20 * time.Minute)
	n := s.ExpireStale()
	if n < 1 {
		t.Fatalf("expired count %d", n)
	}
	got, _ := s.Get(r3.ID)
	if got.State != StateExpired {
		t.Fatalf("state %s", got.State)
	}

	// Reconcile path
	clock = time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	r4, err := s.Reserve(ReserveRequest{
		ProjectID: "p", AttemptID: "a3", Key: key,
		Snapshot:       SnapshotRemaining{Key: key, RemainingFraction: 0.9, Evidence: quotapolicy.EvidenceExact},
		DemandEstimate: 0.15, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	attr, err := s.Reconcile(r4.ID, 0.12, "local_tokens", quotapolicy.EvidenceEstimated)
	if err != nil {
		t.Fatal(err)
	}
	if attr.Drift >= 0 && attr.ObservedFraction != 0.12 {
		t.Fatalf("%+v", attr)
	}
	// drift = 0.12 - 0.15 = -0.03
	if attr.Drift > -0.029 || attr.Drift < -0.031 {
		t.Fatalf("drift %v", attr.Drift)
	}
	if attr.Note == "" || attr.Confidence != quotapolicy.EvidenceEstimated {
		t.Fatalf("%+v", attr)
	}
	// idempotent reconcile
	attr2, err := s.Reconcile(r4.ID, 0.99, "local_tokens", quotapolicy.EvidenceEstimated)
	if err != nil {
		t.Fatal(err)
	}
	if attr2.ID != attr.ID {
		t.Fatal("reconcile should be idempotent")
	}
}

func TestPinNotSubstitutedByMode(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	cands := []quotapolicy.Candidate{
		cand("codex", "gpt-5.2-codex", 0.9, 20*time.Minute), // would win burn mode
		cand("claude", "claude-sonnet-4-5", 0.5, 48*time.Hour),
	}
	pin := &struct{ Provider, Model string }{"claude", "claude-sonnet-4-5"}
	r, err := Rank(RankInput{
		Mode: DefaultModeConfig(ModeBurnBeforeReset), TaskClass: capclass.ClassTera,
		Candidates: cands, Now: now, ExplicitPin: pin,
	})
	if err != nil {
		t.Fatal(err)
	}
	// pin sticky
	var pinned *AdjustedScore
	for i := range r.Scores {
		if r.Scores[i].Provider == "claude" {
			pinned = &r.Scores[i]
		}
		if r.Scores[i].Provider == "codex" && !r.Scores[i].SoftExcluded {
			t.Fatal("codex must not compete under pin")
		}
	}
	if pinned == nil || pinned.AdjustedScore != 1.0 {
		t.Fatalf("pin %+v", pinned)
	}
}

func TestReservationPressureOnRank(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	key := WindowKey{Provider: "codex", Model: "gpt-5.2-codex", Window: quotapolicy.WindowFiveHour}
	_, err := s.Reserve(ReserveRequest{
		ProjectID: "p", AttemptID: "a", Key: key,
		Snapshot:       SnapshotRemaining{Key: key, RemainingFraction: 0.9, Evidence: quotapolicy.EvidenceExact},
		DemandEstimate: 0.5, DemandEvidence: quotapolicy.EvidenceExact,
		Config: DefaultModeConfig(ModeBalanced),
	})
	if err != nil {
		t.Fatal(err)
	}
	cands := []quotapolicy.Candidate{
		cand("codex", "gpt-5.2-codex", 0.8, 30*time.Minute),
		cand("claude", "claude-sonnet-4-5", 0.8, 30*time.Minute),
	}
	r, err := Rank(RankInput{
		Mode: DefaultModeConfig(ModeBalanced), TaskClass: capclass.ClassTera,
		Candidates: cands, Store: s, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// codex has reservation pressure → claude should rank higher or codex lower adjusted
	var codex, claude AdjustedScore
	for _, sc := range r.Scores {
		if sc.Provider == "codex" {
			codex = sc
		}
		if sc.Provider == "claude" {
			claude = sc
		}
	}
	if codex.ReservedFraction <= 0 {
		t.Fatal("expected reserved pressure")
	}
	if codex.AdjustedScore >= codex.BaseSoftScore {
		t.Fatalf("adjusted should drop: base=%v adj=%v", codex.BaseSoftScore, codex.AdjustedScore)
	}
	_ = claude
}

func TestCancel(t *testing.T) {
	now := time.Now().UTC()
	s := NewStore(func() time.Time { return now })
	key := WindowKey{Provider: "antigravity", Model: "gemini-2.5-pro", Window: quotapolicy.WindowFiveHour}
	r, err := s.Reserve(ReserveRequest{
		ProjectID: "p", AttemptID: "a", Key: key,
		Snapshot:       SnapshotRemaining{Key: key, RemainingFraction: 0.7, Evidence: quotapolicy.EvidenceExact},
		DemandEstimate: 0.1, DemandEvidence: quotapolicy.EvidenceExact,
		Config: DefaultModeConfig(ModeBalanced),
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Cancel(r.ID, "start_refusal")
	if err != nil || c.State != StateCancelled {
		t.Fatalf("%+v %v", c, err)
	}
}
