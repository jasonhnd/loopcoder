package providerinventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	claudeCodexBarUsageSchema = "codexbar.usage.claude.v1"
	// A real CodexBar 0.45.2 Claude usage probe has been observed taking 17.2s.
	// Keep a bounded 45s ceiling so cold starts have headroom without turning
	// provider discovery into an unbounded wait.
	claudeCodexBarTimeout     = 45 * time.Second
	claudeCodexBarStdoutBytes = 128 * 1024
	claudeCodexBarStderrBytes = 16 * 1024
	claudeCodexBarFreshFor    = 30 * time.Minute
)

var (
	ErrClaudeCodexBarMalformed      = errors.New("ErrClaudeCodexBarMalformed")
	ErrClaudeCodexBarAccountLinkage = errors.New("ErrClaudeCodexBarAccountLinkage")
)

type claudeCodexBarObservation struct {
	source    QuotaTelemetrySource
	snapshots []QuotaSnapshot
	probe     ProbeResult
	ok        bool
	reason    string
	terminal  string
}

// CodexBar's real usage output is intentionally decoded through a narrow
// allowlist. Pace and a null tertiary window are known compatibility fields;
// they are accepted but never interpreted or persisted.
type claudeCodexBarAccount struct {
	Provider string                  `json:"provider"`
	Source   string                  `json:"source"`
	Version  string                  `json:"version"`
	Usage    claudeCodexBarUsageBody `json:"usage"`
	Pace     json.RawMessage         `json:"pace"`
}

type claudeCodexBarUsageBody struct {
	Identity         claudeCodexBarIdentity      `json:"identity"`
	Primary          *claudeCodexBarUsageWindow  `json:"primary"`
	Secondary        *claudeCodexBarUsageWindow  `json:"secondary"`
	Tertiary         json.RawMessage             `json:"tertiary"`
	ExtraRateWindows []claudeCodexBarExtraWindow `json:"extraRateWindows"`
	UpdatedAt        string                      `json:"updatedAt"`
}

type claudeCodexBarIdentity struct {
	ProviderID string `json:"providerID"`
}

type claudeCodexBarExtraWindow struct {
	ID     string                     `json:"id"`
	Title  string                     `json:"title"`
	Window *claudeCodexBarUsageWindow `json:"window"`
}

type claudeCodexBarUsageWindow struct {
	UsedPercent      json.RawMessage `json:"usedPercent"`
	ResetsAt         string          `json:"resetsAt"`
	WindowMinutes    int             `json:"windowMinutes"`
	ResetDescription string          `json:"resetDescription"`
}

type claudeCodexBarParsed struct {
	Provider      string
	Source        string
	ClaudeVersion string
	UpdatedAt     time.Time
	RawHash       string
	Windows       []claudeCodexBarParsedWindow
}

type claudeCodexBarParsedWindow struct {
	Name          string
	UsedPercent   int64
	ResetAt       time.Time
	WindowMinutes int
	PartialGaps   []string
}

