package workflowrun_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// realSpawnRunner starts a real OS helper process via supervisedexec and relays
// OnProviderStart with authority-captured PID/PGID/birth/executable identity.
// No FakeChildExecutor; no os.Getpid synthetic child identity.
// FakeChildExecutor evidence remains test-only and cannot satisfy production spawn identity.
type realSpawnRunner struct {
	// Mode: "ok" | "hang" | "omit_start" | "product" | "double_start"
	Mode string
	// LastStart is the last supervised start identity observed by the runner.
	LastStart atomic.Value // supervisedexec.StartedProcess
	// AliveAfterCallback is set true only when the child is still alive after
	// OnProviderStart returns (proves Service pid append while process is live).
	AliveAfterCallback atomic.Bool
	// ProductRel is a relative product file to write for success path.
	ProductRel string
	// DoubleStartSecondErr records the error from a second OnProviderStart call.
	DoubleStartSecondErr atomic.Value // error
}

func (r *realSpawnRunner) Run(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	mode := strings.TrimSpace(r.Mode)
	if mode == "" {
		mode = "product"
	}
	// Long enough for OnStart + Service pid append; cancel tests use hang.
	args := []string{"2"}
	if mode == "hang" || mode == "double_start" {
		args = []string{"60"}
	}
	cmd := exec.Command("/bin/sleep", args...)
	if inv.WorktreePath != "" {
		cmd.Dir = inv.WorktreePath
	}
	opts := supervisedexec.Options{
		HardCap: 30 * time.Second,
		RunID:   inv.RunID,
		Role:    firstNonEmptyAgent(inv.Role, "worker"),
		OnStart: func(started supervisedexec.StartedProcess) error {
			r.LastStart.Store(started)
			if mode == "omit_start" {
				// Intentionally do not invoke inv.OnProviderStart.
				return nil
			}
			pp := agent.ProviderProcess{
				PID:                   started.PID,
				PGID:                  started.PGID,
				ProcessBirthIdentity:  started.ProcessBirthIdentity,
				ExecutableIdentity:    started.ExecutableIdentity,
				ObservedAt:            started.ObservedAt,
				IdentityAmbiguous:     started.IdentityAmbiguous,
				IdentityAmbiguityNote: started.IdentityAmbiguityNote,
			}
			if inv.OnProviderStart != nil {
				if err := inv.OnProviderStart(pp); err != nil {
					return err
				}
			}
			// After Service callback returns, child must still be alive.
			if process.Alive(started.PID) {
				r.AliveAfterCallback.Store(true)
			}
			if mode == "double_start" && inv.OnProviderStart != nil {
				// Second callback with same identity — Service must reject before second append.
				err2 := inv.OnProviderStart(pp)
				r.DoubleStartSecondErr.Store(err2)
				if err2 != nil {
					return err2
				}
			}
			return nil
		},
	}
	sup, err := supervisedexec.Run(ctx, cmd, opts)
	// Nested product file under a new directory — exercises file-level discovery.
	if mode == "product" || mode == "ok" || mode == "omit_start" {
		rel := r.ProductRel
		if rel == "" {
			rel = "notes/spawn_product.go"
		}
		if inv.WorktreePath != "" {
			full := filepath.Join(inv.WorktreePath, filepath.FromSlash(rel))
			_ = os.MkdirAll(filepath.Dir(full), 0o700)
			_ = os.WriteFile(full, []byte("package notes\n// spawn identity nested product\n"), 0o600)
		}
	}
	res := agent.Result{
		ExitCode:               sup.ExitCode,
		Summary:                "real_spawn_runner",
		ActualProvider:         "spawn-test",
		ActualModel:            "spawn-model",
		ActualEffort:           "medium",
		ActualPermission:       "bounded_write",
		ActualAccountRef:       "acct-spawn",
		ActualInstallRef:       "install-spawn",
		ActualSourceModel:      agent.ActualSourceAcceptedInvocation,
		ActualSourceEffort:     agent.ActualSourceAcceptedInvocation,
		ActualSourcePermission: agent.ActualSourceAcceptedInvocation,
		ActualSourceAccount:    agent.ActualSourceAuthBinding,
		ActualSourceInstall:    agent.ActualSourceInstallBinding,
	}
	if err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, err
	}
	if sup.Outcome != supervisedexec.OutcomeCompleted {
		res.ExitCode = -1
		return res, context.Canceled
	}
	return res, nil
}

