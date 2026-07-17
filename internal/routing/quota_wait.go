package routing

import (
	"strings"
	"time"
)

// QuotaResetWaitPlan summarizes when a no_route decision is caused by known
// quota-reset windows and the earliest policy-eligible wake time.
type QuotaResetWaitPlan struct {
	Applicable     bool      `json:"applicable"`
	EarliestReset  time.Time `json:"earliest_reset,omitempty"`
	SnapshotIDs    []string  `json:"snapshot_ids,omitempty"`
	RejectionCount int       `json:"rejection_count"`
	Reason         string    `json:"reason,omitempty"`
}

// PlanQuotaResetWait inspects a routing decision for quota-reset-incompatible
// rejections that carry known reset evidence. Unknown/stale quota never yields
// a wait plan (unknown must not become zero or unlimited).
func PlanQuotaResetWait(decision RoutingDecision, now time.Time) QuotaResetWaitPlan {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	var earliest time.Time
	var snapshots []string
	quotaRejects := 0
	for _, rejected := range decision.RejectedCandidates {
		for _, reason := range rejected.Reasons {
			if reason.Code != RejectQuotaResetIncompatible {
				continue
			}
			quotaRejects++
			for _, ref := range reason.EvidenceRecordIDs {
				if strings.TrimSpace(ref) != "" {
					snapshots = append(snapshots, ref)
				}
			}
			// Message may embed RFC3339 reset; prefer structured future if present
			// in evidence keys is not available here. Callers that have snapshots
			// should prefer PlanQuotaResetWaitFromTimes.
		}
	}
	if quotaRejects == 0 {
		return QuotaResetWaitPlan{
			Applicable: false,
			Reason:     "no quota-reset-incompatible rejections",
		}
	}
	if earliest.IsZero() {
		return QuotaResetWaitPlan{
			Applicable:     false,
			RejectionCount: quotaRejects,
			SnapshotIDs:    uniqueBounded(snapshots, 32),
			Reason:         "quota rejections present but no known reset timestamp on decision evidence",
		}
	}
	if !earliest.After(now) {
		return QuotaResetWaitPlan{
			Applicable:     false,
			RejectionCount: quotaRejects,
			SnapshotIDs:    uniqueBounded(snapshots, 32),
			EarliestReset:  earliest,
			Reason:         "earliest known reset is not in the future",
		}
	}
	return QuotaResetWaitPlan{
		Applicable:     true,
		EarliestReset:  earliest,
		SnapshotIDs:    uniqueBounded(snapshots, 32),
		RejectionCount: quotaRejects,
		Reason:         "all inspected quota blocks carry a future known reset",
	}
}

// PlanQuotaResetWaitFromTimes builds a wait plan from explicit known reset times
// (for example loaded from quota snapshots). Empty or zero times are ignored;
// if no future reset remains, the plan is not applicable.
func PlanQuotaResetWaitFromTimes(resetTimes []time.Time, snapshotIDs []string, now time.Time) QuotaResetWaitPlan {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	var earliest time.Time
	for _, reset := range resetTimes {
		if reset.IsZero() {
			continue
		}
		reset = reset.UTC()
		if !reset.After(now) {
			continue
		}
		if earliest.IsZero() || reset.Before(earliest) {
			earliest = reset
		}
	}
	if earliest.IsZero() {
		return QuotaResetWaitPlan{
			Applicable:  false,
			SnapshotIDs: uniqueBounded(snapshotIDs, 32),
			Reason:      "no future known quota reset times",
		}
	}
	return QuotaResetWaitPlan{
		Applicable:    true,
		EarliestReset: earliest,
		SnapshotIDs:   uniqueBounded(snapshotIDs, 32),
		Reason:        "earliest policy-eligible known quota reset selected",
	}
}

func uniqueBounded(values []string, max int) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}
