package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/availability"
	"github.com/jasonhnd/loopcoder/internal/budget"
	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/runtimecap"
	"github.com/jasonhnd/loopcoder/internal/sanitize"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

const (
	RouteOperationSchema  = "loopcoder.route_operation.v1"
	RouteOperationExplain = "explain"
	RouteOperationDecide  = "decide"
	RouteOutcomeSelected  = "selected"
	RouteOutcomeNoRoute   = "no_route"

	maxRouteServiceCandidates = 256
)

// StoredRouteRequest binds a product route operation to immutable local state.
// Role, permission, capability, and task-fit inputs come from the referenced
// TaskRequirement. Budget and reset-window fit come from durable budget/quota
// state plus the active routing policy rather than mutable command-line copies.
type StoredRouteRequest struct {
	ProjectID               string
	DeliveryRunID           string
	TaskRequirementID       string
	DecisionKey             string
	RoutingPolicyProfileKey string
	HostName                string
	Pin                     *CandidateConstraint
	PinReason               string
	PinActor                delivery.Actor
	DecidedBy               delivery.Actor
	Host                    delivery.Host
}

type RouteOperationResult struct {
	SchemaVersion          string          `json:"schema_version"`
	Operation              string          `json:"operation"`
	Outcome                string          `json:"outcome"`
	Persisted              bool            `json:"persisted"`
	Replayed               bool            `json:"replayed"`
	ProviderCalls          int             `json:"provider_calls"`
	PriorRoutingDecisionID string          `json:"prior_routing_decision_id,omitempty"`
	Decision               RoutingDecision `json:"decision"`
}

// ExplainStoredRoute calculates a current route from durable local evidence
// without persisting profiles, pins, or routing decisions.
func ExplainStoredRoute(ctx context.Context, store storage.Store, request StoredRouteRequest) (RouteOperationResult, error) {
	request, profile, err := normalizeStoredRouteRequest(store, request, RouteOperationExplain)
	if err != nil {
		return RouteOperationResult{}, err
	}
	var result RouteOperationResult
	err = store.WithTx(ctx, func(tx storage.Tx) error {
		txStore := routeTransactionStore{parent: store, tx: tx, now: store.Now()}
		input, err := assembleStoredRouteInput(ctx, txStore, request, profile)
		if err != nil {
			return err
		}
		decision, err := BuildRoutingDecision(input)
		if err != nil {
			return err
		}
		prior, found, err := loadRoutingDecisionByKey(ctx, txStore, request.ProjectID, request.DeliveryRunID, request.DecisionKey)
		if err != nil {
			return err
		}
		result = routeOperationResult(RouteOperationExplain, false, false, decision)
		if found {
			result.PriorRoutingDecisionID = prior.RoutingDecisionID
		}
		return nil
	})
	return result, err
}

// DecideStoredRoute persists one immutable route before any provider launch.
// Replays return the prior decision; changing a pin under the same decision key
// fails closed and requires a typed fallback/successor operation.
func DecideStoredRoute(ctx context.Context, store storage.Store, request StoredRouteRequest) (RouteOperationResult, error) {
	request, profile, err := normalizeStoredRouteRequest(store, request, RouteOperationDecide)
	if err != nil {
		return RouteOperationResult{}, err
	}
	var result RouteOperationResult
	var routeErr error
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		result = RouteOperationResult{}
		routeErr = nil
		txStore := routeTransactionStore{parent: store, tx: tx, now: store.Now(), writable: true}
		candidate, candidateErr := decideStoredRouteInTransaction(ctx, txStore, request, profile)
		if candidateErr != nil && candidate.Decision.RoutingDecisionID == "" {
			return candidateErr
		}
		result = candidate
		routeErr = candidateErr
		return nil
	})
	if err != nil {
		return RouteOperationResult{}, err
	}
	return result, routeErr
}

