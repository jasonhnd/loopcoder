package routing

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestVerificationDecisionAcceptsIndependentVerifierAndReplaysStably(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()

	input := verificationInput(worker, verifier, passVerdict(verifier.ChosenCandidateID, "diff clean", "ev-diff"))
	first, err := DecideAndPersistVerification(ctx, store, input)
	if err != nil {
		t.Fatalf("DecideAndPersistVerification: %v", err)
	}
	if first.DecisionStatus != VerificationStatusAccepted || first.FinalAuthority != FinalAuthorityAutomatedVerifier {
		t.Fatalf("accepted decision = %#v", first)
	}
	if first.RequiredIndependence != taskrequirements.IndependenceDifferentProvider || first.ActualIndependence != taskrequirements.IndependenceDifferentProvider {
		t.Fatalf("independence = required %s actual %s", first.RequiredIndependence, first.ActualIndependence)
	}

	replayed, err := DecideAndPersistVerification(ctx, store, input)
	if err != nil {
		t.Fatalf("replay verification: %v", err)
	}
	if replayed.VerificationDecisionID != first.VerificationDecisionID || replayed.CreatedAt != first.CreatedAt || countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 1 {
		t.Fatalf("replay duplicated or rewrote decision: first=%#v replay=%#v count=%d", first, replayed, countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}
	human := ExplainVerificationHuman(first)
	for _, want := range []string{"final_authority=automated-verifier", "required=different-provider", "actual=different-provider"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human explain missing %q:\n%s", want, human)
		}
	}
	stable, err := ExplainVerificationJSON(first)
	if err != nil {
		t.Fatalf("ExplainVerificationJSON: %v", err)
	}
	for _, want := range []string{`"required_independence":"different-provider"`, `"final_authority":"automated-verifier"`, `"evidence_refs"`} {
		if !strings.Contains(string(stable), want) {
			t.Fatalf("json explain missing %q:\n%s", want, stable)
		}
	}
}

func TestVerificationDecisionRequiresHumanWhenIndependenceCannotBeEstablished(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("codex", "acct-b", "codex-verifier"), nil)
	defer store.Close()

	decision, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, passVerdict(verifier.ChosenCandidateID, "same provider", "ev-same-provider")))
	if !errors.Is(err, taskrequirements.ErrVerifierIndependenceRequired) {
		t.Fatalf("verification error = %v, want ErrVerifierIndependenceRequired", err)
	}
	if decision.DecisionStatus != VerificationStatusNeedsHuman || decision.FinalAuthority != FinalAuthorityHuman || decision.ActualIndependence != taskrequirements.IndependenceDifferentAccount {
		t.Fatalf("needs-human independence decision = %#v", decision)
	}
}

