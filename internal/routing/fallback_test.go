package routing

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestFallbackSelectsFreshHardEligibleCandidateAndPersistsReplaySafely(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, input := persistFallbackOriginalRoute(t, ctx, store, fixture)

	fallback, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerRateLimited,
		PriorCandidateID:  original.ChosenCandidateID,
		IdempotencyKey:    "rate-limit-reset-hour-1",
		Inputs:            input.Inputs,
		AttemptLineage:    []string{"att_worker_1"},
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if err != nil {
		t.Fatalf("DecideAndPersistFallback: %v", err)
	}
	if fallback.DecisionStatus != FallbackStatusSelected || fallback.FallbackCandidateID == "" || fallback.FallbackCandidateID == original.ChosenCandidateID {
		t.Fatalf("fallback status/candidate = %s/%s, original %s", fallback.DecisionStatus, fallback.FallbackCandidateID, original.ChosenCandidateID)
	}
	if fallback.BoundsRemaining.FallbacksLeft != 2 {
		t.Fatalf("fallback bounds = %#v, want two remaining", fallback.BoundsRemaining)
	}
	if len(fallback.LegalityResults) != len(fallbackMatrixRows()) {
		t.Fatalf("legality rows = %d, want full matrix %d", len(fallback.LegalityResults), len(fallbackMatrixRows()))
	}
	for _, row := range fallback.LegalityResults {
		if !row.Legal {
			t.Fatalf("selected fallback has illegal row: %#v", row)
		}
	}

	replayed, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerRateLimited,
		PriorCandidateID:  original.ChosenCandidateID,
		IdempotencyKey:    "rate-limit-reset-hour-1",
		Inputs:            input.Inputs,
		AttemptLineage:    []string{"att_worker_1"},
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if err != nil {
		t.Fatalf("replay fallback: %v", err)
	}
	if replayed.FallbackDecisionID != fallback.FallbackDecisionID || countFallbackDecisions(t, ctx, store, original.RoutingDecisionID) != 1 {
		t.Fatalf("replay created duplicate fallback: first=%s replay=%s count=%d", fallback.FallbackDecisionID, replayed.FallbackDecisionID, countFallbackDecisions(t, ctx, store, original.RoutingDecisionID))
	}
	loaded, err := LoadFallbackDecision(ctx, store, fallback.FallbackDecisionID)
	if err != nil {
		t.Fatalf("LoadFallbackDecision: %v", err)
	}
	if loaded.FallbackCandidateID != fallback.FallbackCandidateID || len(loaded.AttemptLineage) != 1 {
		t.Fatalf("loaded fallback lost persisted route/attempt lineage: %#v", loaded)
	}
}

func TestFallbackRejectsSimultaneousOutageQuotaResetAndAuthModelRemoval(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, input := persistFallbackOriginalRoute(t, ctx, store, fixture)

	for i := range input.Inputs.Inventory.AuthReadiness {
		if input.Inputs.Inventory.AuthReadiness[i].AdapterID == "claude" {
			input.Inputs.Inventory.AuthReadiness[i].ReadinessState = providerinventory.ReadinessNotAuthenticated
			input.Inputs.Inventory.AuthReadiness[i].FreshnessState = providerinventory.FreshnessExpired
		}
	}
	for i := range input.Inputs.Inventory.ModelCapabilities {
		if input.Inputs.Inventory.ModelCapabilities[i].AdapterID == "claude" {
			input.Inputs.Inventory.ModelCapabilities[i].LifecycleState = providerinventory.LifecycleRemoved
			input.Inputs.Inventory.ModelCapabilities[i].AvailabilityState = providerinventory.AvailabilityRemoved
		}
	}
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].FreshnessState = providerinventory.FreshnessStale
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceStale
			input.Inputs.Inventory.QuotaSnapshots[i].StaleAfter = fixture.now.Add(-time.Second).Format(time.RFC3339Nano)
		}
	}

	fallback, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerModelRemoved,
		PriorCandidateID:  original.ChosenCandidateID,
		IdempotencyKey:    "simultaneous-outage-quota-reset",
		Inputs:            input.Inputs,
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("fallback error = %v, want ErrReplanRequired", err)
	}
	if fallback.DecisionStatus != FallbackStatusReplanRequired || fallback.FallbackCandidateID != "" {
		t.Fatalf("fallback refusal = %#v", fallback)
	}
	for _, want := range []string{"auth_readiness", "minimum_capability", "quota_confidence"} {
		if !hasIllegalLegalityRow(fallback.LegalityResults, want) {
			t.Fatalf("fallback legality missing illegal %s row: %#v", want, fallback.LegalityResults)
		}
	}
}

