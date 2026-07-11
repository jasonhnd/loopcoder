package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestDiscoverRecordsAbsentProviderWithoutRunningProbe(t *testing.T) {
	deps := fakeDeps(t, nil)
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return t.TempDir()
		}
		return ""
	}
	calls := 0
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		calls++
		return ProbeExecutionResult{}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "missing-provider"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if calls != 0 {
		t.Fatalf("RunProbe calls = %d, want 0 for absent executable", calls)
	}
	found := false
	for _, probe := range report.ProbeResults {
		if probe.AdapterID == "missing-provider" {
			found = true
			if probe.Outcome != OutcomeNotInstalled || probe.ProbeMethod != ProbeMethodLookPath || probe.Confidence != ConfidenceUnavailable {
				t.Fatalf("absent probe = %#v", probe)
			}
		}
	}
	if !found {
		t.Fatalf("missing-provider absent probe not found in %#v", report.ProbeResults)
	}
}

func TestDiscoverDedupesPathEntriesAndKeepsMultipleVersions(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "A")
	dirB := filepath.Join(root, "space dir")
	writeExecutable(t, filepath.Join(dirA, executableName("custom")))
	writeExecutable(t, filepath.Join(dirB, executableName("custom")))

	versions := map[string]string{
		filepath.Clean(filepath.Join(dirA, executableName("custom"))): "custom 1.0.0",
		filepath.Clean(filepath.Join(dirB, executableName("custom"))): "custom 2.0.0",
	}
	deps := fakeDeps(t, versions)
	deps.Getenv = func(key string) string {
		switch key {
		case "PATH":
			return strings.Join([]string{dirA, dirA, dirB}, string(os.PathListSeparator))
		case "PATHEXT":
			return ".EXE;.CMD;.BAT"
		default:
			return ""
		}
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "custom"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	var installs []ProviderInstallation
	for _, installation := range report.Installations {
		if installation.AdapterID == "custom" {
			installs = append(installs, installation)
		}
	}
	if len(installs) != 2 {
		t.Fatalf("custom installations = %d, want 2: %#v", len(installs), installs)
	}
	if installs[0].ProviderInstallationID == installs[1].ProviderInstallationID {
		t.Fatalf("multiple versions share installation id: %#v", installs)
	}
	if !strings.Contains(installs[1].CanonicalPathRedacted, "space dir") {
		t.Fatalf("space path redaction = %q, want parent hint", installs[1].CanonicalPathRedacted)
	}
	for _, installation := range installs {
		if installation.UsableForInvocation != "unknown" {
			t.Fatalf("usable_for_invocation = %q, want unknown", installation.UsableForInvocation)
		}
	}
}

func TestDiscoverUsesExplicitConfiguredLocation(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "not-on-path", executableName("custom"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "custom 3.0.0"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return filepath.Join(root, "empty-path")
		}
		return ""
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{
			Adapters: config.Adapters{Worker: "custom"},
			ProviderInventory: config.ProviderInventory{
				Executables: map[string][]string{"custom": []string{exe}},
			},
		},
		Now: fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	var got *ProviderInstallation
	for i := range report.Installations {
		if report.Installations[i].AdapterID == "custom" {
			got = &report.Installations[i]
			break
		}
	}
	if got == nil {
		t.Fatal("custom installation not found")
	}
	if got.DiscoverySource != DiscoveryExplicitConfig {
		t.Fatalf("DiscoverySource = %q, want explicit-config", got.DiscoverySource)
	}
	if got.Version != "custom 3.0.0" {
		t.Fatalf("Version = %q, want custom 3.0.0", got.Version)
	}
}

func TestDiscoverRecordsSymlinkIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliable in unprivileged Windows test runs")
	}
	root := t.TempDir()
	targetDir := filepath.Join(root, "target")
	linkDir := filepath.Join(root, "link")
	target := filepath.Join(targetDir, "custom")
	link := filepath.Join(linkDir, "custom")
	writeExecutable(t, target)
	if err := os.MkdirAll(linkDir, 0o755); err != nil {
		t.Fatalf("mkdir link dir: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	deps := fakeDeps(t, map[string]string{filepath.Clean(link): "custom 1.0.0"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return linkDir
		}
		return ""
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "custom"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	var got *ProviderInstallation
	for i := range report.Installations {
		if report.Installations[i].AdapterID == "custom" {
			got = &report.Installations[i]
			break
		}
	}
	if got == nil {
		t.Fatal("custom installation not found")
	}
	if got.ExecutableIdentity.SymlinkResolution != "resolved" {
		t.Fatalf("symlink resolution = %q, want resolved", got.ExecutableIdentity.SymlinkResolution)
	}
}

