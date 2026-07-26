package workflowrun_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
)

// TestWorktreeLifecycle_RealGit_ReleaseAfterSuccess: with a real git repo,
// after primary success WorktreeActive=0 and path/list absent.
func TestWorktreeLifecycle_RealGit_ReleaseAfterSuccess(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{HomeDir: home, Now: t0},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-wt-git", RunID: "run_wt_git",
		Definition: workflowrun.OneNodeDefinition("g-wt-git", "impl"),
		Actor:      "owner", RepoPath: repo,
		Integrator: workflowrun.GitBranchIntegrator{Now: t0},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
				Permission: "bounded_write", AccountRef: "a", InstallRef: "i", WindowKind: "five_hour",
				ReservationID: "r", RouteReason: "pin"},
		},
	}))
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%s msg=%s", res.Status, res.Message)
	}
	if res.WorktreeActive != 0 {
		t.Fatalf("WorktreeActive=%d want 0", res.WorktreeActive)
	}
	if len(res.Children) != 1 {
		t.Fatalf("children=%+v", res.Children)
	}
	wt := res.Children[0].WorktreePath
	if wt == "" {
		t.Fatal("empty worktree path on outcome")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree path still present: %v", err)
	}
	cmd := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain")
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), wt) {
		t.Fatalf("git still lists worktree: %s\n%s", wt, out)
	}
}

// TestWorktreeLifecycle_PrimaryCleanupFailure_BlocksAlternate injects a real
// Service release failure (TestReleaseWorktree). Primary MU path must not reach
// human_gate/succeeded, must keep WorktreeActive truthful nonzero, retain path,
// and must not start alternate (zero second provider invocation, no g1 launch).
func TestWorktreeLifecycle_PrimaryCleanupFailure_BlocksAlternate(t *testing.T) {
	home := testHome(t)
	var releaseCalls atomic.Int32
	var retainedPath string
	calls := map[string]int{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0, FailModel: "model-unavailable-token", Calls: calls,
		},
		TestReleaseWorktree: func(repoPath, wtPath string) error {
			releaseCalls.Add(1)
			retainedPath = wtPath
			// Do not delete — force durable leak.
			return fmt.Errorf("injected cleanup failure for %s", wtPath)
		},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-wt-cleanfail", RunID: "run_wt_cleanfail",
		Definition: workflowrun.OneNodeDefinition("g-wt-cleanfail", "impl"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write", AccountRef: "acct-ag", InstallRef: "install-ag",
				WindowKind: "five_hour", ReservationID: "res-ag", RouteReason: "pin-bad"},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "model-unavailable-token", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-ag", InstallRef: "install-ag",
					WindowKind: "five_hour", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-codex", InstallRef: "install-codex",
					WindowKind: "five_hour", HardEligible: true},
			},
		},
	}))
	if err == nil {
		t.Fatalf("want blocked on cleanup failure: %+v", res)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatal("must not human_gate after primary cleanup failure")
	}
	if res.Status == "succeeded" || strings.EqualFold(res.Status, "succeeded") {
		t.Fatal("must not succeeded after primary cleanup failure")
	}
	if !strings.Contains(err.Error(), "injected cleanup failure") &&
		!strings.Contains(res.Message, "injected cleanup failure") &&
		!strings.Contains(res.Message, "worktree cleanup") {
		t.Fatalf("want cleanup failure in error: err=%v msg=%s", err, res.Message)
	}
	if res.WorktreeActive == 0 {
		t.Fatalf("WorktreeActive=%d want truthful nonzero while leaked", res.WorktreeActive)
	}
	if retainedPath == "" {
		t.Fatal("TestReleaseWorktree never called")
	}
	if _, serr := os.Stat(retainedPath); serr != nil {
		t.Fatalf("leaked path must remain present: %v path=%s", serr, retainedPath)
	}
	// Alternate must not run: FailModel would MU primary once; codex would be second call.
	// With FailModel matching only primary model, alternate success would call once more.
	if calls["only"] > 1 {
		t.Fatalf("alternate provider must not start: calls=%v", calls)
	}
	// No g1 launch in event log.
	if res.EventLogPath != "" {
		raw, _ := os.ReadFile(res.EventLogPath)
		if strings.Contains(string(raw), `-g1"`) || strings.Contains(string(raw), `-g1,`) {
			// attempt_id suffix -g1
			if strings.Count(string(raw), `"kind":"launch"`) > 1 {
				t.Fatalf("alternate launch must not appear: %s", raw)
			}
		}
		// Exactly one launch (primary) if any
		nLaunch := strings.Count(string(raw), `"kind":"launch"`)
		if nLaunch > 1 {
			t.Fatalf("launch count=%d want <=1 (no alternate)", nLaunch)
		}
	}
	if releaseCalls.Load() < 1 {
		t.Fatal("expected at least one release attempt")
	}
}

