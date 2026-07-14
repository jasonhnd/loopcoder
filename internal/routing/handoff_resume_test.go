package routing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestResumeApprovedHandoffRoutesProviderAToProviderBOnce(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	var executions atomic.Int64
	var executedKey, executedAdapter string

	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:           handoff.HandoffID,
		DecisionInput:       input,
		ReservationValue:    1,
		ReusableEvidenceIDs: []string{"qsnap-codex-a-good", "availability-observation-a"},
		ExecuteSuccessor: func(_ context.Context, execution HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			executedKey = execution.Launch.ProviderIdempotencyKey
			executedAdapter = execution.Candidate.AdapterID
			return HandoffSuccessorExecutionResult{ProviderReceipt: "receipt-claude"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if err != nil {
		t.Fatalf("ResumeApprovedHandoff: %v\n%s", err, ExplainHuman(result.RoutingDecision))
	}
	if result.Successor.AttemptID == "" || result.Successor.SourceAttemptID != handoff.SourceAttemptID || result.Successor.HandoffGeneration != handoff.HandoffGeneration {
		t.Fatalf("successor launch = %#v, want linked successor", result.Successor)
	}
	if got := selectedCandidate(result.RoutingDecision).AdapterID; got != "claude" {
		t.Fatalf("selected adapter = %q, want provider B claude", got)
	}
	if executions.Load() != 1 || executedAdapter != "claude" || executedKey != result.Successor.ProviderIdempotencyKey {
		t.Fatalf("executor executions=%d adapter=%q key=%q want one claude launch with immutable provider key %q", executions.Load(), executedAdapter, executedKey, result.Successor.ProviderIdempotencyKey)
	}
	if result.Handoff.AcceptedTaskFingerprint != handoff.AcceptedTaskFingerprint || result.Handoff.AuthorizationFingerprint != handoff.AuthorizationFingerprint {
		t.Fatalf("handoff identity changed: %#v vs %#v", result.Handoff, handoff)
	}
	registration := handoffRegistrationState(t, ctx, store, handoff.ChildRunID)
	if registration.adapterID != "claude" || registration.sessionRef != "" || registration.attemptID != result.Successor.AttemptID || registration.state != storage.AgentStateRunning {
		t.Fatalf("registration after successor = %#v", registration)
	}

	replayed, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:           handoff.HandoffID,
		DecisionInput:       input,
		ReservationValue:    1,
		ReusableEvidenceIDs: []string{"qsnap-codex-a-good", "availability-observation-a"},
		ExecuteSuccessor: func(_ context.Context, execution HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "receipt-duplicate"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if err != nil {
		t.Fatalf("replay ResumeApprovedHandoff: %v", err)
	}
	if replayed.Successor.AttemptID != result.Successor.AttemptID || countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 1 {
		t.Fatalf("replay created duplicate successor: %#v first=%#v count=%d", replayed.Successor, result.Successor, countSuccessorAttempts(t, ctx, store, handoff.TaskID))
	}
	if replayed.Successor.LaunchExposed || executions.Load() != 1 {
		t.Fatalf("replay launch exposure=%t executions=%d, want replay observe only", replayed.Successor.LaunchExposed, executions.Load())
	}
}

func TestResumeApprovedHandoffConcurrentReplayLaunchesOnce(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	path := tempDB(t)
	store, handoff, input := handoffResumeFixtureAtPath(t, ctx, fixture, path)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	const workers = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	var executions atomic.Int64
	results := make(chan HandoffResumeResult, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := store
			if i%2 == 1 {
				other, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
				if err != nil {
					errs <- err
					return
				}
				defer other.Close()
				target = other
			}
			<-start
			result, err := ResumeApprovedHandoff(ctx, target, HandoffResumeInput{
				HandoffID:        handoff.HandoffID,
				DecisionInput:    input,
				ReservationValue: 1,
				ExecuteSuccessor: func(_ context.Context, execution HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
					executions.Add(1)
					return HandoffSuccessorExecutionResult{ProviderReceipt: "receipt-" + execution.Launch.ProviderIdempotencyKey}, nil
				},
				DecidedBy: routerActor(),
				Host:      routingHost(),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent resume error: %v", err)
	}
	var first string
	for result := range results {
		if first == "" {
			first = result.Successor.AttemptID
		}
		if result.Successor.AttemptID != first {
			t.Fatalf("concurrent successor attempt = %s, want %s", result.Successor.AttemptID, first)
		}
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 1 {
		t.Fatalf("successor attempts = %d, want 1", countSuccessorAttempts(t, ctx, store, handoff.TaskID))
	}
	if executions.Load() != 1 {
		t.Fatalf("successor provider executions = %d, want 1", executions.Load())
	}
}

func TestResumeApprovedHandoffNoEligiblePersistsBlockedDecision(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		remaining := int64(0)
		input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
	}
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrNoEligibleCandidate) {
		t.Fatalf("ResumeApprovedHandoff error = %v, want ErrNoEligibleCandidate", err)
	}
	if !result.Blocked || result.RoutingDecision.DecisionStatus != DecisionStatusNoEligible || len(result.RoutingDecision.RejectedSummary) == 0 {
		t.Fatalf("blocked result = %#v, want no-eligible decision with rejected reasons", result.RoutingDecision)
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("blocked route launched successor")
	}
}

func TestResumeApprovedHandoffRequiresExecutorBeforeReservation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrInvalidRecord) {
		t.Fatalf("nil executor error = %v, want ErrInvalidRecord", err)
	}
	if result.Reservation.BudgetReservationID != "" || countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("nil executor exposed launch authority: result=%#v attempts=%d", result, countSuccessorAttempts(t, ctx, store, handoff.TaskID))
	}
}

func TestResumeApprovedHandoffCancellationAfterReservationReleasesBeforeLaunch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		Cancelled:        true,
		ExecuteSuccessor: executeStarted("unused"),
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("cancelled resume error = %v, want ErrReplanRequired", err)
	}
	if result.Reservation.BudgetReservationID == "" {
		t.Fatalf("test did not reserve before cancellation")
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("reservation state = %q, want released", state)
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("cancelled resume launched successor")
	}
}