func TestDiscoverProbeTimeoutAndSecretRedaction(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("custom"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, nil)
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		if len(req.Argv) != 2 || req.Argv[1] != "--version" {
			t.Fatalf("argv = %#v, want fixed executable --version", req.Argv)
		}
		if req.Timeout > InstallProbeTimeout || req.StdoutLimitBytes > StdoutLimitBytes || req.CombinedLimitBytes > CombinedLimitBytes {
			t.Fatalf("probe bounds exceeded: %#v", req)
		}
		return ProbeExecutionResult{
			Stdout:   "custom sk-testsecret1234567890",
			Stderr:   "Bearer abcdefghijklmnopqrstuvwxyz",
			ExitCode: 1,
			TimedOut: true,
			Killed:   true,
		}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "custom"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	var probe ProbeResult
	for _, candidate := range report.ProbeResults {
		if candidate.AdapterID == "custom" && candidate.ProviderInstallationID != nil {
			probe = candidate
			break
		}
	}
	if probe.Outcome != OutcomeProbeFailed || !probe.TimedOut || !probe.Killed {
		t.Fatalf("probe = %#v, want timeout probe-failed", probe)
	}
	if strings.Contains(probe.StdoutSummary+probe.StderrSummary, "sk-testsecret") || strings.Contains(probe.StdoutSummary+probe.StderrSummary, "Bearer abc") {
		t.Fatalf("secret material retained in probe summaries: %#v", probe)
	}
	if probe.SecretFindingCount < 2 {
		t.Fatalf("secret finding count = %d, want at least 2", probe.SecretFindingCount)
	}
}

