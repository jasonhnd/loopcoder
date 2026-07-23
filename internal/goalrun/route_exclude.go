package goalrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
)

// RouteExclude is one measured hard-exclude or soft-exclude from the live
// candidate set. Pure exclusion must have Claimed=false (no workclaim).
type RouteExclude struct {
	ChildID      string `json:"child_id"`
	Provider     string `json:"provider"`
	Reason       string `json:"reason"` // exhausted|stale|rate_limited|unavailable|soft_excluded|capacity_refused|permission
	HardEligible bool   `json:"hard_eligible"`
	SoftExcluded bool   `json:"soft_excluded"`
	Claimed      bool   `json:"claimed"` // must be false for exclude-without-work
	Message      string `json:"message,omitempty"`
}

// UnavailableRetryProof is concrete measured evidence for generation-safe
// model_unavailable alternate. BuildUnavailableRetryEvidence must not invent
// no_duplicate_* flags from prose or retryAttemptID alone.
type UnavailableRetryProof struct {
	// FailedAttemptID / RetryAttemptID are distinct durable attempt identities.
	FailedAttemptID string
	RetryAttemptID  string
	// FailedClaimClosed / RetryClaimClosed from workclaim/workflow terminals.
	FailedClaimClosed bool
	RetryClaimClosed  bool
	// FailedIntegrated must be false (failed attempt never product-integrated).
	FailedIntegrated bool
	// RetryIntegrated when the alternate succeeded and was integrated.
	RetryIntegrated bool
	// FailedProductFiles / RetryProductFiles for file-duplication checks.
	FailedProductFiles []string
	RetryProductFiles  []string
	// PriorCapacityState: released|reconciled (not reserved/live).
	PriorCapacityState string
	// AltCapacityState: reserved|reconciled|released.
	AltCapacityState string
	// EventIDs nonempty persisted workflow event ids for model_unavailable+reroute.
	EventIDs []string
}

// BuildUnavailableRetryEvidence derives canary UnavailableRetry from measured
// *unavailability* route excludes and optional generation-safe proof.
// Returns nil when evidence is insufficient (never invents no-duplicate flags).
//
// eligible_not_chosen is multi-provider diversity measurement, NOT unavailability —
// it must never satisfy unavailable_retry scorecard metrics.
//
// Claimed model_unavailable with only prose/event_ref text is insufficient —
// UnavailableRetryProof with concrete claim/ledger/event IDs is required.
func BuildUnavailableRetryEvidence(excludes []RouteExclude, retryAttemptID string) *artifactqual.CanaryUnavailableRetry {
	return BuildUnavailableRetryEvidenceWithProof(excludes, retryAttemptID, nil)
}

// BuildUnavailableRetryEvidenceWithProof is the evidence-bound entry point.
func BuildUnavailableRetryEvidenceWithProof(excludes []RouteExclude, retryAttemptID string, proof *UnavailableRetryProof) *artifactqual.CanaryUnavailableRetry {
	if len(excludes) == 0 {
		return nil
	}
	retryAttemptID = strings.TrimSpace(retryAttemptID)
	// Prefer an exclude that never claimed work and is a real unavailability reason.
	var pick *RouteExclude
	for i := range excludes {
		e := &excludes[i]
		if e.Claimed {
			continue
		}
		if strings.TrimSpace(e.Provider) == "" || strings.TrimSpace(e.Reason) == "" {
			continue
		}
		if !isUnavailableRetryReason(e.Reason) {
			continue
		}
		pick = e
		break
	}
	// Claimed model_unavailable requires concrete proof — never prose-only.
	if pick == nil {
		if proof == nil || !proof.ValidForClaimedModelUnavailable() {
			return nil
		}
		for i := range excludes {
			e := &excludes[i]
			if !e.Claimed {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(e.Reason), "model_unavailable") {
				continue
			}
			if strings.TrimSpace(e.Provider) == "" {
				continue
			}
			pick = e
			break
		}
		if pick == nil {
			return nil
		}
		// Prefer proof retry id.
		if strings.TrimSpace(proof.RetryAttemptID) != "" {
			retryAttemptID = strings.TrimSpace(proof.RetryAttemptID)
		}
	}
	if pick == nil {
		return nil
	}
	raw, _ := json.Marshal(excludes)
	sum := sha256.Sum256(raw)
	ref := "route_exclude_set#sha256:" + hex.EncodeToString(sum[:12])

	noDupClaim := !anyClaimed(excludes, pick.Provider)
	noDupFiles := true
	noDupCap := true
	if pick.Claimed {
		// Derive only from proof — never invent true from retryAttemptID.
		if proof == nil || !proof.ValidForClaimedModelUnavailable() {
			return nil
		}
		noDupClaim = proof.NoDuplicateClaim()
		noDupFiles = proof.NoDuplicateFiles()
		noDupCap = proof.NoDoubleCapacity()
		if !noDupClaim || !noDupFiles || !noDupCap {
			// Incomplete/conflicting proof — fail closed (do not green qualification).
			return nil
		}
		if len(proof.EventIDs) > 0 {
			ref = "event_ids=" + strings.Join(proof.EventIDs, ",") + ";" + ref
		}
		retryAttemptID = firstNonEmptyStr(proof.RetryAttemptID, retryAttemptID)
	}

	return &artifactqual.CanaryUnavailableRetry{
		ExcludedProvider: pick.Provider,
		ExcludedReason:   pick.Reason,
		RetryAttemptID:   retryAttemptID,
		NoDuplicateClaim: noDupClaim,
		NoDuplicateFiles: noDupFiles,
		NoDoubleCapacity: noDupCap,
		EvidenceRef:      ref + ";child=" + pick.ChildID + ";msg=" + truncateMsg(pick.Message, 80),
	}
}