func TestFallbackRecoversPersistedUserPinsAndRefusesForbiddenCandidate(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()

	input := replayDecisionInput(fixture)
	input.Inputs.Requirement = persistFallbackTaskRequirement(t, ctx, store, input.Inputs.Requirement, fixture.now)
	input.TaskRequirementID = input.Inputs.Requirement.TaskRequirementID
	profile := BalancedRoutingPolicyProfile(fixture.now)
	pin := persistPolicyPin(t, ctx, store, profile, CandidateConstraint{AdapterID: "claude"})
	input.RoutingPolicyProfile = profile
	input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	input.PolicyFingerprint = profile.PolicyFingerprint
	input.PolicyInputRecords = []PolicyInputRecord{pin}
	input.Inputs.Policy = profile.EligibilityPolicy
	input.Inputs.Pins = pinsForRecord(pin)
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	original, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if original.ChosenCandidateID == "" || original.UserPinRefs[0] != pin.RoutingPolicyInputID {
		t.Fatalf("original route did not persist user pin: %#v", original)
	}
	if err := PersistRoutingDecision(ctx, store, original); err != nil {
		t.Fatalf("PersistRoutingDecision: %v", err)
	}

	caller := input.Inputs
	caller.Pins = nil
	fallback, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerRateLimited,
		PriorCandidateID:  original.ChosenCandidateID,
		Inputs:            caller,
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("fallback error = %v, want ErrReplanRequired", err)
	}
	if fallback.FallbackCandidateID != "" || !hasIllegalLegalityRow(fallback.LegalityResults, "user_pin") {
		t.Fatalf("fallback ignored recovered persisted pin: %#v", fallback)
	}
}

func TestFallbackRejectsCallerRequirementMutationsBeforeEligibility(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, input := persistFallbackOriginalRoute(t, ctx, store, fixture)

	cases := map[string]func(*taskrequirements.TaskRequirement){
		"permission": func(req *taskrequirements.TaskRequirement) {
			req.PermissionRequired = taskrequirements.PermissionReadOnly
		},
		"risk": func(req *taskrequirements.TaskRequirement) { req.RiskTier = taskrequirements.RiskLow },
		"verification": func(req *taskrequirements.TaskRequirement) {
			req.VerificationRequirements = []taskrequirements.VerificationRequirement{{VerificationKind: taskrequirements.VerificationNone}}
		},
		"scope": func(req *taskrequirements.TaskRequirement) { req.ScopeJSON = `{"paths":["README.md"]}` },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			caller := input.Inputs
			mutate(&caller.Requirement)
			_, err := DecideAndPersistFallback(ctx, store, FallbackInput{
				RoutingDecisionID: original.RoutingDecisionID,
				Trigger:           FallbackTriggerTimeout,
				PriorCandidateID:  original.ChosenCandidateID,
				Inputs:            caller,
				AttemptLineage:    []string{"attempt-" + name},
				DecidedBy:         schedulerActor(),
				Host:              routingHost(),
			})
			if !errors.Is(err, taskrequirements.ErrFallbackWouldWeakenPolicy) {
				t.Fatalf("mutated requirement fallback error = %v, want ErrFallbackWouldWeakenPolicy", err)
			}
		})
	}
	if countFallbackDecisions(t, ctx, store, original.RoutingDecisionID) != 0 {
		t.Fatalf("mutated caller requirement persisted fallback decision")
	}
}

func TestFallbackReplanBoundsTerminateWithoutRetryLoop(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, input := persistFallbackOriginalRoute(t, ctx, store, fixture)

	first, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerWorkerFailed,
		PriorCandidateID:  original.ChosenCandidateID,
		IdempotencyKey:    "fallback-bound-a",
		Inputs:            input.Inputs,
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if err != nil {
		t.Fatalf("fallback 1: %v", err)
	}
	replayed, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerWorkerFailed,
		PriorCandidateID:  original.ChosenCandidateID,
		IdempotencyKey:    "caller-key-must-not-matter",
		Inputs:            input.Inputs,
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if err != nil {
		t.Fatalf("fallback replay with original prior: %v", err)
	}
	if replayed.FallbackDecisionID != first.FallbackDecisionID || countFallbackDecisions(t, ctx, store, original.RoutingDecisionID) != 1 {
		t.Fatalf("replay created duplicate fallback: first=%s replay=%s count=%d", first.FallbackDecisionID, replayed.FallbackDecisionID, countFallbackDecisions(t, ctx, store, original.RoutingDecisionID))
	}
	blocked, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerWorkerFailed,
		PriorCandidateID:  first.FallbackCandidateID,
		IdempotencyKey:    "fallback-bound-b",
		Inputs:            input.Inputs,
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("bound fallback error = %v, want ErrReplanRequired", err)
	}
	if blocked.DecisionStatus != FallbackStatusReplanRequired {
		t.Fatalf("bound fallback decision = %#v", blocked)
	}

	for i := 0; i < 2; i++ {
		trigger := []ReplanTrigger{ReplanTriggerLegalFallbackExhausted, ReplanTriggerNoEligibleCandidate}[i]
		_, err := DecideAndPersistReplan(ctx, store, ReplanInput{
			ProjectID:            original.ProjectID,
			DeliveryRunID:        original.DeliveryRunID,
			RoutingDecisionID:    original.RoutingDecisionID,
			Trigger:              trigger,
			PriorPlanFingerprint: original.PlanFingerprint,
			NewPlanFingerprint:   original.PlanFingerprint,
			IdempotencyKey:       "replan-" + string(rune('a'+i)),
			DecidedBy:            schedulerActor(),
			Host:                 routingHost(),
		})
		if err != nil {
			t.Fatalf("replan %d: %v", i+1, err)
		}
	}
	exhausted, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		RoutingDecisionID:    original.RoutingDecisionID,
		Trigger:              ReplanTriggerGraphBoundHit,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   original.PlanFingerprint,
		IdempotencyKey:       "replan-c",
		DecidedBy:            schedulerActor(),
		Host:                 routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanBoundExceeded) {
		t.Fatalf("replan bound error = %v, want ErrReplanBoundExceeded", err)
	}
	if exhausted.DecisionStatus != ReplanStatusBoundExceeded || exhausted.BoundsRemaining.ReplanPassesLeft != 0 {
		t.Fatalf("exhausted replan = %#v", exhausted)
	}
}

