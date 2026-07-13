package routing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/delivery"
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
	quotaScore := componentByName(t, decision.ScoredCandidates[0].Components, ComponentQuotaHeadroom)
	if quotaScore.Confidence != providerinventory.ConfidenceEstimated || !quotaScore.Heuristic || quotaScore.EvidenceValue == nil || *quotaScore.EvidenceValue != 80 || len(quotaScore.SnapshotIDs) != 1 || quotaScore.SnapshotIDs[0] != highQuota.QuotaSnapshotID {
		t.Fatalf("quota component mixed evidence: %#v", quotaScore)
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
		ComponentAvailability:  30,
		ComponentQuotaHeadroom: 20,
		ComponentQualityFit:    20,
		ComponentLatency:       10,
		ComponentCost:          10,
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
		if !hasComponent(candidate.Components, ComponentAvailability) {
			t.Fatalf("components missing availability score: %#v", candidate.Components)
		}
		if hasComponent(candidate.Components, "health") || hasComponent(candidate.Components, ComponentLocality) || hasComponent(candidate.Components, ComponentUserPreference) {
			t.Fatalf("balanced-v1 silently included non-default component: %#v", candidate.Components)
		}
	}
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

	later := fixture.now.Add(2 * time.Hour)
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
	first, err := DecideAndPersistRoute(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistRoute first: %v", err)
	}
	input.OptimizationPolicy.Weights = map[ComponentName]int{
		ComponentAvailability:   10,
		ComponentQualityFit:     20,
		ComponentQuotaHeadroom:  10,
		ComponentCost:           10,
		ComponentLatency:        10,
		ComponentDiversity:      10,
		ComponentLocality:       20,
		ComponentUserPreference: 10,
	}
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
	input.Inputs.Candidates = []Candidate{fixture.candidate("codex", "acct-a", "codex-broken")}
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
	if loaded.DecisionStatus != DecisionStatusNoEligible || len(loaded.RejectedCandidates) != 1 || len(loaded.RejectedSummary) == 0 {
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

func containsScoredCandidate(candidates []ScoredCandidate, id string) bool {
	for _, candidate := range candidates {
		if candidate.RoutingCandidateID == id {
			return true
		}
	}
	return false
}
