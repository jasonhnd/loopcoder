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
	Permission   string // required permission lane (read-only|bounded_write|…)
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
// candidate whose Effort matches reqDepth exactly and Permission matches
// reqPermission when both are set. It never rewrites a candidate's depth to
// satisfy the request (no silent low→medium or model/depth mismatch).
// Zero value = fail closed.
//
// Production wiring claims a distinct AttemptID (generation bump) with explicit
// SupersedesAttemptID — never reopens the closed failed attempt.
func PickAlternateRouteSameDepth(
	cands []RouteCandidate,
	failedProvider, failedModel, reqDepth string,
) AlternateRoutePick {
	return PickAlternateRouteSameDepthPerm(cands, failedProvider, failedModel, reqDepth, "")
}

// PickAlternateRouteSameDepthPerm is the permission-aware alternate picker.
func PickAlternateRouteSameDepthPerm(
	cands []RouteCandidate,
	failedProvider, failedModel, reqDepth, reqPermission string,
) AlternateRoutePick {
	reqDepth = normalizeDepth(reqDepth)
	reqPermission = normalizePermission(reqPermission)
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
		if reqPermission != "" {
			candPerm := normalizePermission(cv.Permission)
			// Empty observed permission never satisfies a required permission.
			if candPerm == "" || candPerm != reqPermission {
				continue
			}
		}
		return AlternateRoutePick{
			Provider: cv.Provider,
			Model:    cv.Model,
			Depth:    candDepth,
		}
	}
	return AlternateRoutePick{}
}

func normalizePermission(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "ro", "readonly", "read_only", "read-only":
		return "read-only"
	case "write", "bounded-write", "bounded_write", "boundedwrite":
		return "bounded_write"
	default:
		return p
	}
}