func TestFallbackRejectsForgedPriorAfterCanonicalLineageMoves(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, input := persistFallbackOriginalRoute(t, ctx, store, fixture)

	first, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerWorkerFailed,
		PriorCandidateID:  original.ChosenCandidateID,
		Inputs:            input.Inputs,
		AttemptLineage:    []string{"att-1"},
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if err != nil {
		t.Fatalf("first fallback: %v", err)
	}
	_, err = DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerTimeout,
		PriorCandidateID:  original.ChosenCandidateID,
		Inputs:            input.Inputs,
		AttemptLineage:    []string{"forged-new-event"},
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("forged prior error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	if first.FallbackCandidateID == "" || countFallbackDecisions(t, ctx, store, original.RoutingDecisionID) != 1 {
		t.Fatalf("forged prior changed fallback lineage")
	}
}

func TestCancellationStopsReplanAndCancelsHeldReservation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	reservation := reserveCancellationBudget(t, ctx, store, fixture.now)

	_, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:                 "proj-routing",
		DeliveryRunID:             "drun-routing",
		Trigger:                   ReplanTriggerUserChangedIntent,
		PriorPlanFingerprint:      testFingerprint("plan-routing"),
		NewPlanFingerprint:        testFingerprint("cancelled-plan"),
		IdempotencyKey:            "cancelled-replan",
		Cancelled:                 true,
		HeldBudgetReservationID:   reservation.BudgetReservationID,
		HeldReservationGeneration: reservation.Generation,
		DecidedBy:                 schedulerActor(),
		Host:                      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("cancelled replan error = %v, want ErrReplanRequired", err)
	}
	if countReplanDecisions(t, ctx, store, "proj-routing", "drun-routing") != 0 {
		t.Fatalf("cancelled replan persisted a decision")
	}
	if got := budgetReservationState(t, ctx, store, reservation.BudgetReservationID); got != budget.StateCancelled {
		t.Fatalf("reservation state = %s, want cancelled", got)
	}
}

func TestCancellationDiscoveredAtWriteBoundaryReleasesRunReservations(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	base := openRoutingStore(t, ctx, fixture.now)
	defer base.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, base, fixture)
	reservation := reserveCancellationBudget(t, ctx, base, fixture.now)
	hooked := &writeHookStore{Store: base, beforeWrite: func() {
		updateDeliveryRunState(t, ctx, base, original.ProjectID, original.DeliveryRunID, delivery.RunCancelled)
	}}

	_, err := DecideAndPersistReplan(ctx, hooked, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		RoutingDecisionID:    original.RoutingDecisionID,
		Trigger:              ReplanTriggerUserChangedIntent,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   original.PlanFingerprint,
		DecidedBy:            schedulerActor(),
		Host:                 routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("write-boundary cancellation error = %v, want ErrReplanRequired", err)
	}
	if countReplanDecisions(t, ctx, base, original.ProjectID, original.DeliveryRunID) != 0 {
		t.Fatalf("write-boundary cancellation persisted a replan decision")
	}
	if got := budgetReservationState(t, ctx, base, reservation.BudgetReservationID); got != budget.StateCancelled {
		t.Fatalf("reservation state = %s, want cancelled", got)
	}
}

func TestCancellationStorageReadErrorPropagatesWithoutDecision(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	base := openRoutingStore(t, ctx, fixture.now)
	defer base.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, base, fixture)
	sentinel := errors.New("cancellation reservation query failed")
	failing := txErrorStore{Store: base, err: sentinel}

	_, err := DecideAndPersistReplan(ctx, failing, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		Trigger:              ReplanTriggerUserChangedIntent,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   original.PlanFingerprint,
		Cancelled:            true,
		DecidedBy:            schedulerActor(),
		Host:                 routingHost(),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("cancellation storage error = %v, want sentinel", err)
	}
	if countReplanDecisions(t, ctx, base, original.ProjectID, original.DeliveryRunID) != 0 {
		t.Fatalf("storage-error cancellation persisted a replan decision")
	}
}

