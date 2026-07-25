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

func TestParseClaudeCodexBarUsageAllowlistAndWindows(t *testing.T) {
	now := fixedInventoryNow()
	raw := claudeCodexBarPayload(t, now, 9, 2, true)
	parsed, err := parseClaudeCodexBarUsage(raw, now)
	if err != nil {
		t.Fatalf("parseClaudeCodexBarUsage: %v", err)
	}
	if parsed.Provider != "claude" || parsed.Source != "claude" ||
		parsed.ClaudeVersion != "2.1.210" || parsed.UpdatedAt != now.Add(-time.Minute) ||
		len(parsed.Windows) != 3 || parsed.RawHash == "" {
		t.Fatalf("parsed = %#v", parsed)
	}
	want := map[string]struct {
		used    int64
		minutes int
		reset   time.Time
	}{
		"primary_5h":                 {used: 9, minutes: 300, reset: now.Add(5 * time.Hour)},
		"secondary_7d":               {used: 2, minutes: 10080, reset: now.Add(7 * 24 * time.Hour)},
		"claude-weekly-scoped-fable": {used: 2, minutes: 10080, reset: now.Add(7*24*time.Hour - time.Minute)},
	}
	for _, window := range parsed.Windows {
		expected, ok := want[window.Name]
		if !ok || window.UsedPercent != expected.used || window.WindowMinutes != expected.minutes || window.ResetAt != expected.reset {
			t.Fatalf("window = %#v, want %#v", window, expected)
		}
	}
}

func TestParseClaudeCodexBarUsagePartialAndMalformed(t *testing.T) {
	now := fixedInventoryNow()
	partial := claudeCodexBarPayloadMap(now, 8, 0, false)
	usage := partial[0]["usage"].(map[string]any)
	delete(usage, "secondary")
	delete(usage, "extraRateWindows")
	raw, _ := json.Marshal(partial)
	parsed, err := parseClaudeCodexBarUsage(string(raw), now)
	if err != nil || len(parsed.Windows) != 1 ||
		!containsString(parsed.Windows[0].PartialGaps, "secondary-window-missing") {
		t.Fatalf("partial parsed=%#v err=%v", parsed, err)
	}

	cases := []struct {
		name   string
		mutate func([]map[string]any)
	}{
		{name: "unknown-top-level-field", mutate: func(v []map[string]any) { v[0]["email"] = "person@example.com" }},
		{name: "provider-mismatch", mutate: func(v []map[string]any) { v[0]["provider"] = "codex" }},
		{name: "source-mismatch", mutate: func(v []map[string]any) { v[0]["source"] = "browser" }},
		{name: "identity-mismatch", mutate: func(v []map[string]any) {
			v[0]["usage"].(map[string]any)["identity"] = map[string]any{"providerID": "other"}
		}},
		{name: "invalid-updated-at", mutate: func(v []map[string]any) {
			v[0]["usage"].(map[string]any)["updatedAt"] = "not-rfc3339"
		}},
		{name: "invalid-reset", mutate: func(v []map[string]any) {
			v[0]["usage"].(map[string]any)["primary"].(map[string]any)["resetsAt"] = "not-rfc3339"
		}},
		{name: "fractional-percent", mutate: func(v []map[string]any) {
			v[0]["usage"].(map[string]any)["primary"].(map[string]any)["usedPercent"] = 8.5
		}},
		{name: "unsupported-tertiary", mutate: func(v []map[string]any) {
			v[0]["usage"].(map[string]any)["tertiary"] = map[string]any{"usedPercent": 1}
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			value := claudeCodexBarPayloadMap(now, 8, 2, true)
			tt.mutate(value)
			data, _ := json.Marshal(value)
			_, err := parseClaudeCodexBarUsage(string(data), now)
			if err == nil {
				t.Fatalf("parse accepted malformed payload: %s", data)
			}
		})
	}
}

func TestClaudeCodexBarAccountLinkageIsExplicitAndFailClosed(t *testing.T) {
	installationID := "pinst_claude_test"
	profile, auth := readyClaudeAuthFixture(installationID, "acct_profile_one", "auth_one")
	accountID, linkage, err := claudeCodexBarAccountLink(
		installationID, []AccountProfile{profile}, []AuthReadiness{auth},
	)
	if err != nil || accountID != profile.AccountProfileID ||
		linkage != "sole-ready-claude-auth-status-installation-linkage" {
		t.Fatalf("link account=%q linkage=%q err=%v", accountID, linkage, err)
	}
	secondProfile, secondAuth := readyClaudeAuthFixture(installationID, "acct_profile_two", "auth_two")
	if _, _, err := claudeCodexBarAccountLink(
		installationID,
		[]AccountProfile{profile, secondProfile},
		[]AuthReadiness{auth, secondAuth},
	); !errors.Is(err, ErrClaudeCodexBarAccountLinkage) {
		t.Fatalf("ambiguous profiles err=%v", err)
	}
	otherInstallationID := "pinst_other"
	auth.ProviderInstallationID = &otherInstallationID
	if _, _, err := claudeCodexBarAccountLink(
		installationID, []AccountProfile{profile}, []AuthReadiness{auth},
	); !errors.Is(err, ErrClaudeCodexBarAccountLinkage) {
		t.Fatalf("mismatched installation err=%v", err)
	}
}

