package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
)

func TestCodexQuotaCollectionRequiresExplicitGrantAndDoesNotLaunch(t *testing.T) {
	exe := writeFakeCodex(t)
	deps := codexQuotaDeps(t, exe, "codex 0.9.0", CodexAppServerResult{}, nil)
	called := false
	deps.RunCodexRPC = func(context.Context, CodexAppServerRequest) (CodexAppServerResult, error) {
		called = true
		return CodexAppServerResult{}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Now:    fixedInventoryNow,
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if called {
		t.Fatal("RunCodexRPC was called without quota telemetry grant")
	}
	snapshot := onlyQuotaSnapshot(t, report, "codex")
	if snapshot.TerminalErrorCode != "ErrQuotaCollectionGrantRequired" || !containsString(snapshot.GapReasons, "quota-collection-not-granted") {
		t.Fatalf("codex quota snapshot = %#v, want grant-required unavailable", snapshot)
	}
}

func TestCodexQuotaCollectsFiveHourWeeklyCreditsAndScopes(t *testing.T) {
	exe := writeFakeCodex(t)
	stdout := codexQuotaFrames(t,
		map[string]any{"account": map[string]any{"id": "acct_fixture"}},
		map[string]any{"rateLimits": []any{
			map[string]any{"window": "five-hour", "scope": "default", "limit": 100, "used": 25, "remaining": 75, "resetAt": "2026-07-12T05:00:00Z"},
			map[string]any{"window": "weekly", "scope": "team", "limit": 500, "used": 125, "remaining": 375, "windowStart": "2026-07-06T00:00:00Z", "windowEnd": "2026-07-13T00:00:00Z"},
			map[string]any{"type": "credits", "scope": "billing", "balance": 42},
		}},
	)
	var gotReq CodexAppServerRequest
	deps := codexQuotaDeps(t, exe, "codex 0.9.0", CodexAppServerResult{Stdout: stdout, ExitCode: 0}, nil)
	deps.Getenv = func(key string) string {
		switch key {
		case "PATH":
			return filepath.Dir(exe)
		case "HOME":
			return "/home/codex-fixture"
		case "OPENAI_API_KEY", "CODEX_AUTH_TOKEN", "LOOPCODER_TEST_CREDENTIAL":
			return "secret-should-not-cross"
		default:
			return ""
		}
	}
	deps.RunCodexRPC = func(_ context.Context, req CodexAppServerRequest) (CodexAppServerResult, error) {
		gotReq = req
		for _, arg := range req.Argv {
			if strings.Contains(arg, "login") || strings.Contains(arg, "update") || strings.Contains(arg, "exec") {
				t.Fatalf("unsafe codex quota argv: %#v", req.Argv)
			}
		}
		for _, entry := range req.Env {
			if strings.Contains(entry, "secret-should-not-cross") || strings.Contains(strings.ToLower(entry), "token") || strings.Contains(strings.ToLower(entry), "key") {
				t.Fatalf("credential-like env reached codex app-server: %q", entry)
			}
		}
		return CodexAppServerResult{Stdout: stdout, ExitCode: 0}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Now:    fixedInventoryNow,
		NetworkGrants: []NetworkGrant{{
			ProviderID: "codex",
			Purpose:    NetworkPurposeQuotaTelemetry,
			Scope:      NetworkScopeMachineInventory,
		}},
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{exe, "-s", "read-only", "-a", "untrusted", "app-server"}; !sameStrings(gotReq.Argv, want) {
		t.Fatalf("codex argv = %#v, want %#v", gotReq.Argv, want)
	}
	codexSnapshots := quotaSnapshotsFor(report, "codex")
	if len(codexSnapshots) != 3 {
		t.Fatalf("codex quota snapshots = %d: %#v", len(codexSnapshots), report.QuotaSnapshots)
	}
	byName := map[string]QuotaSnapshot{}
	for _, snapshot := range codexSnapshots {
		byName[snapshot.ProviderQuantityName+"|"+string(snapshot.WindowKind)] = snapshot
		if strings.Contains(snapshot.RedactedDiagnostics, "secret-should-not-cross") {
			t.Fatalf("diagnostics retained credential canary: %#v", snapshot)
		}
	}
	five := byName["five_hour|rolling"]
	if five.RollingDurationMS != int64((5*time.Hour).Milliseconds()) || five.RemainingValue == nil || *five.RemainingValue != 75 || five.ScopeKey != "provider:codex/account:acct_fixture/scope:default" {
		t.Fatalf("five-hour snapshot = %#v", five)
	}
	weekly := byName["weekly|fixed-week"]
	if weekly.WindowStart == "" || weekly.WindowEnd == "" || weekly.LimitValue == nil || *weekly.LimitValue != 500 {
		t.Fatalf("weekly snapshot = %#v", weekly)
	}
	credits := byName["credits|unbounded"]
	if credits.QuantityKind != QuantityProviderDefined || credits.RemainingValue == nil || *credits.RemainingValue != 42 {
		t.Fatalf("credits snapshot = %#v", credits)
	}
}

func TestCodexQuotaAbsentFieldsRemainUnknown(t *testing.T) {
	source := codexQuotaSource(fixedInventoryNow())
	account := map[string]any{"account": map[string]any{"id": "acct_fixture"}}
	limits := map[string]any{"rateLimits": []any{map[string]any{"window": "five-hour", "remaining": 9}}}
	snapshots, err := snapshotsFromCodexRateLimits(source, nil, "codex 0.9.0", account, limits, []json.RawMessage{json.RawMessage(`{"result":true}`)}, fixedInventoryNow())
	if err != nil {
		t.Fatalf("snapshotsFromCodexRateLimits: %v", err)
	}
	got := snapshots[0]
	if got.LimitValue != nil || got.UsedValue != nil || got.FieldConfidences["limit_value"] != ConfidenceUnknown || got.FieldConfidences["used_value"] != ConfidenceUnknown {
		t.Fatalf("absent fields snapshot = %#v, want nil unknowns", got)
	}
	if !containsString(got.GapReasons, "missing-limit") || !containsString(got.GapReasons, "missing-used") {
		t.Fatalf("gap reasons = %#v, want missing fields", got.GapReasons)
	}
}

func TestCodexQuotaFailureFixturesLeaveUnavailableSnapshot(t *testing.T) {
	exe := writeFakeCodex(t)
	cases := []struct {
		name     string
		version  string
		result   CodexAppServerResult
		err      error
		wantCode string
		wantGap  string
	}{
		{name: "malformed-frame", version: "codex 0.9.0", result: CodexAppServerResult{Stdout: "Content-Length: nope\r\n\r\n{}", ExitCode: 0}, wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{name: "rpc-error", version: "codex 0.9.0", result: CodexAppServerResult{Stdout: codexQuotaRPCErrorFrames(t), ExitCode: 0}, wantCode: "ErrCodexQuotaRPCError", wantGap: "rpc-error"},
		{name: "timeout", version: "codex 0.9.0", result: CodexAppServerResult{TimedOut: true, Killed: true, ExitCode: -1}, wantCode: "ErrCodexQuotaTimeout", wantGap: "quota-probe-timeout"},
		{name: "transport-loss", version: "codex 0.9.0", result: CodexAppServerResult{ExitCode: -1}, err: errors.New("eof"), wantCode: "ErrCodexQuotaExecutionFailed", wantGap: "quota-probe-failed"},
		{name: "nonzero", version: "codex 0.9.0", result: CodexAppServerResult{ExitCode: 2}, wantCode: "ErrCodexQuotaNonZeroExit", wantGap: "quota-probe-nonzero-exit"},
		{name: "unsupported-version", version: "codex 0.7.0", result: CodexAppServerResult{Stdout: codexQuotaFrames(t, map[string]any{"account": map[string]any{"id": "acct_fixture"}}, map[string]any{"rateLimits": []any{}}), ExitCode: 0}, wantCode: "ErrUnsupportedVersion", wantGap: "unsupported-cli-version"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			deps := codexQuotaDeps(t, exe, tt.version, tt.result, tt.err)
			report, err := Discover(context.Background(), Options{
				Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
				Now:    fixedInventoryNow,
				NetworkGrants: []NetworkGrant{{
					ProviderID: "codex",
					Purpose:    NetworkPurposeQuotaTelemetry,
					Scope:      NetworkScopeMachineInventory,
				}},
			}, deps)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			got := onlyQuotaSnapshot(t, report, "codex")
			if got.TerminalErrorCode != tt.wantCode || got.Confidence != ConfidenceUnavailable || !containsString(got.GapReasons, tt.wantGap) {
				t.Fatalf("snapshot = %#v, want %s/%s unavailable", got, tt.wantCode, tt.wantGap)
			}
		})
	}
}

func TestCodexQuotaCredentialCanaryDoesNotPersist(t *testing.T) {
	exe := writeFakeCodex(t)
	canary := "api_key=sk-" + strings.Repeat("A", 24)
	deps := codexQuotaDeps(t, exe, "codex 0.9.0", CodexAppServerResult{Stdout: canary, ExitCode: 0}, nil)
	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex"}},
		Now:    fixedInventoryNow,
		NetworkGrants: []NetworkGrant{{
			ProviderID: "codex",
			Purpose:    NetworkPurposeQuotaTelemetry,
			Scope:      NetworkScopeMachineInventory,
		}},
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), canary) || strings.Contains(string(data), strings.Repeat("A", 24)) {
		t.Fatalf("credential canary leaked into report: %s", data)
	}
	got := onlyQuotaSnapshot(t, report, "codex")
	if got.TerminalErrorCode != "ErrQuotaCredentialMaterial" || got.RawSourceHash != "" {
		t.Fatalf("credential snapshot = %#v, want redacted failure without raw hash", got)
	}
}

