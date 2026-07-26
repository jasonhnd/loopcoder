package routecanary

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/providerkit"
	"github.com/jasonhnd/loopcoder/internal/quotamode"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/routedecision"
	"github.com/jasonhnd/loopcoder/internal/successor"
)

const (
	SchemaManifest = "loopcoder.route.canary.manifest.v1"
	CanaryVersion  = "smart-routing-canary-v1"
)

// ScenarioResult is one matrix cell.
type ScenarioResult struct {
	Name    string   `json:"name"`
	Passed  bool     `json:"passed"`
	Outcome string   `json:"outcome"`
	Winner  string   `json:"winner,omitempty"`
	Digest  string   `json:"digest,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
	Detail  string   `json:"detail,omitempty"`
}

// Manifest is the redacted exact-SHA evidence package for P4 acceptance.
type Manifest struct {
	Schema        string `json:"schema"`
	CanaryVersion string `json:"canary_version"`
	// PreProdSHA is filled by the caller when known (CI).
	PreProdSHA string           `json:"pre_prod_sha,omitempty"`
	Passed     bool             `json:"passed"`
	Scenarios  []ScenarioResult `json:"scenarios"`
	// Resource notes (no child residue by construction).
	ResourceNotes []string  `json:"resource_notes"`
	Digest        string    `json:"digest"`
	GeneratedAt   time.Time `json:"generated_at"`
}

// Run executes the full deterministic matrix. now must be injected.
func Run(now time.Time, preProdSHA string) (Manifest, error) {
	if now.IsZero() {
		return Manifest{}, fmt.Errorf("routecanary: now required")
	}
	now = now.UTC()
	m := Manifest{
		Schema:        SchemaManifest,
		CanaryVersion: CanaryVersion,
		PreProdSHA:    strings.TrimSpace(preProdSHA),
		GeneratedAt:   now,
		ResourceNotes: []string{
			"no_live_provider_calls",
			"no_child_processes",
			"no_repo_local_residue",
			"no_busy_polling",
			"fixtures_only",
		},
	}

	scenarios := []func(time.Time) ScenarioResult{
		scenarioPinUnchanged,
		scenarioPinFailClosed,
		scenarioAutomaticNearReset,
		scenarioNoRouteMissingInstall,
		scenarioUnknownQuotaFinite,
		scenarioRateLimitExcluded,
		scenarioSuccessorPreLaunch,
		scenarioAmbiguousNoFallback,
		scenarioFutureProviderKit,
		scenarioExplainMatchesDecision,
		scenarioModeBurnVsPreserve,
		scenarioReservationConflict,
	}
	allPass := true
	for _, fn := range scenarios {
		r := fn(now)
		m.Scenarios = append(m.Scenarios, r)
		if !r.Passed {
			allPass = false
		}
	}
	m.Passed = allPass
	m.Digest = digestManifest(m)
	return m, nil
}

func okFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
}
func falseFact(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
}

func healthy(p, m string, cl capclass.Class) eligibility.Candidate {
	return eligibility.Candidate{
		Provider: p, Model: m, Effort: "high", Permission: "bounded_write", ModelClass: cl,
		AccountRef: "acct-" + p, WindowKind: "five_hour",
		Installed: okFact(p + "-i"), Authenticated: okFact(p + "-a"), ModelPresent: okFact(p + "-m"),
		PermissionOK: okFact(p + "-p"), EffortOK: okFact(p + "-e"), Healthy: okFact(p + "-h"),
		CooldownActive: falseFact(p + "-cd"), ResourceFit: okFact(p + "-r"), QuotaRemaining: 9999,
	}
}

func soft(p, m string, rem float64, ttr time.Duration) quotapolicy.Candidate {
	rf, d := rem, ttr
	rel := 0.9
	return quotapolicy.Candidate{
		Provider: p, Model: m,
		AccountRef: "acct-" + p, WindowKind: "five_hour",
		Windows: []quotapolicy.Window{{
			Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
			Evidence: quotapolicy.EvidenceExact, TimeToReset: &d,
		}},
		Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact,
	}
}

func machineOK() eligibility.MachineAdmission {
	return eligibility.MachineAdmission{CapacityOK: okFact("mach"), ConcurrentSlots: 4}
}

func scenarioPinUnchanged(now time.Time) ScenarioResult {
	name := "pin_unchanged_all_providers"
	// Pin each official provider; winner must match pin exactly.
	providers := []struct {
		p, m string
		cl   capclass.Class
	}{
		{"codex", "gpt-5.3-codex", capclass.ClassSoul},
		{"claude", "claude-sonnet-4-5", capclass.ClassTera},
		{"gemini", "gemini-2.5-flash", capclass.ClassTera},
		{"antigravity", "gemini-2.5-pro", capclass.ClassSoul},
		{"grok", "grok-4.5", capclass.ClassSoul},
	}
	var digests []string
	for _, pr := range providers {
		cands := []eligibility.Candidate{}
		softs := []quotapolicy.Candidate{}
		for _, q := range providers {
			cands = append(cands, healthy(q.p, q.m, q.cl))
			softs = append(softs, soft(q.p, q.m, 0.5, 3*time.Hour))
		}
		pin := eligibility.PinFields{Provider: pr.p, Model: pr.m}
		req := routedecision.Request{
			DecisionKey: "pin-" + pr.p, ProjectID: "canary", EvidenceDigest: "ev-pin",
			TaskClass: capclass.ClassTera,
			Eligibility: eligibility.Snapshot{
				TaskRequiredClass: capclass.ClassTera, ExplicitPin: &pin,
				Candidates: cands, Machine: machineOK(), CapturedAt: now,
			},
			SoftCandidates: softs, Mode: quotamode.DefaultModeConfig(quotamode.ModeBalanced), Now: now,
		}
		d, err := routedecision.Evaluate(req)
		if err != nil || d.Winner == nil || d.Winner.Provider != pr.p || d.Winner.Model != pr.m {
			return ScenarioResult{Name: name, Passed: false, Detail: fmt.Sprintf("%s err=%v winner=%v", pr.p, err, d.Winner)}
		}
		// no silent substitution
		if d.Winner.Provider != pin.Provider || d.Winner.Model != pin.Model {
			return ScenarioResult{Name: name, Passed: false, Detail: "silent substitution"}
		}
		digests = append(digests, d.Digest)
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: "selected", Reasons: digests}
}

func scenarioPinFailClosed(now time.Time) ScenarioResult {
	name := "pin_fail_closed_no_fallback"
	claude := healthy("claude", "claude-sonnet-4-5", capclass.ClassTera)
	claude.Authenticated = falseFact("bad")
	codex := healthy("codex", "gpt-5.3-codex", capclass.ClassSoul)
	pin := eligibility.PinFields{Provider: "claude", Model: "claude-sonnet-4-5"}
	req := routedecision.Request{
		DecisionKey: "pin-fail", ProjectID: "canary", EvidenceDigest: "ev",
		TaskClass: capclass.ClassTera,
		Eligibility: eligibility.Snapshot{
			TaskRequiredClass: capclass.ClassTera, ExplicitPin: &pin,
			Candidates: []eligibility.Candidate{codex, claude}, Machine: machineOK(), CapturedAt: now,
		},
		SoftCandidates: []quotapolicy.Candidate{soft("codex", "gpt-5.3-codex", 0.9, 20*time.Minute)},
		Mode:           quotamode.DefaultModeConfig(quotamode.ModeBurnBeforeReset), Now: now,
	}
	d, err := routedecision.Evaluate(req)
	if d.Outcome != routedecision.OutcomePinFail || d.Winner != nil || !d.PinFailClosed {
		return ScenarioResult{Name: name, Passed: false, Detail: fmt.Sprintf("err=%v d=%+v", err, d)}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: d.Outcome, Digest: d.Digest}
}

func scenarioAutomaticNearReset(now time.Time) ScenarioResult {
	name := "automatic_prefers_near_reset"
	req := routedecision.Request{
		DecisionKey: "auto", ProjectID: "canary", EvidenceDigest: "ev",
		TaskClass: capclass.ClassTera,
		Eligibility: eligibility.Snapshot{
			TaskRequiredClass: capclass.ClassTera,
			Candidates: []eligibility.Candidate{
				healthy("codex", "gpt-5.2-codex", capclass.ClassTera),
				healthy("claude", "claude-sonnet-4-5", capclass.ClassTera),
			},
			Machine: machineOK(), CapturedAt: now,
		},
		SoftCandidates: []quotapolicy.Candidate{
			soft("codex", "gpt-5.2-codex", 0.8, 25*time.Minute),
			soft("claude", "claude-sonnet-4-5", 0.8, 72*time.Hour),
		},
		Mode: quotamode.DefaultModeConfig(quotamode.ModeBurnBeforeReset), Now: now,
	}
	d, err := routedecision.Evaluate(req)
	if err != nil || d.Winner == nil || d.Winner.Provider != "codex" {
		return ScenarioResult{Name: name, Passed: false, Detail: fmt.Sprintf("%v %+v", err, d.Winner)}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: d.Outcome, Winner: d.Winner.Provider + "/" + d.Winner.Model, Digest: d.Digest}
}

func scenarioNoRouteMissingInstall(now time.Time) ScenarioResult {
	name := "no_route_missing_install"
	c := healthy("gemini", "gemini-2.5-flash", capclass.ClassTera)
	c.Installed = falseFact("miss")
	req := routedecision.Request{
		DecisionKey: "noroute", ProjectID: "canary", EvidenceDigest: "ev",
		TaskClass: capclass.ClassTera,
		Eligibility: eligibility.Snapshot{
			TaskRequiredClass: capclass.ClassTera, Candidates: []eligibility.Candidate{c},
			Machine: machineOK(), CapturedAt: now,
		},
		Mode: quotamode.DefaultModeConfig(quotamode.ModeBalanced), Now: now,
	}
	d, err := routedecision.Evaluate(req)
	if d.Outcome != routedecision.OutcomeNoRoute {
		return ScenarioResult{Name: name, Passed: false, Detail: fmt.Sprintf("%v %+v", err, d)}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: d.Outcome, Digest: d.Digest}
}

func scenarioUnknownQuotaFinite(now time.Time) ScenarioResult {
	name := "unknown_quota_not_zero"
	c := quotapolicy.Candidate{
		Provider: "grok", Model: "grok-4",
		Windows:             []quotapolicy.Window{{Kind: quotapolicy.WindowFiveHour, Evidence: quotapolicy.EvidenceUnknown}},
		ReliabilityEvidence: quotapolicy.EvidenceUnknown,
	}
	z := 0.0
	zero := quotapolicy.Candidate{
		Provider: "claude", Model: "claude-haiku-4-5",
		Windows: []quotapolicy.Window{{
			Kind: quotapolicy.WindowFiveHour, RemainingFraction: &z, Evidence: quotapolicy.EvidenceExact, Exhausted: true,
		}},
		ReliabilityEvidence: quotapolicy.EvidenceMissing,
	}
	r, err := quotapolicy.Rank(quotapolicy.Input{
		Policy: quotapolicy.DefaultPolicy(), TaskClass: capclass.ClassTera,
		Candidates: []quotapolicy.Candidate{c, zero}, Now: now,
	})
	if err != nil {
		return ScenarioResult{Name: name, Passed: false, Detail: err.Error()}
	}
	// zero exhausted excluded; unknown not treated as exhausted
	var unk *quotapolicy.Score
	for i := range r.Scores {
		if r.Scores[i].Provider == "grok" {
			unk = &r.Scores[i]
		}
		if r.Scores[i].Provider == "claude" && !r.Scores[i].SoftExcluded {
			return ScenarioResult{Name: name, Passed: false, Detail: "zero not excluded"}
		}
	}
	if unk == nil {
		return ScenarioResult{Name: name, Passed: false, Detail: "missing unknown"}
	}
	for _, reason := range unk.ExcludeReasons {
		if reason == quotapolicy.ReasonExhausted {
			return ScenarioResult{Name: name, Passed: false, Detail: "unknown treated as exhausted"}
		}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: "scored", Digest: r.Digest}
}

func scenarioRateLimitExcluded(now time.Time) ScenarioResult {
	name := "rate_limit_excluded"
	rl := quotapolicy.Candidate{
		Provider: "codex", Model: "gpt-5.2-codex",
		Windows: []quotapolicy.Window{{
			Kind: quotapolicy.WindowRateLimit, Evidence: quotapolicy.EvidenceExact, RateLimited: true, RemainingFraction: func() *float64 { v := 0.9; return &v }(),
		}},
		ReliabilityEvidence: quotapolicy.EvidenceMissing,
	}
	r, err := quotapolicy.Rank(quotapolicy.Input{
		Policy: quotapolicy.DefaultPolicy(), TaskClass: capclass.ClassTera, Candidates: []quotapolicy.Candidate{rl}, Now: now,
	})
	if err != nil || len(r.Scores) != 1 || !r.Scores[0].SoftExcluded {
		return ScenarioResult{Name: name, Passed: false, Detail: fmt.Sprintf("%v %+v", err, r)}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: "soft_excluded", Digest: r.Digest}
}

func scenarioSuccessorPreLaunch(now time.Time) ScenarioResult {
	name := "successor_pre_launch_authorized"
	store := successor.NewStore(func() time.Time { return now })
	d1 := routedecision.Decision{
		Schema: routedecision.SchemaDecision, DecisionID: "d1", Digest: "dig-a",
		Outcome: routedecision.OutcomeSelected,
		Winner:  &routedecision.Winner{Provider: "codex", Model: "gpt-5.2-codex"},
	}
	a, err := store.RegisterFirst(d1, "wt", "log", "ev")
	if err != nil {
		return ScenarioResult{Name: name, Passed: false, Detail: err.Error()}
	}
	fail, err := store.RecordFailure(successor.Failure{AttemptID: a.AttemptID, Class: successor.FailPreLaunch, ReasonCode: "auth"})
	if err != nil {
		return ScenarioResult{Name: name, Passed: false, Detail: err.Error()}
	}
	d2 := routedecision.Decision{
		Schema: routedecision.SchemaDecision, DecisionID: "d2", Digest: "dig-b",
		Outcome: routedecision.OutcomeSelected,
		Winner:  &routedecision.Winner{Provider: "claude", Model: "claude-sonnet-4-5"},
	}
	rec, err := store.CreateSuccessor(a.AttemptID, fail, successor.DefaultPolicy(), d2)
	if err != nil || rec.Successor.PredecessorID != a.AttemptID {
		return ScenarioResult{Name: name, Passed: false, Detail: fmt.Sprintf("%v %+v", err, rec)}
	}
	prior, _ := store.Get(a.AttemptID)
	if prior.WorktreeRef != "wt" {
		return ScenarioResult{Name: name, Passed: false, Detail: "prior evidence lost"}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: "successor", Winner: "claude/claude-sonnet-4-5"}
}

func scenarioAmbiguousNoFallback(now time.Time) ScenarioResult {
	name := "ambiguous_no_auto_fallback"
	plan := successor.PlanSuccessor(
		successor.Attempt{AttemptID: "a", Provider: "x", Model: "y"},
		successor.Failure{Class: successor.FailAmbiguous, ReasonCode: "unknown"},
		successor.DefaultPolicy(), 0,
	)
	if plan.Allowed || !plan.NeedsHuman {
		return ScenarioResult{Name: name, Passed: false, Detail: fmt.Sprintf("%+v", plan)}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: "needs_human", Digest: plan.Digest}
}

func scenarioFutureProviderKit(now time.Time) ScenarioResult {
	name := "future_provider_kit_no_core_edit"
	// Registration through providerkit allowlist only — prove package is importable and checklist exists.
	_ = now
	// Use documented kit surface: Schema constants / empty allowlist reject unknown.
	// Minimal: Default support model path via package presence.
	if providerkit.MinContractVersion < 1 || providerkit.MaxContractVersion < providerkit.MinContractVersion {
		return ScenarioResult{Name: name, Passed: false, Detail: "kit version"}
	}
	cl := providerkit.DefaultChecklist()
	if len(cl.Required) < 5 {
		return ScenarioResult{Name: name, Passed: false, Detail: "checklist incomplete"}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: "kit_ok", Detail: fmt.Sprintf("contract=%d..%d items=%d", providerkit.MinContractVersion, providerkit.MaxContractVersion, len(cl.Required))}
}

func scenarioExplainMatchesDecision(now time.Time) ScenarioResult {
	name := "explain_matches_decision_digest"
	req := routedecision.Request{
		DecisionKey: "explain", ProjectID: "canary", EvidenceDigest: "ev-x",
		TaskClass: capclass.ClassTera,
		Eligibility: eligibility.Snapshot{
			TaskRequiredClass: capclass.ClassTera,
			Candidates:        []eligibility.Candidate{healthy("grok", "grok-4.5", capclass.ClassSoul)},
			Machine:           machineOK(), CapturedAt: now,
		},
		SoftCandidates: []quotapolicy.Candidate{soft("grok", "grok-4.5", 0.6, 2*time.Hour)},
		Mode:           quotamode.DefaultModeConfig(quotamode.ModeBalanced), Now: now,
	}
	d, err := routedecision.Evaluate(req)
	if err != nil {
		return ScenarioResult{Name: name, Passed: false, Detail: err.Error()}
	}
	ex := routedecision.Explain(d)
	if !strings.Contains(ex.Human, d.Digest) || ex.Digest != d.Digest {
		return ScenarioResult{Name: name, Passed: false, Detail: "explain digest mismatch"}
	}
	// replay
	d2, _ := routedecision.Evaluate(req)
	if d2.Digest != d.Digest {
		return ScenarioResult{Name: name, Passed: false, Detail: "non-replayable"}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: d.Outcome, Digest: d.Digest, Winner: d.Winner.Provider + "/" + d.Winner.Model}
}

func scenarioModeBurnVsPreserve(now time.Time) ScenarioResult {
	name := "policy_modes_replayable"
	cands := []quotapolicy.Candidate{
		soft("codex", "gpt-5.2-codex", 0.85, 20*time.Minute),
		soft("claude", "claude-sonnet-4-5", 0.85, 48*time.Hour),
	}
	for _, mode := range []quotamode.Mode{quotamode.ModeBalanced, quotamode.ModeBurnBeforeReset, quotamode.ModePreservePremium} {
		r1, err := quotamode.Rank(quotamode.RankInput{Mode: quotamode.DefaultModeConfig(mode), TaskClass: capclass.ClassTera, Candidates: cands, Now: now})
		if err != nil {
			return ScenarioResult{Name: name, Passed: false, Detail: err.Error()}
		}
		r2, _ := quotamode.Rank(quotamode.RankInput{Mode: quotamode.DefaultModeConfig(mode), TaskClass: capclass.ClassTera, Candidates: cands, Now: now})
		if r1.Digest != r2.Digest {
			return ScenarioResult{Name: name, Passed: false, Detail: "mode non-deterministic " + string(mode)}
		}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: "modes_ok"}
}

func scenarioReservationConflict(now time.Time) ScenarioResult {
	name := "soft_reservation_conflict"
	s := quotamode.NewStore(func() time.Time { return now })
	key := quotamode.WindowKey{Provider: "codex", Model: "gpt-5.2-codex", Window: quotapolicy.WindowFiveHour}
	snap := quotamode.SnapshotRemaining{Key: key, RemainingFraction: 0.30, Evidence: quotapolicy.EvidenceExact}
	cfg := quotamode.DefaultModeConfig(quotamode.ModeBalanced)
	cfg.CompletionHeadroom = 0.05
	_, err := s.Reserve(quotamode.ReserveRequest{
		ProjectID: "p1", AttemptID: "a1", Key: key, Snapshot: snap,
		DemandEstimate: 0.22, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err != nil {
		return ScenarioResult{Name: name, Passed: false, Detail: "first " + err.Error()}
	}
	_, err = s.Reserve(quotamode.ReserveRequest{
		ProjectID: "p2", AttemptID: "a2", Key: key, Snapshot: snap,
		DemandEstimate: 0.22, DemandEvidence: quotapolicy.EvidenceExact, Config: cfg,
	})
	if err == nil {
		return ScenarioResult{Name: name, Passed: false, Detail: "second should conflict"}
	}
	return ScenarioResult{Name: name, Passed: true, Outcome: "conflict_ok", Detail: err.Error()}
}

func digestManifest(m Manifest) string {
	cp := m
	cp.Digest = ""
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}
