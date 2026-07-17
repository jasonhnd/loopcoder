package waitstate

import (
	"strings"
	"time"
)

// QuotaResetWaitPlan summarizes a provider-free local wait for a known reset.
type QuotaResetWaitPlan struct {
	Applicable     bool      `json:"applicable"`
	EarliestReset  time.Time `json:"earliest_reset,omitempty"`
	SnapshotIDs    []string  `json:"snapshot_ids,omitempty"`
	RejectionCount int       `json:"rejection_count"`
	Reason         string    `json:"reason,omitempty"`
}

// PlanQuotaResetWaitFromTimes builds a wait plan from explicit known reset times.
// Empty or past times are ignored; unknown capacity is never invented.
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
