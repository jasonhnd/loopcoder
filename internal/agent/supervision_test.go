package agent

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

func TestProviderRunnersSurfaceSupervisedHang(t *testing.T) {
	tests := []struct {
		name    string
		runner  Runner
		outcome supervisedexec.Outcome
		reason  string
	}{
		{name: "codex stall", runner: ExecCodexRunner{}, outcome: supervisedexec.OutcomeStalled, reason: HungReasonStall},
		{name: "claude deadline", runner: ClaudeRunner{}, outcome: supervisedexec.OutcomeDeadline, reason: HungReasonDeadline},
		{name: "gemini stall", runner: GeminiRunner{}, outcome: supervisedexec.OutcomeStalled, reason: HungReasonStall},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worktreePath := t.TempDir()
			restore := stubRunSupervised(t, func(_ context.Context, _ *exec.Cmd, opts supervisedexec.Options) (supervisedexec.Result, error) {
				if opts.HardCap != 123*time.Millisecond {
					t.Fatalf("HardCap = %s, want 123ms", opts.HardCap)
				}
				if opts.StallTimeout != 45*time.Millisecond {
					t.Fatalf("StallTimeout = %s, want 45ms", opts.StallTimeout)
				}
				if opts.LogPath == "" {
					t.Fatal("LogPath was empty")
				}
				if opts.WorktreePath != worktreePath {
					t.Fatalf("WorktreePath = %q, want %q", opts.WorktreePath, worktreePath)
				}
				if opts.LivenessMode != supervisedexec.LivenessModeLogOnly {
					t.Fatalf("LivenessMode = %q, want %q", opts.LivenessMode, supervisedexec.LivenessModeLogOnly)
				}
				if opts.LivenessCommand != "echo alive" {
					t.Fatalf("LivenessCommand = %q, want echo alive", opts.LivenessCommand)
				}
				return supervisedexec.Result{Outcome: tt.outcome, Killed: true}, nil
			})
			defer restore()

			result, err := tt.runner.Run(context.Background(), Invocation{
				WorktreePath:    worktreePath,
				Prompt:          "do work",
				LogPath:         filepath.Join(t.TempDir(), "provider.log"),
				HardCap:         123 * time.Millisecond,
				StallTimeout:    45 * time.Millisecond,
				LivenessMode:    "log-only",
				LivenessCommand: "echo alive",
				OutputSchema:    "",
			})
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if !result.Hung || result.HungReason != tt.reason {
				t.Fatalf("hung fields = (%v, %q), want true/%q", result.Hung, result.HungReason, tt.reason)
			}
			if result.ExitCode != -1 {
				t.Fatalf("ExitCode = %d, want -1 for killed invocation", result.ExitCode)
			}
		})
	}
}

func TestProviderRunnerDoesNotMarkParentContextCancellationHung(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	restore := stubRunSupervised(t, func(ctx context.Context, _ *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeDeadline, Killed: true}, ctx.Err()
	})
	defer restore()

	result, err := ExecCodexRunner{}.Run(ctx, Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "do work",
		LogPath:      filepath.Join(t.TempDir(), "codex.log"),
		HardCap:      time.Second,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if result.Hung || result.HungReason != "" {
		t.Fatalf("hung fields = (%v, %q), want false/empty", result.Hung, result.HungReason)
	}
}

func TestProviderRunnerRelaysLaunchBeforeIdentityCallback(t *testing.T) {
	launched := false
	started := false
	restore := stubRunSupervised(t, func(_ context.Context, _ *exec.Cmd, opts supervisedexec.Options) (supervisedexec.Result, error) {
		if opts.OnLaunch == nil || opts.OnStart == nil {
			t.Fatal("launch callbacks were not relayed")
		}
		opts.OnLaunch(123)
		if !launched {
			t.Fatal("OnLaunch did not mark the provider launch")
		}
		if err := opts.OnStart(supervisedexec.StartedProcess{PID: 123}); err != nil {
			t.Fatalf("OnStart: %v", err)
		}
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted}, nil
	})
	defer restore()

	_, _ = ExecCodexRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "do work",
		LogPath:      filepath.Join(t.TempDir(), "codex.log"),
		OnProviderLaunch: func(pid int) {
			launched = pid == 123
		},
		OnProviderStart: func(process ProviderProcess) error {
			started = process.PID == 123
			return nil
		},
	})
	if !launched || !started {
		t.Fatalf("launched=%v started=%v", launched, started)
	}
}

func stubRunSupervised(t *testing.T, fn func(context.Context, *exec.Cmd, supervisedexec.Options) (supervisedexec.Result, error)) func() {
	t.Helper()
	original := runSupervised
	runSupervised = fn
	return func() {
		runSupervised = original
	}
}