func TestDiscoverRedactsAdversarialProviderOutputBeforePersistence(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("custom"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, nil)
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		for _, entry := range req.Env {
			if strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") || strings.HasPrefix(entry, "AWS_SECRET_ACCESS_KEY=") {
				t.Fatalf("credential env reached probe: %q", entry)
			}
		}
		return ProbeExecutionResult{
			Stdout: strings.Join([]string{
				"ANTHROPIC_API_KEY=sk-ant-redactedredactedredacted",
				"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
				`{"api_key":"abcdefghijklmnopqrstuvwxyzABCDEF1234567890"}`,
				"access AKIAABCDEFGHIJKLMNOP",
			}, "\n"),
			ExitCode: 0,
		}, nil
	}

	report, err := Discover(ctx, Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "custom"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if err := Refresh(ctx, store, report, fixedInventoryNow()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var summary string
	for _, probe := range loaded.ProbeResults {
		if probe.AdapterID == "custom" {
			summary += probe.StdoutSummary + probe.StderrSummary
		}
	}
	for _, notWant := range []string{"sk-ant-", "wJalrXUtn", "abcdefghijklmnopqrstuvwxyzABCDEF", "AKIAABCDEFGHIJKLMNOP"} {
		if strings.Contains(summary, notWant) {
			t.Fatalf("persisted probe summary retained secret %q: %s", notWant, summary)
		}
	}
	if count := strings.Count(summary, "[REDACTED]"); count < 4 {
		t.Fatalf("redaction count in summary = %d, want at least 4: %s", count, summary)
	}
}

func TestRunProbeCommandUsesExplicitEnvAllowlist(t *testing.T) {
	t.Setenv("LOOPCODER_TEST_CREDENTIAL", "credential-should-not-leak")
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := append(probeEnvironment(os.Getenv), "LOOPCODER_PROVIDERINVENTORY_HELPER=env")
	result, err := runProbeCommand(context.Background(), ProbeExecution{
		Argv:               []string{executable, "-test.run=TestProbeEnvironmentHelper", "--"},
		Env:                env,
		Timeout:            InstallProbeTimeout,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	if err != nil {
		t.Fatalf("runProbeCommand returned error: %v stderr=%s", err, result.Stderr)
	}
	if strings.Contains(result.Stdout+result.Stderr, "credential-should-not-leak") {
		t.Fatalf("probe output saw parent credential env: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "credential_absent") {
		t.Fatalf("helper output = %q, want credential_absent", result.Stdout)
	}
}

func TestProbeEnvironmentNameDenylistAppliesAfterAllowlist(t *testing.T) {
	t.Setenv("FAKE_API_KEY", "fake-key-should-not-leak")
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	env := append(probeEnvironmentFromKeys(os.Getenv, []string{"PATH", "FAKE_API_KEY"}), "LOOPCODER_PROVIDERINVENTORY_HELPER=denylist")
	result, err := runProbeCommand(context.Background(), ProbeExecution{
		Argv:               []string{executable, "-test.run=TestProbeEnvironmentHelper", "--"},
		Env:                env,
		Timeout:            InstallProbeTimeout,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	if err != nil {
		t.Fatalf("runProbeCommand returned error: %v stderr=%s", err, result.Stderr)
	}
	if strings.Contains(result.Stdout+result.Stderr, "fake-key-should-not-leak") {
		t.Fatalf("probe output saw denylisted env: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "fake_api_key_absent") {
		t.Fatalf("helper output = %q, want fake_api_key_absent", result.Stdout)
	}
}

func TestProbeEnvironmentHelper(t *testing.T) {
	switch os.Getenv("LOOPCODER_PROVIDERINVENTORY_HELPER") {
	case "env":
		if value := os.Getenv("LOOPCODER_TEST_CREDENTIAL"); value != "" {
			_, _ = os.Stdout.WriteString("leaked=" + value)
			os.Exit(0)
		}
		_, _ = os.Stdout.WriteString("credential_absent")
		os.Exit(0)
	case "denylist":
		if value := os.Getenv("FAKE_API_KEY"); value != "" {
			_, _ = os.Stdout.WriteString("leaked=" + value)
			os.Exit(0)
		}
		_, _ = os.Stdout.WriteString("fake_api_key_absent")
		os.Exit(0)
	default:
		return
	}
}

func TestDiscoverScriptShimReceivesLocationEnvironment(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shim")
	locationRoot := filepath.Join(root, "location root")
	realDir := filepath.Join(locationRoot, "provider-bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("mkdir shim dir: %v", err)
	}
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real dir: %v", err)
	}
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	testExeBytes, err := os.ReadFile(testExe)
	if err != nil {
		t.Fatalf("read test executable: %v", err)
	}
	helperName := "provider-helper"
	if runtime.GOOS == "windows" {
		helperName += ".exe"
	}
	helperPath := filepath.Join(realDir, helperName)
	helperMode := os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		helperMode = 0o644
	}
	if err := os.WriteFile(helperPath, testExeBytes, helperMode); err != nil {
		t.Fatalf("write helper executable: %v", err)
	}

	var shimPath string
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", locationRoot)
		t.Setenv("PATHEXT", ".CMD;.EXE")
		shimPath = filepath.Join(shimDir, "custom.cmd")
		content := "@echo off\r\n\"%LOCALAPPDATA%\\provider-bin\\provider-helper.exe\" -test.run=TestProbeShimHelper -- --provider-shim-helper %*\r\n"
		if err := os.WriteFile(shimPath, []byte(content), 0o644); err != nil {
			t.Fatalf("write cmd shim: %v", err)
		}
	} else {
		t.Setenv("TMPDIR", locationRoot)
		shimPath = filepath.Join(shimDir, "custom")
		content := "#!/bin/sh\nexec \"$TMPDIR/provider-bin/provider-helper\" -test.run=TestProbeShimHelper -- --provider-shim-helper \"$@\"\n"
		if err := os.WriteFile(shimPath, []byte(content), 0o755); err != nil {
			t.Fatalf("write shell shim: %v", err)
		}
	}
	t.Setenv("PATH", shimDir)

	deps := DefaultDeps()
	deps.RandomID = stableProbeIDs()
	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "custom"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	custom := onlyCustomInstallation(t, report)
	if custom.InstallationState != InstallationInstalled {
		t.Fatalf("shim installation state = %q, want installed; probe=%#v", custom.InstallationState, onlyCustomProbe(t, report))
	}
	if custom.Version != "custom 9.9.9" || custom.VersionConfidence != ConfidenceExact {
		t.Fatalf("shim version = (%q, %q), want custom 9.9.9 exact", custom.Version, custom.VersionConfidence)
	}
	if custom.CanonicalPathRedacted == "" || !strings.Contains(filepath.ToSlash(custom.CanonicalPathRedacted), "shim") {
		t.Fatalf("shim path redaction = %q", custom.CanonicalPathRedacted)
	}
	_ = shimPath
}

func TestProbeShimHelper(t *testing.T) {
	if !hasArg("--provider-shim-helper") {
		return
	}
	_, _ = os.Stdout.WriteString("custom 9.9.9\n")
	os.Exit(0)
}

func TestRunProbeCommandTimeoutUsesSentinelExitCode(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	result, err := runProbeCommand(context.Background(), ProbeExecution{
		Argv:               []string{executable, "-test.run=TestProbeTimeoutHelper", "--", "--provider-timeout-helper"},
		Env:                probeEnvironment(os.Getenv),
		Timeout:            10 * time.Millisecond,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	if err != nil {
		t.Fatalf("runProbeCommand returned error: %v", err)
	}
	if !result.TimedOut || !result.Killed {
		t.Fatalf("timeout result = %#v, want timed_out and killed", result)
	}
	if result.ExitCode == 0 {
		t.Fatalf("timeout exit code = 0, want non-zero sentinel or kill status: %#v", result)
	}
}

func TestProbeTimeoutHelper(t *testing.T) {
	if !hasArg("--provider-timeout-helper") {
		return
	}
	time.Sleep(5 * time.Second)
	os.Exit(0)
}

func TestRedactionKeepsPowerShellErrorIdentifiers(t *testing.T) {
	input := "FullyQualifiedErrorId : ParameterArgumentValidationErrorNullNotAllowed,Microsoft.PowerShell.Commands.JoinPathCommand"
	output, findings := redactProviderOutput(input)
	if findings != 0 || strings.Contains(output, "[REDACTED]") {
		t.Fatalf("redacted PowerShell error identifier: findings=%d output=%q", findings, output)
	}
	secretOutput, secretFindings := redactProviderOutput(`{"api_key":"abcdefghijklmnopqrstuvwxyzABCDEF1234567890"}`)
	if secretFindings == 0 || strings.Contains(secretOutput, "abcdefghijklmnopqrstuvwxyzABCDEF") {
		t.Fatalf("failed to redact opaque key/value secret: findings=%d output=%q", secretFindings, secretOutput)
	}
}

func TestDiscoverDistinguishesInstalledUnusableFromProbeFailed(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("custom"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, nil)
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{Stdout: "not authorized\n", ExitCode: 2}, nil
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "custom"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	custom := onlyCustomInstallation(t, report)
	if custom.InstallationState != InstallationInstalledUnusable {
		t.Fatalf("non-zero probe installation state = %q, want installed-but-unusable", custom.InstallationState)
	}
	customProbe := onlyCustomProbe(t, report)
	if customProbe.Outcome != OutcomeInstalledUnusable {
		t.Fatalf("non-zero probe outcome = %q, want installed-but-unusable", customProbe.Outcome)
	}

	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{Stdout: "\n\t\n", ExitCode: 0}, nil
	}
	report, err = Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "custom"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	custom = onlyCustomInstallation(t, report)
	if custom.InstallationState != InstallationInstalledUnusable {
		t.Fatalf("unparseable probe installation state = %q, want installed-but-unusable", custom.InstallationState)
	}
	customProbe = onlyCustomProbe(t, report)
	if customProbe.Outcome != OutcomeInstalledUnusable {
		t.Fatalf("unparseable probe outcome = %q, want installed-but-unusable", customProbe.Outcome)
	}

	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{ExitCode: -1}, errors.New("spawn failed")
	}
	report, err = Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "custom"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	custom = onlyCustomInstallation(t, report)
	if custom.InstallationState != InstallationProbeFailed {
		t.Fatalf("infrastructure failure installation state = %q, want probe-failed", custom.InstallationState)
	}
	customProbe = onlyCustomProbe(t, report)
	if customProbe.Outcome != OutcomeProbeFailed {
		t.Fatalf("infrastructure failure outcome = %q, want probe-failed", customProbe.Outcome)
	}
}

func TestDiscoverAuthReadinessStatesFromStatusCommand(t *testing.T) {
	tests := []struct {
		name      string
		stdout    string
		exitCode  int
		wantState ReadinessState
		wantGap   string
		wantScope AuthorizationScopeState
	}{
		{name: "authenticated", stdout: "Logged in using ChatGPT\n", exitCode: 0, wantState: ReadinessReady, wantScope: AuthorizationUnknown},
		{name: "logged out", stdout: "Not logged in. Run codex login.\n", exitCode: 1, wantState: ReadinessNotAuthenticated, wantGap: "provider-reports-not-authenticated", wantScope: AuthorizationUnknown},
		{name: "expired", stdout: "Credentials expired; refresh required.\n", exitCode: 1, wantState: ReadinessExpired, wantGap: "provider-reports-expired", wantScope: AuthorizationUnknown},
		{name: "partial authorization", stdout: "not authorized for selected project\n", exitCode: 1, wantState: ReadinessUnknown, wantGap: "provider-authorization-partial", wantScope: AuthorizationPartial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, executableName("codex"))
			writeExecutable(t, exe)
			deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "codex 1.2.3"})
			deps.Getenv = func(key string) string {
				if key == "PATH" {
					return dir
				}
				return ""
			}
			deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
				if len(req.Argv) >= 3 && req.Argv[1] == "login" && req.Argv[2] == "status" {
					return ProbeExecutionResult{Stdout: tt.stdout, ExitCode: tt.exitCode}, nil
				}
				return ProbeExecutionResult{Stdout: "codex 1.2.3\n", ExitCode: 0}, nil
			}

			report, err := Discover(context.Background(), Options{
				Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
				Now:    fixedInventoryNow,
			}, deps)
			if err != nil {
				t.Fatalf("Discover returned error: %v", err)
			}
			got := latestAuthReadinessFor(t, report, "codex")
			if got.ReadinessState != tt.wantState {
				t.Fatalf("readiness_state = %q, want %q; record=%#v", got.ReadinessState, tt.wantState, got)
			}
			if got.AuthorizationScopeState != tt.wantScope {
				t.Fatalf("authorization_scope_state = %q, want %q", got.AuthorizationScopeState, tt.wantScope)
			}
			if tt.wantGap != "" && !containsString(got.GapReasons, tt.wantGap) {
				t.Fatalf("gap_reasons = %#v, want %q", got.GapReasons, tt.wantGap)
			}
		})
	}
}

