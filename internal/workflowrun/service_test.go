package workflowrun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowdef"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func t0() time.Time { return time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC) }

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

func TestOneNodeReachesHumanGate(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: workflowrun.OneNodeDefinition("g1", "docs"),
		Actor: "owner",
	})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("%+v", res)
	}
	if res.ClaimCount != 1 || res.LaunchCount != 1 {
		t.Fatalf("claims/launches %+v", res)
	}
	if res.AutoMerge {
		t.Fatal("auto_merge")
	}
	if !strings.Contains(strings.Join(res.Events, "\n"), "human_gate.await_owner") {
		t.Fatalf("events %v", res.Events)
	}
	if len(res.Children) != 1 {
		t.Fatalf("children %+v", res.Children)
	}
	c := res.Children[0]
	if c.OutputEvidence == "" || !strings.HasPrefix(c.OutputEvidence, "sha256:") {
		t.Fatalf("want real evidence digest, got %+v", c)
	}
	if c.WorktreePath == "" {
		t.Fatal("missing worktree")
	}
	if _, err := os.Stat(filepath.Join(c.WorktreePath, ".loopcoder-owned-worktree")); err != nil {
		t.Fatalf("worktree marker: %v", err)
	}
	if c.Terminal != "succeeded" {
		t.Fatalf("terminal %+v", c)
	}
}

func TestThreeNodeChainClaimOnce(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: workflowrun.ChainDefinition("g3"),
		Actor: "owner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClaimCount != 3 || res.LaunchCount != 3 {
		t.Fatalf("%+v", res)
	}
	if len(res.Integrated) != 3 {
		t.Fatalf("integrated %v", res.Integrated)
	}
	// deterministic order a,b,c
	if strings.Join(res.Integrated, ",") != "a,b,c" {
		t.Fatalf("order %v", res.Integrated)
	}
	for _, c := range res.Children {
		if c.OutputEvidence == "" || c.WorktreePath == "" {
			t.Fatalf("child missing evidence/worktree: %+v", c)
		}
	}
}

func TestCyclicCreatesNoClaims(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0},
	}
	def := workflowdef.Definition{
		SchemaVersion: 1, GraphID: "bad", Source: "explicit_definition",
		Items: []workflowdef.DefItem{
			{ID: "a", Intent: "A", Status: "required", IntegrationOrder: 1},
			{ID: "b", Intent: "B", Status: "required", IntegrationOrder: 2},
		},
		Deps: []workflowdef.DefDep{
			{From: "a", To: "b", Kind: "finish_to_start"},
			{From: "b", To: "a", Kind: "finish_to_start"},
		},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: def, Actor: "owner",
	})
	if err == nil {
		t.Fatalf("expected error: %+v", res)
	}
	if res.ClaimCount != 0 || res.LaunchCount != 0 {
		t.Fatalf("side effects: %+v", res)
	}
	if res.Status != workflowrun.StatusInvalid {
		t.Fatalf("status %s", res.Status)
	}
}

func TestRequiredChildFailureBlocksParent(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0,
			FailIDs: map[string]bool{"only": true},
		},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: workflowrun.OneNodeDefinition("g-fail", "boom"),
		Actor: "owner",
	})
	if err == nil {
		t.Fatalf("expected error: %+v", res)
	}
	if res.Status != workflowrun.StatusBlocked {
		t.Fatalf("status %s", res.Status)
	}
	if len(res.Children) != 1 || res.Children[0].Terminal != "failed" {
		t.Fatalf("children %+v", res.Children)
	}
}

func TestProductionFixtureExecutorWritesEvidence(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		// Explicit production executor with fixture route — no live provider.
		Executor: workflowrun.ProductionChildExecutor{HomeDir: home, Now: t0},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: workflowrun.OneNodeDefinition("g-prod", "fixture path"),
		Actor: "owner", Provider: "fixture", Model: "fixture-model",
	})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("%+v", res)
	}
	c := res.Children[0]
	if !strings.HasPrefix(c.OutputEvidence, "sha256:") {
		t.Fatalf("evidence %+v", c)
	}
	if c.ActualSource != "unknown" {
		t.Fatalf("fixture must not invent actual capacity: %+v", c)
	}
	ev := filepath.Join(c.WorktreePath, ".loopcoder", "child-evidence", "only.json")
	if _, err := os.Stat(ev); err != nil {
		t.Fatalf("evidence file: %v", err)
	}
}

func TestPerChildRoutesPropagate(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", Definition: workflowrun.ChainDefinition("g-routes"),
		Actor: "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"a": {Provider: "codex", Model: "gpt-5.5", Depth: "high", RouteReason: "r-a"},
			"b": {Provider: "gemini", Model: "gemini-2.5", Depth: "medium", RouteReason: "r-b"},
			"c": {Provider: "codex", Model: "gpt-5.5", Depth: "low", RouteReason: "r-c"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Children) != 3 {
		t.Fatalf("%+v", res.Children)
	}
	want := []string{"codex", "gemini", "codex"}
	for i, c := range res.Children {
		if c.Provider != want[i] {
			t.Fatalf("child %d provider %s want %s", i, c.Provider, want[i])
		}
		if c.OutputEvidence == "" {
			t.Fatalf("missing evidence %+v", c)
		}
	}
}

