//go:build !windows

package supervisedexec

import (
	"os/exec"
	"syscall"
)

// unixKillGroup makes each child the leader of its own process group, so the
// whole subtree can be terminated with a single group-directed signal.
type unixKillGroup struct {
	pgid int
}

func newKillGroup(runID string) killGroup {
	_ = runID
	return &unixKillGroup{}
}

func (g *unixKillGroup) prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Child leads a new process group (pgid == child pid).
	cmd.SysProcAttr.Setpgid = true
	// On Linux, also die if loopcoder dies (crash-safe orphan reaping).
	setPdeathsig(cmd.SysProcAttr)
}

func (g *unixKillGroup) adopt(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		g.pgid = cmd.Process.Pid // group leader pid == pgid
	}
	return nil
}

func (g *unixKillGroup) activity() processActivityObservation {
	if g.pgid <= 0 {
		return processActivityObservation{}
	}
	return observeUnixProcessGroupActivity(g.pgid)
}

func (g *unixKillGroup) kill() error {
	if g.pgid <= 0 {
		return nil
	}
	// Negative pid signals the whole process group.
	return syscall.Kill(-g.pgid, syscall.SIGKILL)
}

func (g *unixKillGroup) close() {}