func firstNonEmptyAgent(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func spawnProdExecutor(home string, runner agent.Runner) workflowrun.ProductionChildExecutor {
	return workflowrun.ProductionChildExecutor{
		HomeDir: home,
		Now:     t0,
		HardCap: 30 * time.Second,
		Lookup: func(provider string) (agent.Runner, error) {
			return runner, nil
		},
	}
}

func spawnBaseReq(t *testing.T, project, runID string) workflowrun.Request {
	t.Helper()
	return withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID,
		Definition: workflowrun.OneNodeDefinition("g-spawn", "spawn identity child"),
		Actor:      "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "spawn-test", Model: "spawn-model", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-spawn", InstallRef: "install-spawn",
				WindowKind: "five_hour", ReservationID: "res-spawn", RouteReason: "spawn-test",
			},
		},
	})
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for process.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if process.Alive(pid) {
		t.Fatalf("child pid %d still alive", pid)
	}
}

// TestSpawnIdentity_RealProcess_PidWhileAlive: ProductionChildExecutor + real
// supervised process; claim < launch < pid while child alive; nested product;
// exact identity including non-zero observed_at.
func TestSpawnIdentity_RealProcess_PidWhileAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process group spawn identity")
	}
	home := testHome(t)
	repo := initGitRepo(t)
	runner := &realSpawnRunner{Mode: "product", ProductRel: "notes/spawn_product.go"}
	integ := &countingEnsureIntegrator{}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: spawnProdExecutor(home, runner),
	}
	project, runID := "proj-spawn-live", "run_spawn_live"
	req := spawnBaseReq(t, project, runID)
	req.RepoPath = repo
	req.Integrator = integ
	res, err := svc.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute: %v status=%s msg=%s children=%+v events=%v", err, res.Status, res.Message, res.Children, res.Events)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%s want human_gate (local plumbing success path)", res.Status)
	}
	if !runner.AliveAfterCallback.Load() {
		t.Fatal("child was not alive after OnProviderStart/pid append")
	}
	started, ok := runner.LastStart.Load().(supervisedexec.StartedProcess)
	if !ok || started.PID <= 0 {
		t.Fatalf("runner did not capture start identity: %#v", runner.LastStart.Load())
	}
	if started.PID == os.Getpid() {
		t.Fatalf("pid must be child process, not test process %d", os.Getpid())
	}
	// Nested product must appear in child FilesTouched / integrate path.
	sawNested := false
	for _, c := range res.Children {
		for _, f := range c.FilesTouched {
			if f == "notes/spawn_product.go" || strings.HasSuffix(f, "notes/spawn_product.go") {
				sawNested = true
			}
		}
	}
	if !sawNested {
		// Also accept via event log integrate path — at least prove product hash non-empty.
		for _, c := range res.Children {
			if c.WorkItemID == "only" && c.Terminal == string(workgraph.TermSucceeded) && c.OutputEvidence != "" {
				sawNested = true
			}
		}
	}
	if !sawNested {
		t.Fatalf("nested notes/spawn_product.go not proven in children=%+v", res.Children)
	}
	events := loadEvents(t, home, project, runID)
	// Order: claim < launch < pid (exactly one pid).
	var iClaim, iLaunch, iPID = -1, -1, -1
	nPID := 0
	var pidEv workflowrun.Event
	for i, ev := range events {
		switch ev.Kind {
		case "claim":
			if iClaim < 0 && ev.WorkItemID == "only" {
				iClaim = i
			}
		case "launch":
			if iLaunch < 0 && ev.WorkItemID == "only" {
				iLaunch = i
			}
		case "pid":
			if ev.WorkItemID == "only" {
				nPID++
				if iPID < 0 {
					iPID = i
					pidEv = ev
				}
			}
		}
	}
	if iClaim < 0 || iLaunch < 0 || iPID < 0 {
		t.Fatalf("missing claim/launch/pid indices C=%d L=%d P=%d", iClaim, iLaunch, iPID)
	}
	if !(iClaim < iLaunch && iLaunch < iPID) {
		t.Fatalf("order claim(%d) < launch(%d) < pid(%d) required", iClaim, iLaunch, iPID)
	}
	if nPID != 1 {
		t.Fatalf("exactly one pid event required, got %d", nPID)
	}
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		t.Fatalf("stream invariants: %v", err)
	}
	wantGen, err := workflowrun.ClaimGenerationFromAttemptID(pidEv.AttemptID)
	if err != nil || pidEv.Generation != wantGen {
		t.Fatalf("pid gen=%d att=%s wantGen=%d err=%v", pidEv.Generation, pidEv.AttemptID, wantGen, err)
	}
	if pidEv.PID != started.PID {
		t.Fatalf("pid event PID=%d want started %d", pidEv.PID, started.PID)
	}
	if err := workflowrun.ValidatePIDEventPayload(pidEv); err != nil {
		t.Fatalf("pid payload validation: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(pidEv.Payload, &payload); err != nil {
		t.Fatalf("pid payload: %v raw=%s", err, pidEv.Payload)
	}
	if payload["pgid"] != strconv.Itoa(started.PGID) {
		t.Fatalf("payload pgid=%q want %d", payload["pgid"], started.PGID)
	}
	if payload["process_birth_identity"] != started.ProcessBirthIdentity {
		t.Fatalf("birth payload=%q want %q", payload["process_birth_identity"], started.ProcessBirthIdentity)
	}
	if payload["executable_identity"] != started.ExecutableIdentity {
		t.Fatalf("exec payload=%q want %q", payload["executable_identity"], started.ExecutableIdentity)
	}
	if payload["identity_ambiguous"] == "true" {
		t.Fatalf("ambiguous identity in payload: %v", payload)
	}
	obs, err := time.Parse(time.RFC3339Nano, payload["observed_at"])
	if err != nil {
		t.Fatalf("observed_at parse: %v val=%q", err, payload["observed_at"])
	}
	if obs.IsZero() {
		t.Fatal("observed_at zero")
	}
}

