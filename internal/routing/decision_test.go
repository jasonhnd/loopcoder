package routing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/orchestration"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestRouteDecisionScoresOnlyEligibleCandidatesAndExplainsCandidates(t *testing.T) {
	fixture := newFixture(t)
	scores := fixture.availabilityScores()
	for i := range scores {
		scores[i].ScoreConfidence = providerinventory.ConfidenceExact
		scores[i].Score = 75
		if scores[i].Scope.AdapterID == "claude" {
			scores[i].Score = 95
		}
	}
	planFingerprint := testFingerprint("plan-route-decision")
	req := decisionRequirement(t, fixture, workerRequirement("task-route-decision"), "treq-route-decision", planFingerprint)
	input := DecisionInput{
		ProjectID:         "proj-routing",
		DeliveryRunID:     "drun-routing",
		DecisionKey:       "route-worker",
		TaskRequirementID: req.TaskRequirementID,
		RoleDefinitionID:  "role-worker",
		PlanFingerprint:   planFingerprint,
		DecidedBy:         routerActor(),
		Host:              routingHost(),
		Now:               fixture.now,
		Inputs: Inputs{
			Requirement: req,
			Candidates: []Candidate{
				fixture.candidate("codex", "acct-a", "codex-good"),
				fixture.candidate("claude", "acct-c", "claude-good"),
				fixture.candidate("codex", "acct-a", "codex-broken"),
			},
			Inventory:       fixture.inventory,
			Availability:    scores,
			Budgets:         fixture.budgets,
			RuntimeContract: fixture.contract,
			HostName:        "codex-cli",
			Policy: Policy{
				EvidencePolicy:              EvidenceAllowEstimated,
				RequireAvailabilityEvidence: true,
				RequireBudgetEvidence:       true,
			},
		},
	}
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusSelected || decision.ChosenCandidateID == "" {
		t.Fatalf("decision status/chosen = %s/%s", decision.DecisionStatus, decision.ChosenCandidateID)
	}
	if containsScoredCandidate(decision.ScoredCandidates, fixture.candidate("codex", "acct-a", "codex-broken").RoutingCandidateID) {
		t.Fatalf("ineligible broken candidate was scored: %#v", decision.ScoredCandidates)
	}
	human := ExplainHuman(decision)
	for _, want := range []string{"selected " + decision.ChosenCandidateID, "codex-broken", "model-unavailable"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human explain missing %q:\n%s", want, human)
		}
	}
	stable, err := ExplainJSON(decision)
	if err != nil {
		t.Fatalf("ExplainJSON: %v", err)
	}
	if !strings.Contains(string(stable), `"scored_candidates"`) || !strings.Contains(string(stable), `"rejected_candidates"`) {
		t.Fatalf("stable explain JSON missing scored/rejected candidates: %s", stable)
	}
	if strings.Contains(string(stable), `"alternatives"`) {
		t.Fatalf("stable explain JSON includes non-v1 alternatives field: %s", stable)
	}
}

func TestRouteDecisionGrokUsesOrdinaryEligibilityContracts(t *testing.T) {
	fixture := newFixture(t)
	fixture = withGrokOrdinaryWorkerFixture(fixture)
	planFingerprint := testFingerprint("plan-grok-ordinary-route")
	req := decisionRequirement(t, fixture, workerRequirement("task-grok-route"), "treq-grok-route", planFingerprint)
	grok := fixture.candidate("grok", "acct-grok", "grok-good")
	codex := fixture.candidate("codex", "acct-a", "codex-good")
	scores := fixture.availabilityScores()
	for i := range scores {
		scores[i].ScoreConfidence = providerinventory.ConfidenceExact
		switch scores[i].Scope.AdapterID {
		case "grok":
			scores[i].Score = 98
		case "codex":
			scores[i].Score = 25
		default:
			scores[i].Score = 10
		}
	}
	input := DecisionInput{
		ProjectID:         "proj-routing",
		DeliveryRunID:     "drun-routing",
		DecisionKey:       "route-worker-grok-ordinary",
		TaskRequirementID: req.TaskRequirementID,
		RoleDefinitionID:  "role-worker",
		PlanFingerprint:   planFingerprint,
		DecidedBy:         routerActor(),
		Host:              routingHost(),
		Now:               fixture.now,
		Inputs: Inputs{
			Requirement: req,
			Candidates: []Candidate{
				grok,
				codex,
			},
			Inventory:       fixture.inventory,
			Availability:    scores,
			Budgets:         fixture.budgets,
			RuntimeContract: fixture.contract,
			HostName:        "codex-cli",
			Policy: Policy{
				EvidencePolicy:              EvidenceAllowEstimated,
				RequireAvailabilityEvidence: true,
				RequireBudgetEvidence:       true,
			},
		},
	}
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusSelected || !strings.Contains(decision.ChosenReason, "selected") {
		t.Fatalf("decision = %#v, want ordinary selected route", decision)
	}
	if selected := selectedCandidate(decision); selected.AdapterID != "grok" || selected.ModelCapabilityID != "grok-good" {
		t.Fatalf("selected candidate = %#v, want eligible Grok ordinary worker", selected)
	}
	if len(decision.EligibleCandidates) != 2 || len(decision.ScoredCandidates) != 2 {
		t.Fatalf("eligible/scored candidates = %#v/%#v, want Grok and Codex scored through common router", decision.EligibleCandidates, decision.ScoredCandidates)
	}
	grokScore := decision.ScoredCandidates[0]
	if grokScore.Candidate.AdapterID != "grok" {
		t.Fatalf("top scored candidate = %#v, want Grok", grokScore)
	}
	for _, want := range []struct {
		kind string
		id   string
	}{
		{"routing_candidate", grok.RoutingCandidateID},
		{"quota_snapshot", "qsnap-grok-good"},
		{"budget_policy", "bpol-grok"},
		{"availability_score", "avscore-grok-good-acct-grok"},
	} {
		if !hasInputRecordRef(decision.InputRecordRefs, want.kind, want.id) {
			t.Fatalf("decision input refs missing %s/%s: %#v", want.kind, want.id, decision.InputRecordRefs)
		}
	}
	stable, err := ExplainJSON(decision)
	if err != nil {
		t.Fatalf("ExplainJSON: %v", err)
	}
	for _, want := range []string{`"adapter_id":"grok"`, `"record_id":"qsnap-grok-good"`, `"record_id":"bpol-grok"`} {
		if !strings.Contains(string(stable), want) {
			t.Fatalf("stable explain missing %q:\n%s", want, string(stable))
		}
	}
}

func TestRouteDecisionAbsentGrokRejectedWithoutHarmingCodex(t *testing.T) {
	fixture := newFixture(t)
	fixture.contract.Providers = append(fixture.contract.Providers, providerRuntime("grok", true, false, true))
	planFingerprint := testFingerprint("plan-grok-absent-route")
	req := decisionRequirement(t, fixture, workerRequirement("task-grok-absent-route"), "treq-grok-absent-route", planFingerprint)
	grok := fixture.candidate("grok", "acct-grok", "grok-missing")
	codex := fixture.candidate("codex", "acct-a", "codex-good")
	scores := fixture.availabilityScores()
	for i := range scores {
		scores[i].ScoreConfidence = providerinventory.ConfidenceExact
		scores[i].Score = 90
	}
	input := DecisionInput{
		ProjectID:         "proj-routing",
		DeliveryRunID:     "drun-routing",
		DecisionKey:       "route-worker-grok-absent",
		TaskRequirementID: req.TaskRequirementID,
		RoleDefinitionID:  "role-worker",
		PlanFingerprint:   planFingerprint,
		DecidedBy:         routerActor(),
		Host:              routingHost(),
		Now:               fixture.now,
		Inputs: Inputs{
			Requirement: req,
			Candidates: []Candidate{
				grok,
				codex,
			},
			Inventory:       fixture.inventory,
			Availability:    scores,
			Budgets:         fixture.budgets,
			RuntimeContract: fixture.contract,
			HostName:        "codex-cli",
			Policy: Policy{
				EvidencePolicy:              EvidenceAllowEstimated,
				RequireAvailabilityEvidence: true,
				RequireBudgetEvidence:       true,
			},
		},
	}
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if selected := selectedCandidate(decision); selected.AdapterID != "codex" {
		t.Fatalf("selected candidate = %#v, want Codex after absent Grok rejection", selected)
	}
	if len(decision.ScoredCandidates) != 1 || decision.ScoredCandidates[0].Candidate.AdapterID != "codex" {
		t.Fatalf("scored candidates = %#v, want only Codex scored", decision.ScoredCandidates)
	}
	var foundGrokRejection bool
	for _, rejected := range decision.RejectedCandidates {
		if rejected.Candidate.AdapterID == "grok" {
			foundGrokRejection = true
			if !hasRejectionCode(rejected.Reasons, RejectAvailabilityHardIneligible) || !hasRejectionCode(rejected.Reasons, RejectAuthNotReady) || !hasRejectionCode(rejected.Reasons, RejectModelUnavailable) {
				t.Fatalf("grok rejection lacks absent-provider contract reasons: %#v", rejected)
			}
		}
	}
	if !foundGrokRejection {
		t.Fatalf("missing grok rejection in %#v", decision.RejectedCandidates)
	}
}

