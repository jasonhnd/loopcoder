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

func (o processExecutionObservation) executionReaped() bool {
	return o.status == processExecutionStopped
}

func (o processExecutionObservation) String() string {
	if strings.TrimSpace(o.detail) == "" {
		return string(o.status)
	}
	return fmt.Sprintf("%s (%s)", o.status, o.detail)
}

type processReapTarget struct {
	pid            int
	label          string
	diagnosticPath string
	last           processExecutionObservation
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

func drainReapedProcesses(t *testing.T, targets []processReapTarget, timeout time.Duration) []processReapTarget {
	t.Helper()
	if len(targets) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	pending := append([]processReapTarget(nil), targets...)
	for {
		next := pending[:0]
		for _, target := range pending {
			target.last = observeProcessExecution(target.pid)
			if target.last.executionReaped() {
				continue
			}
			next = append(next, target)
		}
		pending = next
		if len(pending) == 0 {
			return nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			return append([]processReapTarget(nil), pending...)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func assertProcessesReaped(t *testing.T, targets []processReapTarget, timeout time.Duration) {
	t.Helper()
	pending := drainReapedProcesses(t, targets, timeout)
	if len(pending) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "processes not reaped after %s:", timeout)
	seenDiagnostics := make(map[string]bool)
	for _, target := range pending {
		fmt.Fprintf(&b, "\n%s pid %d last state: %s", target.label, target.pid, target.last)
	}
	for _, target := range pending {
		if seenDiagnostics[target.diagnosticPath] {
			continue
		}
		seenDiagnostics[target.diagnosticPath] = true
		b.WriteString(guardianDiagnosticSuffix(target.diagnosticPath))
	}
	t.Fatal(b.String())
}

func TestProcessExecutionReapedRejectsZombie(t *testing.T) {
	observation := processExecutionObservation{status: processExecutionZombie, detail: "fixture"}
	if !observation.executionStopped() {
		t.Fatal("zombie observation should count as not executing")
	}
	if observation.executionReaped() {
		t.Fatal("zombie observation should not count as reaped")
	}
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
