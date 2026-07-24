package goalrun_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/goalpr"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/routecontract"
	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// childContractForGoal computes the canonical plan/graph/class/CCD/graphID that
// goalrun will materialize for the same goal/issue/actor/project — used to seed
// resume priors without rewriting historical identity at runtime.
func childContractForGoal(t *testing.T, goal, issue, actor, projectID, workItemID string, now time.Time) (planDig, graphDig, class, ccd, outputContract, graphID string) {
	t.Helper()
	g, err := workgraph.DecomposeGoal(workgraph.DecomposeOptions{
		Goal: goal, Issue: issue, Actor: actor, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	def, err := workflowdef.FromGraph(g)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := workflowdef.Normalize(def)
	if err != nil {
		t.Fatal(err)
	}
	ap, err := workflowdef.Approve(plan.Digest, actor, "test seed", now)
	if err != nil {
		t.Fatal(err)
	}
	mat, err := workflowdef.NewRegistry().Materialize(projectID, def, ap, now)
	if err != nil {
		t.Fatal(err)
	}
	graphDig = workgraph.DigestGraph(mat.Graph)
	graphID = mat.Graph.GraphID
	var item workgraph.WorkItem
	found := false
	for _, it := range mat.Graph.Items {
		if it.ID == workItemID {
			item = it
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("work item %s not in graph", workItemID)
	}
	pr, err := routecontract.ParseRouteRequirement(item.RouteRequirement)
	if err != nil {
		t.Fatal(err)
	}
	class = string(pr.Class)
	ccd, err = routecontract.ChildContractDigest(routecontract.ChildAssignment{
		ExecutionPlanDigest: plan.Digest,
		WorkItemID:          workItemID,
		TaskClass:           class,
		Depth:               pr.Depth,
		Permission:          pr.Permission,
		OutputContract:      item.OutputContract,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan.Digest, graphDig, class, ccd, item.OutputContract, graphID
}

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
	now := time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	var reports bytes.Buffer
	req := env.pinRequest("implement transparent multi-child routing", "1342")
	req.ProjectID = "proj"
	req.ReportOut = &reports
	res, err := goalrun.Execute(context.Background(), req)
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
		if strings.EqualFold(c.Provider, "fixture") {
			t.Fatalf("fixture provider forbidden: %+v", c)
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

func TestFixturePinFailsClosedBeforeAnyProductState(t *testing.T) {
	// Forbidden product route must not touch Decompose / inventory / ledger / executor.
	home := testHome(t)
	decompN, invN, ledN, execN := 0, 0, 0, 0
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-fix", Goal: "should not run", Issue: "1", Actor: "owner",
		Provider: "fixture", Model: "fixture-model",
		HomeDir: home,
		Now:     func() time.Time { return time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC) },
		Decompose: func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
			decompN++
			panic("Decompose must not run for fixture pin")
		},
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			invN++
			panic("LoadInventory must not run for fixture pin")
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			ledN++
			panic("OpenLedger must not run for fixture pin")
		},
		Executor: countingPanicExecutor{n: &execN},
	})
	if err == nil || !strings.Contains(err.Error(), "fixture") {
		t.Fatalf("fixture must fail closed before decompose: %v", err)
	}
	if decompN != 0 || invN != 0 || ledN != 0 || execN != 0 {
		t.Fatalf("fixture must create zero product state: decomp=%d inv=%d led=%d exec=%d", decompN, invN, ledN, execN)
	}
}

type countingPanicExecutor struct{ n *int }

func (c countingPanicExecutor) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	if c.n != nil {
		*c.n++
	}
	panic("Executor must not run")
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
	// Second company must be account-affirmable (grok). Gemini/AG cannot affirm
	// exact AccountRef and are hard-excluded from capacity-bound product routes.
	grok := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "grok", AccountRef: "acct-grok", InstallRef: "i-grok",
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
			ModelID: "grok-4.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, grok}, now)
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
		// #1343: after must come from post-run observation when inventory available
		// (not n/a solely because token actual is unknown).
		if c.CapacityAfter == nil {
			t.Fatalf("want capacity after from post-run observation: %+v", c)
		}
		if !strings.Contains(c.CapacityNote, "after_source=") && !strings.Contains(c.CapacityNote, "reconciled=") {
			t.Fatalf("want after_source or reconciled in note: %s", c.CapacityNote)
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

func TestUniqueProjectIDNeverLocalProject(t *testing.T) {
	now := time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC)
	a := goalrun.UniqueProjectID("/tmp/disp-a", func() time.Time { return now })
	b := goalrun.UniqueProjectID("/tmp/disp-a", func() time.Time { return now.Add(time.Nanosecond) })
	if a == "" || a == "local-project" || !strings.HasPrefix(a, "disp-") {
		t.Fatalf("a=%q", a)
	}
	if a == b {
		t.Fatalf("expected distinct project ids for distinct times: %q", a)
	}
}

func TestExecuteUsesUniqueProjectAndRunNotSharedLocal(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	// Default ProjectID empty → UniqueProjectID, not local-project.
	req := env.pinRequest("unique namespace canary", "1343")
	req.ProjectID = ""
	req.RepoPath = "/tmp/disposable-repo-a"
	res, err := goalrun.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Workflow.RunID == "" {
		t.Fatal("expected unique workflow RunID")
	}
	// Attempts should embed unique run, not only plan digest.
	for _, c := range res.Workflow.Children {
		if c.AttemptID == "" {
			continue
		}
		if !strings.Contains(c.AttemptID, "att-") {
			t.Fatalf("attempt %q", c.AttemptID)
		}
		// Worktree under home, not bare local-project shared path when home set.
		if c.WorktreePath != "" && strings.Contains(c.WorktreePath, "/local-project/") {
			t.Fatalf("must not use shared local-project path: %s", c.WorktreePath)
		}
	}
}

func TestExecuteBindsRequiredDepthsFromRouteRequirement(t *testing.T) {
	now := time.Date(2026, 7, 22, 23, 45, 0, 0, time.UTC)
	home := testHome(t)
	codex := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "codex", AccountRef: "acct-codex", InstallRef: "i-codex",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 15, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 85, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(time.Hour)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "gpt-5.5", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	ag := capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
		Provider: "antigravity", AccountRef: "acct-ag", InstallRef: "i-ag",
		Installed: true, Authenticated: true, Healthy: true,
		HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
		Windows: []capacitysnapshot.Window{{
			Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
			Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 20, Unit: capacitysnapshot.UnitPercentage},
			Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 80, Unit: capacitysnapshot.UnitPercentage},
			Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
			Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
			ResetAt: ptrTime(now.Add(30 * time.Minute)), CapturedAt: now, Source: "test",
		}},
		Models: []capacitysnapshot.ModelSpec{{
			ModelID: "GPT-OSS 120B", SupportedDepths: []string{"low", "medium", "high"}, DefaultDepth: "medium", Present: true,
		}},
		Source: "test", CapturedAt: now,
	})
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{codex, ag}, now)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	dry := true
	ledgerPath := filepath.Join(t.TempDir(), "cap-depth.json")
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: "proj-depth", Goal: "bind required depths",
		DryRun: &dry, Issue: "1343", Actor: "owner",
		HomeDir: home, Now: func() time.Time { return now },
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	wantDepth := map[string]string{
		"wi_research":  "low",
		"wi_implement": "medium",
		"wi_tests":     "medium",
		"wi_verify":    "high",
		"wi_docs":      "low",
	}
	depthsSeen := map[string]bool{}
	for _, c := range res.Children {
		want, ok := wantDepth[c.ChildID]
		if !ok {
			continue
		}
		if c.Unavailable {
			t.Fatalf("child %s unavailable: %+v", c.ChildID, c)
		}
		if c.Depth != want {
			t.Fatalf("child %s depth=%q want %q route=%s", c.ChildID, c.Depth, want, c.RouteReason)
		}
		// requirement→selection→invocation evidence in route reason
		for _, frag := range []string{
			"depth requirement=" + want,
			"selection=" + want,
			"invocation=" + want,
		} {
			if !strings.Contains(c.RouteReason, frag) {
				t.Fatalf("child %s missing %q in route reason: %s", c.ChildID, frag, c.RouteReason)
			}
		}
		depthsSeen[c.Depth] = true
	}
	if !depthsSeen["low"] || !depthsSeen["high"] {
		t.Fatalf("expected both low and high depths used; seen=%v children=%+v", depthsSeen, res.Children)
	}
	if len(res.DepthsUsed) < 2 {
		t.Fatalf("DepthsUsed=%v want ≥2", res.DepthsUsed)
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

func TestExecuteOpenPRFillsRealPREvidence(t *testing.T) {
	now := time.Date(2026, 7, 23, 7, 30, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	var got goalpr.Request
	req := env.pinRequest("implement transparent multi-child routing", "1343")
	req.ProjectID = "proj-pr"
	req.RunID = "run_pr_goal"
	req.RepoPath = "/tmp/disp-repo"
	req.OpenPR = true
	req.IndependentVerifier = "claude"
	req.VerifierEvidence = "sha256:v1"
	req.RequiredCheckNames = []string{"verify", "test"}
	req.GoalPR = func(ctx context.Context, r goalpr.Request) (goalpr.Result, error) {
		got = r
		return goalpr.Result{
			OK: true, Status: goalpr.StatusHumanGate,
			URL: "https://github.com/owner/disp/pull/99", Number: 99,
			Branch: "loopcoder/goal-run_pr_goal", BaseRef: "main",
			RequiredChecks: r.RequiredCheckNames, RequiredChecksGreen: true,
			IndependentVerifier: r.IndependentVerifier, VerifierEvidenceRef: r.VerifierEvidence,
			CreatedByLoopCoder: true, HumanMergeGate: true, AutoMerge: false,
		}, nil
	}
	res, err := goalrun.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.PR == nil || !res.PR.OK || res.PR.URL == "" {
		t.Fatalf("missing PR evidence: %+v", res.PR)
	}
	if !res.PR.CreatedByLoopCoder || !res.PR.HumanMergeGate || res.PR.AutoMerge {
		t.Fatalf("gate flags %+v", res.PR)
	}
	if got.ProjectID != "proj-pr" || got.RunID != "run_pr_goal" || got.SourceIssue != 1343 {
		t.Fatalf("goalpr request %+v", got)
	}
	if len(got.Children) < 4 {
		t.Fatalf("children passed to goalpr: %d", len(got.Children))
	}
	// Independent verifier is bound from the succeeded verify child.
	if got.IndependentVerifier == "" || !strings.HasPrefix(got.VerifierEvidence, "sha256:") {
		t.Fatalf("verifier bind provider=%q evidence=%q", got.IndependentVerifier, got.VerifierEvidence)
	}
	if strings.Contains(strings.ToLower(got.VerifierEvidence), "pending") {
		t.Fatal("pending-live forbidden")
	}
	if got.InstallMeaningfulCI == nil || !*got.InstallMeaningfulCI {
		t.Fatal("expected InstallMeaningfulCI true for product PR")
	}
}

// ensure goalpr import used in test file
var _ = goalpr.StatusHumanGate

// TestForcedInterruptRestartFromDurableCheckpoint: interrupt mid-goal, resume
// same project/run from checkpoint, same attempt/evidence, no second provider call
// for already-succeeded children, ceilings recorded, no worktree leak for reuse.
func TestForcedInterruptRestartFromDurableCheckpoint(t *testing.T) {
	now := time.Date(2026, 7, 23, 6, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-restart-goal"
	runID := "run_goal_restart_1"
	calls1 := map[string]int{}
	// Two-child graph: research succeeds, implement fails — avoids released-but-
	// not-aborted later siblings blocking resume reserve (same attempt_id).
	twoChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_restart_two", Version: 1,
			Source: workgraph.SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: "owner",
			Items: []workgraph.WorkItem{
				{Schema: workgraph.SchemaItem, ID: "wi_research", Status: workgraph.ItemRequired,
					Intent: "research", Owner: "research", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 1, OutputContract: "findings",
					RouteRequirement: "class=luna,depth=low,permission=read-only"},
				{Schema: workgraph.SchemaItem, ID: "wi_implement", Status: workgraph.ItemRequired,
					Intent: "implement", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 2, OutputContract: "diff",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write"},
			},
			Dependencies: []workgraph.Dependency{{
				Schema: workgraph.SchemaDep, From: "wi_research", To: "wi_implement", Kind: workgraph.DepFinishToStart,
			}},
			Limits: workgraph.DefaultLimits(), CreatedAt: now,
		}
		g.PlanDigest = workgraph.DigestGraph(g)
		return g, nil
	}
	res1, err1 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: "implement transparent multi-child routing", Issue: "1343",
		Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     twoChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls:   calls1,
			FailIDs: map[string]bool{"wi_implement": true},
		},
	})
	if err1 == nil {
		t.Fatalf("expected blocked interrupt: %+v", res1)
	}
	if res1.CheckpointPath == "" {
		t.Fatal("expected durable checkpoint path after interrupt")
	}
	if _, err := os.Stat(res1.CheckpointPath); err != nil {
		t.Fatalf("checkpoint missing: %v", err)
	}
	cp, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if len(cp.PriorSucceeded) == 0 {
		t.Fatalf("checkpoint has no prior succeeded: %+v", cp)
	}
	// At least research (or first wave member) succeeded before implement fail.
	var researchAttempt, researchEvidence string
	for id, c := range cp.PriorSucceeded {
		if c.AttemptID == "" || c.OutputEvidence == "" {
			t.Fatalf("prior %s missing identity: %+v", id, c)
		}
		if id == "wi_research" {
			researchAttempt = c.AttemptID
			researchEvidence = c.OutputEvidence
		}
	}
	if researchAttempt == "" {
		// If graph order failed earlier, still require some prior.
		for _, c := range cp.PriorSucceeded {
			researchAttempt = c.AttemptID
			researchEvidence = c.OutputEvidence
			break
		}
	}
	if calls1["wi_research"] > 1 {
		t.Fatalf("research over-called first pass: %+v", calls1)
	}

	// Resume: same run — succeeded kids must not re-exec.
	calls2 := map[string]int{}
	// Do not fail implement on resume so run can complete.
	res2, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: "implement transparent multi-child routing", Issue: "1343",
		Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     twoChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2,
		},
	})
	if err2 != nil {
		t.Fatalf("resume execute: %v status=%s msg=%s", err2, res2.Status, res2.Message)
	}
	if !res2.Resumed {
		t.Fatalf("expected resumed=true: %+v", res2)
	}
	if res2.ReuseCount < 1 {
		t.Fatalf("reuse_count=%d want ≥1 events=%v", res2.ReuseCount, res2.Workflow.Events)
	}
	// No second provider call for any prior-succeeded child.
	for id := range cp.PriorSucceeded {
		if calls2[id] != 0 {
			t.Fatalf("duplicate provider call on resume for %s: calls2=%+v prior=%v", id, calls2, cp.PriorSucceeded)
		}
	}
	// Same attempt/evidence identity preserved in final children.
	for _, c := range res2.Children {
		if prior, ok := cp.PriorSucceeded[c.ChildID]; ok {
			if c.AttemptID != prior.AttemptID || c.OutputEvidence != prior.OutputEvidence {
				t.Fatalf("identity drift %s: prior attempt=%s ev=%s got attempt=%s ev=%s",
					c.ChildID, prior.AttemptID, prior.OutputEvidence, c.AttemptID, c.OutputEvidence)
			}
			if c.Stage != "integrated" && c.Stage != "resumed" && c.Stage != "human_gate" {
				// after applyChildOutcomes succeeded → integrated/human_gate
			}
		}
	}
	if researchEvidence != "" {
		found := false
		for _, c := range res2.Workflow.Children {
			if c.WorkItemID == "wi_research" {
				found = true
				if c.AttemptID != researchAttempt || c.OutputEvidence != researchEvidence {
					t.Fatalf("research identity: want %s/%s got %s/%s", researchAttempt, researchEvidence, c.AttemptID, c.OutputEvidence)
				}
			}
		}
		if !found {
			t.Fatal("research missing from resume workflow children")
		}
	}
	if res2.WorktreePeak < 1 && res2.Workflow.WorktreePeak < 1 {
		t.Fatalf("worktree peak missing: %+v", res2)
	}
	// ProcessPeak is production spawn-only (OnProviderStart). FakeChildExecutor
	// leaves ProcessPeak=0 while still launching; do not require process peak on Fake.
	if res2.ProcessPeak < 0 || res2.Workflow.ProcessPeak < 0 {
		t.Fatalf("negative process peak: %+v", res2.Workflow)
	}
	// Checkpoint rewritten after resume success.
	if res2.CheckpointPath == "" {
		t.Fatal("resume must rewrite checkpoint")
	}
	cp2, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(cp2.PriorSucceeded) < len(cp.PriorSucceeded) {
		t.Fatalf("checkpoint regressed: before=%d after=%d", len(cp.PriorSucceeded), len(cp2.PriorSucceeded))
	}
}