func decideStoredRouteInTransaction(ctx context.Context, store storage.Store, request StoredRouteRequest, profile RoutingPolicyProfile) (RouteOperationResult, error) {
	prior, found, err := loadRoutingDecisionByKey(ctx, store, request.ProjectID, request.DeliveryRunID, request.DecisionKey)
	if err != nil {
		return RouteOperationResult{}, err
	}
	if found {
		if prior.TaskRequirementID != request.TaskRequirementID || prior.RoutingPolicyProfileID != profile.RoutingPolicyProfileID {
			return RouteOperationResult{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "stored routing decision authority does not match replay request"}
		}
		if request.Pin != nil && !decisionContainsPin(prior, *request.Pin) {
			return RouteOperationResult{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "stored routing decision cannot be replaced by a different explicit pin"}
		}
		result := routeOperationResult(RouteOperationDecide, true, true, prior)
		if prior.TerminalErrorCode != "" {
			return result, routingTerminalError(prior.TerminalErrorCode)
		}
		return result, nil
	}

	if _, err := EnsureBuiltInRoutingPolicyProfiles(ctx, store, store.Now()); err != nil {
		return RouteOperationResult{}, err
	}
	input, err := assembleStoredRouteInput(ctx, store, request, profile)
	if err != nil {
		return RouteOperationResult{}, err
	}
	if _, err := BuildRoutingDecision(input); err != nil {
		return RouteOperationResult{}, err
	}
	if request.Pin != nil {
		if _, err := PersistPolicyInput(ctx, store, PolicyInputRecord{
			InputKind:              PolicyInputKindPin,
			ProjectID:              request.ProjectID,
			DeliveryRunID:          request.DeliveryRunID,
			RoutingPolicyProfileID: profile.RoutingPolicyProfileID,
			PolicyFingerprint:      profile.PolicyFingerprint,
			Scope:                  "task:" + input.Inputs.Requirement.TaskID,
			Reason:                 sanitize.Text(request.PinReason),
			Constraint:             *request.Pin,
			Actor:                  request.PinActor,
			Host:                   request.Host,
		}); err != nil {
			return RouteOperationResult{}, err
		}
	}

	// The authoritative decision path reloads the persisted pin set and cached
	// inventory. Do not let a transient caller copy become authority.
	input.Inputs.Pins = nil
	decision, routeErr := DecideAndPersistRoute(ctx, store, input)
	if decision.RoutingDecisionID == "" {
		return RouteOperationResult{}, routeErr
	}
	result := routeOperationResult(RouteOperationDecide, true, false, decision)
	return result, routeErr
}

// routeTransactionStore lets existing routing functions share one outer
// SQLite transaction. Explain leaves writable false so nested persistence
// fails closed; decide enables writes so its pin and decision commit atomically.
type routeTransactionStore struct {
	parent   storage.Store
	tx       storage.Tx
	now      time.Time
	writable bool
}

func (s routeTransactionStore) Close() error { return nil }

func (s routeTransactionStore) Path() string { return s.parent.Path() }

func (s routeTransactionStore) Now() time.Time { return s.now.UTC() }

func (s routeTransactionStore) Health(ctx context.Context) (storage.Health, error) {
	return s.parent.Health(ctx)
}

func (s routeTransactionStore) WithTx(_ context.Context, fn func(storage.Tx) error) error {
	return fn(s.tx)
}

func (s routeTransactionStore) WithWriteTx(_ context.Context, fn func(storage.Tx) error) error {
	if !s.writable {
		return errors.New("route explain transaction is read-only")
	}
	return fn(s.tx)
}