func TestDiscoverClaudeAuthProfilesRedactsAndMarksCollisions(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("claude"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "1.0.0"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		if len(req.Argv) >= 4 && req.Argv[1] == "auth" && req.Argv[2] == "status" {
			return ProbeExecutionResult{Stdout: `{
				"loggedIn": true,
				"token": "sk-ant-redactedredactedredacted",
				"profiles": [
					{"id":"profile-a","email":"alice@example.com","display":"Shared","orgId":"org-a","apiProvider":"firstParty","subscriptionType":"max"},
					{"id":"profile-b","email":"bob@example.com","display":"Shared","orgId":"org-b","apiProvider":"firstParty","subscriptionType":"pro"}
				]
			}`, ExitCode: 0}, nil
		}
		return ProbeExecutionResult{Stdout: "claude 1.2.3\n", ExitCode: 0}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "claude"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(report.AccountProfiles) != 2 {
		t.Fatalf("account profiles = %d, want 2: %#v", len(report.AccountProfiles), report.AccountProfiles)
	}
	for _, profile := range report.AccountProfiles {
		if profile.ProfileReferenceHash == "" || strings.Contains(profile.ProfileReferenceHash, "profile-") {
			t.Fatalf("profile reference hash is not opaque: %#v", profile)
		}
		if len(profile.CollisionSet) != 2 {
			t.Fatalf("collision_set = %#v, want both profile ids", profile.CollisionSet)
		}
		if profile.SelectionState == SelectionDefault {
			t.Fatalf("colliding profile kept default selection: %#v", profile)
		}
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal report: %v", err)
	}
	for _, notWant := range []string{"sk-ant-redacted", "alice@example.com", "bob@example.com"} {
		if strings.Contains(string(data), notWant) {
			t.Fatalf("serialized report retained sensitive profile/status material %q: %s", notWant, data)
		}
	}
}

func TestDiscoverAuthReadinessNeverReadsCredentialFileContents(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	root := t.TempDir()
	homeDir := filepath.Join(root, "home")
	authFile := filepath.Join(homeDir, ".gemini", "oauth_creds.json")
	sentinel := "sentinel-refresh-token-should-not-appear"
	if err := os.MkdirAll(filepath.Dir(authFile), 0o755); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.WriteFile(authFile, []byte(`{"refresh_token":"`+sentinel+`"}`), 0o600); err != nil {
		t.Fatalf("write auth fixture: %v", err)
	}
	binDir := filepath.Join(root, "bin")
	exe := filepath.Join(binDir, executableName("gemini"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "gemini 9.9.9"})
	deps.UserHomeDir = func() (string, error) { return homeDir, nil }
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return binDir
		}
		return ""
	}

	report, err := Discover(ctx, Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "gemini"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	got := latestAuthReadinessFor(t, report, "gemini")
	if got.ReadinessState != ReadinessUnknown || got.EvidenceKind != EvidenceFileExistence {
		t.Fatalf("gemini readiness = %#v, want unknown from file-existence", got)
	}
	if err := Refresh(ctx, store, report, fixedInventoryNow()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	data, err := json.Marshal(loaded)
	if err != nil {
		t.Fatalf("Marshal loaded report: %v", err)
	}
	if strings.Contains(string(data), sentinel) || strings.Contains(string(data), "refresh_token") {
		t.Fatalf("persisted/serialized inventory retained credential fixture content: %s", data)
	}
}

func TestDiscoverAuthReadinessEnvironmentReferenceDoesNotSerializeValue(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("gemini"))
	writeExecutable(t, exe)
	secretValue := "gemini-secret-value-123456"
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "gemini 9.9.9"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	deps.LookupEnv = func(key string) (string, bool) {
		if key == "GEMINI_API_KEY" {
			return secretValue, true
		}
		return "", false
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "gemini"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	got := latestAuthReadinessFor(t, report, "gemini")
	if got.EvidenceKind != EvidenceEnvNameExistence || got.ReadinessState != ReadinessUnknown {
		t.Fatalf("gemini env readiness = %#v, want unknown env-name-existence", got)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal report: %v", err)
	}
	if strings.Contains(string(data), secretValue) || strings.Contains(string(data), "GEMINI_API_KEY") {
		t.Fatalf("serialized inventory retained env secret value or name: %s", data)
	}
}

func TestRefreshPersistsHistoryAndMarksStale(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("custom"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "custom 1.0.0"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	first, err := Discover(ctx, Options{Config: config.Config{Adapters: config.Adapters{Worker: "custom"}}, Now: fixedInventoryNow}, deps)
	if err != nil {
		t.Fatalf("first Discover: %v", err)
	}
	if err := Refresh(ctx, store, first, fixedInventoryNow()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}

	if err := os.Remove(exe); err != nil {
		t.Fatalf("remove executable: %v", err)
	}
	second, err := Discover(ctx, Options{Config: config.Config{Adapters: config.Adapters{Worker: "custom"}}, Now: func() time.Time {
		return fixedInventoryNow().Add(time.Hour)
	}}, deps)
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	if err := Refresh(ctx, store, second, fixedInventoryNow().Add(time.Hour)); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	customInstallations := 0
	for _, installation := range loaded.Installations {
		if installation.AdapterID == "custom" {
			customInstallations++
			if installation.InstallationState != InstallationStale || installation.FreshnessState != FreshnessStale {
				t.Fatalf("stale installation = %#v", installation)
			}
		}
	}
	if customInstallations != 1 {
		t.Fatalf("custom installation count = %d, want retained historical row", customInstallations)
	}
	customProbes := 0
	for _, probe := range loaded.ProbeResults {
		if probe.AdapterID == "custom" {
			customProbes++
		}
	}
	if customProbes < 2 {
		t.Fatalf("custom probe history count = %d, want install plus not-installed", customProbes)
	}
	var probeScope, probePolicy, probeHost string
	var installationPolicy, installationHost string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT scope, policy_version, host_json FROM provider_probe_results WHERE adapter_id = 'custom' ORDER BY captured_at LIMIT 1`).Scan(&probeScope, &probePolicy, &probeHost); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT policy_version, host_json FROM provider_installations WHERE adapter_id = 'custom' LIMIT 1`).Scan(&installationPolicy, &installationHost)
	}); err != nil {
		t.Fatalf("query persisted provenance columns: %v", err)
	}
	if probeScope != "machine" || probePolicy != PolicyVersion || installationPolicy != PolicyVersion {
		t.Fatalf("persisted scope/policy = probe(%q,%q) installation(%q), want machine/%s", probeScope, probePolicy, installationPolicy, PolicyVersion)
	}
	if !strings.Contains(probeHost, `"host_kind"`) || !strings.Contains(installationHost, `"host_kind"`) {
		t.Fatalf("persisted host provenance missing: probe=%s installation=%s", probeHost, installationHost)
	}
}

