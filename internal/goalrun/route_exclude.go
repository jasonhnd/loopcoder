package goalrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
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

// EventSnapshot is a verified durable workflow event used for qualification.
type EventSnapshot struct {
	EventID    string `json:"event_id"`
	Kind       string `json:"kind"`
	AttemptID  string `json:"attempt_id"`
	Generation int    `json:"generation"`
	WorkItemID string `json:"work_item_id,omitempty"`
}

// UnavailableRetryProof is concrete measured evidence for generation-safe
// model_unavailable alternate. BuildUnavailableRetryEvidence must not invent
// no_duplicate_* flags from prose or retryAttemptID alone.
type UnavailableRetryProof struct {
	FailedAttemptID string
	RetryAttemptID  string
	// WorkItemID and FailedProvider bind the claimed exclude to the failed ChildOutcome.
	WorkItemID     string
	FailedProvider string
	// Per-generation structured counts from durable events (must be exact).
	FailedClaimCount     int
	RetryClaimCount      int
	FailedLaunchCount    int
	RetryLaunchCount     int
	FailedIntegrateCount int // must be 0
	RetryIntegrateCount  int // 1 if integrated, else 0
	FailedTerminalCount  int
	RetryTerminalCount   int
	// Claim closed terminals from verified terminal events / workclaim.
	FailedClaimClosed bool
	RetryClaimClosed  bool
	// FailedIntegrated must be false. RetryIntegrated only when IntegrateCommitSHA
	// matches retry attempt in IntegrateCommits (not Terminal==succeeded alone).
	FailedIntegrated   bool
	RetryIntegrated    bool
	FailedProductFiles []string
	RetryProductFiles  []string
	// Durable capacity transitions (ledger-backed), never CapacityNote prose.
	PriorTransition     workflowrun.CapacityTransition
	AlternateTransition workflowrun.CapacityTransition
	// Verified event snapshots from EventLog readback (not string parsing alone).
	ModelUnavailableEvent EventSnapshot
	ClaimEvent            EventSnapshot
	RerouteEvent          EventSnapshot
	LaunchEvent           EventSnapshot
	RetryTerminalEvent    EventSnapshot
	FailedTerminalEvent   EventSnapshot
	// IntegrateEvent is set only when retry integrated (EventID included in evidence).
	IntegrateEvent EventSnapshot
}

// BuildUnavailableRetryEvidence derives canary UnavailableRetry from measured
// unclaimed unavailability excludes only (no generation-safe proof).
func BuildUnavailableRetryEvidence(excludes []RouteExclude, retryAttemptID string) *artifactqual.CanaryUnavailableRetry {
	return BuildUnavailableRetryEvidenceWithProof(excludes, retryAttemptID, nil)
}