func TestCancellationWrongSuppliedGenerationFailsWhileHeld(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, store, fixture)
	reservation := reserveCancellationBudget(t, ctx, store, fixture.now)

	_, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:                 original.ProjectID,
		DeliveryRunID:             original.DeliveryRunID,
		Trigger:                   ReplanTriggerUserChangedIntent,
		PriorPlanFingerprint:      original.PlanFingerprint,
		NewPlanFingerprint:        original.PlanFingerprint,
		Cancelled:                 true,
		HeldBudgetReservationID:   reservation.BudgetReservationID,
		HeldReservationGeneration: reservation.Generation + 1,
		DecidedBy:                 schedulerActor(),
		Host:                      routingHost(),
	})
	if !errors.Is(err, budget.ErrReservationExpired) {
		t.Fatalf("wrong generation error = %v, want ErrReservationExpired", err)
	}
	if got := budgetReservationState(t, ctx, store, reservation.BudgetReservationID); got != budget.StateActive {
		t.Fatalf("reservation state = %s, want active", got)
	}
	if countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID) != 0 {
		t.Fatalf("wrong-generation cancellation persisted a replan decision")
	}
}

func TestCancellationOmittedReservationIDDiscoversActiveRunReservation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, store, fixture)
	reservation := reserveCancellationBudget(t, ctx, store, fixture.now)

	_, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		Trigger:              ReplanTriggerUserChangedIntent,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   original.PlanFingerprint,
		Cancelled:            true,
		DecidedBy:            schedulerActor(),
		Host:                 routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("omitted reservation cancellation error = %v, want ErrReplanRequired", err)
	}
	if got := budgetReservationState(t, ctx, store, reservation.BudgetReservationID); got != budget.StateCancelled {
		t.Fatalf("reservation state = %s, want cancelled", got)
	}
	if active := activeReservationIDsForRun(t, ctx, store, original.ProjectID, original.DeliveryRunID); len(active) != 0 {
		t.Fatalf("active reservations after cancellation = %#v, want none", active)
	}
}

func TestCancellationRejectsForeignReservationAndDoesNotMutateIt(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, store, fixture)
	foreign := reserveCancellationBudgetForRun(t, ctx, store, fixture.now, "proj-routing", "foreign-run", testFingerprint("auth"))

	_, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:                 original.ProjectID,
		DeliveryRunID:             original.DeliveryRunID,
		Trigger:                   ReplanTriggerUserChangedIntent,
		PriorPlanFingerprint:      original.PlanFingerprint,
		NewPlanFingerprint:        original.PlanFingerprint,
		Cancelled:                 true,
		HeldBudgetReservationID:   foreign.BudgetReservationID,
		HeldReservationGeneration: foreign.Generation,
		DecidedBy:                 schedulerActor(),
		Host:                      routingHost(),
	})
	if !errors.Is(err, budget.ErrReservationStateConflict) {
		t.Fatalf("foreign reservation error = %v, want ErrReservationStateConflict", err)
	}
	if got := budgetReservationState(t, ctx, store, foreign.BudgetReservationID); got != budget.StateActive {
		t.Fatalf("foreign reservation state = %s, want active", got)
	}
	if countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID) != 0 {
		t.Fatalf("foreign-reservation cancellation persisted a replan decision")
	}
}

func TestReplanChangedAuthorityRequiresExactFreshApproval(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, store, fixture)

	newPlan := testFingerprint("changed-authority-new-plan")
	needsHuman, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		RoutingDecisionID:    original.RoutingDecisionID,
		Trigger:              ReplanTriggerScopeChangeNeeded,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   newPlan,
		ChangedAuthorityInputs: []ChangedAuthorityInput{{
			InputKind: "plan_fingerprint",
			Previous:  original.PlanFingerprint,
			Current:   newPlan,
		}},
		AttemptLineage: []string{"no-approval"},
		DecidedBy:      schedulerActor(),
		Host:           routingHost(),
	})
	if !errors.Is(err, delivery.ErrApprovalRequired) {
		t.Fatalf("changed authority without approval error = %v, want ErrApprovalRequired", err)
	}
	if needsHuman.DecisionStatus != ReplanStatusNeedsHuman || needsHuman.NewPlanFingerprint != newPlan || !needsHuman.ApprovalRequired {
		t.Fatalf("changed authority without approval was launchable: %#v", needsHuman)
	}

	recordCurrentDeliveryApproval(t, ctx, store, "stale")
	stale, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		RoutingDecisionID:    original.RoutingDecisionID,
		Trigger:              ReplanTriggerScopeChangeNeeded,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   testFingerprint("changed-authority-stale-plan"),
		ChangedAuthorityInputs: []ChangedAuthorityInput{{
			InputKind: "plan_fingerprint",
			Previous:  original.PlanFingerprint,
			Current:   testFingerprint("changed-authority-stale-plan"),
		}},
		AttemptLineage: []string{"stale-approval"},
		DecidedBy:      schedulerActor(),
		Host:           routingHost(),
	})
	if !errors.Is(err, delivery.ErrApprovalRequired) || stale.DecisionStatus != ReplanStatusNeedsHuman {
		t.Fatalf("stale approval authorized changed replan: decision=%#v err=%v", stale, err)
	}

	freshStore := openRoutingStore(t, ctx, fixture.now)
	defer freshStore.Close()
	freshOriginal, _ := persistFallbackOriginalRoute(t, ctx, freshStore, fixture)
	freshPlan := testFingerprint("changed-authority-fresh-plan")
	recordCurrentDeliveryApproval(t, ctx, freshStore, "fresh")
	planned, err := DecideAndPersistReplan(ctx, freshStore, ReplanInput{
		ProjectID:            freshOriginal.ProjectID,
		DeliveryRunID:        freshOriginal.DeliveryRunID,
		RoutingDecisionID:    freshOriginal.RoutingDecisionID,
		Trigger:              ReplanTriggerScopeChangeNeeded,
		PriorPlanFingerprint: freshOriginal.PlanFingerprint,
		NewPlanFingerprint:   freshPlan,
		ChangedAuthorityInputs: []ChangedAuthorityInput{{
			InputKind: "plan_fingerprint",
			Previous:  freshOriginal.PlanFingerprint,
			Current:   freshPlan,
		}},
		AttemptLineage: []string{"fresh-approval"},
		DecidedBy:      schedulerActor(),
		Host:           routingHost(),
	})
	if !errors.Is(err, delivery.ErrApprovalRequired) {
		t.Fatalf("fresh approval replan error = %v, want ErrApprovalRequired", err)
	}
	if planned.DecisionStatus != ReplanStatusNeedsHuman || planned.NewPlanFingerprint != freshPlan || !planned.ApprovalRequired {
		t.Fatalf("fresh approval unexpectedly authorized changed replan: %#v", planned)
	}
}

