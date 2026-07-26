package goalrun_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func TestParseRouteRequirementStrict(t *testing.T) {
	pr, err := goalrun.ParseRouteRequirement("class=soul,depth=high,permission=read-only")
	if err != nil || pr.Class != capclass.ClassSoul || pr.Depth != "high" || pr.Permission != "read-only" {
		t.Fatalf("soul: %+v %v", pr, err)
	}
	pr, err = goalrun.ParseRouteRequirement("class=luna,depth=low,permission=read-only")
	if err != nil || pr.Class != capclass.ClassLuna {
		t.Fatalf("luna: %+v %v", pr, err)
	}
	pr, err = goalrun.ParseRouteRequirement("class=tera,depth=medium,permission=bounded_write")
	if err != nil || pr.Class != capclass.ClassTera || pr.Permission != "bounded_write" {
		t.Fatalf("tera: %+v %v", pr, err)
	}
	// Missing / invalid / duplicate / empty commas / aliases → fail closed (never invent).
	for _, bad := range []string{
		"",
		"depth=high,permission=read-only", // missing class
		"class=soul,permission=read-only", // missing depth
		"class=soul,depth=high",           // missing permission
		"class=mega,depth=high,permission=read-only",
		"class=needs_human,depth=high,permission=read-only",
		"class=soul,depth=xhigh,permission=read-only", // non-canonical depth
		"class=soul,depth=high,permission=admin",
		"class=soul,class=tera,depth=high,permission=read-only", // duplicate
		"class=soul,depth=high,depth=low,permission=read-only",
		"class=soul,depth=high,permission=read-only,permission=bounded_write",
		"class=,depth=high,permission=read-only",
		"class=soul,depth=,permission=read-only",
		"class=soul,depth=high,permission=",
		"class=soul,depth=high,permission=read-only,extra=1",
		// empty comma tokens
		",class=soul,depth=high,permission=read-only",
		"class=soul,depth=high,permission=read-only,",
		"class=soul,,depth=high,permission=read-only",
		// undocumented permission aliases rejected
		"class=soul,depth=high,permission=readonly",
		"class=soul,depth=high,permission=read_only",
		"class=soul,depth=high,permission=ro",
		"class=soul,depth=high,permission=bounded-write",
		"class=soul,depth=high,permission=workspace-write",
	} {
		if _, err := goalrun.ParseRouteRequirement(bad); err == nil {
			t.Fatalf("expected fail for %q", bad)
		}
	}
}

func okF(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
}
func falseF(id string) eligibility.Fact {
	return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
}

func lunaOnlyInventory(t *testing.T) (autoroute.Inventory, capacitysnapshot.Snapshot) {
	t.Helper()
	now := time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC)
	// Luna model only — soul children must not route/pin; luna children may.
	acct := "acct-" + strings.Repeat("l", 64)
	mk := func(effort, perm string) eligibility.Candidate {
		return eligibility.Candidate{
			Provider: "codex", Model: "gpt-5.1-codex-mini", Effort: effort, Permission: perm,
			ModelClass: capclass.ClassLuna,
			AccountRef: acct, InstallRef: "install-codex-luna", WindowKind: "five_hour",
			Installed: okF("i"), Authenticated: okF("a"), ModelPresent: okF("m"),
			PermissionOK: okF("p"), EffortOK: okF("e"), Healthy: okF("h"),
			CooldownActive: falseF("cd"), ResourceFit: okF("r"), QuotaRemaining: 9999,
		}
	}
	cands := []eligibility.Candidate{
		mk("low", "read-only"),
		mk("low", "bounded_write"),
		mk("medium", "read-only"),
		mk("medium", "bounded_write"),
		mk("high", "read-only"),
		mk("high", "bounded_write"),
	}
	// Soft ranking requires exact account/window scores (not only hard eligibility).
	rf, ttr, rel := 0.9, 2*time.Hour, 0.9
	soft := quotapolicy.Candidate{
		Provider: "codex", Model: "gpt-5.1-codex-mini",
		AccountRef: acct, InstallRef: "install-codex-luna", WindowKind: "five_hour",
		Windows: []quotapolicy.Window{{
			Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
			Evidence: quotapolicy.EvidenceExact, TimeToReset: &ttr,
		}},
		Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact,
	}
	inv := autoroute.Inventory{
		EvidenceDigest: "test-luna-only",
		Candidates:     cands,
		Soft:           []quotapolicy.Candidate{soft},
		Machine:        eligibility.MachineAdmission{CapacityOK: okF("mach"), ConcurrentSlots: 4},
	}
	// Minimal snapshot so Reserve can identity-match (when pin/auto succeeds).
	acc := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-" + strings.Repeat("l", 64), InstallRef: "install-codex-luna",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(time.Hour)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.1-codex-mini", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{acc}, now)
	if err != nil {
		t.Fatal(err)
	}
	return inv, snap
}

