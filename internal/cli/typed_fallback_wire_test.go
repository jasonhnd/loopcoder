package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/provideroutcome"
	"github.com/jasonhnd/loopcoder/internal/worker"
)

func TestApplyTypedFallbackAfterDispatchSkipsSuccess(t *testing.T) {
	in := worker.Result{OK: true, FailureClass: string(provideroutcome.ClassQuotaExhausted)}
	out := applyTypedFallbackAfterDispatch(context.TODO(), t.TempDir(), worker.Options{RoutingDecisionID: "r1"}, in, nil, nil)
	if out.FallbackApplied {
		t.Fatal("applied fallback on success")
	}
}

func TestApplyTypedFallbackAfterDispatchRespectsPin(t *testing.T) {
	in := worker.Result{
		OK:                  false,
		FailureClass:        string(provideroutcome.ClassQuotaExhausted),
		AutoFallbackAllowed: true,
	}
	out := applyTypedFallbackAfterDispatch(context.TODO(), t.TempDir(), worker.Options{
		RoutingDecisionID: "r1",
		RoutePinned:       true,
	}, in, nil, nil)
	if !out.FallbackNeedsHuman {
		t.Fatal("pin should needs-human")
	}
	if !strings.Contains(out.NextAction, "pin") {
		t.Fatalf("NextAction = %q", out.NextAction)
	}
}

func TestApplyTypedFallbackAfterDispatchMissingDecision(t *testing.T) {
	in := worker.Result{
		OK:                  false,
		FailureClass:        string(provideroutcome.ClassTransientTransport),
		AutoFallbackAllowed: true,
	}
	out := applyTypedFallbackAfterDispatch(context.TODO(), t.TempDir(), worker.Options{}, in, nil, nil)
	if out.FallbackApplied {
		t.Fatal("applied without decision id")
	}
}
