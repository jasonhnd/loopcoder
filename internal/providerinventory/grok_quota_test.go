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
)

func TestGrokACPBillingCollectsUsageCostCreditsAndDistinctAuthorities(t *testing.T) {
	exe := writeFakeGrok(t)
	resetAt := fixedInventoryNow().Add(2 * time.Hour)
	stdout := grokACPFrames(t, map[string]any{
		"account":                   map[string]any{"id": "acct_grok"},
		"model":                     "grok-4.5",
		"usage":                     map[string]any{"input_tokens": 11, "output_tokens": 7, "total_tokens": 18},
		"cost_usd":                  "0.0123",
		"consumer_weekly_allowance": map[string]any{"limit": 100, "remaining": 40, "reset_at": resetAt.Format(time.RFC3339Nano), "window": "weekly"},
		"build_session_allowance":   map[string]any{"limit": 20, "remaining": 8},
		"api_credits":               map[string]any{"remaining": 42},
		"rate_limit":                map[string]any{"remaining": 3, "reset_at": resetAt.Unix()},
	})
	var gotReq GrokACPBillingRequest
	deps := grokQuotaDeps(t, exe, GrokACPBillingResult{Stdout: stdout, ExitCode: 0}, nil)
	deps.Getenv = func(key string) string {
		switch key {
		case "PATH":
			return filepath.Dir(exe)
		case "HOME":
			return "/home/grok-fixture"
		case "XAI_API_KEY", "LOOPCODER_TEST_CREDENTIAL":
			return "secret-should-not-cross"
		default:
			return ""
		}
	}
	deps.RunGrokACP = func(_ context.Context, req GrokACPBillingRequest) (GrokACPBillingResult, error) {
		gotReq = req
		for _, entry := range req.Env {
			if strings.Contains(entry, "secret-should-not-cross") || strings.Contains(strings.ToLower(entry), "key") || strings.Contains(strings.ToLower(entry), "token") {
				t.Fatalf("credential-like env reached grok ACP billing: %q", entry)
			}
		}
		return GrokACPBillingResult{Stdout: stdout, ExitCode: 0}, nil
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "grok"}},
		Now:    fixedInventoryNow,
		NetworkGrants: []NetworkGrant{{
			ProviderID: "grok",
			Purpose:    NetworkPurposeQuotaTelemetry,
			Scope:      NetworkScopeMachineInventory,
		}},
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{exe, "--no-auto-update", "agent", "stdio"}; !sameStrings(gotReq.Argv, want) {
		t.Fatalf("grok ACP argv = %#v, want %#v", gotReq.Argv, want)
	}
	sources := quotaSourcesFor(report, "grok")
	if len(sources) != 1 {
		t.Fatalf("grok quota sources = %d: %#v", len(sources), report.QuotaTelemetrySources)
	}
	for _, snapshot := range quotaSnapshotsFor(report, "grok") {
		if err := ValidateQuotaSnapshot(sources[0], snapshot); err != nil {
			t.Fatalf("ValidateQuotaSnapshot(%s): %v\n%#v", snapshot.ProviderQuantityName, err, snapshot)
		}
		if strings.Contains(snapshot.RedactedDiagnostics, "secret-should-not-cross") {
			t.Fatalf("diagnostics retained credential canary: %#v", snapshot)
		}
	}
	total := quotaSnapshotByKey(t, report, "total_tokens", WindowUnknown, "provider:grok/account:acct_grok/model:grok-4.5/detail:execution_usage")
	if total.UsedValue == nil || *total.UsedValue != 18 || total.Confidence != ConfidenceExact || total.Unit != "token" {
		t.Fatalf("total token snapshot = %#v", total)
	}
	cost := quotaSnapshotByKey(t, report, "cost_usd", WindowUnbounded, "provider:grok/account:acct_grok/model:grok-4.5/detail:execution_cost")
	if cost.UsedValue == nil || *cost.UsedValue != 123 || cost.ValueScale != 4 || cost.Unit != "usd" {
		t.Fatalf("cost snapshot = %#v", cost)
	}
	consumer := quotaSnapshotByKey(t, report, "consumer_weekly_allowance", WindowProviderDefined, "provider:grok/account:acct_grok/model:grok-4.5/detail:consumer_weekly_allowance")
	build := quotaSnapshotByKey(t, report, "build_session_allowance", WindowUnknown, "provider:grok/account:acct_grok/model:grok-4.5/detail:build_session_allowance")
	credits := quotaSnapshotByKey(t, report, "api_credits", WindowUnknown, "provider:grok/account:acct_grok/model:grok-4.5/detail:api_credits")
	rate := quotaSnapshotByKey(t, report, "rate_limit_remaining", WindowUnknown, "provider:grok/account:acct_grok/model:grok-4.5/detail:rate_limit_remaining")
	for name, snapshot := range map[string]QuotaSnapshot{"consumer": consumer, "build": build, "credits": credits, "rate": rate} {
		if snapshot.RemainingValue == nil || snapshot.ProviderQuantityName == "" {
			t.Fatalf("%s authority snapshot = %#v", name, snapshot)
		}
	}
	if *consumer.RemainingValue != 40 || *build.RemainingValue != 8 || *credits.RemainingValue != 42 || *rate.RemainingValue != 3 {
		t.Fatalf("authority snapshots = consumer:%#v build:%#v credits:%#v rate:%#v", consumer, build, credits, rate)
	}
	unknown := quotaSnapshotByKey(t, report, "provider_wide_allowance", WindowUnknown, "provider:grok/account:acct_grok/model:grok-4.5/detail:provider_wide_allowance")
	if unknown.LimitValue != nil || unknown.RemainingValue != nil || unknown.Confidence != ConfidenceUnknown || !containsString(unknown.GapReasons, "provider-wide-allowance-absent") {
		t.Fatalf("provider-wide allowance = %#v, want absent unknown not zero/unlimited", unknown)
	}
}