func TestResumeApprovedHandoffCancellationAfterPrepareTerminalizesLaunch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	cancelled := false
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterPrepare: func() error {
			cancelled = true
			return nil
		},
		IsCancelled: func(context.Context) bool { return cancelled },
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("post-prepare cancellation error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("post-prepare cancellation executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("post-prepare reservation state = %q, want released", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseCompleted || state.registrationState != storage.AgentStateCancelled || state.attemptState != "cancelled" || state.taskState != "cancelled" {
		t.Fatalf("post-prepare durable state = %#v, want cancelled terminal state", state)
	}
}

func TestResumeApprovedHandoffCancellationAfterRegistrationTerminalizesLaunch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	cancelled := false
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterRegistration: func() error {
			cancelled = true
			return nil
		},
		IsCancelled: func(context.Context) bool { return cancelled },
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("post-registration cancellation error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("post-registration cancellation executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("post-registration reservation state = %q, want released", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseCompleted || state.registrationState != storage.AgentStateCancelled || state.attemptState != "cancelled" || state.taskState != "cancelled" {
		t.Fatalf("post-registration durable state = %#v, want cancelled terminal state", state)
	}
}

func TestResumeApprovedHandoffCancellationAfterPrepareReleasesDestinationOwnership(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	cancelled := false
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterPrepare: func() error {
			cancelled = true
			return nil
		},
		IsCancelled: func(context.Context) bool { return cancelled },
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("post-prepare cancellation error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("post-prepare cancellation executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("post-prepare reservation state = %q, want released", state)
	}
	assertNoDestinationHeldLocks(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration)
	assertDestinationLockState(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration, storage.OwnershipStateReleased)
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseCompleted || state.registrationState != storage.AgentStateCancelled || state.attemptState != "cancelled" || state.taskState != "cancelled" {
		t.Fatalf("post-prepare durable state = %#v, want cancelled terminal state", state)
	}
}

func TestResumeApprovedHandoffCancellationAfterRegistrationReleasesDestinationOwnership(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	cancelled := false
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterRegistration: func() error {
			cancelled = true
			return nil
		},
		IsCancelled: func(context.Context) bool { return cancelled },
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("post-registration cancellation error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("post-registration cancellation executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("post-registration reservation state = %q, want released", state)
	}
	assertNoDestinationHeldLocks(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration)
	assertDestinationLockState(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration, storage.OwnershipStateReleased)
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseCompleted || state.registrationState != storage.AgentStateCancelled || state.attemptState != "cancelled" || state.taskState != "cancelled" {
		t.Fatalf("post-registration durable state = %#v, want cancelled terminal state", state)
	}
}

func TestResumeApprovedHandoffLiveCancellationAtLaunchBoundaryDoesNotExecute(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	cancelled := false
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		BeforeLaunch: func() error {
			cancelled = true
			return nil
		},
		IsCancelled: func(context.Context) bool { return cancelled },
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("live cancellation error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("cancelled handoff executed provider %d times", executions.Load())
	}
	if result.Reservation.BudgetReservationID == "" {
		t.Fatalf("test did not reserve before live cancellation")
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("cancelled destination reservation state = %q, want released", state)
	}
}

func TestResumeApprovedHandoffStaleGenerationFencePreventsLaunch(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_claims SET claim_generation = claim_generation + 1 WHERE run_id = ?`, handoff.ChildRunID)
		return err
	}); err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	_, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: executeStarted("unused"),
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, storage.ErrStaleClaim) {
		t.Fatalf("stale generation error = %v, want ErrStaleClaim", err)
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("stale generation launched successor")
	}
}

func TestResumeApprovedHandoffRejectsUnsafeArtifactRefAndReleasesReservation(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:           handoff.HandoffID,
		DecisionInput:       input,
		ReservationValue:    1,
		ReusableEvidenceIDs: []string{"provider-session-token"},
		ExecuteSuccessor:    executeStarted("unused"),
		DecidedBy:           routerActor(),
		Host:                routingHost(),
	})
	if err == nil {
		t.Fatalf("unsafe artifact ref unexpectedly succeeded")
	}
	if result.Reservation.BudgetReservationID == "" {
		t.Fatalf("test did not reserve before artifact validation")
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("unsafe artifact reservation state = %q, want released", state)
	}
	if countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 0 {
		t.Fatalf("unsafe artifact launched successor")
	}
}

func TestResumeApprovedHandoffReplayAfterPreLaunchStopReusesRouteAndLaunches(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	path := tempDB(t)
	store, handoff, input := handoffResumeFixtureAtPath(t, ctx, fixture, path)
	makeClaudeEligibleOnly(input)
	stopErr := errors.New("stop after reservation")
	first, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		BeforeLaunch:     func() error { return stopErr },
		ExecuteSuccessor: executeStarted("unused"),
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, stopErr) {
		t.Fatalf("pre-launch stop error = %v, want %v", err, stopErr)
	}
	if first.Reservation.BudgetReservationID == "" || reservationState(t, ctx, store, first.Reservation.BudgetReservationID) != string(budget.StateReleased) {
		t.Fatalf("pre-launch stop did not release reservation: %#v", first.Reservation)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	replayed, err := ResumeApprovedHandoff(ctx, reopened, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: executeStarted("receipt-replayed"),
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if replayed.RoutingDecision.RoutingDecisionID != first.RoutingDecision.RoutingDecisionID || replayed.Successor.AttemptID == "" {
		t.Fatalf("replay result = %#v, first route %s", replayed, first.RoutingDecision.RoutingDecisionID)
	}
}

func TestResumeApprovedHandoffExpiredLaunchOwnerBlocksReplayWithoutDuplicateExecute(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	first, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterPrepare:     func() error { return errors.New("simulated crash after prepare") },
		ExecuteSuccessor: executeStarted("unused"),
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("first ResumeApprovedHandoff error = %v, want ErrReplanRequired", err)
	}
	if !first.Successor.LaunchExposed || first.Successor.LaunchPhase != storage.ClaimPhaseLaunching {
		t.Fatalf("first successor launch = %#v, want exposed launch before cleanup", first.Successor)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_claims SET lease_expires_at = ? WHERE run_id = ?`, fixture.now.Add(-time.Minute).Format(time.RFC3339Nano), handoff.ChildRunID)
		return err
	}); err != nil {
		t.Fatalf("expire launch owner: %v", err)
	}
	var executions atomic.Int64
	replayed, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "duplicate"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("expired launch replay error = %v, want ErrReplanRequired; result=%#v", err, replayed)
	}
	if !replayed.Blocked || replayed.Successor.LaunchExposed || executions.Load() != 0 {
		t.Fatalf("expired launch replay result=%#v executions=%d, want blocked observe-only replay", replayed, executions.Load())
	}
}

func TestResumeApprovedHandoffDestinationFailureUsesBoundedFallback(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
	launchErr := errors.New("destination provider launch failed")
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionNotStarted}, launchErr
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("destination failure resume error = %v, want bounded ErrReplanRequired; result=%#v", err, result)
	}
	fallback := result.Fallback
	if fallback.DecisionStatus != FallbackStatusReplanRequired || fallback.BoundsRemaining.MaxFallbacks == 0 || len(fallback.AttemptLineage) != 2 {
		t.Fatalf("fallback = %#v, want bounded policy-preserving fallback with lineage", fallback)
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("destination failure reservation state = %q, want released", state)
	}
}