func TestVerifierIndependenceBoundariesAreOrderedAndNonWeakening(t *testing.T) {
	worker := Candidate{AdapterID: "codex", AccountProfileID: "acct-a", ModelCapabilityID: "model-a", RoutingCandidateID: "worker"}
	cases := []struct {
		name     string
		verifier Candidate
		level    taskrequirements.IndependenceLevel
		wantPass bool
		actual   taskrequirements.IndependenceLevel
	}{
		{"none", worker, taskrequirements.IndependenceNone, true, taskrequirements.IndependenceNone},
		{"different model", Candidate{AdapterID: "codex", AccountProfileID: "acct-a", ModelCapabilityID: "model-b", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceDifferentModel, true, taskrequirements.IndependenceDifferentModel},
		{"different account", Candidate{AdapterID: "codex", AccountProfileID: "acct-b", ModelCapabilityID: "model-b", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceDifferentAccount, true, taskrequirements.IndependenceDifferentAccount},
		{"different provider", Candidate{AdapterID: "claude", AccountProfileID: "acct-c", ModelCapabilityID: "model-c", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceDifferentProvider, true, taskrequirements.IndependenceDifferentProvider},
		{"same provider fails provider boundary", Candidate{AdapterID: "codex", AccountProfileID: "acct-b", ModelCapabilityID: "model-b", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceDifferentProvider, false, taskrequirements.IndependenceDifferentAccount},
		{"human cannot be automated", Candidate{AdapterID: "claude", AccountProfileID: "acct-c", ModelCapabilityID: "model-c", RoutingCandidateID: "verifier"}, taskrequirements.IndependenceHuman, false, taskrequirements.IndependenceDifferentProvider},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := independentEnough(worker, tt.verifier, tt.level); got != tt.wantPass {
				t.Fatalf("independentEnough = %t, want %t", got, tt.wantPass)
			}
			if got := actualIndependence(worker, tt.verifier); got != tt.actual {
				t.Fatalf("actualIndependence = %s, want %s", got, tt.actual)
			}
		})
	}

	req := workerRequirement("task-independence")
	req.RiskTier = taskrequirements.RiskHigh
	req.VerificationRequirements = []taskrequirements.VerificationRequirement{{
		VerificationKind:     taskrequirements.VerificationLoopReview,
		RequiredForRiskTiers: []taskrequirements.RiskTier{taskrequirements.RiskHigh},
		IndependenceLevel:    taskrequirements.IndependenceDifferentProvider,
		OutputContract:       taskrequirements.OutputVerificationVerdict,
	}}
	profile := BalancedRoutingPolicyProfile(time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC))
	profile.EligibilityPolicy.VerifierIndependence = taskrequirements.IndependenceDifferentAccount
	if got := RequiredVerifierIndependence(req, profile); got != taskrequirements.IndependenceDifferentProvider {
		t.Fatalf("required independence = %s, want fallback/profile not to weaken task requirement", got)
	}
}

func TestVerificationDecisionHumanRequiredNoEligibleAndOutputContractFailClosed(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)

	t.Run("human required", func(t *testing.T) {
		store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), func(req *taskrequirements.TaskRequirement) {
			req.VerificationRequirements = []taskrequirements.VerificationRequirement{{
				VerificationKind:     taskrequirements.VerificationHumanApproval,
				RequiredForRiskTiers: []taskrequirements.RiskTier{taskrequirements.RiskHigh},
				IndependenceLevel:    taskrequirements.IndependenceHuman,
				PermissionRequired:   taskrequirements.PermissionReadOnly,
				OutputContract:       taskrequirements.OutputVerificationVerdict,
			}}
		})
		defer store.Close()
		decision, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, passVerdict(verifier.ChosenCandidateID, "assist only", "ev-human")))
		if !errors.Is(err, taskrequirements.ErrVerifierIndependenceRequired) || decision.FinalAuthority != FinalAuthorityHuman {
			t.Fatalf("human-required decision/error = %#v / %v", decision, err)
		}
	})

	t.Run("no eligible verifier route", func(t *testing.T) {
		store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("codex", "acct-a", "codex-broken"), nil)
		defer store.Close()
		if verifier.DecisionStatus != DecisionStatusNoEligible {
			t.Fatalf("fixture verifier route status = %s, want no eligible", verifier.DecisionStatus)
		}
		decision, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier))
		if !errors.Is(err, taskrequirements.ErrNoEligibleCandidate) || decision.DecisionStatus != VerificationStatusNeedsHuman {
			t.Fatalf("no eligible decision/error = %#v / %v", decision, err)
		}
	})

	t.Run("output contract", func(t *testing.T) {
		store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), func(req *taskrequirements.TaskRequirement) {
			req.VerificationRequirements[0].OutputContract = taskrequirements.OutputFreeform
		})
		defer store.Close()
		decision, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, passVerdict(verifier.ChosenCandidateID, "freeform", "ev-output")))
		if !errors.Is(err, taskrequirements.ErrCapabilityUnsupported) || decision.DecisionStatus != VerificationStatusNeedsHuman {
			t.Fatalf("output contract decision/error = %#v / %v", decision, err)
		}
	})
}

