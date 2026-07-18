package orchestration

import (
	"context"
	"fmt"
	"strings"
)

// ChildRouteDecision is the launch-facing selection for one nested child.
// It is applied to the immutable ChildExecutionRequest before the plan is
// persisted and before claim/launch.
type ChildRouteDecision struct {
	RoutingDecisionID    string
	AdapterID            string
	ModelCapabilityID    string
	ReasoningProfileID   string
	Model                string
	Effort               string
	Outcome              string
	ChosenReason         string
	Replayed             bool
	ZeroProviderLaunches bool
}

// ChildRouteResolver decides a provider/model route from the immutable child
// execution contract only. Implementations must be deterministic for a given
// request identity so replay reuses the same decision.
type ChildRouteResolver func(ctx context.Context, request ChildExecutionRequest) (ChildRouteDecision, error)

// ApplyChildRouteDecision fills ProviderDecision and Work from a route
// decision and recomputes the contract fingerprint. Claim generation and
// lifecycle remain fenced runtime bindings.
func ApplyChildRouteDecision(request ChildExecutionRequest, decision ChildRouteDecision) (ChildExecutionRequest, error) {
	request = cloneChildExecutionRequest(request)
	adapter := strings.TrimSpace(decision.AdapterID)
	if adapter == "" {
		return ChildExecutionRequest{}, fmt.Errorf("child route decision requires adapter_id")
	}
	model := firstNonEmptyChild(decision.Model, decision.ModelCapabilityID)
	effort := firstNonEmptyChild(decision.Effort, decision.ReasoningProfileID)
	request.ProviderDecision = ChildProviderDecisionRef{
		RoutingDecisionID:  strings.TrimSpace(decision.RoutingDecisionID),
		AdapterID:          adapter,
		ModelCapabilityID:  strings.TrimSpace(firstNonEmptyChild(decision.ModelCapabilityID, model)),
		ReasoningProfileID: strings.TrimSpace(firstNonEmptyChild(decision.ReasoningProfileID, effort)),
	}
	request.Work.Provider = adapter
	if model != "" {
		request.Work.Model = model
	}
	if effort != "" {
		request.Work.Effort = effort
	}
	request.ContractFingerprint = childExecutionRequestFingerprint(request)
	if err := ValidateChildExecutionRequest(request, false); err != nil {
		return ChildExecutionRequest{}, err
	}
	return request, nil
}

// applyChildRouteToResult copies route receipt fields onto the child result for
// parent and child audit output.
func applyChildRouteToResult(result ChildRunResult, request ChildExecutionRequest, decision ChildRouteDecision) ChildRunResult {
	result.RoutingDecisionID = firstNonEmptyChild(decision.RoutingDecisionID, request.ProviderDecision.RoutingDecisionID)
	result.RouteAdapterID = firstNonEmptyChild(decision.AdapterID, request.ProviderDecision.AdapterID, request.Work.Provider)
	result.RouteModel = firstNonEmptyChild(decision.Model, request.Work.Model, request.ProviderDecision.ModelCapabilityID)
	result.RouteOutcome = firstNonEmptyChild(decision.Outcome, result.RouteOutcome)
	result.RouteReason = firstNonEmptyChild(decision.ChosenReason, result.RouteReason)
	if decision.Replayed {
		result.RouteReplayed = true
	}
	return result
}

func childRouteAlreadyDecided(request ChildExecutionRequest) bool {
	return strings.TrimSpace(request.ProviderDecision.RoutingDecisionID) != "" &&
		strings.TrimSpace(request.ProviderDecision.AdapterID) != ""
}
