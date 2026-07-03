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