func TestVerificationCouncilBoundsTimeoutAndDisagreementAreDurable(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()

	limits := CouncilLimits{
		Enabled:         true,
		MaxMembers:      2,
		MaxRounds:       1,
		MaxDurationMS:   1000,
		MaxBudgetTokens: 50,
		StartedAt:       fixture.now.Add(-2 * time.Second).Format(time.RFC3339Nano),
	}
	input := verificationInput(worker, verifier,
		passVerdict(verifier.ChosenCandidateID, "one pass", "ev-pass"),
		VerifierVerdict{MemberID: "member-b", RoutingCandidateID: verifier.ChosenCandidateID, Verdict: VerificationVerdictFail, Message: "found regression", Authority: "verifier", AuthorityFingerprint: verifier.RoutingFingerprint},
	)
	input.CouncilLimits = limits
	input.CouncilRoundsUsed = 1
	input.CouncilBudgetTokensUsed = 10
	input.Timeout = true

	decision, err := DecideAndPersistVerification(ctx, store, input)
	if !errors.Is(err, taskrequirements.ErrVerifierCouncilBoundExceeded) {
		t.Fatalf("council timeout error = %v, want ErrVerifierCouncilBoundExceeded", err)
	}
	if !decision.Timeout || decision.Council.BoundExceeded != "time" || len(decision.Disagreements) == 0 {
		t.Fatalf("council timeout decision missing durable state: %#v", decision)
	}
	loaded, err := LoadVerificationDecision(ctx, store, decision.VerificationDecisionID)
	if err != nil {
		t.Fatalf("LoadVerificationDecision: %v", err)
	}
	if !loaded.Timeout || loaded.Council.TerminalErrorCode != taskrequirements.ErrVerifierCouncilBoundExceededCode || len(loaded.VerifierVerdicts) != 2 {
		t.Fatalf("loaded council decision lost timeout/disagreement: %#v", loaded)
	}
}

func TestVerificationDecisionConcurrentDuplicateAndAuthorityChanges(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()
	input := verificationInput(worker, verifier, passVerdict(verifier.ChosenCandidateID, "concurrent", "ev-concurrent"))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := DecideAndPersistVerification(ctx, store, input)
			if err != nil {
				errs <- err
				return
			}
			ids <- decision.VerificationDecisionID
		}()
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatalf("concurrent verification: %v", err)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
			continue
		}
		if id != first {
			t.Fatalf("concurrent duplicate returned different ids: %s vs %s", first, id)
		}
	}
	if countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 1 {
		t.Fatalf("concurrent duplicate count = %d, want 1", countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}

	changed := input
	changed.IdempotencyKey = "changed-authority"
	changed.AuthorityFingerprint = testFingerprint("changed-authority")
	second, err := DecideAndPersistVerification(ctx, store, changed)
	if err != nil {
		t.Fatalf("changed authority verification: %v", err)
	}
	if second.VerificationDecisionID == first || countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 2 {
		t.Fatalf("changed authority did not create distinct history: second=%s first=%s count=%d", second.VerificationDecisionID, first, countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}
}

func TestVerificationDecisionRefusesOverwriteOfAcceptedAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, worker, verifier := persistVerificationRoutes(t, ctx, fixture, fixture.candidate("claude", "acct-c", "claude-good"), nil)
	defer store.Close()

	first, err := DecideAndPersistVerification(ctx, store, verificationInput(worker, verifier, passVerdict(verifier.ChosenCandidateID, "accepted", "ev-accepted")))
	if err != nil {
		t.Fatalf("first verification: %v", err)
	}
	overwrite := verificationInput(worker, verifier, VerifierVerdict{
		MemberID:             "member-a",
		RoutingCandidateID:   verifier.ChosenCandidateID,
		Verdict:              VerificationVerdictFail,
		Message:              "later contradictory verdict",
		Authority:            "verifier",
		AuthorityFingerprint: testFingerprint("verdict-overwrite"),
		EvidenceRefs:         []EvidenceRef{{RecordKind: "check", RecordID: "ev-overwrite", Summary: "later contradictory verdict"}},
	})
	overwrite.IdempotencyKey = "overwrite-attempt"
	_, err = DecideAndPersistVerification(ctx, store, overwrite)
	if !errors.Is(err, taskrequirements.ErrVerificationDecisionConflict) {
		t.Fatalf("overwrite error = %v, want ErrVerificationDecisionConflict", err)
	}
	loaded, err := LoadVerificationDecision(ctx, store, first.VerificationDecisionID)
	if err != nil {
		t.Fatalf("LoadVerificationDecision: %v", err)
	}
	if loaded.Verdict != VerificationVerdictPass || countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID) != 1 {
		t.Fatalf("accepted verdict was overwritten or duplicated: %#v count=%d", loaded, countVerificationDecisions(t, ctx, store, worker.RoutingDecisionID))
	}
}