func TestRouteDecisionQuotaAndCostEvidenceStayWithScoreProducingRecord(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	lowQuota := quota("qsnap-low-exact", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 10, fixture.now)
	highQuota := quota("qsnap-high-estimated", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceEstimated, providerinventory.FreshnessFresh, 80, fixture.now)
	candidate.QuotaSnapshotIDs = []string{lowQuota.QuotaSnapshotID, highQuota.QuotaSnapshotID}
	lowBudget := budgetSummary("bpol-low-exact", "codex", "acct-a", "codex-good", 10)
	highBudget := budgetSummary("bpol-high-estimated", "codex", "acct-a", "codex-good", 90)
	highBudget.Confidence = providerinventory.ConfidenceEstimated
	candidate.BudgetPolicyIDs = []string{lowBudget.BudgetPolicyID, highBudget.BudgetPolicyID}
	candidate.RoutingCandidateID = candidateID(candidate)
	candidate.CandidateFingerprint = candidateFingerprint(candidate)
	input.Inputs.Candidates = []Candidate{candidate}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, lowQuota, highQuota)
	input.Inputs.Budgets = append(input.Inputs.Budgets, lowBudget, highBudget)

	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if len(decision.ScoredCandidates) != 1 {
		t.Fatalf("scored candidates = %#v, want one", decision.ScoredCandidates)
	}
	trustScore := componentByName(t, decision.ScoredCandidates[0].Components, ComponentCapacityTrust)
	if trustScore.Confidence != providerinventory.ConfidenceEstimated || !trustScore.Heuristic || trustScore.EvidenceValue == nil || *trustScore.EvidenceValue != 80 || len(trustScore.SnapshotIDs) != 1 || trustScore.SnapshotIDs[0] != highQuota.QuotaSnapshotID {
		t.Fatalf("capacity trust component mixed evidence: %#v", trustScore)
	}
	costScore := componentByName(t, decision.ScoredCandidates[0].Components, ComponentCost)
	if costScore.Confidence != providerinventory.ConfidenceEstimated || !costScore.Heuristic || costScore.EvidenceValue == nil || *costScore.EvidenceValue != 90 || len(costScore.EvidenceRecordIDs) != 1 || costScore.EvidenceRecordIDs[0] != highBudget.BudgetPolicyID {
		t.Fatalf("cost component mixed evidence: %#v", costScore)
	}
}

func TestRouteDecisionScoreEvidenceTieBreaksDeterministically(t *testing.T) {
	fixture := newFixture(t)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	exactQuota := quota("qsnap-b-exact", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 50, fixture.now)
	estimatedQuota := quota("qsnap-a-estimated", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceEstimated, providerinventory.FreshnessFresh, 50, fixture.now)
	candidate.QuotaSnapshotIDs = []string{estimatedQuota.QuotaSnapshotID, exactQuota.QuotaSnapshotID}
	quotaScore := scoreQuota(candidate, mapQuotaSnapshots([]providerinventory.QuotaSnapshot{estimatedQuota, exactQuota}), OptimizationPolicy{Weights: map[ComponentName]int{ComponentQuotaHeadroom: 20}})
	if quotaScore.Confidence != providerinventory.ConfidenceExact || len(quotaScore.SnapshotIDs) != 1 || quotaScore.SnapshotIDs[0] != exactQuota.QuotaSnapshotID {
		t.Fatalf("quota equal-value confidence tie-break = %#v", quotaScore)
	}

	exactBudget := budgetSummary("bpol-b-exact", "codex", "acct-a", "codex-good", 40)
	estimatedBudget := budgetSummary("bpol-a-estimated", "codex", "acct-a", "codex-good", 40)
	estimatedBudget.Confidence = providerinventory.ConfidenceEstimated
	candidate.BudgetPolicyIDs = []string{estimatedBudget.BudgetPolicyID, exactBudget.BudgetPolicyID}
	costScore := scoreCost(candidate, mapBudgets([]budget.Summary{estimatedBudget, exactBudget}), OptimizationPolicy{Weights: map[ComponentName]int{ComponentCost: 10}})
	if costScore.Confidence != providerinventory.ConfidenceExact || len(costScore.EvidenceRecordIDs) != 1 || costScore.EvidenceRecordIDs[0] != exactBudget.BudgetPolicyID {
		t.Fatalf("cost equal-value confidence tie-break = %#v", costScore)
	}
}

func TestRouteDecisionDefaultBalancedV1WeightsAndAvailabilityComponent(t *testing.T) {
	fixture := newFixture(t)
	decision, err := BuildRoutingDecision(replayDecisionInput(fixture))
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	wantWeights := map[ComponentName]int{
		ComponentExpiryUrgency: 15,
		ComponentTaskHeadroom:  15,
		ComponentCapacityTrust: 10,
		ComponentQualityFit:    20,
		ComponentCost:          10,
		ComponentLatency:       10,
		ComponentHealth:        10,
		ComponentDiversity:     10,
	}
	if len(decision.OptimizationPolicy.Weights) != len(wantWeights) {
		t.Fatalf("default weights = %#v, want exactly %#v", decision.OptimizationPolicy.Weights, wantWeights)
	}
	for name, want := range wantWeights {
		if got := decision.OptimizationPolicy.Weights[name]; got != want {
			t.Fatalf("weight %s = %d, want %d", name, got, want)
		}
	}
	for _, candidate := range decision.ScoredCandidates {
		if len(candidate.Components) != len(wantWeights) {
			t.Fatalf("components = %#v, want exactly %d default components", candidate.Components, len(wantWeights))
		}
		if !hasComponent(candidate.Components, ComponentHealth) {
			t.Fatalf("components missing health score: %#v", candidate.Components)
		}
		if hasComponent(candidate.Components, ComponentAvailability) || hasComponent(candidate.Components, ComponentQuotaHeadroom) || hasComponent(candidate.Components, ComponentLocality) || hasComponent(candidate.Components, ComponentUserPreference) {
			t.Fatalf("balanced-v1 silently included non-default component: %#v", candidate.Components)
		}
	}
}

func TestRouteDecisionNearResetBurnWinsOnlyWhenTaskFitsResetBand(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	near := fixture.candidate("codex", "acct-a", "codex-good")
	distant := fixture.candidate("claude", "acct-c", "claude-good")
	nearQuota := quotaWithReset("qsnap-near-80", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 80, fixture.now, fixture.now.Add(10*time.Minute))
	distantQuota := quotaWithReset("qsnap-distant-90", "claude", "pinst-claude", "acct-c", "claude-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 90, fixture.now, fixture.now.Add(2*time.Hour))
	near.QuotaSnapshotIDs = []string{nearQuota.QuotaSnapshotID}
	distant.QuotaSnapshotIDs = []string{distantQuota.QuotaSnapshotID}
	near.BudgetPolicyIDs = []string{"bpol-codex-a"}
	distant.BudgetPolicyIDs = []string{"bpol-claude"}
	input.Inputs.Candidates = []Candidate{near, distant}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, nearQuota, distantQuota)
	input.OptimizationPolicy = OptimizationPolicy{StrategyKey: StrategyBurnBeforeReset}
	req := input.Inputs.Requirement
	req.RiskTier = taskrequirements.RiskLow
	req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
	input.TaskRequirementID = req.TaskRequirementID
	input.Inputs.Requirement = req

	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision very-short: %v", err)
	}
	if selected := selectedCandidate(decision); selected.RoutingCandidateID != near.RoutingCandidateID {
		t.Fatalf("selected = %s, rejected=%#v, want near reset 80%% candidate when task fits under-15m band", selected.RoutingCandidateID, decision.RejectedCandidates)
	}
	nearExpiry := componentByName(t, decision.ScoredCandidates[0].Components, ComponentExpiryUrgency)
	if nearExpiry.ResetWindow != "under-15m" || nearExpiry.TaskClass != "very-short" || nearExpiry.ExpectedWaste == nil {
		t.Fatalf("near expiry component = %#v, want under-15m very-short with expected waste", nearExpiry)
	}

	req.RiskTier = taskrequirements.RiskHigh
	req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
	input.Inputs.Requirement = req
	decision, err = BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision medium: %v", err)
	}
	if selected := selectedCandidate(decision); selected.RoutingCandidateID != distant.RoutingCandidateID {
		t.Fatalf("selected = %s, want distant reset candidate when near under-15m window cannot fit medium task", selected.RoutingCandidateID)
	}
}

