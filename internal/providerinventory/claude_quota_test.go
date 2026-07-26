package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestClaudeQuotaCollectionRequiresExplicitGrantAndDoesNotLaunch(t *testing.T) {
	exe := writeFakeClaude(t)
	deps := claudeQuotaDeps(t, exe, "claude 1.2.3", ClaudePTYResult{}, nil)
	called := false
	deps.RunClaudePTY = func(context.Context, ClaudePTYRequest) (ClaudePTYResult, error) {
		called = true
		return ClaudePTYResult{}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "claude"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if called {
		t.Fatal("RunClaudePTY was called without quota telemetry grant")
	}
	snapshot := onlyQuotaSnapshot(t, report, "claude")
	if snapshot.TerminalErrorCode != "ErrQuotaCollectionGrantRequired" || !containsString(snapshot.GapReasons, "quota-collection-not-granted") {
		t.Fatalf("claude quota snapshot = %#v, want grant-required unavailable", snapshot)
	}
}

func TestClaudeQuotaCollectsCurrentSessionAndWeeklyWindows(t *testing.T) {
	exe := writeFakeClaude(t)
	now := fixedInventoryNow()
	currentReset := now.Add(5 * time.Hour)
	weeklyReset := now.Add(7 * 24 * time.Hour)
	output := "\x1b[1mClaude Code Usage\x1b[0m\n" +
		"Current session: 25% used resets at " + currentReset.Format(time.RFC3339) + "\n" +
		"Weekly: 80% used resets at " + weeklyReset.Format(time.RFC3339) + "\n"
	var gotReq ClaudePTYRequest
	deps := claudeQuotaDeps(t, exe, "claude 1.2.3", ClaudePTYResult{Output: output, ExitCode: 0}, nil)
	deps.Getenv = func(key string) string {
		switch key {
		case "PATH":
			return filepath.Dir(exe)
		case "HOME":
			return "/home/original"
		case "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "GIT_DIR", "GIT_WORK_TREE":
			return "secret-or-repo-state-should-not-cross"
		case "LANG":
			return "C"
		default:
			return ""
		}
	}
	deps.RunClaudePTY = func(_ context.Context, req ClaudePTYRequest) (ClaudePTYResult, error) {
		gotReq = req
		if req.Cwd == "" || strings.Contains(req.Cwd, "wt") {
			t.Fatalf("claude cwd = %q, want neutral temp outside worktree", req.Cwd)
		}
		if req.Input != "1\n/usage\n/exit\n" {
			t.Fatalf("claude input = %q", req.Input)
		}
		if req.Columns != claudeQuotaColumns || req.Rows != claudeQuotaRows {
			t.Fatalf("terminal size = %dx%d", req.Columns, req.Rows)
		}
		for _, arg := range req.Argv {
			lower := strings.ToLower(arg)
			if strings.Contains(lower, "login") || strings.Contains(lower, "update") {
				t.Fatalf("unsafe claude quota argv: %#v", req.Argv)
			}
		}
		for _, entry := range req.Env {
			lower := strings.ToLower(entry)
			if strings.Contains(entry, "secret-or-repo-state-should-not-cross") || strings.HasPrefix(lower, "git_") || strings.Contains(lower, "token") || strings.Contains(lower, "api_key") {
				t.Fatalf("credential or git env reached claude PTY: %q", entry)
			}
		}
		return ClaudePTYResult{Output: output, ExitCode: 0}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "claude"}},
		Now:    fixedInventoryNow,
		NetworkGrants: []NetworkGrant{{
			ProviderID: "claude",
			Purpose:    NetworkPurposeQuotaTelemetry,
			Scope:      NetworkScopeMachineInventory,
		}},
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(gotReq.Argv) == 0 || gotReq.Argv[0] != exe {
		t.Fatalf("claude argv = %#v, want executable %q", gotReq.Argv, exe)
	}
	if !containsString(gotReq.Argv, "--disallowedTools") || !containsString(gotReq.Argv, "--strict-mcp-config") {
		t.Fatalf("claude argv missing no-tools/MCP hardening: %#v", gotReq.Argv)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal report: %v", err)
	}
	if strings.Contains(string(data), output) || strings.Contains(string(data), "secret-or-repo-state-should-not-cross") {
		t.Fatalf("report retained raw output or canary: %s", data)
	}
	sources := quotaSourcesFor(report, "claude")
	if len(sources) != 1 {
		t.Fatalf("claude quota sources = %d: %#v", len(sources), report.QuotaTelemetrySources)
	}
	snapshots := quotaSnapshotsFor(report, "claude")
	if len(snapshots) != 2 {
		t.Fatalf("claude quota snapshots = %d: %#v", len(snapshots), snapshots)
	}
	for _, snapshot := range snapshots {
		if err := ValidateQuotaSnapshot(sources[0], snapshot); err != nil {
			t.Fatalf("ValidateQuotaSnapshot(%s): %v\n%#v", snapshot.ProviderQuantityName, err, snapshot)
		}
		if !strings.Contains(snapshot.RedactedDiagnostics, "terminal width 100") || !strings.Contains(snapshot.RedactedDiagnostics, "ansi true") {
			t.Fatalf("diagnostics missing provenance: %#v", snapshot)
		}
	}
	current := quotaSnapshotByKey(t, report, "current-session_used_percent", WindowProviderDefined, "provider:claude/scope:current-session")
	if current.UsedValue == nil || *current.UsedValue != 25 || current.RemainingValue == nil || *current.RemainingValue != 75 || current.ResetAt != formatTime(currentReset) {
		t.Fatalf("current session snapshot = %#v", current)
	}
	weekly := quotaSnapshotByKey(t, report, "weekly_used_percent", WindowFixedWeek, "provider:claude/scope:weekly")
	if weekly.WindowStart != formatTime(weeklyReset.Add(-7*24*time.Hour)) || weekly.WindowEnd != formatTime(weeklyReset) || weekly.ResetSemantics != ResetWindowBoundary {
		t.Fatalf("weekly fixed fields = %#v", weekly)
	}

	storePath := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(context.Background(), storage.Options{Path: storePath, Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	if err := Refresh(context.Background(), store, report, fixedInventoryNow()); err != nil {
		store.Close()
		t.Fatalf("Refresh: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close store: %v", err)
	}
	reopened, err := storage.Open(context.Background(), storage.Options{Path: storePath, Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open reopened: %v", err)
	}
	defer reopened.Close()
	loaded, err := Load(context.Background(), reopened)
	if err != nil {
		t.Fatalf("Load reopened: %v", err)
	}
	loadedWeekly := quotaSnapshotByKey(t, loaded, "weekly_used_percent", WindowFixedWeek, "provider:claude/scope:weekly")
	if loadedWeekly.WindowStart != formatTime(weeklyReset.Add(-7*24*time.Hour)) || loadedWeekly.WindowEnd != formatTime(weeklyReset) {
		t.Fatalf("loaded weekly fields = %#v", loadedWeekly)
	}
}

func TestClaudeUsageParserVariationsAndDrift(t *testing.T) {
	now := fixedInventoryNow()
	reset := now.Add(7 * 24 * time.Hour)
	cases := []struct {
		name    string
		output  string
		wantErr error
	}{
		{name: "reordered-lines", output: "Usage\nWeekly limit: 70% used resets at " + reset.Format(time.RFC3339) + "\nCurrent session: 20% used resets at " + now.Add(time.Hour).Format(time.RFC3339) + "\n"},
		{name: "wide-date-layout", output: "Claude Code Usage\nWeek: 60% used resets on " + reset.Format("Jan 2, 2006 3:04 PM") + "\n"},
		{name: "missing-reset", output: "Claude Code Usage\nWeekly: 60% used\n", wantErr: ErrClaudeQuotaMalformed},
		{name: "localized-drift", output: "Uso de Claude\nSemanal: 60% usado reinicia " + reset.Format(time.RFC3339) + "\n", wantErr: ErrClaudeQuotaMalformed},
		{name: "malformed-percent", output: "Claude Code Usage\nWeekly: 120% used resets at " + reset.Format(time.RFC3339) + "\n", wantErr: ErrClaudeQuotaMalformed},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			surface, err := parseClaudeUsageSurface(tt.output, "claude 1.2.3", "C", 80, now)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("parse err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(surface.Rows) == 0 || surface.CLIVersion != "claude 1.2.3" || surface.Locale != "C" || surface.Width != 80 {
				t.Fatalf("surface = %#v", surface)
			}
		})
	}
}

func TestClaudeQuotaFailureFixturesLeaveUnavailableSnapshot(t *testing.T) {
	exe := writeFakeClaude(t)
	cases := []struct {
		name     string
		version  string
		result   ClaudePTYResult
		err      error
		wantCode string
		wantGap  string
	}{
		{name: "malformed", result: ClaudePTYResult{Output: "Claude Code Usage\nWeekly unavailable\n", ExitCode: 0}, wantCode: "ErrClaudeQuotaMalformedSurface", wantGap: "unsupported-usage-surface"},
		{name: "truncated", result: ClaudePTYResult{Output: "Claude Code Usage\n", ExitCode: 0, Truncated: true}, wantCode: "ErrClaudeQuotaOutputTruncated", wantGap: "quota-output-truncated"},
		{name: "credential", result: ClaudePTYResult{Output: "Claude Code Usage\napi_key=sk-" + strings.Repeat("A", 24), ExitCode: 0}, wantCode: "ErrQuotaCredentialMaterial", wantGap: "credential-material-redacted"},
		{name: "timeout", result: ClaudePTYResult{TimedOut: true, Killed: true, ExitCode: -1}, wantCode: "ErrClaudeQuotaTimeout", wantGap: "quota-probe-timeout"},
		{name: "workspace-trust-prompt", result: ClaudePTYResult{Output: "Do you trust the files in this workspace?", TimedOut: true, Killed: true, ExitCode: -1}, wantCode: "ErrClaudeQuotaWorkspaceTrustPrompt", wantGap: "quota-workspace-trust-prompt"},
		{name: "nonzero", result: ClaudePTYResult{ExitCode: 2}, wantCode: "ErrClaudeQuotaNonZeroExit", wantGap: "quota-probe-nonzero-exit"},
		{name: "transport", result: ClaudePTYResult{ExitCode: -1}, err: errors.New("pty closed"), wantCode: "ErrClaudeQuotaExecutionFailed", wantGap: "quota-probe-failed"},
		{name: "old-version", version: "claude 0.9.9", result: ClaudePTYResult{ExitCode: 0}, wantCode: "ErrUnsupportedVersion", wantGap: "unsupported-cli-version"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			version := firstNonEmpty(tt.version, "claude 1.2.3")
			deps := claudeQuotaDeps(t, exe, version, tt.result, tt.err)
			called := false
			if tt.name == "old-version" {
				deps.RunClaudePTY = func(context.Context, ClaudePTYRequest) (ClaudePTYResult, error) {
					called = true
					return tt.result, tt.err
				}
			}
			report, err := Discover(context.Background(), Options{
				Config: config.Config{Adapters: config.Adapters{Worker: "claude"}},
				Now:    fixedInventoryNow,
				NetworkGrants: []NetworkGrant{{
					ProviderID: "claude",
					Purpose:    NetworkPurposeQuotaTelemetry,
					Scope:      NetworkScopeMachineInventory,
				}},
			}, deps)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if tt.name == "old-version" && called {
				t.Fatal("old unsupported Claude version launched usage PTY")
			}
			got := onlyQuotaSnapshot(t, report, "claude")
			if got.TerminalErrorCode != tt.wantCode || got.Confidence != ConfidenceUnavailable || !containsString(got.GapReasons, tt.wantGap) {
				t.Fatalf("snapshot = %#v, want %s/%s unavailable", got, tt.wantCode, tt.wantGap)
			}
			if got.RawSourceHash != "" {
				t.Fatalf("failure snapshot retained raw hash: %#v", got)
			}
		})
	}
}