func TestVerificationExplanationRedactsBeforeTruncation(t *testing.T) {
	secret := "AKIA" + strings.Repeat("A", 16)
	decision := VerificationDecision{
		SchemaVersion:             VerificationDecisionSchema,
		RecordVersion:             1,
		VerificationDecisionID:    "vdec_test",
		ProjectID:                 "proj",
		DeliveryRunID:             "run",
		TaskID:                    "task",
		TaskRequirementID:         "treq",
		WorkerRoutingDecisionID:   "rdec_worker",
		DecisionKey:               "verify",
		IdempotencyKey:            "verify",
		DecisionStatus:            VerificationStatusNeedsHuman,
		Verdict:                   VerificationVerdictNeedsHuman,
		RequiredIndependence:      taskrequirements.IndependenceDifferentProvider,
		ActualIndependence:        taskrequirements.IndependenceNone,
		WorkerCandidateID:         "worker",
		VerifierVerdicts:          []VerifierVerdict{{MemberID: "member", Verdict: VerificationVerdictNeedsHuman, Message: strings.Repeat("x", 300) + secret, EvidenceRefs: []EvidenceRef{{RecordKind: "log", RecordID: "ev", Summary: "/Users/example/private/path/with/token secret=" + secret}}}},
		Disagreements:             []string{strings.Repeat("y", 300) + " token=" + secret},
		EvidenceRefs:              []EvidenceRef{{RecordKind: "log", RecordID: "ev", Summary: strings.Repeat("z", 300) + secret}},
		FinalAuthority:            FinalAuthorityHuman,
		FinalAuthorityFingerprint: testFingerprint("auth"),
		VerificationFingerprint:   testFingerprint("verification"),
		PolicyFingerprint:         testFingerprint("policy"),
		PlanFingerprint:           testFingerprint("plan"),
		CreatedAt:                 "2026-07-13T00:00:00Z",
		UpdatedAt:                 "2026-07-13T00:00:00Z",
		DecidedBy:                 schedulerActor(),
		Host:                      routingHost(),
		TerminalErrorCode:         taskrequirements.ErrVerifierIndependenceRequiredCode,
	}
	out, err := ExplainVerificationJSON(decision)
	if err != nil {
		t.Fatalf("ExplainVerificationJSON: %v", err)
	}
	if strings.Contains(string(out), secret) || strings.Contains(string(out), "/Users/example") {
		t.Fatalf("explanation leaked secret/local path after truncation: %s", out)
	}
	if !strings.Contains(string(out), "[REDACTED]") {
		t.Fatalf("explanation did not include redaction marker: %s", out)
	}
}

