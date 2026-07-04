package supervisedexec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

func TestRunWorktreeActivityStallDetection(t *testing.T) {
	tests := []struct {
		name         string
		args         func(logPath, worktreePath string) []string
		stallTimeout time.Duration
		wantOutcome  Outcome
		wantKilled   bool
		minElapsed   time.Duration
	}{
		{
			name: "worktree mtime advance extends stall window",
			args: func(logPath, worktreePath string) []string {
				return []string{"write-worktree-loop", logPath, worktreePath, "100ms", "24", "0"}
			},
			stallTimeout: 1200 * time.Millisecond,
			wantOutcome:  OutcomeCompleted,
			minElapsed:   1500 * time.Millisecond,
		},
		{
			name: "silent worker still stalls",
			args: func(logPath, _ string) []string {
				return []string{"write-then-sleep", logPath, "10s"}
			},
			stallTimeout: 300 * time.Millisecond,
			wantOutcome:  OutcomeStalled,
			wantKilled:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			logPath := filepath.Join(root, "worker.log")
			worktreePath := filepath.Join(root, "wt")
			if err := os.MkdirAll(worktreePath, 0o755); err != nil {
				t.Fatalf("mkdir worktree: %v", err)
			}
			cmd := helperCommand(t, tt.args(logPath, worktreePath)...)

			result, err := Run(context.Background(), cmd, Options{
				HardCap:      30 * time.Second,
				StallTimeout: tt.stallTimeout,
				LogPath:      logPath,
				WorktreePath: worktreePath,
			})
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if result.Outcome != tt.wantOutcome {
				t.Fatalf("Outcome = %v, want %v", result.Outcome, tt.wantOutcome)
			}
			if result.Killed != tt.wantKilled {
				t.Fatalf("Killed = %v, want %v", result.Killed, tt.wantKilled)
			}
			if tt.minElapsed > 0 && result.Elapsed < tt.minElapsed {
				t.Fatalf("Elapsed = %s, want >= %s so worktree activity crossed the stall window", result.Elapsed, tt.minElapsed)
			}
		})
	}
}

func TestRunLogOnlyIgnoresWorktreeActivity(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "worker.log")
	worktreePath := filepath.Join(root, "wt")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	cmd := helperCommand(t, "write-worktree-loop", logPath, worktreePath, "100ms", "20", "0")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: 400 * time.Millisecond,
		LogPath:      logPath,
		WorktreePath: worktreePath,
		LivenessMode: LivenessModeLogOnly,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %v, want %v when log-only ignores worktree writes", result.Outcome, OutcomeStalled)
	}
	if !result.Killed {
		t.Fatal("Killed = false, want true")
	}
}

func TestRunCustomLivenessExtendsStallWindow(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "write-then-sleep", logPath, "1200ms")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:                10 * time.Second,
		StallTimeout:           800 * time.Millisecond,
		LogPath:                logPath,
		LivenessMode:           LivenessModeCustom,
		LivenessCommand:        "echo alive",
		LivenessCommandHardCap: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v because custom liveness reports progress", result.Outcome, OutcomeCompleted)
	}
	if result.Killed {
		t.Fatal("Killed = true, want false")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile log: %v", err)
	}
	for _, want := range []string{"custom liveness ok", "alive"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("custom liveness log missing %q:\n%s", want, string(data))
		}
	}
}

func TestRunCustomLivenessFailureDoesNotSelfSignal(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "write-then-sleep", logPath, "10s")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:                10 * time.Second,
		StallTimeout:           300 * time.Millisecond,
		LogPath:                logPath,
		LivenessMode:           LivenessModeCustom,
		LivenessCommand:        shellExitCommand(1),
		LivenessCommandHardCap: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %v, want %v when custom liveness fails", result.Outcome, OutcomeStalled)
	}
	if !result.Killed {
		t.Fatal("Killed = false, want true")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile log: %v", err)
	}
	if !strings.Contains(string(data), "custom liveness failed") {
		t.Fatalf("log missing failed custom liveness probe:\n%s", string(data))
	}
}

func TestRunCustomLivenessRequiresCommand(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "exit", "0")

	_, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: time.Millisecond,
		LogPath:      logPath,
		LivenessMode: LivenessModeCustom,
	})
	if err == nil {
		t.Fatal("Run error = nil, want missing custom liveness command error")
	}
	if !strings.Contains(err.Error(), "LivenessCommand is required") {
		t.Fatalf("Run error = %v, want LivenessCommand message", err)
	}
}

func TestWorktreePollIntervalDecouplesFromLogPollAtScale(t *testing.T) {
	stallTimeout := 5 * time.Minute
	logInterval := stallPollInterval(stallTimeout)
	walkInterval := worktreePollInterval(stallTimeout, logInterval)
	if logInterval != 500*time.Millisecond {
		t.Fatalf("log interval = %s, want 500ms", logInterval)
	}
	if walkInterval != 30*time.Second {
		t.Fatalf("worktree interval = %s, want 30s", walkInterval)
	}

	start := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	lastWalk := start
	logPolls := 0
	walks := 1 // initial observation before the ticker loop
	for tick := start.Add(logInterval); !tick.After(end); tick = tick.Add(logInterval) {
		logPolls++
		if shouldWalkWorktree(tick, lastWalk, walkInterval) {
			walks++
			lastWalk = tick
		}
	}
	if walks >= logPolls/10 {
		t.Fatalf("worktree walks = %d over %d log polls, want far less than log cadence", walks, logPolls)
	}
}

func TestObserveWorktreeRootErrorWarnsAndReturnsZeroObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	observation := observeWorktree(path)
	if observation.rootErr == nil {
		t.Fatal("rootErr = nil, want root walk error")
	}
	if observation.exists || !observation.latestModTime.IsZero() {
		t.Fatalf("observation = %#v, want no file activity", observation)
	}

	var warnings strings.Builder
	emitted := false
	warnWorktreeUnavailable(&warnings, path, observation.rootErr, &emitted)
	if !emitted {
		t.Fatal("warning was not marked emitted")
	}
	for _, want := range []string{"warning", "worktree liveness signal unavailable", "falling back to log-only"} {
		if !strings.Contains(warnings.String(), want) {
			t.Fatalf("warning missing %q:\n%s", want, warnings.String())
		}
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
	case "write-worktree-loop":
		logPath := args[0]
		worktreePath := args[1]
		interval := parseDuration(args[2])
		count := parseInt(args[3])
		code := parseInt(args[4])
		appendLog(logPath, "first")
		for i := 0; i < count; i++ {
			updateWorktreeActivity(worktreePath, i)
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

func shellExitCommand(code int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("exit /b %d", code)
	}
	return fmt.Sprintf("exit %d", code)
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

func updateWorktreeActivity(worktreePath string, index int) {
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir worktree: %v\n", err)
		os.Exit(2)
	}
	path := filepath.Join(worktreePath, "activity.txt")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("activity %d\n", index)), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write activity: %v\n", err)
		os.Exit(2)
	}
	mtime := time.Now().Add(time.Duration(index+1) * time.Second)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		fmt.Fprintf(os.Stderr, "chtimes activity: %v\n", err)
		os.Exit(2)
	}
}
