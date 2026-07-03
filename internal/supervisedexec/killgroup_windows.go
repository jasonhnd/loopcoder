//go:build windows

package supervisedexec

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsKillGroup assigns each child to a Job Object configured to kill every
// process in the job when the job handle closes (so a crashed loopcoder reaps
// its children) and lets the whole tree be terminated with one call.
type windowsKillGroup struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

func newKillGroup(runID string) killGroup {
	g := &windowsKillGroup{}
	var name *uint16
	if runID != "" {
		if n, err := windows.UTF16PtrFromString("loopcoder/" + runID); err == nil {
			name = n
		}
	}
	job, err := windows.CreateJobObject(nil, name)
	if err != nil {
		return g // job == 0: degrades to per-process kill
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return g
	}
	g.job = job
	return g
}

func (g *windowsKillGroup) prepare(cmd *exec.Cmd) {}

func (g *windowsKillGroup) adopt(cmd *exec.Cmd) error {
	if g.job == 0 || cmd.Process == nil {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_SET_QUOTA, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(g.job, h)
}

func (g *windowsKillGroup) kill() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(g.job, 1)
}

func (g *windowsKillGroup) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.job != 0 && !g.closed {
		windows.CloseHandle(g.job)
		g.closed = true
	}
}