// TestAutoRouteSoulCannotSelectLunaModel proves soul RouteRequirement cannot
// spend on a luna-only inventory; luna children keep task_class=luna (not upgraded).
func TestAutoRouteSoulCannotSelectLunaModel(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 0, 0, 0, time.UTC)
	home := testHome(t)
	inv, snap := lunaOnlyInventory(t)
	ledgerPath := filepath.Join(t.TempDir(), "cap.json")

	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-class", Goal: "implement transparent multi-child routing",
		Issue: "1397", Actor: "owner",
		HomeDir:  home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: func() time.Time { return now }},
		Now:      func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if len(res.Children) < 4 {
		t.Fatalf("expected decompose children, got %d err=%v status=%s msg=%s", len(res.Children), err, res.Status, res.Message)
	}

	var sawSoul, sawLuna, lunaRouted bool
	for _, c := range res.Children {
		if c.TaskClass == "" {
			t.Fatalf("child missing task_class evidence: %+v", c)
		}
		// Requirement identity never upgraded.
		if c.ChildID == "wi_verify" && c.TaskClass != "soul" {
			t.Fatalf("verify child class upgraded: %q", c.TaskClass)
		}
		if (c.ChildID == "wi_research" || c.ChildID == "wi_docs") && c.TaskClass != "luna" {
			t.Fatalf("%s class upgraded: %q", c.ChildID, c.TaskClass)
		}
		switch c.TaskClass {
		case "soul":
			sawSoul = true
			if c.ReservationID != "" || c.CapacityReserved != nil {
				t.Fatalf("soul child must not reserve on luna model: %+v", c)
			}
			if c.Provider != "" || c.Model != "" {
				t.Fatalf("soul child must not select provider/model on luna-only inventory: %+v", c)
			}
			if !c.Unavailable {
				t.Fatalf("soul child must be unavailable: %+v", c)
			}
		case "luna":
			sawLuna = true
			if c.TaskClass != "luna" {
				t.Fatalf("luna child upgraded: %q", c.TaskClass)
			}
			if !c.Unavailable && c.Provider != "" {
				lunaRouted = true
				if c.Model != "gpt-5.1-codex-mini" {
					t.Fatalf("luna child unexpected model: %+v", c)
				}
			}
		case "tera":
			if c.ReservationID != "" || c.Provider != "" {
				t.Fatalf("tera child must not route/reserve on luna-only inventory: %+v", c)
			}
		}
	}
	if !sawSoul {
		t.Fatal("expected soul verify child in decompose")
	}
	if !sawLuna {
		t.Fatal("expected luna research/docs child in decompose")
	}
	if !lunaRouted {
		t.Fatalf("expected at least one luna child to route against luna model; children=%+v err=%v", res.Children, err)
	}
}

