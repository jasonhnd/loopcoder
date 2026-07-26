package workflowrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func TestValidateChildEventIdentity_MissingBothIDsFailClosed(t *testing.T) {
	if err := workflowrun.ValidateChildEventIdentity(workflowrun.Event{Kind: "launch"}); err == nil {
		t.Fatal("expected fail")
	}
}

func TestValidateChildEventIdentity_PartialChildInterruptFailClosed(t *testing.T) {
	if err := workflowrun.ValidateChildEventIdentity(workflowrun.Event{Kind: "interrupt", WorkItemID: "wi"}); err == nil {
		t.Fatal("expected fail")
	}
}

func TestIsParentInterrupt_RejectsNegativeGeneration(t *testing.T) {
	if workflowrun.IsParentInterrupt(workflowrun.Event{Kind: "interrupt", Generation: -1}) {
		t.Fatal("negative must not be parent interrupt")
	}
	if err := workflowrun.ValidateChildEventIdentity(workflowrun.Event{Kind: "interrupt", Generation: -1}); err == nil {
		t.Fatal("expected reject negative parent gen")
	}
}

func TestClaimGenerationFromAttemptID(t *testing.T) {
	g, err := workflowrun.ClaimGenerationFromAttemptID("att-x-g0")
	if err != nil || g != 1 {
		t.Fatalf("got %d %v", g, err)
	}
}

func oneNodeFixture(project, runID, graphID string) workflowrun.Request {
	return workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition: workflowrun.OneNodeDefinition(graphID, "impl"),
		Actor:      "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "fixture", Model: "fixture-model", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-f", InstallRef: "install-f", WindowKind: "five_hour",
				ReservationID: "res-f", RouteReason: "test",
			},
		},
	}
}

func requireKinds(t *testing.T, events []workflowrun.Event, kinds ...string) {
	t.Helper()
	seen := map[string]bool{}
	for _, ev := range events {
		seen[ev.Kind] = true
	}
	for _, k := range kinds {
		if !seen[k] {
			t.Fatalf("missing required persisted kind %q; saw %v", k, seen)
		}
	}
}

func validateAllChildEvents(t *testing.T, events []workflowrun.Event, id workflowrun.EventWriteIdentity, itemOK map[string]bool) {
	t.Helper()
	for _, ev := range events {
		if id.ProjectID == "" {
			id.ProjectID = ev.ProjectID
		}
		if err := workflowrun.ValidateChildEventIdentityForPlan(ev, id, itemOK); err != nil {
			t.Fatalf("kind=%s: %v", ev.Kind, err)
		}
		if workflowrun.ChildLifecycleKinds[ev.Kind] && !workflowrun.IsParentInterrupt(ev) {
			want, err := workflowrun.ClaimGenerationFromAttemptID(ev.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if ev.Generation != want {
				t.Fatalf("kind=%s gen=%d want %d att=%s", ev.Kind, ev.Generation, want, ev.AttemptID)
			}
		}
	}
}

func loadEvents(t *testing.T, home, project, runID string) []workflowrun.Event {
	t.Helper()
	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := elog.ReadAllForRun(project, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("empty log")
	}
	return events
}

// countingEnsureIntegrator counts EnsureGoalBranch without git side effects.
type countingEnsureIntegrator struct {
	EnsureN    int
	IntegrateN int
}

func (c *countingEnsureIntegrator) EnsureGoalBranch(ctx context.Context, repoPath, baseRef, goalBranch string) (string, error) {
	c.EnsureN++
	return "deadbeef", nil
}
func (c *countingEnsureIntegrator) IntegrateChild(ctx context.Context, req workflowrun.IntegrateRequest) (workflowrun.IntegrateCommit, error) {
	c.IntegrateN++
	return workflowrun.IntegrateCommit{
		WorkItemID: req.WorkItemID, AttemptID: req.AttemptID,
		CommitSHA: "sha-" + req.AttemptID, Files: req.ProductFiles,
	}, nil
}

// TestPersistedPrimarySuccessEvents_RequireAllKinds: claim/launch/pid/terminal/integrate.
func TestPersistedPrimarySuccessEvents_RequireAllKinds(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	project, runID := "proj-emit-primary", "run_emit_primary"
	integ := &countingEnsureIntegrator{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0,
			ProductFiles: map[string][]string{"only": {"notes/only.go"}},
		},
	}
	req := withExpectedPlanDigest(t, oneNodeFixture(project, runID, "g-emit-primary"))
	req.RepoPath = repo
	req.Integrator = integ
	res, err := svc.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("%v %+v", err, res)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%s msg=%s", res.Status, res.Message)
	}
	events := loadEvents(t, home, project, runID)
	validateAllChildEvents(t, events, workflowrun.EventWriteIdentity{
		ProjectID: events[0].ProjectID, RunID: runID,
		PlanDigest: res.PlanDigest, GraphDigest: res.GraphDigest,
		GraphID: res.GraphID, GraphVersion: res.GraphVersion,
	}, map[string]bool{"only": true})
	requireKinds(t, events, "claim", "launch", "pid", "terminal", "integrate")
	// Exact attempt/generation on child facts.
	var att string
	for _, ev := range events {
		if ev.Kind == "claim" {
			att = ev.AttemptID
			if ev.Generation != 1 {
				t.Fatalf("claim gen=%d want 1", ev.Generation)
			}
		}
	}
	if att == "" || !strings.HasSuffix(att, "-g0") {
		t.Fatalf("claim attempt=%q", att)
	}
	for _, kind := range []string{"launch", "pid", "terminal", "integrate"} {
		for _, ev := range events {
			if ev.Kind == kind && ev.WorkItemID == "only" {
				if ev.AttemptID != att {
					t.Fatalf("%s attempt %q != claim %q", kind, ev.AttemptID, att)
				}
				if ev.Generation != 1 {
					t.Fatalf("%s gen=%d want 1", kind, ev.Generation)
				}
			}
		}
	}
}