// BuildUnavailableRetryEvidenceWithProof is the evidence-bound entry point.
// Claimed model_unavailable takes absolute precedence: if ANY claimed MU exclude
// exists, unclaimed exhausted/rate_limited/unavailable must not satisfy
// unavailable_retry. Exactly one claimed MU exclude is required, or fail.
// Unclaimed path is allowed only when there is no claimed model_unavailable.
func BuildUnavailableRetryEvidenceWithProof(excludes []RouteExclude, retryAttemptID string, proof *UnavailableRetryProof) *artifactqual.CanaryUnavailableRetry {
	if len(excludes) == 0 {
		return nil
	}
	var claimedMU []*RouteExclude
	for i := range excludes {
		e := &excludes[i]
		if !e.Claimed {
			continue
		}
		// Exact durable reason — no EqualFold/TrimSpace normalize.
		if e.Reason != "model_unavailable" {
			continue
		}
		if e.Provider == "" {
			continue
		}
		claimedMU = append(claimedMU, e)
	}

	// Claimed MU present → only claimed path; unclaimed must not satisfy.
	if len(claimedMU) > 0 {
		if len(claimedMU) != 1 {
			return nil // exactly one claimed exclude required
		}
		if proof == nil || !proof.ValidForClaimedModelUnavailable() {
			return nil
		}
		pick := claimedMU[0]
		// Bind exclude ChildID/Provider exactly to failed ChildOutcome/proof.
		if wi := proof.WorkItemID; wi != "" && pick.ChildID != wi {
			return nil
		}
		if fp := proof.FailedProvider; fp != "" && pick.Provider != fp {
			return nil
		}
		noDupClaim := proof.NoDuplicateClaim()
		noDupFiles := proof.NoDuplicateFiles()
		noDupCap := proof.NoDoubleCapacity()
		if !noDupClaim || !noDupFiles || !noDupCap {
			return nil
		}
		ids := proof.VerifiedEventIDs()
		if len(ids) == 0 {
			return nil
		}
		raw, _ := json.Marshal(excludes)
		sum := sha256.Sum256(raw)
		ref := "event_ids=" + strings.Join(ids, ",") + ";route_exclude_set#sha256:" + hex.EncodeToString(sum[:12])
		retryAttemptID = firstNonEmptyStr(proof.RetryAttemptID, retryAttemptID)
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

	// Unclaimed-only path (no claimed model_unavailable present).
	var pick *RouteExclude
	for i := range excludes {
		e := &excludes[i]
		if e.Claimed {
			continue
		}
		if e.Provider == "" || e.Reason == "" {
			continue
		}
		if !isUnavailableRetryReason(e.Reason) {
			continue
		}
		pick = e
		break
	}
	if pick == nil {
		return nil
	}
	raw, _ := json.Marshal(excludes)
	sum := sha256.Sum256(raw)
	ref := "route_exclude_set#sha256:" + hex.EncodeToString(sum[:12])
	return &artifactqual.CanaryUnavailableRetry{
		ExcludedProvider: pick.Provider,
		ExcludedReason:   pick.Reason,
		RetryAttemptID:   retryAttemptID,
		NoDuplicateClaim: !anyClaimed(excludes, pick.Provider),
		NoDuplicateFiles: true,
		NoDoubleCapacity: true,
		EvidenceRef:      ref + ";child=" + pick.ChildID + ";msg=" + truncateMsg(pick.Message, 80),
	}
}

// ValidForClaimedModelUnavailable reports enough concrete measured fields.
// Durable attempt identity is byte-exact everywhere.
func (p *UnavailableRetryProof) ValidForClaimedModelUnavailable() bool {
	if p == nil {
		return false
	}
	if p.FailedAttemptID == "" || p.RetryAttemptID == "" {
		return false
	}
	if p.FailedAttemptID == p.RetryAttemptID {
		return false
	}
	if p.WorkItemID == "" || p.FailedProvider == "" {
		return false
	}
	if p.FailedClaimCount != 1 || p.RetryClaimCount != 1 {
		return false
	}
	if p.FailedLaunchCount != 1 || p.RetryLaunchCount != 1 {
		return false
	}
	if p.FailedIntegrateCount != 0 {
		return false
	}
	if p.FailedTerminalCount != 1 || p.RetryTerminalCount != 1 {
		return false
	}
	if !p.FailedClaimClosed || !p.RetryClaimClosed {
		return false
	}
	if p.FailedIntegrated {
		return false
	}
	if p.RetryIntegrated && p.RetryIntegrateCount != 1 {
		return false
	}
	if !p.RetryIntegrated && p.RetryIntegrateCount != 0 {
		return false
	}
	if !validEventSnap(p.ModelUnavailableEvent, "model_unavailable", p.FailedAttemptID) {
		return false
	}
	if !validEventSnap(p.FailedTerminalEvent, "terminal", p.FailedAttemptID) {
		return false
	}
	if !validEventSnap(p.ClaimEvent, "claim", p.RetryAttemptID) {
		return false
	}
	if !validEventSnap(p.RerouteEvent, "reroute", p.RetryAttemptID) {
		return false
	}
	if !validEventSnap(p.LaunchEvent, "launch", p.RetryAttemptID) {
		return false
	}
	if !validEventSnap(p.RetryTerminalEvent, "terminal", p.RetryAttemptID) {
		return false
	}
	if p.RetryIntegrated {
		if !validEventSnap(p.IntegrateEvent, "integrate", p.RetryAttemptID) {
			return false
		}
	}
	// Capacity: exactly two transitions with strict state/actual/source/identity.
	if p.PriorTransition.Role != "prior" ||
		p.PriorTransition.AttemptID != p.FailedAttemptID {
		return false
	}
	if p.AlternateTransition.Role != "alternate" ||
		p.AlternateTransition.AttemptID != p.RetryAttemptID {
		return false
	}
	if !validCapacityTransition(p.PriorTransition) || !validCapacityTransition(p.AlternateTransition) {
		return false
	}
	if p.PriorTransition.ReservationID == "" ||
		p.AlternateTransition.ReservationID == "" ||
		p.PriorTransition.ReservationID == p.AlternateTransition.ReservationID {
		return false
	}
	return true
}

func validCapacityTransition(tr workflowrun.CapacityTransition) bool {
	st := tr.State
	if tr.Provider == "" || tr.Model == "" || tr.Depth == "" || tr.AccountRef == "" ||
		tr.WindowKind == "" || tr.ReservationID == "" {
		return false
	}
	switch st {
	case "reconciled":
		return tr.Actual != nil && tr.Source != ""
	case "released":
		return tr.Actual == nil && tr.Source == ""
	default:
		return false
	}
}

func validEventSnap(s EventSnapshot, kind, attempt string) bool {
	if s.EventID == "" {
		return false
	}
	if s.Kind != kind {
		return false
	}
	if s.AttemptID != attempt {
		return false
	}
	return true
}

// VerifiedEventIDs returns nonempty event IDs from verified snapshots.
// Includes FailedTerminalEvent and IntegrateEvent when present.
func (p *UnavailableRetryProof) VerifiedEventIDs() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, s := range []EventSnapshot{
		p.ModelUnavailableEvent, p.FailedTerminalEvent,
		p.ClaimEvent, p.RerouteEvent, p.LaunchEvent, p.RetryTerminalEvent,
		p.IntegrateEvent,
	} {
		if id := s.EventID; id != "" {
			out = append(out, id)
		}
	}
	return out
}