func TestCodexAndGrokQuotaObservationsRemainProviderScoped(t *testing.T) {
	codexExe := writeFakeCodex(t)
	grokExe := writeFakeGrok(t)
	codexReset := fixedInventoryNow().Add(5 * time.Hour).Unix()
	grokReset := fixedInventoryNow().Add(30 * time.Minute)
	codexStdout := codexQuotaFrames(t,
		map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"id": "acct_codex"}},
		map[string]any{
			"rateLimits": map[string]any{
				"limitId": "codex",
				"primary": map[string]any{"usedPercent": 35, "windowDurationMins": 300, "resetsAt": codexReset},
			},
		},
	)
	grokStdout := grokACPFrames(t, map[string]any{
		"account":     map[string]any{"id": "acct_grok"},
		"model":       "grok-4.5",
		"api_credits": map[string]any{"remaining": 42},
		"rate_limit":  map[string]any{"remaining": 9, "reset_at": grokReset.Unix()},
	})
	deps := fakeDeps(t, map[string]string{
		filepath.Clean(codexExe): "codex 0.9.0",
		filepath.Clean(grokExe):  "grok 0.2.100",
	})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return strings.Join([]string{filepath.Dir(codexExe), filepath.Dir(grokExe)}, string(os.PathListSeparator))
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		if filepath.Clean(req.Argv[0]) == filepath.Clean(grokExe) && len(req.Argv) == 4 && req.Argv[1] == "agent" && req.Argv[2] == "stdio" && req.Argv[3] == "--help" {
			return ProbeExecutionResult{Stdout: "grok agent stdio billing/read\n", ExitCode: 0}, nil
		}
		version := "codex 0.9.0"
		if filepath.Clean(req.Argv[0]) == filepath.Clean(grokExe) {
			version = "grok 0.2.100"
		}
		return ProbeExecutionResult{Stdout: version + "\n", ExitCode: 0}, nil
	}
	deps.RunCodexRPC = func(context.Context, CodexAppServerRequest) (CodexAppServerResult, error) {
		return CodexAppServerResult{Stdout: codexStdout, ExitCode: 0}, nil
	}
	deps.RunGrokACP = func(context.Context, GrokACPBillingRequest) (GrokACPBillingResult, error) {
		return GrokACPBillingResult{Stdout: grokStdout, ExitCode: 0}, nil
	}
	// Keep ACP fixture path: do not hit live official credits HTTP.
	deps.RunGrokCredits = func(context.Context, GrokCreditsBillingRequest) (GrokCreditsBillingResult, error) {
		return GrokCreditsBillingResult{}, ErrGrokCreditsAuthMissing
	}

	report, err := Discover(context.Background(), Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "codex", Verifier: "grok"}},
		Now:    fixedInventoryNow,
		NetworkGrants: []NetworkGrant{
			{ProviderID: "codex", Purpose: NetworkPurposeQuotaTelemetry, Scope: NetworkScopeMachineInventory},
			{ProviderID: "grok", Purpose: NetworkPurposeQuotaTelemetry, Scope: NetworkScopeMachineInventory},
		},
	}, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := len(quotaSourcesFor(report, "codex")); got != 1 {
		t.Fatalf("codex quota sources = %d: %#v", got, report.QuotaTelemetrySources)
	}
	if got := len(quotaSourcesFor(report, "grok")); got != 1 {
		t.Fatalf("grok quota sources = %d: %#v", got, report.QuotaTelemetrySources)
	}
	codexPrimary := quotaSnapshotByKey(t, report, "primary_used_percent", WindowRolling, "provider:codex/account:acct_codex/scope:codex/detail:primary")
	grokCredits := quotaSnapshotByKey(t, report, "api_credits", WindowUnknown, "provider:grok/account:acct_grok/model:grok-4.5/detail:api_credits")
	grokRate := quotaSnapshotByKey(t, report, "rate_limit_remaining", WindowUnknown, "provider:grok/account:acct_grok/model:grok-4.5/detail:rate_limit_remaining")
	if codexPrimary.AdapterID != "codex" || grokCredits.AdapterID != "grok" || grokRate.AdapterID != "grok" {
		t.Fatalf("provider ids crossed over: codex=%#v grokCredits=%#v grokRate=%#v", codexPrimary.AdapterID, grokCredits.AdapterID, grokRate.AdapterID)
	}
	if codexPrimary.ProviderInstallationID == nil || grokCredits.ProviderInstallationID == nil || *codexPrimary.ProviderInstallationID == *grokCredits.ProviderInstallationID {
		t.Fatalf("provider installation scope crossed over: codex=%v grok=%v", codexPrimary.ProviderInstallationID, grokCredits.ProviderInstallationID)
	}
	if strings.Contains(codexPrimary.ScopeKey, "grok") || strings.Contains(codexPrimary.ScopeKey, "acct_grok") || strings.Contains(grokCredits.ScopeKey, "codex") || strings.Contains(grokCredits.ScopeKey, "acct_codex") {
		t.Fatalf("account/model scope crossed over: codex=%q grok=%q", codexPrimary.ScopeKey, grokCredits.ScopeKey)
	}
	if codexPrimary.RemainingValue == nil || *codexPrimary.RemainingValue != 65 || grokCredits.RemainingValue == nil || *grokCredits.RemainingValue != 42 || grokRate.RemainingValue == nil || *grokRate.RemainingValue != 9 {
		t.Fatalf("authority values crossed or changed: codex=%#v grokCredits=%#v grokRate=%#v", codexPrimary, grokCredits, grokRate)
	}
}

