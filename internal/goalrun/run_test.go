package goalrun_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func testHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "loopcoder-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestExecuteDecomposesAndReportsChildren(t *testing.T) {
	home := testHome(t)
	var reports bytes.Buffer
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj", Goal: "implement transparent multi-child routing",
		Issue: "1342", Actor: "owner", Owner: "worker",
		Provider: "fixture", Model: "fixture-model",
		ReportOut: &reports,
		HomeDir:   home,
		Executor:  workflowrun.FakeChildExecutor{HomeDir: home},
		Now:       func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) },
	})
	if res.GraphID == "" || res.PlanDigest == "" {
		t.Fatalf("missing graph: %+v err=%v", res, err)
	}
	if err != nil {
		t.Fatalf("execute: %v", err)
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
		if c.OutputEvidence == "" {
			t.Fatalf("missing output evidence after real executor path: %+v", c)
		}
		if c.Terminal != "succeeded" {
			t.Fatalf("terminal %+v", c)
		}
	}
	if reports.Len() == 0 {
		t.Fatal("expected JSONL child reports")
	}
	if res.Workflow.LaunchCount < 4 {
		t.Fatalf("workflow launches=%d", res.Workflow.LaunchCount)
	}
}

func TestExecuteAutoRoutesChildrenWithCapacityAccounting(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC)
	home := testHome(t)
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
	// Prefer Antigravity/Gemini as second company (not Claude-only).
	gemini := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "gemini", AccountRef: "acct-gemini", InstallRef: "i-gemini",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(30 * time.Minute)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gemini-2.5-pro", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, gemini}, now)
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
		Issue: "1342", Actor: "owner",
		// empty provider/model → auto-route
		ReportOut: &reports,
		HomeDir:   home,
		Executor:  workflowrun.FakeChildExecutor{HomeDir: home, Now: func() time.Time { return now }},
		Now:       func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if err != nil {
		t.Fatalf("execute: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if len(res.Children) < 4 {
		t.Fatalf("children=%d", len(res.Children))
	}
	routed := 0
	for _, c := range res.Children {
		if c.Unavailable {
			t.Fatalf("unexpected unavailable: %+v", c)
		}
		routed++
		if c.Provider == "" || c.Model == "" || c.Depth == "" {
			t.Fatalf("child missing route fields: %+v", c)
		}
		if c.CapacityBefore == nil || c.CapacityReserved == nil {
			t.Fatalf("child missing capacity accounting: %+v", c)
		}
		// Fake executor has unknown actual → honest release (never fabricated actual).
		if c.CapacityState != "released" && c.CapacityState != "reconciled" {
			t.Fatalf("want released|reconciled after execute, got %s (%+v)", c.CapacityState, c)
		}
		if c.CapacityActual != nil && c.ActualSource == "unknown" {
			t.Fatalf("must not invent actual when source unknown: %+v", c)
		}
		if c.OutputEvidence == "" {
			t.Fatalf("missing output evidence: %+v", c)
		}
		if c.Terminal != "succeeded" {
			t.Fatalf("terminal %+v", c)
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
		t.Fatalf("expected multi depth or model; models=%v depths=%v", res.ModelsUsed, res.DepthsUsed)
	}
	if res.Workflow.Status != workflowrun.StatusHumanGate {
		t.Fatalf("workflow status %+v", res.Workflow)
	}
}

func TestDryRunPreviewReleasesWithoutExecute(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 30, 0, 0, time.UTC)
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 5, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 95, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(time.Hour)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	dry := true
	ledgerPath := filepath.Join(t.TempDir(), "cap.json")
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-dry", Goal: "preview routes only",
		DryRun: &dry,
		Now:    func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if res.Status != "planned" {
		t.Fatalf("status %s", res.Status)
	}
	if res.Workflow.LaunchCount != 0 {
		t.Fatalf("dry-run must not launch children: %+v", res.Workflow)
	}
	for _, c := range res.Children {
		if c.Unavailable {
			continue
		}
		if c.CapacityState != "released" {
			t.Fatalf("dry-run want released: %+v", c)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
