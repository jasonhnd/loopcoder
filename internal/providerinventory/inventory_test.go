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

func TestProbeTimeoutConstantsUseGenerousHangGuard(t *testing.T) {
	if InstallProbeTimeout != 20*time.Second {
		t.Fatalf("InstallProbeTimeout = %s, want 20s hang guard", InstallProbeTimeout)
	}
	if AuthProbeTimeout != 20*time.Second {
		t.Fatalf("AuthProbeTimeout = %s, want 20s hang guard", AuthProbeTimeout)
	}
}

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
			Stdout:   "custom " + "sk-" + strings.Repeat("t", 20),
			Stderr:   "Bearer " + strings.Repeat("a", 26),
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
		anthropicKey := "sk-ant-" + strings.Repeat("r", 24)
		awsSecret := "wJalrXUtnFEMI/" + "K7MDENG/bPxRfiCY" + "EXAMPLEKEY"
		jsonKey := strings.Repeat("a", 26) + strings.Repeat("B", 6) + strings.Repeat("1", 10)
		awsAccessKey := "AKIA" + strings.Repeat("A", 16)
		for _, entry := range req.Env {
			if strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") || strings.HasPrefix(entry, "AWS_SECRET_ACCESS_KEY=") {
				t.Fatalf("credential env reached probe: %q", entry)
			}
		}
		return ProbeExecutionResult{
			Stdout: strings.Join([]string{
				"ANTHROPIC_API_KEY=" + anthropicKey,
				"AWS_SECRET_ACCESS_KEY=" + awsSecret,
				`{"api_key":"` + jsonKey + `"}`,
				"access " + awsAccessKey,
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
	for _, notWant := range []string{"sk-ant-", "wJalrXUtn", strings.Repeat("a", 26) + strings.Repeat("B", 6), "AKIA" + strings.Repeat("A", 16)} {
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
	secretOutput, secretFindings := redactProviderOutput(`{"api_key":"` + strings.Repeat("a", 26) + strings.Repeat("B", 6) + strings.Repeat("1", 10) + `"}`)
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

func TestDiscoverSkipsNetworkDeclaredAuthProbeByDefault(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("agy"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "agy 1.0.0"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	calls := 0
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		calls++
		if len(req.Argv) > 1 && req.Argv[1] == "models" {
			t.Fatalf("network-declared auth probe executed by default: %#v", req.Argv)
		}
		return ProbeExecutionResult{Stdout: "agy 1.0.0\n", ExitCode: 0}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "antigravity"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("RunProbe calls = %d, want only version probe", calls)
	}
	authProbe := findProbe(t, report, "antigravity", "auth-readiness")
	if !authProbe.NetworkDeclared || authProbe.NetworkPermission != NetworkDenied {
		t.Fatalf("auth probe network = declared %v permission %q, want true denied", authProbe.NetworkDeclared, authProbe.NetworkPermission)
	}
	if !contains(authProbe.GapReasons, "network-permission-denied") {
		t.Fatalf("auth probe gaps = %#v, want network-permission-denied", authProbe.GapReasons)
	}
	if len(report.AuthReadiness) != 1 || report.AuthReadiness[0].ReadinessState != ReadinessUnknown || !contains(report.AuthReadiness[0].GapReasons, "network-permission-denied") {
		t.Fatalf("auth readiness = %#v, want unknown network denied", report.AuthReadiness)
	}
}

func TestDiscoverCodexAuthReadinessStatusBranches(t *testing.T) {
	tests := []struct {
		name      string
		stdout    string
		exitCode  int
		wantState ReadinessState
		wantGap   string
		wantScope AuthorizationScopeState
	}{
		{name: "authenticated", stdout: "Logged in using ChatGPT\n", exitCode: 0, wantState: ReadinessReady, wantScope: AuthorizationUnknown},
		{name: "not authenticated", stdout: "Not logged in. Run codex login.\n", exitCode: 1, wantState: ReadinessNotAuthenticated, wantGap: "provider-reports-not-authenticated", wantScope: AuthorizationNotApplicable},
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
			if got.EvidenceKind != EvidenceStatusCommand {
				t.Fatalf("evidence_kind = %q, want %q", got.EvidenceKind, EvidenceStatusCommand)
			}
			if got.AuthorizationScopeState != tt.wantScope {
				t.Fatalf("authorization_scope_state = %q, want %q", got.AuthorizationScopeState, tt.wantScope)
			}
			if tt.wantGap != "" && !contains(got.GapReasons, tt.wantGap) {
				t.Fatalf("gap_reasons = %#v, want %q", got.GapReasons, tt.wantGap)
			}
		})
	}
}

func TestDiscoverClaudeAuthStatusJSONProfiles(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("claude"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "claude 1.2.3"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return dir
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		if len(req.Argv) >= 4 && req.Argv[1] == "auth" && req.Argv[2] == "status" && req.Argv[3] == "--json" {
			statusToken := "sk-ant-" + strings.Repeat("r", 24)
			return ProbeExecutionResult{Stdout: `{
				"loggedIn": true,
				"token": "` + statusToken + `",
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
		if profile.ProviderAccountKind != "user" {
			t.Fatalf("provider account kind = %q, want user", profile.ProviderAccountKind)
		}
		if !strings.HasPrefix(profile.TenantOrProjectReferenceHash, "sha256:") {
			t.Fatalf("tenant hash = %q, want sha256 hash", profile.TenantOrProjectReferenceHash)
		}
		if len(profile.CollisionSet) != 1 {
			t.Fatalf("collision_set = %#v, want other colliding profile id", profile.CollisionSet)
		}
		if profile.SelectionState == SelectionDefault {
			t.Fatalf("colliding profile kept default selection: %#v", profile)
		}
	}
	got := latestAuthReadinessFor(t, report, "claude")
	if got.EvidenceKind != EvidenceMachineStatus || got.ReadinessState != ReadinessReady {
		t.Fatalf("claude readiness = %#v, want ready machine-readable status", got)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal report: %v", err)
	}
	for _, notWant := range []string{"sk-ant-", "alice@example.com", "bob@example.com"} {
		if strings.Contains(string(data), notWant) {
			t.Fatalf("serialized report retained sensitive profile/status material %q: %s", notWant, data)
		}
	}
}

func TestDiscoverGeminiAuthReadinessUsesExistenceOnlyReferences(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	root := t.TempDir()
	homeDir := filepath.Join(root, "home")
	authFile := filepath.Join(homeDir, ".gemini", "oauth_creds.json")
	sentinel := "sentinel-" + "refresh-" + "token-should-not-appear"
	if err := os.MkdirAll(filepath.Dir(authFile), 0o755); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.WriteFile(authFile, []byte(`{"refresh_`+`token":"`+sentinel+`"}`), 0o600); err != nil {
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

func TestDiscoverGeminiAuthReadinessEnvironmentReferenceDoesNotSerializeValue(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("gemini"))
	writeExecutable(t, exe)
	secretValue := "gemini-" + "opaque-value-123456"
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "gemini 9.9.9"})
	deps.UserHomeDir = func() (string, error) { return filepath.Join(t.TempDir(), "home"), nil }
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
	if got.EvidenceKind != EvidenceEnvironmentName || got.ReadinessState != ReadinessUnknown {
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

func TestAuthReadinessExpiredRefreshLoadRoundTrip(t *testing.T) {
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
	report, err := Discover(ctx, Options{Config: config.Config{Adapters: config.Adapters{Worker: "custom"}}, Now: fixedInventoryNow}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	installation := onlyCustomInstallation(t, report)
	adapter := AdapterDeclaration{AdapterID: "custom", DisplayName: "custom", ExecutableNames: []string{"custom"}, AuthProbeCommand: []string{"custom", "auth", "status"}}
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{Stdout: "profile alice expired refresh required\n", ExitCode: 0}, nil
	}
	profiles, readiness, authProbe := inspectAuthReadiness(ctx, nil, adapter, candidate{path: exe, source: DiscoveryPath}, installation.ProviderInstallationID, fixedInventoryNow(), deps)
	report.AccountProfiles = profiles
	report.AuthReadiness = readiness
	report.ProbeResults = append(report.ProbeResults, *authProbe)

	if err := Refresh(ctx, store, report, fixedInventoryNow()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	loaded, err := Load(ctx, store)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.AccountProfiles) != 1 || loaded.AccountProfiles[0].ProfileDisplay != "alice" {
		t.Fatalf("loaded profiles = %#v, want alice profile", loaded.AccountProfiles)
	}
	if len(loaded.AuthReadiness) < 1 {
		t.Fatalf("loaded auth readiness empty")
	}
	foundExpired := false
	for _, item := range loaded.AuthReadiness {
		if item.AdapterID == "custom" && item.ReadinessState == ReadinessExpired && item.RefreshRequired {
			foundExpired = true
		}
	}
	if !foundExpired {
		t.Fatalf("loaded auth readiness = %#v, want expired refresh-required custom record", loaded.AuthReadiness)
	}
}

func TestAuthProbeTimeoutYieldsUnknownReadiness(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, executableName("custom"))
	writeExecutable(t, exe)
	deps := fakeDeps(t, nil)
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{ExitCode: -1, TimedOut: true, Killed: true}, nil
	}
	adapter := AdapterDeclaration{AdapterID: "custom", DisplayName: "custom", ExecutableNames: []string{"custom"}, AuthProbeCommand: []string{"custom", "auth", "status"}}
	_, readiness, probe := inspectAuthReadiness(context.Background(), nil, adapter, candidate{path: exe, source: DiscoveryPath}, "pinst_custom", fixedInventoryNow(), deps)
	if probe == nil || !probe.TimedOut || probe.TerminalErrorCode != "ErrAuthProbeTimeout" {
		t.Fatalf("auth probe = %#v, want timeout terminal error", probe)
	}
	if len(readiness) != 1 || readiness[0].ReadinessState != ReadinessUnknown || !contains(readiness[0].GapReasons, "auth-probe-timeout") {
		t.Fatalf("readiness = %#v, want unknown timeout", readiness)
	}
}

func TestInaccessibleAuthArtifactYieldsTypedUnknown(t *testing.T) {
	deps := fakeDeps(t, nil)
	deps.Lstat = func(string) (os.FileInfo, error) {
		return nil, os.ErrPermission
	}
	adapter := AdapterDeclaration{AdapterID: "custom", DisplayName: "custom", AuthArtifactPaths: []string{filepath.Join(t.TempDir(), "auth.json")}}
	_, readiness, _ := inspectAuthReadiness(context.Background(), nil, adapter, candidate{}, "pinst_custom", fixedInventoryNow(), deps)
	if len(readiness) != 1 {
		t.Fatalf("readiness count = %d, want 1", len(readiness))
	}
	if readiness[0].ReadinessState != ReadinessUnknown || readiness[0].TerminalErrorCode != "ErrAuthArtifactInaccessible" || !contains(readiness[0].GapReasons, "auth-artifact-inaccessible") {
		t.Fatalf("readiness = %#v, want typed inaccessible unknown", readiness[0])
	}
}

func TestTextAuthParserUsesAllowlistedDisplayAndHashedReference(t *testing.T) {
	raw := "authenticated as jane.doe@example.com"
	parsed := parseTextAuthStatus("custom", raw)
	if len(parsed) != 1 {
		t.Fatalf("parsed count = %d, want 1", len(parsed))
	}
	if parsed[0].Display != "j***@example.com" {
		t.Fatalf("display = %q, want redacted email", parsed[0].Display)
	}
	if strings.Contains(parsed[0].ReferenceHash, "jane") || parsed[0].ReferenceHash == raw {
		t.Fatalf("reference hash retained raw line: %#v", parsed[0])
	}
	unknown := parseTextAuthStatus("custom", "ready $$$")
	if len(unknown) != 1 || !strings.HasPrefix(unknown[0].Display, "profile-") {
		t.Fatalf("unknown display = %#v, want profile suffix", unknown)
	}
	unstructured := parseTextAuthStatus("custom", "ready bob")
	if len(unstructured) != 1 || !strings.HasPrefix(unstructured[0].Display, "profile-") {
		t.Fatalf("unstructured display = %#v, want profile suffix", unstructured)
	}
}

func TestProfileSelectionPolicyUsesOpaqueAccountID(t *testing.T) {
	parsed := parseTextAuthStatus("custom", strings.Join([]string{
		"profile alice ready",
		"profile bob ready",
	}, "\n"))
	if len(parsed) != 2 {
		t.Fatalf("parsed = %#v, want two profiles", parsed)
	}
	selectedID := accountProfileID("custom", string(ProfileSourceStatusCommand), parsed[1].ReferenceHash)
	adapter := AdapterDeclaration{AdapterID: "custom", DisplayName: "custom", SelectedAccountProfileID: selectedID}
	profiles, _ := authRecordsFromParsed(adapter, "pinst_custom", parsed, fixedInventoryNow())
	if len(profiles) != 2 {
		t.Fatalf("profiles = %#v, want two", profiles)
	}
	states := map[string]SelectionState{}
	for _, profile := range profiles {
		states[profile.AccountProfileID] = profile.SelectionState
	}
	if states[selectedID] != SelectionExplicit {
		t.Fatalf("selected profile state = %q, want explicit", states[selectedID])
	}
	for _, profile := range profiles {
		if profile.AccountProfileID != selectedID && profile.SelectionState != SelectionSuperseded {
			t.Fatalf("unselected profile = %#v, want superseded", profile)
		}
	}
}

func TestParsedFieldsSetterRedactsStructurally(t *testing.T) {
	var probe ProbeResult
	secret := "sk-" + strings.Repeat("x", 20)
	probe.setParsedFields(map[string]string{"token_hint": secret})
	if strings.Contains(probe.ParsedFields["token_hint"], secret) || !strings.Contains(probe.ParsedFields["token_hint"], "[REDACTED]") {
		t.Fatalf("parsed fields not redacted: %#v", probe.ParsedFields)
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

func findProbe(t *testing.T, report Report, adapterID, kind string) ProbeResult {
	t.Helper()
	for _, probe := range report.ProbeResults {
		if probe.AdapterID == adapterID && probe.ProbeKind == kind {
			return probe
		}
	}
	t.Fatalf("%s %s probe missing in %#v", adapterID, kind, report.ProbeResults)
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

func contains(values []string, want string) bool {
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

func TestParseGrokTextModelsSkipsLoginBanner(t *testing.T) {
	output := "You are logged in with grok.com.\n\nDefault model: grok-4.5\n\nAvailable models:\n  * grok-4.5 (default)\n  * grok-code-fast-1\n"
	entries := parseGrokTextModels(output)
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ModelID)
	}
	for _, bad := range []string{"You", "Default", "Available"} {
		for _, id := range ids {
			if id == bad {
				t.Fatalf("parsed banner word as model id %q from %#v", bad, ids)
			}
		}
	}
	if len(ids) < 1 || ids[0] != "grok-4.5" {
		t.Fatalf("ids = %#v, want grok-4.5 first", ids)
	}
}
