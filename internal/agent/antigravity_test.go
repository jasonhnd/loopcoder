package agent

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

func TestAntigravityRegistration(t *testing.T) {
	runner, err := Lookup("antigravity")
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if runner == nil {
		t.Fatal("Lookup returned nil runner")
	}
	if !slices.Contains(SupportedProviders(), "antigravity") {
		t.Fatalf("SupportedProviders() = %#v, want antigravity registered", SupportedProviders())
	}
}

func TestBuildAntigravityArgs(t *testing.T) {
	tests := []struct {
		name string
		inv  Invocation
		want []string
	}{
		{
			name: "model and depth",
			inv: Invocation{
				WorktreePath: "wt",
				Prompt:       "do the work",
				Model:        "Gemini 3.1 Pro",
				Effort:       "High",
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--model", "Gemini 3.1 Pro (High)",
			},
		},
		{
			name: "empty depth omits parentheses",
			inv: Invocation{
				WorktreePath: "wt",
				Prompt:       "do the work",
				Model:        "Future Model",
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--model", "Future Model",
			},
		},
		{
			name: "defaults from static antigravity registry",
			inv: Invocation{
				WorktreePath: "wt",
				Prompt:       "do the work",
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--model", "Gemini 3.1 Pro (High)",
			},
		},
		{
			name: "model default depth",
			inv: Invocation{
				WorktreePath: "wt",
				Prompt:       "do the work",
				Model:        "Opus 4.6",
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--model", "Opus 4.6 (Thinking)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildAntigravityArgs(tt.inv)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildAntigravityArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestAntigravityRunnerClosesStdinPinsWorktreeAndCapturesPlainText(t *testing.T) {
	worktree := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "antigravity.log")
	prompt := "do the sensitive work"
	restore := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		if got := strings.TrimSuffix(filepath.Base(cmd.Path), ".exe"); got != "agy" {
			t.Fatalf("Path = %q, want agy executable", cmd.Path)
		}
		if cmd.Dir != worktree {
			t.Fatalf("Dir = %q, want %q", cmd.Dir, worktree)
		}
		if cmd.Stdin != nil {
			t.Fatalf("Stdin = %#v, want nil closed stdin", cmd.Stdin)
		}
		wantArgs := []string{"agy", "-p", prompt, "--add-dir", worktree, "--model", "Gemini 3.1 Pro (High)"}
		if !reflect.DeepEqual(cmd.Args, wantArgs) {
			t.Fatalf("Args = %#v, want %#v", cmd.Args, wantArgs)
		}
		_, _ = io.WriteString(cmd.Stdout, "plain text summary\n")
		_, _ = io.WriteString(cmd.Stderr, "provider warning\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restore()

	result, err := AntigravityRunner{}.Run(context.Background(), Invocation{
		WorktreePath: worktree,
		Prompt:       prompt,
		LogPath:      logPath,
		Model:        "Gemini 3.1 Pro",
		Effort:       "High",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 0 || result.Summary != "plain text summary" || result.Model != "Gemini 3.1 Pro (High)" || result.Effort != "High" {
		t.Fatalf("result = %#v", result)
	}
	assertNilInt64Ptr(t, result.Usage.TotalTokens)
	assertPrivateFileMode(t, logPath)
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, want := range []string{"plain text summary", "provider warning"} {
		if !strings.Contains(string(logBytes), want) {
			t.Fatalf("log missing %q:\n%s", want, string(logBytes))
		}
	}
}

func TestAntigravityRunnerReadOnlyFailsClosedWithoutLaunchingAgy(t *testing.T) {
	restore := stubRunSupervised(t, func(context.Context, *exec.Cmd, supervisedexec.Options) (supervisedexec.Result, error) {
		t.Fatal("read-only antigravity invocation should fail before launching agy")
		return supervisedexec.Result{}, nil
	})
	defer restore()

	result, err := AntigravityRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "review safely",
		LogPath:      filepath.Join(t.TempDir(), "antigravity.log"),
		Model:        "Gemini 3.1 Pro",
		Effort:       "High",
		ReadOnly:     true,
	})
	if err == nil {
		t.Fatal("Run returned nil error, want read-only failure")
	}
	if !strings.Contains(err.Error(), "read-only mode is not available or verified") {
		t.Fatalf("Run error = %v", err)
	}
	if result.ExitCode != 1 || result.Model != "Gemini 3.1 Pro (High)" || result.Effort != "High" {
		t.Fatalf("read-only result = %#v", result)
	}
}
