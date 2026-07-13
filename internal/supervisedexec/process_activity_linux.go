//go:build linux

package supervisedexec

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func observeUnixProcessGroupActivity(pgid int) processActivityObservation {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return processActivityObservation{}
	}

	processes := make([]linuxProcStat, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := readLinuxProcStat(pid)
		if err != nil || stat.pgrp != pgid {
			continue
		}
		processes = append(processes, stat)
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

type linuxProcStat struct {
	pid       int
	state     string
	ppid      int
	pgrp      int
	utime     uint64
	stime     uint64
	cutime    int64
	cstime    int64
	starttime uint64
}

func readLinuxProcStat(pid int) (linuxProcStat, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return linuxProcStat{}, err
	}
	return parseLinuxProcStat(string(data))
}

func parseLinuxProcStat(line string) (linuxProcStat, error) {
	line = strings.TrimSpace(line)
	open := strings.IndexByte(line, '(')
	close := strings.LastIndexByte(line, ')')
	if open < 0 || close < open {
		return linuxProcStat{}, fmt.Errorf("invalid proc stat: %q", line)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil {
		return linuxProcStat{}, err
	}
	fields := strings.Fields(strings.TrimSpace(line[close+1:]))
	// Fields after comm start at stat field 3: state, ppid, pgrp, session, ...
	if len(fields) < 20 {
		return linuxProcStat{}, fmt.Errorf("short proc stat for pid %d", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return linuxProcStat{}, err
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return linuxProcStat{}, err
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return linuxProcStat{}, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return linuxProcStat{}, err
	}
	cutime, err := strconv.ParseInt(fields[13], 10, 64)
	if err != nil {
		return linuxProcStat{}, err
	}
	cstime, err := strconv.ParseInt(fields[14], 10, 64)
	if err != nil {
		return linuxProcStat{}, err
	}
	starttime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return linuxProcStat{}, err
	}
	return linuxProcStat{
		pid:       pid,
		state:     fields[0],
		ppid:      ppid,
		pgrp:      pgrp,
		utime:     utime,
		stime:     stime,
		cutime:    cutime,
		cstime:    cstime,
		starttime: starttime,
	}, nil
}

func (s linuxProcStat) signature() string {
	return fmt.Sprintf("%d:%s:%d:%d:%d:%d:%d:%d", s.pid, s.state, s.ppid, s.utime, s.stime, s.cutime, s.cstime, s.starttime)
}
