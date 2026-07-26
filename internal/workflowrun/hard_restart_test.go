package workflowrun_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/agent"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

const hardRestartHelperEnv = "LOOPCODER_HARD_RESTART_HELPER"

type hangSpawnRunner struct {
	Mode string // hang | product
}

func (r hangSpawnRunner) Run(ctx context.Context, inv agent.Invocation) (agent.Result, error) {
	args := []string{"120"}
	if r.Mode == "product" {
		args = []string{"1"}
	}
	cmd := exec.Command("/bin/sleep", args...)
	if inv.WorktreePath != "" {
		cmd.Dir = inv.WorktreePath
	}
	opts := supervisedexec.Options{
		HardCap:  3 * time.Minute,
		RunID:    inv.RunID,
		Role:     "worker",
		Guardian: inv.Guardian,
		OnStart: func(started supervisedexec.StartedProcess) error {
			if inv.OnProviderStart != nil {
				return inv.OnProviderStart(agent.ProviderProcess{
					PID: started.PID, PGID: started.PGID,
					ProcessBirthIdentity:  started.ProcessBirthIdentity,
					ExecutableIdentity:    started.ExecutableIdentity,
					ObservedAt:            started.ObservedAt,
					IdentityAmbiguous:     started.IdentityAmbiguous,
					IdentityAmbiguityNote: started.IdentityAmbiguityNote,
				})
			}
			return nil
		},
	}
	sup, err := supervisedexec.Run(ctx, cmd, opts)
	if r.Mode == "product" && inv.WorktreePath != "" {
		_ = os.MkdirAll(filepath.Join(inv.WorktreePath, "notes"), 0o700)
		_ = os.WriteFile(filepath.Join(inv.WorktreePath, "notes", "spawn_product.go"), []byte("package notes\n"), 0o600)
	}
	res := agent.Result{
		ExitCode:               sup.ExitCode,
		Summary:                "hang_spawn",
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
		return res, err
	}
	return res, nil
}

