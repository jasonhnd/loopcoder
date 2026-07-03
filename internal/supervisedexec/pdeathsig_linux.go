//go:build linux

package supervisedexec

import "syscall"

// setPdeathsig asks the kernel to SIGKILL the child if loopcoder (its parent)
// dies, so a hard crash cannot leak an orphaned provider CLI on Linux.
func setPdeathsig(a *syscall.SysProcAttr) {
	a.Pdeathsig = syscall.SIGKILL
}