func TestBoundedBufferEnforcesCombinedLimit(t *testing.T) {
	budget := newOutputBudget(8)
	stdout := newBoundedBuffer(64, budget)
	stderr := newBoundedBuffer(64, budget)
	_, _ = stdout.Write([]byte("123456"))
	_, _ = stderr.Write([]byte("abcdef"))
	if got := stdout.String() + stderr.String(); got != "123456ab" {
		t.Fatalf("combined output = %q, want capped to 8 bytes", got)
	}
	if !stderr.Truncated() {
		t.Fatal("stderr truncated = false, want true")
	}
}

func TestBoundedBufferConcurrentWritersShareCombinedLimit(t *testing.T) {
	budget := newOutputBudget(128)
	stdout := newBoundedBuffer(1024, budget)
	stderr := newBoundedBuffer(1024, budget)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = stdout.Write([]byte(strings.Repeat("a", 32)))
		}()
		go func() {
			defer wg.Done()
			_, _ = stderr.Write([]byte(strings.Repeat("b", 32)))
		}()
	}
	wg.Wait()
	if got := len(stdout.String()) + len(stderr.String()); got != 128 {
		t.Fatalf("combined output length = %d, want exactly capped 128", got)
	}
	if !stdout.Truncated() && !stderr.Truncated() {
		t.Fatal("neither stream reported truncation after concurrent overflow")
	}
}

