package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

func TestGrokRegistration(t *testing.T) {
	runner, err := Lookup("grok")
	if err != nil {
		t.Fatalf("Lookup returned error: %v", err)
	}
	if runner == nil {
		t.Fatal("Lookup returned nil runner")
	}
	if !slices.Contains(SupportedProviders(), "grok") {
		t.Fatalf("SupportedProviders() = %#v, want grok registered", SupportedProviders())
	}
}

func TestBuildGrokArgs(t *testing.T) {
	tests := []struct {
		name string
		inv  Invocation
		want []string
	}{
		{
			name: "write argv",
			inv: Invocation{
				Prompt:       "do work; rm -rf /",
				WorktreePath: "/tmp/work tree",
			},
			want: []string{
				"--no-auto-update",
				"-p", "do work; rm -rf /",
				"--cwd", "/tmp/work tree",
				"--output-format", "streaming-json",
				"--no-alt-screen",
				"--disable-web-search",
				"--no-subagents",
				"--no-memory",
				"--always-approve",
			},
		},
		{
			name: "read only argv",
			inv: Invocation{
				Prompt:       "inspect",
				WorktreePath: "/tmp/repo",
				ReadOnly:     true,
				Model:        "grok-4.5",
				Effort:       "high",
			},
			want: []string{
				"--no-auto-update",
				"-p", "inspect",
				"--cwd", "/tmp/repo",
				"--output-format", "streaming-json",
				"--no-alt-screen",
				"--disable-web-search",
				"--no-subagents",
				"--no-memory",
				"--sandbox", "read-only",
				"--deny", "write:*",
				"--deny", "shell:*",
				"--disallowed-tools", "write,edit,shell,terminal",
				"-m", "grok-4.5",
				"--effort", "high",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildGrokArgs(tt.inv)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildGrokArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestGrokRunnerReadOnlyStreamingSuccess(t *testing.T) {
	restoreProbe := stubGrokProbe(t, supportedGrokProbe)
	defer restoreProbe()
	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, opts supervisedexec.Options) (supervisedexec.Result, error) {
		if opts.HardCap != 123*time.Millisecond {
			t.Fatalf("HardCap = %s, want 123ms", opts.HardCap)
		}
		if cmd.Dir == "" {
			t.Fatal("cmd.Dir was empty")
		}
		if !containsArgPair(cmd.Args, "--sandbox", "read-only") {
			t.Fatalf("cmd.Args = %#v, want read-only sandbox", cmd.Args)
		}
		if containsArg(cmd.Args, "--always-approve") {
			t.Fatalf("cmd.Args = %#v, must not auto-approve in read-only mode", cmd.Args)
		}
		_, _ = io.WriteString(cmd.Stdout, `{"type":"system","session_id":"session-123","model":"grok-4.5"}`+"\n")
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","structured_output":{"verdict":"pass","evidence":"fixture"},"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10},"cost_usd":0.001}`+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	logPath := filepath.Join(t.TempDir(), "grok.log")
	result, err := GrokRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "inspect",
		ReadOnly:     true,
		LogPath:      logPath,
		HardCap:      123 * time.Millisecond,
		RunID:        "attempt-1",
		Role:         "verifier",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.ExitCode != 0 || result.Summary != `{"evidence":"fixture","verdict":"pass"}` {
		t.Fatalf("result = %#v, want deterministic structured summary", result)
	}
	if result.Model != "grok-4.5" || result.AdapterVersion != "0.1.211" || result.ExternalSessionRef != "session-123" {
		t.Fatalf("metadata = model %q adapter %q session %q", result.Model, result.AdapterVersion, result.ExternalSessionRef)
	}
	assertInt64Ptr(t, result.Usage.InputTokens, 7)
	assertInt64Ptr(t, result.Usage.OutputTokens, 3)
	assertInt64Ptr(t, result.Usage.TotalTokens, 10)
	assertPrivateFileMode(t, logPath)
	logText := readFileString(t, logPath)
	for _, want := range []string{`"kind":"start"`, `"permission_mode":"read-only"`, `"external_session_ref":"session-123"`, `"cost_usd":0.001`} {
		if !strings.Contains(logText, want) {
			t.Fatalf("log missing %q:\n%s", want, logText)
		}
	}
}

func TestGrokRunnerWriteModeReceivesApprovedWorkspaceAndBoundedEnv(t *testing.T) {
	restoreProbe := stubGrokProbe(t, supportedGrokProbe)
	defer restoreProbe()
	t.Setenv("LOOPCODER_SECRET_CANARY", "should-not-pass")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "should-not-pass")
	t.Setenv("XAI_API_KEY", "xai-runtime-test-value")
	worktree := t.TempDir()
	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		if cmd.Dir != worktree {
			t.Fatalf("cmd.Dir = %q, want %q", cmd.Dir, worktree)
		}
		if !containsArgPair(cmd.Args, "--cwd", worktree) || !containsArg(cmd.Args, "--always-approve") {
			t.Fatalf("cmd.Args = %#v, want approved write workspace", cmd.Args)
		}
		if containsArg(cmd.Args, "--sandbox") {
			t.Fatalf("cmd.Args = %#v, must not include read-only sandbox in write mode", cmd.Args)
		}
		env := strings.Join(cmd.Env, "\n")
		for _, forbidden := range []string{"LOOPCODER_SECRET_CANARY", "AWS_SECRET_ACCESS_KEY"} {
			if strings.Contains(env, forbidden) {
				t.Fatalf("bounded env leaked %s in:\n%s", forbidden, env)
			}
		}
		if !strings.Contains(env, "XAI_API_KEY=xai-runtime-test-value") {
			t.Fatalf("bounded env missing canonical XAI key:\n%s", env)
		}
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"done"}`+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	result, err := GrokRunner{}.Run(context.Background(), Invocation{
		WorktreePath: worktree,
		Prompt:       "write",
		LogPath:      filepath.Join(t.TempDir(), "grok.log"),
		RunID:        "run-1",
		Role:         "worker",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("Summary = %q, want done", result.Summary)
	}
}

func TestGrokRunnerCapabilityNegotiationFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		probe func(context.Context, []string, string, []string, time.Duration, int64) (grokProbeResult, error)
		want  string
	}{
		{
			name: "old version",
			probe: func(_ context.Context, argv []string, _ string, _ []string, _ time.Duration, _ int64) (grokProbeResult, error) {
				if reflect.DeepEqual(argv, []string{"grok", "version"}) {
					return grokProbeResult{Stdout: "grok 0.0.9\n"}, nil
				}
				return grokProbeResult{Stdout: supportedGrokHelp()}, nil
			},
			want: "does not support bounded headless execution",
		},
		{
			name: "missing read-only enforcement",
			probe: func(_ context.Context, argv []string, _ string, _ []string, _ time.Duration, _ int64) (grokProbeResult, error) {
				if reflect.DeepEqual(argv, []string{"grok", "version"}) {
					return grokProbeResult{Stdout: "grok 0.1.211\n"}, nil
				}
				return grokProbeResult{Stdout: "-p --cwd --output-format --no-auto-update --no-alt-screen"}, nil
			},
			want: "missing required flags",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restoreProbe := stubGrokProbe(t, tt.probe)
			defer restoreProbe()
			_, err := GrokRunner{}.Run(context.Background(), Invocation{
				WorktreePath: t.TempDir(),
				Prompt:       "inspect",
				ReadOnly:     true,
				LogPath:      filepath.Join(t.TempDir(), "grok.log"),
			})
			assertGrokError(t, err, GrokErrUnsupportedCapability, tt.want)
		})
	}
}

