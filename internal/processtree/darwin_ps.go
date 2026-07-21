//go:build darwin

package processtree

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// DarwinPS lists processes via `ps` (no full argv — comm only).
type DarwinPS struct{}

// List implements Observer.
func (DarwinPS) List() ([]RawProc, error) {
	// pid, ppid, pgid, state, lstart, comm — no args (avoids secrets).
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,pgid=,state=,lstart=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("processtree: ps: %w", err)
	}
	return parsePS(out)
}

func parsePS(out []byte) ([]RawProc, error) {
	lines := bytes.Split(out, []byte{'\n'})
	var procs []RawProc
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// lstart is multi-field (e.g. "Mon Jul 21 12:00:00 2026"); parse carefully.
		// Format after ps -axo: PID PPID PGID STATE LSTART(5 tokens) COMM...
		fields := strings.Fields(string(line))
		if len(fields) < 9 {
			// Minimal: pid ppid pgid state + something
			if len(fields) < 5 {
				continue
			}
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		state := fields[3]
		// lstart: 5 fields when present (weekday mon day time year)
		var lstart, comm string
		if len(fields) >= 9 {
			lstart = strings.Join(fields[4:9], " ")
			comm = strings.Join(fields[9:], " ")
		} else if len(fields) > 4 {
			comm = strings.Join(fields[4:], " ")
		}
		procs = append(procs, RawProc{
			PID: pid, PPID: ppid, PGID: pgid,
			State: state, LStart: lstart, Comm: comm,
		})
	}
	return procs, nil
}