func filteredEnv() []string {
	out := []string{}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "LOOPCODER_HOME=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func waitFileJSON(path string, deadline time.Time) (map[string]any, error) {
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil && len(raw) > 0 {
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil && len(m) > 0 {
				return m, nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, os.ErrNotExist
}

func waitGuardianStarted(diagPath string, fence map[string]any, deadline time.Time) (map[string]any, error) {
	for time.Now().Before(deadline) {
		f, err := os.Open(diagPath)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var ev map[string]any
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			if ev["event"] != "started" {
				continue
			}
			if matchFence(ev, fence) {
				_ = f.Close()
				return ev, nil
			}
		}
		_ = f.Close()
		time.Sleep(20 * time.Millisecond)
	}
	return nil, os.ErrNotExist
}

// scanGuardianDiagnostics counts started/killed matching fence.
// started schema carries fence + guardian_pid (no provider_pid).
// killed carries fence + provider_pid/pgid (and guardian_pid when present).
// Always rescans the full file (never return-at-first). Late duplicates fail.
func scanGuardianDiagnostics(diagPath string, fence map[string]any, providerPID, providerPGID int, guardianPID int) (started, killed []map[string]any, bad error) {
	f, err := os.Open(diagPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if !matchFence(ev, fence) {
			continue
		}
		switch ev["event"] {
		case "skip", "kill-failed", "startup-failed":
			return nil, nil, &guardianBadEventError{ev: ev}
		case "started":
			// started: fence + guardian_pid (schema does not include provider_pid).
			if guardianPID > 0 {
				g, ok := asInt(ev["guardian_pid"])
				if !ok || g != guardianPID {
					continue
				}
			}
			started = append(started, ev)
		case "killed":
			ppid, _ := asInt(ev["provider_pid"])
			ppgid, _ := asInt(ev["provider_pgid"])
			if ppid != providerPID || ppgid != providerPGID {
				continue
			}
			if guardianPID > 0 {
				if g, ok := asInt(ev["guardian_pid"]); ok && g > 0 && g != guardianPID {
					continue
				}
			}
			killed = append(killed, ev)
		}
	}
	return started, killed, nil
}

func waitGuardianKilled(diagPath string, fence map[string]any, providerPID, providerPGID int, guardianPID int, deadline time.Time) (map[string]any, error) {
	for time.Now().Before(deadline) {
		started, killed, err := scanGuardianDiagnostics(diagPath, fence, providerPID, providerPGID, guardianPID)
		if err != nil {
			if os.IsNotExist(err) {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			// bad event is hard fail
			if _, ok := err.(*guardianBadEventError); ok {
				return nil, err
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if len(killed) > 1 {
			return nil, fmt.Errorf("matching killed events=%d want exactly 1", len(killed))
		}
		if len(killed) == 1 {
			// Rescan once more after brief settle to catch late duplicates.
			time.Sleep(50 * time.Millisecond)
			started2, killed2, err2 := scanGuardianDiagnostics(diagPath, fence, providerPID, providerPGID, guardianPID)
			if err2 != nil {
				return nil, err2
			}
			if len(killed2) != 1 {
				return nil, fmt.Errorf("post-rescan killed=%d want exactly 1", len(killed2))
			}
			if len(started2) != 1 && len(started) != 1 {
				// started may already be 1; require exactly 1 matching started overall.
				if len(started2) != 1 {
					return nil, fmt.Errorf("matching started events=%d want exactly 1", len(started2))
				}
			}
			if len(started2) != 1 {
				return nil, fmt.Errorf("matching started events=%d want exactly 1", len(started2))
			}
			return killed2[0], nil
		}
		_ = started
		time.Sleep(20 * time.Millisecond)
	}
	return nil, os.ErrNotExist
}

type guardianBadEventError struct{ ev map[string]any }

func (e *guardianBadEventError) Error() string {
	b, _ := json.Marshal(e.ev)
	return "guardian bad event: " + string(b)
}

func matchFence(ev, fence map[string]any) bool {
	for _, k := range []string{"project_id", "run_id", "attempt_id", "owner_id"} {
		if strings.TrimSpace(str(ev[k])) != strings.TrimSpace(str(fence[k])) {
			return false
		}
	}
	eg, _ := asInt(ev["claim_generation"])
	fg, _ := asInt(fence["claim_generation"])
	return eg == fg && eg > 0
}

func str(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case json.Number:
		i, err := t.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

// TestHardRestart_GuardianOnly_SIGKILL: no test-side KillGroup. Ready only after
// guardian diagnostic "started" matching fence; after SIGKILL wait unique "killed".
func TestHardRestart_GuardianOnly_SIGKILL(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("guardian-only hard restart requires darwin/arm64")
	}
	if os.Getenv(hardRestartHelperEnv) == "1" {
		runHardRestartHelper(t)
		return
	}
	root := t.TempDir()
	home := filepath.Join(root, "loopcoder-home")
	_ = os.MkdirAll(home, 0o700)
	state := filepath.Join(root, "state")
	_ = os.MkdirAll(state, 0o700)
	readyPath := filepath.Join(state, "ready.json")
	resultPath := filepath.Join(state, "p2_result.json")
	project, runID := "proj-hr-guard", "run_hr_guard"

	cmd := exec.Command(os.Args[0], "-test.run=TestHardRestart_GuardianOnly_SIGKILL$", "-test.v")
	cmd.Env = append(filteredEnv(),
		hardRestartHelperEnv+"=1",
		"LOOPCODER_HR_PHASE=p1",
		"LOOPCODER_HR_HOME="+home,
		"LOOPCODER_HR_READY="+readyPath,
		"LOOPCODER_HR_PROJECT="+project,
		"LOOPCODER_HR_RUN="+runID,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	readyDeadline := time.Now().Add(45 * time.Second)
	ready, err := waitFileJSON(readyPath, readyDeadline)
	if err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatal("p1 ready marker timeout")
	}
	providerPID, _ := asInt(ready["pid"])
	providerPGID, _ := asInt(ready["pgid"])
	if providerPID <= 0 || providerPID == os.Getpid() {
		t.Fatalf("bad provider pid %v", ready["pid"])
	}
	diagPath := str(ready["diagnostic_path"])
	if diagPath == "" {
		t.Fatal("ready missing diagnostic_path")
	}
	fence := map[string]any{
		"project_id": ready["project_id"], "run_id": ready["run_id"],
		"attempt_id": ready["attempt_id"], "owner_id": ready["owner_id"],
		"claim_generation": ready["claim_generation"],
	}
	// Must already have waited for guardian started inside p1 ready; re-assert.
	if _, err := waitGuardianStarted(diagPath, fence, time.Now().Add(5*time.Second)); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatalf("guardian started missing: %v ready=%v", err, ready)
	}

	// SIGKILL p1 only — no KillGroup/KillTree fallback from this parent.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL p1: %v", err)
	}
	_, _ = cmd.Process.Wait()

	// guardian_pid comes from ready.started (must be >0); use for death wait + rescan.
	gpid, ok := asInt(ready["guardian_pid"])
	if !ok || gpid <= 0 {
		t.Fatalf("ready.guardian_pid must be >0, got %v", ready["guardian_pid"])
	}
	// Unique killed with full fence + provider pid/pgid; full-file rescan for late dups.
	killDeadline := time.Now().Add(20 * time.Second)
	killed, kerr := waitGuardianKilled(diagPath, fence, providerPID, providerPGID, gpid, killDeadline)
	if kerr != nil {
		t.Fatalf("guardian killed missing (no test kill fallback): %v", kerr)
	}
	_ = killed
	// Final rescan: exactly one started and one killed for fence+provider.
	startedN, killedN, serr := scanGuardianDiagnostics(diagPath, fence, providerPID, providerPGID, gpid)
	if serr != nil {
		t.Fatal(serr)
	}
	if len(startedN) != 1 {
		t.Fatalf("final started count=%d want 1", len(startedN))
	}
	if len(killedN) != 1 {
		t.Fatalf("final killed count=%d want 1", len(killedN))
	}

	// Provider and guardian both dead — fixed deadline.
	deadDeadline := time.Now().Add(10 * time.Second)
	for (process.Alive(providerPID) || process.Alive(gpid)) && time.Now().Before(deadDeadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if process.Alive(providerPID) {
		t.Fatalf("provider pid %d still alive after guardian killed only", providerPID)
	}
	if process.Alive(gpid) {
		t.Fatalf("guardian pid %d still alive", gpid)
	}

	// p2: same request shape, AttemptGeneration nil — auto g1.
	cmd2 := exec.Command(os.Args[0], "-test.run=TestHardRestart_GuardianOnly_SIGKILL$", "-test.v")
	cmd2.Env = append(filteredEnv(),
		hardRestartHelperEnv+"=1",
		"LOOPCODER_HR_PHASE=p2",
		"LOOPCODER_HR_HOME="+home,
		"LOOPCODER_HR_RESULT="+resultPath,
		"LOOPCODER_HR_PROJECT="+project,
		"LOOPCODER_HR_RUN="+runID,
		"LOOPCODER_HR_MODE=recover_g1",
	)
	out2, err2 := cmd2.CombinedOutput()
	if err2 != nil {
		t.Fatalf("p2: %v\n%s", err2, out2)
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if str(res["err"]) != "" {
		t.Fatalf("p2 err=%q full=%s", res["err"], raw)
	}
	if res["status"] != workflowrun.StatusHumanGate {
		t.Fatalf("p2 status=%v want human_gate", res["status"])
	}
	if res["attempt_generation_nil"] != true {
		t.Fatal("p2 request AttemptGeneration must be nil")
	}
	if n, _ := asInt(res["g0_launch"]); n != 1 {
		t.Fatalf("g0_launch=%v want 1", res["g0_launch"])
	}
	if n, _ := asInt(res["g1_launch"]); n != 1 {
		t.Fatalf("g1_launch=%v want 1", res["g1_launch"])
	}
	if n, _ := asInt(res["g0_interrupt"]); n != 1 {
		t.Fatalf("g0_interrupt=%v want 1", res["g0_interrupt"])
	}
	if n, _ := asInt(res["g0_terminal"]); n != 1 {
		t.Fatalf("g0_terminal=%v want 1", res["g0_terminal"])
	}
	if n, _ := asInt(res["pid_count"]); n != 2 {
		t.Fatalf("pid_count=%v want 2", res["pid_count"])
	}
	if n, _ := asInt(res["process_peak"]); n != 1 {
		t.Fatalf("process_peak=%v want 1 on real hard-restart/g1 path", res["process_peak"])
	}
	if n, _ := asInt(res["worktree_peak"]); n != 1 {
		t.Fatalf("worktree_peak=%v want 1 on real hard-restart/g1 path", res["worktree_peak"])
	}

	// p3: exact g1 succeeded reuse, zero launch
	result3 := filepath.Join(state, "p3_result.json")
	cmd3 := exec.Command(os.Args[0], "-test.run=TestHardRestart_GuardianOnly_SIGKILL$", "-test.v")
	cmd3.Env = append(filteredEnv(),
		hardRestartHelperEnv+"=1",
		"LOOPCODER_HR_PHASE=p3",
		"LOOPCODER_HR_HOME="+home,
		"LOOPCODER_HR_RESULT="+result3,
		"LOOPCODER_HR_PROJECT="+project,
		"LOOPCODER_HR_RUN="+runID,
		"LOOPCODER_HR_MODE=reuse",
	)
	out3, err3 := cmd3.CombinedOutput()
	if err3 != nil {
		t.Fatalf("p3: %v\n%s", err3, out3)
	}
	raw3, _ := os.ReadFile(result3)
	var r3 map[string]any
	if err := json.Unmarshal(raw3, &r3); err != nil {
		t.Fatal(err)
	}
	if str(r3["err"]) != "" {
		t.Fatalf("p3 err=%q full=%s", r3["err"], raw3)
	}
	if r3["status"] != workflowrun.StatusHumanGate {
		t.Fatalf("p3 status=%v want human_gate", r3["status"])
	}
	if n, _ := asInt(r3["launch_count"]); n != 0 {
		t.Fatalf("p3 launch_count=%v want 0", r3["launch_count"])
	}
	if n, _ := asInt(r3["reuse_count"]); n != 1 {
		t.Fatalf("p3 reuse_count=%v want exactly 1", r3["reuse_count"])
	}
	if n, _ := asInt(r3["exact_g1_children"]); n != 1 {
		t.Fatalf("p3 exact_g1_children=%v want 1 unique g1 succeeded child", r3["exact_g1_children"])
	}
	if !strings.HasSuffix(str(r3["reused_attempt_id"]), "-g1") {
		t.Fatalf("p3 reused_attempt_id=%q want *-g1", r3["reused_attempt_id"])
	}
	if str(r3["reused_terminal"]) != "succeeded" {
		t.Fatalf("p3 reused_terminal=%q want succeeded", r3["reused_terminal"])
	}
	if str(r3["reused_evidence"]) == "" {
		t.Fatal("p3 reused OutputEvidence empty")
	}
	if str(r3["g1_terminal_evidence"]) == "" {
		t.Fatal("p3 g1_terminal_evidence empty")
	}
	if str(r3["reused_evidence"]) != str(r3["g1_terminal_evidence"]) {
		t.Fatalf("p3 evidence mismatch reuse=%q g1=%q", r3["reused_evidence"], r3["g1_terminal_evidence"])
	}
}

func TestHardRestart_CompletedSuccess_ZeroRelaunch(t *testing.T) {
	if os.Getenv(hardRestartHelperEnv) == "1" {
		runHardRestartHelper(t)
		return
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	_ = os.MkdirAll(home, 0o700)
	state := filepath.Join(root, "state")
	_ = os.MkdirAll(state, 0o700)
	resultPath := filepath.Join(state, "ok.json")
	run := func(phase string) {
		cmd := exec.Command(os.Args[0], "-test.run=TestHardRestart_CompletedSuccess_ZeroRelaunch$", "-test.v")
		cmd.Env = append(filteredEnv(),
			hardRestartHelperEnv+"=1",
			"LOOPCODER_HR_PHASE="+phase,
			"LOOPCODER_HR_HOME="+home,
			"LOOPCODER_HR_RESULT="+resultPath,
			"LOOPCODER_HR_PROJECT=proj-hr-ok",
			"LOOPCODER_HR_RUN=run_hr_ok",
			"LOOPCODER_HR_MODE=success",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", phase, err, out)
		}
	}
	run("p1")
	run("p2")
	raw, _ := os.ReadFile(resultPath)
	var r map[string]any
	_ = json.Unmarshal(raw, &r)
	if str(r["err"]) != "" {
		t.Fatalf("p2 err=%q", r["err"])
	}
	if r["status"] != workflowrun.StatusHumanGate {
		t.Fatalf("status=%v want human_gate", r["status"])
	}
	if n, _ := asInt(r["launch_count"]); n != 0 {
		t.Fatalf("launch_count=%v want 0", r["launch_count"])
	}
	if n, _ := asInt(r["reuse_count"]); n != 1 {
		t.Fatalf("reuse_count=%v want 1", r["reuse_count"])
	}
	if !strings.HasSuffix(str(r["reused_attempt_id"]), "-g0") {
		t.Fatalf("completed-success must reuse g0, got %q", r["reused_attempt_id"])
	}
	if str(r["reused_terminal"]) != "succeeded" {
		t.Fatalf("reused_terminal=%q", r["reused_terminal"])
	}
}

func TestHardRestart_MultiOpen_SecondCorrupt_ZeroSideEffects(t *testing.T) {
	// Two open launches: first has real live process identity + valid open claim;
	// second has corrupt authority gen. Phase1 must fail before any kill/mutation.
	// KillAfterVerify=true + kill-spy proves first process is untouched.
	home := testHome(t)
	project, runID := "proj-hr-multi", "run_hr_multi"
	ctx := context.Background()

	// Live children (real identities) in their own process groups.
	liveA := exec.Command("/bin/sleep", "60")
	liveB := exec.Command("/bin/sleep", "60")
	liveA.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	liveB.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := liveA.Start(); err != nil {
		t.Fatal(err)
	}
	if err := liveB.Start(); err != nil {
		_ = liveA.Process.Kill()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.KillGroup(liveA.Process.Pid)
		_, _ = liveA.Process.Wait()
		_ = process.KillGroup(liveB.Process.Pid)
		_, _ = liveB.Process.Wait()
	})
	snapA, err := process.Snapshot(liveA.Process.Pid, time.Now())
	if err != nil || snapA.Ambiguous {
		t.Fatalf("snapA: err=%v ambig=%v", err, snapA.Ambiguous)
	}
	snapB, err := process.Snapshot(liveB.Process.Pid, time.Now())
	if err != nil || snapB.Ambiguous {
		t.Fatalf("snapB: err=%v ambig=%v", err, snapB.Ambiguous)
	}

	// Real plan digest from OneNode graph (canonical AttemptID).
	def := workflowrun.OneNodeDefinition("g-multi-a", "multi-a")
	// Two-item graph for a + b
	graph := workgraph.Graph{
		Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
		GraphID: "g-multi", Version: 1,
		Source: workgraph.SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "owner",
		Items: []workgraph.WorkItem{
			{Schema: workgraph.SchemaItem, ID: "a", Intent: "a", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1},
			{Schema: workgraph.SchemaItem, ID: "b", Intent: "b", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 2},
		},
		Limits: workgraph.DefaultLimits(),
	}
	graph.PlanDigest = workgraph.DigestGraph(graph)
	plan := graph.PlanDigest
	_ = def
	ccd := "sha256:" + strings.Repeat("d", 64)
	att0 := workflowrun.AttemptID("a", plan, runID, 0)
	att1 := workflowrun.AttemptID("b", plan, runID, 0)

	runDir, err := workflowrun.RunDurableDir(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claimPath := filepath.Join(runDir, "workclaims.json")
	cs, err := workclaim.OpenPath(claimPath, t0)
	if err != nil {
		t.Fatal(err)
	}
	// First claim must be real valid open claim; second also open (phase1 still sees both).
	var claimA, claimB *workclaim.Claim
	for _, item := range []struct {
		id, att string
		dst     **workclaim.Claim
	}{{"a", att0, &claimA}, {"b", att1, &claimB}} {
		cres, cerr := cs.Claim(workclaim.ClaimRequest{
			ProjectID: project, Graph: graph, WorkItemID: item.id, AttemptID: item.att,
			ExecutorID: "workflowrun", Lease: time.Hour,
			PlanDigest: plan, TaskClass: "tera", ChildContractDigest: ccd,
			Evidence: workgraph.TerminalEvidence{},
		})
		if cerr != nil || cres.Claim == nil {
			t.Fatalf("seed claim %s: code=%v err=%v reason=%s", item.id, cres.Code, cerr, cres.Reason)
		}
		*item.dst = cres.Claim
	}
	if claimA == nil || claimA.State != workclaim.StateClaimed && claimA.State != workclaim.StateRunning {
		t.Fatalf("first claim not open: %+v", claimA)
	}

	store, err := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	type liveItem struct {
		id, att string
		claim   *workclaim.Claim
		snap    process.Identity
		gen     int64 // authority claim gen — corrupt for b
	}
	items := []liveItem{
		{id: "a", att: att0, claim: claimA, snap: snapA, gen: int64(claimA.Generation)},
		{id: "b", att: att1, claim: claimB, snap: snapB, gen: 99}, // corrupt vs claim
	}
	for _, item := range items {
		owner := workflowrun.AuthorityOwnerFromClaimID(item.claim.ClaimID)
		_, err := storage.PersistProviderExecutionAuthority(ctx, store, storage.ProviderExecutionAuthority{
			ProjectID: project, RunID: runID, AttemptID: item.att,
			ProviderPID: item.snap.PID, ProviderPGID: item.snap.PGID,
			ProcessBirthIdentity: item.snap.ProcessBirthIdentity,
			ExecutableIdentity:   item.snap.ExecutableIdentity,
			OwnerID:              owner, ClaimGeneration: item.gen,
			WorktreePath: "/tmp/wt-" + item.id, LogPath: "/tmp/log-" + item.id,
		}, t0())
		if err != nil {
			t.Fatal(err)
		}
		gdig := workgraph.DigestGraph(graph)
		gid, gver := graph.GraphID, graph.Version
		basePL := fullChildIdentityPayload(project, runID, gid, gver, item.id, item.att, 1, plan, gdig, "tera", ccd, seedRouteFields())
		if _, aerr := elog.Append(workflowrun.Event{
			ProjectID: project, RunID: runID, Kind: "claim", WorkItemID: item.id,
			AttemptID: item.att, Generation: 1, GraphID: gid, GraphVersion: gver,
			ExecutionPlanDigest: plan, GraphDigest: gdig,
			TaskClass: "tera", ChildContractDigest: ccd, Payload: eventJSONPayloadHR(basePL),
		}); aerr != nil {
			t.Fatal(aerr)
		}
		if _, aerr := elog.Append(workflowrun.Event{
			ProjectID: project, RunID: runID, Kind: "launch", WorkItemID: item.id,
			AttemptID: item.att, Generation: 1, GraphID: gid, GraphVersion: gver,
			ExecutionPlanDigest: plan, GraphDigest: gdig,
			TaskClass: "tera", ChildContractDigest: ccd, Payload: eventJSONPayloadHR(basePL),
		}); aerr != nil {
			t.Fatal(aerr)
		}
		pidPL := fullChildIdentityPayload(project, runID, gid, gver, item.id, item.att, 1, plan, gdig, "tera", ccd, seedRouteFields())
		pidPL["pid"] = fmtInt(item.snap.PID)
		pidPL["pgid"] = fmtInt(item.snap.PGID)
		pidPL["process_birth_identity"] = item.snap.ProcessBirthIdentity
		pidPL["executable_identity"] = item.snap.ExecutableIdentity
		pidPL["observed_at"] = t0().UTC().Format(time.RFC3339Nano)
		pidPL["identity_ambiguous"] = "false"
		pidPL["worktree_path"] = "/tmp/wt-" + item.id
		pidPL["log_path"] = "/tmp/log-" + item.id
		if _, aerr := elog.Append(workflowrun.Event{
			ProjectID: project, RunID: runID, Kind: "pid", WorkItemID: item.id,
			AttemptID: item.att, Generation: 1, GraphID: gid, GraphVersion: gver, PID: item.snap.PID,
			Payload:             eventJSONPayloadHR(pidPL),
			ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera", ChildContractDigest: ccd,
		}); aerr != nil {
			t.Fatal(aerr)
		}
	}

	beforeLog, err := os.ReadFile(elog.Path())
	if err != nil {
		t.Fatal(err)
	}
	authPath, err := workflowrun.AuthorityStorePath(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	beforeAuth, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeClaim, err := os.ReadFile(claimPath)
	if err != nil {
		t.Fatal(err)
	}

	kills := 0
	n, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 50 * time.Millisecond, KillAfterVerify: true,
		PlanDigest: plan,
		OnKillGroup: func(pgid int) error {
			kills++
			return fmt.Errorf("kill-spy: unexpected kill pgid=%d", pgid)
		},
	})
	if rerr == nil {
		t.Fatalf("expected multi-open corrupt fail, n=%d", n)
	}
	if kills != 0 {
		t.Fatalf("kill-spy fired %d times; phase1 must not mutate/kill", kills)
	}
	afterLog, _ := os.ReadFile(elog.Path())
	afterAuth, _ := os.ReadFile(authPath)
	afterClaim, _ := os.ReadFile(claimPath)
	if string(beforeLog) != string(afterLog) {
		t.Fatal("event log mutated")
	}
	if string(beforeAuth) != string(afterAuth) {
		t.Fatal("authority mutated")
	}
	if string(beforeClaim) != string(afterClaim) {
		t.Fatal("claim store mutated")
	}
	// First live process untouched.
	if !process.Alive(snapA.PID) {
		t.Fatal("first valid process was killed/died; must remain alive")
	}
	if err := process.VerifySnapshot(snapA); err != nil {
		t.Fatalf("first process identity broken: %v", err)
	}
}

func claimGraphDigest(t *testing.T, home, project, runID, att string) string {
	t.Helper()
	runDir, err := workflowrun.RunDurableDir(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs.AllClaims() {
		if c.AttemptID == att {
			if c.GraphDigest == "" {
				t.Fatal("claim GraphDigest empty")
			}
			return c.GraphDigest
		}
	}
	t.Fatal("claim not found")
	return ""
}

// seedOpenRecoverableAttempt builds a pristine open launch+claim+authority+pid for one failpoint window.
func seedOpenRecoverableAttempt(t *testing.T, home, project, runID string) (plan, att string, pid int) {
	t.Helper()
	// Dead pid (process already exited) so ensureProviderDead returns without kill.
	cmd := exec.Command("/bin/sleep", "0.05")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid = cmd.Process.Pid
	_, _ = cmd.Process.Wait()

	def := workflowrun.OneNodeDefinition("g-fp", "fp")
	req := withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID, Definition: def, Actor: "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium", Permission: "bounded_write",
				AccountRef: "a", InstallRef: "i", WindowKind: "five_hour", ReservationID: "r"},
		},
	})
	plan = req.ExpectedPlanDigest
	runDir, err := workflowrun.RunDurableDir(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cs, err := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	if err != nil {
		t.Fatal(err)
	}
	graph := mustOneNodeGraph(t, "g-fp")
	gdig := workgraph.DigestGraph(graph)
	att = workflowrun.AttemptID("only", plan, runID, 0)
	ccd := "sha256:" + strings.Repeat("f", 64)
	cres, cerr := cs.Claim(workclaim.ClaimRequest{
		ProjectID: project, Graph: graph, WorkItemID: "only", AttemptID: att,
		ExecutorID: "workflowrun", Lease: time.Hour,
		PlanDigest: plan, TaskClass: "tera", ChildContractDigest: ccd,
		Evidence: workgraph.TerminalEvidence{},
	})
	if cerr != nil || cres.Claim == nil {
		t.Fatalf("seed claim: code=%v err=%v reason=%s", cres.Code, cerr, cres.Reason)
	}
	owner := workflowrun.AuthorityOwnerFromClaimID(cres.Claim.ClaimID)
	ctx := context.Background()
	store, err := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	if err != nil {
		t.Fatal(err)
	}
	// SpawnPhase lands in the same Persist write as the authority row.
	_, err = storage.PersistProviderExecutionAuthority(ctx, store, storage.ProviderExecutionAuthority{
		ProjectID: project, RunID: runID, AttemptID: att,
		ProviderPID: pid, ProviderPGID: pid,
		ProcessBirthIdentity: "dead-birth", ExecutableIdentity: "/bin/sleep",
		OwnerID: owner, ClaimGeneration: int64(cres.Claim.Generation),
		WorktreePath: "/tmp/wt-fp", LogPath: "/tmp/log-fp",
		SpawnPhase: storage.SpawnPhaseAuthorityPersisted,
	}, t0())
	if err != nil {
		t.Fatal(err)
	}
	// Hard-recovery seeds with full PID transition spawn_phase after pid event exists.
	fence := storage.ProviderExecutionAuthorityFence{
		ProjectID: project, RunID: runID, AttemptID: att,
		OwnerID: owner, ClaimGeneration: int64(cres.Claim.Generation),
	}
	if err := storage.TransitionProviderExecutionSpawnPhase(ctx, store, fence, t0(), storage.SpawnPhasePIDEventPersisted); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	now := t0()
	gid, gver := graph.GraphID, graph.Version
	idPayload := func(extra map[string]string) []byte {
		m := fullChildIdentityPayload(project, runID, gid, gver, "only", att, 1, plan, gdig, "tera", ccd, seedRouteFields())
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return b
	}
	if _, aerr := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "claim", WorkItemID: "only",
		AttemptID: att, Generation: 1, GraphID: gid, GraphVersion: gver,
		ExecutionPlanDigest: plan, GraphDigest: gdig,
		TaskClass: "tera", ChildContractDigest: ccd,
		Payload: idPayload(nil),
	}); aerr != nil {
		t.Fatal(aerr)
	}
	if _, aerr := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "launch", WorkItemID: "only",
		AttemptID: att, Generation: 1, GraphID: gid, GraphVersion: gver,
		ExecutionPlanDigest: plan, GraphDigest: gdig,
		TaskClass: "tera", ChildContractDigest: ccd,
		Payload: idPayload(seedRouteFields()),
	}); aerr != nil {
		t.Fatal(aerr)
	}
	if _, aerr := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "pid", WorkItemID: "only",
		AttemptID: att, Generation: 1, GraphID: gid, GraphVersion: gver, PID: pid,
		ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera", ChildContractDigest: ccd,
		Payload: idPayload(map[string]string{
			"pid": fmtInt(pid), "pgid": fmtInt(pid),
			"process_birth_identity": "dead-birth", "executable_identity": "/bin/sleep",
			"observed_at": now.UTC().Format(time.RFC3339Nano), "identity_ambiguous": "false",
			"worktree_path": "/tmp/wt-fp", "log_path": "/tmp/log-fp",
		}),
	}); aerr != nil {
		t.Fatal(aerr)
	}
	return plan, att, pid
}

