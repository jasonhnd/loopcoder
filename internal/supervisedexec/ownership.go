package supervisedexec

import (
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// Process ownership (spec 0390, Decision 11). loopcoder must identify and
// terminate only its OWN spawned processes. On a real machine the user's Claude
// and Codex desktop apps, other CLI sessions, and the loopcoder CLI session
// itself all appear as `claude`/`codex`/`loopcoder` processes, so terminating by
// bare process name is forbidden. Every spawned child is instead tagged with an
// environment marker and placed in a per-run kill-group (a Windows Job Object or
// a Unix process group) that terminates the whole subtree at once.

const (
	// EnvManaged marks a process as spawned and owned by loopcoder.
	EnvManaged = "LOOPCODER_MANAGED"
	// EnvRunID carries the delivery run id of the spawning loopcoder.
	EnvRunID = "LOOPCODER_RUN_ID"
	// EnvRole carries the spawn role (worker, verifier, git, gh, ...).
	EnvRole = "LOOPCODER_ROLE"
)

// ProcInfo describes a live loopcoder-managed process for `loopcoder ps`.
type ProcInfo struct {
	PID     int
	RunID   string
	Role    string
	Started time.Time
}

type managedProc struct {
	info  ProcInfo
	group killGroup
}

// registry tracks the live processes THIS loopcoder instance has spawned. It is
// per-process, in-memory state — never a machine-wide scan.
var registry = struct {
	mu    sync.Mutex
	procs map[int]*managedProc
}{procs: make(map[int]*managedProc)}

// killGroup is a platform handle that terminates a process and its whole
// descendant subtree at once (Windows Job Object / Unix process group), and
// reaps orphans if the parent dies (Job Object kill-on-close / Linux Pdeathsig).
type killGroup interface {
	// prepare mutates cmd (e.g. SysProcAttr) before Start so the child leads its
	// own group.
	prepare(cmd *exec.Cmd)
	// adopt records or assigns the started process into the group.
	adopt(cmd *exec.Cmd) error
	// kill terminates the whole group.
	kill() error
	// close releases the group handle.
	close()
}

// applyEnvMarkers appends the loopcoder-managed markers to the child environment.
func applyEnvMarkers(cmd *exec.Cmd, runID, role string) {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	env = append(env, EnvManaged+"=1")
	if runID != "" {
		env = append(env, EnvRunID+"="+runID)
	}
	if role != "" {
		env = append(env, EnvRole+"="+role)
	}
	cmd.Env = env
}

func registerProc(cmd *exec.Cmd, runID, role string, g killGroup) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	registry.mu.Lock()
	registry.procs[cmd.Process.Pid] = &managedProc{
		info:  ProcInfo{PID: cmd.Process.Pid, RunID: runID, Role: role, Started: time.Now()},
		group: g,
	}
	registry.mu.Unlock()
}

func deregisterProc(pid int) {
	registry.mu.Lock()
	delete(registry.procs, pid)
	registry.mu.Unlock()
}

// Managed returns a snapshot of every live process this instance is supervising,
// oldest first.
func Managed() []ProcInfo {
	registry.mu.Lock()
	out := make([]ProcInfo, 0, len(registry.procs))
	for _, m := range registry.procs {
		out = append(out, m.info)
	}
	registry.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Shutdown terminates every loopcoder-managed process this instance started, by
// kill-group (whole subtree). It never targets a process by bare name. Intended
// for a SIGINT/SIGTERM handler so a Ctrl-C'd loopcoder does not leak a running
// provider CLI.
func Shutdown() int {
	registry.mu.Lock()
	groups := make([]killGroup, 0, len(registry.procs))
	for pid, m := range registry.procs {
		groups = append(groups, m.group)
		delete(registry.procs, pid)
	}
	registry.mu.Unlock()

	for _, g := range groups {
		if g == nil {
			continue
		}
		_ = g.kill()
		g.close()
	}
	return len(groups)
}