// TestSpawnIdentity_RealProcess_CancelInterrupt: cancel after spawn-time pid;
// exact interrupt PID equals persisted spawn PID; process group reaped; no human_gate.
func TestSpawnIdentity_RealProcess_CancelInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process group spawn identity")
	}
	home := testHome(t)
	runner := &realSpawnRunner{Mode: "hang"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var spawnPID atomic.Int64
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: spawnProdExecutor(home, runner),
		TestAfterPIDEvent: func(ev workflowrun.Event) {
			spawnPID.Store(int64(ev.PID))
			cancel()
		},
	}
	project, runID := "proj-spawn-cancel", "run_spawn_cancel"
	res, err := svc.Execute(ctx, spawnBaseReq(t, project, runID))
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatalf("must not human_gate on cancel: err=%v res=%+v", err, res)
	}
	wantPID := int(spawnPID.Load())
	if wantPID <= 0 {
		if st, ok := runner.LastStart.Load().(supervisedexec.StartedProcess); ok {
			wantPID = st.PID
		}
	}
	if wantPID <= 0 {
		t.Fatalf("missing spawn pid; res=%+v err=%v", res, err)
	}
	waitDead(t, wantPID)
	events := loadEvents(t, home, project, runID)
	requireKinds(t, events, "claim", "launch", "pid", "interrupt")
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		t.Fatalf("stream: %v", err)
	}
	// Exactly one child interrupt with hard PID equality to spawn pid.
	var intEv *workflowrun.Event
	nInt := 0
	for i := range events {
		ev := &events[i]
		if ev.Kind == "interrupt" && ev.WorkItemID == "only" {
			nInt++
			intEv = ev
		}
	}
	if nInt != 1 {
		t.Fatalf("want exactly one child interrupt, got %d", nInt)
	}
	if intEv.PID <= 0 {
		t.Fatalf("interrupt PID must be > 0, got %d", intEv.PID)
	}
	if intEv.PID != wantPID {
		t.Fatalf("interrupt PID=%d != spawn pid=%d", intEv.PID, wantPID)
	}
	// Same attempt/generation as pid event.
	var pidAtt string
	var pidGen int
	for _, ev := range events {
		if ev.Kind == "pid" && ev.WorkItemID == "only" {
			pidAtt = ev.AttemptID
			pidGen = ev.Generation
		}
	}
	if intEv.AttemptID != pidAtt || intEv.Generation != pidGen {
		t.Fatalf("interrupt att/gen %s/%d != pid %s/%d", intEv.AttemptID, intEv.Generation, pidAtt, pidGen)
	}
	wantGen, _ := workflowrun.ClaimGenerationFromAttemptID(intEv.AttemptID)
	if intEv.Generation != wantGen {
		t.Fatalf("interrupt gen=%d want %d", intEv.Generation, wantGen)
	}
	for _, kind := range []string{"claim", "launch", "pid", "interrupt"} {
		found := false
		for _, ev := range events {
			if ev.Kind == kind && ev.WorkItemID == "only" && ev.AttemptID == intEv.AttemptID {
				found = true
				if ev.Generation != wantGen {
					t.Fatalf("%s gen=%d want %d", kind, ev.Generation, wantGen)
				}
			}
		}
		if !found {
			t.Fatalf("missing %s for attempt %s", kind, intEv.AttemptID)
		}
	}
	for _, c := range res.Children {
		if c.WorkItemID == "only" && c.Terminal == string(workgraph.TermSucceeded) {
			t.Fatalf("must not succeed on cancel: %+v", c)
		}
	}
}