func seedRouteFields() map[string]string {
	return map[string]string{
		"provider": "fixture", "model": "fixture-model", "depth": "medium",
		"permission": "bounded_write", "account_ref": "a", "install_ref": "i",
		"window_kind": "five_hour", "reservation_id": "r", "route_reason": "pin",
	}
}

func fullChildIdentityPayload(project, runID, graphID string, graphVer int, workItem, attempt string, gen int, plan, gdig, taskClass, ccd string, route map[string]string) map[string]string {
	m := map[string]string{
		"project_id": project, "run_id": runID,
		"graph_id": graphID, "graph_version": fmt.Sprintf("%d", graphVer),
		"work_item_id": workItem, "attempt_id": attempt,
		"generation":            fmt.Sprintf("%d", gen),
		"execution_plan_digest": plan, "graph_digest": gdig,
		"task_class": taskClass, "child_contract_digest": ccd,
	}
	for k, v := range route {
		m[k] = v
	}
	return m
}

func TestHardRestart_RecoverFailpoint_Idempotent(t *testing.T) {
	// Each crash window gets a pristine seed; failpoint fires AFTER named action;
	// second call converges; interrupt/terminal each exactly once; claim closed; authority complete.
	for _, fp := range []string{"after_interrupt", "after_terminal", "after_claim_close", "after_authority_complete"} {
		fp := fp
		t.Run(fp, func(t *testing.T) {
			home := testHome(t)
			project, runID := "proj-hr-fp-"+fp, "run_hr_fp_"+fp
			plan, att, _ := seedOpenRecoverableAttempt(t, home, project, runID)
			elog, err := workflowrun.OpenEventLog(home, project, runID)
			if err != nil {
				t.Fatal(err)
			}
			_, err1 := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
				HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
				WaitAlive: 100 * time.Millisecond, KillAfterVerify: false,
				FailAfter: fp, PlanDigest: plan,
			})
			if err1 == nil {
				t.Fatalf("failpoint %s: first call must error after action", fp)
			}
			if !strings.Contains(err1.Error(), fp) {
				t.Fatalf("failpoint %s: err=%v want substring %q", fp, err1, fp)
			}
			// Fresh second call (simulates new process) converges.
			_, err2 := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
				HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
				WaitAlive: 100 * time.Millisecond, KillAfterVerify: false,
				PlanDigest: plan,
			})
			if err2 != nil {
				t.Fatalf("converge after %s: %v", fp, err2)
			}
			// Third call is pure no-op (idempotent).
			n3, err3 := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
				HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
				WaitAlive: 100 * time.Millisecond, KillAfterVerify: false,
				PlanDigest: plan,
			})
			if err3 != nil {
				t.Fatalf("third call after %s: %v", fp, err3)
			}
			if n3 != 0 {
				t.Fatalf("third call n=%d want 0 (fully converged)", n3)
			}

			events, rerr := elog.ReadAllForRun(project, runID)
			if rerr != nil {
				t.Fatal(rerr)
			}
			nInt, nTerm := 0, 0
			var term string
			for _, ev := range events {
				if ev.AttemptID != att {
					continue
				}
				switch ev.Kind {
				case "interrupt":
					nInt++
				case "terminal":
					nTerm++
					term = ev.Terminal
				}
			}
			if nInt != 1 {
				t.Fatalf("%s interrupt count=%d want exactly 1", fp, nInt)
			}
			if nTerm != 1 {
				t.Fatalf("%s terminal count=%d want exactly 1", fp, nTerm)
			}
			if term != string(workgraph.TermCancelled) && term != string(workgraph.TermFailed) {
				t.Fatalf("%s terminal=%q want cancelled|failed", fp, term)
			}

			// Claim closed exactly once; authority completed.
			runDir, _ := workflowrun.RunDurableDir(home, project, runID)
			cs, cerr := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
			if cerr != nil {
				t.Fatal(cerr)
			}
			openClaims := 0
			closed := 0
			for _, c := range cs.AllClaims() {
				if c.AttemptID != att {
					continue
				}
				if c.State == workclaim.StateClaimed || c.State == workclaim.StateRunning {
					openClaims++
				}
				if c.State == workclaim.StateClosed {
					closed++
					if string(c.Terminal) != term {
						t.Fatalf("%s claim terminal %q != event %q", fp, c.Terminal, term)
					}
				}
			}
			if openClaims != 0 || closed != 1 {
				t.Fatalf("%s claims open=%d closed=%d want 0/1", fp, openClaims, closed)
			}
			ctx := context.Background()
			store, serr := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
			if serr != nil {
				t.Fatal(serr)
			}
			auth, aerr := storage.LoadProviderExecutionAuthority(ctx, store, project, runID, att)
			_ = store.Close()
			if aerr != nil {
				t.Fatal(aerr)
			}
			if strings.TrimSpace(auth.CompletedAt) == "" {
				t.Fatalf("%s authority still open", fp)
			}
			if strings.TrimSpace(auth.TerminalState) != term {
				t.Fatalf("%s authority terminal %q != event %q", fp, auth.TerminalState, term)
			}
		})
	}
}

