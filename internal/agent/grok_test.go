package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/pathid"
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
				"--permission-mode", "dontAsk",
				"--sandbox", "strict",
				"--allow", "Read",
				"--allow", "Grep",
				"--allow", "Edit(**)",
				"--deny", "Bash(*)",
				"--deny", "WebFetch(*)",
				"--deny", "MCPTool(*)",
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
				"--permission-mode", "dontAsk",
				"--sandbox", "read-only",
				"--allow", "Read",
				"--allow", "Grep",
				"--deny", "Edit(*)",
				"--deny", "Bash(*)",
				"--deny", "WebFetch(*)",
				"--deny", "MCPTool(*)",
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
		if !containsArgPair(cmd.Args, "--permission-mode", "dontAsk") {
			t.Fatalf("cmd.Args = %#v, want dontAsk permission mode", cmd.Args)
		}
		_, _ = io.WriteString(cmd.Stdout, `{"type":"system","session_id":"session-123","model":"grok-4.5"}`+"\n")
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","structured_output":{"verdict":"pass","evidence":"fixture"},"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10},"cost_usd":0.001}`+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	logPath := filepath.Join(t.TempDir(), "grok.log")
	result, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
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
	t.Setenv("LOOPCODER_SECRET_CANARY", "should-not-pass")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "should-not-pass")
	t.Setenv("XAI_API_KEY", "xai-runtime-test-value")
	hostileHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(hostileHome, ".grok", "hooks"), 0o755); err != nil {
		t.Fatalf("mkdir hostile home: %v", err)
	}
	t.Setenv("HOME", hostileHome)
	t.Setenv("USERPROFILE", hostileHome)
	worktree := t.TempDir()
	canonicalWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatalf("eval worktree: %v", err)
	}
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "grok.log")
	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		assertGrokCommandWorkspace(t, cmd, canonicalWorktree)
		if !containsArgPair(cmd.Args, "--sandbox", "strict") || !containsArgPair(cmd.Args, "--permission-mode", "dontAsk") {
			t.Fatalf("cmd.Args = %#v, want approved write workspace", cmd.Args)
		}
		if containsArg(cmd.Args, "--always-approve") {
			t.Fatalf("cmd.Args = %#v, must not include always-approve in write mode", cmd.Args)
		}
		for _, want := range []string{"Edit(**)", "Bash(*)", "WebFetch(*)", "MCPTool(*)"} {
			if !containsArg(cmd.Args, want) {
				t.Fatalf("cmd.Args = %#v, missing enforcement rule %q", cmd.Args, want)
			}
		}
		env := strings.Join(cmd.Env, "\n")
		for _, forbidden := range []string{"LOOPCODER_SECRET_CANARY", "AWS_SECRET_ACCESS_KEY"} {
			if strings.Contains(env, forbidden) {
				t.Fatalf("bounded env leaked %s in:\n%s", forbidden, env)
			}
		}
		if strings.Contains(env, hostileHome) {
			t.Fatalf("bounded env inherited hostile home path %q in:\n%s", hostileHome, env)
		}
		if !strings.Contains(env, "XAI_API_KEY=xai-runtime-test-value") {
			t.Fatalf("bounded env missing canonical XAI key:\n%s", env)
		}
		runtimeRoot := filepath.Join(logDir, "grok.grok-runtime")
		for _, want := range []string{
			"HOME=" + filepath.Join(runtimeRoot, "home"),
			"USERPROFILE=" + filepath.Join(runtimeRoot, "home"),
			"XDG_CONFIG_HOME=" + filepath.Join(runtimeRoot, "xdg-config"),
			"TMPDIR=" + filepath.Join(runtimeRoot, "tmp"),
		} {
			if !strings.Contains(env, want) {
				t.Fatalf("bounded env missing isolated root %q in:\n%s", want, env)
			}
		}
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"done"}`+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	result, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: worktree,
		Prompt:       "write",
		LogPath:      logPath,
		RunID:        "run-1",
		Role:         "worker",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Summary != "done" {
		t.Fatalf("Summary = %q, want done", result.Summary)
	}
	assertPrivateDirMode(t, filepath.Join(logDir, "grok.grok-runtime"))
	assertPrivateDirMode(t, filepath.Join(logDir, "grok.grok-runtime", "home"))
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
		{
			name: "missing write enforcement",
			probe: func(_ context.Context, argv []string, _ string, _ []string, _ time.Duration, _ int64) (grokProbeResult, error) {
				if reflect.DeepEqual(argv, []string{"grok", "version"}) {
					return grokProbeResult{Stdout: "grok 0.1.211\n"}, nil
				}
				return grokProbeResult{Stdout: "-p --cwd --output-format --no-auto-update --no-alt-screen --sandbox --allow --deny --permission-mode dontAsk workspace"}, nil
			},
			want: "missing required flags",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readOnly := tt.name != "missing write enforcement"
			_, err := (GrokRunner{probe: tt.probe}).Run(context.Background(), Invocation{
				WorktreePath: t.TempDir(),
				Prompt:       "inspect",
				ReadOnly:     readOnly,
				LogPath:      filepath.Join(t.TempDir(), "grok.log"),
			})
			assertGrokError(t, err, GrokErrUnsupportedCapability, tt.want)
		})
	}
}