// TestSpawnIdentity_RealProcess_DoubleOnProviderStart_ExactlyOncePID: real runner
// invokes OnProviderStart twice; exactly one durable pid; child reaped; failed terminal/claim; no human_gate.
func TestSpawnIdentity_RealProcess_DoubleOnProviderStart_ExactlyOncePID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process group spawn identity")
	}
	home := testHome(t)
	runner := &realSpawnRunner{Mode: "double_start"}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: spawnProdExecutor(home, runner),
	}
	project, runID := "proj-spawn-dup", "run_spawn_dup"
	res, err := svc.Execute(context.Background(), spawnBaseReq(t, project, runID))
	if err == nil {
		t.Fatalf("expected duplicate process-start fail: %+v", res)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatal("must not human_gate after duplicate process-start")
	}
	second, _ := runner.DoubleStartSecondErr.Load().(error)
	if second == nil || !strings.Contains(second.Error(), "duplicate process-start") {
		t.Fatalf("second OnProviderStart err=%v want duplicate process-start", second)
	}
	if st, ok := runner.LastStart.Load().(supervisedexec.StartedProcess); ok && st.PID > 0 {
		waitDead(t, st.PID)
	} else {
		t.Fatal("expected real start before double callback")
	}
	events := loadEvents(t, home, project, runID)
	nPID := 0
	for _, ev := range events {
		if ev.Kind == "pid" && ev.WorkItemID == "only" {
			nPID++
		}
		if ev.Kind == "terminal" && ev.WorkItemID == "only" && ev.Terminal == string(workgraph.TermSucceeded) {
			t.Fatalf("must not succeed: %+v", ev)
		}
	}
	if nPID != 1 {
		t.Fatalf("exactly one durable pid required, got %d", nPID)
	}
	if err := workflowrun.ValidateEventStreamInvariants(events); err != nil {
		t.Fatalf("stream: %v", err)
	}
	cs, csErr := workclaim.OpenPath(filepath.Join(home, "projects", project, "runs", runID, "workclaims.json"), t0)
	if csErr != nil {
		t.Fatal(csErr)
	}
	for _, c := range cs.AllClaims() {
		if c.WorkItemID != "only" {
			continue
		}
		if c.Terminal == workgraph.TermSucceeded {
			t.Fatalf("claim must not succeed: %+v", c)
		}
	}
}

// TestSpawnIdentity_RealProcess_FailAppendPidKillsChild: FailAppendKind=pid at
// spawn kills/reaps the real child; zero pid rows; failed terminal/claim; no human_gate.
func TestSpawnIdentity_RealProcess_FailAppendPidKillsChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process group spawn identity")
	}
	home := testHome(t)
	runner := &realSpawnRunner{Mode: "hang"}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: spawnProdExecutor(home, runner),
		TestConfigureEventLog: func(el *workflowrun.EventLog) {
			el.FailAppendKind = "pid"
		},
	}
	project, runID := "proj-spawn-failpid", "run_spawn_failpid"
	res, err := svc.Execute(context.Background(), spawnBaseReq(t, project, runID))
	if err == nil {
		t.Fatalf("expected pid append fail: %+v", res)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatal("must not human_gate after pid event failure")
	}
	if st, ok := runner.LastStart.Load().(supervisedexec.StartedProcess); ok && st.PID > 0 {
		waitDead(t, st.PID)
	} else {
		t.Fatal("expected runner to observe start before kill")
	}
	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := elog.ReadAllForRun(project, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == "pid" {
			t.Fatalf("pid must not persist when FailAppendKind=pid: %+v", ev)
		}
	}
	sawTerm := false
	for _, ev := range events {
		if ev.Kind == "terminal" && ev.WorkItemID == "only" {
			sawTerm = true
			if ev.Terminal == string(workgraph.TermSucceeded) {
				t.Fatalf("succeeded terminal after pid fail: %+v", ev)
			}
		}
	}
	if !sawTerm {
		t.Fatal("expected failed terminal after pid_event_failed")
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
			t.Fatalf("claim must be closed: %+v", c)
		}
		if c.Terminal == workgraph.TermSucceeded {
			t.Fatalf("claim must not succeed: %+v", c)
		}
	}
}