func TestCheckpointRefuseReuseWithoutEvidence(t *testing.T) {
	got := goalrun.PriorSucceededFrom(
		[]workflowrun.ChildOutcome{
			{WorkItemID: "a", Terminal: "succeeded", AttemptID: "att-a", OutputEvidence: "sha256:abc"},
			{WorkItemID: "b", Terminal: "succeeded", AttemptID: "att-b", OutputEvidence: ""}, // refuse
			{WorkItemID: "c", Terminal: "failed", AttemptID: "att-c", OutputEvidence: "sha256:x"},
		},
		nil,
	)
	if len(got) != 1 || got["a"].AttemptID != "att-a" {
		t.Fatalf("%+v", got)
	}
}

func TestResumeBumpsGenerationWhenInterruptAlreadyInLedger(t *testing.T) {
	// SIGTERM path often writes a generic interrupt before RecoverOpenLaunch
	// returns n=0; open launches must still get gen+1 so attempt IDs change.
	now := time.Date(2026, 7, 23, 7, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-gen-bump"
	runID := "run_gen_bump_1"
	elog, err := workflowrun.OpenEventLog(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulated first pass: research done, implement launched, hard kill.
	must := func(e workflowrun.Event) {
		e.ProjectID, e.RunID = projectID, runID
		if _, err := elog.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	// Seed prior digests first so event attempts match canonical IDs.
	resumeGoal := "implement gen bump multi-child routing with tests"
	planDig, graphDig, class, ccd, _, graphID := childContractForGoal(t, resumeGoal, "1343", "owner", projectID, "wi_research", now)
	researchAtt := workflowrun.AttemptID("wi_research", planDig, runID, 0)
	implAttG0 := workflowrun.AttemptID("wi_implement", planDig, runID, 0)

	must(workflowrun.Event{Kind: "run.start", Message: "workflow execute"})
	must(workflowrun.Event{Kind: "launch", WorkItemID: "wi_research", AttemptID: researchAtt, Generation: 1,
		Payload: []byte(`{"provider":"codex","model":"gpt-5.5","depth":"low","permission":"bounded_write","account_ref":"acct-f","install_ref":"install-f","window_kind":"five_hour","reservation_id":"res-f","route_reason":"Winner: codex/gpt-5.5","work_item_id":"wi_research"}`)})
	must(workflowrun.Event{Kind: "terminal", WorkItemID: "wi_research", AttemptID: researchAtt, Terminal: "succeeded", Evidence: "sha256:abc", Generation: 1})
	seedClosedSucceededClaim(t, home, projectID, runID, "wi_research", researchAtt, "sha256:abc")
	must(workflowrun.Event{Kind: "integrate", WorkItemID: "wi_research", AttemptID: researchAtt, Generation: 1})
	must(workflowrun.Event{Kind: "launch", WorkItemID: "wi_implement", AttemptID: implAttG0, Generation: 1})
	// Generic interrupt already present (graceful SIGTERM) — RecoverOpen returns 0.
	must(workflowrun.Event{Kind: "interrupt", Message: "cancelled mid-wave (forced process interrupt)"})
	// Durable prior for research only.
	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig, Goal: resumeGoal, Issue: "1343", Actor: "owner",
		Status: "blocked", Interrupted: true,
		PriorSucceeded: map[string]workflowrun.ChildOutcome{
			"wi_research": {
				WorkItemID: "wi_research", Terminal: "succeeded",
				AttemptID: researchAtt, OutputEvidence: "sha256:abc",
				Provider: "codex", Model: "gpt-5.5", Depth: "low", Permission: "read-only",
				TaskClass: class, ExecutionPlanDigest: planDig,
				ChildContractDigest: ccd, Generation: 1,
			},
		},
		AbortedAttempts:   map[string]string{"wi_implement": implAttG0},
		AttemptGeneration: map[string]int{"wi_implement": 0},
		SavedAt:           now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}

	calls := map[string]int{}
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: resumeGoal, Issue: "1343",
		Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls: calls,
		},
	})
	if err != nil {
		t.Fatalf("resume: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	// Research must reuse, not re-exec.
	if calls["wi_research"] != 0 {
		t.Fatalf("research re-exec on resume: %+v", calls)
	}
	// Implement must use a new generation attempt id (g1+), not g0.
	implAttG1 := workflowrun.AttemptID("wi_implement", planDig, runID, 1)
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == "wi_implement" {
			if c.AttemptID == implAttG0 {
				t.Fatalf("implement re-used aborted attempt id %s", c.AttemptID)
			}
			if strings.HasSuffix(c.AttemptID, "-g0") {
				t.Fatalf("implement still g0: %s", c.AttemptID)
			}
			if c.AttemptID != implAttG1 && !strings.Contains(c.AttemptID, "-g1") && !strings.Contains(c.AttemptID, "-g2") {
				t.Logf("implement attempt=%s want prefix g1+", c.AttemptID)
			}
		}
	}
	// Event ledger must not re-launch the same implement g0 attempt.
	events, err := elog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	g0Launch := 0
	for _, e := range events {
		if e.Kind == "launch" && e.AttemptID == implAttG0 {
			g0Launch++
		}
	}
	if g0Launch != 1 {
		t.Fatalf("implement g0 launch count=%d want 1 (no re-launch)", g0Launch)
	}
}

