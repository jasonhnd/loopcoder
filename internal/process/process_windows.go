//go:build windows

package process

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

var tasklistCommand = func(pid int) (string, []string) {
	return "tasklist", []string{"/FI", fmt.Sprintf("PID eq %d", pid), "/NH"}
}

var taskkillCommand = func(pid int) (string, []string) {
	return "taskkill", []string{"/F", "/T", "/PID", strconv.Itoa(pid)}
}

// KillTree terminates pid and its descendant tree via `taskkill /F /T`, which
// walks the live parent-PID tree (best effort). Robust reaping of a subtree
// whose parent-chain has already broken comes from the in-process Job Object's
// kill-on-close (see internal/supervisedexec), not this out-of-process walk.
func KillTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	name, args := taskkillCommand(pid)
	cmd := exec.CommandContext(context.Background(), name, args...)
	_, err := supervisedexec.Run(context.Background(), cmd, supervisedexec.Options{HardCap: livenessHardCap})
	return err
}

func KillGroup(pgid int) error {
	return KillTree(pgid)
}

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	name, args := tasklistCommand(pid)
	cmd := exec.CommandContext(context.Background(), name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	result, err := supervisedexec.Run(context.Background(), cmd, supervisedexec.Options{HardCap: livenessHardCap})
	if err != nil || result.Outcome != supervisedexec.OutcomeCompleted || result.ExitCode != 0 {
		return false
	}
	text := strings.ToLower(stdout.String())
	if strings.Contains(text, "no tasks") || strings.Contains(text, "not found") {
		return false
	}
	return strings.Contains(text, strconv.Itoa(pid))
}
