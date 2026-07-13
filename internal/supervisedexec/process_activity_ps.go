//go:build !windows && !linux

package supervisedexec

import (
	"context"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

func observeUnixProcessGroupActivity(pgid int) processActivityObservation {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=", "-o", "ppid=", "-o", "state=", "-o", "time=", "-g", strconv.Itoa(pgid)).Output()
	if err != nil {
		return processActivityObservation{}
	}
	processes := make([]psProcessActivity, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		process, ok := parsePSProcessActivity(line)
		if ok {
			processes = append(processes, process)
		}
	}
	if len(processes) == 0 {
		return processActivityObservation{}
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].pid < processes[j].pid })
	parts := make([]string, 0, len(processes))
	for _, process := range processes {
		parts = append(parts, process.signature())
	}
	return processActivityObservation{available: true, signature: strings.Join(parts, ",")}
}

type psProcessActivity struct {
	pid   int
	ppid  int
	state string
	cpu   string
}

func parsePSProcessActivity(line string) (psProcessActivity, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 4 {
		return psProcessActivity{}, false
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return psProcessActivity{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return psProcessActivity{}, false
	}
	return psProcessActivity{
		pid:   pid,
		ppid:  ppid,
		state: fields[2],
		cpu:   fields[3],
	}, true
}

func (p psProcessActivity) signature() string {
	return strconv.Itoa(p.pid) + ":" + strconv.Itoa(p.ppid) + ":" + p.state + ":" + p.cpu
}