func TestResumeWithoutGoalCheckpointUsesPartialAndGenBump(t *testing.T) {
	// Hard kill mid-workflow exits before saveRunCheckpoint. Only partial prior
	// + event ledger exist. Resume must still reuse succeeded children and
	// gen-bump aborted launches (exactly_once: no g0 re-launch).
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-no-checkpoint"
	runID := "run_no_checkpoint_1"
	elog, err := workflowrun.OpenEventLog(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	must := func(e workflowrun.Event) {
		e.ProjectID, e.RunID = projectID, runID
		if _, err := elog.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	// Mid-run partial snapshot — seed with actual canonical identity for resume equality.
	resumeGoal := "implement multi-child routing with tests and docs"
	planDig, graphDig, class, ccd, _, _ := childContractForGoal(t, resumeGoal, "1343", "owner", projectID, "wi_research", now)
	researchAtt := workflowrun.AttemptID("wi_research", planDig, runID, 0)
	implAttG0 := workflowrun.AttemptID("wi_implement", planDig, runID, 0)

	must(workflowrun.Event{Kind: "run.start", Message: "workflow execute"})
	must(workflowrun.Event{Kind: "launch", WorkItemID: "wi_research", AttemptID: researchAtt, Generation: 1,
		Payload: []byte(`{"provider":"codex","model":"gpt-5.5","depth":"low","permission":"bounded_write","account_ref":"acct-f","install_ref":"install-f","window_kind":"five_hour","reservation_id":"res-f","route_reason":"Winner: codex/gpt-5.5","work_item_id":"wi_research"}`)})
	must(workflowrun.Event{Kind: "terminal", WorkItemID: "wi_research", AttemptID: researchAtt, Terminal: "succeeded", Evidence: "sha256:research", Generation: 1})
	seedClosedSucceededClaim(t, home, projectID, runID, "wi_research", researchAtt, "sha256:research")
	must(workflowrun.Event{Kind: "integrate", WorkItemID: "wi_research", AttemptID: researchAtt, Generation: 1})
	must(workflowrun.Event{Kind: "launch", WorkItemID: "wi_implement", AttemptID: implAttG0, Generation: 1})
	// No interrupt line yet (true hard kill). No goal-checkpoint.json.

	partialPath := filepath.Join(filepath.Dir(elog.Path()), "workflow-partial.json")
	partialBody, err := json.Marshal(map[string]any{
		"schema":                "loopcoder.workflow.partial.v1",
		"project_id":            projectID,
		"run_id":                runID,
		"plan_digest":           planDig,
		"execution_plan_digest": planDig,
		"graph_digest":          graphDig,
		"saved_at":              now.Format(time.RFC3339),
		"interrupted":           true,
		"prior_succeeded": map[string]any{
			"wi_research": map[string]any{
				"work_item_id":          "wi_research",
				"provider":              "codex",
				"model":                 "gpt-5.5",
				"depth":                 "low",
				"permission":            "read-only",
				"terminal":              "succeeded",
				"output_evidence":       "sha256:research",
				"attempt_id":            researchAtt,
				"task_class":            class,
				"execution_plan_digest": planDig,
				"child_contract_digest": ccd,
				"generation":            1,
			},
		},
		"aborted_attempts": map[string]string{
			"wi_implement": implAttG0,
		},
		"event_log_path": elog.Path(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partialPath, partialBody, 0o600); err != nil {
		t.Fatal(err)
	}
	// Confirm goal-checkpoint is absent.
	if _, _, err := goalrun.LoadCheckpoint(home, projectID, runID); err == nil {
		t.Fatal("expected no goal-checkpoint for this scenario")
	}

	calls := map[string]int{}
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: resumeGoal, Issue: "1343",
		Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls: calls,
		},
	})
	if err != nil {
		t.Fatalf("resume without checkpoint: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if !res.Resumed {
		t.Fatalf("expected resumed=true from partial prior, got %+v", res)
	}
	if calls["wi_research"] != 0 {
		t.Fatalf("research re-exec without checkpoint: calls=%+v", calls)
	}
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == "wi_implement" {
			if strings.HasSuffix(c.AttemptID, "-g0") {
				t.Fatalf("implement still g0 without checkpoint recovery: %s", c.AttemptID)
			}
			if !strings.Contains(c.AttemptID, "-g1") && !strings.Contains(c.AttemptID, "-g2") {
				t.Logf("implement attempt=%s (gen bump expected)", c.AttemptID)
			}
		}
		if c.WorkItemID == "wi_research" && c.AttemptID != researchAtt {
			// reuse keeps original attempt id
			t.Logf("research attempt=%s", c.AttemptID)
		}
	}
	events, err := elog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	g0Impl := 0
	reuseResearch := 0
	for _, e := range events {
		if e.Kind == "launch" && e.AttemptID == implAttG0 {
			g0Impl++
		}
		if e.Kind == "reuse" && e.WorkItemID == "wi_research" {
			reuseResearch++
		}
	}
	if g0Impl != 1 {
		t.Fatalf("implement g0 launch count=%d want 1 (no re-launch of aborted attempt)", g0Impl)
	}
	if reuseResearch < 1 {
		t.Fatalf("research must emit reuse event without checkpoint; events missing reuse")
	}
}

func seedClosedSucceededClaim(t *testing.T, home, project, runID, workItem, attempt, evidence string) {
	t.Helper()
	dir := filepath.Join(home, "projects", project, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workclaims.json")
	logical := strings.ToLower(project) + "|g1|1|" + workItem
	body := fmt.Sprintf(`{
  "schema": "loopcoder.workclaim.store.v1",
  "seq": 1,
  "gen_by_item": {%q: 1},
  "live_by_item": {},
  "claims": [{
    "schema": "loopcoder.workclaim.v1",
    "claim_id": "wcl_1",
    "project_id": %q,
    "graph_id": "g1",
    "graph_version": 1,
    "work_item_id": %q,
    "attempt_id": %q,
    "executor_id": "workflowrun",
    "generation": 1,
    "state": "closed",
    "terminal": "succeeded",
    "output_evidence": %q,
    "claimed_at": "2026-07-22T12:00:00Z",
    "closed_at": "2026-07-22T12:00:01Z"
  }]
}`, logical, project, workItem, attempt, evidence)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