func TestRouteDecisionStrategiesAndResetBandsAreDeterministic(t *testing.T) {
	fixture := newFixture(t)
	for _, strategy := range []string{StrategyQualityFirst, StrategyBalanced, StrategyBurnBeforeReset} {
		for _, tc := range []struct {
			name      string
			reset     time.Duration
			wantBand  string
			wantClass string
			risk      taskrequirements.RiskTier
		}{
			{name: "under-15m", reset: 10 * time.Minute, wantBand: "under-15m", wantClass: "very-short", risk: taskrequirements.RiskLow},
			{name: "15-60m", reset: 30 * time.Minute, wantBand: "15-60m", wantClass: "short", risk: taskrequirements.RiskMedium},
			{name: "over-1h", reset: 2 * time.Hour, wantBand: "over-1h", wantClass: "medium", risk: taskrequirements.RiskHigh},
		} {
			t.Run(strategy+"/"+tc.name, func(t *testing.T) {
				input := replayDecisionInput(fixture)
				candidate := fixture.candidate("codex", "acct-a", "codex-good")
				snap := quotaWithReset("qsnap-"+strategy+"-"+tc.name, "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now, fixture.now.Add(tc.reset))
				candidate.QuotaSnapshotIDs = []string{snap.QuotaSnapshotID}
				input.Inputs.Candidates = []Candidate{candidate}
				input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, snap)
				req := input.Inputs.Requirement
				req.RiskTier = tc.risk
				req.QualityFloor = taskrequirements.QualityStandard
				req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
				input.TaskRequirementID = req.TaskRequirementID
				input.Inputs.Requirement = req
				input.OptimizationPolicy = OptimizationPolicy{StrategyKey: strategy}
				decision, err := BuildRoutingDecision(input)
				if err != nil {
					t.Fatalf("BuildRoutingDecision: %v", err)
				}
				if got := decision.OptimizationPolicy.StrategyKey; got != strategy {
					t.Fatalf("strategy = %s, want %s", got, strategy)
				}
				component := componentByName(t, decision.ScoredCandidates[0].Components, ComponentExpiryUrgency)
				if component.ResetWindow != tc.wantBand || component.TaskClass != tc.wantClass {
					t.Fatalf("expiry component = %#v, want band %s class %s", component, tc.wantBand, tc.wantClass)
				}
			})
		}
	}
}

func TestRouteDecisionHardRequirementsBeatQuotaAbundanceAndUnknownCapacity(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	good := fixture.candidate("codex", "acct-a", "codex-good")
	abundantBroken := fixture.candidate("codex", "acct-a", "codex-broken")
	unknown := fixture.candidate("claude", "acct-c", "claude-good")
	goodQuota := quotaWithReset("qsnap-good-10", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 10, fixture.now, fixture.now.Add(2*time.Hour))
	abundantQuota := quotaWithReset("qsnap-broken-999", "codex", "pinst-codex", "acct-a", "codex-broken", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 999, fixture.now, fixture.now.Add(10*time.Minute))
	good.QuotaSnapshotIDs = []string{goodQuota.QuotaSnapshotID}
	abundantBroken.QuotaSnapshotIDs = []string{abundantQuota.QuotaSnapshotID}
	unknown.QuotaSnapshotIDs = nil
	input.Inputs.Candidates = []Candidate{good, abundantBroken, unknown}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, goodQuota, abundantQuota)
	input.OptimizationPolicy = OptimizationPolicy{StrategyKey: StrategyBurnBeforeReset}

	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if selected := selectedCandidate(decision); selected.RoutingCandidateID != good.RoutingCandidateID {
		t.Fatalf("selected = %#v, want hard-eligible capable route", selected)
	}
	if containsScoredCandidate(decision.ScoredCandidates, abundantBroken.RoutingCandidateID) {
		t.Fatalf("abundant hard-ineligible candidate was scored: %#v", decision.ScoredCandidates)
	}
	if containsScoredCandidate(decision.ScoredCandidates, unknown.RoutingCandidateID) {
		t.Fatalf("unknown-capacity candidate was scored: %#v", decision.ScoredCandidates)
	}
	if !rejectedHas(Result{Rejected: decision.RejectedCandidates}, unknown.RoutingCandidateID, RejectQuotaConfidenceInsufficient) {
		t.Fatalf("unknown-capacity candidate rejections = %#v, want quota-confidence-insufficient", decision.RejectedCandidates)
	}
}

func TestRouteDecisionUnknownCapacitySoleCandidateFailsClosedWithExplain(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	candidate := fixture.candidate("claude", "acct-c", "claude-good")
	candidate.QuotaSnapshotIDs = nil
	input.Inputs.Candidates = []Candidate{candidate}
	input.Inputs.Inventory.QuotaSnapshots = nil

	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || containsScoredCandidate(decision.ScoredCandidates, candidate.RoutingCandidateID) {
		t.Fatalf("decision = %#v, want unknown-capacity no eligible route", decision)
	}
	human := ExplainHuman(decision)
	stable, err := ExplainJSON(decision)
	if err != nil {
		t.Fatalf("ExplainJSON: %v", err)
	}
	for _, output := range []string{human, string(stable)} {
		if !strings.Contains(output, string(RejectQuotaConfidenceInsufficient)) || !strings.Contains(output, "fresh quota capacity evidence is required") {
			t.Fatalf("explain missing quota rejection:\n%s", output)
		}
	}
}

func TestRouteDecisionResetBandsAreHardTaskFitGates(t *testing.T) {
	fixture := newFixture(t)
	for _, tc := range []struct {
		name       string
		reset      time.Duration
		risk       taskrequirements.RiskTier
		wantStatus string
		wantBand   string
	}{
		{name: "medium-under-15m", reset: 10 * time.Minute, risk: taskrequirements.RiskHigh, wantStatus: DecisionStatusNoEligible},
		{name: "medium-under-60m", reset: 30 * time.Minute, risk: taskrequirements.RiskHigh, wantStatus: DecisionStatusNoEligible},
		{name: "short-at-15m", reset: 15 * time.Minute, risk: taskrequirements.RiskMedium, wantStatus: DecisionStatusSelected, wantBand: "15-60m"},
		{name: "medium-at-60m", reset: time.Hour, risk: taskrequirements.RiskHigh, wantStatus: DecisionStatusSelected, wantBand: "over-1h"},
		{name: "expired-reset", reset: -time.Minute, risk: taskrequirements.RiskLow, wantStatus: DecisionStatusNoEligible},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := replayDecisionInput(fixture)
			candidate := fixture.candidate("codex", "acct-a", "codex-good")
			snap := quotaWithReset("qsnap-"+tc.name, "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now, fixture.now.Add(tc.reset))
			candidate.QuotaSnapshotIDs = []string{snap.QuotaSnapshotID}
			input.Inputs.Candidates = []Candidate{candidate}
			input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, snap)
			req := input.Inputs.Requirement
			req.RiskTier = tc.risk
			req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
			input.Inputs.Requirement = req

			decision, err := BuildRoutingDecision(input)
			if err != nil {
				t.Fatalf("BuildRoutingDecision: %v", err)
			}
			if decision.DecisionStatus != tc.wantStatus {
				t.Fatalf("status = %s rejected=%#v, want %s", decision.DecisionStatus, decision.RejectedCandidates, tc.wantStatus)
			}
			if tc.wantStatus == DecisionStatusNoEligible && !rejectedHas(Result{Rejected: decision.RejectedCandidates}, candidate.RoutingCandidateID, RejectQuotaResetIncompatible) {
				t.Fatalf("rejections = %#v, want reset incompatibility", decision.RejectedCandidates)
			}
			if tc.wantBand != "" {
				component := componentByName(t, decision.ScoredCandidates[0].Components, ComponentExpiryUrgency)
				if component.ResetWindow != tc.wantBand {
					t.Fatalf("reset window = %s, want %s", component.ResetWindow, tc.wantBand)
				}
			}
		})
	}
}

func TestRouteDecisionMissingResetAndMostConstrainingWindowFailClosed(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	missing := quota("qsnap-missing-reset", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now)
	missing.ResetAt, missing.WindowEnd, missing.ValidUntil = "", "", ""
	near := quotaWithReset("qsnap-near-window", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now, fixture.now.Add(10*time.Minute))
	distant := quotaWithReset("qsnap-distant-window", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now, fixture.now.Add(2*time.Hour))
	candidate.QuotaSnapshotIDs = []string{missing.QuotaSnapshotID}
	input.Inputs.Candidates = []Candidate{candidate}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, missing)
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision missing reset: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || !rejectedHas(Result{Rejected: decision.RejectedCandidates}, candidate.RoutingCandidateID, RejectQuotaResetIncompatible) {
		t.Fatalf("missing reset decision = %#v", decision)
	}

	req := input.Inputs.Requirement
	req.RiskTier = taskrequirements.RiskHigh
	req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
	input.Inputs.Requirement = req
	candidate.QuotaSnapshotIDs = []string{near.QuotaSnapshotID, distant.QuotaSnapshotID}
	input.Inputs.Candidates = []Candidate{candidate}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, near, distant)
	decision, err = BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision multiple windows: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || !rejectedHas(Result{Rejected: decision.RejectedCandidates}, candidate.RoutingCandidateID, RejectQuotaResetIncompatible) {
		t.Fatalf("multiple-window decision = %#v, want most-constraining reset rejection", decision)
	}
}

func TestRouteDecisionPaidOverageRequiresExplicitPolicy(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	snap := quotaWithReset("qsnap-paid-overage", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now, fixture.now.Add(time.Hour))
	snap.GapReasons = []string{"paid-overage"}
	candidate.QuotaSnapshotIDs = []string{snap.QuotaSnapshotID}
	input.Inputs.Candidates = []Candidate{candidate}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, snap)
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision default: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || !rejectedHas(Result{Rejected: decision.RejectedCandidates}, candidate.RoutingCandidateID, RejectBudgetExhausted) {
		t.Fatalf("decision = %#v, want paid overage rejected by default", decision)
	}

	input.Inputs.Policy.AllowPaidOverage = true
	input.OptimizationPolicy.AllowPaidOverage = true
	decision, err = BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision explicit overage: %v", err)
	}
	if selected := selectedCandidate(decision); selected.RoutingCandidateID != candidate.RoutingCandidateID {
		t.Fatalf("selected = %#v, want explicit paid overage policy to allow candidate", selected)
	}
}

