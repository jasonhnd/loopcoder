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