func TestClaudeQuotaSandboxCreatesAndCleansPrivateRoots(t *testing.T) {
	exe := writeFakeClaude(t)
	deps := claudeQuotaDeps(t, exe, "claude 1.2.3", ClaudePTYResult{}, nil)
	root, argv, env, cleanup, err := prepareClaudeQuotaSandbox(exe, deps)
	if err != nil {
		t.Fatalf("prepareClaudeQuotaSandbox: %v", err)
	}
	if root == "" || !strings.Contains(root, "loopcoder-claude-quota-") {
		t.Fatalf("root = %q", root)
	}
	if !containsString(argv, "--strict-mcp-config") {
		t.Fatalf("argv = %#v", argv)
	}
	for _, want := range []string{"HOME=", "USERPROFILE=", "XDG_CONFIG_HOME=", "APPDATA=", "LOCALAPPDATA=", "TEMP=", "TMP=", "TMPDIR="} {
		if !envHasPrefix(env, want) {
			t.Fatalf("env missing %s in %#v", want, env)
		}
	}
	cleanup()
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sandbox root still exists or stat failed unexpectedly: %v", err)
	}
}

func claudeQuotaDeps(t *testing.T, exe, version string, result ClaudePTYResult, runErr error) Deps {
	t.Helper()
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): version})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return filepath.Dir(exe)
		}
		if key == "LANG" || key == "LC_ALL" {
			return "C"
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{Stdout: version + "\n", ExitCode: 0}, nil
	}
	deps.RunClaudePTY = func(context.Context, ClaudePTYRequest) (ClaudePTYResult, error) {
		return result, runErr
	}
	return deps
}

func writeFakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, executableName("claude"))
	writeExecutable(t, path)
	return path
}

func envHasPrefix(env []string, prefix string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