func TestGrokACPBillingMethodNotFoundIsUnsupportedUnknown(t *testing.T) {
	exe := writeFakeGrok(t)
	stdout := grokACPFramesWithBillingError(t, -32601, "method not found")
	deps := grokQuotaDeps(t, exe, GrokACPBillingResult{Stdout: stdout, ExitCode: 0}, nil)
	report, err := Discover(context.Background(), grokQuotaOptions(), deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	got := onlyQuotaSnapshot(t, report, "grok")
	if got.TerminalErrorCode != "ErrQuotaSourceUnsupported" || got.Confidence != ConfidenceUnavailable || got.RemainingValue != nil || !containsString(got.GapReasons, "method-not-found") {
		t.Fatalf("method-not-found snapshot = %#v", got)
	}
}

func TestGrokACPBillingFailuresNormalizeTypedSnapshots(t *testing.T) {
	exe := writeFakeGrok(t)
	cases := []struct {
		name     string
		result   GrokACPBillingResult
		err      error
		wantCode string
		wantGap  string
	}{
		{name: "malformed", result: GrokACPBillingResult{Stdout: "{not-json}\n", ExitCode: 0}, wantCode: "ErrQuotaSnapshotMalformed", wantGap: "malformed-frame"},
		{name: "auth-expiry", result: GrokACPBillingResult{Stdout: grokACPFramesWithBillingError(t, -32001, "auth expired"), ExitCode: 0}, wantCode: "ErrAuthExpired", wantGap: "auth-expired"},
		{name: "model-unavailable", result: GrokACPBillingResult{Stdout: grokACPFramesWithBillingError(t, -32002, "model unavailable"), ExitCode: 0}, wantCode: "ErrModelUnavailable", wantGap: "model-unavailable"},
		{name: "outage", result: GrokACPBillingResult{Stdout: grokACPFramesWithBillingError(t, -32003, "provider outage 5xx"), ExitCode: 0}, wantCode: "ErrProviderOutage", wantGap: "provider-outage"},
		{name: "transport", result: GrokACPBillingResult{ExitCode: -1}, err: errors.New("eof"), wantCode: "ErrGrokACPBillingExecutionFailed", wantGap: "quota-probe-failed"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			deps := grokQuotaDeps(t, exe, tt.result, tt.err)
			report, err := Discover(context.Background(), grokQuotaOptions(), deps)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			got := onlyQuotaSnapshot(t, report, "grok")
			if got.TerminalErrorCode != tt.wantCode || got.Confidence != ConfidenceUnavailable || !containsString(got.GapReasons, tt.wantGap) {
				t.Fatalf("snapshot = %#v, want %s/%s unavailable", got, tt.wantCode, tt.wantGap)
			}
		})
	}
}

