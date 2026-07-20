package routing

import (
	"context"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/provideroutcome"
)

func TestApplyTypedProviderFailureNeedsHumanClasses(t *testing.T) {
	ctx := context.Background()
	for _, class := range []provideroutcome.Class{
		provideroutcome.ClassAmbiguousExecution,
		provideroutcome.ClassAuthConfigFailure,
		provideroutcome.ClassQuotaUnknown,
		provideroutcome.ClassPermissionMismatch,
		provideroutcome.ClassLocalCancellation,
	} {
		got, err := ApplyTypedProviderFailure(ctx, nil, TypedFallbackRequest{
			RoutingDecisionID: "rd-1",
			PriorCandidateID:  "cand-1",
			Class:             class,
		})
		if err != nil {
			t.Fatalf("class %s: unexpected err %v", class, err)
		}
		if got.Applied || !got.NeedsHuman || got.AutoFallbackAllowed {
			t.Fatalf("class %s result = %#v, want needs-human without apply", class, got)
		}
	}
}

func TestApplyTypedProviderFailurePinBlocksAutoFallback(t *testing.T) {
	got, err := ApplyTypedProviderFailure(context.Background(), nil, TypedFallbackRequest{
		RoutingDecisionID: "rd-1",
		PriorCandidateID:  "cand-1",
		Class:             provideroutcome.ClassQuotaExhausted,
		Pinned:            true,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Applied || !got.NeedsHuman {
		t.Fatalf("pinned result = %#v, want needs-human", got)
	}
	if got.Trigger != FallbackTriggerQuotaExhausted {
		t.Fatalf("trigger = %q", got.Trigger)
	}
}