// P0-1: crash after normal succeeded terminal, claim still open → preserve evidence.
func TestHardRestart_NormalSucceededTerminal_OpenClaim_NoHardRecoveryInject(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-hr-norm-open", "run_hr_norm_open"
	plan, att, _ := seedOpenRecoverableAttempt(t, home, project, runID)
	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	const evid = "ok:exact-success-evidence"
	// GraphDigest must match claim/launch (seed sets it from graph digest).
	gdig := claimGraphDigest(t, home, project, runID, att)
	ccd := "sha256:" + strings.Repeat("f", 64)
	termPL := fullChildIdentityPayload(project, runID, "g-fp", 1, "only", att, 1, plan, gdig, "tera", ccd, seedRouteFields())
	termPL["terminal"] = "succeeded"
	termPL["output_evidence"] = evid
	if _, err := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "terminal", WorkItemID: "only",
		AttemptID: att, Generation: 1, GraphID: "g-fp", GraphVersion: 1,
		Terminal: string(workgraph.TermSucceeded),
		Evidence: evid, ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera",
		ChildContractDigest: ccd,
		Payload:             eventJSONPayloadHR(termPL),
	}); err != nil {
		t.Fatal(err)
	}
	beforeLog, _ := os.ReadFile(elog.Path())
	n, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 100 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
	})
	if rerr != nil {
		t.Fatalf("normal converge: %v", rerr)
	}
	if n < 1 {
		t.Fatalf("n=%d want >=1", n)
	}
	afterLog, _ := os.ReadFile(elog.Path())
	// No new interrupt/terminal lines: log may only grow if we append — must not append.
	events, err := elog.ReadAllForRun(project, runID)
	if err != nil {
		t.Fatal(err)
	}
	nInt, nTerm := 0, 0
	var termEv workflowrun.Event
	for _, ev := range events {
		if ev.AttemptID != att {
			continue
		}
		switch ev.Kind {
		case "interrupt":
			nInt++
		case "terminal":
			nTerm++
			termEv = ev
		}
	}
	if nInt != 0 {
		t.Fatalf("interrupt count=%d want 0 (normal convergence)", nInt)
	}
	if nTerm != 1 {
		t.Fatalf("terminal count=%d want 1", nTerm)
	}
	if termEv.Evidence != evid || termEv.Terminal != "succeeded" {
		t.Fatalf("terminal mutated: term=%q evid=%q", termEv.Terminal, termEv.Evidence)
	}
	// Claim closed with exact values.
	runDir, _ := workflowrun.RunDurableDir(home, project, runID)
	cs, _ := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	var closed workclaim.Claim
	for _, c := range cs.AllClaims() {
		if c.AttemptID == att {
			closed = c
		}
	}
	if closed.State != workclaim.StateClosed {
		t.Fatalf("claim state=%s want closed", closed.State)
	}
	if string(closed.Terminal) != "succeeded" || closed.OutputEvidence != evid {
		t.Fatalf("claim term/evid=%q/%q want succeeded/%q", closed.Terminal, closed.OutputEvidence, evid)
	}
	ctx := context.Background()
	store, _ := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	auth, aerr := storage.LoadProviderExecutionAuthority(ctx, store, project, runID, att)
	_ = store.Close()
	if aerr != nil || strings.TrimSpace(auth.CompletedAt) == "" || auth.TerminalState != "succeeded" {
		t.Fatalf("authority incomplete or wrong: completed=%q term=%q err=%v", auth.CompletedAt, auth.TerminalState, aerr)
	}
	// Byte-identical terminal line retained (no hard-recovery rewrite).
	if !strings.Contains(string(afterLog), evid) {
		t.Fatal("evidence lost from log")
	}
	_ = beforeLog
}

// P0-1: after claim close, authority incomplete — same exact terminal, zero interrupt.
func TestHardRestart_NormalFailedTerminal_ClaimClosed_AuthCompleteOnly(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-hr-norm-fail", "run_hr_norm_fail"
	plan, att, _ := seedOpenRecoverableAttempt(t, home, project, runID)
	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	const evid = "failed:model_unavailable:only"
	gdig := claimGraphDigest(t, home, project, runID, att)
	ccd := "sha256:" + strings.Repeat("f", 64)
	termPL := fullChildIdentityPayload(project, runID, "g-fp", 1, "only", att, 1, plan, gdig, "tera", ccd, seedRouteFields())
	termPL["terminal"] = "failed"
	termPL["output_evidence"] = evid
	termPL["failure_class"] = "model_unavailable"
	if _, err := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "terminal", WorkItemID: "only",
		AttemptID: att, Generation: 1, GraphID: "g-fp", GraphVersion: 1,
		Terminal: string(workgraph.TermFailed), FailureClass: "model_unavailable",
		Evidence: evid, ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera",
		ChildContractDigest: ccd,
		Payload:             eventJSONPayloadHR(termPL),
	}); err != nil {
		t.Fatal(err)
	}
	runDir, _ := workflowrun.RunDurableDir(home, project, runID)
	cs, _ := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	var cl *workclaim.Claim
	for _, c := range cs.AllClaims() {
		if c.AttemptID == att {
			cp := c
			cl = &cp
			break
		}
	}
	if _, err := cs.Close(workclaim.CloseRequest{
		ClaimID: cl.ClaimID, Generation: cl.Generation, ExecutorID: "workflowrun", AttemptID: att,
		Terminal: workgraph.TermFailed, OutputEvidence: evid,
	}); err != nil {
		t.Fatal(err)
	}
	n, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 100 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
	})
	if rerr != nil {
		t.Fatal(rerr)
	}
	if n < 1 {
		t.Fatalf("n=%d", n)
	}
	events, _ := elog.ReadAllForRun(project, runID)
	for _, ev := range events {
		if ev.Kind == "interrupt" && ev.AttemptID == att {
			t.Fatal("must not inject interrupt on normal failed terminal")
		}
	}
	ctx := context.Background()
	store, _ := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	auth, _ := storage.LoadProviderExecutionAuthority(ctx, store, project, runID, att)
	_ = store.Close()
	if auth.TerminalState != "failed" || strings.TrimSpace(auth.CompletedAt) == "" {
		t.Fatalf("auth term=%q completed=%q", auth.TerminalState, auth.CompletedAt)
	}
}