// TestPinSoulFailsOnLunaModelNoSpend: explicit pin to luna model fails soul
// children without reserve; luna children may bind; task_class stays exact.
func TestPinSoulFailsOnLunaModelNoSpend(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 30, 0, 0, time.UTC)
	home := testHome(t)
	inv, snap := lunaOnlyInventory(t)
	ledgerPath := filepath.Join(t.TempDir(), "cap-pin.json")

	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-pin-class", Goal: "implement transparent multi-child routing",
		Issue: "1397", Actor: "owner",
		Provider: "codex", Model: "gpt-5.1-codex-mini",
		HomeDir:  home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: func() time.Time { return now }},
		Now:      func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if len(res.Children) < 4 {
		t.Fatalf("children=%d err=%v status=%s msg=%s", len(res.Children), err, res.Status, res.Message)
	}

	var soulFailed, lunaBound bool
	for _, c := range res.Children {
		if c.TaskClass == "" {
			t.Fatalf("missing task_class on child: %+v", c)
		}
		switch c.TaskClass {
		case "soul":
			if c.ReservationID != "" || c.CapacityReserved != nil {
				t.Fatalf("soul pin must not reserve luna model: %+v", c)
			}
			if c.Terminal != "pin_fail" && !c.Unavailable {
				t.Fatalf("soul pin expected pin_fail/unavailable: %+v", c)
			}
			// Owner pin identity not rewritten to alternate.
			if c.Provider != "" && c.Provider != "codex" {
				t.Fatalf("provider fallback forbidden: %q", c.Provider)
			}
			soulFailed = true
		case "luna":
			if c.TaskClass != "luna" {
				t.Fatalf("luna requirement upgraded: %q", c.TaskClass)
			}
			if !c.Unavailable && c.ReservationID != "" {
				lunaBound = true
				if c.Provider != "codex" || c.Model != "gpt-5.1-codex-mini" {
					t.Fatalf("luna pin identity wrong: %+v", c)
				}
			}
		}
	}
	if !soulFailed {
		t.Fatal("expected soul pin_fail")
	}
	if !lunaBound {
		t.Fatalf("expected luna child pin bind+reserve; children=%+v", res.Children)
	}
}