func TestRouteDecisionExplainAndReevaluationIncludeResetStrategyEvidence(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	first := quotaWithReset("qsnap-reset-first", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 80, fixture.now, fixture.now.Add(30*time.Minute))
	candidate.QuotaSnapshotIDs = []string{first.QuotaSnapshotID}
	input.Inputs.Candidates = []Candidate{candidate}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, first)
	req := input.Inputs.Requirement
	req.RiskTier = taskrequirements.RiskLow
	req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
	input.Inputs.Requirement = req
	input.OptimizationPolicy = OptimizationPolicy{StrategyKey: StrategyBalanced}
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision first: %v", err)
	}
	human := ExplainHuman(decision)
	for _, want := range []string{"strategy balanced", "reset window 15-60m", "confidence=", "freshness=", "component expiry_urgency", "expected_waste_avoided"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human explain missing %q:\n%s", want, human)
		}
	}

	second := quotaWithReset("qsnap-reset-second", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 80, fixture.now.Add(time.Minute), fixture.now.Add(10*time.Minute))
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, second)
	input.Inputs.Candidates[0].QuotaSnapshotIDs = []string{second.QuotaSnapshotID}
	secondDecision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision fresh event: %v", err)
	}
	if secondDecision.RoutingFingerprint == decision.RoutingFingerprint {
		t.Fatalf("routing fingerprint did not change after fresh capacity event")
	}
	if componentByName(t, secondDecision.ScoredCandidates[0].Components, ComponentExpiryUrgency).ResetWindow != "under-15m" {
		t.Fatalf("fresh event did not re-evaluate reset band: %#v", secondDecision.ScoredCandidates[0].Components)
	}
}

func TestRouteDecisionDryRunExplainDoesNotPersist(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	input := replayDecisionInput(fixture)
	explain, err := DryRunExplainRoute(input)
	if err != nil {
		t.Fatalf("DryRunExplainRoute: %v", err)
	}
	if explain.Decision.RoutingDecisionID == "" || !strings.Contains(explain.Human, "strategy balanced") || !strings.Contains(string(explain.Stable), `"scored_candidates"`) {
		t.Fatalf("dry-run explain incomplete: %#v", explain)
	}
	assertRoutingDecisionCount(t, ctx, store, "proj-routing", "drun-routing", "route-worker", 0)
}

func TestManualUnavailableOverrideSuppressesMatchingCandidateUntilExpiry(t *testing.T) {
	fixture := newFixture(t)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	override := manualUnavailableOverride(fixture, candidate, fixture.now.Add(10*time.Minute))

	input := replayDecisionInput(fixture)
	input.Inputs.Candidates = []Candidate{candidate}
	input.AuthorizationFingerprint = testFingerprint("auth")
	input.OverrideProvenance = []OverrideProvenance{override}
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision before expiry: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || !rejectedHas(Result{Rejected: decision.RejectedCandidates}, candidate.RoutingCandidateID, RejectManualUnavailable) {
		t.Fatalf("before expiry decision = %#v", decision)
	}

	input.Now = fixture.now.Add(10 * time.Minute)
	decision, err = BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision at expiry: %v", err)
	}
	if selected := selectedCandidate(decision); selected.RoutingCandidateID != candidate.RoutingCandidateID {
		t.Fatalf("at expiry selected = %#v, want restored candidate", selected)
	}

	input.Now = fixture.now.Add(11 * time.Minute)
	decision, err = BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision after expiry: %v", err)
	}
	if selected := selectedCandidate(decision); selected.RoutingCandidateID != candidate.RoutingCandidateID {
		t.Fatalf("after expiry selected = %#v, want restored candidate", selected)
	}
}

func TestManualUnavailableOverrideScopeMismatchDoesNotSuppress(t *testing.T) {
	fixture := newFixture(t)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	override := manualUnavailableOverride(fixture, candidate, fixture.now.Add(10*time.Minute))
	override.TaskID = "different-task"
	override.Scope = canonicalManualOverrideScope(override)
	input := replayDecisionInput(fixture)
	input.Inputs.Candidates = []Candidate{candidate}
	input.AuthorizationFingerprint = testFingerprint("auth")
	input.OverrideProvenance = []OverrideProvenance{override}
	if _, err := BuildRoutingDecision(input); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("BuildRoutingDecision error = %v, want scope mismatch to fail closed", err)
	}
}

func TestManualResetOverrideReplacesOnlyResetEvidenceAndCannotBypassHardGates(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	input.AuthorizationFingerprint = testFingerprint("auth")
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	near := quotaWithReset("qsnap-reset-override-near", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now, fixture.now.Add(10*time.Minute))
	candidate.QuotaSnapshotIDs = []string{near.QuotaSnapshotID}
	input.Inputs.Candidates = []Candidate{candidate}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, near)
	req := input.Inputs.Requirement
	req.RiskTier = taskrequirements.RiskHigh
	req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
	input.Inputs.Requirement = req
	resetOverride := manualResetOverride(fixture, candidate, fixture.now.Add(2*time.Hour))
	input.OverrideProvenance = []OverrideProvenance{resetOverride}

	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision manual reset: %v", err)
	}
	if selected := selectedCandidate(decision); selected.RoutingCandidateID != candidate.RoutingCandidateID {
		t.Fatalf("selected = %#v, want manual reset to replace reset assumption only", selected)
	}

	broken := fixture.candidate("codex", "acct-a", "codex-broken")
	broken.QuotaSnapshotIDs = []string{"qsnap-codex-broken-high"}
	input.Inputs.Candidates = []Candidate{broken}
	resetOverride.CandidateConstraint.ModelCapabilityID = "codex-broken"
	input.OverrideProvenance = []OverrideProvenance{resetOverride}
	decision, err = BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision broken manual reset: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || !rejectedHas(Result{Rejected: decision.RejectedCandidates}, broken.RoutingCandidateID, RejectModelUnavailable) {
		t.Fatalf("manual reset bypassed hard gate: %#v", decision)
	}

	unknown := fixture.candidate("claude", "acct-c", "claude-good")
	unknown.QuotaSnapshotIDs = nil
	input.Inputs.Candidates = []Candidate{unknown}
	input.Inputs.Inventory.QuotaSnapshots = nil
	input.OverrideProvenance = []OverrideProvenance{manualResetOverride(fixture, unknown, fixture.now.Add(2*time.Hour))}
	decision, err = BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision unknown manual reset: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || !rejectedHas(Result{Rejected: decision.RejectedCandidates}, unknown.RoutingCandidateID, RejectQuotaConfidenceInsufficient) {
		t.Fatalf("manual reset invented capacity: %#v", decision)
	}
}

func TestManualResetOverrideIsBoundToCandidateInvocationProfile(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	input.AuthorizationFingerprint = testFingerprint("auth")
	shared := quotaWithReset("qsnap-shared-invocation", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now, fixture.now.Add(10*time.Minute))
	base := fixture.candidate("codex", "acct-a", "codex-good")
	fast := base
	fast.InvocationProfileKey = "fast"
	fast.QuotaSnapshotIDs = []string{shared.QuotaSnapshotID}
	fast.RoutingCandidateID = candidateID(fast)
	fast.CandidateFingerprint = candidateFingerprint(fast)
	deep := base
	deep.InvocationProfileKey = "deep"
	deep.QuotaSnapshotIDs = []string{shared.QuotaSnapshotID}
	deep.RoutingCandidateID = candidateID(deep)
	deep.CandidateFingerprint = candidateFingerprint(deep)
	input.Inputs.Candidates = []Candidate{fast, deep}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, shared)
	req := input.Inputs.Requirement
	req.RiskTier = taskrequirements.RiskHigh
	req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
	input.Inputs.Requirement = req
	override := manualResetOverride(fixture, fast, fixture.now.Add(2*time.Hour))
	override.CandidateConstraint = CandidateConstraint{InvocationProfileKey: "fast"}
	override.Scope = canonicalManualOverrideScope(override)
	input.OverrideProvenance = []OverrideProvenance{override}

	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if selected := selectedCandidate(decision); selected.RoutingCandidateID != fast.RoutingCandidateID {
		t.Fatalf("selected = %#v, want only fast invocation candidate reset-bound eligible", selected)
	}
	if containsScoredCandidate(decision.ScoredCandidates, deep.RoutingCandidateID) {
		t.Fatalf("deep invocation candidate shared reset mutation: %#v", decision.ScoredCandidates)
	}
	if !rejectedHas(Result{Rejected: decision.RejectedCandidates}, deep.RoutingCandidateID, RejectQuotaResetIncompatible) {
		t.Fatalf("deep invocation rejections = %#v, want original near reset to remain incompatible", decision.RejectedCandidates)
	}
	fastExpiry := componentByName(t, decision.ScoredCandidates[0].Components, ComponentExpiryUrgency)
	if len(fastExpiry.SnapshotIDs) != 1 || fastExpiry.SnapshotIDs[0] == shared.QuotaSnapshotID || !strings.HasPrefix(fastExpiry.SnapshotIDs[0], "manual-reset:") || fastExpiry.ResetWindow != "over-1h" {
		t.Fatalf("fast expiry component = %#v, want candidate-specific manual reset snapshot", fastExpiry)
	}
}

