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
			Provider: provider, AccountRef: acct, InstallRef: "i-" + provider,
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
	prior, err := led.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r", AttemptID: "wi_x",
		Provider: "antigravity", Model: "gpt-oss", Depth: "medium", Snapshot: &snap,
	})
	if err != nil || prior.State == "refused" {
		t.Fatalf("prior reserve: %+v %v", prior, err)
	}
	holds["wi_x"] = capacityHold{projectID: "p", runID: "r", attemptID: "wi_x"}
	hook := &goalCapacityReroute{ledger: led, snap: &snap, projectID: "p", runID: "r", holds: holds}

	res, err := hook.OnModelUnavailableAlternate(workflowrun.CapacityRerouteInput{
		WorkItemID: "wi_x", FailedAttemptID: "att-g0", PriorHoldAttempt: "wi_x",
		NewAttemptID: "att-g1", FailedProvider: "antigravity", FailedModel: "gpt-oss",
		AltProvider: "codex", AltModel: "gpt-5.5", Depth: "medium",
		PriorSource: "unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.PriorState != "released" || res.AlternateState != "reserved" {
		t.Fatalf("%+v", res)
	}
	if res.AccountRef == prior.AccountRef {
		t.Fatalf("account must not cross companies: prior=%s alt=%s", prior.AccountRef, res.AccountRef)
	}
	prev, ok := led.Get("p", "r", "wi_x")
	if !ok || prev.State != "released" || prev.Actual != nil {
		t.Fatalf("prior released nil actual: %+v", prev)
	}
	alt, ok := led.Get("p", "r", "att-g1")
	if !ok || alt.State != "reserved" {
		t.Fatalf("alt: %+v", alt)
	}
	if holds["wi_x"].attemptID != "att-g1" {
		t.Fatalf("hold: %+v", holds)
	}

	// Known actual path: re-seed prior hold
	holds2 := map[string]capacityHold{}
	led2, _ := capacityledger.OpenPath(filepath.Join(t.TempDir(), "cap2.json"), func() time.Time { return now })
	prior2, err := led2.Reserve(capacityledger.ReserveInput{
		ProjectID: "p", RunID: "r2", AttemptID: "wi_y",
		Provider: "antigravity", Model: "gpt-oss", Depth: "medium", Snapshot: &snap,
	})
	if err != nil {
		t.Fatal(err)
	}
	holds2["wi_y"] = capacityHold{projectID: "p", runID: "r2", attemptID: "wi_y"}
	hook2 := &goalCapacityReroute{ledger: led2, snap: &snap, projectID: "p", runID: "r2", holds: holds2}
	act := 0.05
	res2, err := hook2.OnModelUnavailableAlternate(workflowrun.CapacityRerouteInput{
		WorkItemID: "wi_y", FailedAttemptID: "att-f", PriorHoldAttempt: "wi_y",
		NewAttemptID: "att-r", AltProvider: "codex", AltModel: "gpt-5.5", Depth: "medium",
		PriorActual: &act, PriorSource: "provider_usage",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res2.PriorState != "reconciled" {
		t.Fatalf("want reconciled prior: %+v", res2)
	}
	p2, _ := led2.Get("p", "r2", "wi_y")
	if p2.Actual == nil || *p2.Actual != act {
		t.Fatalf("prior actual: %+v", p2)
	}
	_ = prior2
	_ = strings.Contains
}