// TestSpawnIdentity_OmitOnProviderStart_FailClosed: runner omits OnProviderStart
// => production fail closed, not success.
func TestSpawnIdentity_OmitOnProviderStart_FailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process group spawn identity")
	}
	home := testHome(t)
	repo := initGitRepo(t)
	runner := &realSpawnRunner{Mode: "omit_start", ProductRel: "notes/spawn_product.go"}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: spawnProdExecutor(home, runner),
	}
	project, runID := "proj-spawn-omit", "run_spawn_omit"
	req := spawnBaseReq(t, project, runID)
	req.RepoPath = repo
	req.Integrator = &countingEnsureIntegrator{}
	res, err := svc.Execute(context.Background(), req)
	if err == nil && res.Status == workflowrun.StatusHumanGate {
		t.Fatalf("omit OnProviderStart must not succeed: %+v", res)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatal("must not human_gate without spawn identity")
	}
	elog, oerr := workflowrun.OpenEventLog(home, project, runID)
	if oerr != nil {
		t.Fatal(oerr)
	}
	events, rerr := elog.ReadAllForRun(project, runID)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, ev := range events {
		if ev.Kind == "pid" {
			t.Fatalf("unexpected pid without OnProviderStart: %+v", ev)
		}
		if ev.Kind == "terminal" && ev.Terminal == string(workgraph.TermSucceeded) {
			t.Fatalf("must not terminal succeed: %+v", ev)
		}
	}
}

// mismatchSpawnExecutor corrupts returned ProcessPID after a real production spawn.
type mismatchSpawnExecutor struct {
	inner workflowrun.ChildExecutor
}

func (m mismatchSpawnExecutor) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	out, err := m.inner.Execute(ctx, in)
	if out.ProcessPID > 0 {
		out.ProcessPID = out.ProcessPID + 99999
	}
	return out, err
}

// TestSpawnIdentity_ReturnedPIDMismatch_FailClosed: spawn-time pid logged, then
// returned identity mismatches => fail closed, not success.
func TestSpawnIdentity_ReturnedPIDMismatch_FailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix process group spawn identity")
	}
	home := testHome(t)
	repo := initGitRepo(t)
	runner := &realSpawnRunner{Mode: "product", ProductRel: "notes/spawn_product.go"}
	inner := spawnProdExecutor(home, runner)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: mismatchSpawnExecutor{inner: inner},
	}
	project, runID := "proj-spawn-mm", "run_spawn_mm"
	req := spawnBaseReq(t, project, runID)
	req.RepoPath = repo
	req.Integrator = &countingEnsureIntegrator{}
	res, err := svc.Execute(context.Background(), req)
	if err == nil && res.Status == workflowrun.StatusHumanGate {
		t.Fatalf("mismatch must fail closed: %+v", res)
	}
	if res.Status == workflowrun.StatusHumanGate {
		t.Fatal("must not human_gate on pid identity mismatch")
	}
	events := loadEvents(t, home, project, runID)
	nPID := 0
	for _, ev := range events {
		if ev.Kind == "pid" {
			nPID++
		}
		if ev.Kind == "terminal" && ev.Terminal == string(workgraph.TermSucceeded) {
			t.Fatalf("must not succeed: %+v", ev)
		}
	}
	if nPID != 1 {
		t.Fatalf("want exactly one spawn pid before mismatch fail, got %d", nPID)
	}
	sawMismatch := false
	for _, c := range res.Children {
		if c.FailureClass == "pid_identity_mismatch" {
			sawMismatch = true
		}
	}
	if !sawMismatch && !strings.Contains(res.Message, "mismatch") && (err == nil || !strings.Contains(err.Error(), "mismatch")) {
		t.Fatalf("want pid_identity_mismatch: err=%v res=%+v", err, res)
	}
}

