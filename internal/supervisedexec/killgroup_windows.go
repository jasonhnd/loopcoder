//go:build windows

package supervisedexec

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"
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
		uintptr(unsafe.Pointer(&info)), // #nosec G103 -- documented Windows syscall interop: JOBOBJECT_EXTENDED_LIMIT_INFORMATION is passed only for this immediate call.
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
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_SET_QUOTA, false, uint32(cmd.Process.Pid)) // #nosec G115 -- Windows OpenProcess takes a DWORD PID; cmd.Process.Pid comes from operator-trusted process control.
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.AssignProcessToJobObject(g.job, h)
}

func (g *windowsKillGroup) activity() processActivityObservation {
	g.mu.Lock()
	job := g.job
	closed := g.closed
	g.mu.Unlock()
	if job == 0 || closed {
		return processActivityObservation{}
	}

	const maxProcessIDs = 256
	headerSize := 8
	entrySize := int(unsafe.Sizeof(uintptr(0)))
	buf := make([]byte, headerSize+(maxProcessIDs*entrySize))
	var returned uint32
	if err := windows.QueryInformationJobObject(
		job,
		windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])), // #nosec G103 -- documented Windows syscall interop for a local byte buffer.
		uint32(len(buf)),
		&returned,
	); err != nil {
		return processActivityObservation{}
	}
	count := *(*uint32)(unsafe.Pointer(&buf[4])) // #nosec G103 -- fixed JOBOBJECT_BASIC_PROCESS_ID_LIST layout.
	if count == 0 {
		return processActivityObservation{}
	}
	if count > maxProcessIDs {
		count = maxProcessIDs
	}
	raw := (*[maxProcessIDs]uintptr)(unsafe.Pointer(&buf[headerSize]))[:count:count] // #nosec G103 -- fixed JOBOBJECT_BASIC_PROCESS_ID_LIST layout.
	pids := make([]int, 0, len(raw))
	for _, pid := range raw {
		if pid != 0 {
			pids = append(pids, int(pid))
		}
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