// TestPersistedAlternateRetryEvents_RequireAllKinds: MU, reroute, claim×2, launch×2, pid×2, terminal×2, integrate.
func TestPersistedAlternateRetryEvents_RequireAllKinds(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	project, runID := "proj-emit-alt", "run_emit_alt"
	integ := &countingEnsureIntegrator{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0, FailModel: "model-unavailable-token",
			ProductFiles: map[string][]string{"only": {"notes/only.go"}},
		},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID, RepoPath: repo, Integrator: integ,
		Definition: workflowrun.OneNodeDefinition("g-emit-alt", "implement alternate identity"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-ag", InstallRef: "install-ag", WindowKind: "five_hour",
				ReservationID: "res-ag-prior", RouteReason: "pin-bad",
			},
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
		t.Fatalf("%v status=%s msg=%s", err, res.Status, res.Message)
	}
	events := loadEvents(t, home, project, runID)
	validateAllChildEvents(t, events, workflowrun.EventWriteIdentity{
		ProjectID: events[0].ProjectID, RunID: runID,
		PlanDigest: res.PlanDigest, GraphDigest: res.GraphDigest,
		GraphID: res.GraphID, GraphVersion: res.GraphVersion,
	}, map[string]bool{"only": true})
	requireKinds(t, events, "model_unavailable", "reroute", "claim", "launch", "pid", "terminal", "integrate")
	// Count launches/claims >= 2 for alternate.
	nLaunch, nClaim, nPID, nTerm := 0, 0, 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case "launch":
			nLaunch++
		case "claim":
			nClaim++
		case "pid":
			nPID++
		case "terminal":
			nTerm++
		}
	}
	if nLaunch < 2 || nClaim < 2 || nPID < 2 || nTerm < 2 {
		t.Fatalf("want >=2 each launch/claim/pid/terminal; got L=%d C=%d P=%d T=%d", nLaunch, nClaim, nPID, nTerm)
	}
}