// NoDuplicateClaim: exact one claim+launch+terminal per generation; no second accepted terminal.
func (p *UnavailableRetryProof) NoDuplicateClaim() bool {
	if p == nil {
		return false
	}
	if p.FailedClaimCount != 1 || p.RetryClaimCount != 1 {
		return false
	}
	if p.FailedLaunchCount != 1 || p.RetryLaunchCount != 1 {
		return false
	}
	if p.FailedTerminalCount != 1 || p.RetryTerminalCount != 1 {
		return false
	}
	if p.FailedIntegrateCount != 0 {
		return false
	}
	if p.RetryIntegrateCount > 1 {
		return false
	}
	return p.FailedClaimClosed && p.RetryClaimClosed &&
		p.FailedAttemptID != p.RetryAttemptID
}

// NoDuplicateFiles: failed never integrated; product paths do not overlap when both present.
func (p *UnavailableRetryProof) NoDuplicateFiles() bool {
	if p == nil || p.FailedIntegrated || p.FailedIntegrateCount != 0 {
		return false
	}
	if len(p.FailedProductFiles) > 0 && len(p.RetryProductFiles) > 0 {
		seen := map[string]bool{}
		for _, f := range p.FailedProductFiles {
			seen[f] = true
		}
		for _, f := range p.RetryProductFiles {
			if seen[f] {
				return false
			}
		}
	}
	return true
}

// NoDoubleCapacity from durable final ledger transitions only (no reserved alternate).
func (p *UnavailableRetryProof) NoDoubleCapacity() bool {
	if p == nil {
		return false
	}
	if !validCapacityTransition(p.PriorTransition) || !validCapacityTransition(p.AlternateTransition) {
		return false
	}
	return p.PriorTransition.Role == "prior" && p.AlternateTransition.Role == "alternate" &&
		p.PriorTransition.AttemptID != "" &&
		p.AlternateTransition.AttemptID != "" &&
		p.PriorTransition.AttemptID != p.AlternateTransition.AttemptID &&
		p.PriorTransition.ReservationID != p.AlternateTransition.ReservationID
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isUnavailableRetryReason(reason string) bool {
	switch reason {
	case "exhausted", "rate_limited", "unavailable",
		"model_unavailable", "capacity_refused", "permission":
		return true
	default:
		return false
	}
}

func anyClaimed(excludes []RouteExclude, provider string) bool {
	for _, e := range excludes {
		if e.Provider == provider && e.Claimed {
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
		return "soft_excluded"
	default:
		return "unavailable"
	}
}

// SoftExcludedCandidate is one decision-set candidate that was hard-eligible
// but soft-excluded (no work claim).
type SoftExcludedCandidate struct {
	Provider     string
	Model        string
	HardEligible bool
	SoftExcluded bool
}

// SoftExcludedEligibleExcludes derives Claimed=false RouteExclude rows for
// hard-eligible soft-excluded candidates other than the winner.
func SoftExcludedEligibleExcludes(childID, winnerProvider string, cands []SoftExcludedCandidate) []RouteExclude {
	return hardEligibleNonWinnerExcludes(childID, winnerProvider, cands, true)
}

// HardEligibleNonWinnerExcludes derives Claimed=false RouteExclude rows for
// hard-eligible decision-set candidates other than the winner.
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