func TestClaudeQuotaPrefersRealCodexBarUsageAndNeverLaunchesPTY(t *testing.T) {
	now := fixedInventoryNow()
	deps := claudeCodexBarDiscoveryDeps(t, now)
	var request CodexBarRequest
	deps.RunCodexBar = func(_ context.Context, req CodexBarRequest) (CodexBarResult, error) {
		request = req
		return CodexBarResult{Stdout: claudeCodexBarPayload(t, now, 9, 2, true), ExitCode: 0}, nil
	}
	ptyCalled := false
	deps.RunClaudePTY = func(context.Context, ClaudePTYRequest) (ClaudePTYResult, error) {
		ptyCalled = true
		return ClaudePTYResult{}, nil
	}
	report, err := Discover(context.Background(), claudeQuotaOptions(now), deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if ptyCalled {
		t.Fatal("successful CodexBar usage was overridden by Claude PTY")
	}
	if request.Timeout != claudeCodexBarTimeout || request.Timeout <= 17*time.Second ||
		strings.Join(request.Argv[1:], " ") != "usage --provider claude --format json" {
		t.Fatalf("CodexBar request = %#v", request)
	}
	sources := quotaSourcesFor(report, "claude")
	if len(sources) != 1 || sources[0].SourceKind != QuotaSourceTrustedThirdPartyBridge ||
		sources[0].SourceSchemaVersion != claudeCodexBarUsageSchema {
		t.Fatalf("Claude sources = %#v", sources)
	}
	snapshots := quotaSnapshotsFor(report, "claude")
	if len(snapshots) != 3 {
		t.Fatalf("Claude snapshots = %#v", snapshots)
	}
	for _, snapshot := range snapshots {
		if snapshot.ProviderInstallationID == nil || snapshot.AccountProfileID == nil ||
			snapshot.Confidence != ConfidenceEstimated ||
			snapshot.FieldConfidences["used_value"] != ConfidenceEstimated ||
			snapshot.FieldConfidences["remaining_value"] != ConfidenceEstimated ||
			!containsString(snapshot.GapReasons, "account-linkage-estimated") ||
			!containsString(snapshot.GapReasons, "remaining-derived-from-used-percent") {
			t.Fatalf("snapshot linkage/confidence = %#v", snapshot)
		}
		if err := ValidateQuotaSnapshot(sources[0], snapshot); err != nil {
			t.Fatalf("ValidateQuotaSnapshot: %v\n%#v", err, snapshot)
		}
	}
	primary := quotaSnapshotByProviderQuantity(t, snapshots, "primary_5h_used_percent")
	if primary.UsedValue == nil || *primary.UsedValue != 9 ||
		primary.RemainingValue == nil || *primary.RemainingValue != 91 ||
		primary.ResetAt != formatTime(now.Add(5*time.Hour)) ||
		primary.CapturedAt != formatTime(now.Add(-time.Minute)) {
		t.Fatalf("primary = %#v", primary)
	}
	data, _ := json.Marshal(report)
	for _, forbidden := range []string{"person@example.com", "@example.com", `"api_key":`, `"access_token":`, `"cookie":`} {
		if strings.Contains(strings.ToLower(string(data)), strings.ToLower(forbidden)) {
			t.Fatalf("inventory retained forbidden material %q", forbidden)
		}
	}
}

func TestClaudeCodexBarStaleAndExhaustedRemainTyped(t *testing.T) {
	now := fixedInventoryNow()
	deps := claudeCodexBarDiscoveryDeps(t, now)
	deps.RunCodexBar = func(context.Context, CodexBarRequest) (CodexBarResult, error) {
		value := claudeCodexBarPayloadMap(now, 100, 2, false)
		value[0]["usage"].(map[string]any)["updatedAt"] = now.Add(-claudeCodexBarFreshFor - time.Minute).Format(time.RFC3339)
		data, _ := json.Marshal(value)
		return CodexBarResult{Stdout: string(data), ExitCode: 0}, nil
	}
	deps.RunClaudePTY = func(context.Context, ClaudePTYRequest) (ClaudePTYResult, error) {
		t.Fatal("successful stale CodexBar evidence must not launch PTY")
		return ClaudePTYResult{}, nil
	}
	report, err := Discover(context.Background(), claudeQuotaOptions(now), deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	primary := quotaSnapshotByProviderQuantity(t, quotaSnapshotsFor(report, "claude"), "primary_5h_used_percent")
	if primary.UsedValue == nil || *primary.UsedValue != 100 ||
		primary.RemainingValue == nil || *primary.RemainingValue != 0 ||
		primary.Confidence != ConfidenceStale ||
		primary.FreshnessState != FreshnessStale ||
		primary.TerminalErrorCode != "ErrQuotaExhausted" ||
		!containsString(primary.GapReasons, "stale-provider-observation") ||
		!containsString(primary.GapReasons, "quota-exhausted") {
		t.Fatalf("stale exhausted primary = %#v", primary)
	}
}

func TestClaudeQuotaCodexBarFailureFallsBackToPTYWithTypedReason(t *testing.T) {
	now := fixedInventoryNow()
	deps := claudeCodexBarDiscoveryDeps(t, now)
	deps.RunCodexBar = func(_ context.Context, req CodexBarRequest) (CodexBarResult, error) {
		if req.Timeout != claudeCodexBarTimeout {
			t.Fatalf("timeout=%v", req.Timeout)
		}
		return CodexBarResult{TimedOut: true, Killed: true, ExitCode: -1}, context.DeadlineExceeded
	}
	ptyCalled := false
	deps.RunClaudePTY = func(context.Context, ClaudePTYRequest) (ClaudePTYResult, error) {
		ptyCalled = true
		return ClaudePTYResult{
			Output:   "Claude Code Usage\nWeekly: 20% used resets at " + now.Add(7*24*time.Hour).Format(time.RFC3339),
			ExitCode: 0,
		}, nil
	}
	report, err := Discover(context.Background(), claudeQuotaOptions(now), deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !ptyCalled {
		t.Fatal("PTY fallback was not attempted")
	}
	snapshot := onlyQuotaSnapshot(t, report, "claude")
	if snapshot.SourceKind != QuotaSourceOfficialCLIError ||
		!containsString(snapshot.GapReasons, "pty-fallback-after-codexbar-unavailable") {
		t.Fatalf("fallback snapshot = %#v", snapshot)
	}
	found := false
	for _, probe := range report.ProbeResults {
		if probe.ProbeCommandID != "claude-usage-pty" {
			continue
		}
		found = probe.Outcome == OutcomeInstalled &&
			probe.ParsedFields["codexbar_fallback_reason"] == "codexbar-timeout"
	}
	if !found {
		t.Fatalf("PTY probe did not retain typed fallback provenance: %#v", report.ProbeResults)
	}
}

func TestClaudeCodexBarCredentialShapedOutputIsRefusedWithoutPersistence(t *testing.T) {
	now := fixedInventoryNow()
	deps := claudeCodexBarDiscoveryDeps(t, now)
	canary := "AKIA" + strings.Repeat("A", 16)
	deps.RunCodexBar = func(context.Context, CodexBarRequest) (CodexBarResult, error) {
		value := claudeCodexBarPayloadMap(now, 8, 2, true)
		value[0]["api_key"] = canary
		data, _ := json.Marshal(value)
		return CodexBarResult{Stdout: string(data), ExitCode: 0}, nil
	}
	deps.RunClaudePTY = func(context.Context, ClaudePTYRequest) (ClaudePTYResult, error) {
		return ClaudePTYResult{TimedOut: true, Killed: true, ExitCode: -1}, context.DeadlineExceeded
	}
	report, err := Discover(context.Background(), claudeQuotaOptions(now), deps)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), canary) || strings.Contains(string(data), strings.Repeat("A", 16)) {
		t.Fatalf("inventory retained credential canary")
	}
	snapshot := onlyQuotaSnapshot(t, report, "claude")
	if snapshot.RawSourceHash != "" || !containsString(snapshot.GapReasons, "codexbar-primary-unavailable") {
		t.Fatalf("credential failure retained raw evidence or lost fallback reason: %#v", snapshot)
	}
}

func claudeCodexBarDiscoveryDeps(t *testing.T, now time.Time) Deps {
	t.Helper()
	dir := t.TempDir()
	claudePath := filepath.Join(dir, executableName("claude"))
	codexBarPath := filepath.Join(dir, executableName("codexbar"))
	writeExecutable(t, claudePath)
	writeExecutable(t, codexBarPath)
	deps := fakeDeps(t, map[string]string{
		filepath.Clean(claudePath):   "claude 2.1.210",
		filepath.Clean(codexBarPath): "CodexBar",
	})
	deps.Getenv = func(key string) string {
		switch key {
		case "PATH":
			return dir
		case "LANG", "LC_ALL":
			return "C"
		default:
			return ""
		}
	}
	deps.RunProbe = func(_ context.Context, req ProbeExecution) (ProbeExecutionResult, error) {
		base := filepath.Base(req.Argv[0])
		joined := strings.Join(req.Argv[1:], " ")
		switch {
		case base == executableName("claude") && joined == "--version":
			return ProbeExecutionResult{Stdout: "claude 2.1.210\n", ExitCode: 0}, nil
		case base == executableName("claude") && strings.Contains(joined, "auth status"):
			return ProbeExecutionResult{
				Stdout:   `{"loggedIn":true,"email":"person@example.com","authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`,
				ExitCode: 0,
			}, nil
		case base == executableName("codexbar") && joined == "--version":
			return ProbeExecutionResult{Stdout: "CodexBar\n", ExitCode: 0}, nil
		default:
			return ProbeExecutionResult{ExitCode: 0}, nil
		}
	}
	deps.UserHomeDir = func() (string, error) { return t.TempDir(), nil }
	return deps
}

func claudeQuotaOptions(now time.Time) Options {
	return Options{
		Config: config.Config{Adapters: config.Adapters{Worker: "claude"}},
		Now:    func() time.Time { return now },
		NetworkGrants: []NetworkGrant{{
			ProviderID: "claude",
			Purpose:    NetworkPurposeQuotaTelemetry,
			Scope:      NetworkScopeMachineInventory,
		}},
	}
}

func readyClaudeAuthFixture(installationID, accountID, authID string) (AccountProfile, AuthReadiness) {
	profile := AccountProfile{
		AdapterID:              "claude",
		AccountProfileID:       accountID,
		ProviderInstallationID: &installationID,
		SelectionState:         SelectionDefault,
		LatestAuthReadinessID:  authID,
	}
	auth := AuthReadiness{
		AdapterID:              "claude",
		AuthReadinessID:        authID,
		ProviderInstallationID: &installationID,
		AccountProfileID:       &accountID,
		ReadinessState:         ReadinessReady,
		EvidenceKind:           EvidenceMachineStatus,
	}
	return profile, auth
}

func claudeCodexBarPayload(t *testing.T, now time.Time, primaryUsed, secondaryUsed int64, extras bool) string {
	t.Helper()
	value := claudeCodexBarPayloadMap(now, primaryUsed, secondaryUsed, extras)
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(data)
}

func claudeCodexBarPayloadMap(now time.Time, primaryUsed, secondaryUsed int64, extras bool) []map[string]any {
	usage := map[string]any{
		"identity":  map[string]any{"providerID": "claude"},
		"updatedAt": now.Add(-time.Minute).Format(time.RFC3339),
		"primary": map[string]any{
			"usedPercent": primaryUsed, "resetsAt": now.Add(5 * time.Hour).Format(time.RFC3339),
			"windowMinutes": 300, "resetDescription": "in five hours",
		},
		"secondary": map[string]any{
			"usedPercent": secondaryUsed, "resetsAt": now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			"windowMinutes": 10080, "resetDescription": "in seven days",
		},
		"tertiary": nil,
	}
	if extras {
		usage["extraRateWindows"] = []map[string]any{{
			"id": "claude-weekly-scoped-fable", "title": "Fable only",
			"window": map[string]any{
				"usedPercent": 2, "resetsAt": now.Add(7*24*time.Hour - time.Minute).Format(time.RFC3339),
				"windowMinutes": 10080, "resetDescription": "in seven days",
			},
		}}
	} else {
		usage["extraRateWindows"] = []map[string]any{}
	}
	return []map[string]any{{
		"provider": "claude",
		"source":   "claude",
		"version":  "2.1.210",
		"usage":    usage,
		"pace":     map[string]any{"status": "safe-ignored"},
	}}
}

func quotaSnapshotByProviderQuantity(t *testing.T, snapshots []QuotaSnapshot, name string) QuotaSnapshot {
	t.Helper()
	for _, snapshot := range snapshots {
		if snapshot.ProviderQuantityName == name {
			return snapshot
		}
	}
	t.Fatalf("snapshot %q missing: %#v", name, snapshots)
	return QuotaSnapshot{}
}
