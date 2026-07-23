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
				Effort:       "medium",
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--dangerously-skip-permissions",
				"--new-project",
				"--model", "Gemini 3.1 Pro (Medium)",
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
				"--dangerously-skip-permissions",
				"--new-project",
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
				"--dangerously-skip-permissions",
				"--new-project",
				"--model", "Gemini 3.1 Pro (Medium)",
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
				"--dangerously-skip-permissions",
				"--new-project",
				"--model", "Opus 4.6 (High)",
			},
		},
		{
			// Unsupported depth must NOT invent GPT-OSS 120B (Low) or silently
			// downgrade to Medium — keep base only (fail closed at CLI if misrouted).
			name: "gpt-oss unsupported low does not synthesize or downgrade",
			inv: Invocation{
				WorktreePath: "wt",
				Prompt:       "do the work",
				Model:        "GPT-OSS 120B",
				Effort:       "low",
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--dangerously-skip-permissions",
				"--new-project",
				"--model", "GPT-OSS 120B",
			},
		},
		{
			// Exact live slug is passed through (observed selection).
			name: "live slug is exact observed model selection",
			inv: Invocation{
				WorktreePath: "wt",
				Prompt:       "do the work",
				Model:        "gpt-oss-120b-medium",
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--dangerously-skip-permissions",
				"--new-project",
				"--model", "gpt-oss-120b-medium",
			},
		},
		{
			name: "exact parenthetical passes through",
			inv: Invocation{
				WorktreePath: "wt",
				Prompt:       "do the work",
				Model:        "GPT-OSS 120B (Medium)",
				Effort:       "low", // must not rewrite exact token
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--dangerously-skip-permissions",
				"--new-project",
				"--model", "GPT-OSS 120B (Medium)",
			},
		},
		{
			name: "supported medium formats title-case CLI token from base",
			inv: Invocation{
				WorktreePath: "wt",
				Prompt:       "do the work",
				Model:        "GPT-OSS 120B",
				Effort:       "medium",
			},
			want: []string{
				"-p", "do the work",
				"--add-dir", "wt",
				"--dangerously-skip-permissions",
				"--new-project",
				"--model", "GPT-OSS 120B (Medium)",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildAntigravityArgs(tt.inv)
			// --add-dir is absolute for isolation; compare after normalizing.
			want := append([]string{}, tt.want...)
			for i := 0; i+1 < len(want); i++ {
				if want[i] == "--add-dir" {
					if abs, err := filepath.Abs(want[i+1]); err == nil {
						want[i+1] = abs
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("BuildAntigravityArgs() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestAntigravityRunner_ModelUnavailableDoesNotExec(t *testing.T) {
	// Explicit unsupported depth / exact-token mismatch must fail closed before agy launch.
	execCalls := 0
	restore := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		execCalls++
		t.Fatalf("agy must not exec on model_unavailable; args=%v", cmd.Args)
		return supervisedexec.Result{}, nil
	})
	defer restore()
	logPath := filepath.Join(t.TempDir(), "agy.log")
	// GPT-OSS curated medium-only + effort low → model_unavailable, no exec.
	res, err := AntigravityRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(), Prompt: "x", LogPath: logPath,
		Model: "GPT-OSS 120B", Effort: "low",
	})
	if err == nil || res.FailureClass != "model_unavailable" {
		t.Fatalf("want model_unavailable, got err=%v res=%+v", err, res)
	}
	if execCalls != 0 {
		t.Fatalf("execCalls=%d", execCalls)
	}
	// Exact Medium token + effort low → mismatch, no exec.
	res, err = AntigravityRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(), Prompt: "x", LogPath: logPath,
		Model: "GPT-OSS 120B (Medium)", Effort: "low",
	})
	if err == nil || res.FailureClass != "model_unavailable" {
		t.Fatalf("want model_unavailable for token/effort mismatch, got err=%v res=%+v", err, res)
	}
	if execCalls != 0 {
		t.Fatalf("execCalls=%d after exact mismatch", execCalls)
	}
	// Slug medium + effort high → mismatch, no exec.
	res, err = AntigravityRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(), Prompt: "x", LogPath: logPath,
		Model: "gpt-oss-120b-medium", Effort: "high",
	})
	if err == nil || res.FailureClass != "model_unavailable" {
		t.Fatalf("want model_unavailable for slug mismatch, got err=%v res=%+v", err, res)
	}
	if execCalls != 0 {
		t.Fatalf("execCalls=%d after slug mismatch", execCalls)
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
		wantArgs := []string{"agy", "-p", prompt, "--add-dir", worktree, "--dangerously-skip-permissions", "--new-project", "--model", "Gemini 3.1 Pro (Medium)"}
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
		Effort:       "medium",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 0 || result.Summary != "plain text summary" || result.Model != "Gemini 3.1 Pro (Medium)" || result.Effort != "medium" {
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

func TestAntigravityHeadlessPermissionDenied(t *testing.T) {
	if !antigravityHeadlessPermissionDenied(
		`jetski: no output produced — a tool required the "read_file" permission that headless mode cannot prompt for, so it was auto-denied. Alternatively, re-run with --dangerously-skip-permissions to auto-approve all tools.`,
		"",
	) {
		t.Fatal("expected denial detection for jetski empty headless output")
	}
	if antigravityHeadlessPermissionDenied("Created NOTES.md with multi-provider notes.", "ok") {
		t.Fatal("useful write output must not be classified as permission denial")
	}
}

func TestAntigravityRunnerFailsClosedOnHeadlessPermissionDenial(t *testing.T) {
	worktree := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "antigravity.log")
	restore := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		msg := `jetski: no output produced — a tool required the "read_file" permission that headless mode cannot prompt for, so it was auto-denied. Alternatively, re-run with --dangerously-skip-permissions to auto-approve all tools.`
		_, _ = io.WriteString(cmd.Stdout, msg+"\n")
		_, _ = io.WriteString(cmd.Stderr, msg+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restore()

	result, err := AntigravityRunner{}.Run(context.Background(), Invocation{
		WorktreePath: worktree,
		Prompt:       "write notes",
		LogPath:      logPath,
		Model:        "Gemini 3.1 Pro",
		Effort:       "medium",
	})
	if err == nil {
		t.Fatal("Run returned nil error, want headless permission denial")
	}
	if !strings.Contains(err.Error(), "headless permission denial") {
		t.Fatalf("error = %v", err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("ExitCode = %d, want 1", result.ExitCode)
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
		Effort:       "medium",
		ReadOnly:     true,
	})
	if err == nil {
		t.Fatal("Run returned nil error, want read-only failure")
	}
	for _, want := range []string{
		`provider "antigravity" does not support read-only`,
		"choose a supporting provider: claude, codex, gemini",
		"use antigravity only for write-mode worker dispatch",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Run error = %v, want substring %q", err, want)
		}
	}
	if result.ExitCode != 1 || result.Model != "Gemini 3.1 Pro (Medium)" || result.Effort != "medium" {
		t.Fatalf("read-only result = %#v", result)
	}
}

func TestAntigravityRunnerMCPFailsClosedWithoutLaunchingAgy(t *testing.T) {
	restore := stubRunSupervised(t, func(context.Context, *exec.Cmd, supervisedexec.Options) (supervisedexec.Result, error) {
		t.Fatal("antigravity invocation with MCP should fail before launching agy")
		return supervisedexec.Result{}, nil
	})
	defer restore()

	_, err := AntigravityRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "use tools",
		LogPath:      filepath.Join(t.TempDir(), "antigravity.log"),
		MCPServers: []MCPServer{{
			Name:      "worker-index",
			Transport: "stdio",
			Command:   "./tools/worker-index",
			Roles:     []string{"worker"},
		}},
		Role: "worker",
	})
	if err == nil {
		t.Fatal("Run returned nil error, want MCP unsupported failure")
	}
	for _, want := range []string{
		"antigravity MCP configuration",
		`provider "antigravity" does not support mcp-config`,
		"remove MCP servers for this invocation",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Run error = %v, want substring %q", err, want)
		}
	}
}

func TestAntigravityRunnerOutputSchemaFailsClosedWithoutLaunchingAgy(t *testing.T) {
	restore := stubRunSupervised(t, func(context.Context, *exec.Cmd, supervisedexec.Options) (supervisedexec.Result, error) {
		t.Fatal("antigravity invocation with output schema should fail before launching agy")
		return supervisedexec.Result{}, nil
	})
	defer restore()

	_, err := AntigravityRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "return json",
		LogPath:      filepath.Join(t.TempDir(), "antigravity.log"),
		OutputSchema: `{"type":"object"}`,
	})
	if err == nil {
		t.Fatal("Run returned nil error, want output schema unsupported failure")
	}
	for _, want := range []string{
		"antigravity output schema",
		`provider "antigravity" does not support json-output`,
		"do not select antigravity for schema-enforced JSON verifier output",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Run error = %v, want substring %q", err, want)
		}
	}
}
