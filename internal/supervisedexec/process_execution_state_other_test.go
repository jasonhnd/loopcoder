//go:build !darwin

package supervisedexec

import "github.com/jasonhnd/loopcoder/internal/process"

func observeProcessExecution(pid int) processExecutionObservation {
	if !process.Alive(pid) {
		return processExecutionObservation{status: processExecutionStopped, detail: "kill0=false"}
	}
	return processExecutionObservation{status: processExecutionRunning, detail: "kill0=true"}
}