// TestForcedInterruptThenResumeExactlyOnce proves production resume path:
// interrupt after first child succeeds, resume with PriorSucceeded reuses
// claim/attempt/output without a second provider call.
func TestForcedInterruptThenResumeExactlyOnce(t *testing.T) {
	home := testHome(t)
	calls1 := map[string]int{}
	svc1 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, Calls: calls1,
			FailIDs: map[string]bool{"b": true}, // forced interrupt after a
		},
	}
	runID := "run_restart_once"
	r1, err := svc1.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-restart", RunID: runID,
		Definition: workflowrun.ChainDefinition("g-restart"),
		Actor:      "owner",
	})
	if err == nil {
		t.Fatalf("expected interrupt/block: %+v", r1)
	}
	if r1.Status != workflowrun.StatusBlocked {
		t.Fatalf("status %+v", r1)
	}
	var priorA workflowrun.ChildOutcome
	foundA := false
	for _, c := range r1.Children {
		if c.WorkItemID == "a" && c.Terminal == "succeeded" {
			priorA = c
			foundA = true
		}
	}
	if !foundA || priorA.AttemptID == "" || priorA.OutputEvidence == "" {
		t.Fatalf("need durable a outcome: %+v", r1.Children)
	}
	if calls1["a"] != 1 || calls1["b"] != 1 {
		t.Fatalf("first pass calls %+v", calls1)
	}
	if r1.ProcessPeak < 1 || r1.WorktreePeak < 1 {
		t.Fatalf("ceilings missing: proc=%d wt=%d", r1.ProcessPeak, r1.WorktreePeak)
	}

	// Resume: same RunID + PriorSucceeded(a). Only b,c should execute.
	calls2 := map[string]int{}
	svc2 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls2},
	}
	r2, err := svc2.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-restart", RunID: runID,
		Definition: workflowrun.ChainDefinition("g-restart"),
		Actor:      "owner",
		PriorSucceeded: map[string]workflowrun.ChildOutcome{
			"a": priorA,
		},
	})
	if err != nil {
		t.Fatalf("resume: %v %+v", err, r2)
	}
	if r2.Status != workflowrun.StatusHumanGate {
		t.Fatalf("resume status %+v", r2)
	}
	if r2.ReuseCount != 1 {
		t.Fatalf("reuse=%d want 1 events=%v", r2.ReuseCount, r2.Events)
	}
	if r2.ClaimCount != 2 || r2.LaunchCount != 2 {
		t.Fatalf("claims/launches for remaining only: claim=%d launch=%d", r2.ClaimCount, r2.LaunchCount)
	}
	if calls2["a"] != 0 {
		t.Fatalf("a re-executed (duplicate provider call): %+v", calls2)
	}
	if calls2["b"] != 1 || calls2["c"] != 1 {
		t.Fatalf("remaining calls %+v", calls2)
	}
	// Same attempt_id + output evidence for a (exactly-once identity).
	var a2 workflowrun.ChildOutcome
	for _, c := range r2.Children {
		if c.WorkItemID == "a" {
			a2 = c
		}
	}
	if a2.AttemptID != priorA.AttemptID || a2.OutputEvidence != priorA.OutputEvidence {
		t.Fatalf("a identity drift: first=%+v resume=%+v", priorA, a2)
	}
	// No second evidence write for a: worktree path reused, file still one digest.
	if a2.WorktreePath != priorA.WorktreePath {
		t.Fatalf("worktree changed on reuse: %q vs %q", priorA.WorktreePath, a2.WorktreePath)
	}
	if r2.ProcessPeak != 2 {
		t.Fatalf("process peak on resume should be remaining launches only: %d", r2.ProcessPeak)
	}
	joined := strings.Join(r2.Events, "\n")
	if !strings.Contains(joined, "child.reuse:a") {
		t.Fatalf("missing reuse event: %v", r2.Events)
	}
}

func TestPriorSucceededNegativeMissingEvidenceRefusesReuse(t *testing.T) {
	home := testHome(t)
	calls := map[string]int{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	// Empty evidence must NOT skip re-exec (fail-closed reuse gate).
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj", RunID: "run_neg",
		Definition: workflowrun.OneNodeDefinition("g-neg", "docs"),
		Actor:      "owner",
		PriorSucceeded: map[string]workflowrun.ChildOutcome{
			"only": {
				WorkItemID: "only", Terminal: "succeeded", AttemptID: "att-only-x",
				OutputEvidence: "", // missing → re-exec
			},
		},
	})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.ReuseCount != 0 {
		t.Fatalf("must not reuse without evidence: %+v", res)
	}
	if calls["only"] != 1 {
		t.Fatalf("expected re-exec calls=%+v", calls)
	}
}
