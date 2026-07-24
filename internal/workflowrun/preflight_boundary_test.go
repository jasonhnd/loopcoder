package workflowrun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// countingIntegrator records EnsureGoalBranch / IntegrateChild without touching git.
type countingIntegrator struct {
	EnsureN    int
	IntegrateN int
}

func (c *countingIntegrator) EnsureGoalBranch(ctx context.Context, repoPath, baseRef, goalBranch string) (string, error) {
	c.EnsureN++
	return "fake-head", nil
}

func (c *countingIntegrator) IntegrateChild(ctx context.Context, req workflowrun.IntegrateRequest) (workflowrun.IntegrateCommit, error) {
	c.IntegrateN++
	return workflowrun.IntegrateCommit{CommitSHA: "deadbeef", Skipped: true}, nil
}

// assertZeroSideEffects checks pure-preflight failure left no durable spend.
func assertZeroSideEffects(t *testing.T, home, project, runID string, integ *countingIntegrator, calls map[string]int) {
	t.Helper()
	if integ != nil && integ.EnsureN != 0 {
		t.Fatalf("EnsureGoalBranch calls=%d want 0", integ.EnsureN)
	}
	if integ != nil && integ.IntegrateN != 0 {
		t.Fatalf("IntegrateChild calls=%d want 0", integ.IntegrateN)
	}
	for id, n := range calls {
		if n != 0 {
			t.Fatalf("executor calls[%s]=%d want 0", id, n)
		}
	}
	runDir := filepath.Join(home, "projects", project, "runs", runID)
	if _, err := os.Stat(filepath.Join(runDir, "workflow-events.jsonl")); err == nil {
		t.Fatal("workflow-events.jsonl must not exist after pure-preflight fail")
	}
	if _, err := os.Stat(filepath.Join(runDir, "workclaims.json")); err == nil {
		t.Fatal("workclaims.json must not exist after pure-preflight fail")
	}
	// No goal worktree mutation under home.
	if entries, _ := os.ReadDir(filepath.Join(home, "projects", project)); len(entries) > 0 {
		// projects/<id> may not exist at all — also fine
		for _, e := range entries {
			if e.Name() == "runs" {
				// runs dir must not contain this runID
				if _, err := os.Stat(runDir); err == nil {
					// run dir existence alone is a side effect of eventlog/claims
					t.Fatalf("run dir created under pure-preflight fail: %s", runDir)
				}
			}
		}
	}
}

func baseOneNodeReq(project, runID string) workflowrun.Request {
	return workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-preflight", "impl"),
		Actor:      "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "fixture", Model: "fixture-model", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-f", WindowKind: "five_hour",
				ReservationID: "res-f", InstallRef: "install-f", RouteReason: "test",
			},
		},
	}
}

func TestPreflightPlanMismatch_ZeroSideEffects(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-pf-plan", "run_pf_plan"
	calls := map[string]int{}
	integ := &countingIntegrator{}
	// Real git repo so doIntegrate would be true if we reached side effects.
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req := withExpectedPlanDigest(t, baseOneNodeReq(project, runID))
	req.RepoPath = repo
	req.Integrator = integ
	req.ExpectedPlanDigest = "sha256:not-the-real-plan-digest-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected plan mismatch fail: %+v", res)
	}
	if !strings.Contains(err.Error(), "plan") && !strings.Contains(res.Message, "plan") {
		// fail() returns error via StatusInvalid path
		if res.Status != workflowrun.StatusInvalid && !strings.Contains(res.Message, "digest mismatch") {
			t.Fatalf("want plan mismatch, got status=%s msg=%s err=%v", res.Status, res.Message, err)
		}
	}
	assertZeroSideEffects(t, home, project, runID, integ, calls)
}

func TestPreflightGraphMismatch_ZeroSideEffects(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-pf-graph", "run_pf_graph"
	calls := map[string]int{}
	integ := &countingIntegrator{}
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req := withExpectedPlanDigest(t, baseOneNodeReq(project, runID))
	req.RepoPath = repo
	req.Integrator = integ
	req.ExpectedGraphDigest = "sha256:not-the-real-graph-digest-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected graph mismatch: %+v", res)
	}
	if res.Status != workflowrun.StatusInvalid && !strings.Contains(res.Message, "graph digest mismatch") {
		t.Fatalf("status=%s msg=%s err=%v", res.Status, res.Message, err)
	}
	assertZeroSideEffects(t, home, project, runID, integ, calls)
}

func TestPreflightChildRouteMismatch_ZeroSideEffects(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-pf-route", "run_pf_route"
	calls := map[string]int{}
	integ := &countingIntegrator{}
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req := withExpectedPlanDigest(t, baseOneNodeReq(project, runID))
	req.RepoPath = repo
	req.Integrator = integ
	// ChildRoute TaskClass mismatches Definition RouteRequirement (tera) → contract fail.
	req.ChildRoutes = map[string]workflowrun.ChildRoute{
		"only": {
			Provider: "fixture", Model: "fixture-model", TaskClass: "luna",
			Depth: "medium", Permission: "bounded_write",
			AccountRef: "acct-f", WindowKind: "five_hour", InstallRef: "install-f",
		},
	}
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected child route/contract fail: %+v", res)
	}
	if res.Status != workflowrun.StatusInvalid && !strings.Contains(res.Message, "ChildRoute") && !strings.Contains(err.Error(), "ChildRoute") {
		t.Fatalf("want ChildRoute mismatch, got status=%s msg=%s err=%v", res.Status, res.Message, err)
	}
	assertZeroSideEffects(t, home, project, runID, integ, calls)
}