// TestPersistedClosedClaimTerminalReuse_ZeroSecondLaunch: PriorSucceeded empty,
// same attempt re-claim hits ResultTerminalReused; require reuse event; launch count 1.
func TestPersistedClosedClaimTerminalReuse_ZeroSecondLaunch(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-emit-treuse", "run_emit_treuse"
	// Production durable lifecycle requires real spawn identity + authority.
	svc1 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{HomeDir: home, Now: t0},
	}
	req := withExpectedPlanDigest(t, oneNodeFixture(project, runID, "g-emit-treuse"))
	r1, err := svc1.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("p1: %v", err)
	}
	if r1.LaunchCount != 1 {
		t.Fatalf("p1 launch=%d", r1.LaunchCount)
	}
	// Second process: empty PriorSucceeded — closed claim returns TerminalReused.
	calls := map[string]int{}
	svc2 := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{HomeDir: home, Now: t0, Calls: calls},
	}
	req2 := withExpectedPlanDigest(t, oneNodeFixture(project, runID, "g-emit-treuse"))
	// Explicit empty PriorSucceeded — force claim store reuse path.
	req2.PriorSucceeded = nil
	r2, err := svc2.Execute(context.Background(), req2)
	if err != nil {
		t.Fatalf("p2: %v status=%s msg=%s", err, r2.Status, r2.Message)
	}
	if calls["only"] != 0 {
		t.Fatalf("second launch forbidden: calls=%v", calls)
	}
	if r2.LaunchCount != 0 {
		t.Fatalf("p2 LaunchCount=%d want 0 (terminal reuse)", r2.LaunchCount)
	}
	if r2.ReuseCount < 1 {
		t.Fatalf("ReuseCount=%d", r2.ReuseCount)
	}
	var reused *workflowrun.ChildOutcome
	for i := range r2.Children {
		if r2.Children[i].WorkItemID == "only" && r2.Children[i].Terminal == "succeeded" {
			reused = &r2.Children[i]
			break
		}
	}
	if reused == nil {
		t.Fatalf("missing recovered child: %+v", r2.Children)
	}
	if reused.InstallRef == "" || reused.WindowKind == "" ||
		reused.ReservationID == "" || reused.RouteReason == "" {
		t.Fatalf("recovered route identity incomplete: %+v", reused)
	}
	events := loadEvents(t, home, project, runID)
	validateAllChildEvents(t, events, workflowrun.EventWriteIdentity{
		ProjectID: events[0].ProjectID, RunID: runID,
		PlanDigest: r2.PlanDigest, GraphDigest: r2.GraphDigest,
		GraphID: r2.GraphID, GraphVersion: r2.GraphVersion,
	}, map[string]bool{"only": true})
	requireKinds(t, events, "reuse")
	// Message should indicate durable claim+terminal restart reuse.
	sawTermReuse := false
	for _, ev := range events {
		if ev.Kind == "reuse" && strings.Contains(ev.Message, "durable claim+terminal restart reuse") {
			sawTermReuse = true
			want, _ := workflowrun.ClaimGenerationFromAttemptID(ev.AttemptID)
			if ev.Generation != want {
				t.Fatalf("reuse gen=%d want %d", ev.Generation, want)
			}
		}
	}
	if !sawTermReuse {
		// PriorSucceeded empty path uses Message "durable claim+terminal restart reuse"
		// Count reuse events at least.
		nReuse := 0
		for _, ev := range events {
			if ev.Kind == "reuse" {
				nReuse++
			}
		}
		if nReuse < 1 {
			t.Fatal("missing reuse event from ResultTerminalReused path")
		}
	}
}

// TestPersistedChildInterrupt_RequireInterruptAndPid: cancel only after HangIDs
// OnHangEntry handshake (PID-bearing running state), never via sleep.
func TestPersistedChildInterrupt_RequireInterruptAndPid(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-emit-int", "run_emit_int"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hangEntered := make(chan struct{})
	var hangOnce sync.Once
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0, Hang: true,
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID != "only" || pid <= 0 {
					return
				}
				hangOnce.Do(func() { close(hangEntered) })
			},
		},
	}
	go func() {
		<-hangEntered
		cancel()
	}()
	req := withExpectedPlanDigest(t, oneNodeFixture(project, runID, "g-emit-int"))
	res, err := svc.Execute(ctx, req)
	if res.EventLogPath == "" {
		t.Fatalf("no event log: err=%v res=%+v", err, res)
	}
	if res.PlanDigest == "" {
		t.Fatalf("empty plan: err=%v", err)
	}
	events := loadEvents(t, home, project, runID)
	validateAllChildEvents(t, events, workflowrun.EventWriteIdentity{
		ProjectID: events[0].ProjectID, RunID: runID,
		PlanDigest: res.PlanDigest, GraphDigest: res.GraphDigest,
		GraphID: res.GraphID, GraphVersion: res.GraphVersion,
	}, map[string]bool{"only": true})
	requireKinds(t, events, "claim", "launch", "pid", "interrupt")
	// Exact attempt shared across claim/launch/pid/interrupt.
	var att string
	for _, ev := range events {
		if ev.Kind == "claim" && ev.WorkItemID == "only" {
			att = ev.AttemptID
			break
		}
	}
	if att == "" {
		t.Fatal("no claim")
	}
	wantGen, errGen := workflowrun.ClaimGenerationFromAttemptID(att)
	if errGen != nil {
		t.Fatal(errGen)
	}
	for _, kind := range []string{"claim", "launch", "pid", "interrupt"} {
		found := false
		for _, ev := range events {
			if ev.Kind == kind && ev.WorkItemID == "only" {
				found = true
				if ev.AttemptID != att {
					t.Fatalf("%s att %q != %q", kind, ev.AttemptID, att)
				}
				if ev.Generation != wantGen {
					t.Fatalf("%s gen=%d want %d", kind, ev.Generation, wantGen)
				}
			}
		}
		if !found {
			t.Fatalf("missing %s for only", kind)
		}
	}
}