func TestReplanInfersPlanFingerprintChangeRequiresApprovalWhenCallerOmitsAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, store, fixture)
	newPlan := testFingerprint("omitted-authority-new-plan")

	decision, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		RoutingDecisionID:    original.RoutingDecisionID,
		Trigger:              ReplanTriggerScopeChangeNeeded,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   newPlan,
		DecidedBy:            schedulerActor(),
		Host:                 routingHost(),
	})
	if !errors.Is(err, delivery.ErrApprovalRequired) {
		t.Fatalf("omitted authority plan change error = %v, want ErrApprovalRequired", err)
	}
	if decision.DecisionStatus != ReplanStatusNeedsHuman || !decision.ApprovalRequired || decision.NewPlanFingerprint != newPlan {
		t.Fatalf("omitted authority plan change decision = %#v, want retained non-launchable new plan", decision)
	}
	if countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID) != 1 {
		t.Fatalf("omitted authority plan change decision count = %d, want 1", countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID))
	}
}

func TestReplanRejectsWeakerCallerProfileSubstitution(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, store, fixture)
	fast, ok := BuiltInRoutingPolicyProfile(ProfileKeyFast, fixture.now)
	if !ok {
		t.Fatal("missing fast routing profile")
	}

	_, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:              original.ProjectID,
		DeliveryRunID:          original.DeliveryRunID,
		RoutingPolicyProfileID: fast.RoutingPolicyProfileID,
		Trigger:                ReplanTriggerLegalFallbackExhausted,
		PriorPlanFingerprint:   original.PlanFingerprint,
		NewPlanFingerprint:     original.PlanFingerprint,
		DecidedBy:              schedulerActor(),
		Host:                   routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrRoutingFingerprintMismatch) {
		t.Fatalf("weaker profile substitution error = %v, want ErrRoutingFingerprintMismatch", err)
	}
	if countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID) != 0 {
		t.Fatalf("weaker profile substitution persisted a replan decision")
	}
}

func TestFallbackRejectsChangedAuthorityMetadataWithoutSelectedRoute(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, input := persistFallbackOriginalRoute(t, ctx, store, fixture)

	_, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID:      original.RoutingDecisionID,
		Trigger:                FallbackTriggerRequirementsChanged,
		PriorCandidateID:       original.ChosenCandidateID,
		Inputs:                 input.Inputs,
		ChangedAuthorityInputs: []ChangedAuthorityInput{{InputKind: "scope", Previous: "a", Current: "b"}},
		ApprovalRequired:       true,
		DecidedBy:              schedulerActor(),
		Host:                   routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("fallback changed authority error = %v, want ErrReplanRequired", err)
	}
	if countFallbackDecisions(t, ctx, store, original.RoutingDecisionID) != 0 {
		t.Fatalf("fallback changed authority persisted a decision")
	}
}