func TestGrokRunnerRejectsInheritedProjectConfiguration(t *testing.T) {
	tests := []string{
		"CLAUDE.md",
		filepath.Join(".claude", "settings.json"),
		filepath.Join(".claude", "hooks"),
		filepath.Join(".claude", "agents"),
		filepath.Join(".claude", "plugins.json"),
		filepath.Join(".claude", "mcp.json"),
		filepath.Join(".grok", "memory"),
		".mcp.json",
	}
	for _, rel := range tests {
		t.Run(rel, func(t *testing.T) {
			worktree := t.TempDir()
			path := filepath.Join(worktree, rel)
			if strings.HasSuffix(rel, "hooks") || strings.HasSuffix(rel, "agents") || strings.HasSuffix(rel, "memory") {
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatalf("mkdir hostile config: %v", err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("mkdir hostile config parent: %v", err)
				}
				if err := os.WriteFile(path, []byte("hostile"), 0o644); err != nil {
					t.Fatalf("write hostile config: %v", err)
				}
			}

			_, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
				WorktreePath: worktree,
				Prompt:       "run",
				LogPath:      filepath.Join(t.TempDir(), "grok.log"),
				RunID:        "attempt",
				Role:         "worker",
			})
			assertGrokError(t, err, GrokErrUnsupportedCapability, "inherited configuration exists")
		})
	}
}

