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

func TestProbeEnvironmentHelper(t *testing.T) {
	if os.Getenv("LOOPCODER_PROVIDERINVENTORY_HELPER") != "env" {
		return
	}
	if value := os.Getenv("LOOPCODER_TEST_CREDENTIAL"); value != "" {
		_, _ = os.Stdout.WriteString("leaked=" + value)
		os.Exit(0)
	}
	_, _ = os.Stdout.WriteString("credential_absent")
	os.Exit(0)
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

func fixedInventoryNow() time.Time {
	return time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
}