func TestFallbackNoRouteReplayIgnoresCallerLineageAndAuthorityArrays(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, input := persistFallbackOriginalRoute(t, ctx, store, fixture)
	input.Inputs.Candidates = []Candidate{candidateByID(t, input.Inputs.Candidates, original.ChosenCandidateID)}

	first, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerQuotaExhausted,
		PriorCandidateID:  original.ChosenCandidateID,
		Inputs:            input.Inputs,
		AttemptLineage:    []string{"attempt-a"},
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("first no-route fallback error = %v, want ErrReplanRequired", err)
	}
	replayed, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerQuotaExhausted,
		PriorCandidateID:  original.ChosenCandidateID,
		Inputs:            input.Inputs,
		AttemptLineage:    []string{"attempt-b", "attempt-c"},
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("replayed no-route fallback error = %v, want ErrReplanRequired", err)
	}
	if replayed.FallbackDecisionID != first.FallbackDecisionID || countFallbackDecisions(t, ctx, store, original.RoutingDecisionID) != 1 {
		t.Fatalf("no-route replay created duplicate: first=%s replay=%s count=%d", first.FallbackDecisionID, replayed.FallbackDecisionID, countFallbackDecisions(t, ctx, store, original.RoutingDecisionID))
	}
}

func TestReplanReplayIgnoresCallerLineageAndAuthorityArrays(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, store, fixture)

	first, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		RoutingDecisionID:    original.RoutingDecisionID,
		Trigger:              ReplanTriggerNoEligibleCandidate,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   original.PlanFingerprint,
		AttemptLineage:       []string{"attempt-a"},
		DecidedBy:            schedulerActor(),
		Host:                 routingHost(),
	})
	if err != nil {
		t.Fatalf("first replan: %v", err)
	}
	replayed, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		RoutingDecisionID:    original.RoutingDecisionID,
		Trigger:              ReplanTriggerNoEligibleCandidate,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   original.PlanFingerprint,
		AttemptLineage:       []string{"attempt-b"},
		ChangedAuthorityInputs: []ChangedAuthorityInput{{
			InputKind: "caller-noise",
			Previous:  "old",
			Current:   "new",
		}},
		DecidedBy: schedulerActor(),
		Host:      routingHost(),
	})
	if err != nil {
		t.Fatalf("replayed replan: %v", err)
	}
	if replayed.ReplanDecisionID != first.ReplanDecisionID || countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID) != 1 {
		t.Fatalf("replan replay created duplicate: first=%s replay=%s count=%d", first.ReplanDecisionID, replayed.ReplanDecisionID, countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID))
	}
}