func TestGrokRunnerCanonicalizesWorkspaceAlias(t *testing.T) {
	root := t.TempDir()
	physical := filepath.Join(root, "physical", "repo")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatalf("mkdir physical: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "physical"), alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	wantWorkspace, err := filepath.EvalSymlinks(physical)
	if err != nil {
		t.Fatalf("eval physical: %v", err)
	}

	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		assertGrokCommandWorkspace(t, cmd, wantWorkspace)
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"done"}`+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	_, err = (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: filepath.Join(alias, "repo"),
		Prompt:       "run",
		LogPath:      filepath.Join(t.TempDir(), "grok.log"),
		RunID:        "attempt",
		Role:         "worker",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestGrokWorkspacePathIdentityAcceptsWindowsCaseOnlyNotWrongDirectory(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only path case contract")
	}
	root := t.TempDir()
	physical := filepath.Join(root, "Physical", "Repo")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatalf("mkdir physical: %v", err)
	}
	want, err := filepath.EvalSymlinks(physical)
	if err != nil {
		t.Fatalf("eval physical: %v", err)
	}
	caseOnly := strings.ToUpper(want)
	if caseOnly == want {
		caseOnly = strings.ToLower(want)
	}
	if caseOnly == want {
		t.Skipf("path has no alphabetic case to vary: %q", want)
	}
	ok, err := sameGrokPhysicalWorkspacePath(caseOnly, want)
	if err != nil {
		t.Fatalf("sameGrokPhysicalWorkspacePath case-only: %v", err)
	}
	if !ok {
		t.Fatalf("sameGrokPhysicalWorkspacePath(%q, %q) = false, want true for case-only normalization", caseOnly, want)
	}

	outside := filepath.Join(root, "Outside", "Repo")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	ok, err = sameGrokPhysicalWorkspacePath(outside, want)
	if err != nil {
		t.Fatalf("sameGrokPhysicalWorkspacePath outside: %v", err)
	}
	if ok {
		t.Fatalf("sameGrokPhysicalWorkspacePath(%q, %q) = true, want false for wrong directory", outside, want)
	}
}

func TestGrokRunnerRejectsWorkspaceSymlinkEscape(t *testing.T) {
	worktree := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside target: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(worktree, "escape")); err != nil {
		inside, pathErr := grokPathInsideWorkspace(worktree, outside)
		if pathErr != nil {
			t.Fatalf("grokPathInsideWorkspace fallback returned error after symlink unavailable (%v): %v", err, pathErr)
		}
		if inside {
			t.Fatalf("grokPathInsideWorkspace(%q, %q) = true after symlink unavailable (%v), want false", worktree, outside, err)
		}
		return
	}
	_, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: worktree,
		Prompt:       "run",
		LogPath:      filepath.Join(t.TempDir(), "grok.log"),
		RunID:        "attempt",
		Role:         "worker",
	})
	assertGrokError(t, err, GrokErrUnsupportedCapability, "symlink escapes")
}

func TestPrepareGrokRuntimeRootIsPerLogPathAndRejectsSymlink(t *testing.T) {
	logDir := t.TempDir()
	firstLog := filepath.Join(logDir, "job-1.log")
	secondLog := filepath.Join(logDir, "job-2.log")

	firstRoot, err := prepareGrokRuntimeRoot(firstLog)
	if err != nil {
		t.Fatalf("prepare first runtime root: %v", err)
	}
	secondRoot, err := prepareGrokRuntimeRoot(secondLog)
	if err != nil {
		t.Fatalf("prepare second runtime root: %v", err)
	}
	if firstRoot == secondRoot {
		t.Fatalf("runtime roots are shared: %q", firstRoot)
	}
	if firstRoot != filepath.Join(logDir, "job-1.grok-runtime") {
		t.Fatalf("first runtime root = %q, want log-file-specific root", firstRoot)
	}
	for _, rel := range []string{".", "home", "tmp", "xdg-config", "xdg-cache", "xdg-data"} {
		assertPrivateDirMode(t, filepath.Join(firstRoot, rel))
		assertPrivateDirMode(t, filepath.Join(secondRoot, rel))
	}

	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(logDir, "linked.grok-runtime")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := prepareGrokRuntimeRoot(filepath.Join(logDir, "linked.log")); err == nil {
		t.Fatal("prepareGrokRuntimeRoot accepted pre-created runtime-root symlink")
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
			restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
				return tt.run(cmd)
			})
			defer restoreRun()
			ctx := context.Background()
			if tt.ctx != nil {
				ctx = tt.ctx()
			}
			_, err := (GrokRunner{probe: supportedGrokProbe}).Run(ctx, Invocation{
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

func TestRedactGrokOutputAuthorizationCredentials(t *testing.T) {
	canaries := runtimeGrokSecretCanaries()
	jwt := canaries[0]
	basic := canaries[1]
	tests := []string{
		"Authorization: Bearer " + jwt,
		"authorization:\tBearer\t" + jwt,
		`"Authorization": "Bearer ` + jwt + `"`,
		`"authorization":"Basic ` + basic + `"`,
		"AUTHORIZATION='Custom " + jwt + "'",
		"authorization=" + jwt,
		"Bearer " + jwt,
		"Basic " + basic,
		grokRuntimeCredentialLabel() + jwt,
	}
	for _, input := range tests {
		t.Run(input[:min(len(input), 24)], func(t *testing.T) {
			redacted, changed := redactGrokOutput(input)
			if !changed {
				t.Fatalf("redactGrokOutput(%q) reported unchanged", input)
			}
			if !strings.Contains(redacted, "[REDACTED]") {
				t.Fatalf("redactGrokOutput(%q) = %q, missing redaction marker", input, redacted)
			}
			assertNoGrokSecretFragments(t, redacted, canaries...)
		})
	}

	benign := "authorization is discussed here without credential syntax"
	if redacted, changed := redactGrokOutput(benign); changed || redacted != benign {
		t.Fatalf("redactGrokOutput(%q) = %q, %v; want unchanged", benign, redacted, changed)
	}

	jsonValue := `{"value":"bEaReR ` + jwt + `"}`
	redacted, changed := redactGrokOutput(jsonValue)
	if !changed || !json.Valid([]byte(redacted)) {
		t.Fatalf("redactGrokOutput(%q) = %q, %v; want valid redacted JSON", jsonValue, redacted, changed)
	}
	assertNoGrokSecretFragments(t, redacted, canaries...)
}

func TestGrokRunnerRedactsBeforePersisting(t *testing.T) {
	xaiCanary := "xai-" + strings.Repeat("A", 24)
	canaries := append([]string{xaiCanary}, runtimeGrokSecretCanaries()...)
	jwt := canaries[1]
	basic := canaries[2]
	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"`+grokRuntimeCredentialLabel()+xaiCanary+` Authorization: Bearer `)
		_, _ = io.WriteString(cmd.Stdout, jwt+` Basic `+basic+`"}`+"\n")
		_, _ = io.WriteString(cmd.Stderr, "AuThOrIzAtIoN:\tBearer\t"+jwt+"\n")
		_, _ = io.WriteString(cmd.Stderr, "authorization='Basic "+basic+"'\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()
	logPath := filepath.Join(t.TempDir(), "grok.log")
	result, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "run",
		LogPath:      logPath,
		RunID:        "attempt",
		Role:         "worker",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	assertNoGrokSecretFragments(t, result.Summary, canaries...)
	logText := readFileString(t, logPath)
	assertNoGrokSecretFragments(t, logText, canaries...)
	if !strings.Contains(logText, "[REDACTED]") {
		t.Fatalf("log missing redaction marker:\n%s", logText)
	}
}

