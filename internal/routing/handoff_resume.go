package routing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

type HandoffResumeInput struct {
	HandoffID           string
	DecisionInput       DecisionInput
	ReservationValue    int64
	LeaseExpiresAt      time.Time
	ReusableEvidenceIDs []string
	Cancelled           bool
	IsCancelled         func(context.Context) bool
	BeforeLaunch        func() error
	AfterPrepare        func() error
	AfterRegistration   func() error
	ExecuteSuccessor    HandoffSuccessorExecutor
	DecidedBy           delivery.Actor
	Host                delivery.Host
}

type HandoffSuccessorExecutor func(context.Context, HandoffSuccessorExecution) (HandoffSuccessorExecutionResult, error)

type HandoffSuccessorExecution struct {
	Launch    storage.HandoffSuccessorLaunch
	Candidate Candidate
}

type HandoffSuccessorExecutionResult struct {
	ProviderReceipt string
	Outcome         HandoffSuccessorExecutionOutcome
}

type HandoffSuccessorExecutionOutcome string

const (
	HandoffSuccessorExecutionStarted    HandoffSuccessorExecutionOutcome = "started"
	HandoffSuccessorExecutionNotStarted HandoffSuccessorExecutionOutcome = "not-started"
	HandoffSuccessorExecutionAmbiguous  HandoffSuccessorExecutionOutcome = "ambiguous"
)

type HandoffResumeResult struct {
	Handoff         storage.HandoffTransaction
	RoutingDecision RoutingDecision
	Reservation     budget.Reservation
	Successor       storage.HandoffSuccessorLaunch
	Fallback        FallbackDecision
	Blocked         bool
	Replay          bool
}

