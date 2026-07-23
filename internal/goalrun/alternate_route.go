package goalrun

import (
	"strings"
)

// RouteCandidate is one hard-eligibility decision-set member with the depth it
// actually supports (Effort). Used for generation-safe alternate picks after
// typed model_unavailable — never invent a depth the candidate did not carry.
type RouteCandidate struct {
	Provider     string
	Model        string
	Effort       string // required: observed/supported depth for this candidate
	HardEligible bool
	SoftExcluded bool
}

// AlternateRoutePick is a generation-safe alternate for typed model_unavailable.
// Empty Provider means no alternate (fail closed — do not re-run same claim).
type AlternateRoutePick struct {
	Provider string
	Model    string
	Depth    string
}

// PickAlternateRouteSameDepth selects another HardEligible, non-SoftExcluded
// candidate whose Effort matches reqDepth exactly. It never rewrites a
// candidate's depth to satisfy the request (no silent low→medium or model/depth
// mismatch). Zero value = fail closed.
//
// Production note: generation-safe retry (new attempt id, reconcile reservation)
// is a follow-up wiring on #1397 when model_unavailable is observed after claim.
// This helper only picks; it does not execute or re-claim.
func PickAlternateRouteSameDepth(
	cands []RouteCandidate,
	failedProvider, failedModel, reqDepth string,
) AlternateRoutePick {
	reqDepth = normalizeDepth(reqDepth)
	failedProvider = strings.TrimSpace(failedProvider)
	failedModel = strings.TrimSpace(failedModel)
	if reqDepth == "" || len(cands) == 0 {
		return AlternateRoutePick{}
	}
	for _, cv := range cands {
		if !cv.HardEligible || cv.SoftExcluded {
			continue
		}
		if strings.TrimSpace(cv.Provider) == "" || strings.TrimSpace(cv.Model) == "" {
			continue
		}
		if strings.EqualFold(cv.Provider, failedProvider) && strings.EqualFold(cv.Model, failedModel) {
			continue
		}
		candDepth := normalizeDepth(cv.Effort)
		if candDepth == "" || candDepth != reqDepth {
			continue
		}
		return AlternateRoutePick{
			Provider: cv.Provider,
			Model:    cv.Model,
			Depth:    candDepth,
		}
	}
	return AlternateRoutePick{}
}