func TestGrokRunnerTypedFailures(t *testing.T) {
	tests := []struct {
		name     string
		run      func(*exec.Cmd) (supervisedexec.Result, error)
		wantCode GrokErrorCode
		ctx      func() context.Context
	}{
		{
			name: "malformed frame",
			run: func(cmd *exec.Cmd) (supervisedexec.Result, error) {
				_, _ = io.WriteString(cmd.Stdout, "not-json\n")
				return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
			},
			wantCode: GrokErrMalformedFrame,
		},
		{
			name: "output flood",
			run: func(cmd *exec.Cmd) (supervisedexec.Result, error) {
				_, _ = io.WriteString(cmd.Stdout, strings.Repeat("x", grokMaxFrameBytes+1)+"\n")
				return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
			},
			wantCode: GrokErrOutputFlood,
		},
		{
			name: "timeout",
			run: func(_ *exec.Cmd) (supervisedexec.Result, error) {
				return supervisedexec.Result{Outcome: supervisedexec.OutcomeDeadline, Killed: true}, nil
			},
			wantCode: GrokErrTimeout,
		},
		{
			name: "transport loss",
			run: func(cmd *exec.Cmd) (supervisedexec.Result, error) {
				_, _ = io.WriteString(cmd.Stdout, `{"type":"assistant","model":"grok-4.5","text":"still working"}`+"\n")
				return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
			},
			wantCode: GrokErrTransportLoss,
		},
		{
			name: "nonzero exit",
			run: func(cmd *exec.Cmd) (supervisedexec.Result, error) {
				_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"failed"}`+"\n")
				return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 7}, nil
			},
			wantCode: GrokErrNonzeroExit,
		},
		{
			name: "provider error",
			run: func(cmd *exec.Cmd) (supervisedexec.Result, error) {
				_, _ = io.WriteString(cmd.Stdout, `{"type":"error","model":"grok-4.5","error":{"code":"auth","message":"not authenticated"}}`+"\n")
				return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
			},
			wantCode: GrokErrProviderError,
		},
		{
			name: "process tree cancellation",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			run: func(_ *exec.Cmd) (supervisedexec.Result, error) {
				return supervisedexec.Result{Outcome: supervisedexec.OutcomeDeadline, Killed: true}, context.Canceled
			},
			wantCode: GrokErrCanceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restoreProbe := stubGrokProbe(t, supportedGrokProbe)
			defer restoreProbe()
			restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
				return tt.run(cmd)
			})
			defer restoreRun()
			ctx := context.Background()
			if tt.ctx != nil {
				ctx = tt.ctx()
			}
			_, err := GrokRunner{}.Run(ctx, Invocation{
				WorktreePath: t.TempDir(),
				Prompt:       "run",
				LogPath:      filepath.Join(t.TempDir(), "grok.log"),
				RunID:        "attempt",
				Role:         "worker",
			})
			assertGrokError(t, err, tt.wantCode, "")
			if tt.wantCode == GrokErrCanceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled in chain", err)
			}
		})
	}
}

func TestGrokRunnerRedactsBeforePersisting(t *testing.T) {
	restoreProbe := stubGrokProbe(t, supportedGrokProbe)
	defer restoreProbe()
	canary := "xai-" + strings.Repeat("A", 24)
	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"api_key=`+canary+` Bearer `+canary+`"}`+"\n")
		_, _ = io.WriteString(cmd.Stderr, "Authorization: Bearer "+canary+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()
	logPath := filepath.Join(t.TempDir(), "grok.log")
	result, err := GrokRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "run",
		LogPath:      logPath,
		RunID:        "attempt",
		Role:         "worker",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if strings.Contains(result.Summary, canary) {
		t.Fatalf("summary retained canary: %q", result.Summary)
	}
	logText := readFileString(t, logPath)
	if strings.Contains(logText, canary) {
		t.Fatalf("log retained canary:\n%s", logText)
	}
	if !strings.Contains(logText, "[REDACTED]") {
		t.Fatalf("log missing redaction marker:\n%s", logText)
	}
}

func TestGrokRunnerNativeFakeProcessFixture(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableNameForTest("grok"))
	writeFakeGrokExecutable(t, exe)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	result, err := GrokRunner{}.Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "fixture",
		LogPath:      filepath.Join(t.TempDir(), "grok.log"),
		RunID:        "native-fixture",
		Role:         "worker",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "native fixture" || result.Model != "grok-fixture" {
		t.Fatalf("result = %#v, want native fixture summary/model", result)
	}
}

func supportedGrokProbe(_ context.Context, argv []string, _ string, _ []string, _ time.Duration, _ int64) (grokProbeResult, error) {
	if reflect.DeepEqual(argv, []string{"grok", "version"}) {
		return grokProbeResult{Stdout: "grok 0.1.211\n"}, nil
	}
	if reflect.DeepEqual(argv, []string{"grok", "--help"}) {
		return grokProbeResult{Stdout: supportedGrokHelp()}, nil
	}
	return grokProbeResult{ExitCode: 2, Stderr: "unexpected argv"}, nil
}

func supportedGrokHelp() string {
	return "-p --cwd --output-format --no-auto-update --no-alt-screen --sandbox --deny --disallowed-tools --always-approve"
}

func stubGrokProbe(t *testing.T, fn func(context.Context, []string, string, []string, time.Duration, int64) (grokProbeResult, error)) func() {
	t.Helper()
	original := runGrokProbe
	runGrokProbe = fn
	return func() {
		runGrokProbe = original
	}
}

func assertGrokError(t *testing.T, err error, code GrokErrorCode, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want GrokError code %s", code)
	}
	var grokErr *GrokError
	if !errors.As(err, &grokErr) {
		t.Fatalf("error = %T %v, want *GrokError", err, err)
	}
	if grokErr.Code != code {
		t.Fatalf("GrokError.Code = %s, want %s (err=%v)", grokErr.Code, code, err)
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Fatalf("error = %q, want substring %q", err.Error(), contains)
	}
}

