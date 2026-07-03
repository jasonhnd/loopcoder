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

// KillTree terminates pid and its whole descendant tree via `taskkill /T`, which
// reaps children whose parent-chain has already broken (a plain kill does not).
func KillTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	name, args := taskkillCommand(pid)
	cmd := exec.CommandContext(context.Background(), name, args...)
	_, err := supervisedexec.Run(context.Background(), cmd, supervisedexec.Options{HardCap: livenessHardCap})
	return err
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