func TestGrokRunnerRedactsBeforeEveryCapBoundary(t *testing.T) {
	xaiCanary := "xai-" + strings.Repeat("B", 24)
	canaries := append([]string{xaiCanary}, runtimeGrokSecretCanaries()...)
	jwt := canaries[1]
	basic := canaries[2]

	var probe cappedProbeBuffer
	probe.cap = 40
	_, _ = probe.Write([]byte(strings.Repeat("p", 32) + " Authorization: Bearer " + jwt + " tail"))
	assertNoGrokSecretFragments(t, probe.String(), canaries...)

	var stderrProbe cappedProbeBuffer
	stderrProbe.cap = 40
	_, _ = stderrProbe.Write([]byte(strings.Repeat("q", 32) + " authorization=Basic " + basic + " tail"))
	assertNoGrokSecretFragments(t, stderrProbe.String(), canaries...)

	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		_, _ = io.WriteString(cmd.Stdout, strings.Repeat("o", grokMaxFrameBytes-8)+"Authorization: Bearer "+jwt+"\n")
		_, _ = io.WriteString(cmd.Stderr, strings.Repeat("e", grokMaxFrameBytes-8)+"Authorization: Basic "+basic+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	logPath := filepath.Join(t.TempDir(), "grok.log")
	_, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "run",
		LogPath:      logPath,
		RunID:        "attempt",
		Role:         "worker",
	})
	assertGrokError(t, err, GrokErrOutputFlood, "")
	assertNoGrokSecretFragments(t, readFileString(t, logPath), canaries...)
}

func TestGrokRunnerRedactsAuthorizationCredentialsFromMalformedFrame(t *testing.T) {
	canaries := runtimeGrokSecretCanaries()
	jwt := canaries[0]
	basic := canaries[1]
	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		_, _ = io.WriteString(cmd.Stdout, "not-json Authorization: Bearer "+jwt+"\n")
		_, _ = io.WriteString(cmd.Stderr, "authorization=Basic "+basic+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	logPath := filepath.Join(t.TempDir(), "grok.log")
	_, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "run",
		LogPath:      logPath,
		RunID:        "attempt",
		Role:         "worker",
	})
	assertGrokError(t, err, GrokErrMalformedFrame, "")
	assertNoGrokSecretFragments(t, readFileString(t, logPath), canaries...)
}

func TestGrokRunnerCancelsProviderOnTerminalStreamError(t *testing.T) {
	var sawCancel atomic.Bool
	restoreRun := stubRunSupervised(t, func(ctx context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		_, _ = io.WriteString(cmd.Stdout, "not-json\n")
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
			return supervisedexec.Result{Outcome: supervisedexec.OutcomeDeadline, Killed: true}, ctx.Err()
		case <-time.After(time.Second):
			return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
		}
	})
	defer restoreRun()

	_, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "run",
		LogPath:      filepath.Join(t.TempDir(), "grok.log"),
		RunID:        "attempt",
		Role:         "worker",
	})
	assertGrokError(t, err, GrokErrMalformedFrame, "")
	if !sawCancel.Load() {
		t.Fatal("provider context was not canceled after malformed frame")
	}
}