func TestInventoryEnumsFailClosedOnUnknownValue(t *testing.T) {
	var payload struct {
		Confidence Confidence `json:"confidence"`
	}
	err := json.Unmarshal([]byte(`{"confidence":"optimistic"}`), &payload)
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("json.Unmarshal error = %v, want ErrInvalidRecord", err)
	}
}

func TestMarshalJSONTextPropagatesErrors(t *testing.T) {
	if _, err := marshalJSONText(func() {}); err == nil {
		t.Fatal("marshalJSONText returned nil error for unsupported value")
	}
}

func fakeDeps(t *testing.T, versions map[string]string) Deps {
	t.Helper()
	deps := DefaultDeps()
	deps.RandomID = stableProbeIDs()
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		version := versions[filepath.Clean(req.Argv[0])]
		if version == "" {
			version = filepath.Base(req.Argv[0]) + " 0.0.0"
		}
		return ProbeExecutionResult{Stdout: version + "\n", ExitCode: 0}, nil
	}
	return deps
}

func onlyCustomInstallation(t *testing.T, report Report) ProviderInstallation {
	t.Helper()
	for _, installation := range report.Installations {
		if installation.AdapterID == "custom" {
			return installation
		}
	}
	t.Fatalf("custom installation missing in %#v", report.Installations)
	return ProviderInstallation{}
}

