package supervisedexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunCompletedExitCodeZero(t *testing.T) {
	cmd := helperCommand(t, "exit", "0")

	result, err := Run(context.Background(), cmd, Options{HardCap: 10 * time.Second})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v", result.Outcome, OutcomeCompleted)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Killed {
		t.Fatal("Killed = true, want false")
	}
}

func TestRunCompletedExitCodeNonZero(t *testing.T) {
	cmd := helperCommand(t, "exit", "7")

	result, err := Run(context.Background(), cmd, Options{HardCap: 10 * time.Second})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v", result.Outcome, OutcomeCompleted)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
	if result.Killed {
		t.Fatal("Killed = true, want false")
	}
}

func TestRunDeadlineKillsProcess(t *testing.T) {
	cmd := helperCommand(t, "sleep", "10s")

	result, err := Run(context.Background(), cmd, Options{HardCap: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeDeadline {
		t.Fatalf("Outcome = %v, want %v", result.Outcome, OutcomeDeadline)
	}
	if !result.Killed {
		t.Fatal("Killed = false, want true")
	}
	if cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil; Wait was not drained")
	}
}

func TestRunStalledKillsSilentProcess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "write-then-sleep", logPath, "10s")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: 200 * time.Millisecond,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %v, want %v", result.Outcome, OutcomeStalled)
	}
	if !result.Killed {
		t.Fatal("Killed = false, want true")
	}
}

func TestRunSteadyLogGrowthDoesNotStall(t *testing.T) {
	// The process must stay alive across several stall polls (poll interval =
	// StallTimeout/4, capped at 500ms) while the log keeps growing, so the
	// growth-resets-lastProgress path is actually exercised. A quick process
	// that exits before the first poll would not test it. Margins are generous
	// (100ms writes vs a 3s stall timeout) to stay robust on a slow -race runner.
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "write-loop", logPath, "100ms", "15", "0")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:      30 * time.Second,
		StallTimeout: 3 * time.Second,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v (steady growth must not stall)", result.Outcome, OutcomeCompleted)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Killed {
		t.Fatal("Killed = true, want false")
	}
	if result.Elapsed < 500*time.Millisecond {
		t.Fatalf("Elapsed = %s, want >= 500ms so at least one stall poll ran during growth", result.Elapsed)
	}
}

func TestRunStallTimeoutZeroDisablesStallDetection(t *testing.T) {
	cmd := helperCommand(t, "sleep-exit", "80ms", "0")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: 0,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v", result.Outcome, OutcomeCompleted)
	}
	if result.Killed {
		t.Fatal("Killed = true, want false")
	}
}

func TestRunKillDrainsWaitPromptly(t *testing.T) {
	cmd := helperCommand(t, "sleep", "10s")
	start := time.Now()

	result, err := Run(context.Background(), cmd, Options{HardCap: 200 * time.Millisecond})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeDeadline {
		t.Fatalf("Outcome = %v, want %v", result.Outcome, OutcomeDeadline)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run took %s after kill, want under 5s", elapsed)
	}
	if cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil; Wait was not drained")
	}
}

func TestRunZeroHardCapUsesDefault(t *testing.T) {
	oldDefault := defaultHardCap
	defaultHardCap = 200 * time.Millisecond
	t.Cleanup(func() {
		defaultHardCap = oldDefault
	})

	cmd := helperCommand(t, "sleep", "10s")

	result, err := Run(context.Background(), cmd, Options{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeDeadline {
		t.Fatalf("Outcome = %v, want %v", result.Outcome, OutcomeDeadline)
	}
	if !result.Killed {
		t.Fatal("Killed = false, want true")
	}
	if result.Elapsed > 5*time.Second {
		t.Fatalf("Elapsed = %s, default hard cap did not bound the process", result.Elapsed)
	}
}

func TestRunOnStallOnceAndGraceDelaysKill(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "write-then-sleep", logPath, "10s")
	var calls atomic.Int32
	stalled := make(chan time.Duration, 1)
	stallTimeout := 200 * time.Millisecond
	stallGrace := 300 * time.Millisecond

	result, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: stallTimeout,
		LogPath:      logPath,
		StallGrace:   stallGrace,
		OnStall: func(silentFor time.Duration) {
			calls.Add(1)
			stalled <- silentFor
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %v, want %v", result.Outcome, OutcomeStalled)
	}
	select {
	case silentFor := <-stalled:
		if silentFor < stallTimeout {
			t.Fatalf("OnStall silentFor = %s, want at least %s", silentFor, stallTimeout)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnStall was not observed")
	}
	if calls.Load() != 1 {
		t.Fatalf("OnStall calls = %d, want 1", calls.Load())
	}
	if result.Elapsed < stallGrace {
		t.Fatalf("Elapsed = %s, want at least StallGrace %s", result.Elapsed, stallGrace)
	}
}

func TestRunRequiresLogPathWhenStallEnabled(t *testing.T) {
	cmd := helperCommand(t, "exit", "0")

	_, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: time.Millisecond,
	})
	if err == nil {
		t.Fatal("Run error = nil, want error")
	}
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SUPERVISEDEXEC_HELPER") != "1" {
		return
	}
	separator := helperSeparatorIndex(os.Args)
	if separator < 0 || separator+1 >= len(os.Args) {
		fmt.Fprintln(os.Stderr, "missing helper separator")
		os.Exit(2)
	}
	mode := os.Args[separator+1]
	args := os.Args[separator+2:]

	switch mode {
	case "exit":
		code := parseInt(args[0])
		os.Exit(code)
	case "sleep":
		time.Sleep(parseDuration(args[0]))
		os.Exit(0)
	case "sleep-exit":
		time.Sleep(parseDuration(args[0]))
		os.Exit(parseInt(args[1]))
	case "write-then-sleep":
		appendLog(args[0], "first")
		time.Sleep(parseDuration(args[1]))
		os.Exit(0)
	case "write-loop":
		logPath := args[0]
		interval := parseDuration(args[1])
		count := parseInt(args[2])
		code := parseInt(args[3])
		for i := 0; i < count; i++ {
			appendLog(logPath, fmt.Sprintf("line %d", i))
			time.Sleep(interval)
		}
		os.Exit(code)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}

func helperCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	cmdArgs := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(), "GO_WANT_SUPERVISEDEXEC_HELPER=1")
	return cmd
}

func helperSeparatorIndex(args []string) int {
	for i, arg := range args {
		if arg == "--" {
			return i
		}
	}
	return -1
}

func parseDuration(value string) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse duration %q: %v\n", value, err)
		os.Exit(2)
	}
	return duration
}

func parseInt(value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse int %q: %v\n", value, err)
		os.Exit(2)
	}
	return n
}

func appendLog(path, line string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir log dir: %v\n", err)
		os.Exit(2)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		os.Exit(2)
	}
	if _, err := fmt.Fprintln(f, line); err != nil {
		fmt.Fprintf(os.Stderr, "write log: %v\n", err)
		_ = f.Close()
		os.Exit(2)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close log: %v\n", err)
		os.Exit(2)
	}
}
