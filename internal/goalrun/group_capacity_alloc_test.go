package goalrun

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
)

func TestAllocateGroupDelta_TokenWeightedSumExact(t *testing.T) {
	// 3 children, Before=.90 After=.84 => aggregate=.06 — never .18.
	members := []capacityGroupMember{
		{childID: "a", tokens: 100, reserved: 0.05, launched: true},
		{childID: "b", tokens: 200, reserved: 0.05, launched: true},
		{childID: "c", tokens: 300, reserved: 0.05, launched: true},
	}
	const agg = 0.06
	shares, method := allocateGroupDelta(members, agg)
	if method != "estimated_group_delta_token_weighted" {
		t.Fatalf("method=%s", method)
	}
	var sum float64
	for _, s := range shares {
		sum += s
		if s < 0 {
			t.Fatalf("negative share %v", shares)
		}
	}
	if math.Abs(sum-agg) > 1e-12 {
		t.Fatalf("sum=%v want %v shares=%v", sum, agg, shares)
	}
	// Proportional: 100:200:300 = 1:2:3 of 0.06 => 0.01, 0.02, 0.03
	if math.Abs(shares[0]-0.01) > 1e-12 || math.Abs(shares[1]-0.02) > 1e-12 || math.Abs(shares[2]-0.03) > 1e-12 {
		t.Fatalf("shares=%v want 0.01,0.02,0.03", shares)
	}
}

func TestAllocateGroupDelta_ReservationWeightedWhenNoTokens(t *testing.T) {
	members := []capacityGroupMember{
		{childID: "a", tokens: 0, reserved: 0.02, launched: true},
		{childID: "b", tokens: 0, reserved: 0.04, launched: true},
		{childID: "c", tokens: 0, reserved: 0.06, launched: true},
	}
	const agg = 0.06
	shares, method := allocateGroupDelta(members, agg)
	if method != "estimated_group_delta_reservation_weighted" {
		t.Fatalf("method=%s", method)
	}
	var sum float64
	for _, s := range shares {
		sum += s
	}
	if math.Abs(sum-agg) > 1e-12 {
		t.Fatalf("sum=%v want %v", sum, agg)
	}
	// 2:4:6 of 0.06 => 0.01, 0.02, 0.03
	if math.Abs(shares[0]-0.01) > 1e-12 || math.Abs(shares[1]-0.02) > 1e-12 || math.Abs(shares[2]-0.03) > 1e-12 {
		t.Fatalf("shares=%v", shares)
	}
}

func TestAllocateGroupDelta_ZeroAggregate(t *testing.T) {
	members := []capacityGroupMember{{childID: "a", tokens: 10, launched: true}}
	shares, method := allocateGroupDelta(members, 0)
	if method != "estimated_group_delta_zero" {
		t.Fatalf("%s", method)
	}
	if shares[0] != 0 {
		t.Fatalf("%v", shares)
	}
}