func TestManualResetInvocationOnlyConstraintDoesNotExpandToWildcard(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	input.AuthorizationFingerprint = testFingerprint("auth")
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	candidate.InvocationProfileKey = "default"
	candidate.RoutingCandidateID = candidateID(candidate)
	near := quotaWithReset("qsnap-invocation-only-no-match", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 100, fixture.now, fixture.now.Add(10*time.Minute))
	candidate.QuotaSnapshotIDs = []string{near.QuotaSnapshotID}
	input.Inputs.Candidates = []Candidate{candidate}
	input.Inputs.Inventory.QuotaSnapshots = append(input.Inputs.Inventory.QuotaSnapshots, near)
	req := input.Inputs.Requirement
	req.RiskTier = taskrequirements.RiskHigh
	req = decisionRequirement(t, fixture, req, req.TaskRequirementID, input.PlanFingerprint)
	input.Inputs.Requirement = req
	override := manualResetOverride(fixture, candidate, fixture.now.Add(2*time.Hour))
	override.CandidateConstraint = CandidateConstraint{InvocationProfileKey: "different"}
	override.Scope = canonicalManualOverrideScope(override)
	input.OverrideProvenance = []OverrideProvenance{override}

	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || containsScoredCandidate(decision.ScoredCandidates, candidate.RoutingCandidateID) {
		t.Fatalf("decision = %#v, want invocation-only non-match to leave candidate rejected", decision)
	}
	if !rejectedHas(Result{Rejected: decision.RejectedCandidates}, candidate.RoutingCandidateID, RejectQuotaResetIncompatible) {
		t.Fatalf("rejections = %#v, want original reset incompatibility", decision.RejectedCandidates)
	}
}

func TestManualOverrideScopeMustMatchStructuredTaskRunBindings(t *testing.T) {
	fixture := newFixture(t)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	for _, tc := range []struct {
		name   string
		mutate func(*OverrideProvenance)
	}{
		{name: "empty structured bindings", mutate: func(override *OverrideProvenance) {
			override.TaskID = ""
			override.DeliveryRunID = ""
			override.Scope = "manual-reset task:task-a"
		}},
		{name: "misleading prose scope", mutate: func(override *OverrideProvenance) {
			override.Scope = "manual-reset please only task:task-a"
		}},
		{name: "task mismatch", mutate: func(override *OverrideProvenance) {
			override.TaskID = "task-other"
			override.Scope = canonicalManualOverrideScope(*override)
		}},
		{name: "run mismatch", mutate: func(override *OverrideProvenance) {
			override.DeliveryRunID = "drun-other"
			override.Scope = canonicalManualOverrideScope(*override)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := replayDecisionInput(fixture)
			input.AuthorizationFingerprint = testFingerprint("auth")
			input.Inputs.Candidates = []Candidate{candidate}
			override := manualResetOverride(fixture, candidate, fixture.now.Add(time.Hour))
			tc.mutate(&override)
			input.OverrideProvenance = []OverrideProvenance{override}
			if _, err := BuildRoutingDecision(input); !errors.Is(err, taskrequirements.ErrInvalidRecord) && !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
				t.Fatalf("BuildRoutingDecision error = %v, want fail-closed invalid or mismatch", err)
			}
		})
	}

	exact := manualResetOverride(fixture, candidate, fixture.now.Add(time.Hour))
	if diagnostics := ValidateOverrideProvenance([]OverrideProvenance{exact}, fixture.now, exact.PolicyFingerprint, exact.AuthorizationFingerprint); len(diagnostics) != 0 {
		t.Fatalf("exact combined scope diagnostics = %#v, want valid", diagnostics)
	}
}

func TestManualOverrideProvenanceIsRedactedInExplain(t *testing.T) {
	fixture := newFixture(t)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	override := manualUnavailableOverride(fixture, candidate, fixture.now.Add(10*time.Minute))
	secret := "sk-" + strings.Repeat("x", 24)
	override.Reason = "operator note " + secret
	input := replayDecisionInput(fixture)
	input.Inputs.Candidates = []Candidate{candidate}
	input.AuthorizationFingerprint = testFingerprint("auth")
	input.OverrideProvenance = []OverrideProvenance{override}

	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	human := ExplainHuman(decision)
	stable, err := ExplainJSON(decision)
	if err != nil {
		t.Fatalf("ExplainJSON: %v", err)
	}
	for _, output := range []string{human, string(stable)} {
		if strings.Contains(output, secret) || !strings.Contains(output, "[REDACTED_") {
			t.Fatalf("override provenance not redacted:\n%s", output)
		}
	}
}

func TestManualOverrideProvenanceRedactsSecretPathAndControlCanaries(t *testing.T) {
	fixture := newFixture(t)
	candidate := fixture.candidate("codex", "acct-a", "codex-good")
	cases := map[string]string{
		"github token":      "ghp_" + strings.Repeat("A", 24),
		"github pat":        "github_pat_" + strings.Repeat("B", 24),
		"aws access key":    "AKIA" + strings.Repeat("C", 16),
		"openai key":        "sk-" + strings.Repeat("D", 24),
		"bearer":            "Bearer " + strings.Repeat("E", 24),
		"token assignment":  credentialAssignmentCanary([]string{"tok", "en"}, []string{"="}, strings.Repeat("F", 24)),
		"password assign":   credentialAssignmentCanary([]string{"pass", "word"}, []string{":"}, strings.Repeat("G", 24)),
		"secret assignment": credentialAssignmentCanary([]string{"sec", "ret"}, []string{"="}, strings.Repeat("H", 24)),
		"api key assign":    credentialAssignmentCanary([]string{"api", "-key"}, []string{"="}, strings.Repeat("I", 24)),
		"local path":        "/Users/tester/projects/loopcoder/private.json",
		"control chars":     "safe\x00\x1bsecret",
		"split long secret": strings.Repeat("x", 230) + " sk-" + strings.Repeat("J", 48),
	}
	for name, canary := range cases {
		t.Run(name, func(t *testing.T) {
			input := replayDecisionInput(fixture)
			input.Inputs.Candidates = []Candidate{candidate}
			input.AuthorizationFingerprint = testFingerprint("auth")
			override := manualUnavailableOverride(fixture, candidate, fixture.now.Add(10*time.Minute))
			override.Reason = "reason " + canary
			override.Source = "source " + canary
			input.OverrideProvenance = []OverrideProvenance{override}
			decision, err := BuildRoutingDecision(input)
			if err != nil {
				t.Fatalf("BuildRoutingDecision: %v", err)
			}
			human := ExplainHuman(decision)
			stable, err := ExplainJSON(decision)
			if err != nil {
				t.Fatalf("ExplainJSON: %v", err)
			}
			ctx := context.Background()
			store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
			if err != nil {
				t.Fatalf("Open storage: %v", err)
			}
			defer store.Close()
			seedRoutingDecisionStore(t, ctx, store, fixture.now)
			if err := PersistRoutingDecision(ctx, store, decision); err != nil {
				t.Fatalf("PersistRoutingDecision: %v", err)
			}
			loaded, err := LoadRoutingDecision(ctx, store, decision.RoutingDecisionID)
			if err != nil {
				t.Fatalf("LoadRoutingDecision: %v", err)
			}
			persisted, err := ExplainJSON(loaded)
			if err != nil {
				t.Fatalf("ExplainJSON persisted: %v", err)
			}
			for _, output := range []string{human, string(stable), string(persisted)} {
				if strings.Contains(output, canary) || strings.ContainsAny(output, "\x00\x1b") {
					t.Fatalf("output leaked canary/control characters:\n%s", output)
				}
				if name != "control chars" && !strings.Contains(output, "[REDACTED_") {
					t.Fatalf("output missing redaction marker:\n%s", output)
				}
			}
		})
	}
}

func credentialAssignmentCanary(keyParts, separatorParts []string, value string) string {
	return strings.Join(keyParts, "") + strings.Join(separatorParts, "") + value
}

func TestReevaluateRoutePersistsOnTaskBoundaryOrFreshCapacityAndDryRunDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	input := replayDecisionInput(fixture)

	first, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtTaskBoundary})
	if err != nil {
		t.Fatalf("ReevaluateRoute first: %v", err)
	}
	if !first.Changed {
		t.Fatalf("first re-evaluation changed = false, want new persisted decision")
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 1)

	if err := replaceStoredQuotaReset(t, ctx, store, "qsnap-codex-a-good", fixture.now.Add(30*time.Minute)); err != nil {
		t.Fatalf("replaceStoredQuotaReset: %v", err)
	}
	dry, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtFreshCapacityEvent, DryRun: true})
	if err != nil {
		t.Fatalf("ReevaluateRoute dry run: %v", err)
	}
	if !dry.Changed || dry.Decision.RoutingDecisionID == first.Decision.RoutingDecisionID {
		t.Fatalf("dry re-evaluation = %#v, want changed new decision without persistence", dry)
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 1)

	persisted, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtFreshCapacityEvent})
	if err != nil {
		t.Fatalf("ReevaluateRoute fresh event: %v", err)
	}
	if !persisted.Changed || persisted.Decision.RoutingDecisionID == first.Decision.RoutingDecisionID {
		t.Fatalf("persisted re-evaluation = %#v, want new fingerprint", persisted)
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 2)
}

