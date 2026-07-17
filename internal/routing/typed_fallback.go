package routing

import (
	"context"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/provideroutcome"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

// TypedFallbackRequest connects a classified provider failure to bounded
// fallback against a persisted first-route decision.
type TypedFallbackRequest struct {
	RoutingDecisionID string
	PriorCandidateID  string
	Class             provideroutcome.Class
	AttemptLineage    []string
	IdempotencyKey    string
	Pinned            bool
	DecidedBy         delivery.Actor
	Host              delivery.Host
}

// TypedFallbackResult is the orchestration-facing fallback receipt.
type TypedFallbackResult struct {
	Class               provideroutcome.Class `json:"class"`
	Trigger             FallbackTrigger       `json:"trigger"`
	AutoFallbackAllowed bool                  `json:"auto_fallback_allowed"`
	NeedsHuman          bool                  `json:"needs_human"`
	Applied             bool                  `json:"applied"`
	Decision            FallbackDecision      `json:"decision,omitempty"`
	NextAction          string                `json:"next_action"`
}

func fallbackTriggerFromClass(class provideroutcome.Class) FallbackTrigger {
	return FallbackTrigger(provideroutcome.FallbackTrigger(class))
}

// ApplyTypedProviderFailure maps a structured provider failure onto the
// existing DecideAndPersistFallback path. Ambiguous and needs-human classes
// never select a successor. Explicit pins refuse automatic fallback.
func ApplyTypedProviderFailure(ctx context.Context, store storage.Store, req TypedFallbackRequest) (TypedFallbackResult, error) {
	class := req.Class
	trigger := fallbackTriggerFromClass(class)
	if !provideroutcome.AllowsAutomaticFallback(class) || provideroutcome.NeedsHuman(class) {
		return TypedFallbackResult{
			Class:               class,
			Trigger:             trigger,
			AutoFallbackAllowed: false,
			NeedsHuman:          true,
			Applied:             false,
			NextAction:          "needs-human: typed outcome forbids automatic successor provider launch",
		}, nil
	}
	if req.Pinned {
		return TypedFallbackResult{
			Class:               class,
			Trigger:             trigger,
			AutoFallbackAllowed: false,
			NeedsHuman:          true,
			Applied:             false,
			NextAction:          "needs-human: explicit pin forbids automatic fallback without owner authorization",
		}, nil
	}
	if strings.TrimSpace(req.RoutingDecisionID) == "" || strings.TrimSpace(req.PriorCandidateID) == "" {
		return TypedFallbackResult{
			Class:               class,
			Trigger:             trigger,
			AutoFallbackAllowed: true,
			NeedsHuman:          false,
			Applied:             false,
			NextAction:          "typed failure recorded but missing routing decision or prior candidate for fallback",
		}, fmt.Errorf("routing decision and prior candidate are required for automatic fallback")
	}
	decision, err := DecideAndPersistFallback(ctx, store, FallbackInput{
		RoutingDecisionID: req.RoutingDecisionID,
		Trigger:           trigger,
		PriorCandidateID:  req.PriorCandidateID,
		IdempotencyKey:    firstNonEmptyFallback(req.IdempotencyKey, "typed-fallback:"+req.RoutingDecisionID+":"+string(class)),
		AttemptLineage:    append([]string{}, req.AttemptLineage...),
		DecidedBy:         req.DecidedBy,
		Host:              req.Host,
	})
	if err != nil {
		return TypedFallbackResult{
			Class:               class,
			Trigger:             trigger,
			AutoFallbackAllowed: true,
			NeedsHuman:          false,
			Applied:             false,
			NextAction:          "fallback decide failed: " + err.Error(),
		}, err
	}
	out := TypedFallbackResult{
		Class:               class,
		Trigger:             trigger,
		AutoFallbackAllowed: true,
		Applied:             true,
		Decision:            decision,
	}
	switch decision.DecisionStatus {
	case FallbackStatusSelected:
		out.NextAction = "dispatch successor candidate " + decision.FallbackCandidateID
	case FallbackStatusNeedsHuman:
		out.NeedsHuman = true
		out.NextAction = "needs-human: fallback decided needs-human"
	case FallbackStatusReplanRequired, FallbackStatusBlocked:
		out.NeedsHuman = true
		out.NextAction = "needs-human: fallback exhausted or blocked (" + decision.DecisionStatus + ")"
	default:
		out.NextAction = "inspect fallback decision " + decision.FallbackDecisionID
	}
	return out, nil
}

func firstNonEmptyFallback(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