func TestResumeApprovedHandoffAmbiguousExecutorErrorDoesNotFallback(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	launchErr := errors.New("ambiguous transport error")
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionAmbiguous}, launchErr
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) || !errors.Is(err, launchErr) {
		t.Fatalf("ambiguous launch error = %v, want launchErr joined with ErrReplanRequired", err)
	}
	if result.Fallback.FallbackDecisionID != "" {
		t.Fatalf("ambiguous launch persisted fallback %#v, want none", result.Fallback)
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("ambiguous launch reservation state = %q, want released", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseCompleted || state.registrationState != storage.AgentStateNeedsHuman || state.attemptState != "needs-human" || state.taskState != "needs-human" {
		t.Fatalf("ambiguous launch durable state = %#v, want needs-human", state)
	}
}

func TestResumeApprovedHandoffNilErrorNotStartedFallsBackAndBlocksReplay(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionNotStarted}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("nil-error not-started error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("nil-error not-started executions = %d, want 1", executions.Load())
	}
	if result.Fallback.DecisionStatus != FallbackStatusReplanRequired || result.Fallback.FallbackDecisionID == "" {
		t.Fatalf("nil-error not-started fallback = %#v, want durable bounded fallback", result.Fallback)
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("nil-error not-started reservation state = %q, want released", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseCompleted || state.registrationState != storage.AgentStateNeedsHuman || state.attemptState != "needs-human" || state.taskState != "needs-human" {
		t.Fatalf("nil-error not-started durable state = %#v, want needs-human", state)
	}

	replayed, replayErr := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "duplicate"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(replayErr, taskrequirements.ErrReplanRequired) {
		t.Fatalf("nil-error not-started replay error = %v, want ErrReplanRequired", replayErr)
	}
	if executions.Load() != 1 || !replayed.Blocked || replayed.Successor.LaunchExposed || countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 1 {
		t.Fatalf("nil-error not-started replay result=%#v executions=%d attempts=%d, want blocked observe-only replay", replayed, executions.Load(), countSuccessorAttempts(t, ctx, store, handoff.TaskID))
	}
	if replayed.Reservation.BudgetReservationID != "" {
		if state := reservationState(t, ctx, store, replayed.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
			t.Fatalf("nil-error not-started replay reservation state = %q, want released", state)
		}
	}
	if got := countFallbackDecisions(t, ctx, store, result.RoutingDecision.RoutingDecisionID); got != 1 {
		t.Fatalf("nil-error not-started fallback decisions = %d, want 1", got)
	}
}

func TestResumeApprovedHandoffNilErrorZeroOutcomeNeedsHumanWithoutFallback(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("nil-error zero outcome error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("nil-error zero outcome executions = %d, want 1", executions.Load())
	}
	if result.Fallback.FallbackDecisionID != "" || countFallbackDecisions(t, ctx, store, result.RoutingDecision.RoutingDecisionID) != 0 {
		t.Fatalf("nil-error zero outcome fallback result=%#v count=%d, want none", result.Fallback, countFallbackDecisions(t, ctx, store, result.RoutingDecision.RoutingDecisionID))
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("nil-error zero outcome reservation state = %q, want released", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseCompleted || state.registrationState != storage.AgentStateNeedsHuman || state.attemptState != "needs-human" || state.taskState != "needs-human" {
		t.Fatalf("nil-error zero outcome durable state = %#v, want needs-human", state)
	}

	replayed, replayErr := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "duplicate"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(replayErr, taskrequirements.ErrReplanRequired) {
		t.Fatalf("nil-error zero outcome replay error = %v, want ErrReplanRequired", replayErr)
	}
	if executions.Load() != 1 || !replayed.Blocked || replayed.Successor.LaunchExposed || countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 1 {
		t.Fatalf("nil-error zero outcome replay result=%#v executions=%d attempts=%d, want blocked observe-only replay", replayed, executions.Load(), countSuccessorAttempts(t, ctx, store, handoff.TaskID))
	}
	if replayed.Reservation.BudgetReservationID != "" {
		if state := reservationState(t, ctx, store, replayed.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
			t.Fatalf("nil-error zero outcome replay reservation state = %q, want released", state)
		}
	}
}

func TestResumeApprovedHandoffNilErrorExplicitAmbiguousNeedsHumanWithoutFallback(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionAmbiguous}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("nil-error ambiguous outcome error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("nil-error ambiguous outcome executions = %d, want 1", executions.Load())
	}
	if result.Fallback.FallbackDecisionID != "" || countFallbackDecisions(t, ctx, store, result.RoutingDecision.RoutingDecisionID) != 0 {
		t.Fatalf("nil-error ambiguous outcome fallback result=%#v count=%d, want none", result.Fallback, countFallbackDecisions(t, ctx, store, result.RoutingDecision.RoutingDecisionID))
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("nil-error ambiguous outcome reservation state = %q, want released", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseCompleted || state.registrationState != storage.AgentStateNeedsHuman || state.attemptState != "needs-human" || state.taskState != "needs-human" {
		t.Fatalf("nil-error ambiguous outcome durable state = %#v, want needs-human", state)
	}
}

func TestResumeApprovedHandoffNotStartedReleasesDestinationOwnership(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionNotStarted}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("not-started outcome error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("not-started executions = %d, want 1", executions.Load())
	}
	if result.Fallback.FallbackDecisionID == "" || countFallbackDecisions(t, ctx, store, result.RoutingDecision.RoutingDecisionID) != 1 {
		t.Fatalf("not-started fallback result=%#v count=%d, want bounded fallback", result.Fallback, countFallbackDecisions(t, ctx, store, result.RoutingDecision.RoutingDecisionID))
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("not-started reservation state = %q, want released", state)
	}
	assertNoDestinationHeldLocks(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration)
	assertDestinationLockState(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration, storage.OwnershipStateReleased)
}

func TestResumeApprovedHandoffAmbiguousFencesDestinationOwnershipNeedsHuman(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionAmbiguous}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("ambiguous outcome error = %v, want ErrReplanRequired", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("ambiguous executions = %d, want 1", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("ambiguous reservation state = %q, want released", state)
	}
	assertNoDestinationHeldLocks(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration)
	assertDestinationLockState(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration, storage.OwnershipStateNeedsHuman)

	replayed, replayErr := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "duplicate"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(replayErr, taskrequirements.ErrReplanRequired) {
		t.Fatalf("ambiguous replay error = %v, want ErrReplanRequired", replayErr)
	}
	if executions.Load() != 1 || !replayed.Blocked || replayed.Successor.LaunchExposed {
		t.Fatalf("ambiguous replay result=%#v executions=%d, want blocked observe-only replay", replayed, executions.Load())
	}
}

func TestResumeApprovedHandoffTerminalCleanupStaleOwnershipRollsBack(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	cancelled := false
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterRegistration: func() error {
			cancelled = true
			return store.WithWriteTx(ctx, func(tx storage.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE agent_ownership_locks SET claim_generation = claim_generation + 10 WHERE run_id = ? AND claim_generation = ?`, handoff.ChildRunID, handoff.HandoffGeneration)
				return err
			})
		},
		IsCancelled: func(context.Context) bool { return cancelled },
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, storage.ErrOwnershipStale) {
		t.Fatalf("stale ownership cleanup error = %v, want ErrOwnershipStale", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("stale ownership cleanup executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("stale ownership cleanup reservation state = %q, want active rollback", state)
	}
	if count := destinationHeldLockCount(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration); count != 0 {
		t.Fatalf("stale ownership cleanup held locks at original generation = %d, want 0", count)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseLaunching || state.registrationState != storage.AgentStateLaunching || state.attemptState != "claimed" || state.taskState != "claimed" {
		t.Fatalf("stale ownership cleanup durable state = %#v, want rollback to launching/claimed", state)
	}
}

func TestResumeApprovedHandoffTerminalCleanupStaleSameClaimRenewalRollsBack(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	stopErr := errors.New("stop after stale renewal")
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterPrepare: func() error {
			return errors.Join(storage.RenewChildRunClaim(ctx, store, handoff.ChildRunID, handoff.DestinationExecutorID, handoff.HandoffGeneration, fixture.now.Add(2*time.Minute), fixture.now.Add(time.Hour)), stopErr)
		},
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, storage.ErrOwnershipStale) || !errors.Is(err, stopErr) {
		t.Fatalf("stale same-claim cleanup error = %v, want ErrOwnershipStale and stopErr", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("stale same-claim cleanup executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("stale same-claim cleanup reservation state = %q, want active rollback", state)
	}
	for id, generation := range destinationLockGenerations(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration) {
		if generation != result.Successor.OwnershipLocks[0].LockGeneration+1 {
			t.Fatalf("renewed lock %s generation = %d, want launch generation + 1", id, generation)
		}
	}
	assertDestinationLockState(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration, storage.OwnershipStateHeld)
	assertHandoffLaunchUnterminalized(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID, storage.AgentStateRegistered)
}

func TestResumeApprovedHandoffTerminalCleanupWithRenewedAuthoritySucceeds(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	stopErr := errors.New("stop after renewal")
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterPrepare: func() error {
			return errors.Join(storage.RenewChildRunClaim(ctx, store, handoff.ChildRunID, handoff.DestinationExecutorID, handoff.HandoffGeneration, fixture.now.Add(2*time.Minute), fixture.now.Add(time.Hour)), stopErr)
		},
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, storage.ErrOwnershipStale) {
		t.Fatalf("old cleanup error = %v, want stale authority", err)
	}
	refreshed := result.Successor
	refreshed.OwnershipLocks = handoffLaunchDecisionOwnershipSnapshot(t, ctx, store, handoff)
	if len(refreshed.OwnershipLocks) != len(result.Successor.OwnershipLocks) {
		t.Fatalf("refreshed ownership snapshot = %#v, want same lock count as launch %#v", refreshed.OwnershipLocks, result.Successor.OwnershipLocks)
	}
	if err := terminalizeHandoffLaunch(ctx, store, handoff, refreshed, result.Reservation, "cancelled", taskrequirements.ErrReplanRequiredCode, "renewed-authority-cleanup", handoffTerminalOwnershipRelease, routerActor(), routingHost()); err != nil {
		t.Fatalf("terminal cleanup with renewed authority: %v", err)
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateReleased) {
		t.Fatalf("renewed authority cleanup reservation state = %q, want released", state)
	}
	assertDestinationLockState(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration, storage.OwnershipStateReleased)
}

func TestResumeApprovedHandoffTerminalCleanupMixedLockGenerationRollsBack(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var changedID string
	var executions atomic.Int64
	stopErr := errors.New("stop after mixed generation")
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterPrepare: func() error {
			changedID = resultLockID(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration, 0)
			mutateErr := store.WithWriteTx(ctx, func(tx storage.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE agent_ownership_locks SET lock_generation = lock_generation + 1 WHERE id = ?`, changedID)
				return err
			})
			return errors.Join(mutateErr, stopErr)
		},
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, storage.ErrOwnershipStale) || !errors.Is(err, stopErr) {
		t.Fatalf("mixed generation cleanup error = %v, want ErrOwnershipStale and stopErr", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("mixed generation cleanup executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("mixed generation cleanup reservation state = %q, want active rollback", state)
	}
	generations := destinationLockGenerations(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration)
	for _, lock := range result.Successor.OwnershipLocks {
		want := lock.LockGeneration
		if lock.LockID == changedID {
			want++
		}
		if generations[lock.LockID] != want {
			t.Fatalf("lock %s generation = %d, want %d", lock.LockID, generations[lock.LockID], want)
		}
	}
	assertDestinationLockState(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration, storage.OwnershipStateHeld)
	assertHandoffLaunchUnterminalized(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID, storage.AgentStateRegistered)
}

func TestResumeApprovedHandoffTerminalCleanupExtraRegistrationLockRollsBack(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeOwnershipFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	extraID := "lock-extra"
	var executions atomic.Int64
	stopErr := errors.New("stop after extra lock")
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterPrepare: func() error {
			mutateErr := store.WithWriteTx(ctx, func(tx storage.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO agent_ownership_locks(
					id, project_id, delivery_run_id, child_agent_id, run_id, claim_generation, lock_generation,
					resource_kind, resource_key, lock_mode, state, lease_expires_at, heartbeat_at, conflicts_with_json, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, ?, 1, 'provider-receipt', 'extra-receipt', 'write', ?, ?, ?, '[]', ?, ?)`,
					extraID, handoff.ProjectID, handoff.DeliveryRunID, "agent-source", handoff.ChildRunID, handoff.HandoffGeneration,
					storage.OwnershipStateHeld, fixture.now.Add(time.Hour).Format(time.RFC3339Nano), fixture.now.Format(time.RFC3339Nano), fixture.now.Format(time.RFC3339Nano), fixture.now.Format(time.RFC3339Nano)); err != nil {
					return err
				}
				var rawIDs string
				if err := tx.QueryRow(ctx, `SELECT ownership_lock_ids_json FROM agent_registrations WHERE child_run_id = ?`, handoff.ChildRunID).Scan(&rawIDs); err != nil {
					return err
				}
				var ids []string
				if err := json.Unmarshal([]byte(rawIDs), &ids); err != nil {
					return err
				}
				ids = append(ids, extraID)
				raw, err := json.Marshal(ids)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, `UPDATE agent_registrations SET ownership_lock_ids_json = ? WHERE child_run_id = ?`, string(raw), handoff.ChildRunID)
				return err
			})
			return errors.Join(mutateErr, stopErr)
		},
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, storage.ErrOwnershipStale) || !errors.Is(err, stopErr) {
		t.Fatalf("extra lock cleanup error = %v, want ErrOwnershipStale and stopErr", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("extra lock cleanup executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("extra lock cleanup reservation state = %q, want active rollback", state)
	}
	assertDestinationLockState(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration, storage.OwnershipStateHeld)
	assertHandoffLaunchUnterminalized(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID, storage.AgentStateRegistered)
	if generations := destinationLockGenerations(t, ctx, store, handoff.ChildRunID, handoff.HandoffGeneration); generations[extraID] != 1 {
		t.Fatalf("extra lock generation/state was not preserved: %#v", generations)
	}
}

func TestResumeApprovedHandoffTerminalCleanupCommitFailureRollsBackReservationAndOwnership(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	path := tempDB(t)
	seedStore, handoff, input := handoffResumeOwnershipFixtureAtPath(t, ctx, fixture, path)
	makeClaudeEligibleOnly(input)
	if err := seedStore.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	commitErr := errors.New("terminal cleanup commit failed")
	var failCleanupCommit atomic.Bool
	store, err := storage.Open(ctx, storage.Options{
		Path: path,
		Now:  func() time.Time { return fixture.now },
		WriteTxCommitHookForTest: func(commitCtx context.Context, _ storage.Tx, commit func(context.Context) error) error {
			if failCleanupCommit.Load() {
				return commitErr
			}
			return commit(commitCtx)
		},
	})
	if err != nil {
		t.Fatalf("Open storage with commit hook: %v", err)
	}
	defer store.Close()
	cancelled := false
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		AfterRegistration: func() error {
			failCleanupCommit.Store(true)
			cancelled = true
			return nil
		},
		IsCancelled: func(context.Context) bool { return cancelled },
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "unexpected"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, commitErr) || !errors.Is(err, taskrequirements.ErrReplanRequired) {
		t.Fatalf("cleanup commit error = %v, want commitErr and ErrReplanRequired", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("cleanup commit failure executed provider %d times", executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("cleanup commit failure reservation state = %q, want active rollback", state)
	}
	if count := destinationHeldLockCount(t, ctx, store, handoff.ChildRunID, result.Successor.ClaimGeneration); count == 0 {
		t.Fatalf("cleanup commit failure held locks = %d, want rollback to held launch authority", count)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseLaunching || state.registrationState != storage.AgentStateLaunching || state.attemptState != "claimed" || state.taskState != "claimed" {
		t.Fatalf("cleanup commit failure durable state = %#v, want rollback to launching/claimed", state)
	}
}

func TestResumeApprovedHandoffStartedOutcomeWithoutReceiptMarksExecuting(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionStarted}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if err != nil {
		t.Fatalf("started without receipt error = %v", err)
	}
	if executions.Load() != 1 || result.Successor.ProviderReceipt != "" || result.Successor.LaunchPhase != storage.ClaimPhaseExecuting || result.Fallback.FallbackDecisionID != "" {
		t.Fatalf("started without receipt result=%#v executions=%d, want executing without fallback", result, executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("started without receipt reservation state = %q, want active", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseExecuting || state.registrationState != storage.AgentStateRunning || state.attemptState != "claimed" || state.taskState != "claimed" {
		t.Fatalf("started without receipt durable state = %#v, want executing/running launch", state)
	}
}

func TestResumeApprovedHandoffStartedOutcomeErrorPersistsExecutingAndPropagates(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	launchErr := errors.New("provider accepted launch but stream failed")
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionStarted}, launchErr
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, launchErr) || !strings.Contains(err.Error(), "launch started") {
		t.Fatalf("started with executor error = %v, want launchErr with started signal", err)
	}
	if executions.Load() != 1 || result.Successor.ProviderReceipt != "" || result.Successor.LaunchPhase != storage.ClaimPhaseExecuting || result.Fallback.FallbackDecisionID != "" {
		t.Fatalf("started with executor error result=%#v executions=%d, want executing without fallback", result, executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("started with executor error reservation state = %q, want active", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseExecuting || state.claimReceipt != "" || state.registrationState != storage.AgentStateRunning || state.attemptState != "claimed" || state.taskState != "claimed" {
		t.Fatalf("started with executor error durable state = %#v, want executing/running launch", state)
	}

	replayed, replayErr := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "duplicate"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if replayErr != nil {
		t.Fatalf("started with executor error replay error = %v", replayErr)
	}
	if executions.Load() != 1 || replayed.Successor.LaunchExposed || countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 1 {
		t.Fatalf("started with executor error replay result=%#v executions=%d attempts=%d, want observe-only replay", replayed, executions.Load(), countSuccessorAttempts(t, ctx, store, handoff.TaskID))
	}
}

func TestResumeApprovedHandoffReceiptOverridesAmbiguousOutcome(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	var executions atomic.Int64
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "receipt-authoritative", Outcome: HandoffSuccessorExecutionAmbiguous}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if err != nil {
		t.Fatalf("receipt precedence error = %v", err)
	}
	if executions.Load() != 1 || result.Successor.ProviderReceipt != "receipt-authoritative" || result.Successor.LaunchPhase != storage.ClaimPhaseExecuting || result.Fallback.FallbackDecisionID != "" {
		t.Fatalf("receipt precedence result=%#v executions=%d, want executing with receipt and no fallback", result, executions.Load())
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("receipt precedence reservation state = %q, want active", state)
	}
	state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseExecuting || state.registrationState != storage.AgentStateRunning || state.attemptState != "claimed" || state.taskState != "claimed" {
		t.Fatalf("receipt precedence durable state = %#v, want executing/running launch", state)
	}

	replayed, replayErr := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "duplicate"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if replayErr != nil {
		t.Fatalf("receipt precedence replay error = %v", replayErr)
	}
	if executions.Load() != 1 || replayed.Successor.LaunchExposed {
		t.Fatalf("receipt precedence replay result=%#v executions=%d, want observe-only replay", replayed, executions.Load())
	}
}

func TestResumeApprovedHandoffReceiptErrorOverridesAmbiguousAndZeroOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome HandoffSuccessorExecutionOutcome
	}{
		{name: "ambiguous", outcome: HandoffSuccessorExecutionAmbiguous},
		{name: "zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newFixture(t)
			store, handoff, input := handoffResumeFixture(t, ctx, fixture)
			defer store.Close()
			makeClaudeEligibleOnly(input)
			var executions atomic.Int64
			receipt := "receipt-" + tc.name
			launchErr := errors.New("provider returned receipt and warning")
			result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
				HandoffID:        handoff.HandoffID,
				DecisionInput:    input,
				ReservationValue: 1,
				ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
					executions.Add(1)
					return HandoffSuccessorExecutionResult{ProviderReceipt: receipt, Outcome: tc.outcome}, launchErr
				},
				DecidedBy: routerActor(),
				Host:      routingHost(),
			})
			if !errors.Is(err, launchErr) || !strings.Contains(err.Error(), "launch started") {
				t.Fatalf("receipt+%s executor error = %v, want launchErr with started signal", tc.name, err)
			}
			if executions.Load() != 1 || result.Successor.ProviderReceipt != receipt || result.Successor.LaunchPhase != storage.ClaimPhaseExecuting || result.Fallback.FallbackDecisionID != "" {
				t.Fatalf("receipt+%s result=%#v executions=%d, want executing with receipt and no fallback", tc.name, result, executions.Load())
			}
			if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
				t.Fatalf("receipt+%s reservation state = %q, want active", tc.name, state)
			}
			state := handoffLaunchDurableState(t, ctx, store, handoff.ChildRunID, result.Successor.AttemptID)
			if state.claimPhase != storage.ClaimPhaseExecuting || state.claimReceipt != receipt || state.registrationState != storage.AgentStateRunning || state.attemptState != "claimed" || state.taskState != "claimed" {
				t.Fatalf("receipt+%s durable state = %#v, want executing/running launch with receipt", tc.name, state)
			}

			replayed, replayErr := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
				HandoffID:        handoff.HandoffID,
				DecisionInput:    input,
				ReservationValue: 1,
				ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
					executions.Add(1)
					return HandoffSuccessorExecutionResult{ProviderReceipt: "duplicate"}, nil
				},
				DecidedBy: routerActor(),
				Host:      routingHost(),
			})
			if replayErr != nil {
				t.Fatalf("receipt+%s replay error = %v", tc.name, replayErr)
			}
			if executions.Load() != 1 || replayed.Successor.LaunchExposed || countSuccessorAttempts(t, ctx, store, handoff.TaskID) != 1 {
				t.Fatalf("receipt+%s replay result=%#v executions=%d attempts=%d, want observe-only replay", tc.name, replayed, executions.Load(), countSuccessorAttempts(t, ctx, store, handoff.TaskID))
			}
		})
	}
}

func TestResumeApprovedHandoffStartedOutcomePersistenceFailureJoinsExecutorError(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	path := tempDB(t)
	seedStore, handoff, input := handoffResumeFixtureAtPath(t, ctx, fixture, path)
	makeClaudeEligibleOnly(input)
	if err := seedStore.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	persistErr := errors.New("persist executing state failed")
	var failExecutingPersist atomic.Bool
	store, err := storage.Open(ctx, storage.Options{
		Path: path,
		Now:  func() time.Time { return fixture.now },
		WriteTxCommitHookForTest: func(commitCtx context.Context, _ storage.Tx, commit func(context.Context) error) error {
			if failExecutingPersist.Load() {
				return persistErr
			}
			return commit(commitCtx)
		},
	})
	if err != nil {
		t.Fatalf("Open storage with commit hook: %v", err)
	}
	var executions atomic.Int64
	launchErr := errors.New("provider launch returned after start")
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			failExecutingPersist.Store(true)
			return HandoffSuccessorExecutionResult{Outcome: HandoffSuccessorExecutionStarted}, launchErr
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if !errors.Is(err, launchErr) || !errors.Is(err, persistErr) {
		t.Fatalf("started persistence failure error = %v, want launchErr joined with persistErr", err)
	}
	if result.Fallback.FallbackDecisionID != "" {
		t.Fatalf("started persistence failure fallback = %#v, want none", result.Fallback)
	}
	if result.Reservation.BudgetReservationID == "" {
		t.Fatalf("started persistence failure did not reserve destination budget")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close failing store: %v", err)
	}
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer reopened.Close()
	if state := reservationState(t, ctx, reopened, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("started persistence failure reservation state = %q, want active", state)
	}
	state := handoffLaunchDurableState(t, ctx, reopened, handoff.ChildRunID, result.Successor.AttemptID)
	if state.claimPhase != storage.ClaimPhaseLaunching || state.claimReceipt != "" || state.registrationState != storage.AgentStateLaunching || state.attemptState != "claimed" || state.taskState != "claimed" {
		t.Fatalf("started persistence failure durable state = %#v, want prior launching state", state)
	}

	replayed, replayErr := ResumeApprovedHandoff(ctx, reopened, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
			executions.Add(1)
			return HandoffSuccessorExecutionResult{ProviderReceipt: "duplicate"}, nil
		},
		DecidedBy: routerActor(),
		Host:      routingHost(),
	})
	if replayErr != nil {
		t.Fatalf("started persistence failure replay error = %v", replayErr)
	}
	if executions.Load() != 1 || replayed.Successor.LaunchExposed || countSuccessorAttempts(t, ctx, reopened, handoff.TaskID) != 1 {
		t.Fatalf("started persistence failure replay result=%#v executions=%d attempts=%d, want fail-closed observe-only replay", replayed, executions.Load(), countSuccessorAttempts(t, ctx, reopened, handoff.TaskID))
	}
}

func TestResumeApprovedHandoffReleasesSourceReservationAfterDestinationBinding(t *testing.T) {
	ctx := context.Background()
	fixture := newFixture(t)
	store, handoff, input := handoffResumeFixture(t, ctx, fixture)
	defer store.Close()
	makeClaudeEligibleOnly(input)
	sourceReservationID := sourceReservationID(t, ctx, store, handoff.ChildRunID)
	result, err := ResumeApprovedHandoff(ctx, store, HandoffResumeInput{
		HandoffID:        handoff.HandoffID,
		DecisionInput:    input,
		ReservationValue: 1,
		ExecuteSuccessor: executeStarted("receipt-source-release"),
		DecidedBy:        routerActor(),
		Host:             routingHost(),
	})
	if err != nil {
		t.Fatalf("ResumeApprovedHandoff: %v", err)
	}
	if result.Reservation.BudgetReservationID == "" || result.Reservation.BudgetReservationID == sourceReservationID {
		t.Fatalf("destination reservation = %q source = %q", result.Reservation.BudgetReservationID, sourceReservationID)
	}
	if state := reservationState(t, ctx, store, sourceReservationID); state != string(budget.StateReleased) {
		t.Fatalf("source reservation state = %q, want released after destination bind", state)
	}
	if state := reservationState(t, ctx, store, result.Reservation.BudgetReservationID); state != string(budget.StateActive) {
		t.Fatalf("destination reservation state = %q, want active", state)
	}
}

func makeClaudeEligibleOnly(input DecisionInput) {
	for i := range input.Inputs.Inventory.QuotaSnapshots {
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "codex" {
			remaining := int64(0)
			input.Inputs.Inventory.QuotaSnapshots[i].RemainingValue = &remaining
		}
		if input.Inputs.Inventory.QuotaSnapshots[i].AdapterID == "claude" {
			input.Inputs.Inventory.QuotaSnapshots[i].Confidence = providerinventory.ConfidenceExact
		}
	}
}

func executeStarted(receipt string) HandoffSuccessorExecutor {
	return func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error) {
		return HandoffSuccessorExecutionResult{ProviderReceipt: receipt, Outcome: HandoffSuccessorExecutionStarted}, nil
	}
}

func handoffResumeFixture(t *testing.T, ctx context.Context, fixture hardFixture) (storage.Store, storage.HandoffTransaction, DecisionInput) {
	t.Helper()
	return handoffResumeFixtureAtPath(t, ctx, fixture, tempDB(t))
}

func handoffResumeFixtureAtPath(t *testing.T, ctx context.Context, fixture hardFixture, path string) (storage.Store, storage.HandoffTransaction, DecisionInput) {
	t.Helper()
	return handoffResumeFixtureAtPathWithOwnership(t, ctx, fixture, path, false)
}

func handoffResumeOwnershipFixture(t *testing.T, ctx context.Context, fixture hardFixture) (storage.Store, storage.HandoffTransaction, DecisionInput) {
	t.Helper()
	return handoffResumeOwnershipFixtureAtPath(t, ctx, fixture, tempDB(t))
}

func handoffResumeOwnershipFixtureAtPath(t *testing.T, ctx context.Context, fixture hardFixture, path string) (storage.Store, storage.HandoffTransaction, DecisionInput) {
	t.Helper()
	return handoffResumeFixtureAtPathWithOwnership(t, ctx, fixture, path, true)
}

func handoffResumeFixtureAtPathWithOwnership(t *testing.T, ctx context.Context, fixture hardFixture, path string, withOwnership bool) (storage.Store, storage.HandoffTransaction, DecisionInput) {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixture.now }})
	if err != nil {
		t.Fatalf("Open storage: %v", err)
	}
	seedRoutingDecisionStore(t, ctx, store, fixture.now)
	input := replayDecisionInput(fixture)
	input.Inputs.Requirement.PermissionRequired = taskrequirements.PermissionReadOnly
	input.Inputs.Requirement.SideEffectClass = taskrequirements.SideEffectLocalRead
	input.Inputs.Requirement.RoleKey = RoleKeyLuna
	input.Inputs.Requirement.ScopeJSON = `{"paths":["README.md"],"repo":"proj-routing"}`
	if withOwnership {
		input.Inputs.Requirement.PermissionRequired = taskrequirements.PermissionWrite
		input.Inputs.Requirement.SideEffectClass = taskrequirements.SideEffectLocalWrite
	}
	for i := range input.Inputs.Candidates {
		input.Inputs.Candidates[i].Permission = taskrequirements.PermissionReadOnly
		input.Inputs.Candidates[i].LaunchSideEffectClass = taskrequirements.SideEffectLocalRead
		input.Inputs.Candidates[i].RoleKey = RoleKeyLuna
		input.Inputs.Candidates[i].RoutingCandidateID = ""
		input.Inputs.Candidates[i].CandidateFingerprint = ""
		if withOwnership {
			input.Inputs.Candidates[i].Permission = taskrequirements.PermissionWrite
			input.Inputs.Candidates[i].LaunchSideEffectClass = taskrequirements.SideEffectLocalWrite
		}
	}
	input.Inputs.Requirement = persistFallbackTaskRequirement(t, ctx, store, input.Inputs.Requirement, fixture.now)
	input.TaskRequirementID = input.Inputs.Requirement.TaskRequirementID
	profile := BalancedRoutingPolicyProfile(fixture.now)
	input.RoutingPolicyProfile = profile
	input.RoutingPolicyProfileID = profile.RoutingPolicyProfileID
	input.PolicyFingerprint = profile.PolicyFingerprint
	input.Inputs.Policy = profile.EligibilityPolicy
	if _, err := PersistRoutingPolicyProfile(ctx, store, profile); err != nil {
		t.Fatalf("PersistRoutingPolicyProfile: %v", err)
	}
	seedHandoffRuntime(t, ctx, store, fixture, input)
	claim, err := storage.ClaimChildRunExecution(ctx, store, "run-routing", "run-routing-child", "executor-source", fixture.now, fixture.now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ClaimChildRunExecution: %v", err)
	}
	seedHandoffAgentAuthority(t, ctx, store, fixture, claim, input, withOwnership)
	handoff, err := storage.RecordHandoffTransaction(ctx, store, storage.HandoffRequest{
		IdempotencyKey:            "handoff-quota-a",
		ProjectID:                 "proj-routing",
		DeliveryRunID:             "drun-routing",
		TaskID:                    "task-a",
		ParentRunID:               "run-routing",
		ChildRunID:                "run-routing-child",
		SourceAttemptID:           "attempt-source",
		SourceExecutorID:          claim.ExecutorID,
		SourceClaimGeneration:     claim.ClaimGeneration,
		TriggerKind:               storage.HandoffTriggerQuotaAvailability,
		ReasonCodes:               []string{"quota-exhausted"},
		EvidenceRecordIDs:         []string{"qsnap-codex-a-good"},
		TriggerSnapshotJSON:       `{"reason_codes":["quota-exhausted"],"remaining":0}`,
		SideEffectState:           storage.SideEffectStateReadOnly,
		DestinationExecutorID:     "handoff-destination",
		RequestedAt:               fixture.now.Add(time.Minute).Format(time.RFC3339Nano),
		DestinationLeaseExpiresAt: fixture.now.Add(time.Hour).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("RecordHandoffTransaction: %v", err)
	}
	return store, handoff, input
}

func seedHandoffRuntime(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture, input DecisionInput) {
	t.Helper()
	at := delivery.CanonicalTimestamp(fixture.now)
	permission := input.Inputs.Requirement.PermissionRequired
	if permission == "" {
		permission = taskrequirements.PermissionReadOnly
	}
	sideEffect := input.Inputs.Requirement.SideEffectClass
	if sideEffect == "" {
		sideEffect = taskrequirements.SideEffectLocalRead
	}
	scopeJSON := strings.TrimSpace(input.Inputs.Requirement.ScopeJSON)
	if scopeJSON == "" {
		scopeJSON = `{"paths":["README.md"],"repo":"proj-routing"}`
	}
	if err := storage.PersistChildPlanGraph(ctx, store,
		storage.RunNode{RunID: "run-routing", ProjectID: "proj-routing", RootRunID: "run-routing", Depth: 0, Origin: "test", Status: "running", CreatedAt: at, UpdatedAt: at},
		[]storage.RunNode{{RunID: "run-routing-child", ProjectID: "proj-routing", ParentRunID: "run-routing", RootRunID: "run-routing", Depth: 1, Origin: "handoff", Status: "planned", CreatedAt: at, UpdatedAt: at}},
		storage.ChildPlanRecord{PlanID: "plan-child", ParentRunID: "run-routing", RootRunID: "run-routing", SchemaVersion: "loopcoder.child_plan.v1", MaxDepth: 2, MaxConcurrency: 1, PlanJSON: "{}", CreatedAt: at},
		[]storage.RunEdgeRecord{{ParentRunID: "run-routing", ChildRunID: "run-routing-child", RootRunID: "run-routing", PlanID: "plan-child", ChildKey: "worker", Depth: 1, Ordinal: 0, ScopeJSON: scopeJSON, Permission: string(permission), AggregationJSON: "{}", Status: "planned", CreatedAt: at, UpdatedAt: at}},
	); err != nil {
		t.Fatalf("PersistChildPlanGraph: %v", err)
	}
	seedProviderInventoryRows(t, ctx, store, fixture)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delivery_attempts(
			attempt_id, schema_version, record_version, project_id, delivery_run_id, task_id, attempt_ordinal, state,
			claim_generation, executor_id, provider_idempotency_key, side_effect_class, started_at, created_at, updated_at,
			created_by_json, updated_by_json, host_json)
			VALUES ('attempt-source', 'loopcoder.attempt.v1', 1, 'proj-routing', 'drun-routing', 'task-a', 1, 'running',
			1, 'executor-source', 'provider-source-key', ?, ?, ?, ?, '{}', '{}', '{}')`,
			sideEffect, at, at, at)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE delivery_tasks SET state = 'running', permission = ?, side_effect_class = ?, scope_json = ?, active_attempt_id = 'attempt-source', attempt_count = 1 WHERE task_id = 'task-a'`,
			permission, sideEffect, scopeJSON)
		return err
	}); err != nil {
		t.Fatalf("seed source attempt: %v", err)
	}
	for _, c := range input.Inputs.Candidates {
		scope := budget.Scope{ScopeKind: budget.ScopeSubAgent, ProjectID: "proj-routing", DeliveryRunID: "drun-routing", TaskID: "task-a", SubAgentID: "agent-source", AdapterID: c.AdapterID, AccountProfileID: c.AccountProfileID, ModelCapabilityID: c.ModelCapabilityID}
		if _, err := budget.UpsertPolicy(ctx, store, budget.PolicyInput{
			Scope:         scope,
			QuantityKind:  providerinventory.QuantityLocalPolicy,
			WindowKind:    providerinventory.WindowUnbounded,
			PolicyMode:    budget.PolicyHard,
			CeilingValue:  100,
			PolicyVersion: "handoff-test",
			Ordinal:       c.AdapterID,
			Actor:         budget.Actor{ActorID: "test"},
			Host:          budget.Host{HostID: "test"},
		}); err != nil {
			t.Fatalf("upsert destination budget policy: %v", err)
		}
	}
}

func seedProviderInventoryRows(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture) {
	t.Helper()
	at := delivery.CanonicalTimestamp(fixture.now)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		for _, adapter := range []string{"codex", "claude"} {
			if _, err := tx.Exec(ctx, `INSERT INTO adapter_declarations(
				adapter_declaration_id, schema_version, record_version, adapter_id, adapter_version, display_name,
				executable_names_json, created_at, updated_at, payload_json)
				VALUES (?, 'loopcoder.adapter_declaration.v1', 1, ?, 'test', ?, '[]', ?, ?, '{}')
				ON CONFLICT(adapter_declaration_id) DO NOTHING`,
				"adecl-"+adapter, adapter, adapter, at, at); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO provider_installations(
				provider_installation_id, schema_version, record_version, scope, project_id, adapter_id, adapter_declaration_id,
				provider_display_name, executable_name, executable_identity_json, canonical_path_redacted, discovery_source,
				discovery_order, platform, version_confidence, installation_state, usable_for_invocation, created_at,
				updated_at, captured_at, stale_after, freshness_state, confidence, side_effect_class, classification, payload_json)
				VALUES (?, 'loopcoder.provider_installation.v1', 1, 'project', 'proj-routing', ?, ?, ?, ?, '{}', ?, 'test',
				1, 'darwin/arm64', 'exact', 'active', 'yes', ?, ?, ?, ?, 'fresh', 'exact', 'local-read', 'local-diagnostic', '{}')
				ON CONFLICT(provider_installation_id) DO NOTHING`,
				"pinst-"+adapter, adapter, "adecl-"+adapter, adapter, adapter, adapter, at, at, at, fixture.now.Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
				return err
			}
			accountID := "acct-a"
			modelID := "codex-good"
			if adapter == "claude" {
				accountID = "acct-c"
				modelID = "claude-good"
			}
			if _, err := tx.Exec(ctx, `INSERT INTO account_profiles(account_profile_id, adapter_id, provider_installation_id, payload_json)
				VALUES (?, ?, ?, '{}') ON CONFLICT(account_profile_id) DO NOTHING`, accountID, adapter, "pinst-"+adapter); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO model_catalog_snapshots(model_catalog_snapshot_id, adapter_id, provider_installation_id, payload_json)
				VALUES (?, ?, ?, '{}') ON CONFLICT(model_catalog_snapshot_id) DO NOTHING`, "mcats-"+adapter, adapter, "pinst-"+adapter); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO model_capabilities(model_capability_id, model_catalog_snapshot_id, adapter_id, payload_json)
				VALUES (?, ?, ?, '{}') ON CONFLICT(model_capability_id) DO NOTHING`, modelID, "mcats-"+adapter, adapter); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed provider inventory rows: %v", err)
	}
}

func seedHandoffAgentAuthority(t *testing.T, ctx context.Context, store storage.Store, fixture hardFixture, claim storage.ClaimResult, input DecisionInput, withOwnership bool) {
	t.Helper()
	sourceScope := budget.Scope{ScopeKind: budget.ScopeSubAgent, ProjectID: "proj-routing", DeliveryRunID: "drun-routing", TaskID: "task-a", SubAgentID: "agent-source", AdapterID: "codex", AccountProfileID: "acct-a", ModelCapabilityID: "codex-good"}
	sourcePolicy, err := budget.UpsertPolicy(ctx, store, budget.PolicyInput{
		Scope: sourceScope, QuantityKind: providerinventory.QuantityLocalPolicy, WindowKind: providerinventory.WindowUnbounded,
		PolicyMode: budget.PolicyHard, CeilingValue: 10, PolicyVersion: "handoff-source-test", Ordinal: "source",
		Actor: budget.Actor{ActorID: "test"}, Host: budget.Host{HostID: "test"},
	})
	if err != nil {
		t.Fatalf("upsert source policy: %v", err)
	}
	sourceReservation, err := budget.Reserve(ctx, store, budget.ReserveRequest{
		ScopeChain: []budget.Scope{sourceScope}, QuantityKind: providerinventory.QuantityLocalPolicy, WindowKind: providerinventory.WindowUnbounded,
		RequestedValue: 1, LeaseExpiresAt: fixture.now.Add(time.Hour), IdempotencyKey: "source-reservation", RequesterID: "agent-source",
		AuthorizationFingerprint: testFingerprint("auth"), RequirementConfidence: providerinventory.ConfidenceExact,
		Actor: budget.Actor{ActorID: "test"}, Host: budget.Host{HostID: "test"},
	})
	if err != nil {
		t.Fatalf("reserve source policy: %v", err)
	}
	at := delivery.CanonicalTimestamp(fixture.now)
	permission := input.Inputs.Requirement.PermissionRequired
	if permission == "" {
		permission = taskrequirements.PermissionReadOnly
	}
	sideEffect := input.Inputs.Requirement.SideEffectClass
	if sideEffect == "" {
		sideEffect = taskrequirements.SideEffectLocalRead
	}
	scopeJSON := strings.TrimSpace(input.Inputs.Requirement.ScopeJSON)
	if scopeJSON == "" {
		scopeJSON = `{"paths":["README.md"],"repo":"proj-routing"}`
	}
	ownershipLockIDsJSON := `[]`
	if withOwnership {
		ownershipLockIDsJSON = `["lock-source"]`
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_scope_grants(
			id, project_id, delivery_run_id, child_agent_id, schema_version, record_version, scope_json, permission,
			side_effect_class, policy_version, policy_fingerprint, plan_fingerprint, authorization_fingerprint,
			agent_federation_fingerprint, created_at, updated_at)
			VALUES ('scope-source', 'proj-routing', 'drun-routing', 'agent-source', 'loopcoder.agent_scope_grant.v1', 1, ?, ?, ?, 'test', ?, ?, ?, 'seed-fp', ?, ?)`,
			scopeJSON,
			permission, sideEffect, testFingerprint("delivery-policy"), testFingerprint("plan-routing"), testFingerprint("auth"), at, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_budget_bindings(
			id, project_id, delivery_run_id, child_agent_id, budget_policy_id, budget_reservation_id,
			reservation_scope, reserved_quantities_json, ancestor_budget_refs_json, reservation_state, created_at, updated_at)
			VALUES ('binding-source', 'proj-routing', 'drun-routing', 'agent-source', ?, ?, 'sub-agent', '{}', '[]', 'active', ?, ?)`,
			sourcePolicy.BudgetPolicyID, sourceReservation.Reservation.BudgetReservationID, at, at); err != nil {
			return err
		}
		if withOwnership {
			if _, err := tx.Exec(ctx, `INSERT INTO agent_ownership_locks(
				id, project_id, delivery_run_id, child_agent_id, run_id, claim_generation, lock_generation,
				resource_kind, resource_key, lock_mode, state, lease_expires_at, heartbeat_at, conflicts_with_json, created_at, updated_at)
				VALUES ('lock-source', 'proj-routing', 'drun-routing', 'agent-source', 'run-routing-child', ?, 1,
				'repo-path', 'README.md', 'write', ?, ?, ?, '[]', ?, ?)`,
				claim.ClaimGeneration, storage.OwnershipStateHeld, fixture.now.Add(time.Hour).Format(time.RFC3339Nano), at, at, at); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO agent_registrations(
			id, record_version, project_id, delivery_run_id, root_run_id, parent_run_id, child_run_id, task_id, attempt_id,
			plan_id, child_key, adapter_id, provider_installation_id, account_profile_id, model_capability_id,
			routing_decision_id, provider_session_ref, scope_grant_id, permission, side_effect_class, budget_binding_ids_json,
			ownership_lock_ids_json, claim_generation, executor_id, provider_idempotency_key, cancellation_channel,
			expected_outputs_json, registration_state, depth, policy_version, plan_fingerprint, policy_fingerprint,
			authorization_fingerprint, agent_federation_fingerprint, registration_payload_hash, created_at, updated_at)
			VALUES ('agent-source', 1, 'proj-routing', 'drun-routing', 'run-routing', 'run-routing', 'run-routing-child', 'task-a', 'attempt-source',
			'plan-child', 'worker', 'codex', 'pinst-codex', 'acct-a', 'codex-good', '', '', 'scope-source', ?, ?, '["binding-source"]',
			?, ?, ?, ?, 'cancel', '{}', 'registered', 1, 'test', ?, ?, ?, 'seed-fp', 'seed-payload', ?, ?)`,
			permission, sideEffect, ownershipLockIDsJSON, claim.ClaimGeneration, claim.ExecutorID, claim.ProviderKey,
			testFingerprint("plan-routing"), testFingerprint("delivery-policy"), testFingerprint("auth"), at, at)
		return err
	}); err != nil {
		t.Fatalf("seed agent authority: %v", err)
	}
}

func countSuccessorAttempts(t *testing.T, ctx context.Context, store storage.Store, taskID string) int {
	t.Helper()
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM delivery_attempts WHERE task_id = ? AND attempt_id <> 'attempt-source'`, taskID).Scan(&count)
	}); err != nil {
		t.Fatalf("count successor attempts: %v", err)
	}
	return count
}

func reservationState(t *testing.T, ctx context.Context, store storage.Store, reservationID string) string {
	t.Helper()
	var state string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM budget_reservations WHERE budget_reservation_id = ?`, reservationID).Scan(&state)
	}); err != nil {
		t.Fatalf("reservation state: %v", err)
	}
	return state
}

func handoffLaunchDecisionOwnershipSnapshot(t *testing.T, ctx context.Context, store storage.Store, handoff storage.HandoffTransaction) []storage.HandoffOwnershipLockSnapshot {
	t.Helper()
	var raw string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT output_json FROM delivery_decisions
			WHERE project_id = ? AND delivery_run_id = ? AND decision_key = ?`,
			handoff.ProjectID, handoff.DeliveryRunID, "handoff-successor:"+handoff.HandoffID).Scan(&raw)
	}); err != nil {
		t.Fatalf("handoff launch decision snapshot: %v", err)
	}
	var payload struct {
		OwnershipLocks []storage.HandoffOwnershipLockSnapshot `json:"ownership_locks"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("handoff launch decision JSON: %v", err)
	}
	return payload.OwnershipLocks
}

func destinationLockGenerations(t *testing.T, ctx context.Context, store storage.Store, childRunID string, claimGeneration int64) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, lock_generation FROM agent_ownership_locks
			WHERE run_id = ? AND claim_generation = ?
			ORDER BY id`, childRunID, claimGeneration)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var generation int64
			if err := rows.Scan(&id, &generation); err != nil {
				return err
			}
			out[id] = generation
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("destination lock generations: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("destination lock generations empty for %s generation %d", childRunID, claimGeneration)
	}
	return out
}

