//go:build !unix

package cli

import "os/exec"

func detachProcessGroup(cmd *exec.Cmd) {}