func inspectClaudeCodexBarQuota(
	ctx context.Context,
	discovery *discoveryContext,
	adapter AdapterDeclaration,
	installation ProviderInstallation,
	profiles []AccountProfile,
	readiness []AuthReadiness,
	now time.Time,
	deps Deps,
) claudeCodexBarObservation {
	now = now.UTC()
	source := normalizeQuotaTelemetrySource(QuotaTelemetrySource{
		AdapterID:           "claude",
		SourceKind:          QuotaSourceTrustedThirdPartyBridge,
		SourceKey:           "codexbar-usage-claude",
		SourceSchemaVersion: claudeCodexBarUsageSchema,
		SupportedQuantities: []QuantityKind{QuantityProviderDefined},
		SupportedWindows:    []WindowKind{WindowRolling, WindowProviderDefined},
		ScopeDimensions:     []string{"provider", "account", "window"},
		ConfidenceContract: map[string]Confidence{
			"limit_value":     ConfidenceEstimated,
			"used_value":      ConfidenceEstimated,
			"remaining_value": ConfidenceEstimated,
			"reset_at":        ConfidenceEstimated,
		},
		NetworkDeclared:        true,
		NetworkPermissionScope: "provider:claude/action:quota-read/source:codexbar-usage",
		Argv:                   []string{codexBarExecutableName, "usage", "--provider", "claude", "--format", "json"},
		EnvironmentKeys:        codexBarEnvKeys(),
		TimeoutMS:              int(claudeCodexBarTimeout / time.Millisecond),
		OutputLimits: OutputLimits{
			StdoutBytes: claudeCodexBarStdoutBytes, StderrBytes: claudeCodexBarStderrBytes,
			CombinedBytes: claudeCodexBarStdoutBytes + claudeCodexBarStderrBytes,
			DecodedBytes:  claudeCodexBarStdoutBytes,
		},
		ClassificationRules: []string{
			"codexbar-usage-json-field-allowlist",
			"third-party-bridge-never-exact",
			"claude-auth-status-installation-linkage",
			"used-percent-observed",
			"remaining-derived-from-used-percent",
			"redact-before-truncate",
			"no-raw-output-persistence",
			"no-email-token-cookie-or-credential-persistence",
		},
		CreatedAt:     formatTime(now),
		UpdatedAt:     formatTime(now),
		PolicyVersion: PolicyVersion,
		GapReasons:    []string{},
	})

	installationID := installation.ProviderInstallationID
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "quota"
	probe.ProbeCommandID = "codexbar-usage-claude"
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.NetworkDeclared = true
	probe.NetworkPermission = networkPermissionFor(discovery, adapter, NetworkPurposeQuotaTelemetry, true)
	probe.TimeoutMS = int(claudeCodexBarTimeout / time.Millisecond)
	probe.StdoutLimitBytes = claudeCodexBarStdoutBytes
	probe.StderrLimitBytes = claudeCodexBarStderrBytes
	probe.CombinedOutputLimitBytes = claudeCodexBarStdoutBytes + claudeCodexBarStderrBytes
	probe.StaleAfter = formatTime(now.Add(claudeCodexBarFreshFor))
	probe.Source = SourceDescriptor{
		Kind: "command", AdapterID: "claude", ProbeCommandID: probe.ProbeCommandID,
		DiscoverySource: string(DiscoveryPath), ExecutableName: codexBarExecutableName,
	}
	probe.Evidence = EvidenceSummary{
		Kind: "bounded-codexbar-usage-json", CommandBounded: true, NoShell: true,
		RepositoryMutation: false, SecretMaterialRetained: false,
	}
	probe.Argv = redactArgv(source.Argv)
	probe.EnvironmentKeys = codexBarEnvKeys()

	unavailable := func(reason, terminal string) claudeCodexBarObservation {
		source.GapReasons = dedupeStrings(append(source.GapReasons, reason, "not-collected"))
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = []string{reason}
		probe.TerminalErrorCode = terminal
		return claudeCodexBarObservation{
			source: source, probe: probe, reason: reason, terminal: terminal,
		}
	}

	if probe.NetworkPermission != NetworkGranted {
		probe.SideEffectClass = "not-run"
		return unavailable("network-permission-denied", "ErrNetworkPermissionDenied")
	}
	accountID, linkGap, err := claudeCodexBarAccountLink(installationID, profiles, readiness)
	if err != nil {
		probe.SideEffectClass = "not-run"
		return unavailable("codexbar-account-linkage-unavailable", "ErrClaudeCodexBarAccountLinkage")
	}
	bar := discoverCodexBar(ctx, deps)
	if bar.path == "" {
		probe.SideEffectClass = "not-run"
		return unavailable("codexbar-not-installed", "ErrCodexBarAbsent")
	}
	probe.Source.ExecutableName = filepath.Base(bar.path)
	argv := []string{bar.path, "usage", "--provider", "claude", "--format", "json"}
	probe.Argv = redactArgv(argv)
	env := codexBarEnv(deps.Getenv)
	result, runErr := deps.RunCodexBar(ctx, CodexBarRequest{
		Argv: argv, Env: env, Cwd: os.TempDir(),
		Timeout:            claudeCodexBarTimeout,
		StdoutLimitBytes:   claudeCodexBarStdoutBytes,
		StderrLimitBytes:   claudeCodexBarStderrBytes,
		CombinedLimitBytes: claudeCodexBarStdoutBytes + claudeCodexBarStderrBytes,
	})
	stderr, stderrFindings := redactProviderOutputBeforeTruncate(result.Stderr, 2048)
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	if result.Truncated {
		probe.StdoutSummary = "codexbar Claude usage JSON rejected because bounded output was truncated"
		return unavailable("codexbar-output-truncated", "ErrCodexBarOutputTruncated")
	}
	if runErr != nil || result.TimedOut || result.Killed {
		probe.StdoutSummary = "codexbar Claude usage command did not complete"
		if result.TimedOut {
			return unavailable("codexbar-timeout", "ErrCodexBarTimeout")
		}
		return unavailable("codexbar-execution-failed", "ErrCodexBarExecutionFailed")
	}
	if result.ExitCode != 0 {
		probe.StdoutSummary = "codexbar Claude usage command returned nonzero"
		return unavailable("codexbar-nonzero-exit", "ErrCodexBarNonZeroExit")
	}
	if codexBarCredentialMaterialLike(result.Stdout) || codexBarCredentialMaterialLike(result.Stderr) ||
		emailPattern.MatchString(result.Stdout) || emailPattern.MatchString(result.Stderr) {
		probe.StdoutSummary = "codexbar Claude usage JSON rejected by credential-material guard"
		return unavailable("credential-material-redacted", "ErrQuotaCredentialMaterial")
	}

	parsed, err := parseClaudeCodexBarUsage(result.Stdout, now)
	if err != nil {
		probe.StdoutSummary = "codexbar Claude usage JSON rejected by strict allowlist parser"
		terminal := "ErrClaudeCodexBarMalformed"
		reason := "codexbar-malformed-json"
		if errors.Is(err, ErrClaudeCodexBarAccountLinkage) {
			terminal = "ErrClaudeCodexBarAccountLinkage"
			reason = "codexbar-account-mismatch"
		}
		return unavailable(reason, terminal)
	}
	snapshots := snapshotsFromClaudeCodexBar(source, &installationID, &accountID, parsed, linkGap, now)
	if len(snapshots) == 0 {
		probe.StdoutSummary = "codexbar Claude usage JSON contained no usable windows"
		return unavailable("codexbar-no-usable-windows", "ErrClaudeCodexBarEmpty")
	}
	probe.StdoutSummary = fmt.Sprintf(
		"codexbar Claude usage JSON accepted provider claude windows %d observed %s",
		len(snapshots), formatTime(parsed.UpdatedAt),
	)
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceEstimated
	probe.FreshnessState = snapshots[0].FreshnessState
	probe.setParsedFields(map[string]string{
		"parser":                  claudeCodexBarUsageSchema,
		"provider":                parsed.Provider,
		"source":                  parsed.Source,
		"claude_version":          parsed.ClaudeVersion,
		"codexbar_version":        bar.version,
		"command_fingerprint":     bar.fingerprint,
		"snapshot_count":          strconv.Itoa(len(snapshots)),
		"account_linkage":         linkGap,
		"observation_captured_at": formatTime(parsed.UpdatedAt),
	})
	return claudeCodexBarObservation{
		source: source, snapshots: snapshots, probe: probe, ok: true,
	}
}