func persistVerificationRoutes(t *testing.T, ctx context.Context, fixture hardFixture, verifierCandidate Candidate, mutateWorkerReq func(*taskrequirements.TaskRequirement)) (storage.Store, RoutingDecision, RoutingDecision) {
	t.Helper()
	store := openRoutingStore(t, ctx, fixture.now)
	workerInput := replayDecisionInput(fixture)
	workerReq := workerInput.Inputs.Requirement
	workerReq.RiskTier = taskrequirements.RiskHigh
	workerReq.VerificationRequirements = []taskrequirements.VerificationRequirement{{
		VerificationKind:     taskrequirements.VerificationLoopReview,
		RequiredForRiskTiers: []taskrequirements.RiskTier{taskrequirements.RiskHigh, taskrequirements.RiskCritical},
		IndependenceLevel:    taskrequirements.IndependenceDifferentProvider,
		PermissionRequired:   taskrequirements.PermissionReadOnly,
		OutputContract:       taskrequirements.OutputVerificationVerdict,
	}}
	if mutateWorkerReq != nil {
		mutateWorkerReq(&workerReq)
	}
	workerInput.Inputs.Requirement = persistFallbackTaskRequirement(t, ctx, store, workerReq, fixture.now)
	workerInput.TaskRequirementID = workerInput.Inputs.Requirement.TaskRequirementID
	profile := BalancedRoutingPolicyProfile(fixture.now)
	workerInput.RoutingPolicyProfile = profile
	workerInput.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	workerInput.PolicyFingerprint = profile.PolicyFingerprint
	workerInput.Inputs.Policy = profile.EligibilityPolicy
	for i := range workerInput.Inputs.Inventory.QuotaSnapshots {
		workerInput.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
	}
	for i := range workerInput.Inputs.Availability {
		workerInput.Inputs.Availability[i].ScoreConfidence = providerinventory.ConfidenceExact
		if workerInput.Inputs.Availability[i].Scope.AdapterID == "codex" {
			workerInput.Inputs.Availability[i].Score = 99
		} else {
			workerInput.Inputs.Availability[i].Score = 70
		}
	}
	worker, err := BuildRoutingDecision(workerInput)
	if err != nil {
		t.Fatalf("BuildRoutingDecision worker: %v", err)
	}
	if err := PersistRoutingDecision(ctx, store, worker); err != nil {
		t.Fatalf("PersistRoutingDecision worker: %v", err)
	}

	verifierReq := decisionRequirement(t, fixture, verifierRequirement("task-verifier-route"), "treq-verifier-route", worker.PlanFingerprint)
	verifierReq.TaskID = "task-verifier-route"
	verifierReq.TaskKey = verifierReq.TaskID
	verifierReq = persistFallbackTaskRequirement(t, ctx, store, verifierReq, fixture.now)
	verifierCandidate.TaskID = verifierReq.TaskID
	verifierCandidate.RoleKey = RoleKeyVerifier
	verifierCandidate.Permission = taskrequirements.PermissionReadOnly
	verifierCandidate.LaunchSideEffectClass = taskrequirements.SideEffectProviderLaunch
	verifierCandidate.RoutingCandidateID = candidateID(verifierCandidate)
	verifierCandidate.CandidateFingerprint = candidateFingerprint(verifierCandidate)
	verifierInput := workerInput
	verifierInput.DecisionKey = "route-verifier"
	verifierInput.TaskRequirementID = verifierReq.TaskRequirementID
	verifierInput.RoleDefinitionID = "role-verifier"
	verifierInput.Inputs.Requirement = verifierReq
	verifierInput.Inputs.Candidates = []Candidate{verifierCandidate}
	verifierInput.Inputs.WorkerRoute = nil
	verifier, err := BuildRoutingDecision(verifierInput)
	if err != nil {
		t.Fatalf("BuildRoutingDecision verifier: %v", err)
	}
	if err := PersistRoutingDecision(ctx, store, verifier); err != nil {
		t.Fatalf("PersistRoutingDecision verifier: %v", err)
	}
	return store, worker, verifier
}

func verificationInput(worker, verifier RoutingDecision, verdicts ...VerifierVerdict) VerificationDecisionInput {
	return VerificationDecisionInput{
		WorkerRoutingDecisionID:   worker.RoutingDecisionID,
		VerifierRoutingDecisionID: verifier.RoutingDecisionID,
		DecisionKey:               "verify-worker",
		IdempotencyKey:            "verify-worker-key",
		VerifierVerdicts:          verdicts,
		AuthorityFingerprint:      worker.RoutingFingerprint,
		DecidedBy:                 schedulerActor(),
		Host:                      routingHost(),
	}
}

func passVerdict(candidateID, message, evidenceID string) VerifierVerdict {
	return VerifierVerdict{
		MemberID:             "member-a",
		RoutingCandidateID:   candidateID,
		Verdict:              VerificationVerdictPass,
		Message:              message,
		Authority:            "verifier",
		AuthorityFingerprint: testFingerprint("verdict-" + evidenceID),
		EvidenceRefs:         []EvidenceRef{{RecordKind: "check", RecordID: evidenceID, Summary: message}},
	}
}

func countVerificationDecisions(t *testing.T, ctx context.Context, store storage.Store, workerRoutingDecisionID string) int {
	t.Helper()
	var count int
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM verification_decisions WHERE worker_routing_decision_id = ?`, workerRoutingDecisionID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count verification decisions: %v", err)
	}
	return count
}