func normalizeStoredRouteRequest(store storage.Store, request StoredRouteRequest, operation string) (StoredRouteRequest, RoutingPolicyProfile, error) {
	if store == nil {
		return request, RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "route store is required"}
	}
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.DeliveryRunID = strings.TrimSpace(request.DeliveryRunID)
	request.TaskRequirementID = strings.TrimSpace(request.TaskRequirementID)
	request.DecisionKey = strings.TrimSpace(request.DecisionKey)
	request.RoutingPolicyProfileKey = strings.TrimSpace(request.RoutingPolicyProfileKey)
	request.HostName = strings.TrimSpace(request.HostName)
	request.PinReason = strings.TrimSpace(request.PinReason)
	if request.ProjectID == "" || request.DeliveryRunID == "" || request.TaskRequirementID == "" || request.DecisionKey == "" {
		return request, RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "project_id, delivery_run_id, task_requirement_id, and decision_key are required"}
	}
	if request.RoutingPolicyProfileKey == "" {
		request.RoutingPolicyProfileKey = ProfileKeyBalanced
	}
	profile, ok := BuiltInRoutingPolicyProfile(request.RoutingPolicyProfileKey, store.Now())
	if !ok {
		return request, RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("unknown routing profile %q", request.RoutingPolicyProfileKey)}
	}
	if request.HostName == "" {
		request.HostName = "generic-local"
	}
	if request.DecidedBy.ActorID == "" {
		request.DecidedBy = delivery.Actor{ActorKind: "system", ActorID: "route-service", DecisionAuthority: "router", Source: "loopcoder route " + operation}
	}
	if request.Host.HostID == "" {
		request.Host = delivery.Host{HostKind: "cli", HostID: "generic-local", Platform: "generic-local"}
	}
	if request.Pin != nil {
		pin := *request.Pin
		pin.AdapterID = strings.TrimSpace(pin.AdapterID)
		pin.ProviderInstallationID = strings.TrimSpace(pin.ProviderInstallationID)
		pin.AccountProfileID = strings.TrimSpace(pin.AccountProfileID)
		pin.ModelCapabilityID = strings.TrimSpace(pin.ModelCapabilityID)
		pin.InvocationProfileKey = strings.TrimSpace(pin.InvocationProfileKey)
		request.Pin = &pin
		if emptyCandidateConstraint(*request.Pin) {
			return request, RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "explicit pin must name a provider, installation, account, model, or invocation profile"}
		}
		if operation == RouteOperationDecide && request.PinReason == "" {
			return request, RoutingPolicyProfile{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: "persisted explicit pin requires a reason"}
		}
		if request.PinActor.ActorID == "" {
			request.PinActor = delivery.Actor{ActorKind: "user", ActorID: "local-user", DecisionAuthority: "user", Source: "loopcoder route " + operation}
		}
	}
	return request, profile, nil
}

func assembleStoredRouteInput(ctx context.Context, store storage.Store, request StoredRouteRequest, profile RoutingPolicyProfile) (DecisionInput, error) {
	now := store.Now().UTC()
	requirement, err := taskrequirements.LoadTaskRequirement(ctx, store, request.TaskRequirementID)
	if err != nil {
		return DecisionInput{}, err
	}
	if err := taskrequirements.Validate(requirement); err != nil {
		return DecisionInput{}, err
	}
	if requirement.ProjectID != request.ProjectID || requirement.DeliveryRunID != request.DeliveryRunID {
		return DecisionInput{}, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "task requirement does not belong to the requested project and delivery run"}
	}
	authorizationFingerprint, err := loadRunAuthorizationFingerprint(ctx, store, request.ProjectID, request.DeliveryRunID)
	if err != nil {
		return DecisionInput{}, err
	}
	roles := BuiltInRoleDefinitions()
	role, ok := ResolveRoleDefinition(requirement.RoleKey, roles)
	if !ok {
		return DecisionInput{}, &taskrequirements.TypedError{Code: taskrequirements.ErrMissingReferenceCode, Message: fmt.Sprintf("role %q is not registered", requirement.RoleKey)}
	}
	availabilityResult, err := availability.Load(ctx, store)
	if err != nil {
		return DecisionInput{}, err
	}
	budgets, err := budget.Summaries(ctx, store, request.ProjectID)
	if err != nil {
		return DecisionInput{}, err
	}
	inputs, err := InputsWithCachedInventory(ctx, store, Inputs{
		Requirement:        requirement,
		RoleDefinitions:    roles,
		Availability:       availabilityResult.Scores,
		CircuitBreakers:    availabilityResult.CircuitBreakers,
		Budgets:            budgets,
		RuntimeContract:    runtimecap.DefaultContract(),
		HostName:           request.HostName,
		Policy:             profile.EligibilityPolicy,
		OptimizationPolicy: profile.OptimizationPolicy,
		Now:                now,
	})
	if err != nil {
		return DecisionInput{}, err
	}
	if len(inputs.Candidates) > maxRouteServiceCandidates {
		return DecisionInput{}, &taskrequirements.TypedError{Code: taskrequirements.ErrInvalidRecordCode, Message: fmt.Sprintf("route candidate count %d exceeds limit %d", len(inputs.Candidates), maxRouteServiceCandidates)}
	}
	records, err := LoadActivePolicyInputs(ctx, store, request.ProjectID, request.DeliveryRunID, profile.RoutingPolicyProfileID)
	if err != nil {
		return DecisionInput{}, err
	}
	records = policyInputsForTask(records, requirement.TaskID)
	pins, exclusions := constraintsFromPolicyInputRecords(records)
	if request.Pin != nil {
		transient := Pin{
			PinID:                  "route-cli-transient-pin",
			AdapterID:              request.Pin.AdapterID,
			ProviderInstallationID: request.Pin.ProviderInstallationID,
			AccountProfileID:       request.Pin.AccountProfileID,
			ModelCapabilityID:      request.Pin.ModelCapabilityID,
			InvocationProfileKey:   request.Pin.InvocationProfileKey,
		}
		if diagnostics := ValidatePolicyInputs(inputs.Inventory, []Pin{transient}, nil, profile.PolicyFingerprint, now); len(diagnostics) > 0 {
			return DecisionInput{}, &taskrequirements.TypedError{Code: diagnostics[0].Code, Message: diagnostics[0].Message}
		}
		pins = append(pins, transient)
	}
	inputs.Pins = pins
	inputs.Exclusions = exclusions
	input := DecisionInput{
		ProjectID:                request.ProjectID,
		DeliveryRunID:            request.DeliveryRunID,
		DecisionKey:              request.DecisionKey,
		TaskRequirementID:        requirement.TaskRequirementID,
		RoleDefinitionID:         role.RoleDefinitionID,
		PlanFingerprint:          requirement.PlanFingerprint,
		PolicyFingerprint:        profile.PolicyFingerprint,
		AuthorizationFingerprint: authorizationFingerprint,
		RoutingPolicyProfileID:   profile.RoutingPolicyProfileID,
		RoutingPolicyProfile:     profile,
		PolicyInputRecords:       records,
		Inputs:                   inputs,
		OptimizationPolicy:       profile.OptimizationPolicy,
		DecidedBy:                request.DecidedBy,
		Host:                     request.Host,
		Now:                      now,
	}
	if err := validateStoredPolicyInputsForDecision(records, input, profile.RoutingPolicyProfileID, profile.PolicyFingerprint); err != nil {
		return DecisionInput{}, err
	}
	return input, nil
}