func TestFreshCapacityRefreshHandlerReevaluatesRouteThroughProductionEvent(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	input := replayDecisionInput(fixture)
	first, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtTaskBoundary})
	if err != nil {
		t.Fatalf("ReevaluateRoute seed: %v", err)
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 1)

	manager := providerinventory.NewRefreshManager(store, providerinventory.DefaultDeps())
	refreshedSnapshot := quotaWithReset("qsnap_fresh_event_codex_a_good", "codex", "pinst-codex", "acct-a", "codex-good", providerinventory.ConfidenceExact, providerinventory.FreshnessFresh, 500, fixture.now.Add(time.Minute), fixture.now.Add(30*time.Minute))
	refreshedSnapshot.QuotaSourceID = "qsrc-fixture-codex"
	refreshedSnapshot.ScopeKey = "provider:codex/account:acct-a/model:codex-good"
	refreshedSnapshot.PolicyVersion = providerinventory.PolicyVersion
	refreshedSnapshot.CreatedAt = refreshedSnapshot.CapturedAt
	refreshedSnapshot.UpdatedAt = refreshedSnapshot.CapturedAt
	refreshedSnapshot.WindowStart = delivery.CanonicalTimestamp(fixture.now.Add(-30 * time.Minute))
	refreshed := providerinventory.Report{
		SchemaVersion:         providerinventory.ProviderInventoryJSONSchema,
		GeneratedAt:           delivery.CanonicalTimestamp(fixture.now.Add(time.Minute)),
		Confidence:            providerinventory.ConfidenceExact,
		QuotaTelemetrySources: []providerinventory.QuotaTelemetrySource{routingFixtureQuotaSource("codex", fixture.now)},
		QuotaSnapshots:        []providerinventory.QuotaSnapshot{refreshedSnapshot},
	}
	manager.Collector = func(context.Context, providerinventory.Options, providerinventory.Deps) (providerinventory.Report, error) {
		return refreshed, nil
	}
	var callbackCount int
	var callbackDecision RoutingDecision
	result, err := manager.Refresh(ctx, providerinventory.RefreshRequest{
		Config:  config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Trigger: providerinventory.RefreshTriggerExplicit,
		Now:     func() time.Time { return fixture.now.Add(time.Minute) },
		AfterFreshCapacityEvent: func(ctx context.Context, result providerinventory.RefreshResult) error {
			callbackCount++
			reeval, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtFreshCapacityEvent})
			callbackDecision = reeval.Decision
			return err
		},
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if callbackCount != 1 || !refreshResultHasRefreshedProvider(result, "codex") {
		t.Fatalf("callbackCount/result = %d/%#v, want one fresh capacity callback", callbackCount, result.Providers)
	}
	if callbackDecision.RoutingFingerprint == first.Decision.RoutingFingerprint {
		t.Fatalf("fresh capacity callback did not produce new routing fingerprint")
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 2)

	dry, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtFreshCapacityEvent, DryRun: true})
	if err != nil {
		t.Fatalf("ReevaluateRoute dry replay: %v", err)
	}
	if dry.Changed {
		t.Fatalf("dry replay changed = true, want no changed fingerprint after persisted fresh capacity decision")
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 2)
}

func TestTaskBoundaryHandlerReevaluatesRouteThroughNestedSchedulerEvent(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	input := replayDecisionInput(fixture)
	first, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtTaskBoundary})
	if err != nil {
		t.Fatalf("ReevaluateRoute seed: %v", err)
	}
	var callbackCount int
	report, err := orchestration.ScheduleNestedRuns(ctx, orchestration.NestedScheduleOptions{
		RepoPath:         t.TempDir(),
		ParentRunID:      "run-20260713T120000Z-wave",
		BaseBranch:       "main",
		ConcurrencyLimit: 1,
		MaxChildren:      1,
		Now:              fixture.now,
		Clock:            func() time.Time { return fixture.now.Add(time.Minute) },
		Children: []orchestration.ChildRunPlan{{
			ID:         "route-boundary",
			Issue:      844,
			Permission: "write",
			Required:   true,
		}},
		Execute: func(context.Context, orchestration.ChildRunPlan) (orchestration.ChildRunResult, error) {
			if err := replaceStoredQuotaReset(t, ctx, store, "qsnap-codex-a-good", fixture.now.Add(30*time.Minute)); err != nil {
				return orchestration.ChildRunResult{}, err
			}
			return orchestration.ChildRunResult{Status: orchestration.NestedStatusSucceeded}, nil
		},
		TaskBoundaryRouteReevaluation: func(ctx context.Context, event orchestration.TaskBoundaryRouteReevaluationEvent) error {
			callbackCount++
			if event.Status != orchestration.NestedStatusSucceeded || event.ChildKey == "" {
				t.Fatalf("task boundary event = %#v", event)
			}
			_, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtTaskBoundary})
			return err
		},
	})
	if err != nil {
		t.Fatalf("ScheduleNestedRuns: %v", err)
	}
	if report.Status != orchestration.NestedStatusSucceeded || callbackCount != 1 {
		t.Fatalf("report/callback = %#v/%d, want succeeded with one task-boundary callback", report, callbackCount)
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 2)
	latest, err := latestRoutingDecision(ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey)
	if err != nil {
		t.Fatalf("latestRoutingDecision: %v", err)
	}
	if latest.RoutingFingerprint == first.Decision.RoutingFingerprint {
		t.Fatalf("task boundary callback did not persist a new routing fingerprint")
	}
	dry, err := ReevaluateRoute(ctx, store, ReevaluateRouteInput{DecisionInput: input, Trigger: ReevaluateAtTaskBoundary, DryRun: true})
	if err != nil {
		t.Fatalf("ReevaluateRoute dry no-change: %v", err)
	}
	if dry.Changed {
		t.Fatalf("dry no-change changed = true, want false")
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 2)
}

func TestRouteDecisionRejectsInvalidWeightsBeforeScoring(t *testing.T) {
	fixture := newFixture(t)
	for name, weights := range map[string]map[ComponentName]int{
		"unknown": {
			ComponentAvailability:  90,
			ComponentName("bogus"): 10,
		},
		"negative": {
			ComponentAvailability:  30,
			ComponentQuotaHeadroom: 20,
			ComponentQualityFit:    20,
			ComponentLatency:       10,
			ComponentCost:          -1,
			ComponentDiversity:     21,
		},
		"oversized": {
			ComponentAvailability: 101,
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := replayDecisionInput(fixture)
			input.OptimizationPolicy.Weights = weights
			if _, err := BuildRoutingDecision(input); !errors.Is(err, taskrequirements.ErrInvalidRecord) {
				t.Fatalf("BuildRoutingDecision error = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

func TestRouteDecisionRejectsPolicyFingerprintMismatch(t *testing.T) {
	fixture := newFixture(t)
	input := replayDecisionInput(fixture)
	input.PolicyFingerprint = testFingerprint("wrong-policy")
	if _, err := BuildRoutingDecision(input); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("BuildRoutingDecision error = %v, want ErrRoutingFingerprintMismatch", err)
	}
}

func TestRouteDecisionRejectsMalformedRequiredInputs(t *testing.T) {
	fixture := newFixture(t)
	cases := map[string]func(*DecisionInput){
		"task requirement": func(input *DecisionInput) { input.TaskRequirementID = "" },
		"role definition":  func(input *DecisionInput) { input.RoleDefinitionID = "" },
		"plan fingerprint": func(input *DecisionInput) { input.PlanFingerprint = "sha256:plan" },
		"actor authority":  func(input *DecisionInput) { input.DecidedBy.DecisionAuthority = "operator" },
		"host":             func(input *DecisionInput) { input.Host.HostID = "" },
		"unknown requirement value": func(input *DecisionInput) {
			input.Inputs.Requirement.RiskTier = taskrequirements.RiskTier("surprise")
			input.Inputs.Requirement.TaskRequirementFingerprint = ""
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := replayDecisionInput(fixture)
			mutate(&input)
			if _, err := BuildRoutingDecision(input); !errors.Is(err, taskrequirements.ErrInvalidRecord) {
				t.Fatalf("BuildRoutingDecision error = %v, want ErrInvalidRecord", err)
			}
		})
	}
}

func TestPersistRoutingDecisionValidatesDerivedFingerprintFields(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)

	decision, err := BuildRoutingDecision(replayDecisionInput(fixture))
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	decision.PolicyFingerprint = testFingerprint("tampered-policy")
	if err := PersistRoutingDecision(ctx, store, decision); !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("PersistRoutingDecision error = %v, want ErrRoutingFingerprintMismatch", err)
	}
}

func TestRouteDecisionStableExplainGolden(t *testing.T) {
	fixture := newFixture(t)
	decision, err := BuildRoutingDecision(replayDecisionInput(fixture))
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	got, err := ExplainJSON(decision)
	if err != nil {
		t.Fatalf("ExplainJSON: %v", err)
	}
	if os.Getenv("UPDATE_ROUTING_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile("testdata/route_explain_selected.golden.json", got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile("testdata/route_explain_selected.golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("stable explain JSON mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}

func TestRouteDecisionReplayPersistsOneReproducibleSelection(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	path := tempDB(t)
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	seedRoutingDecisionStore(t, ctx, store, fixture.now)

	input := replayDecisionInput(fixture)
	first, err := DecideAndPersistRoute(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute first: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close first store: %v", err)
	}

	later := fixture.now.Add(30 * time.Minute)
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return later }})
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer reopened.Close()
	second, err := DecideAndPersistRoute(ctx, reopened, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute replay: %v", err)
	}
	if first.RoutingDecisionID != second.RoutingDecisionID || first.ChosenCandidateID != second.ChosenCandidateID || first.CreatedAt != second.CreatedAt {
		t.Fatalf("replay changed decision:\nfirst=%#v\nsecond=%#v", first, second)
	}
	assertRoutingDecisionCount(t, ctx, reopened, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 1)
}

func TestRouteDecisionWeightChangeCreatesNewPolicyVersionWithoutRewritingHistory(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)

	input := replayDecisionInput(fixture)
	profile := BalancedRoutingPolicyProfile(fixture.now)
	input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	first, err := DecideAndPersistRoute(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute first: %v", err)
	}
	changed := profile
	changed.ProfileVersion = "2"
	changed.RoutingPolicyProfileID = ""
	changed.PolicyFingerprint = ""
	changed.OptimizationPolicy.PolicyVersion = ""
	changed.OptimizationPolicy.Weights = map[ComponentName]int{
		ComponentAvailability:   10,
		ComponentQualityFit:     20,
		ComponentQuotaHeadroom:  10,
		ComponentCost:           10,
		ComponentLatency:        10,
		ComponentDiversity:      10,
		ComponentLocality:       20,
		ComponentUserPreference: 10,
	}
	changed, err = PersistRoutingPolicyProfile(ctx, store, changed)
	if err != nil {
		t.Fatalf("PersistRoutingPolicyProfile changed version: %v", err)
	}
	input.RoutingPolicyProfileID = changed.RoutingPolicyProfileID
	second, err := DecideAndPersistRoute(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute changed weights: %v", err)
	}
	if first.RoutingDecisionID == second.RoutingDecisionID {
		t.Fatalf("routing decision id did not change after weight change")
	}
	if first.OptimizationPolicy.PolicyVersion == second.OptimizationPolicy.PolicyVersion {
		t.Fatalf("policy version did not change after weight change: %s", first.OptimizationPolicy.PolicyVersion)
	}
	assertRoutingDecisionCount(t, ctx, store, input.ProjectID, input.DeliveryRunID, input.DecisionKey, 2)
	loadedFirst, err := LoadRoutingDecision(ctx, store, first.RoutingDecisionID)
	if err != nil {
		t.Fatalf("LoadRoutingDecision first: %v", err)
	}
	if loadedFirst.OptimizationPolicy.PolicyVersion != first.OptimizationPolicy.PolicyVersion {
		t.Fatalf("first history was rewritten: got %s want %s", loadedFirst.OptimizationPolicy.PolicyVersion, first.OptimizationPolicy.PolicyVersion)
	}
}

