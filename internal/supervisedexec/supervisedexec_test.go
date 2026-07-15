package supervisedexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/storage"
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

func TestRunDarwinArm64ProviderAuthorityIdentifiesProviderProcessGroup(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("macOS arm64 process authority fixture")
	}
	outerPID := os.Getpid()
	var started StartedProcess
	cmd := exec.Command("/bin/sleep", "0.1")
	result, err := Run(context.Background(), cmd, Options{
		HardCap: 5 * time.Second,
		RunID:   "run-authority-fixture",
		Role:    "worker",
		OnStart: func(startedProcess StartedProcess) error {
			started = startedProcess
			identity := process.Identity{
				PID:                  startedProcess.PID,
				PGID:                 startedProcess.PGID,
				ProcessBirthIdentity: startedProcess.ProcessBirthIdentity,
				ExecutableIdentity:   startedProcess.ExecutableIdentity,
				Ambiguous:            startedProcess.IdentityAmbiguous,
			}
			if err := process.VerifySnapshot(identity); err != nil {
				return err
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != OutcomeCompleted || result.ExitCode != 0 {
		t.Fatalf("result = %#v, want completed sleep", result)
	}
	if started.PID <= 0 || started.PID == outerPID {
		t.Fatalf("started PID = %d, outer PID = %d; want provider child PID", started.PID, outerPID)
	}
	if started.PGID != started.PID {
		t.Fatalf("started PGID = %d, PID = %d; want provider-led process group", started.PGID, started.PID)
	}
	if started.ProcessBirthIdentity == "" || started.ExecutableIdentity == "" || started.IdentityAmbiguous {
		t.Fatalf("started process authority = %#v, want complete identity", started)
	}
	identity := process.Identity{
		PID:                  started.PID,
		PGID:                 started.PGID,
		ProcessBirthIdentity: started.ProcessBirthIdentity,
		ExecutableIdentity:   started.ExecutableIdentity,
		Ambiguous:            started.IdentityAmbiguous,
	}
	if err := process.VerifySnapshot(identity); err == nil {
		t.Fatalf("VerifySnapshot succeeded after sleep exited; fixture should not treat exited/PID-reused process as proof")
	}
}

func TestGuardianVerifyAndKillRequiresCurrentAuthority(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("macOS arm64 guardian authority fixture")
	}
	cmd, authority := startGuardianAuthorityProcess(t)
	defer terminateProcessGroup(cmd.Process.Pid)
	defer cmd.Wait()

	cfg := guardianConfig{
		SchemaVersion:   guardianSchema,
		ProjectID:       authority.ProjectID,
		RunID:           authority.RunID,
		AttemptID:       authority.AttemptID,
		OwnerID:         authority.OwnerID,
		ClaimGeneration: authority.ClaimGeneration,
	}
	tests := []struct {
		name       string
		mutate     func(storage.ProviderExecutionAuthority) storage.ProviderExecutionAuthority
		wantEvent  string
		wantKilled bool
	}{
		{
			name:       "current live authority kills process group",
			wantEvent:  "killed",
			wantKilled: true,
		},
		{
			name: "completed authority is not killed",
			mutate: func(a storage.ProviderExecutionAuthority) storage.ProviderExecutionAuthority {
				a.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
				a.TerminalState = "succeeded"
				return a
			},
			wantEvent: "skip",
		},
		{
			name: "authority generation change is not killed",
			mutate: func(a storage.ProviderExecutionAuthority) storage.ProviderExecutionAuthority {
				a.ClaimGeneration++
				return a
			},
			wantEvent: "skip",
		},
		{
			name: "pid reuse birth mismatch is not killed",
			mutate: func(a storage.ProviderExecutionAuthority) storage.ProviderExecutionAuthority {
				a.ProcessBirthIdentity = "different process birth identity"
				return a
			},
			wantEvent: "skip",
		},
		{
			name: "ambiguous identity is not killed",
			mutate: func(a storage.ProviderExecutionAuthority) storage.ProviderExecutionAuthority {
				a.IdentityAmbiguous = true
				a.AmbiguityReason = "process-birth-identity-unavailable"
				return a
			},
			wantEvent: "skip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := authority
			if tt.mutate != nil {
				candidate = tt.mutate(candidate)
			}
			var killedPGID int
			event := guardianVerifyAndKill(context.Background(), cfg, func(context.Context, guardianConfig) (storage.ProviderExecutionAuthority, error) {
				return candidate, nil
			}, func(pgid int) error {
				killedPGID = pgid
				return nil
			})
			if event.Event != tt.wantEvent {
				t.Fatalf("event = %#v, want %q", event, tt.wantEvent)
			}
			if (killedPGID != 0) != tt.wantKilled {
				t.Fatalf("killedPGID = %d, want killed=%v event=%#v", killedPGID, tt.wantKilled, event)
			}
			if tt.wantKilled && killedPGID != authority.ProviderPGID {
				t.Fatalf("killed pgid = %d, want %d", killedPGID, authority.ProviderPGID)
			}
		})
	}
}

func TestDarwinGuardianReapsProviderAfterSupervisorSIGKILL(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("macOS arm64 guardian crash fixture")
	}
	// Full 100-run acceptance under normal tests; under -race each attempt is multi-second
	// and the package would exceed CI's default 10m package timeout.
	attempts := 100
	if raceBuildEnabled {
		attempts = 5
	}
	for i := 0; i < attempts; i++ {
		t.Run(fmt.Sprintf("attempt-%03d", i), func(t *testing.T) {
			root := t.TempDir()
			storePath := filepath.Join(root, "home", "data", "loopcoder.db")
			diagPath := filepath.Join(root, "home", "logs", fmt.Sprintf("guardian-%03d.jsonl", i))
			readyPath := filepath.Join(root, "ready.json")
			ctx := context.Background()
			store, err := storage.Open(ctx, storage.Options{Path: storePath})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			seedGuardianProject(t, ctx, store, "proj-guardian", root)
			if err := store.Close(); err != nil {
				t.Fatalf("Close seed store: %v", err)
			}

			parent := helperCommand(t, "guardian-supervisor", storePath, diagPath, readyPath, strconv.Itoa(i))
			if err := parent.Start(); err != nil {
				t.Fatalf("start supervisor helper: %v", err)
			}
			ready := waitGuardianReady(t, readyPath)
			if ready.ProviderPID <= 0 || ready.GuardianPID <= 0 {
				t.Fatalf("ready = %#v, want provider and guardian pids", ready)
			}
			if !process.Alive(ready.ProviderPID) {
				t.Fatalf("provider pid %d is not alive before crash", ready.ProviderPID)
			}
			if err := parent.Process.Kill(); err != nil {
				t.Fatalf("kill supervisor: %v", err)
			}
			_ = parent.Wait()
			reapWithin := 2 * time.Second
			if raceBuildEnabled {
				reapWithin = 10 * time.Second
			}
			waitNotExecuting(t, ready.ProviderPID, reapWithin, "provider", diagPath)
			waitNotExecuting(t, ready.GuardianPID, reapWithin, "guardian", diagPath)
			assertGuardianDiagnostic(t, diagPath, "killed")
		})
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

func TestRunSilentProcessTreeActivityDoesNotStall(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "silent-child-churn", logPath, "100ms", "16", "850ms", "0")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: 1500 * time.Millisecond,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v (quiet child-process activity must not stall)", result.Outcome, OutcomeCompleted)
	}
	if result.Killed {
		t.Fatal("Killed = true, want false")
	}
	if result.Elapsed < 2*time.Second {
		t.Fatalf("Elapsed = %s, want >= 2s so process-tree activity crossed the stall window", result.Elapsed)
	}
}