func TestReplanConcurrentReplayPersistsOneDecision(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, _ := persistFallbackOriginalRoute(t, ctx, store, fixture)

	const workers = 8
	var wg sync.WaitGroup
	results := make(chan ReplanDecision, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := DecideAndPersistReplan(ctx, store, ReplanInput{
				ProjectID:            original.ProjectID,
				DeliveryRunID:        original.DeliveryRunID,
				RoutingDecisionID:    original.RoutingDecisionID,
				Trigger:              ReplanTriggerNoEligibleCandidate,
				PriorPlanFingerprint: original.PlanFingerprint,
				NewPlanFingerprint:   original.PlanFingerprint,
				AttemptLineage:       []string{"caller-noise"},
				DecidedBy:            schedulerActor(),
				Host:                 routingHost(),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- decision
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent replay error: %v", err)
	}
	var first string
	for decision := range results {
		if first == "" {
			first = decision.ReplanDecisionID
		}
		if decision.ReplanDecisionID != first {
			t.Fatalf("concurrent replay returned different decision: %s vs %s", decision.ReplanDecisionID, first)
		}
	}
	if countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID) != 1 {
		t.Fatalf("concurrent replay decision count = %d, want 1", countReplanDecisions(t, ctx, store, original.ProjectID, original.DeliveryRunID))
	}
}

func openRoutingStore(t *testing.T, ctx context.Context, now time.Time) storage.Store {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: tempDB(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	seedRoutingDecisionStore(t, ctx, store, now)
	return store
}

func persistFallbackOriginalRoute(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture) (RoutingDecision, DecisionInput) {
	t.Helper()
	input := replayDecisionInput(fixture)
	input.Inputs.Requirement = persistFallbackTaskRequirement(t, ctx, store, input.Inputs.Requirement, fixture.now)
	input.TaskRequirementID = input.Inputs.Requirement.TaskRequirementID
	profile := BalancedRoutingPolicyProfile(fixture.now)
	input.RoutingPolicyProfile = profile
	input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	input.PolicyFingerprint = profile.PolicyFingerprint
	input.Inputs.Policy = profile.EligibilityPolicy
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	for i := range input.Inputs.Availability {
		input.Inputs.Availability[i].ScoreConfidence = providerinventory.ConfidenceExact
		if input.Inputs.Availability[i].Scope.AdapterID == "codex" {
			input.Inputs.Availability[i].Score = 99
		} else {
			input.Inputs.Availability[i].Score = 70
		}
	}
	decision, err := BuildRoutingDecision(input)
	if err != nil {
		t.Fatalf("BuildRoutingDecision: %v", err)
	}
	if err := PersistRoutingDecision(ctx, store, decision); err != nil {
		t.Fatalf("PersistRoutingDecision: %v", err)
	}
	if decision.ChosenCandidateID == "" {
		t.Fatal("fixture did not choose an original candidate")
	}
	return decision, input
}

func persistFallbackTaskRequirement(t *testing.T, ctx context.Context, store storage.Store, req taskrequirements.TaskRequirement, now time.Time) taskrequirements.TaskRequirement {
	t.Helper()
	at := delivery.CanonicalTimestamp(now)
	_, err := delivery.PersistTask(ctx, store, delivery.Task{
		SchemaVersion:            delivery.SchemaTask,
		RecordVersion:            1,
		TaskID:                   req.TaskID,
		TaskKey:                  req.TaskKey,
		DeliveryRunID:            req.DeliveryRunID,
		ProjectID:                req.ProjectID,
		State:                    delivery.TaskReady,
		Title:                    req.TaskKey,
		RequirementsJSON:         "{}",
		ScopeJSON:                "{}",
		Permission:               string(req.PermissionRequired),
		SideEffectClass:          string(req.SideEffectClass),
		PolicyVersion:            req.PolicyVersion,
		PlanFingerprint:          req.PlanFingerprint,
		AuthorizationFingerprint: testFingerprint("auth"),
		CreatedAt:                at,
		UpdatedAt:                at,
		ReadyAt:                  at,
		CreatedBy:                routerActor(),
		UpdatedBy:                routerActor(),
		Host:                     routingHost(),
	}, delivery.PersistOptions{IdempotencyKey: "task-" + req.TaskID, Now: now})
	if err != nil {
		t.Fatalf("persist fallback delivery task: %v", err)
	}
	req.TaskRequirementFingerprint = ""
	stored, err := taskrequirements.PersistTaskRequirement(ctx, store, req, taskrequirements.PersistOptions{Now: now})
	if err != nil {
		t.Fatalf("persist fallback task requirement: %v", err)
	}
	return stored
}

func persistPolicyPin(t *testing.T, ctx context.Context, store storage.Store, profile RoutingPolicyProfile, constraint CandidateConstraint) PolicyInputRecord {
	t.Helper()
	record, err := PersistPolicyInput(ctx, store, PolicyInputRecord{
		SchemaVersion:          PolicyInputSchema,
		RecordVersion:          1,
		InputKind:              PolicyInputKindPin,
		ProjectID:              "proj-routing",
		DeliveryRunID:          "drun-routing",
		RoutingPolicyProfileID: profile.RoutingPolicyProfileID,
		PolicyFingerprint:      profile.PolicyFingerprint,
		Scope:                  "delivery-run",
		Reason:                 "test pin",
		Status:                 PolicyInputStatusActive,
		Constraint:             constraint,
		ValidationStatus:       ValidationStatusValid,
		Actor:                  userActor(),
		Host:                   routingHost(),
	})
	if err != nil {
		t.Fatalf("PersistPolicyInput pin: %v", err)
	}
	return record
}

func userActor() delivery.Actor {
	return delivery.Actor{
		ActorKind:         "user",
		ActorID:           "user",
		DecisionAuthority: "user",
		Source:            "test",
	}
}

func recordCurrentDeliveryApproval(t *testing.T, ctx context.Context, store storage.Store, suffix string) {
	t.Helper()
	run := loadRunForApproval(t, ctx, store)
	_, err := delivery.RecordApproval(ctx, store, delivery.Approval{
		SchemaVersion:            delivery.SchemaApproval,
		RecordVersion:            1,
		ProjectID:                run.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		ApprovalKind:             "plan-approval",
		AuthorizationFingerprint: run.AuthorizationFingerprint,
		InputFingerprint:         run.InputFingerprint,
		PolicyFingerprint:        run.PolicyFingerprint,
		PlanFingerprint:          run.PlanFingerprint,
		ApprovedSideEffectClass:  run.MaxSideEffectClass,
		ApprovedScopeJSON:        "{}",
		ApprovedBy:               userActor(),
		Status:                   "active",
		CreatedBy:                userActor(),
		UpdatedBy:                userActor(),
		Host:                     routingHost(),
	}, delivery.PersistOptions{IdempotencyKey: "record-current-approval-" + suffix, Now: store.Now()})
	if err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
}

func loadRunForApproval(t *testing.T, ctx context.Context, store storage.Store) replanRunAuthority {
	t.Helper()
	var run replanRunAuthority
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, state, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint, max_side_effect_class
			FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, "proj-routing", "drun-routing").Scan(
			&run.ProjectID, &run.DeliveryRunID, &run.State, &run.InputFingerprint, &run.PolicyFingerprint, &run.PlanFingerprint, &run.AuthorizationFingerprint, &run.MaxSideEffectClass)
	})
	if err != nil {
		t.Fatalf("load run for approval: %v", err)
	}
	return run
}

func schedulerActor() delivery.Actor {
	return delivery.Actor{
		ActorKind:         "scheduler",
		ActorID:           "scheduler",
		DecisionAuthority: "scheduler",
		Source:            "test",
	}
}

func hasIllegalLegalityRow(rows []LegalityResult, dimension string) bool {
	for _, row := range rows {
		if row.Dimension == dimension && !row.Legal {
			return true
		}
	}
	return false
}