func TestRouteDecisionNoEligibleCandidatePersistsTypedBlockedDecision(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)

	input := replayDecisionInput(fixture)
	input.DecisionKey = "route-blocked"
	req := workerRequirement("task-blocked-permission")
	req.PermissionRequired = taskrequirements.PermissionOrchestrate
	req = decisionRequirement(t, fixture, req, "treq-blocked-permission", input.PlanFingerprint)
	input.TaskRequirementID = req.TaskRequirementID
	input.Inputs.Requirement = req
	decision, err := DecideAndPersistRoute(ctx, store, input)
	if !errors.Is(err, taskrequirements.ErrNoEligibleCandidate) {
		t.Fatalf("DecideAndPersistRoute error = %v, want ErrNoEligibleCandidate", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || decision.TerminalErrorCode != taskrequirements.ErrNoEligibleCandidateCode {
		t.Fatalf("blocked decision = %#v", decision)
	}
	loaded, err := LoadRoutingDecision(ctx, store, decision.RoutingDecisionID)
	if err != nil {
		t.Fatalf("LoadRoutingDecision: %v", err)
	}
	if loaded.DecisionStatus != DecisionStatusNoEligible || len(loaded.RejectedCandidates) == 0 || len(loaded.RejectedSummary) == 0 {
		t.Fatalf("loaded blocked decision missing rejection evidence: %#v", loaded)
	}
	replayed, err := DecideAndPersistRoute(ctx, store, input)
	if !errors.Is(err, taskrequirements.ErrNoEligibleCandidate) {
		t.Fatalf("blocked replay error = %v, want ErrNoEligibleCandidate", err)
	}
	if replayed.RoutingDecisionID != decision.RoutingDecisionID || replayed.CreatedAt != decision.CreatedAt || replayed.TerminalErrorCode != taskrequirements.ErrNoEligibleCandidateCode {
		t.Fatalf("blocked replay changed immutable decision:\nfirst=%#v\nreplay=%#v", decision, replayed)
	}
}

func TestDecideAndPersistRouteUsesDurableCachedInventoryOverCallerCandidates(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStore(t, ctx, store, fixture.now)

	input := replayDecisionInput(fixture)
	input.DecisionKey = "route-cache-authoritative"
	grokFixture := withGrokOrdinaryWorkerFixture(fixture)
	fabricated := grokFixture.candidate("grok", "acct-grok", "grok-good")
	input.Inputs.Candidates = []Candidate{fabricated}
	input.Inputs.Inventory = grokFixture.inventory
	decision, err := DecideAndPersistRoute(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute: %v", err)
	}
	for _, candidate := range append(decision.EligibleCandidates, rejectedDecisionCandidates(decision.RejectedCandidates)...) {
		if candidate.AdapterID == "grok" {
			t.Fatalf("caller-supplied grok candidate bypassed cached inventory: %#v", decision)
		}
	}
	if len(decision.InputRecordRefs) == 0 || !hasInputRecordRef(decision.InputRecordRefs, "quota_snapshot", "qsnap-codex-a-good") {
		t.Fatalf("decision did not bind cached quota evidence: %#v", decision.InputRecordRefs)
	}
}

func TestDecideAndPersistRouteMissingDurableInventoryFailsClosed(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	defer store.Close()
	seedRoutingDecisionStoreMetadataOnly(t, ctx, store, fixture.now)

	input := replayDecisionInput(fixture)
	input.DecisionKey = "route-cache-missing"
	decision, err := DecideAndPersistRoute(ctx, store, input)
	if !errors.Is(err, taskrequirements.ErrNoEligibleCandidate) {
		t.Fatalf("DecideAndPersistRoute error = %v, want ErrNoEligibleCandidate", err)
	}
	if decision.DecisionStatus != DecisionStatusNoEligible || len(decision.EligibleCandidates) != 0 || len(decision.ScoredCandidates) != 0 {
		t.Fatalf("missing cached inventory decision = %#v", decision)
	}
}

func replayDecisionInput(fixture hardFixture) DecisionInput {
	scores := fixture.availabilityScores()
	for i := range scores {
		scores[i].Score = 90
		scores[i].ScoreConfidence = providerinventory.ConfidenceExact
	}
	planFingerprint := testFingerprint("plan-routing")
	req := decisionRequirement(nil, fixture, workerRequirement("task-a"), "treq-routing", planFingerprint)
	return DecisionInput{
		ProjectID:         "proj-routing",
		DeliveryRunID:     "drun-routing",
		DecisionKey:       "route-worker",
		TaskRequirementID: req.TaskRequirementID,
		RoleDefinitionID:  "role-worker",
		PlanFingerprint:   planFingerprint,
		DecidedBy:         routerActor(),
		Host:              routingHost(),
		Now:               fixture.now,
		Inputs: Inputs{
			Requirement: req,
			Candidates: []Candidate{
				fixture.candidate("codex", "acct-a", "codex-good"),
				fixture.candidate("claude", "acct-c", "claude-good"),
			},
			Inventory:       fixture.inventory,
			Availability:    scores,
			Budgets:         fixture.budgets,
			RuntimeContract: fixture.contract,
			HostName:        "codex-cli",
			Policy: Policy{
				EvidencePolicy:              EvidenceAllowEstimated,
				RequireAvailabilityEvidence: true,
				RequireBudgetEvidence:       true,
			},
		},
	}
}

func tempDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir + "/loopcoder.db"
}

func seedRoutingDecisionStore(t *testing.T, ctx context.Context, store storage.Store, now time.Time) {
	t.Helper()
	seedRoutingDecisionStoreMetadataOnly(t, ctx, store, now)
	seedCachedRoutingInventoryPayloads(t, ctx, store, newFixture(t))
}