// TestWorktreeLifecycle_OnWorktreeAllocatedReject_ReleasesNewPath: when the
// allocation callback rejects (prior lease active), the just-created path is
// released and absent from filesystem/git list; prior lease is not decremented.
func TestWorktreeLifecycle_OnWorktreeAllocatedReject_ReleasesNewPath(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	// First allocation succeeds and is "held" by Service-style callback.
	var priorPath string
	fake := workflowrun.FakeChildExecutor{HomeDir: home, Now: t0}
	// First execute: enter lease
	res1, err := fake.Execute(context.Background(), workflowrun.ChildExecInput{
		ProjectID: "proj-wt-rej", GraphID: "g1", WorkItemID: "a", AttemptID: "att-a-x-g0",
		RepoPath: repo, BaseRef: "main",
		Route: workflowrun.ChildRoute{
			Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
			Permission: "bounded_write", AccountRef: "a", InstallRef: "i", WindowKind: "five_hour",
			ReservationID: "r", RouteReason: "pin",
		},
		OnWorktreeAllocated: func(p string) error {
			priorPath = p
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if priorPath == "" || res1.WorktreePath == "" {
		t.Fatal("first allocation empty")
	}
	if _, err := os.Stat(priorPath); err != nil {
		t.Fatalf("prior path missing: %v", err)
	}

	// Second execute: callback refuses because prior is still active; new path must be gone.
	var rejectedNew string
	res2, err2 := fake.Execute(context.Background(), workflowrun.ChildExecInput{
		ProjectID: "proj-wt-rej", GraphID: "g1", WorkItemID: "b", AttemptID: "att-b-x-g0",
		RepoPath: repo, BaseRef: "main",
		Route: workflowrun.ChildRoute{
			Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
			Permission: "bounded_write", AccountRef: "a", InstallRef: "i", WindowKind: "five_hour",
			ReservationID: "r", RouteReason: "pin",
		},
		OnWorktreeAllocated: func(p string) error {
			rejectedNew = p
			return fmt.Errorf("workflowrun: worktree still active at %s; refusing allocate %s", priorPath, p)
		},
	})
	if err2 == nil {
		t.Fatalf("want refuse second allocation: %+v", res2)
	}
	if rejectedNew == "" {
		t.Fatal("second path never allocated")
	}
	if rejectedNew == priorPath {
		t.Fatal("second path must differ from prior")
	}
	// New path cleaned (or empty WorktreePath on result if cleaned).
	if _, err := os.Stat(rejectedNew); !os.IsNotExist(err) {
		t.Fatalf("rejected new worktree must be absent: path=%s err=%v res.WorktreePath=%q", rejectedNew, err, res2.WorktreePath)
	}
	// Prior still present (not decremented/released by second callback).
	if _, err := os.Stat(priorPath); err != nil {
		t.Fatalf("prior lease path must remain: %v", err)
	}
	// git list must not include rejected path
	cmd := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain")
	gout, _ := cmd.CombinedOutput()
	if strings.Contains(string(gout), rejectedNew) {
		t.Fatalf("git still lists rejected worktree: %s\n%s", rejectedNew, gout)
	}
	// Production executor same contract (fixture path).
	var rejProd string
	prod := workflowrun.ProductionChildExecutor{HomeDir: home, Now: t0, AllowFixture: true}
	_, err3 := prod.Execute(context.Background(), workflowrun.ChildExecInput{
		ProjectID: "proj-wt-rej", GraphID: "g1", WorkItemID: "c", AttemptID: "att-c-x-g0",
		RepoPath: repo, BaseRef: "main",
		Route: workflowrun.ChildRoute{
			Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium",
			Permission: "bounded_write", AccountRef: "a", InstallRef: "i", WindowKind: "five_hour",
			ReservationID: "r", RouteReason: "pin",
		},
		OnWorktreeAllocated: func(p string) error {
			rejProd = p
			return fmt.Errorf("refuse production second")
		},
	})
	if err3 == nil {
		t.Fatal("production want refuse")
	}
	if rejProd == "" {
		t.Fatal("production path not allocated")
	}
	if _, err := os.Stat(rejProd); !os.IsNotExist(err) {
		t.Fatalf("production rejected path still present: %s", rejProd)
	}
}

// TestWorktreeLifecycle_PrimaryMU_ThenAlternate_ActiveZero: primary MU releases
// before alternate; final WorktreeActive=0.
func TestWorktreeLifecycle_PrimaryMU_ThenAlternate_ActiveZero(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0, FailModel: "model-unavailable-token",
		},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-wt-mu", RunID: "run_wt_mu",
		Definition: workflowrun.OneNodeDefinition("g-wt-mu", "impl"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write", AccountRef: "acct-ag", InstallRef: "install-ag",
				WindowKind: "five_hour", ReservationID: "res-ag", RouteReason: "pin-bad"},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "model-unavailable-token", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-ag", InstallRef: "install-ag",
					WindowKind: "five_hour", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium",
					Permission: "bounded_write", AccountRef: "acct-codex", InstallRef: "install-codex",
					WindowKind: "five_hour", HardEligible: true},
			},
		},
	}))
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%s msg=%s", res.Status, res.Message)
	}
	if res.WorktreeActive != 0 {
		t.Fatalf("WorktreeActive=%d want 0 after primary+alternate", res.WorktreeActive)
	}
	if res.WorktreePeak < 1 {
		t.Fatalf("WorktreePeak=%d", res.WorktreePeak)
	}
}