func countFallbackDecisions(t *testing.T, ctx context.Context, store storage.Store, routingDecisionID string) int {
	t.Helper()
	var count int
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM fallback_decisions WHERE routing_decision_id = ?`, routingDecisionID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count fallback decisions: %v", err)
	}
	return count
}

func countReplanDecisions(t *testing.T, ctx context.Context, store storage.Store, projectID, deliveryRunID string) int {
	t.Helper()
	var count int
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM replan_decisions WHERE project_id = ? AND delivery_run_id = ?`, projectID, deliveryRunID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count replan decisions: %v", err)
	}
	return count
}

func reserveCancellationBudget(t *testing.T, ctx context.Context, store storage.Store, now time.Time) budget.Reservation {
	t.Helper()
	result, err := budget.Reserve(ctx, store, budget.ReserveRequest{
		ScopeChain: []budget.Scope{{
			ScopeKind:     budget.ScopeDeliveryRun,
			ProjectID:     "proj-routing",
			DeliveryRunID: "drun-routing",
		}},
		QuantityKind:             providerinventory.QuantityRequests,
		Unit:                     "request",
		RequestedValue:           1,
		LeaseExpiresAt:           now.Add(time.Hour),
		IdempotencyKey:           "cancel-reservation",
		RequesterID:              "scheduler",
		AuthorizationFingerprint: testFingerprint("auth"),
		RequirementConfidence:    providerinventory.ConfidenceExact,
		Actor:                    budget.Actor{ActorID: "scheduler", Role: "scheduler"},
		Host:                     budget.Host{HostID: "routing-test"},
	})
	if err != nil {
		t.Fatalf("Reserve cancellation budget: %v", err)
	}
	return result.Reservation
}

func budgetReservationState(t *testing.T, ctx context.Context, store storage.Store, reservationID string) budget.ReservationState {
	t.Helper()
	return budgetReservation(t, ctx, store, reservationID).State
}

func budgetReservation(t *testing.T, ctx context.Context, store storage.Store, reservationID string) budget.Reservation {
	t.Helper()
	var payload string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM budget_reservations WHERE budget_reservation_id = ?`, reservationID).Scan(&payload)
	})
	if err != nil {
		t.Fatalf("load reservation: %v", err)
	}
	var reservation budget.Reservation
	if err := json.Unmarshal([]byte(payload), &reservation); err != nil {
		t.Fatalf("decode reservation: %v", err)
	}
	return reservation
}

func reserveCancellationBudgetForRun(t *testing.T, ctx context.Context, store storage.Store, now time.Time, projectID, deliveryRunID, auth string) budget.Reservation {
	t.Helper()
	result, err := budget.Reserve(ctx, store, budget.ReserveRequest{
		ScopeChain: []budget.Scope{{
			ScopeKind:     budget.ScopeDeliveryRun,
			ProjectID:     projectID,
			DeliveryRunID: deliveryRunID,
		}},
		QuantityKind:             providerinventory.QuantityRequests,
		Unit:                     "request",
		RequestedValue:           1,
		LeaseExpiresAt:           now.Add(time.Hour),
		IdempotencyKey:           "cancel-reservation-" + deliveryRunID,
		RequesterID:              "scheduler",
		AuthorizationFingerprint: auth,
		RequirementConfidence:    providerinventory.ConfidenceExact,
		Actor:                    budget.Actor{ActorID: "scheduler", Role: "scheduler"},
		Host:                     budget.Host{HostID: "routing-test"},
	})
	if err != nil {
		t.Fatalf("Reserve cancellation budget for run: %v", err)
	}
	return result.Reservation
}

func updateDeliveryRunState(t *testing.T, ctx context.Context, store storage.Store, projectID, deliveryRunID, state string) {
	t.Helper()
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delivery_runs SET state = ? WHERE project_id = ? AND delivery_run_id = ?`, state, projectID, deliveryRunID)
		return err
	})
	if err != nil {
		t.Fatalf("update delivery run state: %v", err)
	}
}

func activeReservationIDsForRun(t *testing.T, ctx context.Context, store storage.Store, projectID, deliveryRunID string) []string {
	t.Helper()
	ids, err := activeRunReservations(ctx, store, projectID, deliveryRunID)
	if err != nil {
		t.Fatalf("active reservations for run: %v", err)
	}
	return ids
}

func candidateByID(t *testing.T, candidates []Candidate, id string) Candidate {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.RoutingCandidateID == id {
			return candidate
		}
	}
	t.Fatalf("candidate %s not found", id)
	return Candidate{}
}

type writeHookStore struct {
	storage.Store
	beforeWrite func()
}

func (s *writeHookStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	if s.beforeWrite != nil {
		hook := s.beforeWrite
		s.beforeWrite = nil
		hook()
	}
	return s.Store.WithWriteTx(ctx, fn)
}

type txErrorStore struct {
	storage.Store
	err error
}

func (s txErrorStore) WithTx(context.Context, func(storage.Tx) error) error {
	return s.err
}