func TestGrokACPBillingExhaustionAndStaleFixtures(t *testing.T) {
	exe := writeFakeGrok(t)
	stdout := grokACPFrames(t, map[string]any{
		"account":     map[string]any{"id": "acct_grok"},
		"api_credits": map[string]any{"remaining": 0},
		"rate_limit":  map[string]any{"remaining": 0, "reset_at": fixedInventoryNow().Add(time.Hour).Unix()},
	})
	deps := grokQuotaDeps(t, exe, GrokACPBillingResult{Stdout: stdout, ExitCode: 0}, nil)
	report, err := Discover(context.Background(), grokQuotaOptions(), deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	credits := quotaSnapshotByKey(t, report, "api_credits", WindowUnknown, "provider:grok/account:acct_grok/detail:api_credits")
	if credits.TerminalErrorCode != "ErrQuotaExhausted" || !containsString(credits.GapReasons, "quota-exhausted") {
		t.Fatalf("credit exhaustion snapshot = %#v", credits)
	}
	rate := quotaSnapshotByKey(t, report, "rate_limit_remaining", WindowUnknown, "provider:grok/account:acct_grok/detail:rate_limit_remaining")
	if rate.TerminalErrorCode != "ErrRateLimited" || !containsString(rate.GapReasons, "rate-limited-429") {
		t.Fatalf("rate limit snapshot = %#v", rate)
	}

	stale := markQuotaFreshness([]QuotaSnapshot{rate}, fixedInventoryNow().Add(2*time.Hour))[0]
	if stale.Confidence != ConfidenceStale || stale.FreshnessState != FreshnessStale || !containsString(stale.GapReasons, "stale-cache") {
		t.Fatalf("stale rate snapshot = %#v", stale)
	}
}

func TestGrokACPBillingCapabilityRequiredBeforeMethodProbe(t *testing.T) {
	exe := writeFakeGrok(t)
	deps := grokQuotaDeps(t, exe, GrokACPBillingResult{}, nil)
	// Isolate from host ~/.grok/auth.json so credits path reports auth-missing.
	emptyHome := t.TempDir()
	deps.UserHomeDir = func() (string, error) { return emptyHome, nil }
	prevEnv := deps.Getenv
	deps.Getenv = func(key string) string {
		if key == "HOME" || key == "GROK_HOME" {
			return emptyHome
		}
		if prevEnv != nil {
			return prevEnv(key)
		}
		return ""
	}
	called := false
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		switch {
		case len(req.Argv) == 2 && req.Argv[1] == "version":
			return ProbeExecutionResult{Stdout: "grok 0.2.100\n", ExitCode: 0}, nil
		case len(req.Argv) == 4 && req.Argv[1] == "agent" && req.Argv[2] == "stdio" && req.Argv[3] == "--help":
			return ProbeExecutionResult{Stdout: "grok agent stdio\n", ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected probe argv: %#v", req.Argv)
			return ProbeExecutionResult{ExitCode: 2}, nil
		}
	}
	deps.RunGrokACP = func(context.Context, GrokACPBillingRequest) (GrokACPBillingResult, error) {
		called = true
		return GrokACPBillingResult{}, nil
	}
	report, err := Discover(context.Background(), grokQuotaOptions(), deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if called {
		t.Fatal("Grok ACP billing method was called without advertised billing capability")
	}
	got := onlyQuotaSnapshot(t, report, "grok")
	// Primary path is official credits billing; without auth and without ACP
	// capability the typed gap is credits-auth-missing (not a silent invent).
	if got.TerminalErrorCode != "ErrGrokCreditsAuthMissing" || !containsString(got.GapReasons, "credits-auth-missing") {
		t.Fatalf("capability snapshot = %#v", got)
	}
}

func TestGrokACPInitializeBillingCapabilityIsNotInferredFromUnrelatedBooleans(t *testing.T) {
	raw := mustRawJSON(t, map[string]any{
		"authMethods": []any{"cached_token"},
		"capabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": true},
			"terminal": true,
		},
	})
	if grokACPInitializeAdvertisesBilling(raw) {
		t.Fatalf("billing capability inferred from unrelated initialize payload: %s", raw)
	}
	raw = mustRawJSON(t, map[string]any{"capabilities": map[string]any{"billing": true}})
	if !grokACPInitializeAdvertisesBilling(raw) {
		t.Fatalf("billing capability not detected from explicit payload: %s", raw)
	}
}

