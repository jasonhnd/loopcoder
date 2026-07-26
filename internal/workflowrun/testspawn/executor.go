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
	HomeDir      string
	Now          func() time.Time
	Hang         bool
	HangIDs      map[string]bool // hang only matching work items (after durable PID)
	OnHangEntry  func(workItemID string, pid int)
	ProductFiles map[string][]string
	FailModel    string
	FailIDs      map[string]bool
	// FailModelUnavailableOnceIDs fails the first real spawn of matching work
	// items with typed model_unavailable (after durable PID). Subsequent
	// launches of the same work item succeed so generation-safe alternate
	// retries can complete. FailModelUnavailableCounts must be a shared map
	// (caller-owned) so counts survive Executor value copies.
	FailModelUnavailableOnceIDs map[string]bool
	FailModelUnavailableCounts  map[string]int
	CancelAfterIDs              map[string]bool // after durable PID, return forced_interrupt without hang
	// MutateInterruptedRoute alters only the route echoed after a Service-owned
	// context cancellation. It reproduces production executors that can report
	// a partial invocation identity when interrupted before final stamping.
	MutateInterruptedRoute func(workflowrun.ChildRoute) workflowrun.ChildRoute
	Calls                  map[string]int
	InvocationCountPath    string
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

	// FailModel / FailIDs / one-shot MU must win before Hang so alternate-reroute
	// tests can fail primary with model_unavailable then hang only on a later attempt.
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
	if e.FailModelUnavailableOnceIDs != nil && e.FailModelUnavailableOnceIDs[in.WorkItemID] {
		if e.FailModelUnavailableCounts == nil {
			_ = process.KillGroup(pid)
			_, _ = cmd.Process.Wait()
			return failRes(wt, in.Route, "testspawn_misconfig", "FailModelUnavailableCounts map required", pid),
				fmt.Errorf("testspawn: FailModelUnavailableCounts required for once-IDs")
		}
		n := e.FailModelUnavailableCounts[in.WorkItemID]
		e.FailModelUnavailableCounts[in.WorkItemID] = n + 1
		if n == 0 {
			_ = process.KillGroup(pid)
			_, _ = cmd.Process.Wait()
			// Typed MU: remove every product path Fake wrote into this worktree
			// (do not hide physical writes by clearing metadata only). Alternate
			// alone may write/integrate once.
			if err := ScrubFailedWorktreeProduct(wt, base.FilesTouched); err != nil {
				return failRes(wt, in.Route, "mu_scrub_failed", err.Error(), pid), err
			}
			return stamp(workflowrun.ChildExecResult{
				Terminal: workgraph.TermFailed,
				OutputEvidence: "failed:model_unavailable:" + in.WorkItemID + ":" +
					strings.TrimSpace(in.Route.Provider) + "/" + strings.TrimSpace(in.Route.Model),
				WorktreePath: wt, ProcessPID: pid, ExitCode: 1, FailureClass: "model_unavailable",
				Message: "invalid model selection " + strings.TrimSpace(in.Route.Model) +
					" (model_unavailable) work_item=" + in.WorkItemID,
				FilesTouched: nil, SpawnObserved: true, ActualSource: "unknown",
			}, in.Route), nil
		}
		// Subsequent launches (generation-safe alternate) fall through to success.
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
		out := stamp(workflowrun.ChildExecResult{
			Terminal: workgraph.TermCancelled, OutputEvidence: base.OutputEvidence, WorktreePath: wt,
			ProcessPID: pid, ExitCode: 130, FailureClass: "forced_interrupt",
			Message:      "forced interrupt while running " + in.WorkItemID,
			FilesTouched: base.FilesTouched, SpawnObserved: true, ActualSource: "unknown",
		}, in.Route)
		if e.MutateInterruptedRoute != nil {
			out.InvokedRoute = e.MutateInterruptedRoute(out.InvokedRoute)
		}
		return out, ctx.Err()
	}

	_ = cmd.Wait()

	evid := base.OutputEvidence
	if evid == "" {
		h := sha256.Sum256([]byte(in.WorkItemID + wt))
		evid = "sha256:" + hex.EncodeToString(h[:])
	}
	// Real disposable process spawn → durable accepted-invocation proof for
	// canary useful-child measurement (ArgvDigest + route sources). Capacity
	// ActualSource remains unknown (no provider usage stream).
	argvDig := sha256.Sum256([]byte("/bin/sleep\x00" + strings.Join(args, "\x00") + "\x00" + in.WorkItemID + "\x00" + in.AttemptID))
	out := workflowrun.ChildExecResult{
		Terminal: workgraph.TermSucceeded, OutputEvidence: evid, WorktreePath: wt,
		ProcessPID: pid, ExitCode: 0, FilesTouched: base.FilesTouched,
		SpawnObserved: true, ActualSource: "unknown", Message: "testspawn_ok",
		ArgvDigest: "sha256:" + hex.EncodeToString(argvDig[:]),
	}
	out.ActualSources.Model = "accepted_invocation"
	out.ActualSources.Effort = "accepted_invocation"
	out.ActualSources.Permission = "accepted_invocation"
	return stamp(out, in.Route), nil
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

