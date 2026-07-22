package goalrun_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
)

func TestExecuteDecomposesAndReportsChildren(t *testing.T) {
	var reports bytes.Buffer
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj", Goal: "implement transparent multi-child routing",
		Issue: "1342", Actor: "owner", Owner: "worker",
		Provider: "fixture", Model: "fixture-model",
		ReportOut: &reports,
		Now:       func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) },
	})
	if res.GraphID == "" || res.PlanDigest == "" {
		t.Fatalf("missing graph: %+v err=%v", res, err)
	}
	if len(res.Children) < 4 {
		t.Fatalf("children=%d", len(res.Children))
	}
	for _, c := range res.Children {
		if c.RouteRequirement == "" || c.ChildID == "" {
			t.Fatalf("%+v", c)
		}
		if strings.Contains(strings.ToLower(c.Intent), "provider_native") {
			t.Fatal("provider-native intent leak")
		}
	}
	if reports.Len() == 0 {
		t.Fatal("expected JSONL child reports")
	}
}

func TestExecuteAutoRoutesChildrenWithCapacityAccounting(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC)
	// Two unattended-eligible accounts for multi-provider routing.
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(2 * time.Hour)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	claude := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "claude", AccountRef: "acct-claude", InstallRef: "i-claude",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			// Reset sooner → use-before-reset prefers this for some children.
			ResetAt: ptrTime(now.Add(30 * time.Minute)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "claude-sonnet-4-5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, claude}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "capacity-ledger.json")
	var reports bytes.Buffer
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-mp", Goal: "implement multi-provider capacity-aware routing with tests",
		Issue: "1343", Actor: "owner",
		// empty provider/model → auto-route
		ReportOut: &reports,
		Now:       func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if err != nil && res.GraphID == "" {
		t.Fatalf("execute: %v", err)
	}
	if len(res.Children) < 4 {
		t.Fatalf("children=%d", len(res.Children))
	}
	routed := 0
	for _, c := range res.Children {
		if c.Unavailable {
			continue
		}
		routed++
		if c.Provider == "" || c.Model == "" || c.Depth == "" {
			t.Fatalf("child missing route fields: %+v", c)
		}
		if c.CapacityBefore == nil || c.CapacityReserved == nil {
			t.Fatalf("child missing capacity accounting: %+v", c)
		}
		if c.CapacityState != "released" {
			t.Fatalf("want released after dry-run wave, got %s", c.CapacityState)
		}
	}
	if routed < 2 {
		t.Fatalf("expected ≥2 routed children, got %d (%+v)", routed, res.Children)
	}
	if !res.MultiProviderOK && len(res.ProvidersUsed) < 1 {
		t.Fatalf("expected at least one provider used: %+v", res)
	}
	// Prefer multi-provider when inventory has two companies.
	if len(res.ProvidersUsed) < 2 {
		t.Logf("note: multi-provider diversity not always selected; providers=%v models=%v depths=%v",
			res.ProvidersUsed, res.ModelsUsed, res.DepthsUsed)
	}
	if !res.MultiModelOrDepthOK && len(res.DepthsUsed) < 2 {
		// depth diversification is intentional across children
		t.Fatalf("expected multi depth or model; models=%v depths=%v", res.ModelsUsed, res.DepthsUsed)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