func resultLockID(t *testing.T, ctx context.Context, store storage.Store, childRunID string, claimGeneration int64, index int) string {
	t.Helper()
	ids := []string{}
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM agent_ownership_locks
			WHERE run_id = ? AND claim_generation = ?
			ORDER BY id`, childRunID, claimGeneration)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("destination lock id: %v", err)
	}
	if index < 0 || index >= len(ids) {
		t.Fatalf("destination lock index %d out of range for %v", index, ids)
	}
	return ids[index]
}

func assertHandoffLaunchUnterminalized(t *testing.T, ctx context.Context, store storage.Store, childRunID, attemptID, wantRegistrationState string) {
	t.Helper()
	state := handoffLaunchDurableState(t, ctx, store, childRunID, attemptID)
	if state.claimPhase != storage.ClaimPhaseLaunching || state.registrationState != wantRegistrationState || state.attemptState != "claimed" || state.taskState != "claimed" {
		t.Fatalf("handoff launch durable state = %#v, want launching/%s/claimed", state, wantRegistrationState)
	}
}

func assertNoDestinationHeldLocks(t *testing.T, ctx context.Context, store storage.Store, childRunID string, claimGeneration int64) {
	t.Helper()
	if count := destinationHeldLockCount(t, ctx, store, childRunID, claimGeneration); count != 0 {
		t.Fatalf("destination held locks for %s generation %d = %d, want 0", childRunID, claimGeneration, count)
	}
}

func destinationHeldLockCount(t *testing.T, ctx context.Context, store storage.Store, childRunID string, claimGeneration int64) int {
	t.Helper()
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM agent_ownership_locks
			WHERE run_id = ? AND claim_generation = ? AND state = ?`,
			childRunID, claimGeneration, storage.OwnershipStateHeld).Scan(&count)
	}); err != nil {
		t.Fatalf("destination held lock count: %v", err)
	}
	return count
}