// TestPersistedAlternateChildInterrupt_RequireInterruptAndPid: MU primary then
// hang on alternate; cancel only after OnHangEntry; claim/launch/pid/interrupt
// must all bind the alternate attempt and generation.
func TestPersistedAlternateChildInterrupt_RequireInterruptAndPid(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-emit-alt-int", "run_emit_alt_int"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hangEntered := make(chan struct{})
	var hangOnce sync.Once
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0,
			FailModel: "model-unavailable-token",
			Hang:      true,
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID != "only" || pid <= 0 {
					return
				}
				hangOnce.Do(func() { close(hangEntered) })
			},
		},
	}
	go func() {
		<-hangEntered
		cancel()
	}()
	res, err := svc.Execute(ctx, withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-emit-alt-int", "implement alternate interrupt"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-ag", InstallRef: "install-ag", WindowKind: "five_hour",
				ReservationID: "res-ag-prior", RouteReason: "pin-bad",
			},
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
	if res.EventLogPath == "" {
		t.Fatalf("no event log: err=%v res=%+v", err, res)
	}
	if res.PlanDigest == "" {
		t.Fatalf("empty plan: err=%v", err)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatalf("interrupted alternate must not report human_gate: %+v", res)
	}
	events := loadEvents(t, home, project, runID)
	validateAllChildEvents(t, events, workflowrun.EventWriteIdentity{
		ProjectID: events[0].ProjectID, RunID: runID,
		PlanDigest: res.PlanDigest, GraphDigest: res.GraphDigest,
		GraphID: res.GraphID, GraphVersion: res.GraphVersion,
	}, map[string]bool{"only": true})
	requireKinds(t, events, "model_unavailable", "reroute", "claim", "launch", "pid", "interrupt")

	// Interrupt is the alternate-attempt proof anchor.
	var altAtt string
	var altGen int
	for _, ev := range events {
		if ev.Kind == "interrupt" && ev.WorkItemID == "only" {
			altAtt = ev.AttemptID
			altGen = ev.Generation
			break
		}
	}
	if altAtt == "" {
		t.Fatal("missing alternate interrupt event")
	}
	if !strings.HasSuffix(altAtt, "-g1") {
		t.Fatalf("alternate interrupt attempt=%q want -g1 suffix", altAtt)
	}
	wantGen, errGen := workflowrun.ClaimGenerationFromAttemptID(altAtt)
	if errGen != nil {
		t.Fatal(errGen)
	}
	if altGen != wantGen {
		t.Fatalf("interrupt gen=%d want %d", altGen, wantGen)
	}
	for _, kind := range []string{"claim", "launch", "pid", "interrupt"} {
		found := false
		for _, ev := range events {
			if ev.Kind != kind || ev.WorkItemID != "only" || ev.AttemptID != altAtt {
				continue
			}
			found = true
			if ev.Generation != wantGen {
				t.Fatalf("alt %s gen=%d want %d att=%s", kind, ev.Generation, wantGen, altAtt)
			}
		}
		if !found {
			t.Fatalf("missing alt %s for attempt %s", kind, altAtt)
		}
	}
}

// TestInvalidLog_BeforeEnsureGoalBranch_ZeroGit: invalid log => EnsureN=0.
func TestInvalidLog_BeforeEnsureGoalBranch_ZeroGit(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	project, runID := "proj-pre-git", "run_pre_git"
	req := withExpectedPlanDigest(t, oneNodeFixture(project, runID, "g-pre-git"))
	dir := filepath.Join(home, "projects", project, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "workflow-events.jsonl")
	// Missing generation on launch.
	att := workflowrun.AttemptID("only", req.ExpectedPlanDigest, runID, 0)
	ev := workflowrun.Event{
		Schema: workflowrun.EventSchema, ProjectID: project, RunID: runID,
		Kind: "launch", WorkItemID: "only", AttemptID: att, Generation: 0, EventID: "e1",
	}
	raw, _ := json.Marshal(ev)
	body := append(raw, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), body...)

	integ := &countingEnsureIntegrator{}
	calls := map[string]int{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	req.RepoPath = repo
	req.Integrator = integ
	_, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected fail")
	}
	if integ.EnsureN != 0 || integ.IntegrateN != 0 {
		t.Fatalf("EnsureN=%d IntegrateN=%d want 0", integ.EnsureN, integ.IntegrateN)
	}
	if calls["only"] != 0 {
		t.Fatal("executor ran")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("log mutated")
	}
	if _, err := os.Stat(filepath.Join(dir, "workclaims.json")); err == nil {
		t.Fatal("workclaims created")
	}
}

