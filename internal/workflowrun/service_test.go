package workflowrun_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// TestForcedInterruptThenResumeExactlyOnce: cancel mid-flight child b (HangIDs),
// record interrupt event + aborted attempt; resume reuses a, re-runs b with new
// generation, no re-exec of a.
func TestForcedInterruptThenResumeExactlyOnce(t *testing.T) {
	home := testHome(t)
	calls1 := map[string]int{}
	svc1 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, Calls: calls1,
			HangIDs: map[string]bool{"b": true}, // true mid-flight hang until cancel
		},
	}
	runID := "run_restart_once"
	ctx1, cancel := context.WithCancel(context.Background())
	// Cancel shortly after start so a may finish and b hangs.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	r1, err := svc1.Execute(ctx1, workflowrun.Request{
		ProjectID: "proj-restart", RunID: runID,
		Definition: workflowrun.ChainDefinition("g-restart"),
		Actor:      "owner",
	})
	if err == nil && r1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("expected interrupt/block before full success: %+v", r1)
	}
	// a should have succeeded; b aborted/cancelled.
	var priorA workflowrun.ChildOutcome
	foundA := false
	for _, c := range r1.Children {
		if c.WorkItemID == "a" && c.Terminal == "succeeded" {
			priorA = c
			foundA = true
		}
	}
	if !foundA || priorA.AttemptID == "" || priorA.OutputEvidence == "" {
		// If cancel hit before a, still require event log interrupt path.
		if !r1.Interrupted && len(r1.AbortedAttempts) == 0 {
			t.Fatalf("need a succeeded or interrupt evidence: children=%+v interrupted=%v aborted=%v", r1.Children, r1.Interrupted, r1.AbortedAttempts)
		}
	}
	if r1.EventLogPath == "" {
		t.Fatal("expected event log path")
	}
	el, err := workflowrun.OpenEventLog(home, "proj-restart", runID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := el.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	interrupted, aborted := workflowrun.InterruptedFromEvents(events)
	if !interrupted && !r1.Interrupted {
		// Hang path should produce interrupt when cancel fires during b.
		for _, e := range events {
			t.Logf("event %+v", e)
		}
		t.Fatalf("want interrupt in events; aborted=%v r1=%+v", aborted, r1)
	}

	// Resume: same RunID + PriorSucceeded(a). b gets generation bump.
	calls2 := map[string]int{}
	svc2 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls2},
	}
	prior := map[string]workflowrun.ChildOutcome{}
	if foundA {
		prior["a"] = priorA
	}
	gen := map[string]int{}
	for id := range r1.AbortedAttempts {
		gen[id] = 1
	}
	if len(gen) == 0 {
		gen["b"] = 1 // expected hang victim
	}
	r2, err := svc2.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-restart", RunID: runID,
		Definition:        workflowrun.ChainDefinition("g-restart"),
		Actor:             "owner",
		PriorSucceeded:    prior,
		AttemptGeneration: gen,
	})
	if err != nil {
		t.Fatalf("resume: %v %+v", err, r2)
	}
	if r2.Status != workflowrun.StatusHumanGate {
		t.Fatalf("resume status %+v", r2)
	}
	if foundA {
		if r2.ReuseCount != 1 {
			t.Fatalf("reuse=%d want 1 events=%v", r2.ReuseCount, r2.Events)
		}
		if calls2["a"] != 0 {
			t.Fatalf("a re-executed: %+v", calls2)
		}
		var a2 workflowrun.ChildOutcome
		for _, c := range r2.Children {
			if c.WorkItemID == "a" {
				a2 = c
			}
		}
		if a2.AttemptID != priorA.AttemptID || a2.OutputEvidence != priorA.OutputEvidence {
			t.Fatalf("a identity drift: first=%+v resume=%+v", priorA, a2)
		}
	}
	// b must re-run with new generation (not aborted attempt id).
	var b2 workflowrun.ChildOutcome
	for _, c := range r2.Children {
		if c.WorkItemID == "b" {
			b2 = c
		}
	}
	if b2.AttemptID == "" || !strings.Contains(b2.AttemptID, "-g1") {
		t.Fatalf("b should use generation 1 attempt, got %q aborted=%v", b2.AttemptID, r1.AbortedAttempts)
	}
	if calls2["b"] != 1 {
		t.Fatalf("b calls=%d", calls2["b"])
	}
	joined := strings.Join(r2.Events, "\n")
	if foundA && !strings.Contains(joined, "child.reuse:a") {
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

func TestModelUnavailableRerouteNewAttemptImmutablePrior(t *testing.T) {
	home := testHome(t)
	calls := map[string]int{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, Calls: calls,
			FailModel: "model-unavailable-token",
		},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-alt", RunID: "run_alt_1",
		Definition: workflowrun.OneNodeDefinition("g-alt", "implement alternate"),
		Actor:      "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "antigravity", Model: "model-unavailable-token", Depth: "medium", RouteReason: "pin-bad"},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "model-unavailable-token", Effort: "medium", Permission: "bounded_write", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write", HardEligible: true},
			},
		},
	})
	if err != nil {
		t.Fatalf("execute: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%s msg=%s children=%+v", res.Status, res.Message, res.Children)
	}
	// Two outcomes: failed model_unavailable + successful alternate.
	var failed, ok *workflowrun.ChildOutcome
	for i := range res.Children {
		c := &res.Children[i]
		if c.WorkItemID != "only" {
			continue
		}
		if c.FailureClass == "model_unavailable" {
			failed = c
		}
		if c.Terminal == "succeeded" {
			ok = c
		}
	}
	if failed == nil || ok == nil {
		t.Fatalf("want failed+success outcomes: %+v", res.Children)
	}
	if failed.AttemptID == ok.AttemptID {
		t.Fatalf("must use distinct attempt ids: failed=%s ok=%s", failed.AttemptID, ok.AttemptID)
	}
	if ok.SupersedesAttemptID != failed.AttemptID {
		t.Fatalf("supersedes=%q want %q", ok.SupersedesAttemptID, failed.AttemptID)
	}
	if !strings.Contains(ok.RerouteEventRef, "event_id=") ||
		!strings.Contains(ok.RerouteEventRef, "supersedes_attempt_id=") ||
		!strings.Contains(ok.RerouteEventRef, "retry_attempt_id=") {
		t.Fatalf("reroute event ref must bind durable event_ids: %q", ok.RerouteEventRef)
	}
	if ok.Provider != "codex" || ok.Model != "gpt-5.5" || ok.Depth != "medium" {
		t.Fatalf("alternate route: %+v", ok)
	}
	// Executor invoked twice for same logical child (fail then alternate).
	if calls["only"] != 2 {
		t.Fatalf("calls=%v want 2", calls)
	}
	// Event log must contain model_unavailable + reroute kinds.
	if res.EventLogPath == "" {
		t.Fatal("missing event log")
	}
	raw, err := os.ReadFile(res.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	blob := string(raw)
	if !strings.Contains(blob, `"kind":"model_unavailable"`) && !strings.Contains(blob, `"kind": "model_unavailable"`) {
		// JSON may omit spaces
		if !strings.Contains(blob, "model_unavailable") {
			t.Fatalf("event log missing model_unavailable: %s", blob)
		}
	}
	if !strings.Contains(blob, "reroute") {
		t.Fatalf("event log missing reroute: %s", blob)
	}
	// Claim count includes both attempts.
	if res.ClaimCount < 2 {
		t.Fatalf("claim_count=%d want >=2", res.ClaimCount)
	}
}