func TestGrokACPJSONRPCRequestsIncludeProtocolVersion(t *testing.T) {
	var out strings.Builder
	if err := writeGrokACPJSONL(&out, grokACPJSONRPCRequest{JSONRPC: "2.0", ID: 7, Method: "billing/read", Params: map[string]any{}}); err != nil {
		t.Fatalf("writeGrokACPJSONL: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &fields); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if fields["jsonrpc"] != "2.0" || fields["method"] != "billing/read" || fields["id"] != float64(7) {
		t.Fatalf("request envelope = %#v", fields)
	}
}

func grokQuotaDeps(t *testing.T, exe string, result GrokACPBillingResult, runErr error) Deps {
	t.Helper()
	deps := fakeDeps(t, map[string]string{filepath.Clean(exe): "grok 0.2.100"})
	deps.Getenv = func(key string) string {
		if key == "PATH" {
			return filepath.Dir(exe)
		}
		return ""
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		switch {
		case len(req.Argv) == 2 && req.Argv[1] == "version":
			return ProbeExecutionResult{Stdout: "grok 0.2.100\n", ExitCode: 0}, nil
		case len(req.Argv) == 4 && req.Argv[1] == "agent" && req.Argv[2] == "stdio" && req.Argv[3] == "--help":
			return ProbeExecutionResult{Stdout: "grok agent stdio billing/read\n", ExitCode: 0}, nil
		default:
			t.Fatalf("unexpected grok probe argv: %#v", req.Argv)
			return ProbeExecutionResult{ExitCode: 2}, nil
		}
	}
	deps.RunGrokACP = func(context.Context, GrokACPBillingRequest) (GrokACPBillingResult, error) {
		return result, runErr
	}
	// Force ACP-path tests to skip official credits HTTP (auth absent in fixture home).
	deps.RunGrokCredits = func(context.Context, GrokCreditsBillingRequest) (GrokCreditsBillingResult, error) {
		return GrokCreditsBillingResult{}, ErrGrokCreditsAuthMissing
	}
	return deps
}

func grokQuotaOptions() Options {
	return Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "grok"}},
		Now:    fixedInventoryNow,
		NetworkGrants: []NetworkGrant{{
			ProviderID: "grok",
			Purpose:    NetworkPurposeQuotaTelemetry,
			Scope:      NetworkScopeMachineInventory,
		}},
	}
}