func TestPreflightInvalidPrior_ZeroSideEffects(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-pf-prior", "run_pf_prior"
	calls := map[string]int{}
	integ := &countingIntegrator{}
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req := withExpectedPlanDigest(t, baseOneNodeReq(project, runID))
	req.RepoPath = repo
	req.Integrator = integ
	req.PriorSucceeded = map[string]workflowrun.ChildOutcome{
		"only": {
			WorkItemID: "only", Terminal: "succeeded", AttemptID: "att-bogus-g0",
			OutputEvidence: "sha256:ev", Generation: 1, TaskClass: "tera",
			ExecutionPlanDigest: req.ExpectedPlanDigest,
			ChildContractDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		},
	}
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected invalid prior fail (err must be non-nil): %+v", res)
	}
	if !strings.Contains(err.Error(), "attempt_id") && !strings.Contains(err.Error(), "prior") {
		t.Fatalf("want prior/attempt error class, got %v (status=%s msg=%s)", err, res.Status, res.Message)
	}
	assertZeroSideEffects(t, home, project, runID, integ, calls)
}

func TestPreflightGhostPrior_ZeroSideEffects(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-pf-ghost", "run_pf_ghost"
	calls := map[string]int{}
	integ := &countingIntegrator{}
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req := withExpectedPlanDigest(t, baseOneNodeReq(project, runID))
	req.RepoPath = repo
	req.Integrator = integ
	req.PriorSucceeded = map[string]workflowrun.ChildOutcome{
		"not_in_graph": {
			WorkItemID: "not_in_graph", Terminal: "succeeded",
			AttemptID: "att-x", OutputEvidence: "sha256:ev", Generation: 1,
		},
	}
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected ghost prior fail (err must be non-nil): %+v", res)
	}
	if !strings.Contains(err.Error(), "ghost") && !strings.Contains(err.Error(), "not in current graph") {
		t.Fatalf("want ghost key error, got %v (status=%s msg=%s)", err, res.Status, res.Message)
	}
	assertZeroSideEffects(t, home, project, runID, integ, calls)
}

func TestPreflightPriorKeyValueMismatch_ZeroSideEffects(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-pf-kv", "run_pf_kv"
	calls := map[string]int{}
	integ := &countingIntegrator{}
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req := withExpectedPlanDigest(t, baseOneNodeReq(project, runID))
	req.RepoPath = repo
	req.Integrator = integ
	req.PriorSucceeded = map[string]workflowrun.ChildOutcome{
		"only": {
			WorkItemID: "other_id", Terminal: "succeeded",
			AttemptID: "att-x", OutputEvidence: "sha256:ev", Generation: 1,
		},
	}
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected key/value mismatch (err must be non-nil): %+v", res)
	}
	if !strings.Contains(err.Error(), "WorkItemID") && !strings.Contains(err.Error(), "work_item") {
		t.Fatalf("want WorkItemID mismatch error, got %v (status=%s msg=%s)", err, res.Status, res.Message)
	}
	assertZeroSideEffects(t, home, project, runID, integ, calls)
}

func TestPreflightNegativeAttemptGeneration_ZeroSideEffects(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-pf-neggen", "run_pf_neggen"
	calls := map[string]int{}
	integ := &countingIntegrator{}
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req := withExpectedPlanDigest(t, baseOneNodeReq(project, runID))
	req.RepoPath = repo
	req.Integrator = integ
	req.AttemptGeneration = map[string]int{"only": -1}
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected negative AttemptGeneration fail (err must be non-nil): %+v", res)
	}
	if !strings.Contains(err.Error(), "negative") && !strings.Contains(err.Error(), "AttemptGeneration") {
		t.Fatalf("want negative AttemptGeneration error, got %v (status=%s msg=%s)", err, res.Status, res.Message)
	}
	assertZeroSideEffects(t, home, project, runID, integ, calls)
}

func TestPreflightGhostAttemptGeneration_ZeroSideEffects(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-pf-ghostgen", "run_pf_ghostgen"
	calls := map[string]int{}
	integ := &countingIntegrator{}
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req := withExpectedPlanDigest(t, baseOneNodeReq(project, runID))
	req.RepoPath = repo
	req.Integrator = integ
	req.AttemptGeneration = map[string]int{"not_in_graph": 1}
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected ghost AttemptGeneration fail (err must be non-nil): %+v", res)
	}
	if !strings.Contains(err.Error(), "ghost") && !strings.Contains(err.Error(), "not in current graph") {
		t.Fatalf("want ghost AttemptGeneration error, got %v (status=%s msg=%s)", err, res.Status, res.Message)
	}
	assertZeroSideEffects(t, home, project, runID, integ, calls)
}