func seedRoutingDecisionStoreMetadataOnly(t *testing.T, ctx context.Context, store storage.Store, now time.Time) {
	t.Helper()
	at := delivery.CanonicalTimestamp(now)
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
			"proj-routing", "/repo", at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO delivery_runs(
			delivery_run_id, run_id, schema_version, record_version, project_id, root_run_id, parent_run_id,
			state, intent_summary, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint,
			policy_version, max_side_effect_class, approval_status, override_status, created_at, updated_at,
			created_by_json, updated_by_json, host_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}')
			ON CONFLICT(delivery_run_id) DO NOTHING`,
			"drun-routing", "run-routing", delivery.SchemaDeliveryRun, 1, "proj-routing", "run-routing", "",
			"approved", "routing test", testFingerprint("input"), testFingerprint("delivery-policy"), testFingerprint("plan-routing"), testFingerprint("auth"),
			"routing-test", "provider-launch", "approved", "none", at, at); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed storage: %v", err)
	}
	if _, err := EnsureBuiltInRoutingPolicyProfiles(ctx, store, now); err != nil {
		t.Fatalf("seed routing policy profiles: %v", err)
	}
}

func decisionRequirement(t *testing.T, fixture hardFixture, req taskrequirements.TaskRequirement, reqID, planFingerprint string) taskrequirements.TaskRequirement {
	if t != nil {
		t.Helper()
	}
	req.SchemaVersion = taskrequirements.SchemaTaskRequirement
	req.RecordVersion = 1
	req.TaskRequirementID = reqID
	req.ProjectID = "proj-routing"
	req.DeliveryRunID = "drun-routing"
	req.TaskKey = req.TaskID
	req.RequiredOutput = taskrequirements.OutputMarkdown
	req.ScopeJSON = "{}"
	req.DataClassification = "internal"
	req.ClassificationRules = []string{"test.routing"}
	req.ProvenanceSummary = "test routing requirement"
	req.PolicyVersion = "routing-test"
	req.PlanFingerprint = planFingerprint
	req.CreatedAt = delivery.CanonicalTimestamp(fixture.now)
	req.UpdatedAt = req.CreatedAt
	req.CreatedBy = routerActor()
	req.UpdatedBy = routerActor()
	req.Host = routingHost()
	req.Classification = "deterministic"
	req.Confidence = providerinventory.ConfidenceExact
	req.Source = taskrequirements.SourceDescriptor{SourceKind: "test", SourceReference: "routing"}
	fp, err := taskrequirements.FingerprintRequirement(req)
	if err != nil {
		if t != nil {
			t.Fatalf("FingerprintRequirement: %v", err)
		}
		panic(err)
	}
	req.TaskRequirementFingerprint = fp
	return req
}

func routerActor() delivery.Actor {
	return delivery.Actor{
		ActorKind:         "system",
		ActorID:           "router",
		DecisionAuthority: "router",
		Source:            "test",
	}
}

func routingHost() delivery.Host {
	return delivery.Host{HostKind: "test", HostID: "routing-test"}
}

func testFingerprint(value string) string {
	return delivery.SHA256Digest([]byte(value))
}

func manualUnavailableOverride(fixture hardFixture, candidate Candidate, until time.Time) OverrideProvenance {
	policy, err := normalizeOptimizationPolicy(OptimizationPolicy{}, "")
	if err != nil {
		panic(err)
	}
	taskID := "task-a"
	override := OverrideProvenance{
		OverrideID:               "ovr-unavailable-" + candidate.ModelCapabilityID,
		OverrideKind:             "manual-unavailable-until",
		Reason:                   "operator marked candidate unavailable",
		TaskID:                   taskID,
		DeliveryRunID:            "drun-routing",
		CandidateConstraint:      CandidateConstraint{AdapterID: candidate.AdapterID, ProviderInstallationID: candidate.ProviderInstallationID, AccountProfileID: candidate.AccountProfileID, ModelCapabilityID: candidate.ModelCapabilityID},
		ManualUnavailableUntil:   until.UTC().Format(time.RFC3339Nano),
		ExpiresAt:                fixture.now.Add(time.Hour).Format(time.RFC3339Nano),
		PolicyFingerprint:        policy.PolicyFingerprint,
		AuthorizationFingerprint: testFingerprint("auth"),
		Actor:                    delivery.Actor{ActorKind: "user", ActorID: "user-1", DecisionAuthority: "user", Source: "test"},
		Host:                     routingHost(),
		Source:                   "test",
	}
	override.Scope = canonicalManualOverrideScope(override)
	return override
}

func manualResetOverride(fixture hardFixture, candidate Candidate, resetAt time.Time) OverrideProvenance {
	policy, err := normalizeOptimizationPolicy(OptimizationPolicy{}, "")
	if err != nil {
		panic(err)
	}
	taskID := "task-a"
	override := OverrideProvenance{
		OverrideID:               "ovr-reset-" + candidate.ModelCapabilityID,
		OverrideKind:             "manual-reset",
		Reason:                   "operator provided bounded reset evidence",
		TaskID:                   taskID,
		DeliveryRunID:            "drun-routing",
		CandidateConstraint:      CandidateConstraint{AdapterID: candidate.AdapterID, ProviderInstallationID: candidate.ProviderInstallationID, AccountProfileID: candidate.AccountProfileID, ModelCapabilityID: candidate.ModelCapabilityID},
		ManualResetAt:            resetAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt:                fixture.now.Add(time.Hour).Format(time.RFC3339Nano),
		PolicyFingerprint:        policy.PolicyFingerprint,
		AuthorizationFingerprint: testFingerprint("auth"),
		Actor:                    delivery.Actor{ActorKind: "user", ActorID: "user-1", DecisionAuthority: "user", Source: "test"},
		Host:                     routingHost(),
		Source:                   "test",
	}
	override.Scope = canonicalManualOverrideScope(override)
	return override
}

func replaceStoredQuotaReset(t *testing.T, ctx context.Context, store storage.Store, quotaSnapshotID string, resetAt time.Time) error {
	t.Helper()
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		var payload string
		if err := tx.QueryRow(ctx, `SELECT payload_json FROM quota_snapshots WHERE quota_snapshot_id = ?`, quotaSnapshotID).Scan(&payload); err != nil {
			return err
		}
		var snapshot providerinventory.QuotaSnapshot
		if err := json.Unmarshal([]byte(payload), &snapshot); err != nil {
			return err
		}
		canonical := resetAt.UTC().Format(time.RFC3339Nano)
		snapshot.ResetAt = canonical
		snapshot.WindowEnd = canonical
		snapshot.ValidUntil = canonical
		snapshot.StaleAfter = canonical
		updated, err := delivery.CanonicalJSON(snapshot)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE quota_snapshots SET payload_json = ?, stale_after = ? WHERE quota_snapshot_id = ?`, string(updated), canonical, quotaSnapshotID)
		return err
	})
}

func hasComponent(components []ComponentScore, name ComponentName) bool {
	for _, component := range components {
		if component.Name == name {
			return true
		}
	}
	return false
}

func componentByName(t *testing.T, components []ComponentScore, name ComponentName) ComponentScore {
	t.Helper()
	for _, component := range components {
		if component.Name == name {
			return component
		}
	}
	t.Fatalf("missing component %s in %#v", name, components)
	return ComponentScore{}
}

func assertRoutingDecisionCount(t *testing.T, ctx context.Context, store storage.Store, projectID, deliveryRunID, decisionKey string, want int) {
	t.Helper()
	var count int
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM routing_decisions WHERE project_id = ? AND delivery_run_id = ? AND decision_key = ?`, projectID, deliveryRunID, decisionKey).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count routing decisions: %v", err)
	}
	if count != want {
		t.Fatalf("routing decision count = %d, want %d", count, want)
	}
}

func latestRoutingDecision(ctx context.Context, store storage.Store, projectID, deliveryRunID, decisionKey string) (RoutingDecision, error) {
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM routing_decisions
			WHERE project_id = ? AND delivery_run_id = ? AND decision_key = ?
			ORDER BY created_at DESC, routing_decision_id DESC LIMIT 1`, projectID, deliveryRunID, decisionKey).Scan(&payload)
	})
	if err != nil {
		return RoutingDecision{}, err
	}
	var decision RoutingDecision
	if err := json.Unmarshal([]byte(payload), &decision); err != nil {
		return RoutingDecision{}, err
	}
	return decision, nil
}

func refreshResultHasRefreshedProvider(result providerinventory.RefreshResult, adapterID string) bool {
	for _, provider := range result.Providers {
		if provider.AdapterID == adapterID && provider.Refreshed && provider.ErrorCode == "" {
			return true
		}
	}
	return false
}

func routingFixtureQuotaSource(adapterID string, now time.Time) providerinventory.QuotaTelemetrySource {
	at := delivery.CanonicalTimestamp(now)
	return providerinventory.QuotaTelemetrySource{
		QuotaSourceID:       "qsrc-fixture-" + adapterID,
		SchemaVersion:       providerinventory.QuotaTelemetrySourceSchema,
		RecordVersion:       1,
		AdapterID:           adapterID,
		SourceKind:          providerinventory.QuotaSourceFixture,
		SourceKey:           "fixture-quota-v1",
		SourceSchemaVersion: "fixture.quota.v1",
		SupportedQuantities: []providerinventory.QuantityKind{providerinventory.QuantityRequests},
		SupportedWindows:    []providerinventory.WindowKind{providerinventory.WindowFixedHour},
		ScopeDimensions:     []string{"provider", "account", "model"},
		ConfidenceContract:  map[string]providerinventory.Confidence{"remaining_value": providerinventory.ConfidenceExact, "reset_at": providerinventory.ConfidenceExact},
		NetworkDeclared:     false,
		TimeoutMS:           1000,
		ClassificationRules: []string{"fixture"},
		CreatedAt:           at,
		UpdatedAt:           at,
		PolicyVersion:       providerinventory.PolicyVersion,
	}
}

func rejectedDecisionCandidates(rejected []RejectedCandidate) []Candidate {
	out := make([]Candidate, 0, len(rejected))
	for _, candidate := range rejected {
		out = append(out, candidate.Candidate)
	}
	return out
}

func containsScoredCandidate(candidates []ScoredCandidate, id string) bool {
	for _, candidate := range candidates {
		if candidate.RoutingCandidateID == id {
			return true
		}
	}
	return false
}

func hasInputRecordRef(refs []InputRecordRef, kind, id string) bool {
	for _, ref := range refs {
		if ref.RecordKind == kind && ref.RecordID == id {
			return true
		}
	}
	return false
}

func hasRejectionCode(reasons []RejectionReason, code RejectionCode) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}