func ResumeApprovedHandoff(ctx context.Context, store storage.Store, input HandoffResumeInput) (HandoffResumeResult, error) {
	if store == nil {
		return HandoffResumeResult{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "store is required"}
	}
	handoff, err := storage.LoadHandoffTransaction(ctx, store, input.HandoffID)
	if err != nil {
		return HandoffResumeResult{}, err
	}
	if handoff.HandoffStatus != storage.HandoffStatusTransferred {
		return HandoffResumeResult{Handoff: handoff, Blocked: true}, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff is not approved for automatic successor launch"}
	}
	routeInput := input.DecisionInput
	routeInput.ProjectID = handoff.ProjectID
	routeInput.DeliveryRunID = handoff.DeliveryRunID
	routeInput.DecisionKey = "handoff-successor:" + handoff.HandoffID
	routeInput.PlanFingerprint = handoff.PlanFingerprint
	routeInput.AuthorizationFingerprint = handoff.AuthorizationFingerprint
	routeInput.DecidedBy = firstActor(input.DecidedBy, routeInput.DecidedBy)
	routeInput.Host = firstHost(input.Host, routeInput.Host)
	routeInput.Now = store.Now()
	if routeInput.Inputs.Requirement.TaskID != handoff.TaskID || routeInput.Inputs.Requirement.ProjectID != handoff.ProjectID ||
		routeInput.Inputs.Requirement.DeliveryRunID != handoff.DeliveryRunID || routeInput.Inputs.Requirement.PlanFingerprint != handoff.PlanFingerprint {
		return HandoffResumeResult{Handoff: handoff}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "handoff successor requirement does not match approved task authority"}
	}
	decision, routeErr := DecideAndPersistRoute(ctx, store, routeInput)
	result := HandoffResumeResult{Handoff: handoff, RoutingDecision: decision}
	if routeErr != nil {
		if errors.Is(routeErr, taskrequirements.ErrNoEligibleCandidate) {
			result.Blocked = true
		}
		return result, routeErr
	}
	selected := selectedCandidate(decision)
	if selected.RoutingCandidateID == "" {
		result.Blocked = true
		return result, taskrequirements.ErrNoEligibleCandidate
	}
	if input.ExecuteSuccessor == nil {
		result.Blocked = true
		return result, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "handoff successor executor is required for provider launch"}
	}
	childAgentID, err := handoffChildAgentID(ctx, store, handoff.ChildRunID)
	if err != nil {
		return result, err
	}
	reserveReq := handoffReserveRequest(handoff, childAgentID, selected, decision, input, store.Now())
	reserved, err := reserveHandoffBudget(ctx, store, reserveReq)
	if err != nil {
		result.Reservation = reserved.Reservation
		return result, err
	}
	result.Reservation = reserved.Reservation
	obsoleteReservations, err := handoffObsoleteReservations(ctx, store, handoff.ChildRunID, result.Reservation.BudgetReservationID)
	if err != nil {
		releaseErr := releaseHandoffReservationCleanup(store, result.Reservation, "inspect-obsolete-reservation-failed", input.DecidedBy, input.Host)
		return result, errors.Join(err, releaseErr)
	}
	if handoffResumeCancelled(ctx, input) {
		releaseErr := releaseHandoffReservationCleanup(store, result.Reservation, "cancelled-before-launch", input.DecidedBy, input.Host)
		return result, errors.Join(&taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff resume cancelled before launch"}, releaseErr)
	}
	if input.BeforeLaunch != nil {
		if err := input.BeforeLaunch(); err != nil {
			releaseErr := releaseHandoffReservationCleanup(store, result.Reservation, "stopped-before-launch", input.DecidedBy, input.Host)
			return result, errors.Join(err, releaseErr)
		}
	}
	actorJSON, err := delivery.CanonicalJSON(firstActor(input.DecidedBy, routeInput.DecidedBy))
	if err != nil {
		releaseErr := releaseHandoffReservationCleanup(store, result.Reservation, "launch-canonicalization-failed", input.DecidedBy, input.Host)
		return result, errors.Join(err, releaseErr)
	}
	hostJSON, err := delivery.CanonicalJSON(firstHost(input.Host, routeInput.Host))
	if err != nil {
		releaseErr := releaseHandoffReservationCleanup(store, result.Reservation, "launch-canonicalization-failed", input.DecidedBy, input.Host)
		return result, errors.Join(err, releaseErr)
	}
	launch, err := storage.PrepareHandoffSuccessorLaunch(ctx, store, storage.HandoffSuccessorRequest{
		HandoffID:           handoff.HandoffID,
		HandoffGeneration:   handoff.HandoffGeneration,
		SourceAttemptID:     handoff.SourceAttemptID,
		RoutingDecisionID:   decision.RoutingDecisionID,
		RoutingFingerprint:  decision.RoutingFingerprint,
		BudgetReservationID: result.Reservation.BudgetReservationID,
		BudgetPolicyIDs:     result.Reservation.PolicyIDs,
		ReusableEvidenceIDs: boundedEvidenceRefs(input.ReusableEvidenceIDs, handoff.EvidenceRecordIDs),
		Candidate: storage.HandoffSuccessorCandidate{
			RoutingDecisionID:      decision.RoutingDecisionID,
			RoutingCandidateID:     selected.RoutingCandidateID,
			AdapterID:              selected.AdapterID,
			ProviderInstallationID: selected.ProviderInstallationID,
			AccountProfileID:       selected.AccountProfileID,
			ModelCapabilityID:      selected.ModelCapabilityID,
		},
		CreatedAt: delivery.CanonicalTimestamp(store.Now()),
		ActorJSON: string(actorJSON),
		HostJSON:  string(hostJSON),
	})
	if err != nil {
		releaseErr := releaseHandoffReservationCleanup(store, result.Reservation, "launch-refused", input.DecidedBy, input.Host)
		return result, errors.Join(err, releaseErr)
	}
	result.Successor = launch
	result.Replay = reserved.Replay || launch.Replay
	if err := releaseHandoffReservationsCleanup(store, obsoleteReservations, "source-reservation-superseded:"+handoff.HandoffID, input.DecidedBy, input.Host); err != nil {
		return result, err
	}
	if !launch.LaunchExposed {
		if launch.LaunchPhase == storage.ClaimPhaseCompleted && strings.TrimSpace(launch.ProviderReceipt) == "" {
			result.Blocked = true
			releaseErr := error(nil)
			if result.Reservation.BudgetReservationID != "" && result.Reservation.BudgetReservationID != launch.BudgetReservationID {
				releaseErr = releaseHandoffReservationCleanup(store, result.Reservation, "terminal-replay-reservation-release", input.DecidedBy, input.Host)
			}
			return result, errors.Join(&taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff successor launch previously ended without provider receipt; replay requires human recovery"}, releaseErr)
		}
		if handoffLaunchNeedsHuman(launch, store.Now()) {
			result.Blocked = true
			return result, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff successor launch ownership is ambiguous; human recovery required before replay"}
		}
		return result, nil
	}
	if input.AfterPrepare != nil {
		if err := input.AfterPrepare(); err != nil {
			cleanupErr := cleanupHandoffLaunchTerminal(store, handoff, launch, result.Reservation, "needs-human", taskrequirements.ErrReplanRequiredCode, "stopped-after-prepare", input.DecidedBy, input.Host)
			return result, errors.Join(err, cleanupErr, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff resume stopped after successor preparation"})
		}
	}
	if handoffResumeCancelled(ctx, input) {
		cleanupErr := cleanupHandoffLaunchTerminal(store, handoff, launch, result.Reservation, "cancelled", taskrequirements.ErrReplanRequiredCode, "cancelled-at-launch-boundary", input.DecidedBy, input.Host)
		return result, errors.Join(&taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff resume cancelled at launch boundary"}, cleanupErr)
	}
	if err := transitionHandoffSuccessorLaunching(ctx, store, handoff, launch); err != nil {
		releaseErr := releaseHandoffReservationCleanup(store, result.Reservation, "launch-transition-refused", input.DecidedBy, input.Host)
		return result, errors.Join(err, releaseErr)
	}
	if input.AfterRegistration != nil {
		if err := input.AfterRegistration(); err != nil {
			cleanupErr := cleanupHandoffLaunchTerminal(store, handoff, launch, result.Reservation, "needs-human", taskrequirements.ErrReplanRequiredCode, "stopped-after-registration", input.DecidedBy, input.Host)
			return result, errors.Join(err, cleanupErr, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff resume stopped after successor registration launch"})
		}
	}
	if handoffResumeCancelled(ctx, input) {
		cleanupErr := cleanupHandoffLaunchTerminal(store, handoff, launch, result.Reservation, "cancelled", taskrequirements.ErrReplanRequiredCode, "cancelled-before-provider-invocation", input.DecidedBy, input.Host)
		return result, errors.Join(&taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff resume cancelled before provider invocation"}, cleanupErr)
	}
	executed, execErr := input.ExecuteSuccessor(ctx, HandoffSuccessorExecution{Launch: launch, Candidate: selected})
	result.Successor.ProviderReceipt = strings.TrimSpace(executed.ProviderReceipt)
	switch handoffExecutionOutcome(executed) {
	case HandoffSuccessorExecutionStarted:
		if err := markHandoffSuccessorExecuting(store, handoff, launch, result.Successor.ProviderReceipt); err != nil {
			return result, err
		}
		result.Successor.LaunchPhase = storage.ClaimPhaseExecuting
		return result, nil
	case HandoffSuccessorExecutionNotStarted:
		cleanupErr := cleanupHandoffLaunchTerminal(store, handoff, launch, result.Reservation, "needs-human", taskrequirements.ErrReplanRequiredCode, "destination-launch-not-started", input.DecidedBy, input.Host)
		fallback, fallbackErr := HandoffDestinationFailureFallback(ctx, store, result, routeInput.Inputs, FallbackTriggerWorkerFailed, firstActor(input.DecidedBy, routeInput.DecidedBy), firstHost(input.Host, routeInput.Host))
		result.Fallback = fallback
		return result, errors.Join(execErr, cleanupErr, fallbackErr)
	default:
		cleanupErr := cleanupHandoffLaunchTerminal(store, handoff, launch, result.Reservation, "needs-human", taskrequirements.ErrReplanRequiredCode, "destination-launch-ambiguous", input.DecidedBy, input.Host)
		return result, errors.Join(execErr, cleanupErr, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff successor launch outcome is ambiguous; human reconciliation required"})
	}
}

func handoffExecutionOutcome(result HandoffSuccessorExecutionResult) HandoffSuccessorExecutionOutcome {
	if strings.TrimSpace(result.ProviderReceipt) != "" {
		return HandoffSuccessorExecutionStarted
	}
	switch result.Outcome {
	case HandoffSuccessorExecutionNotStarted, HandoffSuccessorExecutionStarted, HandoffSuccessorExecutionAmbiguous:
		return result.Outcome
	default:
		return HandoffSuccessorExecutionAmbiguous
	}
}

func reserveHandoffBudget(ctx context.Context, store storage.Store, req budget.ReserveRequest) (budget.Result, error) {
	baseKey := req.IdempotencyKey
	var last budget.Result
	for i := 0; i < 8; i++ {
		if i == 0 {
			req.IdempotencyKey = baseKey
		} else {
			req.IdempotencyKey = baseKey + ":retry:" + strconv.Itoa(i)
		}
		reserved, err := budget.Reserve(ctx, store, req)
		if err != nil {
			return reserved, err
		}
		if !reserved.Replay || liveReservationState(reserved.Reservation.State) {
			return reserved, nil
		}
		last = reserved
	}
	return last, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff successor reservation retry bound exceeded"}
}

func liveReservationState(state budget.ReservationState) bool {
	return state == budget.StateActive || state == budget.StatePartiallyCommitted
}

func HandoffDestinationFailureFallback(ctx context.Context, store storage.Store, result HandoffResumeResult, inputs Inputs, trigger FallbackTrigger, actor delivery.Actor, host delivery.Host) (FallbackDecision, error) {
	inputs.Candidates = candidatesFromRoutingDecision(result.RoutingDecision)
	return DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID:         result.RoutingDecision.RoutingDecisionID,
		Trigger:                   trigger,
		PriorCandidateID:          result.RoutingDecision.ChosenCandidateID,
		Inputs:                    inputs,
		AttemptLineage:            []string{result.Handoff.SourceAttemptID, result.Successor.AttemptID},
		HeldBudgetReservationID:   result.Reservation.BudgetReservationID,
		HeldReservationGeneration: result.Reservation.Generation,
		DecidedBy:                 actor,
		Host:                      host,
	})
}

func handoffResumeCancelled(ctx context.Context, input HandoffResumeInput) bool {
	if input.Cancelled {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	if input.IsCancelled != nil && input.IsCancelled(ctx) {
		return true
	}
	return false
}

func handoffLaunchNeedsHuman(launch storage.HandoffSuccessorLaunch, now time.Time) bool {
	if strings.TrimSpace(launch.ProviderReceipt) != "" || launch.LaunchPhase == storage.ClaimPhaseExecuting || launch.LaunchPhase == storage.ClaimPhaseCompleted {
		return false
	}
	if launch.LaunchPhase != storage.ClaimPhaseLaunching {
		return false
	}
	lease, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(launch.LeaseExpiresAt))
	if err != nil {
		return true
	}
	return !lease.After(now.UTC())
}

func transitionHandoffSuccessorLaunching(ctx context.Context, store storage.Store, handoff storage.HandoffTransaction, launch storage.HandoffSuccessorLaunch) error {
	registration, err := storage.ValidateNativeChildLaunch(ctx, store, handoff.ChildRunID, launch.ExecutorID, launch.ClaimGeneration)
	if err != nil {
		return err
	}
	_, err = storage.TransitionAgentRegistration(ctx, store, registration.ChildAgentID, storage.AgentActionLaunch, launch.ExecutorID, launch.ClaimGeneration, delivery.CanonicalTimestamp(store.Now()))
	return err
}

func markHandoffSuccessorExecuting(store storage.Store, handoff storage.HandoffTransaction, launch storage.HandoffSuccessorLaunch, providerReceipt string) error {
	cleanupCtx, cancel := handoffCleanupContext()
	defer cancel()
	if err := storage.UpdateChildRunClaimPhase(cleanupCtx, store, handoff.ParentRunID, handoff.ChildRunID, launch.ExecutorID, launch.ClaimGeneration, storage.ClaimPhaseExecuting, delivery.CanonicalTimestamp(store.Now()), providerReceipt); err != nil {
		return err
	}
	childAgentID, err := handoffChildAgentID(cleanupCtx, store, handoff.ChildRunID)
	if err != nil {
		return err
	}
	_, err = storage.TransitionAgentRegistration(cleanupCtx, store, childAgentID, storage.AgentActionHeartbeat, launch.ExecutorID, launch.ClaimGeneration, delivery.CanonicalTimestamp(store.Now()))
	return err
}

func cleanupHandoffLaunchTerminal(store storage.Store, handoff storage.HandoffTransaction, launch storage.HandoffSuccessorLaunch, reservation budget.Reservation, status string, code taskrequirements.ErrorCode, reason string, actor delivery.Actor, host delivery.Host) error {
	cleanupCtx, cancel := handoffCleanupContext()
	defer cancel()
	releaseErr := releaseHandoffReservation(cleanupCtx, store, reservation, reason, actor, host)
	terminalErr := terminalizeHandoffLaunch(cleanupCtx, store, handoff, launch, status, code, reason)
	if terminalErr != nil && status != "needs-human" {
		terminalErr = errors.Join(terminalErr, terminalizeHandoffLaunch(cleanupCtx, store, handoff, launch, "needs-human", code, reason+"-cleanup-failed"))
	}
	return errors.Join(releaseErr, terminalErr)
}

func terminalizeHandoffLaunch(ctx context.Context, store storage.Store, handoff storage.HandoffTransaction, launch storage.HandoffSuccessorLaunch, status string, code taskrequirements.ErrorCode, reason string) error {
	status = strings.TrimSpace(status)
	if status != "cancelled" && status != "needs-human" {
		return fmt.Errorf("terminalize handoff launch: unsupported status %q", status)
	}
	at := delivery.CanonicalTimestamp(store.Now())
	agentState := storage.AgentStateCancelled
	if status == "needs-human" {
		agentState = storage.AgentStateNeedsHuman
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		result, err := tx.Exec(ctx, `UPDATE run_claims
			SET phase = ?, heartbeat_at = ?
			WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`,
			storage.ClaimPhaseCompleted, at, handoff.ChildRunID, launch.ExecutorID, launch.ClaimGeneration)
		if err != nil {
			return fmt.Errorf("terminalize handoff launch claim: %w", err)
		}
		if err := requireHandoffCleanupRow(result, "claim"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE runs
			SET status = ?, ended_at = COALESCE(ended_at, ?), updated_at = ?
			WHERE id = ?`,
			status, at, at, handoff.ChildRunID); err != nil {
			return fmt.Errorf("terminalize handoff launch run: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE run_edges
			SET status = ?, updated_at = ?
			WHERE parent_run_id = ? AND child_run_id = ?`,
			status, at, handoff.ParentRunID, handoff.ChildRunID); err != nil {
			return fmt.Errorf("terminalize handoff launch edge: %w", err)
		}
		result, err = tx.Exec(ctx, `UPDATE delivery_attempts
			SET state = ?, ended_at = COALESCE(ended_at, ?), updated_at = ?, terminal_error_code = ?, error_message = ?
			WHERE attempt_id = ? AND executor_id = ? AND claim_generation = ?`,
			status, at, at, string(code), reason, launch.AttemptID, launch.ExecutorID, launch.ClaimGeneration)
		if err != nil {
			return fmt.Errorf("terminalize handoff launch attempt: %w", err)
		}
		if err := requireHandoffCleanupRow(result, "attempt"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE delivery_tasks
			SET state = ?, ended_at = COALESCE(ended_at, ?), updated_at = ?, terminal_error_code = ?, error_message = ?
			WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
			status, at, at, string(code), reason, handoff.ProjectID, handoff.DeliveryRunID, handoff.TaskID); err != nil {
			return fmt.Errorf("terminalize handoff launch task: %w", err)
		}
		result, err = tx.Exec(ctx, `UPDATE agent_registrations
			SET registration_state = ?, terminal_error_code = ?, updated_at = ?, record_version = record_version + 1
			WHERE child_run_id = ? AND executor_id = ? AND claim_generation = ?`,
			agentState, string(code), at, handoff.ChildRunID, launch.ExecutorID, launch.ClaimGeneration)
		if err != nil {
			return fmt.Errorf("terminalize handoff launch registration: %w", err)
		}
		if err := requireHandoffCleanupRow(result, "registration"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_budget_bindings
			SET reservation_state = ?, updated_at = ?
			WHERE budget_reservation_id = ?`,
			status, at, launch.BudgetReservationID); err != nil {
			return fmt.Errorf("terminalize handoff launch budget binding: %w", err)
		}
		return nil
	})
}

func requireHandoffCleanupRow(result interface{ RowsAffected() (int64, error) }, label string) error {
	affected, err := result.RowsAffected()
	if err == nil && affected != 1 {
		return fmt.Errorf("terminalize handoff launch %s: updated %d rows, want 1", label, affected)
	}
	return nil
}

func candidatesFromRoutingDecision(decision RoutingDecision) []Candidate {
	out := make([]Candidate, 0, len(decision.EligibleCandidates)+len(decision.RejectedCandidates))
	out = append(out, decision.EligibleCandidates...)
	for _, rejected := range decision.RejectedCandidates {
		out = append(out, rejected.Candidate)
	}
	return out
}

func handoffReserveRequest(handoff storage.HandoffTransaction, childAgentID string, selected Candidate, decision RoutingDecision, input HandoffResumeInput, now time.Time) budget.ReserveRequest {
	value := input.ReservationValue
	if value <= 0 {
		value = 1
	}
	lease := input.LeaseExpiresAt
	if lease.IsZero() {
		lease = now.Add(15 * time.Minute)
	}
	scope := budget.Scope{
		ScopeKind:         budget.ScopeSubAgent,
		ProjectID:         handoff.ProjectID,
		DeliveryRunID:     handoff.DeliveryRunID,
		TaskID:            handoff.TaskID,
		SubAgentID:        childAgentID,
		AdapterID:         selected.AdapterID,
		AccountProfileID:  selected.AccountProfileID,
		ModelCapabilityID: selected.ModelCapabilityID,
	}
	return budget.ReserveRequest{
		ScopeChain:                   []budget.Scope{scope},
		QuantityKind:                 providerinventory.QuantityLocalPolicy,
		WindowKind:                   providerinventory.WindowUnbounded,
		RequestedValue:               value,
		LeaseExpiresAt:               lease,
		IdempotencyKey:               "handoff-successor:" + handoff.HandoffID + ":" + decision.RoutingFingerprint,
		RequesterID:                  handoff.DestinationExecutorID,
		AuthorizationFingerprint:     handoff.AuthorizationFingerprint,
		SourceEstimateUsageRecordIDs: boundedEvidenceRefs(input.ReusableEvidenceIDs, handoff.EvidenceRecordIDs),
		RequirementConfidence:        providerinventory.ConfidenceExact,
		Actor:                        budget.Actor{ActorID: input.DecidedBy.ActorID, Role: input.DecidedBy.ActorKind},
		Host:                         budget.Host{HostID: input.Host.HostID, Provider: input.Host.HostKind},
	}
}

func handoffChildAgentID(ctx context.Context, store storage.Store, childRunID string) (string, error) {
	var childAgentID string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM agent_registrations WHERE child_run_id = ?`, childRunID).Scan(&childAgentID)
	})
	if err != nil {
		return "", err
	}
	return childAgentID, nil
}

func releaseHandoffReservation(ctx context.Context, store storage.Store, reservation budget.Reservation, key string, actor delivery.Actor, host delivery.Host) error {
	if reservation.BudgetReservationID == "" || reservation.Generation <= 0 {
		return nil
	}
	_, err := budget.Release(ctx, store, budget.MutationRequest{
		ReservationID:  reservation.BudgetReservationID,
		IdempotencyKey: key,
		Generation:     reservation.Generation,
		Actor:          budget.Actor{ActorID: actor.ActorID, Role: actor.ActorKind},
		Host:           budget.Host{HostID: host.HostID, Provider: host.HostKind},
	})
	return err
}

func handoffCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func releaseHandoffReservationCleanup(store storage.Store, reservation budget.Reservation, key string, actor delivery.Actor, host delivery.Host) error {
	cleanupCtx, cancel := handoffCleanupContext()
	defer cancel()
	return releaseHandoffReservation(cleanupCtx, store, reservation, key, actor, host)
}

func releaseHandoffReservationsCleanup(store storage.Store, reservations []budget.Reservation, keyPrefix string, actor delivery.Actor, host delivery.Host) error {
	var out error
	for _, reservation := range reservations {
		key := keyPrefix
		if reservation.BudgetReservationID != "" {
			key += ":" + reservation.BudgetReservationID
		}
		if err := releaseHandoffReservationCleanup(store, reservation, key, actor, host); err != nil {
			if live, liveErr := handoffReservationLive(context.Background(), store, reservation.BudgetReservationID); liveErr == nil && !live {
				continue
			}
			out = errors.Join(out, fmt.Errorf("release obsolete handoff reservation %s: %w", reservation.BudgetReservationID, err))
		}
	}
	return out
}

func handoffReservationLive(ctx context.Context, store storage.Store, reservationID string) (bool, error) {
	var state string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM budget_reservations WHERE budget_reservation_id = ?`, reservationID).Scan(&state)
	}); err != nil {
		return false, err
	}
	return liveReservationState(budget.ReservationState(state)), nil
}

func handoffObsoleteReservations(ctx context.Context, store storage.Store, childRunID, destinationReservationID string) ([]budget.Reservation, error) {
	var reservations []budget.Reservation
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT br.budget_reservation_id, br.generation, br.state
			FROM agent_registrations ar
			JOIN agent_budget_bindings abb ON abb.child_agent_id = ar.id
			JOIN budget_reservations br ON br.budget_reservation_id = abb.budget_reservation_id
			WHERE ar.child_run_id = ?`, childRunID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var reservation budget.Reservation
			var state string
			if err := rows.Scan(&reservation.BudgetReservationID, &reservation.Generation, &state); err != nil {
				return err
			}
			reservation.State = budget.ReservationState(state)
			if reservation.BudgetReservationID != "" && reservation.BudgetReservationID != destinationReservationID && liveReservationState(reservation.State) {
				reservations = append(reservations, reservation)
			}
		}
		return rows.Err()
	})
	return reservations, err
}

func selectedCandidate(decision RoutingDecision) Candidate {
	for _, scored := range decision.ScoredCandidates {
		if scored.RoutingCandidateID == decision.ChosenCandidateID {
			return scored.Candidate
		}
	}
	for _, candidate := range decision.EligibleCandidates {
		if candidate.RoutingCandidateID == decision.ChosenCandidateID {
			return candidate
		}
	}
	return Candidate{}
}

func boundedEvidenceRefs(primary, fallback []string) []string {
	refs := append([]string{}, primary...)
	if len(refs) == 0 {
		refs = append(refs, fallback...)
	}
	refs = dedupeStrings(refs)
	if len(refs) > 32 {
		refs = refs[:32]
	}
	return refs
}

func firstActor(values ...delivery.Actor) delivery.Actor {
	for _, value := range values {
		if value.ActorID != "" || value.ActorKind != "" || value.DecisionAuthority != "" || value.Source != "" {
			return value
		}
	}
	return delivery.Actor{}
}

func firstHost(values ...delivery.Host) delivery.Host {
	for _, value := range values {
		if value.HostID != "" || value.HostKind != "" {
			return value
		}
	}
	return delivery.Host{}
}
