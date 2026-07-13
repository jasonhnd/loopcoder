//go:build !windows

package supervisedexec

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=", "-g", strconv.Itoa(g.pgid)).Output()
	if err != nil {
		return processActivityObservation{}
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return processActivityObservation{}
	}
	pids := make([]int, 0, len(fields))
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	if len(pids) == 0 {
		return processActivityObservation{}
	}
	sort.Ints(pids)
	parts := make([]string, 0, len(pids))
	for _, pid := range pids {
		parts = append(parts, fmt.Sprintf("%d", pid))
	}
	return processActivityObservation{available: true, signature: strings.Join(parts, ",")}
}

func (g *unixKillGroup) kill() error {
	if g.pgid <= 0 {
		return nil
	}
	// Negative pid signals the whole process group.
	return syscall.Kill(-g.pgid, syscall.SIGKILL)
}

func (g *unixKillGroup) close() {}