// P0-1: completed authority terminal mismatches event → phase1 fail, zero mutation.
func TestHardRestart_CompletedAuthorityTerminalMismatch_NoMutation(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-hr-mismatch", "run_hr_mismatch"
	plan, att, _ := seedOpenRecoverableAttempt(t, home, project, runID)
	elog, _ := workflowrun.OpenEventLog(home, project, runID)
	gdig := claimGraphDigest(t, home, project, runID, att)
	if _, err := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "terminal", WorkItemID: "only",
		AttemptID: att, Generation: 1, Terminal: "succeeded", Evidence: "ok:a",
		ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera",
		ChildContractDigest: "sha256:" + strings.Repeat("f", 64),
	}); err != nil {
		t.Fatal(err)
	}
	runDir, _ := workflowrun.RunDurableDir(home, project, runID)
	cs, _ := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	var cl *workclaim.Claim
	for _, c := range cs.AllClaims() {
		if c.AttemptID == att {
			cp := c
			cl = &cp
		}
	}
	// Complete authority as failed while event says succeeded — corruption.
	ctx := context.Background()
	store, _ := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	owner := workflowrun.AuthorityOwnerFromClaimID(cl.ClaimID)
	if err := workflowrun.CompleteChildExecutionAuthority(ctx, store, project, runID, att, owner, cl.Generation, "failed", t0()); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	beforeLog, _ := os.ReadFile(elog.Path())
	claimPath := filepath.Join(runDir, "workclaims.json")
	beforeClaim, _ := os.ReadFile(claimPath)
	authPath, _ := workflowrun.AuthorityStorePath(home, project, runID)
	beforeAuth, _ := os.ReadFile(authPath)
	_, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 50 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
	})
	if rerr == nil {
		t.Fatal("expected phase1 fail on terminal mismatch")
	}
	afterLog, _ := os.ReadFile(elog.Path())
	afterClaim, _ := os.ReadFile(claimPath)
	afterAuth, _ := os.ReadFile(authPath)
	if string(beforeLog) != string(afterLog) || string(beforeClaim) != string(afterClaim) || string(beforeAuth) != string(afterAuth) {
		t.Fatal("mutation on corruption path")
	}
}

// P0-3: authority without PID (typed spawn_phase on authority row) converges without inventing pid.
func TestHardRestart_PrePIDAuthorityWindow_ConvergesWithoutPID(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-hr-prepid", "run_hr_prepid"
	// Seed without pid event: claim+launch+authority only; SpawnPhase in same Persist write.
	def := workflowrun.OneNodeDefinition("g-prepid", "prepid")
	req := withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID, Definition: def, Actor: "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium", Permission: "bounded_write",
				AccountRef: "a", InstallRef: "i", WindowKind: "five_hour", ReservationID: "r"},
		},
	})
	plan := req.ExpectedPlanDigest
	runDir, _ := workflowrun.RunDurableDir(home, project, runID)
	_ = os.MkdirAll(runDir, 0o700)
	cs, _ := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	graph := mustOneNodeGraph(t, "g-prepid")
	gdig := workgraph.DigestGraph(graph)
	att := workflowrun.AttemptID("only", plan, runID, 0)
	ccd := "sha256:" + strings.Repeat("f", 64)
	cres, cerr := cs.Claim(workclaim.ClaimRequest{
		ProjectID: project, Graph: graph, WorkItemID: "only", AttemptID: att,
		ExecutorID: "workflowrun", Lease: time.Hour,
		PlanDigest: plan, TaskClass: "tera", ChildContractDigest: ccd,
	})
	if cerr != nil || cres.Claim == nil {
		t.Fatalf("claim: %v %v", cerr, cres)
	}
	// Dead pid in authority (process never got durable pid event).
	cmd := exec.Command("/bin/sleep", "0.01")
	_ = cmd.Start()
	pid := cmd.Process.Pid
	_, _ = cmd.Process.Wait()
	ctx := context.Background()
	store, _ := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	owner := workflowrun.AuthorityOwnerFromClaimID(cres.Claim.ClaimID)
	// Typed phase is on the authority row in this same Persist write — not a later event.
	auth, err := storage.PersistProviderExecutionAuthority(ctx, store, storage.ProviderExecutionAuthority{
		ProjectID: project, RunID: runID, AttemptID: att,
		ProviderPID: pid, ProviderPGID: pid,
		ProcessBirthIdentity: "dead-birth", ExecutableIdentity: "/bin/sleep",
		OwnerID: owner, ClaimGeneration: cres.Claim.Generation,
		WorktreePath: "/tmp/wt-prepid", LogPath: "/tmp/log-prepid",
		SpawnPhase: storage.SpawnPhaseAuthorityPersisted,
	}, t0())
	if err != nil {
		t.Fatal(err)
	}
	if auth.SpawnPhase != storage.SpawnPhaseAuthorityPersisted {
		t.Fatalf("SpawnPhase=%q want authority_persisted in same Persist write", auth.SpawnPhase)
	}
	_ = store.Close()
	elog, _ := workflowrun.OpenEventLog(home, project, runID)
	gid, gver := graph.GraphID, graph.Version
	for _, kind := range []string{"claim", "launch"} {
		pl := fullChildIdentityPayload(project, runID, gid, gver, "only", att, 1, plan, gdig, "tera", ccd, seedRouteFields())
		ev := workflowrun.Event{
			ProjectID: project, RunID: runID, Kind: kind, WorkItemID: "only",
			AttemptID: att, Generation: 1, GraphID: gid, GraphVersion: gver,
			ExecutionPlanDigest: plan, GraphDigest: gdig,
			TaskClass: "tera", ChildContractDigest: ccd,
			Payload: eventJSONPayloadHR(pl),
		}
		if _, aerr := elog.Append(ev); aerr != nil {
			t.Fatal(aerr)
		}
	}
	// No pid event — recovery allowed only because authority.SpawnPhase is typed.
	n, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 100 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
	})
	if rerr != nil {
		t.Fatalf("pre-PID recover: %v", rerr)
	}
	if n < 1 {
		t.Fatalf("n=%d", n)
	}
	events, _ := elog.ReadAllForRun(project, runID)
	nPID, nTerm := 0, 0
	for _, ev := range events {
		if ev.AttemptID != att {
			continue
		}
		if ev.Kind == "pid" {
			nPID++
		}
		if ev.Kind == "terminal" {
			nTerm++
			if ev.Terminal != "failed" || !strings.Contains(ev.Evidence, "pid_event") {
				t.Fatalf("pre-PID terminal=%q evid=%q", ev.Terminal, ev.Evidence)
			}
		}
	}
	if nPID != 0 {
		t.Fatalf("must not invent pid event, got %d", nPID)
	}
	if nTerm != 1 {
		t.Fatalf("terminal=%d want 1", nTerm)
	}
}