func claudeCodexBarAccountLink(
	installationID string,
	profiles []AccountProfile,
	readiness []AuthReadiness,
) (string, string, error) {
	profileByID := map[string]AccountProfile{}
	for _, profile := range profiles {
		if profile.AdapterID != "claude" || profile.ProviderInstallationID == nil ||
			*profile.ProviderInstallationID != installationID ||
			profile.SelectionState == SelectionSuperseded {
			continue
		}
		profileByID[profile.AccountProfileID] = profile
	}
	var candidates []string
	for _, auth := range readiness {
		if auth.AdapterID != "claude" || auth.ProviderInstallationID == nil ||
			*auth.ProviderInstallationID != installationID ||
			auth.AccountProfileID == nil ||
			auth.ReadinessState != ReadinessReady ||
			auth.EvidenceKind != EvidenceMachineStatus {
			continue
		}
		profile, ok := profileByID[*auth.AccountProfileID]
		if !ok || profile.LatestAuthReadinessID != auth.AuthReadinessID {
			continue
		}
		candidates = append(candidates, *auth.AccountProfileID)
	}
	candidates = dedupeStrings(candidates)
	if len(candidates) != 1 {
		return "", "", fmt.Errorf("%w: ready profiles for installation %d", ErrClaudeCodexBarAccountLinkage, len(candidates))
	}
	// CodexBar exposes only providerID=claude, not the Claude principal.
	// The profile ID is therefore the exact durable auth identity while the
	// quota-to-profile association remains explicitly estimated through the
	// sole ready profile on the same discovered installation.
	return candidates[0], "sole-ready-claude-auth-status-installation-linkage", nil
}