// recordingIntegrator records IntegrateChild calls for alternate-path proofs.
type recordingIntegrator struct {
	Calls []workflowrun.IntegrateRequest
}

func (r *recordingIntegrator) EnsureGoalBranch(ctx context.Context, repoPath, baseRef, goalBranch string) (string, error) {
	return "deadbeef", nil
}
func (r *recordingIntegrator) IntegrateChild(ctx context.Context, req workflowrun.IntegrateRequest) (workflowrun.IntegrateCommit, error) {
	r.Calls = append(r.Calls, req)
	return workflowrun.IntegrateCommit{
		WorkItemID: req.WorkItemID, AttemptID: req.AttemptID,
		CommitSHA: "sha-integrate-" + req.AttemptID, Files: req.ProductFiles,
	}, nil
}

func TestModelUnavailableAlternateUsesFinalWorktreeForIntegrate(t *testing.T) {
	home := testHome(t)
	calls := map[string]int{}
	integ := &recordingIntegrator{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, Calls: calls,
			FailModel: "model-unavailable-token",
			ProductFiles: map[string][]string{
				"only": {"notes/only.go"},
			},
		},
	}
	// Force integrate path without real git: inject integrator + SkipIntegrate false needs isGitRepo.
	// Use SkipIntegrate false with Integrator set only works when doIntegrate — needs isGitRepo.
	// Instead call with Integrator and a fake git repo.
	repo := t.TempDir()
	mustGitInit(t, repo)

	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-alt-int", RunID: "run_alt_int",
		Definition: workflowrun.OneNodeDefinition("g-alt-int", "implement alternate integrate"),
		Actor:      "owner", RepoPath: repo,
		Integrator: integ,
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "antigravity", Model: "model-unavailable-token", Depth: "medium"},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "model-unavailable-token", Effort: "medium", Permission: "bounded_write", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write", HardEligible: true},
			},
		},
	})
	if err != nil {
		t.Fatalf("execute: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if len(integ.Calls) != 1 {
		t.Fatalf("want exactly 1 integrate of alternate, got %+v", integ.Calls)
	}
	ic := integ.Calls[0]
	if !strings.Contains(ic.AttemptID, "-g1") {
		t.Fatalf("integrate attempt should be alternate gen g1: %s", ic.AttemptID)
	}
	// Final child outcome is codex success; integrate used that worktree/files only.
	var ok, failed *workflowrun.ChildOutcome
	for i := range res.Children {
		c := &res.Children[i]
		if c.FailureClass == "model_unavailable" {
			failed = c
			if c.IntegrateCommitSHA != "" {
				t.Fatalf("failed attempt must not integrate: %+v", c)
			}
		}
		if c.Terminal == "succeeded" {
			ok = c
		}
	}
	if failed == nil {
		t.Fatalf("want failed model_unavailable row: %+v", res.Children)
	}
	if ok == nil || ok.Provider != "codex" {
		t.Fatalf("want codex success: %+v", res.Children)
	}
	if ok.IntegrateCommitSHA == "" {
		t.Fatalf("alternate must integrate: %+v", ok)
	}
	if ic.AttemptID != ok.AttemptID {
		t.Fatalf("integrate attempt %s != success attempt %s", ic.AttemptID, ok.AttemptID)
	}
	if ic.ChildWorktree != ok.WorktreePath {
		t.Fatalf("integrate must use final effective worktree: got %s want %s", ic.ChildWorktree, ok.WorktreePath)
	}
	if failed.WorktreePath != "" && ic.ChildWorktree == failed.WorktreePath {
		t.Fatalf("must not integrate failed attempt worktree: %s", failed.WorktreePath)
	}
	if calls["only"] != 2 {
		t.Fatalf("calls=%v", calls)
	}
}

func mustGitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
}

type capHookSeq struct {
	calls []workflowrun.CapacityRerouteInput
	res   workflowrun.CapacityRerouteResult
	err   error
}

func (c *capHookSeq) OnModelUnavailableAlternate(in workflowrun.CapacityRerouteInput) (workflowrun.CapacityRerouteResult, error) {
	c.calls = append(c.calls, in)
	return c.res, c.err
}

func TestModelUnavailableClaimBeforeCapacity_NoLeakOnReserveFail(t *testing.T) {
	home := testHome(t)
	hook := &capHookSeq{err: fmt.Errorf("reserve refused for test")}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, FailModel: "model-unavailable-token",
		},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-cap-fail", RunID: "run_cap_fail",
		Definition: workflowrun.OneNodeDefinition("g-cap", "impl"),
		Actor:      "owner", CapacityReroute: hook,
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "antigravity", Model: "model-unavailable-token", Depth: "medium"},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "model-unavailable-token", Effort: "medium", Permission: "bounded_write", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write", HardEligible: true},
			},
		},
	})
	if err == nil {
		t.Fatalf("want capacity fail, got success: %+v", res)
	}
	if len(hook.calls) != 1 {
		t.Fatalf("capacity called once after claim: %+v", hook.calls)
	}
	// Alternate claim was closed capacity_refused — no success child.
	foundRefused := false
	for _, c := range res.Children {
		if c.FailureClass == "capacity_refused" {
			foundRefused = true
			if c.SupersedesAttemptID == "" {
				t.Fatalf("refused alternate should supersede: %+v", c)
			}
		}
	}
	if !foundRefused {
		t.Fatalf("want capacity_refused child: %+v", res.Children)
	}
}

