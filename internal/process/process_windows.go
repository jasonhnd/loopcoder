//go:build windows

package process

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	output, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH").Output()
	if err != nil {
		return false
	}
	text := strings.ToLower(string(output))
	if strings.Contains(text, "no tasks") || strings.Contains(text, "not found") {
		return false
	}
	return strings.Contains(text, strconv.Itoa(pid))
}
