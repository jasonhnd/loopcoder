package providerinventory

import (
	"bufio"
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
)

func init() {
	if os.Getenv("LOOPCODER_FAKE_APP_SERVER") == "" {
		return
	}
	runFakeCodexAppServer()
	os.Exit(0)
}

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
	if got := len(quotaSnapshotsFor(report, "codex")); got != 1 {
		t.Fatalf("codex quota snapshots = %d, want exactly one no-grant snapshot", got)
	}
	if got := len(quotaSourcesFor(report, "codex")); got != 1 {
		t.Fatalf("codex quota sources = %d, want exactly one no-grant source", got)
	}
}

func TestCodexQuotaCollectsFiveHourWeeklyCreditsAndScopes(t *testing.T) {
	exe := writeFakeCodex(t)
	resetFiveHour := fixedInventoryNow().Add(5 * time.Hour).Unix()
	resetWeek := fixedInventoryNow().Add(7 * 24 * time.Hour).Unix()
	resetCreditExpiry := fixedInventoryNow().Add(24 * time.Hour).Unix()
	stdout := codexQuotaFrames(t,
		map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "planType": "pro", "id": "acct_fixture"}},
		map[string]any{
			"rateLimits": map[string]any{
				"limitId":   "codex",
				"planType":  "pro",
				"primary":   map[string]any{"usedPercent": 25, "windowDurationMins": 300, "resetsAt": resetFiveHour},
				"secondary": map[string]any{"usedPercent": 80, "windowDurationMins": 10080, "resetsAt": resetWeek},
				"credits":   map[string]any{"balance": "42.75", "hasCredits": true, "unlimited": false},
				"future":    map[string]any{"ignored": true},
			},
			"rateLimitsByLimitId": map[string]any{
				"workspace": map[string]any{
					"limitId": "workspace",
					"primary": map[string]any{"usedPercent": 10, "windowDurationMins": 300, "resetsAt": resetFiveHour},
				},
			},
			"rateLimitResetCredits": map[string]any{
				"availableCount": 2,
				"credits": []any{
					map[string]any{"id": "reset_fixture", "resetType": "codexRateLimits", "status": "available", "grantedAt": fixedInventoryNow().Unix(), "expiresAt": resetCreditExpiry},
				},
			},
		},
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
	reportJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal report: %v", err)
	}
	if strings.Contains(string(reportJSON), "usedPercent") || strings.Contains(string(reportJSON), stdout) {
		t.Fatalf("report retained raw app-server protocol payload: %s", reportJSON)
	}
	codexSnapshots := quotaSnapshotsFor(report, "codex")
	if len(codexSnapshots) != 5 {
		t.Fatalf("codex quota snapshots = %d: %#v", len(codexSnapshots), report.QuotaSnapshots)
	}
	byName := map[string]QuotaSnapshot{}
	for _, snapshot := range codexSnapshots {
		byName[snapshot.ProviderQuantityName+"|"+string(snapshot.WindowKind)+"|"+snapshot.ScopeKey] = snapshot
		if strings.Contains(snapshot.RedactedDiagnostics, "secret-should-not-cross") {
			t.Fatalf("diagnostics retained credential canary: %#v", snapshot)
		}
	}
	defaultPrimaryScope := "provider:codex/account:acct_fixture/scope:codex/detail:primary"
	five := byName["primary_used_percent|rolling|"+defaultPrimaryScope]
	if five.RollingDurationMS != int64((5*time.Hour).Milliseconds()) || five.UsedValue == nil || *five.UsedValue != 25 || five.RemainingValue == nil || *five.RemainingValue != 75 || five.Unit != "percent" {
		t.Fatalf("five-hour snapshot = %#v", five)
	}
	if five.LimitValue != nil || five.FieldConfidences["remaining_value"] != ConfidenceEstimated || !containsString(five.GapReasons, "remaining-derived-from-used-percent") {
		t.Fatalf("five-hour percentage semantics = %#v, want no fabricated limit and derived remaining", five)
	}
	weekly := byName["secondary_used_percent|fixed-week|provider:codex/account:acct_fixture/scope:codex/detail:secondary"]
	if weekly.ResetAt != formatTime(time.Unix(resetWeek, 0)) || weekly.RollingDurationMS != int64((7*24*time.Hour).Milliseconds()) {
		t.Fatalf("weekly snapshot = %#v", weekly)
	}
	credits := byName["credits_balance|unbounded|provider:codex/account:acct_fixture/scope:codex/detail:credits"]
	if credits.QuantityKind != QuantityProviderDefined || credits.RemainingValue == nil || *credits.RemainingValue != 4275 || credits.ValueScale != 2 {
		t.Fatalf("credits snapshot = %#v", credits)
	}
	workspace := byName["primary_used_percent|rolling|provider:codex/account:acct_fixture/scope:workspace/detail:primary"]
	if workspace.RemainingValue == nil || *workspace.RemainingValue != 90 {
		t.Fatalf("workspace snapshot = %#v", workspace)
	}
	resetCredits := byName["rate_limit_reset_credits|unbounded|provider:codex/account:acct_fixture/scope:default/detail:reset_credits"]
	if resetCredits.RemainingValue == nil || *resetCredits.RemainingValue != 2 || resetCredits.ValidUntil != formatTime(time.Unix(resetCreditExpiry, 0)) {
		t.Fatalf("reset credits snapshot = %#v", resetCredits)
	}
}

