//go:build !windows && !linux

package supervisedexec

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

func observeUnixProcessGroupActivity(pgid int) processActivityObservation {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=", "-g", strconv.Itoa(pgid)).Output()
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
