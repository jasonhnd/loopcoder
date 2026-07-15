//go:build darwin

package supervisedexec

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
)

func observeProcessExecution(pid int) processExecutionObservation {
	if pid <= 0 {
		return processExecutionObservation{status: processExecutionStopped, detail: "non-positive pid"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "pid=", "-o", "ppid=", "-o", "state=", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		if !process.Alive(pid) {
			return processExecutionObservation{status: processExecutionStopped, detail: "kill0=false; ps=" + err.Error()}
		}
		return processExecutionObservation{status: processExecutionUnknown, detail: "kill0=true; ps=" + err.Error()}
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		activity, ok := parsePSProcessActivity(line)
		if !ok || activity.pid != pid {
			continue
		}
		if isZombiePSState(activity.state) {
			return processExecutionObservation{status: processExecutionZombie, detail: activity.signature()}
		}
		return processExecutionObservation{status: processExecutionRunning, detail: activity.signature()}
	}
	if !process.Alive(pid) {
		return processExecutionObservation{status: processExecutionStopped, detail: "not listed by ps; kill0=false"}
	}
	return processExecutionObservation{status: processExecutionUnknown, detail: "not listed by ps; kill0=true"}
}

func isZombiePSState(state string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(state)), "Z")
}

func TestDarwinProcessExecutionStateTreatsZombieAsStopped(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start exiting child: %v", err)
	}
	defer cmd.Wait()

	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	var last processExecutionObservation
	for time.Now().Before(deadline) {
		last = observeProcessExecution(pid)
		if last.status == processExecutionZombie {
			if !process.Alive(pid) {
				t.Fatalf("zombie pid %d should still satisfy kill(pid, 0)", pid)
			}
			if !last.executionStopped() {
				t.Fatalf("zombie observation = %s, want execution stopped", last)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child pid %d did not reach zombie state; last state: %s", pid, last)
}
