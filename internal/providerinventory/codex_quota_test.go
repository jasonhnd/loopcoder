package providerinventory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/storage"
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
	codexSources := quotaSourcesFor(report, "codex")
	if len(codexSources) != 1 {
		t.Fatalf("codex quota sources = %d: %#v", len(codexSources), report.QuotaTelemetrySources)
	}
	for _, snapshot := range codexSnapshots {
		if err := ValidateQuotaSnapshot(codexSources[0], snapshot); err != nil {
			t.Fatalf("ValidateQuotaSnapshot(%s): %v\n%#v", snapshot.ProviderQuantityName, err, snapshot)
		}
	}
	byName := map[string]QuotaSnapshot{}
	for _, snapshot := range codexSnapshots {
		byName[snapshot.ProviderQuantityName+"|"+string(snapshot.WindowKind)+"|"+snapshot.ScopeKey] = snapshot
		if strings.Contains(snapshot.RedactedDiagnostics, "secret-should-not-cross") {
			t.Fatalf("diagnostics retained credential canary: %#v", snapshot)
		}
	}
	defaultPrimaryScope := "provider:codex/scope:codex/detail:primary"
	five := byName["primary_used_percent|rolling|"+defaultPrimaryScope]
	if five.RollingDurationMS != int64((5*time.Hour).Milliseconds()) || five.UsedValue == nil || *five.UsedValue != 25 || five.RemainingValue == nil || *five.RemainingValue != 75 || five.Unit != "percent" {
		t.Fatalf("five-hour snapshot = %#v", five)
	}
	if five.LimitValue != nil || five.FieldConfidences["remaining_value"] != ConfidenceEstimated || !containsString(five.GapReasons, "remaining-derived-from-used-percent") {
		t.Fatalf("five-hour percentage semantics = %#v, want no fabricated limit and derived remaining", five)
	}
	weekly := byName["secondary_used_percent|fixed-week|provider:codex/scope:codex/detail:secondary"]
	if weekly.ResetAt != formatTime(time.Unix(resetWeek, 0)) || weekly.RollingDurationMS != int64((7*24*time.Hour).Milliseconds()) {
		t.Fatalf("weekly snapshot = %#v", weekly)
	}
	if weekly.WindowStart != formatTime(time.Unix(resetWeek, 0).Add(-7*24*time.Hour)) || weekly.WindowEnd != formatTime(time.Unix(resetWeek, 0)) || weekly.ResetSemantics != ResetWindowBoundary {
		t.Fatalf("weekly fixed boundary fields = %#v", weekly)
	}
	credits := byName["credits_balance|unbounded|provider:codex/scope:codex/detail:credits"]
	if credits.QuantityKind != QuantityProviderDefined || credits.RemainingValue == nil || *credits.RemainingValue != 4275 || credits.ValueScale != 2 {
		t.Fatalf("credits snapshot = %#v", credits)
	}
	workspace := byName["primary_used_percent|rolling|provider:codex/scope:workspace/detail:primary"]
	if workspace.RemainingValue == nil || *workspace.RemainingValue != 90 {
		t.Fatalf("workspace snapshot = %#v", workspace)
	}
	resetCredits := byName["rate_limit_reset_credits|unbounded|provider:codex/scope:default/detail:reset_credits"]
	if resetCredits.RemainingValue == nil || *resetCredits.RemainingValue != 2 || resetCredits.ValidUntil != formatTime(time.Unix(resetCreditExpiry, 0)) {
		t.Fatalf("reset credits snapshot = %#v", resetCredits)
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
	loadedFive := quotaSnapshotByKey(t, loaded, "primary_used_percent", WindowRolling, defaultPrimaryScope)
	if loadedFive.RollingDurationMS != int64((5*time.Hour).Milliseconds()) || loadedFive.ResetAt != formatTime(time.Unix(resetFiveHour, 0)) || loadedFive.ResetSemantics != ResetRolling || loadedFive.WindowStart != "" || loadedFive.WindowEnd != "" {
		t.Fatalf("loaded five-hour fields = %#v", loadedFive)
	}
	loadedWeekly := quotaSnapshotByKey(t, loaded, "secondary_used_percent", WindowFixedWeek, "provider:codex/scope:codex/detail:secondary")
	if loadedWeekly.WindowStart != formatTime(time.Unix(resetWeek, 0).Add(-7*24*time.Hour)) || loadedWeekly.WindowEnd != formatTime(time.Unix(resetWeek, 0)) || loadedWeekly.ResetAt != formatTime(time.Unix(resetWeek, 0)) || loadedWeekly.ResetSemantics != ResetWindowBoundary || loadedWeekly.RollingDurationMS != int64((7*24*time.Hour).Milliseconds()) {
		t.Fatalf("loaded weekly fields = %#v", loadedWeekly)
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

func TestCodexQuotaInvalidWindowInputsPersistAsNonFixed(t *testing.T) {
	exe := writeFakeCodex(t)
	now := fixedInventoryNow()
	weekReset := now.Add(7 * 24 * time.Hour).Unix()
	overflowMinutes := int64(1<<63-1)/int64(time.Minute) + 1
	stdout := codexQuotaFramesReordered(t,
		map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "planType": "pro", "id": "acct_mixed"}},
		map[string]any{
			"rateLimits": map[string]any{
				"limitId":   "codex",
				"primary":   map[string]any{"usedPercent": 30, "windowDurationMins": 10080, "resetsAt": weekReset},
				"secondary": map[string]any{"usedPercent": 40, "windowDurationMins": 0, "resetsAt": weekReset},
			},
			"rateLimitsByLimitId": map[string]any{
				"negative": map[string]any{
					"limitId": "negative",
					"primary": map[string]any{"usedPercent": 50, "windowDurationMins": -5, "resetsAt": weekReset},
				},
				"noreset": map[string]any{
					"limitId": "noreset",
					"primary": map[string]any{"usedPercent": 60, "windowDurationMins": 10080},
				},
				"overflow": map[string]any{
					"limitId": "overflow",
					"primary": map[string]any{"usedPercent": 70, "windowDurationMins": overflowMinutes, "resetsAt": weekReset},
				},
				"outofrange": map[string]any{
					"limitId": "outofrange",
					"primary": map[string]any{"usedPercent": 80, "windowDurationMins": 10080, "resetsAt": int64(1 << 62)},
				},
			},
		},
	)
	deps := codexQuotaDeps(t, exe, "codex 0.9.0", CodexAppServerResult{Stdout: stdout, ExitCode: 0}, nil)
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
	codexSources := quotaSourcesFor(report, "codex")
	if len(codexSources) != 1 {
		t.Fatalf("codex quota sources = %d: %#v", len(codexSources), report.QuotaTelemetrySources)
	}
	for _, snapshot := range quotaSnapshotsFor(report, "codex") {
		if err := ValidateQuotaSnapshot(codexSources[0], snapshot); err != nil {
			t.Fatalf("ValidateQuotaSnapshot(%s %s): %v\n%#v", snapshot.ScopeKey, snapshot.WindowKind, err, snapshot)
		}
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

	validWeekly := quotaSnapshotByKey(t, loaded, "primary_used_percent", WindowFixedWeek, "provider:codex/scope:codex/detail:primary")
	if validWeekly.WindowStart != formatTime(time.Unix(weekReset, 0).Add(-7*24*time.Hour)) || validWeekly.WindowEnd != formatTime(time.Unix(weekReset, 0)) {
		t.Fatalf("valid weekly fields = %#v", validWeekly)
	}
	noReset := quotaSnapshotByKey(t, loaded, "primary_used_percent", WindowProviderDefined, "provider:codex/scope:noreset/detail:primary")
	if noReset.WindowStart != "" || noReset.WindowEnd != "" || noReset.ResetAt != "" || noReset.RollingDurationMS != int64((7*24*time.Hour).Milliseconds()) || !containsString(noReset.GapReasons, "missing-reset-at") {
		t.Fatalf("no-reset weekly fields = %#v", noReset)
	}
	zero := quotaSnapshotByKey(t, loaded, "secondary_used_percent", WindowUnknown, "provider:codex/scope:codex/detail:secondary")
	if zero.RollingDurationMS != 0 || zero.WindowStart != "" || zero.WindowEnd != "" || !containsString(zero.GapReasons, "invalid-window-duration") {
		t.Fatalf("zero-duration fields = %#v", zero)
	}
	negative := quotaSnapshotByKey(t, loaded, "primary_used_percent", WindowUnknown, "provider:codex/scope:negative/detail:primary")
	if negative.RollingDurationMS != 0 || !containsString(negative.GapReasons, "invalid-window-duration") {
		t.Fatalf("negative-duration fields = %#v", negative)
	}
	overflow := quotaSnapshotByKey(t, loaded, "primary_used_percent", WindowUnknown, "provider:codex/scope:overflow/detail:primary")
	if overflow.RollingDurationMS != 0 || !containsString(overflow.GapReasons, "invalid-window-duration") {
		t.Fatalf("overflow-duration fields = %#v", overflow)
	}
	outOfRange := quotaSnapshotByKey(t, loaded, "primary_used_percent", WindowProviderDefined, "provider:codex/scope:outofrange/detail:primary")
	if outOfRange.WindowStart != "" || outOfRange.WindowEnd != "" || outOfRange.ResetAt != "" || !containsString(outOfRange.GapReasons, "invalid-reset-at") {
		t.Fatalf("out-of-range reset fields = %#v", outOfRange)
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
		{name: "error-missing-code", version: "codex 0.9.0", result: CodexAppServerResult{Stdout: `{"id":1,"error":{"message":"bad"}}`, ExitCode: 0}, wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{name: "fractional-response-id", version: "codex 0.9.0", result: CodexAppServerResult{Stdout: `{"id":1.5,"result":{}}`, ExitCode: 0}, wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{name: "exponent-response-id", version: "codex 0.9.0", result: CodexAppServerResult{Stdout: `{"id":1e0,"result":{}}`, ExitCode: 0}, wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
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
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	deps := codexQuotaDeps(t, first, "codex 0.9.0", CodexAppServerResult{}, nil)
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
		return runCodexAppServer(context.Background(), CodexAppServerRequest{
			Argv:               []string{exe},
			Env:                []string{"LOOPCODER_FAKE_APP_SERVER=success"},
			Timeout:            2 * time.Second,
			StdoutLimitBytes:   codexQuotaOutputBytes,
			StderrLimitBytes:   StdoutLimitBytes,
			CombinedLimitBytes: codexQuotaOutputBytes + StdoutLimitBytes,
		})
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
		assertNoJSONRPCMember(t, line)
		msg, err := decodeCodexJSONLMessage(line)
		if err != nil {
			t.Fatalf("decode trace: %v", err)
		}
		got = append(got, msg)
	}
	if got[0].Method != "initialize" || got[1].Method != "initialized" || got[2].Method != "account/read" || got[3].Method != "account/rateLimits/read" {
		t.Fatalf("methods = %#v, want initialize/initialized/account/read/account/rateLimits/read", []string{got[0].Method, got[1].Method, got[2].Method, got[3].Method})
	}
	initializedCount := 0
	for _, msg := range got {
		if msg.Method == "initialized" {
			initializedCount++
		}
	}
	if initializedCount != 1 {
		t.Fatalf("initialized notifications = %d, want exactly one", initializedCount)
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
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		assertNoJSONRPCMember(t, line)
	}
}

func TestRunCodexAppServerAcceptsOfficialEnvelopeReorderingAndNotifications(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	trace := filepath.Join(t.TempDir(), "trace.jsonl")
	result, err := runCodexAppServer(context.Background(), CodexAppServerRequest{
		Argv:               []string{exe},
		Env:                []string{"LOOPCODER_FAKE_APP_SERVER=reorder", "LOOPCODER_FAKE_APP_SERVER_TRACE=" + trace},
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
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 4 {
		t.Fatalf("stdout lines = %d: %s", len(lines), result.Stdout)
	}
	var ids []string
	for _, line := range lines {
		assertNoJSONRPCMember(t, line)
		msg, err := decodeCodexJSONLMessage(line)
		if err != nil {
			t.Fatalf("decode stdout: %v", err)
		}
		if msg.Method != "" {
			continue
		}
		ids = append(ids, jsonRPCID(msg.ID))
	}
	if !sameStrings(ids, []string{"1", "3", "2"}) {
		t.Fatalf("response ids = %#v, want initialize/rate/account reordering", ids)
	}
	traceData, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("ReadFile trace: %v", err)
	}
	traceLines := strings.Split(strings.TrimSpace(string(traceData)), "\n")
	if len(traceLines) != 4 {
		t.Fatalf("trace lines = %d: %s", len(traceLines), traceData)
	}
	for i, line := range traceLines {
		assertNoJSONRPCMember(t, line)
		msg, err := decodeCodexJSONLMessage(line)
		if err != nil {
			t.Fatalf("decode trace: %v", err)
		}
		if i == 0 && msg.Method != "initialize" {
			t.Fatalf("first request = %s, want initialize", msg.Method)
		}
		if i == 1 && msg.Method != "initialized" {
			t.Fatalf("second request = %s, want initialized notification", msg.Method)
		}
		if i == 3 && msg.Method != "account/rateLimits/read" {
			t.Fatalf("fourth request = %s, want rate-limit request after handshake", msg.Method)
		}
	}
}

func TestRunCodexAppServerProtocolFailuresUseTypedErrors(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	cases := []struct {
		mode    string
		wantErr error
	}{
		{mode: "eof", wantErr: ErrCodexQuotaMalformed},
		{mode: "rpc-error", wantErr: ErrCodexQuotaRPC},
		{mode: "malformed-line", wantErr: ErrCodexQuotaMalformed},
		{mode: "contradictory-envelope", wantErr: ErrCodexQuotaMalformed},
		{mode: "trailing-json", wantErr: ErrCodexQuotaMalformed},
		{mode: "malformed-rpc-error", wantErr: ErrCodexQuotaMalformed},
		{mode: "rpc-error-missing-code", wantErr: ErrCodexQuotaMalformed},
		{mode: "fractional-response-id", wantErr: ErrCodexQuotaMalformed},
		{mode: "exponent-response-id", wantErr: ErrCodexQuotaMalformed},
		{mode: "oversized-line", wantErr: ErrCodexQuotaMalformed},
		{mode: "unsupported-init", wantErr: ErrCodexQuotaUnsupported},
	}
	for _, tt := range cases {
		t.Run(tt.mode, func(t *testing.T) {
			result, err := runCodexAppServer(context.Background(), CodexAppServerRequest{
				Argv:               []string{exe},
				Env:                []string{"LOOPCODER_FAKE_APP_SERVER=" + tt.mode},
				Timeout:            2 * time.Second,
				StdoutLimitBytes:   codexQuotaOutputBytes,
				StderrLimitBytes:   StdoutLimitBytes,
				CombinedLimitBytes: codexQuotaOutputBytes + StdoutLimitBytes,
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("runCodexAppServer err = %v, want %v; result=%#v", err, tt.wantErr, result)
			}
		})
	}
}

func TestDriveCodexAppServerProtocolPreservesRPCErrorBeforeEOF(t *testing.T) {
	events := make(chan codexProtocolStdoutEvent, 4)
	for _, line := range strings.Split(strings.TrimSpace(codexQuotaRPCErrorFrames(t)), "\n") {
		events <- codexProtocolStdoutEvent{line: line}
	}
	events <- codexProtocolStdoutEvent{err: io.EOF}

	err := driveCodexAppServerProtocolEvents(context.Background(), io.Discard, events)
	if !errors.Is(err, ErrCodexQuotaRPC) {
		t.Fatalf("driveCodexAppServerProtocolEvents err = %v, want rpc error", err)
	}
}

func TestInspectCodexQuotaPreservesRealRunnerProtocolFailures(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	fakeCodex := writeFakeCodex(t)
	cases := []struct {
		mode     string
		wantCode string
		wantGap  string
	}{
		{mode: "eof", wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{mode: "rpc-error", wantCode: "ErrCodexQuotaRPCError", wantGap: "rpc-error"},
		{mode: "malformed-line", wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{mode: "contradictory-envelope", wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{mode: "trailing-json", wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{mode: "malformed-rpc-error", wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{mode: "rpc-error-missing-code", wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{mode: "fractional-response-id", wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{mode: "exponent-response-id", wantCode: "ErrCodexQuotaMalformedFrame", wantGap: "malformed-frame"},
		{mode: "unsupported-init", wantCode: "ErrUnsupportedVersion", wantGap: "unsupported-cli-version"},
	}
	for _, tt := range cases {
		t.Run(tt.mode, func(t *testing.T) {
			deps := codexQuotaDeps(t, fakeCodex, "codex 0.9.0", CodexAppServerResult{}, nil)
			deps.RunCodexRPC = func(ctx context.Context, _ CodexAppServerRequest) (CodexAppServerResult, error) {
				return runCodexAppServer(ctx, CodexAppServerRequest{
					Argv:               []string{exe},
					Env:                []string{"LOOPCODER_FAKE_APP_SERVER=" + tt.mode},
					Timeout:            2 * time.Second,
					StdoutLimitBytes:   codexQuotaOutputBytes,
					StderrLimitBytes:   StdoutLimitBytes,
					CombinedLimitBytes: codexQuotaOutputBytes + StdoutLimitBytes,
				})
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
			got := onlyQuotaSnapshot(t, report, "codex")
			if got.TerminalErrorCode != tt.wantCode || !containsString(got.GapReasons, tt.wantGap) {
				t.Fatalf("snapshot = %#v, want %s/%s", got, tt.wantCode, tt.wantGap)
			}
		})
	}
}

func TestCodexAppServerOfficialEnvelopeGoldens(t *testing.T) {
	valid := []struct {
		name string
		line string
	}{
		{name: "request", line: codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Method: "initialize", Params: map[string]any{"clientInfo": map[string]any{"name": "loopcoder", "version": PolicyVersion}}})},
		{name: "request-string-id", line: `{"id":"initialize-1","method":"initialize","params":{}}`},
		{name: "notification", line: codexQuotaJSONL(t, jsonRPCMessage{Method: "initialized"})},
		{name: "success-response", line: codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, map[string]any{"requiresOpenaiAuth": false})})},
		{name: "success-response-string-id", line: `{"id":"account-2","result":{"requiresOpenaiAuth":false}}`},
		{name: "error-response", line: codexQuotaJSONL(t, jsonRPCMessage{ID: 3, Error: &jsonRPCError{Code: -32000, Message: "rate limited"}})},
		{name: "error-response-zero-code", line: `{"id":3,"error":{"code":0,"message":"no error code reserved by schema"}}`},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			assertNoJSONRPCMember(t, tt.line)
			if _, err := decodeCodexJSONLMessage(tt.line); err != nil {
				t.Fatalf("decode %s %s: %v", tt.name, tt.line, err)
			}
		})
	}

	malformed := []struct {
		name string
		line string
	}{
		{name: "missing-response-id", line: `{"result":{}}`},
		{name: "missing-response-payload", line: `{"id":1}`},
		{name: "request-carries-result", line: `{"id":1,"method":"initialize","result":{}}`},
		{name: "request-carries-error", line: `{"id":1,"method":"initialize","error":{"code":-32000,"message":"bad"}}`},
		{name: "notification-carries-result", line: `{"method":"initialized","result":{}}`},
		{name: "notification-carries-error", line: `{"method":"initialized","error":{"code":-32000,"message":"bad"}}`},
		{name: "success-carries-method", line: `{"id":2,"method":"account/read","result":{}}`},
		{name: "response-carries-result-and-error", line: `{"id":3,"result":{},"error":{"code":-32000,"message":"bad"}}`},
		{name: "error-missing-code", line: `{"id":3,"error":{"message":"bad"}}`},
		{name: "error-missing-message", line: `{"id":3,"error":{"code":-32000}}`},
		{name: "error-empty-message", line: `{"id":3,"error":{"code":-32000,"message":"  "}}`},
		{name: "request-fractional-id", line: `{"id":1.5,"method":"initialize"}`},
		{name: "request-exponent-id", line: `{"id":1e0,"method":"initialize"}`},
		{name: "response-fractional-id", line: `{"id":2.5,"result":{}}`},
		{name: "response-exponent-id", line: `{"id":2e0,"result":{}}`},
		{name: "response-empty-string-id", line: `{"id":"","result":{}}`},
		{name: "response-whitespace-string-id", line: `{"id":"  ","result":{}}`},
		{name: "trailing-json", line: `{"method":"initialized"} {"method":"second"}`},
	}
	for _, tt := range malformed {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeCodexJSONLMessage(tt.line); !errors.Is(err, ErrCodexQuotaMalformed) {
				t.Fatalf("decode %s err = %v, want malformed", tt.line, err)
			}
		})
	}
}

func TestWriteJSONLPropagatesMarshalErrors(t *testing.T) {
	if err := writeJSONL(io.Discard, jsonRPCMessage{Method: "bad", Params: func() {}}); err == nil {
		t.Fatal("writeJSONL err = nil, want marshal error")
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
		codexQuotaJSONL(t, jsonRPCMessage{Method: "codex/notice", Params: map[string]any{"ignored": true}}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test"})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, account)}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 3, Result: mustRawJSON(t, limits)}),
	}, "\n") + "\n"
}

func codexQuotaFramesReordered(t *testing.T, account, limits map[string]any) string {
	t.Helper()
	return strings.Join([]string{
		codexQuotaJSONL(t, jsonRPCMessage{Method: "codex/notice", Params: map[string]any{"ignored": true}}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test"})}),
		codexQuotaJSONL(t, jsonRPCMessage{Method: "account/rate_limits/updated", Params: map[string]any{"ignored": true}}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 3, Result: mustRawJSON(t, limits)}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, account)}),
	}, "\n") + "\n"
}

func codexQuotaRPCErrorFrames(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test"})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 2, Result: mustRawJSON(t, map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"id": "acct_fixture"}})}),
		codexQuotaJSONL(t, jsonRPCMessage{ID: 3, Error: &jsonRPCError{Code: -32000, Message: "rate limited"}}),
	}, "\n") + "\n"
}

func codexQuotaUnsupportedInitialize(t *testing.T) string {
	t.Helper()
	return codexQuotaJSONL(t, jsonRPCMessage{ID: 1, Result: mustRawJSON(t, map[string]any{"protocolVersion": "future"})}) + "\n"
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

func assertNoJSONRPCMember(t *testing.T, line string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		t.Fatalf("Unmarshal JSON object: %v", err)
	}
	if _, ok := fields["jsonrpc"]; ok {
		t.Fatalf("jsonrpc member present in official app-server envelope: %s", line)
	}
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

func quotaSnapshotByKey(t *testing.T, report Report, providerName string, windowKind WindowKind, scope string) QuotaSnapshot {
	t.Helper()
	for _, snapshot := range report.QuotaSnapshots {
		if snapshot.ProviderQuantityName == providerName && snapshot.WindowKind == windowKind && snapshot.ScopeKey == scope {
			return snapshot
		}
	}
	t.Fatalf("quota snapshot %s/%s/%s missing in %#v", providerName, windowKind, scope, report.QuotaSnapshots)
	return QuotaSnapshot{}
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
		data, _ := json.Marshal(jsonRPCMessage{ID: id, Result: mustRawJSONNoT(result)})
		fmt.Println(string(data))
	}
	writeError := func(id int, code int, message string) {
		data, _ := json.Marshal(jsonRPCMessage{ID: id, Error: &jsonRPCError{Code: code, Message: message}})
		fmt.Println(string(data))
	}
	writeNotification := func(method string, params any) {
		data, _ := json.Marshal(jsonRPCMessage{Method: method, Params: params})
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
	if mode == "eof" {
		return
	}
	if mode == "malformed-line" {
		fmt.Println("{not-json")
		return
	}
	if mode == "contradictory-envelope" {
		fmt.Println(`{"id":1,"method":"initialize","result":{"codexHome":"/tmp/codex","platformFamily":"unix","platformOs":"linux","userAgent":"codex-test"}}`)
		return
	}
	if mode == "trailing-json" {
		fmt.Println(`{"id":1,"result":{"codexHome":"/tmp/codex","platformFamily":"unix","platformOs":"linux","userAgent":"codex-test"}} {"method":"codex/notice"}`)
		return
	}
	if mode == "malformed-rpc-error" {
		fmt.Println(`{"id":1,"error":{"code":-32000}}`)
		return
	}
	if mode == "rpc-error-missing-code" {
		fmt.Println(`{"id":1,"error":{"message":"bad"}}`)
		return
	}
	if mode == "fractional-response-id" {
		fmt.Println(`{"id":1.5,"result":{"codexHome":"/tmp/codex","platformFamily":"unix","platformOs":"linux","userAgent":"codex-test"}}`)
		return
	}
	if mode == "exponent-response-id" {
		fmt.Println(`{"id":1e0,"result":{"codexHome":"/tmp/codex","platformFamily":"unix","platformOs":"linux","userAgent":"codex-test"}}`)
		return
	}
	if mode == "oversized-line" {
		fmt.Println(strings.Repeat("x", codexQuotaLineBytes+1))
		return
	}
	if mode == "unsupported-init" {
		writeResponse(1, map[string]any{"protocolVersion": "future"})
		return
	}
	writeResponse(1, map[string]any{"codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux", "userAgent": "codex-test"})
	writeNotification("account/rate_limits/updated", map[string]any{"ignored": true})
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
	if mode == "rpc-error" {
		writeResponse(2, map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "id": "acct_fixture"}})
		writeError(3, -32000, "rate limited")
		return
	}
	rateLimitPayload := map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 10, "windowDurationMins": 300, "resetsAt": fixedInventoryNow().Add(time.Hour).Unix()}}}
	if mode == "reorder" {
		writeResponse(3, rateLimitPayload)
		writeResponse(2, map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "id": "acct_fixture"}})
		return
	}
	writeResponse(2, map[string]any{"requiresOpenaiAuth": false, "account": map[string]any{"type": "chatgpt", "id": "acct_fixture"}})
	writeResponse(3, rateLimitPayload)
}

func mustRawJSONNoT(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
