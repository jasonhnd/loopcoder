//go:build darwin && arm64

package providerinventory

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const claudePTYHelperEnv = "LOOPCODER_CLAUDE_PTY_HELPER"

func TestRunClaudeUsagePTYUsesRealTerminalSizeAndInteractiveInput(t *testing.T) {
	req := claudePTYTestRequest(t, "interactive")
	req.Columns = 123
	req.Rows = 37
	result, err := runClaudeUsagePTY(context.Background(), req)
	if err != nil {
		t.Fatalf("runClaudeUsagePTY: %v\nresult=%#v", err, result)
	}
	if result.ExitCode != 0 || result.TimedOut || result.Killed || result.Truncated {
		t.Fatalf("result = %#v, want clean exit", result)
	}
	for _, want := range []string{
		"TTY status=true rows=37 cols=123",
		"INPUT /usage",
		"Claude Code Usage",
		"Current session: 25% used resets at 2026-07-14T12:00:00Z",
	} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("output missing %q:\n%s", want, result.Output)
		}
	}
}

func TestRunClaudeUsagePTYCancellationKillsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	req := claudePTYTestRequest(t, "hang")
	req.Timeout = 5 * time.Second
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	result, err := runClaudeUsagePTY(ctx, req)
	if err == nil {
		t.Fatalf("runClaudeUsagePTY err = nil, want cancellation; result=%#v output=%q", result, result.Output)
	}
	if !result.Killed || result.TimedOut {
		t.Fatalf("result = %#v, want killed by parent cancellation", result)
	}
}

func TestRunClaudeUsagePTYTimeoutKillsOwnedProcessTree(t *testing.T) {
	req := claudePTYTestRequest(t, "spawn-and-hang")
	req.Timeout = 150 * time.Millisecond
	result, err := runClaudeUsagePTY(context.Background(), req)
	if err == nil {
		t.Fatalf("runClaudeUsagePTY err = nil, want timeout; result=%#v output=%q", result, result.Output)
	}
	if !result.TimedOut || !result.Killed {
		t.Fatalf("result = %#v, want timeout and kill", result)
	}
	childPID := childPIDFromOutput(t, result.Output)
	waitForProcessGone(t, childPID)
}

func TestRunClaudeUsagePTYTruncationKillsProcessAndReportsTruncated(t *testing.T) {
	req := claudePTYTestRequest(t, "flood")
	req.StdoutLimitBytes = 1024
	req.CombinedLimitBytes = 1024
	req.Timeout = 5 * time.Second
	result, err := runClaudeUsagePTY(context.Background(), req)
	if err != nil {
		t.Fatalf("runClaudeUsagePTY: %v\nresult=%#v", err, result)
	}
	if !result.Truncated || !result.Killed {
		t.Fatalf("result = %#v, want truncated and killed", result)
	}
	if len(result.Output) > 1024 {
		t.Fatalf("output length = %d, want bounded", len(result.Output))
	}
}

func TestRunClaudeUsagePTYEarlyExitReturnsExitCode(t *testing.T) {
	req := claudePTYTestRequest(t, "early-exit")
	result, err := runClaudeUsagePTY(context.Background(), req)
	if err != nil {
		t.Fatalf("runClaudeUsagePTY: %v\nresult=%#v", err, result)
	}
	if result.ExitCode != 7 || result.TimedOut || result.Killed {
		t.Fatalf("result = %#v, want exit code 7 without supervision kill", result)
	}
	if !strings.Contains(result.Output, "early exit after /usage") {
		t.Fatalf("output = %q, want early-exit marker", result.Output)
	}
}

func TestClaudePTYProductionRunnerHelper(t *testing.T) {
	mode := os.Getenv(claudePTYHelperEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "interactive":
		runClaudePTYInteractiveHelper()
	case "hang":
		fmt.Println("READY hang")
		sleepForever()
	case "spawn-and-hang":
		runClaudePTYSpawnAndHangHelper()
	case "sleep-child":
		sleepForever()
	case "flood":
		runClaudePTYFloodHelper()
	case "early-exit":
		runClaudePTYEarlyExitHelper()
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

func claudePTYTestRequest(t *testing.T, mode string) ClaudePTYRequest {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	return ClaudePTYRequest{
		Argv:               []string{exe, "-test.run=^TestClaudePTYProductionRunnerHelper$"},
		Env:                []string{claudePTYHelperEnv + "=" + mode},
		Cwd:                t.TempDir(),
		Input:              "/usage\n/exit\n",
		Timeout:            2 * time.Second,
		StdoutLimitBytes:   claudeQuotaOutputBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: claudeQuotaOutputBytes + StderrLimitBytes,
		Columns:            claudeQuotaColumns,
		Rows:               claudeQuotaRows,
	}
}

func runClaudePTYInteractiveHelper() {
	info, err := os.Stdin.Stat()
	isTerminal := err == nil && info.Mode()&os.ModeCharDevice != 0
	ws, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		fmt.Printf("TTY status=%t size-error=%v\n", isTerminal, err)
	} else {
		fmt.Printf("TTY status=%t rows=%d cols=%d\n", isTerminal, ws.Row, ws.Col)
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimSpace(line)
			fmt.Println("INPUT " + line)
			if line == "/usage" {
				fmt.Println("Claude Code Usage")
				fmt.Println("Current session: 25% used resets at 2026-07-14T12:00:00Z")
			}
			if line == "/exit" {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func runClaudePTYSpawnAndHangHelper() {
	cmd := exec.Command(os.Args[0], "-test.run=^TestClaudePTYProductionRunnerHelper$")
	cmd.Env = []string{claudePTYHelperEnv + "=sleep-child"}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start child: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("CHILD_PID=%d\n", cmd.Process.Pid)
	sleepForever()
}

func runClaudePTYFloodHelper() {
	chunk := strings.Repeat("x", 256)
	for i := 0; i < 1024; i++ {
		fmt.Println(chunk)
	}
	sleepForever()
}

func runClaudePTYEarlyExitHelper() {
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	fmt.Printf("early exit after %s\n", strings.TrimSpace(line))
	os.Exit(7)
}

func childPIDFromOutput(t *testing.T, output string) int {
	t.Helper()
	match := regexp.MustCompile(`CHILD_PID=(\d+)`).FindStringSubmatch(output)
	if len(match) != 2 {
		t.Fatalf("child pid missing from output:\n%s", output)
	}
	pid, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse child pid %q: %v", match[1], err)
	}
	return pid
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still alive after PTY runner cleanup", pid)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func sleepForever() {
	for {
		time.Sleep(time.Hour)
	}
}