func TestCodexQuotaAbsentFieldsRemainUnknown(t *testing.T) {
	source := codexQuotaSource(fixedInventoryNow())
	account := map[string]any{"account": map[string]any{"id": "acct_fixture"}}
	limits := map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"windowDurationMins": 300}}}
	snapshots, err := snapshotsFromCodexRateLimits(source, nil, "codex 0.9.0", account, limits, []json.RawMessage{json.RawMessage(`{"result":true}`)}, fixedInventoryNow())
	if err != nil {
		t.Fatalf("snapshotsFromCodexRateLimits: %v", err)
	}
	got := snapshots[0]
	if got.LimitValue != nil || got.UsedValue != nil || got.RemainingValue != nil || got.FieldConfidences["limit_value"] != ConfidenceUnknown || got.FieldConfidences["used_value"] != ConfidenceUnknown {
		t.Fatalf("absent fields snapshot = %#v, want nil unknowns", got)
	}
	if !containsString(got.GapReasons, "missing-used-percent") || !containsString(got.GapReasons, "missing-reset-at") {
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
		{name: "malformed-jsonl", version: "codex 0.9.0", result: CodexAppServerResult{Stdout: "{not-json}\n", ExitCode: 0}, wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{name: "oversized-jsonl", version: "codex 0.9.0", result: CodexAppServerResult{Stdout: strings.Repeat("x", codexQuotaLineBytes+1), ExitCode: 0}, wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{name: "rpc-error", version: "codex 0.9.0", result: CodexAppServerResult{Stdout: codexQuotaRPCErrorFrames(t), ExitCode: 0}, wantCode: "ErrCodexQuotaRPCError", wantGap: "rpc-error"},
		{name: "timeout", version: "codex 0.9.0", result: CodexAppServerResult{TimedOut: true, Killed: true, ExitCode: -1}, wantCode: "ErrCodexQuotaTimeout", wantGap: "quota-probe-timeout"},
		{name: "transport-loss", version: "codex 0.9.0", result: CodexAppServerResult{ExitCode: -1}, err: errors.New("eof"), wantCode: "ErrCodexQuotaExecutionFailed", wantGap: "quota-probe-failed"},
		{name: "nonzero", version: "codex 0.9.0", result: CodexAppServerResult{ExitCode: 2}, wantCode: "ErrCodexQuotaNonZeroExit", wantGap: "quota-probe-nonzero-exit"},
		{name: "unsupported-schema", version: "codex 0.7.0", result: CodexAppServerResult{Stdout: codexQuotaUnsupportedInitialize(t), ExitCode: 0}, wantCode: "ErrUnsupportedVersion", wantGap: "unsupported-cli-version"},
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
			if got := len(quotaSnapshotsFor(report, "codex")); got != 1 {
				t.Fatalf("codex quota snapshots = %d, want exactly one failure snapshot", got)
			}
			if got := len(quotaSourcesFor(report, "codex")); got != 1 {
				t.Fatalf("codex quota sources = %d, want exactly one failure source", got)
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

func TestCodexQuotaAttemptsOnlyOneCandidate(t *testing.T) {
	first := writeFakeCodex(t)
	second := writeFakeCodex(t)
	stdout := codexQuotaFrames(t,
		map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "id": "acct_fixture"}},
		map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 1, "windowDurationMins": 300, "resetsAt": fixedInventoryNow().Add(time.Hour).Unix()}}},
	)
	deps := codexQuotaDeps(t, first, "codex 0.9.0", CodexAppServerResult{Stdout: stdout, ExitCode: 0}, nil)
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return filepath.Dir(first) + string(os.PathListSeparator) + filepath.Dir(second)
		}
		return ""
	}
	calls := 0
	deps.RunCodexRPC = func(_ context.Context, req CodexAppServerRequest) (CodexAppServerResult, error) {
		calls++
		if req.Argv[0] != first {
			t.Fatalf("RunCodexRPC argv[0] = %q, want first candidate %q", req.Argv[0], first)
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
	if calls != 1 {
		t.Fatalf("RunCodexRPC calls = %d, want exactly one", calls)
	}
	if got := len(quotaSourcesFor(report, "codex")); got != 1 {
		t.Fatalf("codex quota sources = %d, want one", got)
	}
}

func TestRunCodexAppServerUsesJSONLHandshakeAndRefreshTokenFalse(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	result, err := runCodexAppServer(context.Background(), CodexAppServerRequest{
		Argv:               []string{exe},
		Env:                []string{"LOOPCODER_FAKE_APP_SERVER=success", "LOOPCODER_FAKE_APP_SERVER_TRACE=" + trace},
		Timeout:            2 * time.Second,
		StdoutLimitBytes:   codexQuotaOutputBytes,
		StderrLimitBytes:   StdoutLimitBytes,
		CombinedLimitBytes: codexQuotaOutputBytes + StdoutLimitBytes,
	})
	if err != nil {
		t.Fatalf("runCodexAppServer: %v, stderr=%s", err, result.Stderr)
	}
	if result.ExitCode != 0 || result.TimedOut || result.Killed {
		t.Fatalf("result = %#v, want clean exit", result)
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("ReadFile trace: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 {
		t.Fatalf("trace lines = %d: %s", len(lines), data)
	}
	var got []jsonRPCMessage
	for _, line := range lines {
		msg, err := decodeCodexJSONLMessage(line)
		if err != nil {
			t.Fatalf("decode trace: %v", err)
		}
		got = append(got, msg)
	}
	if got[0].Method != "initialize" || got[1].Method != "initialized" || got[2].Method != "account/read" || got[3].Method != "account/rateLimits/read" {
		t.Fatalf("methods = %#v, want initialize/initialized/account/read/account/rateLimits/read", []string{got[0].Method, got[1].Method, got[2].Method, got[3].Method})
	}
	var accountParams map[string]bool
	data, _ = json.Marshal(got[2].Params)
	if err := json.Unmarshal(data, &accountParams); err != nil {
		t.Fatalf("account params: %v", err)
	}
	if accountParams["refreshToken"] {
		t.Fatalf("account/read params = %#v, want refreshToken false", got[2].Params)
	}
	if strings.Contains(result.Stdout, "Content-Length") {
		t.Fatalf("stdout retained content-length frame: %q", result.Stdout)
	}
}

func TestRunCodexAppServerTimeoutKillsProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	result, err := runCodexAppServer(context.Background(), CodexAppServerRequest{
		Argv:               []string{exe},
		Env:                []string{"LOOPCODER_FAKE_APP_SERVER=hang"},
		Timeout:            100 * time.Millisecond,
		StdoutLimitBytes:   codexQuotaOutputBytes,
		StderrLimitBytes:   StdoutLimitBytes,
		CombinedLimitBytes: codexQuotaOutputBytes + StdoutLimitBytes,
	})
	if err == nil {
		t.Fatalf("runCodexAppServer err = nil, want timeout error")
	}
	if !result.TimedOut || !result.Killed {
		t.Fatalf("result = %#v, want timed out and killed", result)
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
	return strings.Join([]string{
		codexQuotaJSONL(t, jsonRPCMessage{JSONRPC: "2.0", Method: "codex/notice", Result: json.RawMessage(`{"ignored":true}`)}),
		codexQuotaJSONL(t, jsonRPCMessage{JSONRPC: "2.0", ID: 1, Result: mustRawJSON(t, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test"})}),
		codexQuotaJSONL(t, jsonRPCMessage{JSONRPC: "2.0", ID: 2, Result: mustRawJSON(t, account)}),
		codexQuotaJSONL(t, jsonRPCMessage{JSONRPC: "2.0", ID: 3, Result: mustRawJSON(t, limits)}),
	}, "\n") + "\n"
}

func codexQuotaRPCErrorFrames(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		codexQuotaJSONL(t, jsonRPCMessage{JSONRPC: "2.0", ID: 1, Result: mustRawJSON(t, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test"})}),
		codexQuotaJSONL(t, jsonRPCMessage{JSONRPC: "2.0", ID: 2, Result: mustRawJSON(t, map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"id": "acct_fixture"}})}),
		codexQuotaJSONL(t, jsonRPCMessage{JSONRPC: "2.0", ID: 3, Error: &jsonRPCError{Code: -32000, Message: "rate limited"}}),
	}, "\n") + "\n"
}

func codexQuotaUnsupportedInitialize(t *testing.T) string {
	t.Helper()
	return codexQuotaJSONL(t, jsonRPCMessage{JSONRPC: "2.0", ID: 1, Result: mustRawJSON(t, map[string]any{"protocolVersion": "future"})}) + "\n"
}

func codexQuotaJSONL(t *testing.T, message jsonRPCMessage) string {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("Marshal JSONL: %v", err)
	}
	return string(data)
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

func quotaSourcesFor(report Report, adapterID string) []QuotaTelemetrySource {
	var sources []QuotaTelemetrySource
	for _, source := range report.QuotaTelemetrySources {
		if source.AdapterID == adapterID {
			sources = append(sources, source)
		}
	}
	return sources
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

func runFakeCodexAppServer() {
	mode := os.Getenv("LOOPCODER_FAKE_APP_SERVER")
	if mode == "hang" {
		time.Sleep(10 * time.Second)
		return
	}
	tracePath := os.Getenv("LOOPCODER_FAKE_APP_SERVER_TRACE")
	var trace *os.File
	if tracePath != "" {
		file, err := os.Create(tracePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer file.Close()
		trace = file
	}
	scanner := bufio.NewScanner(os.Stdin)
	writeResponse := func(id int, result any) {
		data, _ := json.Marshal(jsonRPCMessage{JSONRPC: "2.0", ID: id, Result: mustRawJSONNoT(result)})
		fmt.Println(string(data))
	}
	read := func() jsonRPCMessage {
		if !scanner.Scan() {
			fmt.Fprintln(os.Stderr, "missing request")
			os.Exit(3)
		}
		line := scanner.Text()
		if trace != nil {
			fmt.Fprintln(trace, line)
		}
		msg, err := decodeCodexJSONLMessage(line)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		return msg
	}
	initMsg := read()
	if initMsg.Method != "initialize" {
		fmt.Fprintf(os.Stderr, "first method = %s\n", initMsg.Method)
		os.Exit(5)
	}
	writeResponse(1, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test"})
	initialized := read()
	if initialized.Method != "initialized" {
		fmt.Fprintf(os.Stderr, "second method = %s\n", initialized.Method)
		os.Exit(6)
	}
	account := read()
	if account.Method != "account/read" {
		fmt.Fprintf(os.Stderr, "third method = %s\n", account.Method)
		os.Exit(7)
	}
	rateLimits := read()
	if rateLimits.Method != "account/rateLimits/read" {
		fmt.Fprintf(os.Stderr, "fourth method = %s\n", rateLimits.Method)
		os.Exit(8)
	}
	writeResponse(2, map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "id": "acct_fixture"}})
	writeResponse(3, map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 10, "windowDurationMins": 300, "resetsAt": fixedInventoryNow().Add(time.Hour).Unix()}}})
}

func mustRawJSONNoT(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