func routeOperationResult(operation string, persisted, replayed bool, decision RoutingDecision) RouteOperationResult {
	outcome := RouteOutcomeSelected
	if decision.DecisionStatus == DecisionStatusNoEligible {
		outcome = RouteOutcomeNoRoute
	}
	return RouteOperationResult{
		SchemaVersion: RouteOperationSchema,
		Operation:     operation,
		Outcome:       outcome,
		Persisted:     persisted,
		Replayed:      replayed,
		Decision:      decision,
	}
}

func loadRoutingDecisionByKey(ctx context.Context, store storage.Store, projectID, deliveryRunID, decisionKey string) (RoutingDecision, bool, error) {
	var payloads []string
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload_json FROM routing_decisions
			WHERE project_id = ? AND delivery_run_id = ? AND decision_key = ?
			ORDER BY created_at, routing_decision_id LIMIT 2`, projectID, deliveryRunID, decisionKey)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			payloads = append(payloads, payload)
		}
		return rows.Err()
	})
	if err != nil {
		return RoutingDecision{}, false, err
	}
	if len(payloads) == 0 {
		return RoutingDecision{}, false, nil
	}
	if len(payloads) > 1 {
		return RoutingDecision{}, false, &taskrequirements.TypedError{Code: taskrequirements.ErrRoutingFingerprintMismatchCode, Message: "multiple first routing decisions exist for one execution attempt"}
	}
	var decision RoutingDecision
	if err := json.Unmarshal([]byte(payloads[0]), &decision); err != nil {
		return RoutingDecision{}, false, err
	}
	return decision, true, nil
}

func decisionContainsPin(decision RoutingDecision, constraint CandidateConstraint) bool {
	for _, record := range decision.PolicyInputRecords {
		if record.InputKind == PolicyInputKindPin && sameConstraint(record.Constraint, constraint) {
			return true
		}
	}
	return false
}
