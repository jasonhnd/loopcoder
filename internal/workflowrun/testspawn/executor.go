// Package testspawn provides test-only child executors that spawn real isolated
// process groups. Never imported by production LoopCoder runtime code.
package testspawn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// Executor spawns an isolated short-lived process group, reports a real
// process.Snapshot through OnProcessStart, and drains/kills the child.
// Never uses the supervisor/test-runner PID.
//
// Use this for any recovery/restart test that enters the production durable
// lifecycle (authority + PID). Do not forge authority or use os.Getpid.
type Executor struct {
	HomeDir             string
	Now                 func() time.Time
	Hang                bool
	HangIDs             map[string]bool // hang only matching work items (after durable PID)
	OnHangEntry         func(workItemID string, pid int)
	ProductFiles        map[string][]string
	FailModel           string
	FailIDs             map[string]bool
	CancelAfterIDs      map[string]bool // after durable PID, return forced_interrupt without hang
	Calls               map[string]int
	InvocationCountPath string
}

// Execute implements workflowrun.ChildExecutor.
func (e Executor) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	if e.Calls != nil {
		e.Calls[in.WorkItemID]++
	}
	if p := strings.TrimSpace(e.InvocationCountPath); p != "" {
		_ = appendInvocationCount(p, in.WorkItemID)
	}

	// Reuse Fake solely for worktree allocation + product/evidence shape (no spawn).
	fake := workflowrun.FakeChildExecutor{
		HomeDir: e.HomeDir, Now: e.Now, ProductFiles: e.ProductFiles,
	}
	base, ferr := fake.Execute(ctx, in)
	if ferr != nil && base.WorktreePath == "" {
		return base, ferr
	}
	wt := base.WorktreePath
	if wt == "" {
		return workflowrun.ChildExecResult{Terminal: workgraph.TermFailed, FailureClass: "worktree"}, fmt.Errorf("testspawn: empty worktree")
	}

	hangThis := e.Hang || (e.HangIDs != nil && e.HangIDs[in.WorkItemID])
	args := []string{"0.12"}
	if hangThis {
		args = []string{"120"}
	}
	cmd := exec.Command("/bin/sleep", args...)
	cmd.Dir = wt
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return failRes(wt, in.Route, "spawn", err.Error(), 0), err
	}
	pid := cmd.Process.Pid
	snap, serr := process.Snapshot(pid, time.Now())
	if serr != nil || snap.Ambiguous {
		_ = process.KillGroup(pid)
		_, _ = cmd.Process.Wait()
		return failRes(wt, in.Route, "spawn_identity", fmt.Sprintf("%v ambig=%v", serr, snap.Ambiguous), pid),
			fmt.Errorf("testspawn snapshot: %v", serr)
	}
	ps := workflowrun.ProcessStart{
		PID: snap.PID, PGID: snap.PGID,
		ProcessBirthIdentity: snap.ProcessBirthIdentity,
		ExecutableIdentity:   snap.ExecutableIdentity,
		ObservedAt:           snap.ObservedAt,
		WorktreePath:         wt,
		LogPath:              filepath.Join(wt, ".loopcoder-child-provider.log"),
	}
	if in.OnProcessStart != nil {
		if err := in.OnProcessStart(ps); err != nil {
			_ = process.KillGroup(pid)
			_, _ = cmd.Process.Wait()
			out := failRes(wt, in.Route, "pid_event_failed", err.Error(), pid)
			out.SpawnObserved = true
			return out, err
		}
	}

	// FailModel / FailIDs must win before Hang so alternate-reroute tests can
	// fail primary with model_unavailable then hang only on a later attempt.
	if e.FailIDs != nil && e.FailIDs[in.WorkItemID] {
		_ = process.KillGroup(pid)
		_, _ = cmd.Process.Wait()
		return stamp(workflowrun.ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: base.OutputEvidence, WorktreePath: wt,
			ProcessPID: pid, ExitCode: 1, FailureClass: "injected_fail",
			Message: "testspawn fail " + in.WorkItemID, FilesTouched: base.FilesTouched,
			SpawnObserved: true, ActualSource: "unknown",
		}, in.Route), nil
	}
	if strings.TrimSpace(e.FailModel) != "" &&
		strings.EqualFold(strings.TrimSpace(in.Route.Model), strings.TrimSpace(e.FailModel)) {
		_ = process.KillGroup(pid)
		_, _ = cmd.Process.Wait()
		return stamp(workflowrun.ChildExecResult{
			Terminal: workgraph.TermFailed, OutputEvidence: base.OutputEvidence, WorktreePath: wt,
			ProcessPID: pid, ExitCode: 1, FailureClass: "model_unavailable",
			Message: "invalid model selection " + in.Route.Model, FilesTouched: base.FilesTouched,
			SpawnObserved: true, ActualSource: "unknown",
		}, in.Route), nil
	}
	if e.CancelAfterIDs != nil && e.CancelAfterIDs[in.WorkItemID] {
		// Executor-local cancel (not Service forced_interrupt pair). Service owns
		// service_forced_interrupt identity only when it cancels mid-flight via context.
		_ = process.KillGroup(pid)
		_, _ = cmd.Process.Wait()
		return stamp(workflowrun.ChildExecResult{
			Terminal: workgraph.TermCancelled, OutputEvidence: base.OutputEvidence, WorktreePath: wt,
			ProcessPID: pid, ExitCode: 130, FailureClass: workflowrun.FailureClassExecutorCancelled,
			Message: "testspawn cancel-after " + in.WorkItemID, FilesTouched: base.FilesTouched,
			SpawnObserved: true, ActualSource: "unknown",
		}, in.Route), nil
	}

	if hangThis {
		if e.OnHangEntry != nil {
			e.OnHangEntry(in.WorkItemID, pid)
		}
		<-ctx.Done()
		_ = process.KillGroup(pid)
		_, _ = cmd.Process.Wait()
		return stamp(workflowrun.ChildExecResult{
			Terminal: workgraph.TermCancelled, OutputEvidence: base.OutputEvidence, WorktreePath: wt,
			ProcessPID: pid, ExitCode: 130, FailureClass: "forced_interrupt",
			Message:      "forced interrupt while running " + in.WorkItemID,
			FilesTouched: base.FilesTouched, SpawnObserved: true, ActualSource: "unknown",
		}, in.Route), ctx.Err()
	}

	_ = cmd.Wait()

	evid := base.OutputEvidence
	if evid == "" {
		h := sha256.Sum256([]byte(in.WorkItemID + wt))
		evid = "sha256:" + hex.EncodeToString(h[:])
	}
	return stamp(workflowrun.ChildExecResult{
		Terminal: workgraph.TermSucceeded, OutputEvidence: evid, WorktreePath: wt,
		ProcessPID: pid, ExitCode: 0, FilesTouched: base.FilesTouched,
		SpawnObserved: true, ActualSource: "unknown", Message: "testspawn_ok",
	}, in.Route), nil
}

func stamp(out workflowrun.ChildExecResult, r workflowrun.ChildRoute) workflowrun.ChildExecResult {
	out.InvokedRoute = r
	out.Provider = first(out.Provider, r.Provider)
	out.Model = first(out.Model, r.Model)
	out.Depth = first(out.Depth, r.Depth)
	return out
}

func failRes(wt string, r workflowrun.ChildRoute, class, msg string, pid int) workflowrun.ChildExecResult {
	return stamp(workflowrun.ChildExecResult{
		Terminal: workgraph.TermFailed, FailureClass: class, Message: msg,
		WorktreePath: wt, ProcessPID: pid, ActualSource: "unknown",
	}, r)
}

func first(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func appendInvocationCount(path, workItemID string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n", workItemID)
	return err
}