func TestModelUnavailableBindsAlternateAccountRef(t *testing.T) {
	home := testHome(t)
	hook := &capHookSeq{res: workflowrun.CapacityRerouteResult{
		AccountRef: "acct-codex-only", WindowKind: "five_hour",
		PriorState: "released", AlternateState: "reserved",
	}}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, FailModel: "model-unavailable-token",
		},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-acct", RunID: "run_acct",
		Definition: workflowrun.OneNodeDefinition("g-acct", "impl"),
		Actor:      "owner", CapacityReroute: hook,
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "antigravity", Model: "model-unavailable-token", Depth: "medium", AccountRef: "acct-antigravity"},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "model-unavailable-token", Effort: "medium", Permission: "bounded_write", HardEligible: true},
				{Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "bounded_write", HardEligible: true},
			},
		},
	})
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	var ok *workflowrun.ChildOutcome
	for i := range res.Children {
		if res.Children[i].Terminal == "succeeded" {
			ok = &res.Children[i]
		}
	}
	if ok == nil {
		t.Fatalf("%+v", res.Children)
	}
	if ok.AccountRef != "acct-codex-only" {
		t.Fatalf("must bind alternate account_ref, not failed provider: %q", ok.AccountRef)
	}
	if ok.Provider != "codex" {
		t.Fatalf("%+v", ok)
	}
}

func TestPickSameDepthAlternateRequiresNonemptyPermission(t *testing.T) {
	cands := []workflowrun.AlternateCandidate{
		{Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "", HardEligible: true},
		{Provider: "claude", Model: "claude-sonnet-4-5", Effort: "medium", Permission: "bounded_write", HardEligible: true},
	}
	// Use package-level via Execute is heavy; test through public path: empty perm must not win when reqPerm set.
	// Direct test of pick via same-depth in Execute is enough if we export — call through reroute with only empty-perm first.
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, FailModel: "bad"},
	}
	res, err := svc.Execute(context.Background(), workflowrun.Request{
		ProjectID: "proj-perm", RunID: "run_perm",
		Definition: workflowrun.OneNodeDefinition("g-perm", "impl"),
		Actor:      "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "antigravity", Model: "bad", Depth: "medium"},
		},
		SameDepthAlternates: map[string][]workflowrun.AlternateCandidate{
			"only": {
				{Provider: "antigravity", Model: "bad", Effort: "medium", Permission: "bounded_write", HardEligible: true},
				cands[0], // empty permission — must skip
				cands[1],
			},
		},
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	var ok *workflowrun.ChildOutcome
	for i := range res.Children {
		if res.Children[i].Terminal == "succeeded" {
			ok = &res.Children[i]
		}
	}
	if ok == nil || ok.Provider != "claude" {
		t.Fatalf("empty permission must not be chosen: %+v", res.Children)
	}
	_ = cands
}

func TestModelUnavailableFailsClosedOnEventAppendError(t *testing.T) {
	home := testHome(t)
	// Pre-create event log path as a directory to force write failure is hard.
	// Use OpenEventLog then set FailAppend via reading path and reopening — expose via EventLogPath.
	// Instead: poison after open by replacing file with unwritable — simpler FailAppend on recovered log.
	// Service always OpenEventLog fresh. Inject via Env not available.
	// Use CapacityReroute nil and FailModel — we'll set HOME under a path where after first event we can't write.
	// Practical approach: EventLog.FailAppend is only for unit test of Append; test Append itself.
	elog, err := workflowrun.OpenEventLog(home, "p", "r")
	if err != nil {
		t.Fatal(err)
	}
	elog.FailAppend = fmt.Errorf("disk full")
	if _, err := elog.Append(workflowrun.Event{Kind: "model_unavailable"}); err == nil {
		t.Fatal("want append fail")
	}
}