func TestRunSilentCPUActiveChildDoesNotStall(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "silent-cpu-child", logPath, "2500ms", "0")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: 1200 * time.Millisecond,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v (quiet CPU-active child must not stall)", result.Outcome, OutcomeCompleted)
	}
	if result.Killed {
		t.Fatal("Killed = true, want false")
	}
	if result.Elapsed < 2*time.Second {
		t.Fatalf("Elapsed = %s, want >= 2s so CPU activity crossed the stall window", result.Elapsed)
	}
}

func TestRunSilentCPUActiveProviderContinuesWhenReceiptPersistenceFails(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	logPath := filepath.Join(root, "worker.log")
	dbPath := filepath.Join(root, "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer store.Close()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, local_path_canonical, display_name, identity_source, created_at, updated_at)
			VALUES ('proj_supervised_progress', ?, ?, 'repo', 'local-path', ?, ?)`,
			root, root, ts, ts)
		return err
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	failing := &supervisedFailingWriteStore{Store: store, skip: 1, failures: 100}
	emitter, err := progress.NewEmitter(progress.EmitterOptions{
		Store:              failing,
		ProjectID:          "proj_supervised_progress",
		DeliveryRunID:      "run-supervised-progress",
		RunID:              "run-supervised-progress",
		CorrelationID:      "cpu-active-worker",
		MaxSilenceInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	emitter.Start(ctx)
	if _, err := emitter.Emit(ctx, progress.Observation{
		TaskID:         "issue-828",
		AttemptID:      "job-cpu-active",
		AttemptOrdinal: 1,
		Phase:          "codex_started",
		Status:         "running",
		TaskCounts:     progress.TaskCounts{Total: 1, Running: 1},
		Provider: progress.ProviderIdentity{
			ProviderID:         "codex",
			ProviderConfidence: "exact",
		},
		Heartbeat: progress.AgeEvidence{State: "exact", ObservedAt: ts},
		Progress:  progress.AgeEvidence{State: "exact", ObservedAt: ts},
		Evidence: []progress.EvidenceRef{{
			RecordKind:     "attempt-sidecar",
			RecordID:       "job-cpu-active",
			Summary:        "worker process is still supervised",
			Classification: "local-diagnostic",
			Confidence:     "exact",
		}},
		QuotaBudget: progress.QuotaBudgetState{State: progress.Unknown, Confidence: progress.Unknown, RemainingQuantity: -1},
		NextAction:  progress.ActionState{State: "wait-provider"},
		OccurredAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Emit initial: %v", err)
	}
	defer emitter.Stop(context.Background())

	cmd := helperCommand(t, "silent-cpu-child", logPath, "2500ms", "0")
	result, err := Run(ctx, cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: 1200 * time.Millisecond,
		LogPath:      logPath,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v while CPU activity continues despite receipt persistence failures", result.Outcome, OutcomeCompleted)
	}
	if got := failing.Attempts(); got < 2 {
		t.Fatalf("progress write attempts = %d, want initial plus failed periodic attempt", got)
	}
	receipts, err := progress.ListReceipts(ctx, store, progress.ListFilter{
		ProjectID:     "proj_supervised_progress",
		DeliveryRunID: "run-supervised-progress",
		CorrelationID: "cpu-active-worker",
	})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	if len(receipts) != 1 {
		t.Fatalf("receipt count = %d, want only initial receipt while periodic persistence fails", len(receipts))
	}
}

func TestRunLoopCoderReceiptLogGrowthDoesNotResetStallClock(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "write-then-sleep", logPath, "10s")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				appendLog(logPath, "[loopcoder] receipt: still supervising")
			}
		}
	}()

	result, err := Run(ctx, cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: 300 * time.Millisecond,
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
	if result.Elapsed > 2*time.Second {
		t.Fatalf("Elapsed = %s, receipt-only log writes reset the stall clock", result.Elapsed)
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

func TestRunLogOnlyIgnoresProcessTreeActivity(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "silent-child-churn", logPath, "100ms", "16", "850ms", "0")

	result, err := Run(context.Background(), cmd, Options{
		HardCap:      10 * time.Second,
		StallTimeout: 400 * time.Millisecond,
		LogPath:      logPath,
		LivenessMode: LivenessModeLogOnly,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeStalled {
		t.Fatalf("Outcome = %v, want %v when log-only ignores process-tree churn", result.Outcome, OutcomeStalled)
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

func TestRunCustomLivenessArgvDoesNotUseShell(t *testing.T) {
	t.Setenv("GO_WANT_SUPERVISEDEXEC_HELPER", "1")
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := helperCommand(t, "write-then-sleep", logPath, "1200ms")
	literal := "alive && exit 9"

	result, err := Run(context.Background(), cmd, Options{
		HardCap:                10 * time.Second,
		StallTimeout:           800 * time.Millisecond,
		LogPath:                logPath,
		LivenessMode:           LivenessModeCustom,
		LivenessCommand:        EncodeLivenessArgv([]string{os.Args[0], "-test.run=TestHelperProcess", "--", "assert-arg", literal}),
		LivenessCommandHardCap: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Outcome != OutcomeCompleted {
		t.Fatalf("Outcome = %v, want %v because argv liveness reports progress", result.Outcome, OutcomeCompleted)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile log: %v", err)
	}
	for _, want := range []string{"custom liveness ok", literal} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("custom liveness argv log missing %q:\n%s", want, string(data))
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

func TestProcessActivityFirstAvailableCountsAsProgress(t *testing.T) {
	current := processActivityObservation{available: true, signature: "123:S:0"}
	if !current.changedFrom(processActivityObservation{}) {
		t.Fatal("changedFrom = false, want first available process observation to count as progress")
	}
	if current.changedFrom(current) {
		t.Fatal("changedFrom = true, want unchanged available process observation to be idle")
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

func TestObserveWorktreeSkipsGeneratedDirsAndEarlyExitsOnProgress(t *testing.T) {
	oldLimit := worktreeLivenessMaxFiles
	worktreeLivenessMaxFiles = 1
	t.Cleanup(func() {
		worktreeLivenessMaxFiles = oldLimit
	})

	root := t.TempDir()
	base := time.Now().Add(-time.Hour)
	if err := writeFileWithMTime(filepath.Join(root, "a-new.txt"), "new", base.Add(time.Minute)); err != nil {
		t.Fatalf("write new file: %v", err)
	}
	if err := writeFileWithMTime(filepath.Join(root, "b-old.txt"), "old", base); err != nil {
		t.Fatalf("write old file: %v", err)
	}
	if err := writeFileWithMTime(filepath.Join(root, ".git", "index"), "ignored", base.Add(2*time.Minute)); err != nil {
		t.Fatalf("write git file: %v", err)
	}
	if err := writeFileWithMTime(filepath.Join(root, "node_modules", "pkg", "index.js"), "ignored", base.Add(3*time.Minute)); err != nil {
		t.Fatalf("write generated file: %v", err)
	}

	observation := observeWorktreeAfter(root, base)
	if observation.rootErr != nil {
		t.Fatalf("rootErr = %v, want nil because first newer tracked file should early-exit before cap", observation.rootErr)
	}
	if observation.filesExamined != 1 {
		t.Fatalf("filesExamined = %d, want early exit after first file", observation.filesExamined)
	}
	if !observation.latestModTime.After(base) {
		t.Fatalf("latestModTime = %s, want newer than baseline", observation.latestModTime)
	}
}

func TestObserveWorktreeCapsWalkWithDiagnosticFallback(t *testing.T) {
	oldLimit := worktreeLivenessMaxFiles
	worktreeLivenessMaxFiles = 1
	t.Cleanup(func() {
		worktreeLivenessMaxFiles = oldLimit
	})

	root := t.TempDir()
	base := time.Now().Add(-time.Hour)
	if err := writeFileWithMTime(filepath.Join(root, "a-old.txt"), "old", base); err != nil {
		t.Fatalf("write first file: %v", err)
	}
	if err := writeFileWithMTime(filepath.Join(root, "b-old.txt"), "old", base); err != nil {
		t.Fatalf("write second file: %v", err)
	}
	if err := writeFileWithMTime(filepath.Join(root, ".git", "index"), "ignored", base.Add(time.Hour)); err != nil {
		t.Fatalf("write git file: %v", err)
	}

	observation := observeWorktreeAfter(root, base.Add(time.Minute))
	if observation.rootErr == nil {
		t.Fatal("rootErr = nil, want file cap diagnostic")
	}
	if !strings.Contains(observation.rootErr.Error(), "worktree liveness file cap exceeded after 1 files") {
		t.Fatalf("rootErr = %v, want file cap diagnostic", observation.rootErr)
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
	case "silent-child-churn":
		logPath := args[0]
		interval := parseDuration(args[1])
		count := parseInt(args[2])
		childDuration := parseDuration(args[3])
		code := parseInt(args[4])
		appendLog(logPath, "first")
		children := make([]*exec.Cmd, 0, count)
		for i := 0; i < count; i++ {
			child := helperCommandForProcess("sleep-exit", childDuration.String(), "0")
			child.Stdout = io.Discard
			child.Stderr = io.Discard
			if err := child.Start(); err != nil {
				fmt.Fprintf(os.Stderr, "start silent child: %v\n", err)
				os.Exit(2)
			}
			children = append(children, child)
			time.Sleep(interval)
		}
		for _, child := range children {
			if err := child.Wait(); err != nil {
				fmt.Fprintf(os.Stderr, "wait silent child: %v\n", err)
				os.Exit(2)
			}
		}
		os.Exit(code)
	case "silent-cpu-child":
		logPath := args[0]
		childDuration := parseDuration(args[1])
		code := parseInt(args[2])
		appendLog(logPath, "first")
		child := helperCommandForProcess("burn-cpu", childDuration.String(), "0")
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start cpu child: %v\n", err)
			os.Exit(2)
		}
		if err := child.Wait(); err != nil {
			fmt.Fprintf(os.Stderr, "wait cpu child: %v\n", err)
			os.Exit(2)
		}
		os.Exit(code)
	case "burn-cpu":
		duration := parseDuration(args[0])
		code := parseInt(args[1])
		deadline := time.Now().Add(duration)
		var sum uint64
		for time.Now().Before(deadline) {
			sum += uint64(time.Now().UnixNano()) | 1
			sum ^= sum << 7
			sum ^= sum >> 3
		}
		if sum == 0 {
			fmt.Fprintln(os.Stderr, "unreachable cpu sink")
		}
		os.Exit(code)
	case "assert-arg":
		if len(args) != 1 {
			fmt.Fprintf(os.Stderr, "assert-arg got %d args\n", len(args))
			os.Exit(2)
		}
		fmt.Println(args[0])
		os.Exit(0)
	case "guardian-supervisor":
		runGuardianSupervisorHelper(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(2)
	}
}

type guardianReadyRecord struct {
	ProviderPID  int `json:"provider_pid"`
	ProviderPGID int `json:"provider_pgid"`
	GuardianPID  int `json:"guardian_pid"`
}

func runGuardianSupervisorHelper(args []string) {
	if len(args) != 4 {
		fmt.Fprintf(os.Stderr, "guardian-supervisor got %d args\n", len(args))
		os.Exit(2)
	}
	storePath, diagPath, readyPath, suffix := args[0], args[1], args[2], args[3]
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: storePath})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open store: %v\n", err)
		os.Exit(2)
	}
	defer store.Close()
	runID := "run-guardian-crash-" + suffix
	attemptID := "job-guardian-crash-" + suffix
	ownerID := "worker:" + runID + ":" + attemptID + ":1"
	var started StartedProcess
	cmd := helperCommandForProcess("sleep", "30s")
	_, err = Run(ctx, cmd, Options{
		HardCap: 35 * time.Second,
		RunID:   runID,
		Role:    "worker",
		OnStart: func(process StartedProcess) error {
			started = process
			_, err := storage.PersistProviderExecutionAuthority(ctx, store, storage.ProviderExecutionAuthority{
				ProjectID:            "proj-guardian",
				RunID:                runID,
				AttemptID:            attemptID,
				ProviderPID:          process.PID,
				ProviderPGID:         process.PGID,
				ProcessBirthIdentity: process.ProcessBirthIdentity,
				ExecutableIdentity:   process.ExecutableIdentity,
				OwnerID:              ownerID,
				ClaimGeneration:      1,
				WorktreePath:         filepath.Dir(readyPath),
				LogPath:              filepath.Join(filepath.Dir(readyPath), "provider.log"),
				IdentityAmbiguous:    process.IdentityAmbiguous,
				AmbiguityReason:      process.IdentityAmbiguityNote,
			}, time.Now())
			return err
		},
		Guardian: GuardianOptions{
			Enabled:         true,
			StorePath:       storePath,
			DiagnosticPath:  diagPath,
			ProjectID:       "proj-guardian",
			RunID:           runID,
			AttemptID:       attemptID,
			OwnerID:         ownerID,
			ClaimGeneration: 1,
			OnStart: func(guardian GuardianProcess) error {
				return writeGuardianReadyRecord(readyPath, guardianReadyRecord{
					ProviderPID:  started.PID,
					ProviderPGID: started.PGID,
					GuardianPID:  guardian.PID,
				})
			},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "guardian supervisor run: %v\n", err)
		os.Exit(2)
	}
	os.Exit(0)
}

func helperCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	return helperCommandForProcess(args...)
}

func helperCommandForProcess(args ...string) *exec.Cmd {
	cmdArgs := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = helperProcessEnv("GO_WANT_SUPERVISEDEXEC_HELPER=1")
	return cmd
}

func helperProcessEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, extra...)
}

type supervisedFailingWriteStore struct {
	storage.Store
	mu       sync.Mutex
	attempts int
	skip     int
	failures int
}

func (s *supervisedFailingWriteStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	s.mu.Lock()
	s.attempts++
	attempt := s.attempts
	shouldFail := attempt > s.skip && s.failures > 0
	if shouldFail {
		s.failures--
	}
	s.mu.Unlock()
	if shouldFail {
		return errors.New("injected progress write failure")
	}
	return s.Store.WithWriteTx(ctx, fn)
}

func (s *supervisedFailingWriteStore) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
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

func writeFileWithMTime(path, content string, mtime time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Chtimes(path, mtime, mtime)
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

func startGuardianAuthorityProcess(t *testing.T) (*exec.Cmd, storage.ProviderExecutionAuthority) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start authority process: %v", err)
	}
	identity, err := process.Snapshot(cmd.Process.Pid, time.Now())
	if err != nil {
		terminateProcessGroup(cmd.Process.Pid)
		t.Fatalf("snapshot authority process: %v", err)
	}
	return cmd, storage.ProviderExecutionAuthority{
		ProjectID:            "proj-guardian",
		RunID:                "run-guardian",
		AttemptID:            "job-guardian",
		ProviderPID:          identity.PID,
		ProviderPGID:         identity.PGID,
		ProcessBirthIdentity: identity.ProcessBirthIdentity,
		ExecutableIdentity:   identity.ExecutableIdentity,
		OwnerID:              "worker:run-guardian:job-guardian:1",
		ClaimGeneration:      1,
		WorktreePath:         t.TempDir(),
		LogPath:              filepath.Join(t.TempDir(), "provider.log"),
		IdentityAmbiguous:    identity.Ambiguous,
		AmbiguityReason:      identity.AmbiguityReason,
	}
}

func terminateProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func seedGuardianProject(t *testing.T, ctx context.Context, store storage.Store, projectID, projectPath string) {
	t.Helper()
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := tx.Exec(ctx, `INSERT INTO projects(
				id, local_path, created_at, updated_at, local_path_canonical, git_root, identity_source
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			projectID, projectPath, now, now, projectPath, projectPath, "test")
		return err
	})
	if err != nil {
		t.Fatalf("seed guardian project: %v", err)
	}
}

func waitGuardianReady(t *testing.T, path string) guardianReadyRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			var ready guardianReadyRecord
			if err := json.Unmarshal(data, &ready); err != nil {
				t.Fatalf("decode ready record: %v\n%s", err, string(data))
			}
			return ready
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ready record %s not written: %v", path, lastErr)
	return guardianReadyRecord{}
}

func writeGuardianReadyRecord(path string, record guardianReadyRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func assertGuardianDiagnostic(t *testing.T, path, wantEvent string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guardian diagnostic: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event guardianEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode guardian diagnostic line %q: %v", line, err)
		}
		if event.Event == wantEvent {
			return
		}
	}
	t.Fatalf("guardian diagnostic missing event %q:\n%s", wantEvent, string(data))
}