// Phase-1 whole-state: candidate A dead/recoverable + B exact-live with KillAfterVerify=false
// must fail before ANY durable mutation (A and B bytes stable, zero kill).
func TestHardRestart_MultiCandidate_BExactLive_ZeroMutation(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-hr-multi-state", "run_hr_multi_state"
	ctx := context.Background()

	// A: dead process, open launch — recoverable hard path if alone.
	// B: exact-live process — phase1 fail with KillAfterVerify=false.
	deadCmd := exec.Command("/bin/sleep", "0.01")
	if err := deadCmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadPID := deadCmd.Process.Pid
	_, _ = deadCmd.Process.Wait()

	live := exec.Command("/bin/sleep", "60")
	live.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.KillGroup(live.Process.Pid)
		_, _ = live.Process.Wait()
	})
	snapB, err := process.Snapshot(live.Process.Pid, time.Now())
	if err != nil || snapB.Ambiguous {
		t.Fatalf("snapB: %v", err)
	}

	graph := workgraph.Graph{
		Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
		GraphID: "g-multi-state", Version: 1,
		Source: workgraph.SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "owner",
		Items: []workgraph.WorkItem{
			{Schema: workgraph.SchemaItem, ID: "a", Intent: "a", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1},
			{Schema: workgraph.SchemaItem, ID: "b", Intent: "b", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 2},
		},
		Limits: workgraph.DefaultLimits(),
	}
	graph.PlanDigest = workgraph.DigestGraph(graph)
	plan := graph.PlanDigest
	gdig := plan
	// use DigestGraph for GraphDigest field separately
	gdig = workgraph.DigestGraph(graph)
	ccd := "sha256:" + strings.Repeat("e", 64)
	attA := workflowrun.AttemptID("a", plan, runID, 0)
	attB := workflowrun.AttemptID("b", plan, runID, 0)

	runDir, _ := workflowrun.RunDurableDir(home, project, runID)
	_ = os.MkdirAll(runDir, 0o700)
	claimPath := filepath.Join(runDir, "workclaims.json")
	cs, _ := workclaim.OpenPath(claimPath, t0)
	var claimA, claimB *workclaim.Claim
	for _, item := range []struct {
		id, att string
		dst     **workclaim.Claim
	}{{"a", attA, &claimA}, {"b", attB, &claimB}} {
		cres, cerr := cs.Claim(workclaim.ClaimRequest{
			ProjectID: project, Graph: graph, WorkItemID: item.id, AttemptID: item.att,
			ExecutorID: "workflowrun", Lease: time.Hour,
			PlanDigest: plan, TaskClass: "tera", ChildContractDigest: ccd,
		})
		if cerr != nil || cres.Claim == nil {
			t.Fatalf("claim %s: %v", item.id, cerr)
		}
		*item.dst = cres.Claim
	}

	store, _ := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	defer store.Close()
	type seed struct {
		id, att string
		claim   *workclaim.Claim
		pid     int
		pgid    int
		birth   string
		exe     string
	}
	seeds := []seed{
		{id: "a", att: attA, claim: claimA, pid: deadPID, pgid: deadPID, birth: "dead-birth-a", exe: "/bin/sleep"},
		{id: "b", att: attB, claim: claimB, pid: snapB.PID, pgid: snapB.PGID, birth: snapB.ProcessBirthIdentity, exe: snapB.ExecutableIdentity},
	}
	elog, _ := workflowrun.OpenEventLog(home, project, runID)
	for _, s := range seeds {
		owner := workflowrun.AuthorityOwnerFromClaimID(s.claim.ClaimID)
		auth, err := storage.PersistProviderExecutionAuthority(ctx, store, storage.ProviderExecutionAuthority{
			ProjectID: project, RunID: runID, AttemptID: s.att,
			ProviderPID: s.pid, ProviderPGID: s.pgid,
			ProcessBirthIdentity: s.birth, ExecutableIdentity: s.exe,
			OwnerID: owner, ClaimGeneration: s.claim.Generation,
			WorktreePath: "/tmp/wt-" + s.id, LogPath: "/tmp/log-" + s.id,
		}, t0())
		if err != nil {
			t.Fatal(err)
		}
		fence := storage.ProviderExecutionAuthorityFence{
			ProjectID: auth.ProjectID, RunID: auth.RunID, AttemptID: auth.AttemptID,
			OwnerID: auth.OwnerID, ClaimGeneration: auth.ClaimGeneration,
		}
		if err := storage.TransitionProviderExecutionSpawnPhase(ctx, store, fence, t0(), storage.SpawnPhasePIDEventPersisted); err != nil {
			t.Fatal(err)
		}
		lp, _ := json.Marshal(map[string]string{
			"provider": "fixture", "model": "fixture-model", "depth": "medium",
			"permission": "bounded_write", "account_ref": "a", "install_ref": "i",
			"window_kind": "five_hour", "reservation_id": "r", "route_reason": "pin",
		})
		for _, kind := range []string{"claim", "launch"} {
			ev := workflowrun.Event{
				ProjectID: project, RunID: runID, Kind: kind, WorkItemID: s.id,
				AttemptID: s.att, Generation: 1, ExecutionPlanDigest: plan, GraphDigest: gdig,
				TaskClass: "tera", ChildContractDigest: ccd,
			}
			if kind == "launch" {
				ev.Payload = lp
			}
			if _, aerr := elog.Append(ev); aerr != nil {
				t.Fatal(aerr)
			}
		}
		payload, _ := json.Marshal(map[string]string{
			"pid": fmtInt(s.pid), "pgid": fmtInt(s.pgid),
			"process_birth_identity": s.birth, "executable_identity": s.exe,
			"observed_at": t0().UTC().Format(time.RFC3339Nano), "identity_ambiguous": "false",
		})
		if _, aerr := elog.Append(workflowrun.Event{
			ProjectID: project, RunID: runID, Kind: "pid", WorkItemID: s.id,
			AttemptID: s.att, Generation: 1, PID: s.pid, Payload: payload,
			ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera", ChildContractDigest: ccd,
		}); aerr != nil {
			t.Fatal(aerr)
		}
	}

	beforeLog, _ := os.ReadFile(elog.Path())
	authPath, _ := workflowrun.AuthorityStorePath(home, project, runID)
	beforeAuth, _ := os.ReadFile(authPath)
	beforeClaim, _ := os.ReadFile(claimPath)
	kills := 0
	_, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 50 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
		OnKillGroup: func(int) error { kills++; return fmt.Errorf("unexpected kill") },
	})
	if rerr == nil {
		t.Fatal("expected phase1 fail: B exact-live with KillAfterVerify=false")
	}
	if kills != 0 {
		t.Fatalf("kills=%d want 0", kills)
	}
	afterLog, _ := os.ReadFile(elog.Path())
	afterAuth, _ := os.ReadFile(authPath)
	afterClaim, _ := os.ReadFile(claimPath)
	if string(beforeLog) != string(afterLog) {
		t.Fatal("event log mutated despite multi-candidate phase1 fail")
	}
	if string(beforeAuth) != string(afterAuth) {
		t.Fatal("authority mutated despite multi-candidate phase1 fail")
	}
	if string(beforeClaim) != string(afterClaim) {
		t.Fatal("claim mutated despite multi-candidate phase1 fail")
	}
	if !process.Alive(snapB.PID) {
		t.Fatal("B process must remain alive")
	}
}