// ScrubFailedWorktreeProduct is the exported MU failed-worktree cleanup used by
// the one-shot MU branch and by focused scrub-boundary tests.
func ScrubFailedWorktreeProduct(wt string, rels []string) error {
	return scrubFailedWorktreeProduct(wt, rels)
}

// scrubFailedWorktreeProduct removes every relative product path written into
// the failed MU worktree (disk + git index), then asserts full worktree git
// porcelain is clean. Fails closed on invalid/empty/dot/traversal paths and on
// non-git worktrees (never tolerate "not a git repository").
func scrubFailedWorktreeProduct(wt string, rels []string) error {
	wt = strings.TrimSpace(wt)
	if wt == "" {
		return fmt.Errorf("testspawn: empty worktree for MU scrub")
	}
	// Must be a real git worktree — refuse silent fallback.
	if out, err := exec.Command("git", "-C", wt, "rev-parse", "--is-inside-work-tree").CombinedOutput(); err != nil {
		return fmt.Errorf("testspawn: MU scrub requires git worktree: %v %s", err, out)
	}
	if len(rels) == 0 {
		// Even with empty list, full porcelain must be clean of product residuals.
		return assertFullGitWorktreeClean(wt)
	}
	wtClean := filepath.Clean(wt)
	for _, raw := range rels {
		rawTrim := strings.TrimSpace(raw)
		if rawTrim == "" {
			return fmt.Errorf("testspawn: MU scrub invalid empty FilesTouched path")
		}
		rel := filepath.Clean(rawTrim)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("testspawn: MU scrub invalid/dot/traversal path %q", raw)
		}
		if filepath.IsAbs(rel) {
			return fmt.Errorf("testspawn: MU scrub absolute path forbidden: %q", raw)
		}
		abs := filepath.Join(wt, rel)
		// Refuse path escape outside worktree.
		if !strings.HasPrefix(abs, wtClean+string(filepath.Separator)) && abs != wtClean {
			return fmt.Errorf("testspawn: MU scrub path escapes worktree: %s", rel)
		}
		if err := os.RemoveAll(abs); err != nil {
			return fmt.Errorf("testspawn: MU scrub remove %s: %w", rel, err)
		}
		if _, err := os.Stat(abs); err == nil {
			return fmt.Errorf("testspawn: MU scrub path still present after remove: %s", rel)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("testspawn: MU scrub stat %s: %w", rel, err)
		}
		// Clear staged index entries (Fake stages with git add → AD after delete).
		rm := exec.Command("git", "-C", wt, "rm", "-f", "--cached", "--ignore-unmatch", "--", rel)
		if out, err := rm.CombinedOutput(); err != nil {
			return fmt.Errorf("testspawn: MU scrub git rm --cached %s: %v %s", rel, err, out)
		}
	}
	// FakeChildExecutor also writes non-product audit stubs (.loopcoder/, ownership
	// markers, provider logs). Remove them so full porcelain is clean — failed MU
	// must leave no product/staged/untracked residuals for the alternate to own.
	for _, meta := range []string{
		".loopcoder",
		".loopcoder-owned-worktree",
		"loopcoder-child-provider.log",
	} {
		abs := filepath.Join(wt, meta)
		if err := os.RemoveAll(abs); err != nil {
			return fmt.Errorf("testspawn: MU scrub remove meta %s: %w", meta, err)
		}
		rm := exec.Command("git", "-C", wt, "rm", "-rf", "--cached", "--ignore-unmatch", "--", meta)
		if out, err := rm.CombinedOutput(); err != nil {
			return fmt.Errorf("testspawn: MU scrub git rm meta %s: %v %s", meta, err, out)
		}
	}
	return assertFullGitWorktreeClean(wt)
}

// assertFullGitWorktreeClean requires the entire failed worktree porcelain to be
// empty (no staged/unstaged/untracked product residuals).
func assertFullGitWorktreeClean(wt string) error {
	cmd := exec.Command("git", "-C", wt, "status", "--porcelain", "--untracked-files=all")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("testspawn: MU scrub full git status: %v %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("testspawn: MU scrub full worktree not clean after product removal:\n%s", out)
	}
	return nil
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