func writeFakeGrok(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, executableName("grok"))
	writeExecutable(t, path)
	return path
}

func grokACPFrames(t *testing.T, billing map[string]any) string {
	t.Helper()
	return strings.Join([]string{
		grokACPJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{"capabilities": map[string]any{"billing": true}})}),
		grokACPJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, billing)}),
	}, "\n") + "\n"
}

func grokACPFramesWithBillingError(t *testing.T, code int, message string) string {
	t.Helper()
	return strings.Join([]string{
		grokACPJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{"capabilities": map[string]any{"billing": true}})}),
		grokACPJSONL(t, jsonRPCMessage{ID: 2, Error: &jsonRPCError{Code: code, Message: message, codePresent: true, messagePresent: true}}),
	}, "\n") + "\n"
}

func grokACPJSONL(t *testing.T, message jsonRPCMessage) string {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("Marshal JSONL: %v", err)
	}
	return string(data)
}

func TestGrokCreditsBillingParsesWeeklyUsageAndProduct(t *testing.T) {
	exe := writeFakeGrok(t)
	body := []byte(`{
  "config": {
    "currentPeriod": {
      "type": "USAGE_PERIOD_TYPE_WEEKLY",
      "start": "2026-07-21T23:18:03.510207+00:00",
      "end": "2026-07-28T23:18:03.510207+00:00"
    },
    "creditUsagePercent": 31,
    "productUsage": [{"product": "GrokBuild", "usagePercent": 31}],
    "isUnifiedBillingUser": true
  }
}`)
	var sawToken bool
	deps := grokQuotaDeps(t, exe, GrokACPBillingResult{}, nil)
	deps.RunGrokCredits = func(_ context.Context, req GrokCreditsBillingRequest) (GrokCreditsBillingResult, error) {
		if req.Token == "" {
			t.Fatal("token required for credits billing")
		}
		if strings.Contains(req.Token, "secret") {
			// ok; token itself is the secret — must not appear in diagnostics later
		}
		sawToken = true
		if req.URL != "" && !strings.Contains(req.URL, "billing") {
			t.Fatalf("unexpected URL %q", req.URL)
		}
		return GrokCreditsBillingResult{StatusCode: 200, Body: body}, nil
	}
	// Provide fake auth via HOME auth.json (account-keyed nested key).
	home := t.TempDir()
	authDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := map[string]any{
		"https://auth.x.ai::acct-fixture": map[string]any{
			"key":        "secret-test-token-not-for-logs",
			"auth_mode":  "oidc",
			"expires_at": fixedInventoryNow().Add(time.Hour).Format(time.RFC3339Nano),
		},
	}
	raw, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	prevHome := deps.Getenv
	deps.Getenv = func(key string) string {
		if key == "HOME" {
			return home
		}
		if key == "PATH" {
			return filepath.Dir(exe)
		}
		if prevHome != nil {
			return prevHome(key)
		}
		return ""
	}
	deps.UserHomeDir = func() (string, error) { return home, nil }

	report, err := Discover(context.Background(), grokQuotaOptions(), deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !sawToken {
		t.Fatal("RunGrokCredits was not invoked")
	}
	sources := quotaSourcesFor(report, "grok")
	if len(sources) != 1 || sources[0].SourceSchemaVersion != grokCreditsBillingSourceSchema {
		t.Fatalf("sources = %#v", sources)
	}
	primary := quotaSnapshotByKey(t, report, "credit_usage_percent", WindowFixedWeek, "provider:grok/account:acct-fixture/detail:credits_usage")
	if primary.UsedValue == nil || *primary.UsedValue != 31 || primary.RemainingValue == nil || *primary.RemainingValue != 69 {
		t.Fatalf("primary = %#v", primary)
	}
	if primary.LimitValue == nil || *primary.LimitValue != 100 || primary.Unit != "percent" || primary.Confidence != ConfidenceExact {
		t.Fatalf("primary flags = %#v", primary)
	}
	if primary.ResetAt == "" || primary.FieldConfidences["reset_at"] != ConfidenceExact {
		t.Fatalf("reset = %#v", primary)
	}
	if strings.Contains(primary.RedactedDiagnostics, "secret-test-token") {
		t.Fatalf("diagnostics retained token: %#v", primary.RedactedDiagnostics)
	}
	product := quotaSnapshotByKey(t, report, "product_usage_percent", WindowFixedWeek, "provider:grok/account:acct-fixture/detail:product_grokbuild")
	if product.RemainingValue == nil || *product.RemainingValue != 69 {
		t.Fatalf("product = %#v", product)
	}
	// ACP must not be required when credits succeeds.
}

func TestGrokCreditsBillingAuthShapesAndRedaction(t *testing.T) {
	// Account-keyed nested key (current).
	home := t.TempDir()
	authDir := filepath.Join(home, ".grok")
	_ = os.MkdirAll(authDir, 0o700)
	_ = os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(`{"https://auth.x.ai::u1":{"key":"tok-a","refresh_token":"ref-should-not-be-used"}}`), 0o600)
	tok, ref, err := loadGrokCLIAuthToken(home, nil)
	if err != nil || tok != "tok-a" || ref == "" {
		t.Fatalf("account-keyed: tok=%q ref=%q err=%v", tok, ref, err)
	}
	// Root access_token compatible shape.
	home2 := t.TempDir()
	_ = os.MkdirAll(filepath.Join(home2, ".grok"), 0o700)
	_ = os.WriteFile(filepath.Join(home2, ".grok", "auth.json"), []byte(`{"access_token":"tok-root"}`), 0o600)
	tok, ref, err = loadGrokCLIAuthToken(home2, nil)
	if err != nil || tok != "tok-root" || ref != "root" {
		t.Fatalf("root access_token: tok=%q ref=%q err=%v", tok, ref, err)
	}
	// Missing auth.
	home3 := t.TempDir()
	if _, _, err := loadGrokCLIAuthToken(home3, nil); err == nil {
		t.Fatal("expected missing auth error")
	}
	// snapshotsFromGrokCreditsBilling refuses credential-like body noise that looks like secrets.
	body := []byte(`{"config":{"creditUsagePercent":10,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2026-07-28T00:00:00Z"}}}`)
	snaps, err := snapshotsFromGrokCreditsBilling(grokCreditsBillingSource(fixedInventoryNow()), nil, "0.2.111", "acct", body, fixedInventoryNow())
	if err != nil || len(snaps) < 1 {
		t.Fatalf("snaps err=%v n=%d", err, len(snaps))
	}
	for _, s := range snaps {
		if strings.Contains(strings.ToLower(s.RedactedDiagnostics), "bearer ") || strings.Contains(s.RedactedDiagnostics, "tok-") {
			t.Fatalf("diagnostics leaked credential material: %q", s.RedactedDiagnostics)
		}
	}
}

func TestGrokCreditsBillingRequiresNetworkGrant(t *testing.T) {
	exe := writeFakeGrok(t)
	deps := grokQuotaDeps(t, exe, GrokACPBillingResult{}, nil)
	called := false
	deps.RunGrokCredits = func(context.Context, GrokCreditsBillingRequest) (GrokCreditsBillingResult, error) {
		called = true
		return GrokCreditsBillingResult{StatusCode: 200, Body: []byte(`{"config":{"creditUsagePercent":1}}`)}, nil
	}
	opts := grokQuotaOptions()
	opts.NetworkGrants = nil // deny
	report, err := Discover(context.Background(), opts, deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if called {
		t.Fatal("credits HTTP must not run without network grant")
	}
	got := onlyQuotaSnapshot(t, report, "grok")
	if !containsString(got.GapReasons, "quota-collection-not-granted") {
		t.Fatalf("grant snapshot = %#v", got)
	}
}
