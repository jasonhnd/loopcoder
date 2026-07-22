package goalpr

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// FinalizeRequest refreshes PR evidence from live GitHub checks (product path).
// Never invents required_checks_green=true without host.ListChecks observation.
type FinalizeRequest struct {
	PRNumber               int
	HeadOID                string // must match PR head when host supports it
	IndependentVerifier    string
	VerifierEvidenceRef    string // must be sha256:... or bound digest; no pending-live
	VerifierAttemptID      string
	VerifierProvider       string
	RequiredMeaningfulOnly bool // when true, only product-tests/product-build count as green
	Wait                   time.Duration
	PollEvery              time.Duration
	Host                   Host
	Now                    func() time.Time
}

// FinalizePREvidence queries GitHub for checks and returns an updated Result
// slice of fields. Fail closed on pending-live verifier or empty checks when
// waiting elapsed.
func FinalizePREvidence(ctx context.Context, base Result, req FinalizeRequest) (Result, error) {
	out := base
	if req.PRNumber <= 0 && out.Number <= 0 {
		return out, fmt.Errorf("%w: pr number required", ErrInvalid)
	}
	num := req.PRNumber
	if num <= 0 {
		num = out.Number
	}
	if strings.Contains(strings.ToLower(req.VerifierEvidenceRef), "pending") ||
		strings.Contains(strings.ToLower(out.VerifierEvidenceRef), "pending") {
		return out, fmt.Errorf("%w: refuse pending-live verifier evidence", ErrNotReady)
	}
	// Bind verifier to same head SHA when provided.
	verRef := firstNonEmpty(req.VerifierEvidenceRef, out.VerifierEvidenceRef)
	head := firstNonEmpty(req.HeadOID, out.HeadOID)
	if head != "" && verRef != "" && !strings.Contains(verRef, head) && strings.HasPrefix(verRef, "sha256:") {
		verRef = verRef + "@head:" + head
	}
	if req.VerifierProvider != "" {
		out.IndependentVerifier = req.VerifierProvider
	} else if req.IndependentVerifier != "" {
		out.IndependentVerifier = req.IndependentVerifier
	}
	out.VerifierEvidenceRef = verRef
	if req.VerifierAttemptID != "" {
		out.Events = append(out.Events, "verifier.attempt="+req.VerifierAttemptID)
	}
	if head != "" {
		out.HeadOID = head
		out.Events = append(out.Events, "verifier.head="+head)
	}

	host := req.Host
	if host == nil {
		return out, fmt.Errorf("%w: host required for finalize", ErrInvalid)
	}
	wait := req.Wait
	every := req.PollEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	deadline := time.Time{}
	if wait > 0 {
		nowFn := req.Now
		if nowFn == nil {
			nowFn = time.Now
		}
		deadline = nowFn().Add(wait)
	}

	var lastNames []string
	var lastGreen bool
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		names, green, err := host.ListChecks(ctx, num)
		if err == nil {
			lastNames, lastGreen = names, green
			if req.RequiredMeaningfulOnly {
				lastNames, lastGreen = filterMeaningfulGreen(names, green)
			}
			out.RequiredChecks = lastNames
			out.RequiredChecksGreen = lastGreen && len(lastNames) > 0
			out.Events = append(out.Events, fmt.Sprintf("finalize.checks count=%d green=%v", len(lastNames), out.RequiredChecksGreen))
			if out.RequiredChecksGreen {
				out.OK = out.URL != "" && out.CreatedByLoopCoder && out.HumanMergeGate &&
					out.IndependentVerifier != "" && out.VerifierEvidenceRef != "" &&
					!strings.Contains(strings.ToLower(out.VerifierEvidenceRef), "pending")
				out.Message = fmt.Sprintf("PR %s finalized; checks green=%v head=%s verifier=%s",
					out.URL, out.RequiredChecksGreen, out.HeadOID, out.IndependentVerifier)
				return out, nil
			}
		}
		if wait <= 0 || deadline.IsZero() {
			break
		}
		nowFn := req.Now
		if nowFn == nil {
			nowFn = time.Now
		}
		if nowFn().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(every):
		}
	}
	out.RequiredChecks = lastNames
	out.RequiredChecksGreen = lastGreen && len(lastNames) > 0
	// Honest: not green after wait is not OK for canary gate.
	out.OK = false
	out.Message = fmt.Sprintf("PR %s finalize: checks not green (count=%d green=%v)", out.URL, len(lastNames), out.RequiredChecksGreen)
	return out, fmt.Errorf("%w: required checks not green", ErrNotReady)
}

func filterMeaningfulGreen(names []string, allGreen bool) ([]string, bool) {
	var out []string
	for _, n := range names {
		if IsMeaningfulCheck(n) {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return out, false
	}
	// When host reported allGreen for full set, meaningful subset is green too
	// if present. If not allGreen, we cannot claim meaningful green without
	// per-check conclusions — fail closed.
	if !allGreen {
		return out, false
	}
	// Require both product-tests and product-build when possible.
	hasTests, hasBuild := false, false
	for _, n := range out {
		ln := strings.ToLower(n)
		if strings.Contains(ln, "product-test") {
			hasTests = true
		}
		if strings.Contains(ln, "product-build") {
			hasBuild = true
		}
	}
	return out, hasTests && hasBuild
}