// Three launched children on one provider×account×install×window must share a
// single ObserveAfter delta (Before−After once), never ×N of the aggregate.
// Actual is estimated group-window allocation — never token/argv/score/prose.
func TestReconcileCapacityGroups_SingleWindowDeltaNotMultiplied(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "cap-group.json")
	led, err := capacityledger.OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	const (
		acc  = "acct-group-shared"
		inst = "pinst_group_shared"
		win  = "five_hour"
		prov = "codex"
	)
	// Before window at reserve: 90% remaining.
	beforeRem := 0.90
	snapBefore, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: prov, AccountRef: acc, InstallRef: inst,
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: win, Unit: capacitysnapshot.UnitPercentage,
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: beforeRem * 100, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				Source: "codexbar", CapturedAt: now.Add(-2 * time.Minute),
			}},
		}),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// After window (run-final): 84% remaining → aggregate delta 0.06 once.
	afterRem := 0.84
	snapAfter, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: prov, AccountRef: acc, InstallRef: inst,
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: win, Unit: capacitysnapshot.UnitPercentage,
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: afterRem * 100, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				Source: "codexbar", CapturedAt: now,
			}},
		}),
	}, now)
	if err != nil {
		t.Fatal(err)
	}

	ids := []string{"wi_a", "wi_b", "wi_c"}
	tokens := []int64{100, 200, 300}
	holds := map[string]capacityHold{}
	children := make([]ChildReport, 0, 3)
	for i, id := range ids {
		att := "att-" + id + "-g0"
		e, rerr := led.Reserve(capacityledger.ReserveInput{
			ProjectID: "proj-g", RunID: "run-g", AttemptID: att,
			PlanDigest: "sha256:plan", GraphDigest: "sha256:graph", TaskClass: "tera",
			ChildContractDigest: "sha256:ccd-" + id,
			Provider:            prov, Model: "gpt-5.5", Depth: "medium",
			AccountRef: acc, InstallRef: inst, WindowKind: win,
			Snapshot: &snapBefore, DemandFraction: 0.05, DemandConfidence: quotapolicy.EvidenceEstimated,
		})
		if rerr != nil {
			t.Fatalf("reserve %s: %v", id, rerr)
		}
		if e.State != "reserved" || e.Before < 0.89 || e.Before > 0.91 {
			t.Fatalf("reserve entry %+v", e)
		}
		// Before must come from observed window source, not invent.
		if e.BeforeSource != "codexbar" {
			t.Fatalf("BeforeSource=%q want codexbar", e.BeforeSource)
		}
		holds[att] = capacityHold{projectID: "proj-g", runID: "run-g", attemptID: att}
		children = append(children, ChildReport{
			ChildID: id, Provider: prov, Model: "gpt-5.5", Depth: "medium",
			Terminal: "succeeded", AttemptID: att, TokenTotal: tokens[i],
			AccountRef: acc, InstallRef: inst, WindowKind: win,
		})
	}

	reconcileCapacityGroups(children, led, holds, &snapAfter, false)

	const wantAgg = 0.06 // 0.90 − 0.84 once — never 0.18
	var sumActual float64
	for i, c := range children {
		if c.CapacityAfter == nil || *c.CapacityAfter < 0.83 || *c.CapacityAfter > 0.85 {
			t.Fatalf("child %s after=%v want ~0.84", c.ChildID, c.CapacityAfter)
		}
		if c.CapacityAfterState != "observed" {
			t.Fatalf("child %s AfterState=%q want observed", c.ChildID, c.CapacityAfterState)
		}
		if c.CapacityAfterSource != "codexbar" || c.CapacityAfterFreshness != "fresh" {
			t.Fatalf("child %s after src/fresh=%q/%q", c.ChildID, c.CapacityAfterSource, c.CapacityAfterFreshness)
		}
		if c.CapacityActual == nil {
			t.Fatalf("child %s missing actual", c.ChildID)
		}
		sumActual += *c.CapacityActual
		// Actual is group-window estimate, not token count or reservation alone.
		if c.ActualSource == "" || c.ActualSource == "unknown" {
			t.Fatalf("child %s ActualSource empty/unknown: %q", c.ChildID, c.ActualSource)
		}
		if c.CapacityActualConfidence != string(quotapolicy.EvidenceEstimated) {
			t.Fatalf("child %s conf=%q want estimated", c.ChildID, c.CapacityActualConfidence)
		}
		// Token totals must not equal actual (masquerade check).
		if math.Abs(*c.CapacityActual-float64(tokens[i])) < 1e-9 {
			t.Fatalf("actual must not equal raw token total for %s", c.ChildID)
		}
		// Shared group observe id (one observation).
		if c.CapacityGroupObserveID == "" || c.CapacityGroupID == "" {
			t.Fatalf("child %s missing group evidence ids", c.ChildID)
		}
		if i > 0 && c.CapacityGroupObserveID != children[0].CapacityGroupObserveID {
			t.Fatalf("group observe id diverged: %q vs %q", children[0].CapacityGroupObserveID, c.CapacityGroupObserveID)
		}
	}
	if math.Abs(sumActual-wantAgg) > 1e-9 {
		t.Fatalf("sum Actual=%v want single aggregate %v (got ×N?)", sumActual, wantAgg)
	}
	// Proportional token weights 1:2:3 of 0.06.
	if math.Abs(*children[0].CapacityActual-0.01) > 1e-9 ||
		math.Abs(*children[1].CapacityActual-0.02) > 1e-9 ||
		math.Abs(*children[2].CapacityActual-0.03) > 1e-9 {
		t.Fatalf("shares a=%v b=%v c=%v", *children[0].CapacityActual, *children[1].CapacityActual, *children[2].CapacityActual)
	}
}
