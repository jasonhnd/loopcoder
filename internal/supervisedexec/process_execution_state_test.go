package supervisedexec

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type processExecutionStatus string

const (
	processExecutionUnknown processExecutionStatus = "unknown"
	processExecutionStopped processExecutionStatus = "stopped"
	processExecutionRunning processExecutionStatus = "running"
	processExecutionZombie  processExecutionStatus = "zombie"
)

type processExecutionObservation struct {
	status processExecutionStatus
	detail string
}

func (o processExecutionObservation) executionStopped() bool {
	return o.status == processExecutionStopped || o.status == processExecutionZombie
}

func (o processExecutionObservation) String() string {
	if strings.TrimSpace(o.detail) == "" {
		return string(o.status)
	}
	return fmt.Sprintf("%s (%s)", o.status, o.detail)
}

func waitNotExecuting(t *testing.T, pid int, timeout time.Duration, label, diagnosticPath string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := processExecutionObservation{status: processExecutionUnknown, detail: "not observed"}
	for time.Now().Before(deadline) {
		last = observeProcessExecution(pid)
		if last.executionStopped() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s pid %d still executing after %s; last state: %s%s", label, pid, timeout, last, guardianDiagnosticSuffix(diagnosticPath))
}

func guardianDiagnosticSuffix(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("\nguardian diagnostics unavailable: %v", err)
	}
	return "\nguardian diagnostics:\n" + string(data)
}