func TestDirectExecute_CrossPlanAttemptInLog_NoMutation(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-xplan-log", "run_xplan_log"
	req := withExpectedPlanDigest(t, oneNodeFixture(project, runID, "g-xplan-log"))
	dir := filepath.Join(home, "projects", project, "runs", runID)
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "workflow-events.jsonl")
	ev := workflowrun.Event{
		Schema: workflowrun.EventSchema, ProjectID: project, RunID: runID,
		Kind: "launch", WorkItemID: "only", AttemptID: "att-only-deadbeefdead-g0",
		Generation: 1, EventID: "e1",
	}
	raw, _ := json.Marshal(ev)
	body := append(raw, '\n')
	_ = os.WriteFile(path, body, 0o600)
	before := append([]byte(nil), body...)
	calls := map[string]int{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	_, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected fail")
	}
	if calls["only"] != 0 {
		t.Fatal("executor ran")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("mutated")
	}
}

func TestDirectExecute_MissingIdentityInLog_NoMutation(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-miss-log", "run_miss_log"
	req := withExpectedPlanDigest(t, oneNodeFixture(project, runID, "g-miss-log"))
	dir := filepath.Join(home, "projects", project, "runs", runID)
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "workflow-events.jsonl")
	ev := workflowrun.Event{
		Schema: workflowrun.EventSchema, ProjectID: project, RunID: runID,
		Kind: "launch", EventID: "e1",
	}
	raw, _ := json.Marshal(ev)
	body := append(raw, '\n')
	_ = os.WriteFile(path, body, 0o600)
	before := append([]byte(nil), body...)
	calls := map[string]int{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, Calls: calls},
	}
	_, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected fail")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("mutated")
	}
}

func TestRecoverOpenLaunch_G0InterruptDoesNotSuppressG1(t *testing.T) {
	home := t.TempDir()
	elog, err := workflowrun.OpenEventLog(home, "proj-r", "run-r")
	if err != nil {
		t.Fatal(err)
	}
	must := func(e workflowrun.Event) {
		e.ProjectID, e.RunID = "proj-r", "run-r"
		e.ExecutionPlanDigest, e.GraphDigest = "sha256:plan-r", "sha256:graph-r"
		e.GraphID, e.GraphVersion = "g-r", 1
		e.TaskClass, e.ChildContractDigest = "tera", "sha256:ccd-r"
		if _, err := elog.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	must(workflowrun.Event{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1})
	must(workflowrun.Event{
		Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, Terminal: "cancelled",
		// Soft ledger recovery class (not authoritative; not service_forced).
		Payload: []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_soft_g0"}`),
	})
	must(workflowrun.Event{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g1", Generation: 2})
	n, err := workflowrun.RecoverOpenLaunchInterrupts(elog, "proj-r", "run-r")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n=%d want 1", n)
	}
	n2, _ := workflowrun.RecoverOpenLaunchInterrupts(elog, "proj-r", "run-r")
	if n2 != 0 {
		t.Fatalf("n2=%d", n2)
	}
}

func TestOverlappingOpenAttempts_FailClosedNoAppend(t *testing.T) {
	home := t.TempDir()
	elog, _ := workflowrun.OpenEventLog(home, "proj-ov", "run-ov")
	path := elog.Path()
	var b strings.Builder
	for _, e := range []workflowrun.Event{
		{Schema: workflowrun.EventSchema, ProjectID: "proj-ov", RunID: "run-ov", Kind: "launch",
			WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, EventID: "e1"},
		{Schema: workflowrun.EventSchema, ProjectID: "proj-ov", RunID: "run-ov", Kind: "launch",
			WorkItemID: "wi", AttemptID: "att-wi-x-g1", Generation: 2, EventID: "e2"},
	} {
		raw, _ := json.Marshal(e)
		b.Write(raw)
		b.WriteByte('\n')
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
	before, _ := os.ReadFile(path)
	n, err := workflowrun.RecoverOpenLaunchInterrupts(elog, "proj-ov", "run-ov")
	if err == nil || n != 0 {
		t.Fatalf("err=%v n=%d", err, n)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("mutated")
	}
}

func TestRecover_GenMismatchLog_NoMutation(t *testing.T) {
	home := t.TempDir()
	elog, _ := workflowrun.OpenEventLog(home, "proj-gm", "run-gm")
	path := elog.Path()
	// g1 with Generation=1 is invalid identity.
	ev := workflowrun.Event{
		Schema: workflowrun.EventSchema, ProjectID: "proj-gm", RunID: "run-gm",
		Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g1", Generation: 1, EventID: "e1",
	}
	raw, _ := json.Marshal(ev)
	body := append(raw, '\n')
	_ = os.WriteFile(path, body, 0o600)
	before := append([]byte(nil), body...)
	n, err := workflowrun.RecoverOpenLaunchInterrupts(elog, "proj-gm", "run-gm")
	if err == nil || n != 0 {
		t.Fatalf("err=%v n=%d", err, n)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("mutated")
	}
}

func TestStreamInvariant_ConcurrentLaunchDetectedAtArrival(t *testing.T) {
	// launch g0; launch g1 while g0 open — fail even if g0 later terminals.
	err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g1", Generation: 2},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
	})
	if err == nil {
		t.Fatal("expected concurrent open fail at second launch")
	}
	if !strings.Contains(err.Error(), "still open") {
		t.Fatalf("err=%v", err)
	}
}

func TestStreamInvariant_DuplicateExactLaunchFails(t *testing.T) {
	err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate launch") {
		t.Fatalf("err=%v", err)
	}
}

