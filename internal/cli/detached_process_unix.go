//go:build unix

package cli

import (
	"os/exec"
	"syscall"
)

func detachProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
