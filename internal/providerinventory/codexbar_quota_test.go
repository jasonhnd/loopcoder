package providerinventory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
)

func init() {
	if os.Getenv("LOOPCODER_FAKE_CODEXBAR") == "" {
		return
	}
	if os.Getenv("LOOPCODER_FAKE_CODEXBAR") == "hang" {
		time.Sleep(24 * time.Hour)
	}
	os.Exit(2)
}

func TestCodexBarDisabledNeverDiscoversOrLaunches(t *testing.T) {
	calledProbe := false
	calledBridge := false
	deps := fakeDeps(t, nil)
	deps.Getenv = func(string) string { return "" }
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		calledProbe = true
		return ProbeExecutionResult{}, nil
	}
	deps.RunCodexBar = func(context.Context, CodexBarRequest) (CodexBarResult, error) {
		calledBridge = true
		return CodexBarResult{}, nil
	}

	_, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if calledProbe || calledBridge {
		t.Fatalf("codexbar disabled still touched process hooks: probe=%v bridge=%v", calledProbe, calledBridge)
	}
}

func TestCodexBarGenericUnsupportedAndDisabledTrustFailClosedWithoutLaunch(t *testing.T) {
	bridgeCalls := 0
	probeCalls := 0
	deps := fakeDeps(t, nil)
	deps.Getenv = func(string) string { return "" }
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		probeCalls++
		return ProbeExecutionResult{}, nil
	}
	deps.RunCodexBar = func(context.Context, CodexBarRequest) (CodexBarResult, error) {
		bridgeCalls++
		return CodexBarResult{}, nil
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{ProviderInventory: config.ProviderInventory{CodexBar: config.CodexBar{
			Enabled: true,
			Providers: []config.CodexBarProviderOpt{
				{Provider: "codex", TrustClass: "internal-protocol"},
				{Provider: "claude", TrustClass: "disabled"},
			},
		}}},
		Now: fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if bridgeCalls != 0 || probeCalls != 0 {
		t.Fatalf("generic bridge touched process hooks: RunCodexBar=%d RunProbe=%d", bridgeCalls, probeCalls)
	}
	codex := genericCodexBarSnapshot(t, report, "codex")
	if codex.TerminalErrorCode != "ErrCodexBarGenericProtocolUnsupported" ||
		!containsString(codex.GapReasons, "codexbar-generic-protocol-unsupported") {
		t.Fatalf("codex unsupported snapshot = %#v", codex)
	}
	claude := genericCodexBarSnapshot(t, report, "claude")
	if claude.TerminalErrorCode != "ErrCodexBarTrustDenied" || !containsString(claude.GapReasons, "codexbar-trust-denied") {
		t.Fatalf("claude trust snapshot = %#v", claude)
	}
}

func TestCodexBarGenericFixtureProtocolIsNeverLaunchedOrDeclaredAvailable(t *testing.T) {
	exe := writeFakeCodexBar(t)
	deps := codexBarDeps(t, exe, "codexbar 1.2.3")
	calls := 0
	deps.RunCodexBar = func(context.Context, CodexBarRequest) (CodexBarResult, error) {
		calls++
		return CodexBarResult{Stdout: `{"schema_version":"codexbar.quota_bridge.v1"}`, ExitCode: 0}, nil
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{ProviderInventory: config.ProviderInventory{CodexBar: config.CodexBar{
			Enabled: true,
			Providers: []config.CodexBarProviderOpt{
				{Provider: "codex", TrustClass: "internal-protocol"},
				{Provider: "claude", TrustClass: "credential-delegated"},
			},
		}}},
		Now: fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if calls != 0 {
		t.Fatalf("fixture-only generic bridge launched %d times", calls)
	}
	for _, provider := range []string{"codex", "claude"} {
		source := genericCodexBarSource(t, report, provider)
		if len(source.Argv) != 0 || source.SourceSchemaVersion != codexBarDisabledSchema ||
			source.NetworkDeclared ||
			!containsString(source.GapReasons, "codexbar-generic-protocol-unsupported") {
			t.Fatalf("%s generic source still advertises the impossible protocol: %#v", provider, source)
		}
		snapshot := genericCodexBarSnapshot(t, report, provider)
		if snapshot.TerminalErrorCode != "ErrCodexBarGenericProtocolUnsupported" ||
			!containsString(snapshot.GapReasons, "codexbar-generic-protocol-unsupported") ||
			snapshot.Confidence != ConfidenceUnavailable {
			t.Fatalf("%s generic bridge did not fail closed: %#v", provider, snapshot)
		}
	}
	for _, probe := range report.ProbeResults {
		if probe.ProbeCommandID == "codexbar-generic-bridge-disabled" &&
			(probe.Outcome != OutcomeProbeFailed || probe.SideEffectClass != "not-run" ||
				probe.ProbeMethod != ProbeMethodConfigured || probe.NetworkPermission != NetworkNotNeeded) {
			t.Fatalf("generic probe claimed execution or availability: %#v", probe)
		}
	}
}

func TestRunCodexBarCommandTimeoutKillsProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	result, err := runCodexBarQuota(context.Background(), CodexBarRequest{
		Argv:               []string{exe},
		Env:                []string{"LOOPCODER_FAKE_CODEXBAR=hang"},
		Cwd:                os.TempDir(),
		Timeout:            100 * time.Millisecond,
		StdoutLimitBytes:   codexBarStdoutBytes,
		StderrLimitBytes:   codexBarStderrBytes,
		CombinedLimitBytes: codexBarStdoutBytes + codexBarStderrBytes,
	})
	if !result.TimedOut || !result.Killed {
		t.Fatalf("result = %#v err=%v, want timed out and killed", result, err)
	}
}

func codexBarDeps(t *testing.T, exe, version string) Deps {
	t.Helper()
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): version})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return filepath.Dir(exe)
		}
		return ""
	}
	deps.RunProbe = func(context.Context, ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{Stdout: version + "\n", ExitCode: 0}, nil
	}
	return deps
}

func writeFakeCodexBar(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, executableName("codexbar"))
	writeExecutable(t, path)
	return path
}

func genericCodexBarSnapshot(t *testing.T, report Report, provider string) QuotaSnapshot {
	t.Helper()
	for _, snapshot := range report.QuotaSnapshots {
		if snapshot.AdapterID == provider && snapshot.SourceKind == QuotaSourceTrustedThirdPartyBridge {
			return snapshot
		}
	}
	t.Fatalf("generic codexbar snapshot for %s missing", provider)
	return QuotaSnapshot{}
}

func genericCodexBarSource(t *testing.T, report Report, provider string) QuotaTelemetrySource {
	t.Helper()
	for _, source := range report.QuotaTelemetrySources {
		if source.AdapterID == provider && source.SourceKind == QuotaSourceTrustedThirdPartyBridge &&
			strings.Contains(source.SourceKey, "codexbar-third-party-bridge") {
			return source
		}
	}
	t.Fatalf("generic codexbar source for %s missing", provider)
	return QuotaTelemetrySource{}
}
