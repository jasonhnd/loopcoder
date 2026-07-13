package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestRouteDecisionScoresOnlyEligibleCandidatesAndExplainsAlternatives(t *testing.T) {
	fixture := newFixture(t)
	scores := fixture.availabilityScores()
	for i := range scores {
		scores[i].ScoreConfidence = providerinventory.ConfidenceExact
		scores[i].Score = 75
		if scores[i].Scope.AdapterID == "claude" {
			scores[i].Score = 95
		}
	}
	req := workerRequirement("task-route-decision")
	input := DecisionInput{
		ProjectID:         "proj-routing",
		DeliveryRunID:     "drun-routing",
		DecisionKey:       "route-worker",
		TaskRequirementID: "treq-routing",
		PlanFingerprint:   "sha256:plan",
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
		Now: fixture.now,
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
	if !strings.Contains(string(stable), `"scored_candidates"`) || !strings.Contains(string(stable), `"alternatives"`) {
		t.Fatalf("stable explain JSON missing scored candidates/alternatives: %s", stable)
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
		ComponentQualityFit:     30,
		ComponentQuotaHeadroom:  10,
		ComponentCost:           10,
		ComponentLatency:        10,
		ComponentHealth:         10,
		ComponentDiversity:      10,
		ComponentLocality:       10,
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
	if loaded.DecisionStatus != DecisionStatusNoEligible || len(loaded.RejectedCandidates) != 1 || len(loaded.Alternatives) == 0 {
		t.Fatalf("loaded blocked decision missing rejection evidence: %#v", loaded)
	}
}

func replayDecisionInput(fixture hardFixture) DecisionInput {
	scores := fixture.availabilityScores()
	for i := range scores {
		scores[i].Score = 90
		scores[i].ScoreConfidence = providerinventory.ConfidenceExact
	}
	req := workerRequirement("task-a")
	return DecisionInput{
		ProjectID:         "proj-routing",
		DeliveryRunID:     "drun-routing",
		DecisionKey:       "route-worker",
		TaskRequirementID: "treq-routing",
		PlanFingerprint:   "sha256:plan",
		DecidedBy: delivery.Actor{
			ActorKind:         "system",
			ActorID:           "router",
			DecisionAuthority: "router",
			Source:            "test",
		},
		Host: delivery.Host{HostKind: "test", HostID: "routing-test"},
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
			"approved", "routing test", "sha256:input", "sha256:policy", "sha256:plan", "sha256:auth",
			"routing-test", "provider-launch", "approved", "none", at, at); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed storage: %v", err)
	}
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