func codexQuotaDeps(t *testing.T, exe, version string, result CodexAppServerResult, runErr error) Deps {
	t.Helper()
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): version})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return filepath.Dir(exe)
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		return ProbeExecutionResult{Stdout: version + "\n", ExitCode: 0}, nil
	}
	deps.RunCodexRPC = func(context.Context, CodexAppServerRequest) (CodexAppServerResult, error) {
		return result, runErr
	}
	return deps
}

func writeFakeCodex(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, executableName("codex"))
	writeExecutable(t, path)
	return path
}

func codexQuotaFrames(t *testing.T, account, limits map[string]any) string {
	t.Helper()
	return encodeJSONRPCFrame(jsonRPCMessage{JSONRPC: "2.0", Method: "codex/notice", Result: json.RawMessage(`{"ignored":true}`)}) +
		encodeJSONRPCFrame(jsonRPCMessage{JSONRPC: "2.0", ID: 1, Result: mustRawJSON(t, account)}) +
		encodeJSONRPCFrame(jsonRPCMessage{JSONRPC: "2.0", ID: 2, Result: mustRawJSON(t, limits)})
}

func codexQuotaRPCErrorFrames(t *testing.T) string {
	t.Helper()
	return encodeJSONRPCFrame(jsonRPCMessage{JSONRPC: "2.0", ID: 1, Result: mustRawJSON(t, map[string]any{"account": map[string]any{"id": "acct_fixture"}})}) +
		encodeJSONRPCFrame(jsonRPCMessage{JSONRPC: "2.0", ID: 2, Error: &jsonRPCError{Code: -32000, Message: "rate limited"}})
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}

func onlyQuotaSnapshot(t *testing.T, report Report, adapterID string) QuotaSnapshot {
	t.Helper()
	snapshots := quotaSnapshotsFor(report, adapterID)
	if len(snapshots) > 0 {
		return snapshots[0]
	}
	t.Fatalf("quota snapshot for %s missing in %#v", adapterID, report.QuotaSnapshots)
	return QuotaSnapshot{}
}

func quotaSnapshotsFor(report Report, adapterID string) []QuotaSnapshot {
	var snapshots []QuotaSnapshot
	for _, snapshot := range report.QuotaSnapshots {
		if snapshot.AdapterID == adapterID {
			snapshots = append(snapshots, snapshot)
		}
	}
	return snapshots
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