func containsArg(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, key, value string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key && args[index+1] == value {
			return true
		}
	}
	return false
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func executableNameForTest(name string) string {
	if strings.EqualFold(filepath.Ext(name), ".exe") {
		return name
	}
	if os.PathSeparator == '\\' {
		return name + ".bat"
	}
	return name
}

func writeFakeGrokExecutable(t *testing.T, path string) {
	t.Helper()
	if os.PathSeparator == '\\' {
		script := "@echo off\r\n" +
			"if \"%1\"==\"version\" echo grok 0.1.211&& exit /b 0\r\n" +
			"if \"%1\"==\"--help\" echo -p --cwd --output-format --no-auto-update --no-alt-screen --sandbox --deny --disallowed-tools --always-approve&& exit /b 0\r\n" +
			"echo {\"type\":\"result\",\"model\":\"grok-fixture\",\"result\":\"native fixture\"}\r\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatalf("write fake grok: %v", err)
		}
		return
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) echo 'grok 0.1.211'; exit 0 ;;\n" +
		"  --help) echo '-p --cwd --output-format --no-auto-update --no-alt-screen --sandbox --deny --disallowed-tools --always-approve'; exit 0 ;;\n" +
		"esac\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"model\":\"grok-fixture\",\"result\":\"native fixture\"}'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake grok: %v", err)
	}
}
