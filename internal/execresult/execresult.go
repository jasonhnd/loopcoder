// Package execresult builds a comparable exit error from a finished command,
// preserving the process exit code even when the standard *exec.ExitError is
// unavailable. It is shared by the packages that run external tools (git, gh,
// scaffold) so the identical helper is defined once.
package execresult

import (
	"fmt"
	"os/exec"
)

// ExitStatusError carries a process exit code for the rare path where a
// finished command has no *os.ProcessState to build an *exec.ExitError from.
type ExitStatusError struct {
	Code int
}

func (e ExitStatusError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

func (e ExitStatusError) ExitCode() int { return e.Code }

// CommandExitError returns the exit error for a completed command. It prefers
// the standard *exec.ExitError, which is available whenever cmd.Wait has run
// (the normal case); the ExitStatusError branch is a defensive fallback for the
// rare case where cmd.ProcessState is nil.
func CommandExitError(cmd *exec.Cmd, code int) error {
	if cmd.ProcessState != nil {
		return &exec.ExitError{ProcessState: cmd.ProcessState}
	}
	return ExitStatusError{Code: code}
}