func TestGrokRunnerCapsStructuredSummaryAndRejectsMalformedCost(t *testing.T) {
	t.Run("structured summary cap", func(t *testing.T) {
		xaiCanary := "xai-" + strings.Repeat("C", 24)
		canaries := append([]string{xaiCanary}, runtimeGrokSecretCanaries()...)
		jwt := canaries[1]
		basic := canaries[2]
		restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
			huge := strings.Repeat("s", grokMaxSummaryBytes-6) + " Authorization: Bearer " + jwt + " Basic " + basic + " " + grokRuntimeCredentialLabel() + xaiCanary + strings.Repeat("z", 128)
			payload := `{"type":"result","model":"grok-4.5","structured_output":{"Authorization":"Bearer ` + jwt + `","value":"` + huge + `"}}`
			_, _ = io.WriteString(cmd.Stdout, payload+"\n")
			return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
		})
		defer restoreRun()

		logPath := filepath.Join(t.TempDir(), "grok.log")
		result, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
			WorktreePath: t.TempDir(),
			Prompt:       "run",
			LogPath:      logPath,
			RunID:        "attempt",
			Role:         "worker",
		})
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if len(result.Summary) > grokMaxSummaryBytes {
			t.Fatalf("summary length = %d, want <= %d", len(result.Summary), grokMaxSummaryBytes)
		}
		assertNoGrokSecretFragments(t, result.Summary, canaries...)
		assertNoGrokSecretFragments(t, readFileString(t, logPath), canaries...)
	})

	t.Run("malformed cost does not hide result", func(t *testing.T) {
		restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
			_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"done","cost_usd":"NaN"}`+"\n")
			return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
		})
		defer restoreRun()

		logPath := filepath.Join(t.TempDir(), "grok.log")
		result, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
			WorktreePath: t.TempDir(),
			Prompt:       "run",
			LogPath:      logPath,
			RunID:        "attempt",
			Role:         "worker",
		})
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
		if result.Summary != "done" {
			t.Fatalf("Summary = %q, want done", result.Summary)
		}
		if logText := readFileString(t, logPath); !strings.Contains(logText, `"gap_reasons":["malformed-cost-usd"]`) {
			t.Fatalf("log missing malformed cost gap:\n%s", logText)
		}
	})
}

func TestGrokRunnerNativeFakeProcessFixture(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableNameForTest("grok"))
	writeFakeGrokExecutable(t, exe)
	restoreCommand := stubGrokCommand(t, exe)
	defer restoreCommand()

	result, err := (GrokRunner{probe: supportedGrokProbeForAnyCommand}).Run(context.Background(), Invocation{
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

func TestGrokRunnerCancellationKillsNativeProcessTree(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableNameForTest("grok"))
	marker := filepath.Join(dir, "leaked-child.txt")
	started := filepath.Join(dir, "started.txt")
	writeCancellableGrokExecutable(t, exe, marker, started)
	restoreCommand := stubGrokCommand(t, exe)
	defer restoreCommand()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cancelAfterFileExists(started, cancel, 3*time.Second)
	_, err := (GrokRunner{probe: supportedGrokProbeForAnyCommand}).Run(ctx, Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "fixture",
		LogPath:      filepath.Join(t.TempDir(), "grok.log"),
		HardCap:      5 * time.Second,
		RunID:        "native-cancel-fixture",
		Role:         "worker",
	})
	assertGrokError(t, err, GrokErrCanceled, "")
	time.Sleep(1500 * time.Millisecond)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("process-tree child was not killed; marker stat err=%v", statErr)
	}
}

func TestGrokOrdinaryWorkerConformanceSuite(t *testing.T) {
	secretCanaries := append(runtimeGrokSecretCanaries(), "AKIA"+strings.Repeat("A", 16), "xai-"+strings.Repeat("Z", 24))
	hostileHome := t.TempDir()
	t.Setenv("HOME", hostileHome)
	t.Setenv("USERPROFILE", hostileHome)
	t.Setenv("AWS_ACCESS_KEY_ID", secretCanaries[2])
	t.Setenv("XAI_API_KEY", secretCanaries[3])

	var launchCount atomic.Int32
	var launchDirs []string
	var launchHomes []string
	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		launchCount.Add(1)
		launchDirs = append(launchDirs, cmd.Dir)
		launchHomes = append(launchHomes, envValue(cmd.Env, "HOME"))
		if strings.Contains(strings.Join(cmd.Env, "\n"), secretCanaries[2]) {
			t.Fatalf("non-Grok credential reached worker env: %s", strings.Join(cmd.Env, "\n"))
		}
		_, _ = io.WriteString(cmd.Stdout, `{"type":"system","session_id":"sess-conformance","model":"grok-4.5"}`+"\n")
		_, _ = io.WriteString(cmd.Stdout, `{"type":"assistant","model":"grok-4.5","text":"Authorization: Bearer `+secretCanaries[0]+`"}`+"\n")
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"done `+secretCanaries[2]+`","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	logRoot := t.TempDir()
	for _, project := range []string{"project-a", "project-b"} {
		t.Run(project, func(t *testing.T) {
			worktree := filepath.Join(t.TempDir(), project)
			if err := os.MkdirAll(worktree, 0o755); err != nil {
				t.Fatalf("mkdir worktree: %v", err)
			}
			promptCanary := "prompt-secret-" + project + "-" + strings.Repeat("p", 16)
			logPath := filepath.Join(logRoot, project+".log")
			result, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
				WorktreePath: worktree,
				Prompt:       "ordinary worker prompt " + promptCanary,
				LogPath:      logPath,
				RunID:        "run-" + project,
				ProviderKey:  "provider-key-" + project,
				Role:         "worker",
			})
			if err != nil {
				t.Fatalf("Run returned error: %v", err)
			}
			if !strings.Contains(result.Summary, "done [REDACTED]") || strings.Contains(result.Summary, secretCanaries[0]) || strings.Contains(result.Summary, secretCanaries[2]) || result.Model != "grok-4.5" {
				t.Fatalf("result = %#v, want redacted summary and parsed model", result)
			}
			logText := readFileString(t, logPath)
			for _, forbidden := range append(secretCanaries, promptCanary, hostileHome) {
				if strings.Contains(logText, forbidden) {
					t.Fatalf("log retained forbidden value %q:\n%s", forbidden, logText)
				}
			}
			if !strings.Contains(logText, `"kind":"progress"`) || !strings.Contains(logText, `"kind":"terminal"`) || !strings.Contains(logText, "[REDACTED]") {
				t.Fatalf("log missing normalized stream/redaction evidence:\n%s", logText)
			}
			assertPrivateDirMode(t, grokRuntimeRootPath(logPath))
		})
	}
	if launchCount.Load() != 2 {
		t.Fatalf("launch count = %d, want two isolated launches", launchCount.Load())
	}
	if len(launchDirs) != 2 || launchDirs[0] == launchDirs[1] {
		t.Fatalf("launch dirs = %#v, want distinct project workspaces", launchDirs)
	}
	if len(launchHomes) != 2 || launchHomes[0] == "" || launchHomes[1] == "" || launchHomes[0] == launchHomes[1] || strings.Contains(strings.Join(launchHomes, "\n"), hostileHome) {
		t.Fatalf("launch homes = %#v, want distinct isolated homes not inherited", launchHomes)
	}
}

func TestGrokOrdinaryWorkerRestartRecoveryConformance(t *testing.T) {
	var attempt atomic.Int32
	restoreRun := stubRunSupervised(t, func(_ context.Context, cmd *exec.Cmd, _ supervisedexec.Options) (supervisedexec.Result, error) {
		if attempt.Add(1) == 1 {
			_, _ = io.WriteString(cmd.Stdout, `{"type":"assistant","model":"grok-4.5","text":"partial"}`+"\n")
			return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
		}
		_, _ = io.WriteString(cmd.Stdout, `{"type":"result","model":"grok-4.5","result":"recovered"}`+"\n")
		return supervisedexec.Result{Outcome: supervisedexec.OutcomeCompleted, ExitCode: 0}, nil
	})
	defer restoreRun()

	worktree := t.TempDir()
	firstLog := filepath.Join(t.TempDir(), "first.log")
	_, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: worktree,
		Prompt:       "recover",
		LogPath:      firstLog,
		RunID:        "restart-recovery",
		ProviderKey:  "same-logical-operation",
		Role:         "worker",
	})
	assertGrokError(t, err, GrokErrTransportLoss, "terminal result")

	secondLog := filepath.Join(t.TempDir(), "second.log")
	result, err := (GrokRunner{probe: supportedGrokProbe}).Run(context.Background(), Invocation{
		WorktreePath: worktree,
		Prompt:       "recover",
		LogPath:      secondLog,
		RunID:        "restart-recovery",
		ProviderKey:  "same-logical-operation",
		Role:         "worker",
	})
	if err != nil {
		t.Fatalf("recovery Run returned error: %v", err)
	}
	if result.Summary != "recovered" || strings.Contains(readFileString(t, secondLog), "partial") {
		t.Fatalf("recovery result/log = %#v\n%s", result, readFileString(t, secondLog))
	}
}

func TestGrokLiveSmokeRequiresExplicitOptIn(t *testing.T) {
	if os.Getenv("LOOPCODER_GROK_LIVE_SMOKE") != "1" || os.Getenv("LOOPCODER_ALLOW_LIVE_PROVIDER") != "1" {
		t.Skip("live Grok smoke is disabled by default; set LOOPCODER_GROK_LIVE_SMOKE=1 and LOOPCODER_ALLOW_LIVE_PROVIDER=1 to opt in")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skipf("v0.8 native live Grok smoke is scoped to darwin/arm64, got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	logPath := filepath.Join(t.TempDir(), "grok-live.log")
	result, err := (GrokRunner{}).Run(context.Background(), Invocation{
		WorktreePath: t.TempDir(),
		Prompt:       "Return exactly: loopcoder grok live smoke",
		LogPath:      logPath,
		ReadOnly:     true,
		HardCap:      30 * time.Second,
		RunID:        "live-smoke",
		Role:         "verifier",
	})
	if err != nil {
		t.Fatalf("live Grok smoke failed: %v", err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Model) == "" {
		t.Fatalf("live Grok smoke result = %#v, want successful parsed model", result)
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
	return "-p --cwd --output-format --no-auto-update --no-alt-screen --sandbox strict read-only --permission-mode dontAsk --allow --deny"
}

func supportedGrokProbeForAnyCommand(_ context.Context, argv []string, _ string, _ []string, _ time.Duration, _ int64) (grokProbeResult, error) {
	if len(argv) == 2 && argv[1] == "version" {
		return grokProbeResult{Stdout: "grok 0.1.211\n"}, nil
	}
	if len(argv) == 2 && argv[1] == "--help" {
		return grokProbeResult{Stdout: supportedGrokHelp()}, nil
	}
	return grokProbeResult{ExitCode: 2, Stderr: "unexpected argv"}, nil
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

func argValue(args []string, key string) (string, bool) {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == key {
			return args[index+1], true
		}
	}
	return "", false
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func assertGrokCommandWorkspace(t *testing.T, cmd *exec.Cmd, want string) {
	t.Helper()
	cwd, ok := argValue(cmd.Args, "--cwd")
	if !ok {
		t.Fatalf("cmd.Args = %#v, missing --cwd", cmd.Args)
	}
	if cmd.Dir != cwd {
		t.Fatalf("cmd.Dir = %q, --cwd = %q; want exact same launch workspace", cmd.Dir, cwd)
	}
	same, err := sameGrokPhysicalWorkspacePath(cmd.Dir, want)
	if err != nil {
		t.Fatalf("compare Grok workspace path %q to %q: %v", cmd.Dir, want, err)
	}
	if !same {
		t.Fatalf("cmd.Dir = %q, want physical workspace %q", cmd.Dir, want)
	}
}

func sameGrokPhysicalWorkspacePath(got, want string) (bool, error) {
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		return false, err
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		return false, err
	}
	if !samePathString(got, gotResolved) {
		return false, nil
	}
	gotID, err := pathid.Identity(got)
	if err != nil {
		return false, err
	}
	wantID, err := pathid.Identity(wantResolved)
	if err != nil {
		return false, err
	}
	return gotID == wantID, nil
}

func samePathString(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func runtimeGrokSecretCanaries() []string {
	jwt := strings.Join([]string{
		base64.RawURLEncoding.EncodeToString([]byte("lc834-header")),
		base64.RawURLEncoding.EncodeToString([]byte("lc834-payload-middle")),
		base64.RawURLEncoding.EncodeToString([]byte("lc834-signature-suffix")),
	}, ".")
	basic := base64.StdEncoding.EncodeToString([]byte("lc834-basic-user:lc834-basic-password-suffix"))
	return []string{jwt, basic}
}

func grokRuntimeCredentialLabel() string {
	return strings.Join([]string{"api", "_", "key", "="}, "")
}

func assertNoGrokSecretFragments(t *testing.T, value string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		fragments := []string{secret}
		if len(secret) >= 24 {
			fragments = append(fragments,
				secret[:12],
				secret[len(secret)/2-6:len(secret)/2+6],
				secret[len(secret)-12:],
			)
		}
		for _, forbidden := range fragments {
			if strings.Contains(value, forbidden) {
				t.Fatalf("value retained secret fragment %q:\n%s", forbidden, value)
			}
		}
	}
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

func stubGrokCommand(t *testing.T, path string) func() {
	t.Helper()
	previous := grokCommand
	grokCommand = path
	return func() {
		grokCommand = previous
	}
}

func writeFakeGrokExecutable(t *testing.T, path string) {
	t.Helper()
	if os.PathSeparator == '\\' {
		script := "@echo off\r\n" +
			"if \"%1\"==\"version\" echo grok 0.1.211&& exit /b 0\r\n" +
			"if \"%1\"==\"--help\" echo -p --cwd --output-format --no-auto-update --no-alt-screen --sandbox strict read-only --permission-mode dontAsk --allow --deny&& exit /b 0\r\n" +
			"echo {\"type\":\"result\",\"model\":\"grok-fixture\",\"result\":\"native fixture\"}\r\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatalf("write fake grok: %v", err)
		}
		return
	}
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) echo 'grok 0.1.211'; exit 0 ;;\n" +
		"  --help) echo '-p --cwd --output-format --no-auto-update --no-alt-screen --sandbox strict read-only --permission-mode dontAsk --allow --deny'; exit 0 ;;\n" +
		"esac\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"model\":\"grok-fixture\",\"result\":\"native fixture\"}'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake grok: %v", err)
	}
}

func cancelAfterFileExists(path string, cancel context.CancelFunc, timeout time.Duration) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			time.Sleep(100 * time.Millisecond)
			cancel()
			return
		}
		select {
		case <-deadline:
			cancel()
			return
		case <-ticker.C:
		}
	}
}

func writeCancellableGrokExecutable(t *testing.T, path, marker, started string) {
	t.Helper()
	if os.PathSeparator == '\\' {
		quotedMarker := strings.ReplaceAll(marker, "'", "''")
		quotedStarted := strings.ReplaceAll(started, "'", "''")
		script := "@echo off\r\n" +
			"if \"%1\"==\"version\" echo grok 0.1.211&& exit /b 0\r\n" +
			"if \"%1\"==\"--help\" echo -p --cwd --output-format --no-auto-update --no-alt-screen --sandbox strict read-only --permission-mode dontAsk --allow --deny&& exit /b 0\r\n" +
			"powershell -NoProfile -Command \"Set-Content -LiteralPath '" + quotedStarted + "' -Value started\"\r\n" +
			"start \"\" /b powershell -NoProfile -Command \"Start-Sleep -Milliseconds 1200; Set-Content -LiteralPath '" + quotedMarker + "' -Value leaked\"\r\n" +
			"ping -n 30 127.0.0.1 >nul\r\n"
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatalf("write cancellable grok: %v", err)
		}
		return
	}
	quotedMarker := strings.ReplaceAll(marker, "'", "'\\''")
	quotedStarted := strings.ReplaceAll(started, "'", "'\\''")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) echo 'grok 0.1.211'; exit 0 ;;\n" +
		"  --help) echo '-p --cwd --output-format --no-auto-update --no-alt-screen --sandbox strict read-only --permission-mode dontAsk --allow --deny'; exit 0 ;;\n" +
		"esac\n" +
		"printf started > '" + quotedStarted + "'\n" +
		"( sleep 1.2; printf leaked > '" + quotedMarker + "' ) &\n" +
		"while :; do sleep 10; done\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write cancellable grok: %v", err)
	}
}
