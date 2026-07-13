package routing

import (
	"context"
	"errors"
	"strconv"
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
	BeforeLaunch        func() error
	DecidedBy           delivery.Actor
	Host                delivery.Host
}

type HandoffResumeResult struct {
	Handoff         storage.HandoffTransaction
	RoutingDecision RoutingDecision
	Reservation     budget.Reservation
	Successor       storage.HandoffSuccessorLaunch
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
	if input.Cancelled {
		_ = releaseHandoffReservation(ctx, store, result.Reservation, "cancelled-before-launch", input.DecidedBy, input.Host)
		return result, &taskrequirements.TypedError{Code: taskrequirements.ErrReplanRequiredCode, Message: "handoff resume cancelled before launch"}
	}
	if input.BeforeLaunch != nil {
		if err := input.BeforeLaunch(); err != nil {
			_ = releaseHandoffReservation(ctx, store, result.Reservation, "stopped-before-launch", input.DecidedBy, input.Host)
			return result, err
		}
	}
	actorJSON, err := delivery.CanonicalJSON(firstActor(input.DecidedBy, routeInput.DecidedBy))
	if err != nil {
		return result, err
	}
	hostJSON, err := delivery.CanonicalJSON(firstHost(input.Host, routeInput.Host))
	if err != nil {
		return result, err
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
		_ = releaseHandoffReservation(ctx, store, result.Reservation, "launch-refused", input.DecidedBy, input.Host)
		return result, err
	}
	result.Successor = launch
	result.Replay = reserved.Replay || launch.Replay
	return result, nil
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