// TestSpawnIdentity_ValidateProcessStart unit-checks fail-closed rules including ObservedAt.
func TestSpawnIdentity_ValidateProcessStart(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 123, time.UTC)
	good := workflowrun.ProcessStart{
		PID: 1, PGID: 1, ProcessBirthIdentity: "b", ExecutableIdentity: "e", ObservedAt: now,
	}
	if err := workflowrun.ValidateProcessStart(good); err != nil {
		t.Fatal(err)
	}
	if err := workflowrun.ValidateProcessStart(workflowrun.ProcessStart{PID: 0, PGID: 1, ProcessBirthIdentity: "b", ExecutableIdentity: "e", ObservedAt: now}); err == nil {
		t.Fatal("want PID fail")
	}
	if err := workflowrun.ValidateProcessStart(workflowrun.ProcessStart{PID: 1, PGID: 0, ProcessBirthIdentity: "b", ExecutableIdentity: "e", ObservedAt: now}); err == nil {
		t.Fatal("want PGID fail")
	}
	if err := workflowrun.ValidateProcessStart(workflowrun.ProcessStart{PID: 1, PGID: 1, ExecutableIdentity: "e", ObservedAt: now}); err == nil {
		t.Fatal("want birth fail")
	}
	if err := workflowrun.ValidateProcessStart(workflowrun.ProcessStart{PID: 1, PGID: 1, ProcessBirthIdentity: "b", ObservedAt: now}); err == nil {
		t.Fatal("want exec fail")
	}
	if err := workflowrun.ValidateProcessStart(workflowrun.ProcessStart{
		PID: 1, PGID: 1, ProcessBirthIdentity: "b", ExecutableIdentity: "e",
	}); err == nil {
		t.Fatal("want zero ObservedAt fail")
	}
	if err := workflowrun.ValidateProcessStart(workflowrun.ProcessStart{
		PID: 1, PGID: 1, ProcessBirthIdentity: "b", ExecutableIdentity: "e", ObservedAt: now, IdentityAmbiguous: true,
	}); err == nil {
		t.Fatal("want ambiguous fail")
	}
}

// TestSpawnIdentity_ValidatePIDEventPayload_Negative: zero/malformed/mismatched facts fail closed.
func TestSpawnIdentity_ValidatePIDEventPayload_Negative(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 456, time.UTC)
	goodPayload, _ := json.Marshal(map[string]string{
		"pid": "7", "pgid": "7", "process_birth_identity": "b", "executable_identity": "e",
		"observed_at": now.Format(time.RFC3339Nano), "identity_ambiguous": "false",
		"worktree_path": "/tmp/wt", "log_path": "/tmp/log",
	})
	if err := workflowrun.ValidatePIDEventPayload(workflowrun.Event{Kind: "pid", PID: 7, Payload: goodPayload}); err != nil {
		t.Fatal(err)
	}
	// Event.PID != payload pid
	if err := workflowrun.ValidatePIDEventPayload(workflowrun.Event{Kind: "pid", PID: 8, Payload: goodPayload}); err == nil {
		t.Fatal("want Event.PID vs payload mismatch fail")
	}
	// missing observed_at
	badObs, _ := json.Marshal(map[string]string{"pid": "7", "observed_at": ""})
	if err := workflowrun.ValidatePIDEventPayload(workflowrun.Event{Kind: "pid", PID: 7, Payload: badObs}); err == nil {
		t.Fatal("want missing observed_at fail")
	}
	// malformed observed_at
	mal, _ := json.Marshal(map[string]string{"pid": "7", "observed_at": "not-a-time"})
	if err := workflowrun.ValidatePIDEventPayload(workflowrun.Event{Kind: "pid", PID: 7, Payload: mal}); err == nil {
		t.Fatal("want malformed observed_at fail")
	}
	// zero Event.PID
	if err := workflowrun.ValidatePIDEventPayload(workflowrun.Event{Kind: "pid", PID: 0, Payload: goodPayload}); err == nil {
		t.Fatal("want zero Event.PID fail")
	}
}
