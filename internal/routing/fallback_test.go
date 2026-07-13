package routing

import (
	"context"
	"encoding/json"
	"errors"
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

func TestFallbackReplanBoundsTerminateWithoutRetryLoop(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store := openRoutingStore(t, ctx, fixture.now)
	defer store.Close()
	original, input := persistFallbackOriginalRoute(t, ctx, store, fixture)

	for i := 0; i < 3; i++ {
		_, err := DecideAndPersistFallback(ctx, store, FallbackInput{
			RoutingDecisionID: original.RoutingDecisionID,
			Trigger:           FallbackTriggerWorkerFailed,
			PriorCandidateID:  original.ChosenCandidateID,
			IdempotencyKey:    "fallback-bound-" + string(rune('a'+i)),
			Inputs:            input.Inputs,
			DecidedBy:         schedulerActor(),
			Host:              routingHost(),
		})
		if err != nil {
			t.Fatalf("fallback %d: %v", i+1, err)
		}
	}
	blocked, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: original.RoutingDecisionID,
		Trigger:           FallbackTriggerWorkerFailed,
		PriorCandidateID:  original.ChosenCandidateID,
		IdempotencyKey:    "fallback-bound-d",
		Inputs:            input.Inputs,
		DecidedBy:         schedulerActor(),
		Host:              routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("bound fallback error = %v, want ErrReplanRequired", err)
	}
	if blocked.DecisionStatus != FallbackStatusReplanRequired || blocked.BoundsRemaining.FallbacksLeft != 0 {
		t.Fatalf("bound fallback decision = %#v", blocked)
	}

	for i := 0; i < 2; i++ {
		_, err := DecideAndPersistReplan(ctx, store, ReplanInput{
			ProjectID:            original.ProjectID,
			DeliveryRunID:        original.DeliveryRunID,
			RoutingDecisionID:    original.RoutingDecisionID,
			Trigger:              ReplanTriggerLegalFallbackExhausted,
			PriorPlanFingerprint: original.PlanFingerprint,
			NewPlanFingerprint:   testFingerprint("new-plan-" + string(rune('a'+i))),
			IdempotencyKey:       "replan-" + string(rune('a'+i)),
			ChangedAuthorityInputs: []ChangedAuthorityInput{{
				InputKind: "plan_fingerprint",
				Previous:  original.PlanFingerprint,
				Current:   testFingerprint("new-plan-" + string(rune('a'+i))),
			}},
			DecidedBy: schedulerActor(),
			Host:      routingHost(),
		})
		if err != nil {
			t.Fatalf("replan %d: %v", i+1, err)
		}
	}
	exhausted, err := DecideAndPersistReplan(ctx, store, ReplanInput{
		ProjectID:            original.ProjectID,
		DeliveryRunID:        original.DeliveryRunID,
		RoutingDecisionID:    original.RoutingDecisionID,
		Trigger:              ReplanTriggerLegalFallbackExhausted,
		PriorPlanFingerprint: original.PlanFingerprint,
		NewPlanFingerprint:   testFingerprint("new-plan-c"),
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
	return reservation.State
}