// ValidForClaimedModelUnavailable reports enough concrete measured fields.
func (p *UnavailableRetryProof) ValidForClaimedModelUnavailable() bool {
	if p == nil {
		return false
	}
	if strings.TrimSpace(p.FailedAttemptID) == "" || strings.TrimSpace(p.RetryAttemptID) == "" {
		return false
	}
	if strings.EqualFold(p.FailedAttemptID, p.RetryAttemptID) {
		return false
	}
	if !p.FailedClaimClosed || !p.RetryClaimClosed {
		return false
	}
	if p.FailedIntegrated {
		return false
	}
	if len(p.EventIDs) == 0 {
		return false
	}
	for _, id := range p.EventIDs {
		if strings.TrimSpace(id) == "" {
			return false
		}
	}
	prior := strings.ToLower(strings.TrimSpace(p.PriorCapacityState))
	if prior != "released" && prior != "reconciled" {
		return false
	}
	alt := strings.ToLower(strings.TrimSpace(p.AltCapacityState))
	if alt != "reserved" && alt != "reconciled" && alt != "released" {
		return false
	}
	return true
}

// NoDuplicateClaim: distinct closed attempts, failed not integrated as success claim reuse.
func (p *UnavailableRetryProof) NoDuplicateClaim() bool {
	if p == nil {
		return false
	}
	return p.FailedClaimClosed && p.RetryClaimClosed &&
		!strings.EqualFold(p.FailedAttemptID, p.RetryAttemptID)
}

// NoDuplicateFiles: failed attempt has no product files that were integrated;
// retry may have product files only when not overlapping a failed integrate.
func (p *UnavailableRetryProof) NoDuplicateFiles() bool {
	if p == nil {
		return false
	}
	if p.FailedIntegrated {
		return false
	}
	// Failed product files present without integrate is OK (worktree only);
	// must not share paths with retry product files when both non-empty.
	if len(p.FailedProductFiles) > 0 && len(p.RetryProductFiles) > 0 {
		seen := map[string]bool{}
		for _, f := range p.FailedProductFiles {
			seen[strings.TrimSpace(f)] = true
		}
		for _, f := range p.RetryProductFiles {
			if seen[strings.TrimSpace(f)] {
				return false
			}
		}
	}
	return true
}

// NoDoubleCapacity: prior released/reconciled and alternate reserved/reconciled/released.
func (p *UnavailableRetryProof) NoDoubleCapacity() bool {
	if p == nil {
		return false
	}
	prior := strings.ToLower(strings.TrimSpace(p.PriorCapacityState))
	alt := strings.ToLower(strings.TrimSpace(p.AltCapacityState))
	okPrior := prior == "released" || prior == "reconciled"
	okAlt := alt == "reserved" || alt == "reconciled" || alt == "released"
	return okPrior && okAlt
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// isUnavailableRetryReason is the closed set accepted by canary unavailable_retry.
//
// Rejected (not unavailability):
//   - eligible_not_chosen / not_chosen — multi-provider diversity only
//   - soft_excluded — soft ranking/policy, not hard unavailability
//   - stale — freshness alone is not a typed unavailable observation
func isUnavailableRetryReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "exhausted", "rate_limited", "unavailable",
		"model_unavailable", "capacity_refused", "permission":
		return true
	default:
		return false
	}
}

