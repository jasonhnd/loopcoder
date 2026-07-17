package routing

import (
	"testing"
	"time"
)

func TestPlanQuotaResetWaitFromTimesPicksEarliestFuture(t *testing.T) {
	now := time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC)
	plan := PlanQuotaResetWaitFromTimes([]time.Time{
		now.Add(-time.Hour),
		now.Add(2 * time.Hour),
		now.Add(30 * time.Minute),
		{},
	}, []string{"qsnap-a", "qsnap-b"}, now)
	if !plan.Applicable {
		t.Fatalf("plan = %#v, want applicable", plan)
	}
	want := now.Add(30 * time.Minute)
	if !plan.EarliestReset.Equal(want) {
		t.Fatalf("earliest = %s, want %s", plan.EarliestReset, want)
	}
	if len(plan.SnapshotIDs) != 2 {
		t.Fatalf("snapshots = %#v", plan.SnapshotIDs)
	}
}

func TestPlanQuotaResetWaitFromTimesUnknownWhenNoFuture(t *testing.T) {
	now := time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC)
	plan := PlanQuotaResetWaitFromTimes([]time.Time{now.Add(-time.Minute), {}}, nil, now)
	if plan.Applicable {
		t.Fatalf("plan = %#v, want not applicable", plan)
	}
}

func TestPlanQuotaResetWaitWithoutQuotaRejects(t *testing.T) {
	plan := PlanQuotaResetWait(RoutingDecision{}, time.Now())
	if plan.Applicable {
		t.Fatalf("empty decision should not plan wait: %#v", plan)
	}
}