func assertDestinationLockState(t *testing.T, ctx context.Context, store storage.Store, childRunID string, claimGeneration int64, want string) {
	t.Helper()
	var states []string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT state FROM agent_ownership_locks
			WHERE run_id = ? AND claim_generation = ?
			ORDER BY id`, childRunID, claimGeneration)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			if err := rows.Scan(&state); err != nil {
				return err
			}
			states = append(states, state)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("destination lock states: %v", err)
	}
	if len(states) == 0 {
		t.Fatalf("destination lock states empty for %s generation %d", childRunID, claimGeneration)
	}
	for _, state := range states {
		if state != want {
			t.Fatalf("destination lock states = %v, want all %q", states, want)
		}
	}
}

func handoffRegistrationState(t *testing.T, ctx context.Context, store storage.Store, childRunID string) struct {
	attemptID  string
	adapterID  string
	sessionRef string
	state      string
} {
	t.Helper()
	var out struct {
		attemptID  string
		adapterID  string
		sessionRef string
		state      string
	}
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT attempt_id, adapter_id, provider_session_ref, registration_state
			FROM agent_registrations WHERE child_run_id = ?`, childRunID).Scan(&out.attemptID, &out.adapterID, &out.sessionRef, &out.state)
	}); err != nil {
		t.Fatalf("handoff registration state: %v", err)
	}
	return out
}