func anyClaimed(excludes []RouteExclude, provider string) bool {
	for _, e := range excludes {
		if strings.EqualFold(e.Provider, provider) && e.Claimed {
			return true
		}
	}
	return false
}

func truncateMsg(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ClassifyExcludeReason maps route notes / outcomes to canary reason tokens.
func ClassifyExcludeReason(note, terminal, routeReason string) string {
	blob := strings.ToLower(note + " " + terminal + " " + routeReason)
	switch {
	case strings.Contains(blob, "exhaust"):
		return "exhausted"
	case strings.Contains(blob, "stale"):
		return "stale"
	case strings.Contains(blob, "rate") && strings.Contains(blob, "limit"):
		return "rate_limited"
	case strings.Contains(blob, "capacity_refused") || strings.Contains(blob, "refused"):
		return "exhausted"
	case strings.Contains(blob, "permission"):
		return "unavailable"
	case strings.Contains(blob, "soft"):
		// Hard-eligible soft-excluded candidates are real measured excludes
		// (e.g. reserve.breach), not stale inventory.
		return "soft_excluded"
	default:
		return "unavailable"
	}
}

// SoftExcludedEligibleExclude is one decision-set candidate that was hard-eligible
// but soft-excluded (no work claim). Used after a successful route to record
// unavailable_retry evidence without inventing flags.
type SoftExcludedCandidate struct {
	Provider     string
	Model        string
	HardEligible bool
	SoftExcluded bool
}

// SoftExcludedEligibleExcludes derives Claimed=false RouteExclude rows for
// hard-eligible soft-excluded candidates other than the winner. Never invents
// excludes when the decision set is empty.
func SoftExcludedEligibleExcludes(childID, winnerProvider string, cands []SoftExcludedCandidate) []RouteExclude {
	return hardEligibleNonWinnerExcludes(childID, winnerProvider, cands, true)
}

// HardEligibleNonWinnerExcludes derives Claimed=false RouteExclude rows for
// hard-eligible decision-set candidates other than the winner:
//   - SoftExcluded → reason soft_excluded
//   - otherwise → reason eligible_not_chosen (measured multi-provider exclusion
//     without a work claim; never invents SoftExcluded=true)
//
// Used after a successful route so unavailable_retry has real exclude evidence
// even when no provider is currently soft-excluded by quota.
func HardEligibleNonWinnerExcludes(childID, winnerProvider string, cands []SoftExcludedCandidate) []RouteExclude {
	return hardEligibleNonWinnerExcludes(childID, winnerProvider, cands, false)
}

func hardEligibleNonWinnerExcludes(childID, winnerProvider string, cands []SoftExcludedCandidate, softOnly bool) []RouteExclude {
	childID = strings.TrimSpace(childID)
	winnerProvider = strings.TrimSpace(winnerProvider)
	if childID == "" || len(cands) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []RouteExclude
	for _, cv := range cands {
		p := strings.TrimSpace(cv.Provider)
		if p == "" || !cv.HardEligible {
			continue
		}
		if softOnly && !cv.SoftExcluded {
			continue
		}
		if winnerProvider != "" && strings.EqualFold(p, winnerProvider) {
			continue
		}
		key := strings.ToLower(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		reason := "eligible_not_chosen"
		msg := "hard-eligible non-winner candidate"
		if cv.SoftExcluded {
			reason = "soft_excluded"
			msg = "hard-eligible soft-excluded candidate"
		}
		if strings.TrimSpace(cv.Model) != "" {
			msg += " model=" + strings.TrimSpace(cv.Model)
		}
		if winnerProvider != "" {
			msg += "; winner=" + winnerProvider
		}
		out = append(out, RouteExclude{
			ChildID: childID, Provider: p, Reason: reason,
			HardEligible: true, SoftExcluded: cv.SoftExcluded, Claimed: false, Message: msg,
		})
	}
	return out
}

// FormatExcludeEvidence is a stable single-line for event logs.
func FormatExcludeEvidence(e RouteExclude) string {
	return fmt.Sprintf("exclude provider=%s reason=%s child=%s claimed=%v hard=%v soft=%v",
		e.Provider, e.Reason, e.ChildID, e.Claimed, e.HardEligible, e.SoftExcluded)
}
