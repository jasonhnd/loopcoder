package worker

import (
	"errors"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/provideroutcome"
)

func TestAttachTypedFailureNeedsHumanClassesDoNotAutoFallback(t *testing.T) {
	dispatch := &dispatchContext{
		opts: Options{RoutingDecisionID: "route-1", RoutePinned: false},
	}
	result := attachTypedFailure(dispatch, Result{OK: false, Status: "failed"}, agent.Result{
		FailureClass: string(provideroutcome.ClassAmbiguousExecution),
	}, errors.New("ambiguous"))
	if !result.FallbackNeedsHuman {
		t.Fatalf("FallbackNeedsHuman = false, want true")
	}
	if result.FallbackApplied {
		t.Fatal("FallbackApplied true for needs-human class")
	}
	if result.AutoFallbackAllowed {
		t.Fatal("AutoFallbackAllowed true for ambiguous class")
	}
	if !strings.Contains(result.NextAction, "needs-human") {
		t.Fatalf("NextAction = %q", result.NextAction)
	}
}

func TestAttachTypedFailurePinBlocksAutomaticFallback(t *testing.T) {
	dispatch := &dispatchContext{
		opts: Options{
			RoutingDecisionID: "route-pinned",
			RoutePinned:       true,
		},
	}
	result := attachTypedFailure(dispatch, Result{OK: false}, agent.Result{
		FailureClass: string(provideroutcome.ClassQuotaExhausted),
	}, errors.New("quota"))
	if !result.AutoFallbackAllowed {
		t.Fatal("quota class should allow auto fallback at classification layer")
	}
	if !result.FallbackNeedsHuman {
		t.Fatal("pinned route should mark fallback needs-human at worker boundary")
	}
	if !strings.Contains(result.NextAction, "explicit pin") {
		t.Fatalf("NextAction = %q", result.NextAction)
	}
	joined := strings.Join(result.Evidence, " ")
	if !strings.Contains(joined, "route_pinned=true") {
		t.Fatalf("evidence missing pin flag: %v", result.Evidence)
	}
}

func TestAttachTypedFailureWithoutRoutingDecision(t *testing.T) {
	result := attachTypedFailure(&dispatchContext{opts: Options{}}, Result{OK: false}, agent.Result{
		FailureClass: string(provideroutcome.ClassTransientTransport),
	}, errors.New("transport"))
	if result.FallbackApplied {
		t.Fatal("applied fallback without routing decision")
	}
	if !strings.Contains(result.NextAction, "no routing decision") {
		t.Fatalf("NextAction = %q", result.NextAction)
	}
}
