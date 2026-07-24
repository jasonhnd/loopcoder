package goalrun

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestCapacityRerouteReconcileOrReleaseThenReserve(t *testing.T) {
	now := time.Date(2026, 7, 23, 20, 0, 0, 0, time.UTC)
	ledgerPath := filepath.Join(t.TempDir(), "cap.json")
	led, err := capacityledger.OpenPath(ledgerPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	mk := func(provider, model, acct string, rem float64) capacitysnapshot.AccountObservation {
		return capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: provider, AccountRef: acct, InstallRef: "i-test",
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100 - rem, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: rem, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				ResetAt: func() *time.Time { t := now.Add(time.Hour); return &t }(), CapturedAt: now, Source: "test",
			}},
			Models: []capacitysnapshot.ModelSpec{{
				ModelID: model, SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
			}},
			Source: "test", CapturedAt: now,
		})
	}
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		mk("codex", "gpt-5.5", "acct-codex", 90),
		mk("antigravity", "gpt-oss", "acct-ag", 80),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	holds := map[string]capacityHold{}
	// Exact workflow attempt IDs — never WorkItemID as capacity key.
	priorAtt := "att-wi_x-deadbeef-g0"
	altAtt := "att-wi_x-deadbeef-g1"
	prior, err := led.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: priorAtt,
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "antigravity", Model: "gpt-oss", Depth: "medium",
		AccountRef: "acct-ag", WindowKind: "five_hour", Snapshot: &snap,
		InstallRef: "i-test",
	})
	if err != nil || prior.State == "refused" {
		t.Fatalf("prior reserve: %+v %v", prior, err)
	}
	holds["wi_x"] = capacityHold{projectID: "p", runID: "r", attemptID: priorAtt}
	hook := &goalCapacityReroute{ledger: led, snap: &snap, projectID: "p", runID: "r", holds: holds}

	res, err := hook.OnModelUnavailableAlternate(workflowrun.CapacityRerouteInput{
		WorkItemID: "wi_x", FailedAttemptID: priorAtt, PriorHoldAttempt: priorAtt,
		NewAttemptID: altAtt,
		PlanDigest:   "sha256:test-exec-plan", GraphDigest: "sha256:test-graph",
		TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract",
		FailedProvider: "antigravity", FailedModel: "gpt-oss", FailedDepth: "medium",
		FailedPermission: "bounded_write", FailedAccountRef: prior.AccountRef, FailedWindowKind: prior.WindowKind,
		FailedInstallRef:    "i-test",
		FailedReservationID: prior.ReservationID,
		AltProvider:         "codex", AltModel: "gpt-5.5", AltDepth: "medium", AltPermission: "bounded_write",
		AltAccountRef: "acct-codex", AltInstallRef: "i-test", AltWindowKind: "five_hour",
		Depth: "medium", Permission: "bounded_write",
		PriorSource: "unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.PriorState != "released" || res.AlternateState != "reserved" {
		t.Fatalf("%+v", res)
	}
	if res.PriorTransition.AttemptID != priorAtt {
		t.Fatalf("prior transition must bind failed attempt: %+v", res.PriorTransition)
	}
	if res.AccountRef == prior.AccountRef {
		t.Fatalf("account must not cross companies: prior=%s alt=%s", prior.AccountRef, res.AccountRef)
	}
	prev, ok := led.Get("p", "r", priorAtt)
	if !ok || prev.State != "released" || prev.Actual != nil {
		t.Fatalf("prior released nil actual: %+v", prev)
	}
	if prev.ActualSource != "" {
		t.Fatalf("released ActualSource must be empty (honest unknown), got %q", prev.ActualSource)
	}
	alt, ok := led.Get("p", "r", altAtt)
	if !ok || alt.State != "reserved" {
		t.Fatalf("alt: %+v", alt)
	}
	if holds["wi_x"].attemptID != altAtt {
		t.Fatalf("hold: %+v", holds)
	}

	// Known actual path: re-seed prior hold under exact attempt id
	holds2 := map[string]capacityHold{}
	led2, _ := capacityledger.OpenPath(filepath.Join(t.TempDir(), "cap2.json"), func() time.Time { return now })
	prior2Att := "att-wi_y-cafebabe-g0"
	prior2, err := led2.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r2", AttemptID: prior2Att,
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "antigravity", Model: "gpt-oss", Depth: "medium",
		AccountRef: "acct-ag", WindowKind: "five_hour", Snapshot: &snap,
		InstallRef: "i-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	holds2["wi_y"] = capacityHold{projectID: "p", runID: "r2", attemptID: prior2Att}
	hook2 := &goalCapacityReroute{ledger: led2, snap: &snap, projectID: "p", runID: "r2", holds: holds2}
	act := 0.05
	res2, err := hook2.OnModelUnavailableAlternate(workflowrun.CapacityRerouteInput{
		WorkItemID: "wi_y", FailedAttemptID: prior2Att, PriorHoldAttempt: prior2Att,
		NewAttemptID: "att-wi_y-cafebabe-g1",
		PlanDigest:   "sha256:test-exec-plan", GraphDigest: "sha256:test-graph",
		TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract",
		FailedProvider: "antigravity", FailedModel: "gpt-oss", FailedDepth: "medium",
		FailedPermission: "bounded_write", FailedAccountRef: prior2.AccountRef, FailedWindowKind: prior2.WindowKind,
		FailedInstallRef:    "i-test",
		FailedReservationID: prior2.ReservationID,
		AltProvider:         "codex", AltModel: "gpt-5.5", AltDepth: "medium", AltPermission: "bounded_write",
		AltAccountRef: "acct-codex", AltInstallRef: "i-test", AltWindowKind: "five_hour",
		Depth: "medium", Permission: "bounded_write",
		PriorActual: &act, PriorSource: "provider_usage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.PriorState != "reconciled" {
		t.Fatalf("want reconciled prior: %+v", res2)
	}
	p2, _ := led2.Get("p", "r2", prior2Att)
	if p2.Actual == nil || *p2.Actual != act {
		t.Fatalf("prior actual: %+v", p2)
	}
	if p2.ActualSource != "provider_usage" {
		t.Fatalf("ActualSource=%q", p2.ActualSource)
	}
	_ = prior2
	_ = strings.Contains
}

func TestCapacityRerouteRejectsWorkItemIDFallback(t *testing.T) {
	now := time.Date(2026, 7, 23, 20, 0, 0, 0, time.UTC)
	led, err := capacityledger.OpenPath(filepath.Join(t.TempDir(), "cap.json"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: "codex", AccountRef: "a", InstallRef: "i-test",
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh, CapturedAt: now, Source: "t",
			}},
			Models: []capacitysnapshot.ModelSpec{{ModelID: "gpt-5.5", Present: true, SupportedDepths: []string{"medium"}, DefaultDepth: "medium"}},
			Source: "t", CapturedAt: now,
		}),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	hook := &goalCapacityReroute{ledger: led, snap: &snap, projectID: "p", runID: "r", holds: map[string]capacityHold{}}
	// Empty prior + empty failed → refuse (no WorkItemID fallback).
	if _, err := hook.OnModelUnavailableAlternate(workflowrun.CapacityRerouteInput{
		WorkItemID: "wi_x", NewAttemptID: "att-g1", AltProvider: "codex", AltModel: "gpt-5.5", Depth: "medium",
	}); err == nil {
		t.Fatal("want refuse when prior attempt id missing")
	}
	// prior hold != failed attempt → refuse
	_, _ = led.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "att-hold",
		PlanDigest: "sha256:test-exec-plan", GraphDigest: "sha256:test-graph", TaskClass: "tera", ChildContractDigest: "sha256:test-child-contract", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		AccountRef: "a", WindowKind: "five_hour", Snapshot: &snap,
		InstallRef: "i-test",
	})
	hook.holds["wi_x"] = capacityHold{projectID: "p", runID: "r", attemptID: "att-hold"}
	if _, err := hook.OnModelUnavailableAlternate(workflowrun.CapacityRerouteInput{
		WorkItemID: "wi_x", FailedAttemptID: "att-other", PriorHoldAttempt: "att-hold",
		NewAttemptID: "att-g1", AltProvider: "codex", AltModel: "gpt-5.5", Depth: "medium",
	}); err == nil {
		t.Fatal("want refuse when prior != failed")
	}
}