func TestStreamInvariant_PidAfterLaunchOK_PidWithoutLaunchFails(t *testing.T) {
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "pid", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, PID: 1},
	}); err != nil {
		t.Fatalf("pid after launch: %v", err)
	}
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "pid", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, PID: 1},
	}); err == nil || !strings.Contains(err.Error(), "without prior launch") {
		t.Fatalf("err=%v", err)
	}
}

// TestStreamInvariant_DuplicatePidExactAttemptFails: second pid for the same
// work_item/attempt fails closed (identical or divergent payload).
func TestStreamInvariant_DuplicatePidExactAttemptFails(t *testing.T) {
	// Identical second pid.
	err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "pid", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, PID: 42},
		{Kind: "pid", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, PID: 42},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate pid") {
		t.Fatalf("identical second pid: err=%v", err)
	}
	// Divergent second pid payload/PID.
	err = workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "pid", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, PID: 42},
		{Kind: "pid", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, PID: 99},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate pid") {
		t.Fatalf("divergent second pid: err=%v", err)
	}
}

// Typed interrupt pair invariants: only exact matching class pairs legal.
func TestStreamInvariant_TypedInterruptPairs(t *testing.T) {
	svcForcedInt := []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_svc1","terminal":"cancelled"}`)
	svcForcedTerm := []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_svc1","terminal":"cancelled"}`)
	hardInt := []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_hard1","terminal":"cancelled"}`)
	hardTerm := []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","recovery":"authoritative","failure_class":"hard_kill_recovery","interrupt_class":"hard_kill_recovery","interrupt_id":"iint_hard1","terminal":"cancelled"}`)
	softHardInt := []byte(`{"work_item_id":"wi","attempt_id":"att-wi-x-g0","generation":"1","failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_soft1","terminal":"cancelled"}`)

	// Legal: service forced interrupt → matching cancelled terminal.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: svcForcedInt},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: svcForcedTerm},
	}); err != nil {
		t.Fatalf("service pair: %v", err)
	}
	// Legal: authoritative hard_kill_recovery pair.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "hard_kill_recovery", Payload: hardInt},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "hard_kill_recovery", Payload: hardTerm},
	}); err != nil {
		t.Fatalf("hard pair: %v", err)
	}
	// Soft ledger hard interrupt alone (no terminal) is legal.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: softHardInt},
	}); err != nil {
		t.Fatalf("soft hard interrupt alone: %v", err)
	}
	// Reject untyped child interrupt.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, Terminal: "cancelled"},
	}); err == nil || !strings.Contains(err.Error(), "untyped") {
		t.Fatalf("untyped interrupt: err=%v", err)
	}
	// Reject mismatched: service interrupt then hard terminal.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", Payload: svcForcedInt},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", Payload: hardTerm},
	}); err == nil {
		t.Fatal("mismatched service→hard terminal must fail")
	}
	// Reject mismatched: hard interrupt then service cancelled terminal.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", Payload: hardInt},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", Payload: svcForcedTerm},
	}); err == nil {
		t.Fatal("mismatched hard→service terminal must fail")
	}
	// Reject terminal after interrupt that is succeeded (not cancelled).
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", Payload: svcForcedInt},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "succeeded", Payload: svcForcedTerm},
	}); err == nil {
		t.Fatal("succeeded terminal after service interrupt must fail")
	}
	// Reject duplicate child interrupt.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", Payload: svcForcedInt},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", Payload: svcForcedInt},
	}); err == nil || !strings.Contains(err.Error(), "duplicate child interrupt") {
		t.Fatalf("duplicate interrupt: err=%v", err)
	}
	// Reject interrupt-after-terminal.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, Terminal: "succeeded"},
		{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1,
			Terminal: "cancelled", Payload: svcForcedInt},
	}); err == nil || !strings.Contains(err.Error(), "after terminal") {
		t.Fatalf("interrupt after terminal: err=%v", err)
	}
	// Parent wave interrupt does not require a typed child pair for the terminal.
	if err := workflowrun.ValidateEventStreamInvariants([]workflowrun.Event{
		{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1},
		{Kind: "interrupt", Generation: 0}, // parent
		{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-x-g0", Generation: 1, Terminal: "cancelled"},
	}); err != nil {
		t.Fatalf("parent interrupt then terminal: %v", err)
	}
}