// Legacy/untyped spawn_phase cannot auto-recover without PID.
func TestHardRestart_LegacySpawnPhase_NoAutoRecover(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-hr-legacy-phase", "run_hr_legacy_phase"
	plan, att, pid := seedOpenRecoverableAttempt(t, home, project, runID)
	// Force authority back to legacy_unknown and remove pid event by... we can't remove
	// pid easily. Instead re-seed: open authority, set spawn_phase legacy, no - wait seed has PID.
	// Use prePID-style without PID and force legacy phase.
	_ = plan
	_ = att
	_ = pid
	// Rebuild: claim+launch+authority legacy, no pid.
	home2 := testHome(t)
	project, runID = "proj-hr-leg2", "run_hr_leg2"
	def := workflowrun.OneNodeDefinition("g-leg", "leg")
	req := withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID, Definition: def, Actor: "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium", Permission: "bounded_write",
				AccountRef: "a", InstallRef: "i", WindowKind: "five_hour", ReservationID: "r"},
		},
	})
	plan = req.ExpectedPlanDigest
	runDir, _ := workflowrun.RunDurableDir(home2, project, runID)
	_ = os.MkdirAll(runDir, 0o700)
	cs, _ := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	graph := mustOneNodeGraph(t, "g-leg")
	gdig := workgraph.DigestGraph(graph)
	att = workflowrun.AttemptID("only", plan, runID, 0)
	ccd := "sha256:" + strings.Repeat("f", 64)
	cres, _ := cs.Claim(workclaim.ClaimRequest{
		ProjectID: project, Graph: graph, WorkItemID: "only", AttemptID: att,
		ExecutorID: "workflowrun", Lease: time.Hour, PlanDigest: plan, TaskClass: "tera", ChildContractDigest: ccd,
	})
	ctx := context.Background()
	store, _ := workflowrun.OpenAuthorityStore(ctx, home2, project, runID, t0)
	owner := workflowrun.AuthorityOwnerFromClaimID(cres.Claim.ClaimID)
	_, err := storage.PersistProviderExecutionAuthority(ctx, store, storage.ProviderExecutionAuthority{
		ProjectID: project, RunID: runID, AttemptID: att,
		ProviderPID: 8881, ProviderPGID: 8881,
		ProcessBirthIdentity: "x", ExecutableIdentity: "/bin/sleep",
		OwnerID: owner, ClaimGeneration: cres.Claim.Generation,
		WorktreePath: "/tmp/wt", LogPath: "/tmp/log",
		SpawnPhase: storage.SpawnPhaseAuthorityPersisted,
	}, t0())
	if err != nil {
		t.Fatal(err)
	}
	// Force legacy_unknown after Persist (simulates pre-v33 row).
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_execution_authorities SET spawn_phase = ? WHERE attempt_id = ?`,
			storage.SpawnPhaseLegacyUnknown, att)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	elog, _ := workflowrun.OpenEventLog(home2, project, runID)
	lp, _ := json.Marshal(map[string]string{
		"provider": "fixture", "model": "fixture-model", "depth": "medium",
		"permission": "bounded_write", "account_ref": "a", "install_ref": "i",
		"window_kind": "five_hour", "reservation_id": "r", "route_reason": "pin",
	})
	for _, kind := range []string{"claim", "launch"} {
		ev := workflowrun.Event{
			ProjectID: project, RunID: runID, Kind: kind, WorkItemID: "only",
			AttemptID: att, Generation: 1, ExecutionPlanDigest: plan, GraphDigest: gdig,
			TaskClass: "tera", ChildContractDigest: ccd,
		}
		if kind == "launch" {
			ev.Payload = lp
		}
		_, _ = elog.Append(ev)
	}
	before, _ := os.ReadFile(elog.Path())
	_, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home2, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 50 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
	})
	if rerr == nil {
		t.Fatal("legacy_unknown without PID must not recover")
	}
	after, _ := os.ReadFile(elog.Path())
	if string(before) != string(after) {
		t.Fatal("log mutated on legacy_unknown fail")
	}
}

// P0-4: exact-live hard recovery with KillAfterVerify=false fails before mutation (zero kill).
func TestHardRestart_ExactLive_NoKillWhenKillAfterFalse(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-hr-live", "run_hr_live"
	// Seed open launch with real live identity.
	live := exec.Command("/bin/sleep", "30")
	live.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.KillGroup(live.Process.Pid)
		_, _ = live.Process.Wait()
	})
	snap, err := process.Snapshot(live.Process.Pid, time.Now())
	if err != nil || snap.Ambiguous {
		t.Fatalf("snap: %v", err)
	}
	def := workflowrun.OneNodeDefinition("g-live", "live")
	req := withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: project, RunID: runID, Definition: def, Actor: "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {Provider: "fixture", Model: "fixture-model", TaskClass: "tera", Depth: "medium", Permission: "bounded_write",
				AccountRef: "a", InstallRef: "i", WindowKind: "five_hour", ReservationID: "r"},
		},
	})
	plan := req.ExpectedPlanDigest
	runDir, _ := workflowrun.RunDurableDir(home, project, runID)
	_ = os.MkdirAll(runDir, 0o700)
	cs, _ := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	graph := mustOneNodeGraph(t, "g-live")
	att := workflowrun.AttemptID("only", plan, runID, 0)
	ccd := "sha256:" + strings.Repeat("f", 64)
	cres, cerr := cs.Claim(workclaim.ClaimRequest{
		ProjectID: project, Graph: graph, WorkItemID: "only", AttemptID: att,
		ExecutorID: "workflowrun", Lease: time.Hour, PlanDigest: plan, TaskClass: "tera", ChildContractDigest: ccd,
	})
	if cerr != nil || cres.Claim == nil {
		t.Fatalf("claim: %v", cerr)
	}
	ctx := context.Background()
	store, _ := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	owner := workflowrun.AuthorityOwnerFromClaimID(cres.Claim.ClaimID)
	_, err = storage.PersistProviderExecutionAuthority(ctx, store, storage.ProviderExecutionAuthority{
		ProjectID: project, RunID: runID, AttemptID: att,
		ProviderPID: snap.PID, ProviderPGID: snap.PGID,
		ProcessBirthIdentity: snap.ProcessBirthIdentity, ExecutableIdentity: snap.ExecutableIdentity,
		OwnerID: owner, ClaimGeneration: cres.Claim.Generation,
		WorktreePath: "/tmp/wt-live", LogPath: "/tmp/log-live",
	}, t0())
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	elog, _ := workflowrun.OpenEventLog(home, project, runID)
	lp, _ := json.Marshal(map[string]string{
		"provider": "fixture", "model": "fixture-model", "depth": "medium",
		"permission": "bounded_write", "account_ref": "a", "install_ref": "i",
		"window_kind": "five_hour", "reservation_id": "r", "route_reason": "pin",
	})
	gdig := workgraph.DigestGraph(graph)
	for _, kind := range []string{"claim", "launch"} {
		ev := workflowrun.Event{
			ProjectID: project, RunID: runID, Kind: kind, WorkItemID: "only",
			AttemptID: att, Generation: 1, ExecutionPlanDigest: plan, GraphDigest: gdig,
			TaskClass: "tera", ChildContractDigest: ccd,
		}
		if kind == "launch" {
			ev.Payload = lp
		}
		if _, aerr := elog.Append(ev); aerr != nil {
			t.Fatal(aerr)
		}
	}
	payload, _ := json.Marshal(map[string]string{
		"pid": fmtInt(snap.PID), "pgid": fmtInt(snap.PGID),
		"process_birth_identity": snap.ProcessBirthIdentity, "executable_identity": snap.ExecutableIdentity,
		"observed_at": t0().UTC().Format(time.RFC3339Nano), "identity_ambiguous": "false",
	})
	if _, aerr := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "pid", WorkItemID: "only",
		AttemptID: att, Generation: 1, PID: snap.PID, Payload: payload,
		ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera", ChildContractDigest: ccd,
	}); aerr != nil {
		t.Fatal(aerr)
	}
	beforeLog, _ := os.ReadFile(elog.Path())
	beforeClaim, _ := os.ReadFile(filepath.Join(runDir, "workclaims.json"))
	authPath, _ := workflowrun.AuthorityStorePath(home, project, runID)
	beforeAuth, _ := os.ReadFile(authPath)
	kills := 0
	_, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 80 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
		OnKillGroup: func(int) error { kills++; return nil },
	})
	if rerr == nil {
		t.Fatal("exact-live without KillAfterVerify must fail before mutation")
	}
	if kills != 0 {
		t.Fatalf("kills=%d want 0", kills)
	}
	afterLog, _ := os.ReadFile(elog.Path())
	afterClaim, _ := os.ReadFile(filepath.Join(runDir, "workclaims.json"))
	afterAuth, _ := os.ReadFile(authPath)
	if string(beforeLog) != string(afterLog) || string(beforeClaim) != string(afterClaim) || string(beforeAuth) != string(afterAuth) {
		t.Fatal("durable mutation despite exact-live fail-before-mutation")
	}
	if !process.Alive(snap.PID) {
		t.Fatal("live process must remain alive")
	}
}

// Normal success terminal + claim closed + authority still open must still be a
// recovery candidate (finalize crash after claim close / authority complete fail).
func TestHardRestart_IncompleteAuthorityAfterSuccessTerminal(t *testing.T) {
	home := testHome(t)
	project, runID := "proj-hr-auth-leak", "run_hr_auth_leak"
	plan, att, pid := seedOpenRecoverableAttempt(t, home, project, runID)
	elog, err := workflowrun.OpenEventLog(home, project, runID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate normal finalize: terminal succeeded + claim closed, authority incomplete.
	gdig := claimGraphDigest(t, home, project, runID, att)
	ccd := "sha256:" + strings.Repeat("f", 64)
	termPL := fullChildIdentityPayload(project, runID, "g-fp", 1, "only", att, 1, plan, gdig, "tera", ccd, seedRouteFields())
	termPL["terminal"] = "succeeded"
	termPL["output_evidence"] = "ok:success"
	if _, err := elog.Append(workflowrun.Event{
		ProjectID: project, RunID: runID, Kind: "terminal", WorkItemID: "only",
		AttemptID: att, Generation: 1, GraphID: "g-fp", GraphVersion: 1,
		Terminal: string(workgraph.TermSucceeded),
		Evidence: "ok:success", ExecutionPlanDigest: plan, GraphDigest: gdig, TaskClass: "tera",
		ChildContractDigest: ccd,
		Payload:             eventJSONPayloadHR(termPL),
	}); err != nil {
		t.Fatal(err)
	}
	runDir, _ := workflowrun.RunDurableDir(home, project, runID)
	cs, err := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), t0)
	if err != nil {
		t.Fatal(err)
	}
	var claim *workclaim.Claim
	for _, c := range cs.AllClaims() {
		if c.AttemptID == att {
			cp := c
			claim = &cp
			break
		}
	}
	if claim == nil {
		t.Fatal("missing claim")
	}
	if _, err := cs.Close(workclaim.CloseRequest{
		ClaimID: claim.ClaimID, Generation: claim.Generation,
		ExecutorID: "workflowrun", AttemptID: att,
		Terminal: workgraph.TermSucceeded, OutputEvidence: "ok:success",
	}); err != nil {
		t.Fatal(err)
	}
	// Authority still incomplete by construction.
	ctx := context.Background()
	store, err := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := storage.LoadProviderExecutionAuthority(ctx, store, project, runID, att)
	if err != nil || strings.TrimSpace(auth.CompletedAt) != "" {
		t.Fatalf("precondition: incomplete auth required, completed=%q err=%v pid=%d", auth.CompletedAt, err, pid)
	}
	_ = store.Close()

	n, rerr := workflowrun.RecoverOpenLaunchInterruptsAuthoritative(elog, workflowrun.RecoverOptions{
		HomeDir: home, ProjectID: project, RunID: runID, Now: t0,
		WaitAlive: 100 * time.Millisecond, KillAfterVerify: false, PlanDigest: plan,
	})
	if rerr != nil {
		t.Fatalf("recover incomplete authority: %v", rerr)
	}
	if n < 1 {
		t.Fatalf("n=%d want >=1 (authority complete only)", n)
	}
	store, err = workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
	if err != nil {
		t.Fatal(err)
	}
	auth, err = storage.LoadProviderExecutionAuthority(ctx, store, project, runID, att)
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(auth.CompletedAt) == "" {
		t.Fatal("authority still incomplete after recover")
	}
	if auth.TerminalState != string(workgraph.TermSucceeded) {
		t.Fatalf("authority terminal=%q want succeeded", auth.TerminalState)
	}
	// No hard-recovery interrupt injected for normal success path.
	events, err := elog.ReadAllForRun(project, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == "interrupt" && ev.AttemptID == att {
			t.Fatal("must not inject hard-recovery interrupt on normal success finalize leak")
		}
	}
}

func eventJSONPayloadHR(m map[string]string) []byte {
	b, _ := json.Marshal(m)
	return b
}

func mustOneNodeGraph(t *testing.T, graphID string) workgraph.Graph {
	t.Helper()
	g := workgraph.Graph{
		Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
		GraphID: graphID, Version: 1,
		Source: workgraph.SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "owner",
		Items: []workgraph.WorkItem{
			{Schema: workgraph.SchemaItem, ID: "only", Intent: "fp", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1},
		},
		Limits: workgraph.DefaultLimits(),
	}
	g.PlanDigest = workgraph.DigestGraph(g)
	return g
}

// Production concurrentPeaks max/overlap covered by package workflowrun peaks_test.go.

// Executor error must never produce succeeded terminal/claim/integration (primary).
func TestService_PrimaryExecutorError_NoSucceededTerminal(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	base := workflowrun.FakeChildExecutor{HomeDir: home, Now: t0, ForceProcessPID: true,
		ProductFiles: map[string][]string{"only": {"notes/only.go"}}}
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: errWrappingExecutor{inner: base, err: fmt.Errorf("injected executor boom")},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-exec-err", RunID: "run_exec_err", RepoPath: repo,
		Integrator: &countingEnsureIntegrator{},
		Definition: workflowrun.OneNodeDefinition("g-exec-err", "exec"),
		Actor:      "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "fixture", Model: "fixture-model", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "a", InstallRef: "i", WindowKind: "five_hour", ReservationID: "r",
			},
		},
	}))
	if err == nil {
		t.Fatal("expected error from executor path")
	}
	for _, c := range res.Children {
		if c.Terminal == "succeeded" {
			t.Fatalf("executor error must not yield succeeded child: %+v", c)
		}
	}
	if res.WorktreeActive != 0 {
		t.Fatalf("WorktreeActive=%d want 0", res.WorktreeActive)
	}
}

// errWrappingExecutor returns a succeeded-looking result together with a non-nil error
// (the forbidden coexistence that Service must rewrite to failed).
type errWrappingExecutor struct {
	inner workflowrun.FakeChildExecutor
	err   error
}

func (e errWrappingExecutor) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	out, _ := e.inner.Execute(ctx, in)
	out.Terminal = workgraph.TermSucceeded
	if strings.TrimSpace(out.OutputEvidence) == "" {
		out.OutputEvidence = "sha256:fake_evidence"
	}
	return out, e.err
}

// P0-6: Service success path leaves WorktreeActive=0 and ProcessActive=0 (real peaks).
func TestService_WorktreeAndProcessActiveZero_AfterSuccess(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, ForceProcessPID: true,
			ProductFiles: map[string][]string{"only": {"notes/only.go"}},
		},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-act0", RunID: "run_act0", RepoPath: repo,
		Integrator: &countingEnsureIntegrator{},
		Definition: workflowrun.OneNodeDefinition("g-act0", "act0"),
		Actor:      "owner",
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "fixture", Model: "fixture-model", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "a", InstallRef: "i", WindowKind: "five_hour", ReservationID: "r",
			},
		},
	}))
	if err != nil {
		t.Fatalf("%v status=%s", err, res.Status)
	}
	if res.WorktreeActive != 0 {
		t.Fatalf("WorktreeActive=%d want 0", res.WorktreeActive)
	}
	if res.ProcessActive != 0 {
		t.Fatalf("ProcessActive=%d want 0", res.ProcessActive)
	}
	if res.WorktreePeak < 1 {
		t.Fatalf("WorktreePeak=%d want >=1", res.WorktreePeak)
	}
}

// P0-6: model_unavailable alternate path sequential worktree peak and active=0.
func TestService_AlternateErrorFamily_ActiveZero(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, FailModel: "model-unavailable-token", ForceProcessPID: true,
			ProductFiles: map[string][]string{"only": {"notes/only.go"}},
		},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-alt-act", RunID: "run_alt_act", RepoPath: repo,
		Integrator: &countingEnsureIntegrator{},
		Definition: workflowrun.OneNodeDefinition("g-alt-act", "alt"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-ag", InstallRef: "install-ag", WindowKind: "five_hour",
				ReservationID: "res-ag", RouteReason: "pin",
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
		t.Fatalf("%v", err)
	}
	if res.WorktreeActive != 0 || res.ProcessActive != 0 {
		t.Fatalf("active w=%d p=%d want 0/0", res.WorktreeActive, res.ProcessActive)
	}
	if res.WorktreePeak > 1 {
		t.Fatalf("WorktreePeak=%d want <=1 sequential", res.WorktreePeak)
	}
	if res.ProcessPeak > 1 {
		t.Fatalf("ProcessPeak=%d want <=1", res.ProcessPeak)
	}
}

func TestProcessPeak_SequentialReroute_AtMostOne(t *testing.T) {
	home := testHome(t)
	repo := initGitRepo(t)
	svc := workflowrun.Service{
		Now: t0, HomeDir: home,
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: t0, FailModel: "model-unavailable-token", ForceProcessPID: true,
			ProductFiles: map[string][]string{"only": {"notes/only.go"}},
		},
	}
	res, err := svc.Execute(context.Background(), withExpectedPlanDigest(t, workflowrun.Request{
		ProjectID: "proj-peak2", RunID: "run_peak2", RepoPath: repo,
		Integrator: &countingEnsureIntegrator{},
		Definition: workflowrun.OneNodeDefinition("g-peak2", "peak"),
		Actor:      "owner", CapacityReroute: passThroughCapHook{},
		ChildRoutes: map[string]workflowrun.ChildRoute{
			"only": {
				Provider: "antigravity", Model: "model-unavailable-token", TaskClass: "tera",
				Depth: "medium", Permission: "bounded_write",
				AccountRef: "acct-ag", InstallRef: "install-ag", WindowKind: "five_hour",
				ReservationID: "res-ag", RouteReason: "pin",
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
		t.Fatalf("%v", err)
	}
	if res.LaunchCount < 2 {
		t.Fatalf("LaunchCount=%d", res.LaunchCount)
	}
	if res.ProcessPeak > 1 {
		t.Fatalf("ProcessPeak=%d", res.ProcessPeak)
	}
	if res.WorktreePeak > 1 {
		t.Fatalf("WorktreePeak=%d", res.WorktreePeak)
	}
}

func runHardRestartHelper(t *testing.T) {
	t.Helper()
	phase := os.Getenv("LOOPCODER_HR_PHASE")
	home := os.Getenv("LOOPCODER_HR_HOME")
	project := os.Getenv("LOOPCODER_HR_PROJECT")
	runID := os.Getenv("LOOPCODER_HR_RUN")
	mode := os.Getenv("LOOPCODER_HR_MODE")
	os.Setenv("LOOPCODER_HOME", home)

	prod := func(runner agent.Runner) workflowrun.ProductionChildExecutor {
		return workflowrun.ProductionChildExecutor{
			HomeDir: home, Now: t0, HardCap: 2 * time.Minute,
			Lookup: func(string) (agent.Runner, error) { return runner, nil },
		}
	}

	if phase == "p1" && mode != "success" {
		readyPath := os.Getenv("LOOPCODER_HR_READY")
		svc := workflowrun.Service{
			Now: t0, HomeDir: home,
			Executor: prod(hangSpawnRunner{Mode: "hang"}),
			TestAfterGuardianReady: func(ps workflowrun.ProcessStart, guardianPID int, diagPath string) {
				// Read claim owner from authority store.
				ctx := context.Background()
				store, err := workflowrun.OpenAuthorityStore(ctx, home, project, runID, t0)
				if err != nil {
					return
				}
				// Find attempt from latest pid — use last claim attempt from events later;
				// authority load by listing is hard; write attempt from ProcessStart not available.
				// Service passes attempt via diagnostic path name; load from event log.
				elog, _ := workflowrun.OpenEventLog(home, project, runID)
				events, _ := elog.ReadAllForRun(project, runID)
				var att, owner string
				var gen int
				for _, ev := range events {
					if ev.Kind == "pid" && ev.PID == ps.PID {
						att = ev.AttemptID
						gen = ev.Generation
					}
				}
				if att != "" {
					if auth, aerr := storage.LoadProviderExecutionAuthority(ctx, store, project, runID, att); aerr == nil {
						owner = auth.OwnerID
						if gen == 0 {
							gen = int(auth.ClaimGeneration)
						}
					}
				}
				_ = store.Close()
				ready := map[string]any{
					"pid": ps.PID, "pgid": ps.PGID,
					"birth": ps.ProcessBirthIdentity, "executable": ps.ExecutableIdentity,
					"attempt_id": att, "claim_generation": gen,
					"owner_id": owner, "project_id": project, "run_id": runID,
					"diagnostic_path": diagPath, "guardian_pid": guardianPID,
				}
				raw, _ := json.Marshal(ready)
				_ = os.WriteFile(readyPath, raw, 0o600)
			},
		}
		req := spawnBaseReq(t, project, runID)
		if req.AttemptGeneration != nil {
			t.Fatal("p1 AttemptGeneration must be nil")
		}
		_, _ = svc.Execute(context.Background(), req)
		select {}
	}

	if phase == "p1" && mode == "success" {
		repo := initGitRepo(t)
		svc := workflowrun.Service{Now: t0, HomeDir: home, Executor: prod(hangSpawnRunner{Mode: "product"})}
		req := spawnBaseReq(t, project, runID)
		req.RepoPath = repo
		req.Integrator = &countingEnsureIntegrator{}
		res, err := svc.Execute(context.Background(), req)
		writeHR(os.Getenv("LOOPCODER_HR_RESULT"), map[string]any{
			"status": res.Status, "launch_count": res.LaunchCount, "err": errStr(err),
		})
		return
	}

	if phase == "p2" && mode == "success" {
		repo := initGitRepo(t)
		svc := workflowrun.Service{Now: t0, HomeDir: home, Executor: prod(hangSpawnRunner{Mode: "product"})}
		req := spawnBaseReq(t, project, runID)
		req.RepoPath = repo
		req.Integrator = &countingEnsureIntegrator{}
		res, err := svc.Execute(context.Background(), req)
		att, term, ev := "", "", ""
		for _, c := range res.Children {
			if c.WorkItemID == "only" && c.Terminal == "succeeded" {
				att, term, ev = c.AttemptID, c.Terminal, c.OutputEvidence
			}
		}
		writeHR(os.Getenv("LOOPCODER_HR_RESULT"), map[string]any{
			"status": res.Status, "launch_count": res.LaunchCount, "reuse_count": res.ReuseCount, "err": errStr(err),
			"reused_attempt_id": att, "reused_terminal": term, "reused_evidence": ev,
		})
		return
	}

	if phase == "p2" && mode == "recover_g1" {
		repo := initGitRepo(t)
		svc := workflowrun.Service{Now: t0, HomeDir: home, Executor: prod(hangSpawnRunner{Mode: "product"})}
		req := spawnBaseReq(t, project, runID)
		req.RepoPath = repo
		req.Integrator = &countingEnsureIntegrator{}
		// CRITICAL: AttemptGeneration must be nil — auto from durable recovery.
		if req.AttemptGeneration != nil {
			t.Fatal("p2 AttemptGeneration must be nil")
		}
		res, err := svc.Execute(context.Background(), req)
		raw, _ := os.ReadFile(res.EventLogPath)
		g0L, g1L, nPID, g0I, g0T := 0, 0, 0, 0, 0
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev workflowrun.Event
			if json.Unmarshal([]byte(line), &ev) != nil || ev.WorkItemID != "only" {
				continue
			}
			switch ev.Kind {
			case "launch":
				if strings.HasSuffix(ev.AttemptID, "-g0") {
					g0L++
				}
				if strings.HasSuffix(ev.AttemptID, "-g1") {
					g1L++
				}
			case "pid":
				nPID++
			case "interrupt":
				if strings.HasSuffix(ev.AttemptID, "-g0") {
					g0I++
				}
			case "terminal":
				if strings.HasSuffix(ev.AttemptID, "-g0") {
					g0T++
				}
			}
		}
		writeHR(os.Getenv("LOOPCODER_HR_RESULT"), map[string]any{
			"status": res.Status, "err": errStr(err),
			"attempt_generation_nil": true,
			"g0_launch":              g0L, "g1_launch": g1L,
			"g0_interrupt": g0I, "g0_terminal": g0T, "pid_count": nPID,
			"process_peak": res.ProcessPeak, "worktree_peak": res.WorktreePeak,
			"launch_count": res.LaunchCount,
		})
		return
	}

	if phase == "p3" && mode == "reuse" {
		repo := initGitRepo(t)
		svc := workflowrun.Service{Now: t0, HomeDir: home, Executor: prod(hangSpawnRunner{Mode: "product"})}
		req := spawnBaseReq(t, project, runID)
		req.RepoPath = repo
		req.Integrator = &countingEnsureIntegrator{}
		if req.AttemptGeneration != nil {
			t.Fatal("p3 AttemptGeneration must be nil")
		}
		res, err := svc.Execute(context.Background(), req)
		reusedAtt, reusedTerm, reusedEv, g1Ev := "", "", "", ""
		nReuse := 0
		for _, c := range res.Children {
			if c.WorkItemID != "only" {
				continue
			}
			if strings.HasSuffix(c.AttemptID, "-g1") && c.Terminal == "succeeded" {
				reusedAtt = c.AttemptID
				reusedTerm = c.Terminal
				reusedEv = c.OutputEvidence
				nReuse++
			}
		}
		// Also scan log for g1 terminal evidence
		if res.EventLogPath != "" {
			raw, _ := os.ReadFile(res.EventLogPath)
			for _, line := range strings.Split(string(raw), "\n") {
				var ev workflowrun.Event
				if json.Unmarshal([]byte(line), &ev) != nil {
					continue
				}
				if ev.Kind == "terminal" && strings.HasSuffix(ev.AttemptID, "-g1") && ev.Terminal == "succeeded" {
					g1Ev = ev.Evidence
				}
			}
		}
		writeHR(os.Getenv("LOOPCODER_HR_RESULT"), map[string]any{
			"status": res.Status, "err": errStr(err),
			"launch_count": res.LaunchCount, "reuse_count": res.ReuseCount,
			"reused_attempt_id": reusedAtt, "reused_terminal": reusedTerm,
			"reused_evidence": reusedEv, "g1_terminal_evidence": g1Ev,
			"exact_g1_children": nReuse,
		})
		return
	}
	t.Fatalf("unknown phase/mode %s/%s", phase, mode)
}

func writeHR(path string, m map[string]any) {
	if path == "" {
		return
	}
	raw, _ := json.Marshal(m)
	_ = os.WriteFile(path, raw, 0o600)
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func fmtInt(n int) string { return strconv.Itoa(n) }