// TestExecuteInvalidRouteRequirementNoSpend covers missing/invalid/duplicate
// class/depth/permission via injected Decompose. Prevalidation is before
// Materialize/inventory/ledger; executor never runs; prior-success is not reused.
func TestExecuteInvalidRouteRequirementNoSpend(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		route  string
		substr string
	}{
		{"missing_class", "depth=high,permission=read-only", "missing class"},
		{"missing_depth", "class=soul,permission=read-only", "missing depth"},
		{"missing_permission", "class=soul,depth=high", "missing permission"},
		{"invalid_class", "class=unknown_tier,depth=medium,permission=bounded_write", "invalid"},
		{"needs_human", "class=needs_human,depth=high,permission=read-only", "needs_human"},
		{"dup_class", "class=soul,class=tera,depth=high,permission=read-only", "duplicate"},
		{"dup_depth", "class=soul,depth=high,depth=low,permission=read-only", "duplicate"},
		{"dup_permission", "class=soul,depth=high,permission=read-only,permission=bounded_write", "duplicate"},
		{"bad_depth", "class=soul,depth=xhigh,permission=read-only", "depth"},
		{"bad_permission", "class=soul,depth=high,permission=admin", "permission"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := testHome(t)
			ledgerPath := filepath.Join(t.TempDir(), "cap-class.json")
			invN, ledN, decompN := 0, 0, 0
			calls := map[string]int{}
			// Seed prior success that must NOT be reused under invalid contract.
			prior := map[string]workflowrun.ChildOutcome{
				"wi_bad": {
					WorkItemID: "wi_bad", Terminal: "succeeded",
					AttemptID: "att-prior-g0", OutputEvidence: "sha256:prior",
					Provider: "codex", Model: "gpt-5.5", Depth: "medium",
				},
			}
			res, err := goalrun.Execute(context.Background(), goalrun.Request{
				ProjectID: "proj-bad-rr-" + tc.name, RunID: "run_bad_" + tc.name,
				Goal: "route requirement gate", Issue: "1397", Actor: "owner",
				Provider: "codex", Model: "gpt-5.5",
				HomeDir: home, Now: func() time.Time { return now },
				PriorSucceeded: prior,
				Decompose: func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
					decompN++
					return malformedRouteGraph(tc.route, now), nil
				},
				LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
					invN++
					return autoroute.Inventory{}, capacitysnapshot.Snapshot{}, fmt.Errorf("LoadInventory must not run")
				},
				OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
					ledN++
					return capacityledger.OpenPath(ledgerPath, nowFn)
				},
				Executor: workflowrun.FakeChildExecutor{
					HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
				},
			})
			_ = err
			if decompN != 1 {
				t.Fatalf("Decompose must run once, got %d", decompN)
			}
			if invN != 0 {
				t.Fatalf("LoadInventory must not run: %d", invN)
			}
			if ledN != 0 {
				t.Fatalf("OpenLedger must not run: %d", ledN)
			}
			if res.Status != "blocked" {
				t.Fatalf("status=%s want blocked", res.Status)
			}
			// Pre-Normalize early failure: PlanDigest (canonical ExecutionPlanDigest)
			// stays empty; GraphDigest alone carries workgraph.DigestGraph.
			if res.PlanDigest != "" {
				t.Fatalf("pre-Normalize PlanDigest must stay empty, got %q", res.PlanDigest)
			}
			wantGraphDigest := workgraph.DigestGraph(malformedRouteGraph(tc.route, now))
			if res.GraphDigest == "" {
				t.Fatalf("pre-Normalize GraphDigest must carry DigestGraph")
			}
			if res.GraphDigest != wantGraphDigest {
				t.Fatalf("GraphDigest=%q want DigestGraph=%q", res.GraphDigest, wantGraphDigest)
			}
			if len(res.Children) < 1 {
				t.Fatalf("expected blocked child evidence: %+v", res)
			}
			blocked := false
			for _, c := range res.Children {
				if c.ChildID != "wi_bad" {
					continue
				}
				blocked = true
				if !c.Unavailable || c.Terminal != "route_requirement_invalid" {
					t.Fatalf("want route_requirement_invalid: %+v", c)
				}
				if c.ReservationID != "" || c.CapacityReserved != nil {
					t.Fatalf("must not reserve: %+v", c)
				}
				// Prior success must NOT be reused (prevalidation before resume).
				if c.Stage == "resumed" || c.AttemptID == "att-prior-g0" || c.OutputEvidence == "sha256:prior" {
					t.Fatalf("prior success reused under invalid requirement: %+v", c)
				}
				if !strings.Contains(strings.ToLower(c.RouteReason), strings.ToLower(tc.substr)) {
					t.Fatalf("reason %q should mention %q", c.RouteReason, tc.substr)
				}
			}
			if !blocked {
				t.Fatalf("wi_bad missing: %+v", res.Children)
			}
			if calls["wi_bad"] != 0 {
				t.Fatalf("executor called: %+v", calls)
			}
			entries := ledgerFileEntries(t, ledgerPath)
			if len(entries) != 0 {
				t.Fatalf("ledger must be empty, got %d", len(entries))
			}
		})
	}
}

func malformedRouteGraph(routeReq string, now time.Time) workgraph.Graph {
	g := workgraph.Graph{
		Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
		GraphID: "g_bad_route", Version: 1,
		Source: workgraph.SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: "owner",
		Items: []workgraph.WorkItem{{
			Schema: workgraph.SchemaItem, ID: "wi_bad", Status: workgraph.ItemRequired,
			Intent: "malformed route child", Owner: "worker",
			Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1,
			OutputContract: "none", RouteRequirement: routeReq,
		}},
		Limits:    workgraph.DefaultLimits(),
		CreatedAt: now,
	}
	g.PlanDigest = workgraph.DigestGraph(g)
	return g
}