func onlyCustomProbe(t *testing.T, report Report) ProbeResult {
	t.Helper()
	for _, probe := range report.ProbeResults {
		if probe.AdapterID == "custom" && probe.ProviderInstallationID != nil {
			return probe
		}
	}
	t.Fatalf("custom probe missing in %#v", report.ProbeResults)
	return ProbeResult{}
}

func latestAuthReadinessFor(t *testing.T, report Report, adapterID string) AuthReadiness {
	t.Helper()
	for i := len(report.AuthReadiness) - 1; i >= 0; i-- {
		if report.AuthReadiness[i].AdapterID == adapterID {
			return report.AuthReadiness[i]
		}
	}
	t.Fatalf("auth readiness for %s missing in %#v", adapterID, report.AuthReadiness)
	return AuthReadiness{}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func stableProbeIDs() func() string {
	count := 0
	return func() string {
		count++
		return "probe_test_" + string(rune('a'+count))
	}
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir executable dir: %v", err)
	}
	mode := os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		mode = 0o644
	}
	if err := os.WriteFile(path, []byte(""), mode); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func hasArg(arg string) bool {
	for _, candidate := range os.Args {
		if candidate == arg {
			return true
		}
	}
	return false
}

func fixedInventoryNow() time.Time {
	return time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
}
