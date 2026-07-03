//go:build !linux && !windows

package supervisedexec

import "syscall"

// setPdeathsig is a no-op on non-Linux Unix (e.g. macOS/BSD have no parent-death
// signal); graceful shutdown still kills the process group, and hard-crash
// orphans fall back to passive resume.
func setPdeathsig(a *syscall.SysProcAttr) {}
