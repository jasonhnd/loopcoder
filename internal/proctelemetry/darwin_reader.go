//go:build darwin

package proctelemetry

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// DarwinReader reads CPU time and RSS for a bounded PID list via `ps`
// (no full process table scan, no argv).
type DarwinReader struct{}

// Read implements ResourceReader.
func (DarwinReader) Read(pids []int) (map[int]ProcResources, error) {
	out := make(map[int]ProcResources, len(pids))
	if len(pids) == 0 {
		return out, nil
	}
	const batch = 32
	for i := 0; i < len(pids); i += batch {
		end := i + batch
		if end > len(pids) {
			end = len(pids)
		}
		if err := readBatch(pids[i:end], out); err != nil {
			return out, err
		}
	}
	return out, nil
}

func readBatch(pids []int, out map[int]ProcResources) error {
	plist := make([]string, len(pids))
	for i, p := range pids {
		plist[i] = strconv.Itoa(p)
	}
	cmd := exec.Command("ps", "-o", "pid=,rss=,time=", "-p", strings.Join(plist, ","))
	body, err := cmd.Output()
	if err != nil && len(body) == 0 {
		return fmt.Errorf("proctelemetry: ps: %w", err)
	}
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		rssKB, err2 := strconv.ParseInt(fields[1], 10, 64)
		cpuSecs, err3 := parsePSTime(fields[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		out[pid] = ProcResources{
			PID: pid, RSSBytes: rssKB * 1024, CPUTimeSecs: cpuSecs, OK: true,
		}
	}
	for _, p := range pids {
		if _, ok := out[p]; !ok {
			out[p] = ProcResources{PID: p, OK: false}
		}
	}
	return nil
}