func handoffLaunchDurableState(t *testing.T, ctx context.Context, store storage.Store, childRunID, attemptID string) struct {
	claimPhase        string
	claimReceipt      string
	registrationState string
	attemptState      string
	taskState         string
} {
	t.Helper()
	var out struct {
		claimPhase        string
		claimReceipt      string
		registrationState string
		attemptState      string
		taskState         string
	}
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT phase, provider_receipt FROM run_claims WHERE run_id = ?`, childRunID).Scan(&out.claimPhase, &out.claimReceipt); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT registration_state FROM agent_registrations WHERE child_run_id = ?`, childRunID).Scan(&out.registrationState); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state FROM delivery_attempts WHERE attempt_id = ?`, attemptID).Scan(&out.attemptState); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT state FROM delivery_tasks WHERE active_attempt_id = ?`, attemptID).Scan(&out.taskState)
	}); err != nil {
		t.Fatalf("handoff launch durable state: %v", err)
	}
	return out
}

func sourceReservationID(t *testing.T, ctx context.Context, store storage.Store, childRunID string) string {
	t.Helper()
	var id string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT br.budget_reservation_id
			FROM agent_registrations ar
			JOIN agent_budget_bindings abb ON abb.child_agent_id = ar.id
			JOIN budget_reservations br ON br.budget_reservation_id = abb.budget_reservation_id
			WHERE ar.child_run_id = ? AND br.state = ?
			ORDER BY br.created_at, br.budget_reservation_id LIMIT 1`, childRunID, string(budget.StateActive)).Scan(&id)
	}); err != nil {
		t.Fatalf("source reservation id: %v", err)
	}
	return id
}
