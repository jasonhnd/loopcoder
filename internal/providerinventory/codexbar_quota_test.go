package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func init() {
	if os.Getenv("LOOPCODER_FAKE_CODEXBAR") == "" {
		return
	}
	runFakeCodexBar()
	os.Exit(0)
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

func TestCodexBarAbsenceAndDisabledTrustFailClosedWithoutLaunch(t *testing.T) {
	bridgeCalls := 0
	deps := fakeDeps(t, nil)
	deps.Getenv = func(string) string { return "" }
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
	if bridgeCalls != 0 {
		t.Fatalf("RunCodexBar calls = %d, want none", bridgeCalls)
	}
	codex := codexBarQuotaSnapshot(t, report, "codex")
	if codex.TerminalErrorCode != "ErrCodexBarAbsent" || !containsString(codex.GapReasons, "codexbar-not-installed") {
		t.Fatalf("codex absence snapshot = %#v", codex)
	}
	claude := codexBarQuotaSnapshot(t, report, "claude")
	if claude.TerminalErrorCode != "ErrCodexBarTrustDenied" || !containsString(claude.GapReasons, "codexbar-trust-denied") {
		t.Fatalf("claude trust snapshot = %#v", claude)
	}
}

func TestCodexBarCollectsConfiguredProvidersIndependently(t *testing.T) {
	exe := writeFakeCodexBar(t)
	now := fixedInventoryNow()
	reset := now.Add(2 * time.Hour)
	calls := map[string]int{}
	deps := codexBarDeps(t, exe, "codexbar 1.2.3")
	deps.Getenv = func(key string) string {
		switch key {
		case "PATH":
			return filepath.Dir(exe)
		case "GIT_DIR", "GIT_WORK_TREE", "PWD", "OPENAI_API_KEY", "CODEX_AUTH_TOKEN":
			return "must-not-cross"
		default:
			return ""
		}
	}
	deps.RunCodexBar = func(_ context.Context, req CodexBarRequest) (CodexBarResult, error) {
		if len(req.Argv) == 0 || req.Argv[0] != exe {
			t.Fatalf("codexbar argv = %#v, want executable path first", req.Argv)
		}
		if !containsString(req.Argv, "--provider") || containsString(req.Argv, "all") {
			t.Fatalf("codexbar argv did not pin one provider: %#v", req.Argv)
		}
		provider := req.Argv[indexOfString(req.Argv, "--provider")+1]
		calls[provider]++
		if req.Cwd == "" || samePath(t, req.Cwd, ".") {
			t.Fatalf("codexbar cwd = %q, want neutral cwd", req.Cwd)
		}
		for _, entry := range req.Env {
			if strings.Contains(entry, "must-not-cross") || strings.HasPrefix(entry, "GIT_") || strings.HasPrefix(entry, "PWD=") || strings.Contains(strings.ToLower(entry), "token") || strings.Contains(strings.ToLower(entry), "key") {
				t.Fatalf("unsafe env reached codexbar: %q", entry)
			}
		}
		switch provider {
		case "codex":
			return CodexBarResult{Stdout: codexBarPayloadJSON(t, provider, "internal-protocol", "codexbar 1.2.3", []map[string]any{{
				"scope": map[string]any{
					"account_profile_id":  "jane@example.com",
					"model_capability_id": "/Users/jane/.config/provider/id.json",
				},
				"quantity_kind":          "requests",
				"provider_quantity_name": "requests",
				"unit":                   "requests",
				"window":                 map[string]any{"kind": "rolling", "rolling_duration_ms": int64((2 * time.Hour).Milliseconds()), "reset_at": formatTime(reset), "reset_semantics": "rolling"},
				"limit_value":            int64(100),
				"used_value":             int64(25),
				"remaining_value":        int64(75),
				"confidence":             "exact",
				"freshness_state":        "fresh",
			}}), ExitCode: 0}, nil
		case "claude":
			return CodexBarResult{Stdout: codexBarErrorJSON(t, provider, "credential-delegated", "codexbar 1.2.3", "rate-limited"), ExitCode: 0}, nil
		case "gemini":
			return CodexBarResult{Stdout: `{"schema_version":"future","provider":"gemini"}`, ExitCode: 0}, nil
		case "antigravity":
			return CodexBarResult{Stdout: "{not-json}", ExitCode: 0}, nil
		case "grok":
			return CodexBarResult{Stdout: codexBarPayloadJSON(t, provider, "browser-session", "codexbar 1.2.3", []map[string]any{{
				"quantity_kind":          "provider-defined",
				"provider_quantity_name": "credits",
				"unit":                   "credits",
				"window":                 map[string]any{"kind": "unbounded", "reset_semantics": "none"},
				"remaining_value":        int64(5),
				"confidence":             "estimated",
				"freshness_state":        "fresh",
			}}), ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected provider %q", provider)
		}
		return CodexBarResult{}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{ProviderInventory: config.ProviderInventory{CodexBar: config.CodexBar{
			Enabled: true,
			Providers: []config.CodexBarProviderOpt{
				{Provider: "codex", TrustClass: "internal-protocol"},
				{Provider: "claude", TrustClass: "credential-delegated"},
				{Provider: "gemini", TrustClass: "local-machine"},
				{Provider: "antigravity", TrustClass: "local-machine"},
				{Provider: "grok", TrustClass: "browser-session"},
			},
		}}},
		Now: fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, provider := range []string{"codex", "claude", "gemini", "antigravity", "grok"} {
		if calls[provider] != 1 {
			t.Fatalf("codexbar calls[%s] = %d, want exactly one", provider, calls[provider])
		}
	}
	codex := codexBarQuotaSnapshot(t, report, "codex")
	if codex.Confidence != ConfidenceExact || codex.RemainingValue == nil || *codex.RemainingValue != 75 || codex.AccountProfileID != nil || codex.ModelCapabilityID != nil {
		t.Fatalf("codex snapshot = %#v, want exact sanitized scope", codex)
	}
	if !strings.Contains(codex.RedactedDiagnostics, "codexbar 1.2.3") || !strings.Contains(codex.RedactedDiagnostics, "cmdfp_") || strings.Contains(codex.RedactedDiagnostics, "jane@example.com") {
		t.Fatalf("diagnostics missing provenance or leaked identity: %#v", codex.RedactedDiagnostics)
	}
	if got := codexBarQuotaSnapshot(t, report, "claude"); got.TerminalErrorCode != "ErrCodexBarPartialProviderFailure" || got.Confidence != ConfidenceUnavailable {
		t.Fatalf("claude partial failure = %#v", got)
	}
	if got := codexBarQuotaSnapshot(t, report, "gemini"); got.TerminalErrorCode != "ErrCodexBarSchemaUnsupported" {
		t.Fatalf("gemini schema failure = %#v", got)
	}
	if got := codexBarQuotaSnapshot(t, report, "antigravity"); got.TerminalErrorCode != "ErrCodexBarMalformedJSON" {
		t.Fatalf("antigravity malformed failure = %#v", got)
	}
	if got := codexBarQuotaSnapshot(t, report, "grok"); got.Confidence != ConfidenceEstimated || got.RemainingValue == nil || *got.RemainingValue != 5 {
		t.Fatalf("grok estimated snapshot = %#v", got)
	}
}

func TestCodexBarSecretCanaryRefusesBeforePersistence(t *testing.T) {
	exe := writeFakeCodexBar(t)
	accessKeyCanary := "AKIA" + strings.Repeat("A", 16)
	pathCanary := filepath.Join("Users", "jane", ".config", "provider", "credential.json")
	deps := codexBarDeps(t, exe, "codexbar 1.2.3")
	deps.RunCodexBar = func(context.Context, CodexBarRequest) (CodexBarResult, error) {
		payload := map[string]any{
			"schema_version":   codexBarQuotaSchema,
			"codexbar_version": "codexbar 1.2.3",
			"provider":         "codex",
			"source":           map[string]any{"trust_class": "internal-protocol", "confidence": "exact", "freshness": "fresh"},
			"snapshots": []any{map[string]any{
				"quantity_kind":          "requests",
				"provider_quantity_name": "requests",
				"unit":                   "requests",
				"window":                 map[string]any{"kind": "unbounded", "reset_semantics": "none"},
				"remaining_value":        int64(1),
				"confidence":             "exact",
			}},
			"ignored_identity":       "jane@example.com",
			"ignored_credential_ref": pathCanary,
			"ignored_secret":         accessKeyCanary,
		}
		data, _ := json.Marshal(payload)
		return CodexBarResult{Stdout: string(data), ExitCode: 0}, nil
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{ProviderInventory: config.ProviderInventory{CodexBar: config.CodexBar{
			Enabled:   true,
			Providers: []config.CodexBarProviderOpt{{Provider: "codex", TrustClass: "internal-protocol"}},
		}}},
		Now: fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal report: %v", err)
	}
	for _, forbidden := range []string{accessKeyCanary, strings.Repeat("A", 16), "jane@example.com", pathCanary, "ignored_secret", "ignored_credential_ref"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("codexbar report leaked %q: %s", forbidden, data)
		}
	}
	got := codexBarQuotaSnapshot(t, report, "codex")
	if got.TerminalErrorCode != "ErrQuotaCredentialMaterial" || got.RawSourceHash != "" {
		t.Fatalf("secret failure snapshot = %#v, want credential material refusal without raw hash", got)
	}
}

func TestCodexBarFailureFixturesAreTypedAndIsolated(t *testing.T) {
	exe := writeFakeCodexBar(t)
	cases := []struct {
		name     string
		result   CodexBarResult
		err      error
		wantCode string
		wantGap  string
	}{
		{name: "timeout", result: CodexBarResult{TimedOut: true, Killed: true, ExitCode: -1}, wantCode: "ErrCodexBarTimeout", wantGap: "codexbar-timeout"},
		{name: "truncated", result: CodexBarResult{Truncated: true, ExitCode: 0}, wantCode: "ErrCodexBarOutputTruncated", wantGap: "codexbar-output-truncated"},
		{name: "nonzero", result: CodexBarResult{ExitCode: 2}, wantCode: "ErrCodexBarNonZeroExit", wantGap: "codexbar-nonzero-exit"},
		{name: "execution", result: CodexBarResult{ExitCode: -1}, err: errors.New("boom"), wantCode: "ErrCodexBarExecutionFailed", wantGap: "codexbar-execution-failed"},
		{name: "trailing-json", result: CodexBarResult{Stdout: codexBarPayloadJSON(t, "codex", "internal-protocol", "codexbar 1.2.3", []map[string]any{{"quantity_kind": "requests", "window": map[string]any{"kind": "unbounded", "reset_semantics": "none"}, "confidence": "exact"}}) + "\n{}", ExitCode: 0}, wantCode: "ErrCodexBarMalformedJSON", wantGap: "codexbar-malformed-json"},
		{name: "trust-mismatch", result: CodexBarResult{Stdout: codexBarPayloadJSON(t, "codex", "browser-session", "codexbar 1.2.3", []map[string]any{{"quantity_kind": "requests", "window": map[string]any{"kind": "unbounded", "reset_semantics": "none"}, "confidence": "exact"}}), ExitCode: 0}, wantCode: "ErrCodexBarTrustDenied", wantGap: "codexbar-trust-denied"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			deps := codexBarDeps(t, exe, "codexbar 1.2.3")
			deps.RunCodexBar = func(context.Context, CodexBarRequest) (CodexBarResult, error) {
				return tt.result, tt.err
			}
			report, err := Discover(context.Background(), Options{
				Config: config.Config{ProviderInventory: config.ProviderInventory{CodexBar: config.CodexBar{
					Enabled:   true,
					Providers: []config.CodexBarProviderOpt{{Provider: "codex", TrustClass: "internal-protocol"}},
				}}},
				Now: fixedInventoryNow,
			}, deps)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			got := codexBarQuotaSnapshot(t, report, "codex")
			if got.TerminalErrorCode != tt.wantCode || !containsString(got.GapReasons, tt.wantGap) {
				t.Fatalf("snapshot = %#v, want %s/%s", got, tt.wantCode, tt.wantGap)
			}
		})
	}
}

func TestRunCodexBarQuotaTimeoutKillsProcess(t *testing.T) {
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

func TestCodexBarSnapshotsPersistNormalizedOnly(t *testing.T) {
	exe := writeFakeCodexBar(t)
	deps := codexBarDeps(t, exe, "codexbar 1.2.3")
	deps.RunCodexBar = func(context.Context, CodexBarRequest) (CodexBarResult, error) {
		return CodexBarResult{Stdout: codexBarPayloadJSON(t, "codex", "internal-protocol", "codexbar 1.2.3", []map[string]any{{
			"quantity_kind":          "total-tokens",
			"provider_quantity_name": "tokens",
			"unit":                   "tokens",
			"window":                 map[string]any{"kind": "fixed-day", "start": formatTime(fixedInventoryNow()), "end": formatTime(fixedInventoryNow().Add(24 * time.Hour)), "reset_at": formatTime(fixedInventoryNow().Add(24 * time.Hour)), "reset_semantics": "window-boundary"},
			"limit_value":            int64(1000),
			"used_value":             int64(100),
			"remaining_value":        int64(900),
			"confidence":             "exact",
		}}), ExitCode: 0}, nil
	}
	report, err := Discover(context.Background(), Options{
		Config: config.Config{ProviderInventory: config.ProviderInventory{CodexBar: config.CodexBar{
			Enabled:   true,
			Providers: []config.CodexBarProviderOpt{{Provider: "codex", TrustClass: "internal-protocol"}},
		}}},
		Now: fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	source := codexBarQuotaSourceFor(t, report, "codex")
	snapshot := codexBarQuotaSnapshot(t, report, "codex")
	if err := ValidateQuotaTelemetrySource(source); err != nil {
		t.Fatalf("ValidateQuotaTelemetrySource: %v", err)
	}
	if err := ValidateQuotaSnapshot(source, snapshot); err != nil {
		t.Fatalf("ValidateQuotaSnapshot: %v\n%#v", err, snapshot)
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
		t.Fatalf("Close: %v", err)
	}
	reopened, err := storage.Open(context.Background(), storage.Options{Path: storePath, Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open reopened: %v", err)
	}
	defer reopened.Close()
	loaded, err := Load(context.Background(), reopened)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	loadedSnapshot := codexBarQuotaSnapshot(t, loaded, "codex")
	if loadedSnapshot.RawSourceHash != "" || loadedSnapshot.RemainingValue == nil || *loadedSnapshot.RemainingValue != 900 {
		t.Fatalf("loaded codexbar snapshot = %#v", loadedSnapshot)
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

func codexBarPayloadJSON(t *testing.T, provider, trustClass, version string, snapshots []map[string]any) string {
	t.Helper()
	payload := map[string]any{
		"schema_version":   codexBarQuotaSchema,
		"codexbar_version": version,
		"provider":         provider,
		"source":           map[string]any{"trust_class": trustClass, "confidence": "exact", "freshness": "fresh"},
		"snapshots":        snapshots,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	return string(data)
}

func codexBarErrorJSON(t *testing.T, provider, trustClass, version, code string) string {
	t.Helper()
	payload := map[string]any{
		"schema_version":   codexBarQuotaSchema,
		"codexbar_version": version,
		"provider":         provider,
		"source":           map[string]any{"trust_class": trustClass, "confidence": "unavailable", "freshness": "not-applicable"},
		"error":            map[string]any{"code": code},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal error payload: %v", err)
	}
	return string(data)
}

func codexBarQuotaSnapshot(t *testing.T, report Report, provider string) QuotaSnapshot {
	t.Helper()
	for _, snapshot := range report.QuotaSnapshots {
		if snapshot.AdapterID == provider {
			for _, source := range report.QuotaTelemetrySources {
				if source.QuotaSourceID == snapshot.QuotaSourceID && source.SourceSchemaVersion == codexBarQuotaSchema {
					return snapshot
				}
			}
		}
	}
	t.Fatalf("codexbar quota snapshot for %s missing in %#v", provider, report.QuotaSnapshots)
	return QuotaSnapshot{}
}

func codexBarQuotaSourceFor(t *testing.T, report Report, provider string) QuotaTelemetrySource {
	t.Helper()
	for _, source := range report.QuotaTelemetrySources {
		if source.AdapterID == provider && source.SourceSchemaVersion == codexBarQuotaSchema {
			return source
		}
	}
	t.Fatalf("codexbar quota source for %s missing in %#v", provider, report.QuotaTelemetrySources)
	return QuotaTelemetrySource{}
}

func indexOfString(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func samePath(t *testing.T, got, want string) bool {
	t.Helper()
	gotAbs, err := filepath.Abs(got)
	if err != nil {
		t.Fatalf("Abs got: %v", err)
	}
	wantAbs, err := filepath.Abs(want)
	if err != nil {
		t.Fatalf("Abs want: %v", err)
	}
	return filepath.Clean(gotAbs) == filepath.Clean(wantAbs)
}

func runFakeCodexBar() {
	switch os.Getenv("LOOPCODER_FAKE_CODEXBAR") {
	case "hang":
		time.Sleep(10 * time.Second)
	default:
		fmt.Println(`{"schema_version":"codexbar.quota_bridge.v1","codexbar_version":"codexbar test","provider":"codex","source":{"trust_class":"local-machine","confidence":"exact","freshness":"fresh"},"snapshots":[]}`)
	}
}