// TestFailAppendKindPid_SurfacesErrorNoHumanGate.
func TestFailAppendKindPid_SurfacesErrorNoHumanGate(t *testing.T) {
	home := testHome(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{HomeDir: home, Now: t0},
		TestConfigureEventLog: func(el *workflowrun.EventLog) {
			el.FailAppendKind = "pid"
		},
	}
	req := withExpectedPlanDigest(t, oneNodeFixture("proj-fail-pid", "run_fail_pid", "g-fail-pid"))
	res, err := svc.Execute(context.Background(), req)
	if err == nil {
		t.Fatalf("expected pid append fail: %+v", res)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatal("must not report human_gate after pid event failure")
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Fatalf("err=%v", err)
	}
}

// TestFailAppendKindInterrupt_SurfacesErrorNoHumanGate.
func TestFailAppendKindInterrupt_SurfacesErrorNoHumanGate(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-fail-int", "run_fail_int"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hangEntered := make(chan struct{})
	var hangOnce sync.Once
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0, Hang: true,
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID != "only" || pid <= 0 {
					return
				}
				hangOnce.Do(func() { close(hangEntered) })
			},
		},
		TestConfigureEventLog: func(el *workflowrun.EventLog) {
			el.FailAppendKind = "interrupt"
		},
	}
	go func() {
		<-hangEntered
		cancel()
	}()
	req := withExpectedPlanDigest(t, oneNodeFixture(project, runID, "g-fail-int"))
	res, err := svc.Execute(ctx, req)
	if err == nil {
		t.Fatalf("expected interrupt append fail: %+v", res)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatal("must not report human_gate after interrupt event failure")
	}
	// Terminal + claim must finalize without a succeeded path.
	events := loadEvents(t, home, project, runID)
	sawTermFailed := false
	for _, ev := range events {
		if ev.Kind == "interrupt" {
			t.Fatalf("interrupt must not persist when FailAppendKind=interrupt: %+v", ev)
		}
		if ev.Kind == "terminal" && ev.WorkItemID == "only" {
			if ev.Terminal == string(workgraph.TermSucceeded) {
				t.Fatalf("succeeded terminal after interrupt fail: %+v", ev)
			}
			sawTermFailed = true
		}
	}
	if !sawTermFailed {
		t.Fatal("expected failed terminal after interrupt_event_failed")
	}
	cs, csErr := workclaim.OpenPath(filepath.Join(home, "projects", project, "runs", runID, "workclaims.json"), t0)
	if csErr != nil {
		t.Fatal(csErr)
	}
	for _, c := range cs.AllClaims() {
		if c.WorkItemID != "only" {
			continue
		}
		if c.State != workclaim.StateClosed {
			t.Fatalf("claim must be closed after interrupt fail: %+v", c)
		}
		if c.Terminal == workgraph.TermSucceeded {
			t.Fatalf("claim must not be succeeded: %+v", c)
		}
	}
}