func parseClaudeCodexBarUsage(raw string, now time.Time) (claudeCodexBarParsed, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > claudeCodexBarStdoutBytes {
		return claudeCodexBarParsed{}, fmt.Errorf("%w: empty or oversized payload", ErrClaudeCodexBarMalformed)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var accounts []claudeCodexBarAccount
	if err := dec.Decode(&accounts); err != nil {
		return claudeCodexBarParsed{}, fmt.Errorf("%w: %v", ErrClaudeCodexBarMalformed, err)
	}
	if err := ensureClaudeCodexBarEOF(dec); err != nil {
		return claudeCodexBarParsed{}, err
	}
	if len(accounts) != 1 {
		return claudeCodexBarParsed{}, fmt.Errorf("%w: expected one Claude account, got %d", ErrClaudeCodexBarAccountLinkage, len(accounts))
	}
	account := accounts[0]
	if strings.TrimSpace(account.Provider) != "claude" ||
		strings.TrimSpace(account.Source) != "claude" ||
		strings.TrimSpace(account.Usage.Identity.ProviderID) != "claude" {
		return claudeCodexBarParsed{}, fmt.Errorf("%w: provider/source/identity mismatch", ErrClaudeCodexBarAccountLinkage)
	}
	if len(account.Usage.Tertiary) > 0 && string(bytes.TrimSpace(account.Usage.Tertiary)) != "null" {
		return claudeCodexBarParsed{}, fmt.Errorf("%w: unsupported tertiary window", ErrClaudeCodexBarMalformed)
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(account.Usage.UpdatedAt))
	if err != nil {
		return claudeCodexBarParsed{}, fmt.Errorf("%w: invalid updatedAt", ErrClaudeCodexBarMalformed)
	}
	updatedAt = updatedAt.UTC()
	if updatedAt.After(now.UTC().Add(5 * time.Minute)) {
		return claudeCodexBarParsed{}, fmt.Errorf("%w: future updatedAt", ErrClaudeCodexBarMalformed)
	}
	parsed := claudeCodexBarParsed{
		Provider:      "claude",
		Source:        "claude",
		ClaudeVersion: boundedToken(safeSummary(account.Version), 80),
		UpdatedAt:     updatedAt,
		RawHash:       rawSourceHash([]byte(raw)),
	}
	addWindow := func(name string, window *claudeCodexBarUsageWindow, gaps ...string) error {
		if window == nil {
			return nil
		}
		used, err := claudeCodexBarPercent(window.UsedPercent)
		if err != nil {
			return err
		}
		if window.WindowMinutes <= 0 || window.WindowMinutes > 60*24*31 {
			return fmt.Errorf("%w: invalid windowMinutes", ErrClaudeCodexBarMalformed)
		}
		resetAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(window.ResetsAt))
		if err != nil {
			return fmt.Errorf("%w: invalid resetsAt", ErrClaudeCodexBarMalformed)
		}
		parsed.Windows = append(parsed.Windows, claudeCodexBarParsedWindow{
			Name:          safeProviderQuantityName(name),
			UsedPercent:   used,
			ResetAt:       resetAt.UTC(),
			WindowMinutes: window.WindowMinutes,
			PartialGaps:   dedupeStrings(gaps),
		})
		return nil
	}
	if err := addWindow("primary_5h", account.Usage.Primary, func() string {
		if account.Usage.Secondary == nil {
			return "secondary-window-missing"
		}
		return ""
	}()); err != nil {
		return claudeCodexBarParsed{}, err
	}
	if err := addWindow("secondary_7d", account.Usage.Secondary, func() string {
		if account.Usage.Primary == nil {
			return "primary-window-missing"
		}
		return ""
	}()); err != nil {
		return claudeCodexBarParsed{}, err
	}
	for i, extra := range account.Usage.ExtraRateWindows {
		name := strings.TrimSpace(extra.ID)
		if name == "" || !codexBarSafeScopeToken(name) {
			name = fmt.Sprintf("extra_window_%d", i+1)
		}
		if err := addWindow(name, extra.Window); err != nil {
			return claudeCodexBarParsed{}, err
		}
	}
	if len(parsed.Windows) == 0 {
		return claudeCodexBarParsed{}, fmt.Errorf("%w: no observed windows", ErrClaudeCodexBarMalformed)
	}
	return parsed, nil
}

func ensureClaudeCodexBarEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: multiple JSON values", ErrClaudeCodexBarMalformed)
		}
		return fmt.Errorf("%w: trailing JSON: %v", ErrClaudeCodexBarMalformed, err)
	}
	return nil
}

func claudeCodexBarPercent(raw json.RawMessage) (int64, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return 0, fmt.Errorf("%w: missing usedPercent", ErrClaudeCodexBarMalformed)
	}
	percent, err := strconv.ParseInt(value, 10, 64)
	if err != nil || percent < 0 || percent > 100 {
		return 0, fmt.Errorf("%w: invalid usedPercent", ErrClaudeCodexBarMalformed)
	}
	return percent, nil
}

func snapshotsFromClaudeCodexBar(
	source QuotaTelemetrySource,
	installationID, accountID *string,
	parsed claudeCodexBarParsed,
	linkGap string,
	now time.Time,
) []QuotaSnapshot {
	out := make([]QuotaSnapshot, 0, len(parsed.Windows))
	for _, window := range parsed.Windows {
		limit := int64(100)
		used := window.UsedPercent
		remaining := int64(100) - used
		resetAt := formatTime(window.ResetAt)
		scope := "provider:claude/account:" + *accountID + "/window:" + window.Name
		freshness := FreshnessFresh
		confidence := ConfidenceEstimated
		gaps := []string{
			"remaining-derived-from-used-percent",
			"third-party-codexbar-usage",
			"account-linkage-estimated",
			linkGap,
		}
		gaps = append(gaps, window.PartialGaps...)
		if now.UTC().After(parsed.UpdatedAt.Add(claudeCodexBarFreshFor)) {
			freshness = FreshnessStale
			confidence = ConfidenceStale
			gaps = append(gaps, "stale-provider-observation")
		}
		terminal := ""
		if remaining == 0 {
			terminal = "ErrQuotaExhausted"
			gaps = append(gaps, "quota-exhausted")
		}
		out = append(out, normalizeQuotaSnapshot(QuotaSnapshot{
			QuotaSnapshotID:        quotaSnapshotID("claude", source.QuotaSourceID, scope, formatTime(parsed.UpdatedAt), resetAt),
			QuotaSourceID:          source.QuotaSourceID,
			SourceKind:             source.SourceKind,
			AdapterID:              "claude",
			ProviderInstallationID: installationID,
			AccountProfileID:       accountID,
			ScopeKey:               scope,
			QuantityKind:           QuantityProviderDefined,
			ProviderQuantityName:   window.Name + "_used_percent",
			Unit:                   "percent",
			WindowKind:             WindowRolling,
			RollingDurationMS:      int64(window.WindowMinutes) * int64(time.Minute/time.Millisecond),
			ResetAt:                resetAt,
			ResetSemantics:         ResetRolling,
			LimitValue:             &limit,
			UsedValue:              &used,
			RemainingValue:         &remaining,
			ValueScale:             0,
			Confidence:             confidence,
			FieldConfidences: map[string]Confidence{
				"limit_value": ConfidenceEstimated, "used_value": ConfidenceEstimated,
				"remaining_value": ConfidenceEstimated, "reset_at": ConfidenceEstimated,
			},
			FreshnessState: freshness,
			CapturedAt:     formatTime(parsed.UpdatedAt),
			ValidUntil:     resetAt,
			StaleAfter:     formatTime(parsed.UpdatedAt.Add(claudeCodexBarFreshFor)),
			RawSourceHash:  parsed.RawHash,
			RedactedDiagnostics: fmt.Sprintf(
				"claude quota observed through codexbar usage parser %s source claude window %s used %d percent remaining derived %d percent",
				claudeCodexBarUsageSchema, window.Name, used, remaining,
			),
			ConflictSet:       []string{},
			GapReasons:        dedupeStrings(gaps),
			TerminalErrorCode: terminal,
			CreatedAt:         formatTime(now),
			UpdatedAt:         formatTime(now),
			PolicyVersion:     PolicyVersion,
		}))
	}
	return out
}