// TestFailAppendKindInterrupt_Alternate_SurfacesErrorNoHumanGate: alternate
// hang cancel with FailAppendKind=interrupt — error, no human_gate, consistent
// terminal/claim finalization on the alternate attempt.
func TestFailAppendKindInterrupt_Alternate_SurfacesErrorNoHumanGate(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-fail-alt-int", "run_fail_alt_int"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hangEntered := make(chan struct{})
	var hangOnce sync.Once
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: testspawn.Executor{
			HomeDir: home, Now: t0,
			FailModel: "model-unavailable-token",
			Hang:      true,
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID != "only" || pid <= 0 {
					return
				}
				hangOnce.Do(func() { close(hangEntered) })
			},
		},
		TestConfigureEventLog: func(el *workflowrun.EventLog) {
			el.FailAppendKind = "interrupt"
		},
	}
	go func() {
		<-hangEntered
		cancel()
	}()
	res, err := svc.Execute(ctx, withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-fail-alt-int", "implement alt interrupt fail"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-ag", InstallRef: "install-ag", WindowKind: "five_hour",
				ReservationID: "res-ag-prior", RouteReason: "pin-bad",
			},
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
		t.Fatalf("expected alternate interrupt append fail: %+v", res)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatal("must not report human_gate after alternate interrupt event failure")
	}
	if !strings.Contains(err.Error(), "interrupt") && !strings.Contains(res.Message, "interrupt") {
		// Failure may surface as required-child terminal/failed with interrupt_event_failed.
		sawClass := false
		for _, c := range res.Children {
			if c.FailureClass == "interrupt_event_failed" {
				sawClass = true
			}
		}
		if !sawClass {
			t.Fatalf("want interrupt-related failure: err=%v res=%+v", err, res)
		}
	}
	events := loadEvents(t, home, project, runID)
	// No interrupt row; alternate attempt must still have claim/launch/pid/terminal.
	var altAtt string
	for _, ev := range events {
		if ev.Kind == "interrupt" {
			t.Fatalf("interrupt must not persist when FailAppendKind=interrupt: %+v", ev)
		}
		if ev.Kind == "launch" && ev.WorkItemID == "only" && strings.HasSuffix(ev.AttemptID, "-g1") {
			altAtt = ev.AttemptID
		}
	}
	if altAtt == "" {
		t.Fatal("missing alternate launch (g1)")
	}
	wantGen, errGen := workflowrun.ClaimGenerationFromAttemptID(altAtt)
	if errGen != nil {
		t.Fatal(errGen)
	}
	sawTerm := false
	for _, kind := range []string{"claim", "launch", "pid", "terminal"} {
		found := false
		for _, ev := range events {
			if ev.Kind != kind || ev.WorkItemID != "only" || ev.AttemptID != altAtt {
				continue
			}
			found = true
			if ev.Generation != wantGen {
				t.Fatalf("alt %s gen=%d want %d", kind, ev.Generation, wantGen)
			}
			if kind == "terminal" {
				sawTerm = true
				if ev.Terminal == string(workgraph.TermSucceeded) {
					t.Fatalf("alternate terminal must not succeed: %+v", ev)
				}
			}
		}
		if !found {
			t.Fatalf("missing alt %s for attempt %s", kind, altAtt)
		}
	}
	if !sawTerm {
		t.Fatal("missing alternate terminal after interrupt_event_failed")
	}
	cs, csErr := workclaim.OpenPath(filepath.Join(home, "projects", project, "runs", runID, "workclaims.json"), t0)
	if csErr != nil {
		t.Fatal(csErr)
	}
	altClaim, getErr := cs.GetByAttempt(project, res.GraphID, res.GraphVersion, "only", altAtt)
	if getErr != nil {
		// Fall back to scan if GraphID empty on early fail.
		found := false
		for _, c := range cs.AllClaims() {
			if c.WorkItemID == "only" && c.AttemptID == altAtt {
				altClaim = c
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("alternate claim missing: %v", getErr)
		}
	}
	if altClaim.State != workclaim.StateClosed {
		t.Fatalf("alternate claim must be closed: %+v", altClaim)
	}
	if altClaim.Terminal == workgraph.TermSucceeded {
		t.Fatalf("alternate claim must not succeed: %+v", altClaim)
	}
}
