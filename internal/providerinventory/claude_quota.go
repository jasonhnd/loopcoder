package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	claudeQuotaSourceSchema = "claude.rendered_usage_status.v1"
	// Interactive /usage needs headroom after cold start + auth seed.
	claudeQuotaTimeout     = 90 * time.Second
	claudeQuotaOutputBytes = 64 * 1024
	claudeQuotaColumns     = 100
	claudeQuotaRows        = 30
	// Delay before slash commands so the TUI can finish bootstrapping.
	claudeQuotaInputDelay = 3 * time.Second
)

var (
	ErrClaudeQuotaGrantRequired  = errors.New("ErrQuotaCollectionGrantRequired")
	ErrClaudeQuotaUnsupported    = errors.New("ErrUnsupportedVersion")
	ErrClaudeQuotaMalformed      = errors.New("ErrClaudeQuotaMalformedSurface")
	ErrClaudeQuotaTimeout        = errors.New("ErrClaudeQuotaTimeout")
	ErrClaudeQuotaTruncated      = errors.New("ErrClaudeQuotaOutputTruncated")
	ErrClaudeQuotaPTYUnsupported = errors.New("ErrClaudeQuotaPTYUnsupported")

	ansiPattern              = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	claudeUsageHeaderPattern = regexp.MustCompile(`(?i)\b(claude\s+code\s+)?usage\b`)
	claudeUsageLinePattern   = regexp.MustCompile(`(?i)\b(current\s+session|session|weekly|week)\b.*?(\d{1,3})\s*%.*?\b(?:reset(?:s)?|renews?|until|through)\b\s*(?:at|on|in)?\s*([^;\n|]+)`)
)

type ClaudePTYRequest struct {
	Argv               []string
	Env                []string
	Cwd                string
	Input              string
	Timeout            time.Duration
	StdoutLimitBytes   int
	StderrLimitBytes   int
	CombinedLimitBytes int
	Columns            int
	Rows               int
}

type ClaudePTYResult struct {
	Output    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Killed    bool
	Truncated bool
}

type claudeQuotaSurface struct {
	Rows       []claudeQuotaRow
	ANSI       bool
	Locale     string
	Width      int
	CLIVersion string
	RawHash    string
}

type claudeQuotaRow struct {
	Name       string
	Percent    int64
	ResetAt    time.Time
	WindowKind WindowKind
	ScopePart  string
}

func inspectClaudeQuota(
	ctx context.Context,
	discovery *discoveryContext,
	adapter AdapterDeclaration,
	candidate candidate,
	installation ProviderInstallation,
	profiles []AccountProfile,
	readiness []AuthReadiness,
	now time.Time,
	deps Deps,
) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
	source := claudeQuotaSource(now)
	installationID := installation.ProviderInstallationID
	fallbackReason := ""
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "quota"
	probe.ProbeCommandID = "claude-usage-pty"
	probe.ProbeMethod = ProbeMethodFixedCommand
	probe.TimeoutMS = int(claudeQuotaTimeout / time.Millisecond)
	probe.StdoutLimitBytes = claudeQuotaOutputBytes
	probe.StderrLimitBytes = StderrLimitBytes
	probe.CombinedOutputLimitBytes = claudeQuotaOutputBytes + StderrLimitBytes
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.NetworkDeclared = true
	probe.NetworkPermission = networkPermissionFor(discovery, adapter, NetworkPurposeQuotaTelemetry, true)
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path)}
	probe.Evidence = EvidenceSummary{Kind: "bounded-claude-rendered-usage-pty", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	unavailable := func(reason, terminal string) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
		snapshot := claudeQuotaUnavailableSnapshot(source, &installationID, now, reason, terminal)
		if fallbackReason != "" {
			snapshot.GapReasons = dedupeStrings(append(snapshot.GapReasons, "codexbar-primary-unavailable"))
		}
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = []string{reason}
		if fallbackReason != "" {
			probe.GapReasons = dedupeStrings(append(probe.GapReasons, "codexbar-primary-unavailable"))
			probe.setParsedFields(map[string]string{
				"codexbar_fallback_reason": fallbackReason,
			})
		}
		probe.TerminalErrorCode = terminal
		return source, []QuotaSnapshot{snapshot}, probe
	}

	if probe.NetworkPermission != NetworkGranted {
		probe.SideEffectClass = "not-run"
		return unavailable("quota-collection-not-granted", "ErrQuotaCollectionGrantRequired")
	}
	if claudeQuotaUnsupportedVersion(installation.Version) {
		probe.SideEffectClass = "not-run"
		return unavailable("unsupported-cli-version", "ErrUnsupportedVersion")
	}
	codexbar := inspectClaudeCodexBarQuota(ctx, discovery, adapter, installation, profiles, readiness, now, deps)
	if codexbar.ok {
		return codexbar.source, codexbar.snapshots, codexbar.probe
	}
	fallbackReason = codexbar.reason

	root, argv, env, cleanup, err := prepareClaudeQuotaSandbox(candidate.path, deps)
	if err != nil {
		return unavailable("quota-sandbox-failed", "ErrClaudeQuotaSandboxFailed")
	}
	defer cleanup()
	probe.Argv = redactArgv(argv)
	probe.WorkingDirectory = "neutral-temp"
	probe.EnvironmentKeys = environmentKeys(env)

	result, runErr := deps.RunClaudePTY(ctx, ClaudePTYRequest{
		Argv: argv,
		Env:  env,
		Cwd:  root,
		// Enter dismisses residual prompts; then /usage and /exit.
		// "2" selects dark theme if first-run UI still appears; then /usage.
		// "1" accepts workspace trust if still shown; then /usage + /exit.
		Input:              "1\n/usage\n/exit\n",
		Timeout:            claudeQuotaTimeout,
		StdoutLimitBytes:   claudeQuotaOutputBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: claudeQuotaOutputBytes + StderrLimitBytes,
		Columns:            claudeQuotaColumns,
		Rows:               claudeQuotaRows,
	})
	stdout, stdoutFindings := redactProviderOutputBeforeTruncate(result.Output, 4096)
	stderr, stderrFindings := redactProviderOutputBeforeTruncate(result.Stderr, 4096)
	probe.StdoutSummary = stdout
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	if result.Truncated {
		return unavailable("quota-output-truncated", "ErrClaudeQuotaOutputTruncated")
	}
	if errors.Is(runErr, ErrClaudeQuotaPTYUnsupported) {
		return unavailable("quota-pty-unavailable", "ErrClaudeQuotaPTYUnsupported")
	}
	if runErr != nil || result.TimedOut || result.Killed {
		if result.TimedOut {
			if claudeWorkspaceTrustPrompt(result.Output) {
				return unavailable("quota-workspace-trust-prompt", "ErrClaudeQuotaWorkspaceTrustPrompt")
			}
			return unavailable("quota-probe-timeout", "ErrClaudeQuotaTimeout")
		}
		return unavailable("quota-probe-failed", "ErrClaudeQuotaExecutionFailed")
	}
	if result.ExitCode != 0 {
		return unavailable("quota-probe-nonzero-exit", "ErrClaudeQuotaNonZeroExit")
	}
	if credentialMaterialLike(result.Output) || credentialMaterialLike(result.Stderr) {
		return unavailable("credential-material-redacted", "ErrQuotaCredentialMaterial")
	}
	surface, err := parseClaudeUsageSurface(result.Output, installation.Version, localeFromEnv(env), claudeQuotaColumns, now)
	if err != nil {
		return unavailable(claudeQuotaReason(err), claudeQuotaTerminal(err))
	}
	snapshots := snapshotsFromClaudeUsageSurface(source, &installationID, surface, now)
	if len(snapshots) == 0 {
		return unavailable("unsupported-usage-surface", "ErrClaudeQuotaMalformedSurface")
	}
	for i := range snapshots {
		snapshots[i].GapReasons = dedupeStrings(append(snapshots[i].GapReasons, "pty-fallback-after-codexbar-unavailable"))
	}
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.setParsedFields(map[string]string{
		"parser":                   claudeQuotaSourceSchema,
		"cli_version":              installation.Version,
		"locale":                   surface.Locale,
		"terminal_width":           strconv.Itoa(surface.Width),
		"ansi":                     strconv.FormatBool(surface.ANSI),
		"snapshot_count":           strconv.Itoa(len(snapshots)),
		"codexbar_fallback_reason": fallbackReason,
	})
	return source, snapshots, probe
}

func claudeWorkspaceTrustPrompt(output string) bool {
	normalized := strings.ToLower(ansiPattern.ReplaceAllString(output, ""))
	return (strings.Contains(normalized, "trust") && strings.Contains(normalized, "workspace")) ||
		(strings.Contains(normalized, "trust") && strings.Contains(normalized, "folder")) ||
		strings.Contains(normalized, "yes, i trust")
}

func claudeQuotaSource(now time.Time) QuotaTelemetrySource {
	now = now.UTC()
	return normalizeQuotaTelemetrySource(QuotaTelemetrySource{
		AdapterID:              "claude",
		SourceKind:             QuotaSourceOfficialCLIError,
		SourceKey:              "claude-usage-rendered-status-v1",
		SourceSchemaVersion:    claudeQuotaSourceSchema,
		SupportedQuantities:    []QuantityKind{QuantityProviderDefined},
		SupportedWindows:       []WindowKind{WindowFixedWeek, WindowProviderDefined, WindowUnknown},
		ScopeDimensions:        []string{"provider", "account", "scope"},
		ConfidenceContract:     map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceExact, "remaining_value": ConfidenceEstimated, "reset_at": ConfidenceExact},
		NetworkDeclared:        true,
		NetworkPermissionScope: "provider:claude/action:quota-read/side-effect:read/freshness:interactive",
		Argv:                   claudeQuotaSourceArgv(),
		EnvironmentKeys:        claudeQuotaEnvKeys(),
		TimeoutMS:              int(claudeQuotaTimeout / time.Millisecond),
		OutputLimits:           OutputLimits{StdoutBytes: claudeQuotaOutputBytes, StderrBytes: StderrLimitBytes, CombinedBytes: claudeQuotaOutputBytes + StderrLimitBytes, DecodedBytes: claudeQuotaOutputBytes},
		ClassificationRules:    []string{"rendered-status-allowlist", "ansi-normalized", "redact-before-truncate", "no-credential-material", "no-login-update-or-provider-work"},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
		GapReasons:             []string{},
	})
}

func claudeQuotaSourceArgv() []string {
	// Keep tool denylist aligned with current Claude Code tool names only.
	// Obsolete names (MultiEdit, LS, …) emit startup warnings and slow the
	// interactive surface that hosts /usage.
	return []string{
		"claude",
		"--bare",
		"--disallowedTools", "Bash,Edit,Write,Read,Glob,Grep,Task,WebFetch,WebSearch,NotebookEdit",
		"--mcp-config", "loopcoder-empty-mcp.json",
		"--strict-mcp-config",
	}
}

func claudeQuotaUnavailableSnapshot(source QuotaTelemetrySource, installationID *string, now time.Time, reason, terminal string) QuotaSnapshot {
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("claude", source.QuotaSourceID, reason, formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "claude",
		ProviderInstallationID: installationID,
		ScopeKey:               "provider:claude",
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   "quota",
		Unit:                   "provider-defined",
		WindowKind:             WindowUnknown,
		ResetSemantics:         ResetUnknown,
		ValueScale:             0,
		Confidence:             ConfidenceUnavailable,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnavailable, "used_value": ConfidenceUnavailable, "remaining_value": ConfidenceUnavailable, "reset_at": ConfidenceUnavailable},
		FreshnessState:         FreshnessNotApplicable,
		CapturedAt:             formatTime(now),
		RedactedDiagnostics:    "claude rendered usage quota unavailable due to " + reason,
		ConflictSet:            []string{},
		GapReasons:             []string{reason, "not-collected"},
		TerminalErrorCode:      terminal,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func prepareClaudeQuotaSandbox(executable string, deps Deps) (string, []string, []string, func(), error) {
	root, err := deps.MkdirTemp("", "loopcoder-claude-quota-*")
	if err != nil {
		return "", nil, nil, func() {}, err
	}
	cleanup := func() { _ = deps.RemoveAll(root) }
	for _, dir := range []string{"home", "xdg-config", "xdg-cache", "xdg-state", "appdata", "localappdata", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			cleanup()
			return "", nil, nil, func() {}, err
		}
	}
	// Seed allowlisted Claude auth/config files into the private HOME so
	// interactive /usage can complete. Without this, the PTY hangs on login
	// because credentials live under ~/.claude and env secrets are denied.
	// File contents never enter probe records (output is redacted separately).
	// Trust the sandbox cwd so the workspace trust dialog is skipped.
	if err := seedClaudeQuotaAuthHome(filepath.Join(root, "home"), root, deps); err != nil {
		cleanup()
		return "", nil, nil, func() {}, err
	}
	mcpPath := filepath.Join(root, "loopcoder-empty-mcp.json")
	if err := deps.WriteFile(mcpPath, []byte(`{"mcpServers":{}}`+"\n"), 0o600); err != nil {
		cleanup()
		return "", nil, nil, func() {}, err
	}
	argv := claudeQuotaSourceArgv()
	argv[0] = executable
	for i := range argv {
		if argv[i] == "loopcoder-empty-mcp.json" {
			argv[i] = mcpPath
		}
	}
	env := claudeQuotaEnvironment(deps.Getenv, root)
	return root, argv, env, cleanup, nil
}

// seedClaudeQuotaAuthHome copies a minimal allowlist of Claude Code local auth
// artifacts into the sandbox home. Missing sources are OK (probe will fail
// closed as before). No credential env vars are introduced.
//
// Writes a redacted ~/.claude.json stub that:
//   - hasCompletedOnboarding=true (skip theme picker)
//   - trusts sandboxCwd (skip "Is this a project you trust?" dialog)
//
// cwd must match the PTY working directory or the trust dialog reappears.
func seedClaudeQuotaAuthHome(sandboxHome, sandboxCwd string, deps Deps) error {
	if sandboxHome == "" {
		return errors.New("claude quota sandbox home empty")
	}
	realHome := ""
	if deps.UserHomeDir != nil {
		if h, err := deps.UserHomeDir(); err == nil {
			realHome = strings.TrimSpace(h)
		}
	}
	if realHome == "" && deps.Getenv != nil {
		realHome = strings.TrimSpace(deps.Getenv("HOME"))
	}
	if realHome == "" {
		return nil
	}
	srcRoot := filepath.Join(realHome, ".claude")
	dstRoot := filepath.Join(sandboxHome, ".claude")
	if err := os.MkdirAll(dstRoot, 0o700); err != nil {
		return err
	}
	// Credentials only — do not copy host settings.json (hooks/plugins/deny
	// rules inject obsolete MultiEdit/LS warnings and slow TUI bootstrap).
	credSrc := filepath.Join(srcRoot, ".credentials.json")
	if info, err := os.Lstat(credSrc); err == nil && info.Mode().IsRegular() && info.Size() > 0 && info.Size() <= 256*1024 {
		if raw, err := os.ReadFile(credSrc); err == nil {
			if err := os.WriteFile(filepath.Join(dstRoot, ".credentials.json"), raw, 0o600); err != nil {
				return err
			}
		}
	}
	// Minimal settings: skip prompts without host hooks/plugins.
	minimalSettings := []byte(`{
  "tui": "fullscreen",
  "skipDangerousModePermissionPrompt": true,
  "skipWorkflowUsageWarning": true
}
`)
	if err := os.WriteFile(filepath.Join(dstRoot, "settings.json"), minimalSettings, 0o600); err != nil {
		return err
	}
	// Global state: skip theme/onboarding + workspace trust for the probe cwd.
	// Do not copy the full host ~/.claude.json (projects map, caches, PII).
	cwd := strings.TrimSpace(sandboxCwd)
	if cwd == "" {
		cwd = sandboxHome
	}
	// Escape JSON string for path.
	cwdJSON, _ := json.Marshal(cwd)
	onboardStub := []byte(fmt.Sprintf(`{
  "hasCompletedOnboarding": true,
  "lastOnboardingVersion": "2.1.210",
  "numStartups": 10,
  "theme": "dark",
  "optionAsMetaKeyInstalled": true,
  "effortCalloutV2Dismissed": true,
  "projects": {
    %s: {
      "hasTrustDialogAccepted": true,
      "hasCompletedProjectOnboarding": true,
      "projectOnboardingSeenCount": 1,
      "allowedTools": []
    }
  }
}
`, string(cwdJSON)))
	if err := os.WriteFile(filepath.Join(sandboxHome, ".claude.json"), onboardStub, 0o600); err != nil {
		return err
	}
	return nil
}

func claudeQuotaEnvKeys() []string {
	return []string{"PATH", "TERM", "LANG", "LC_ALL", "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "TMPDIR", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_AUTOUPDATER"}
}

func claudeQuotaEnvironment(getenv func(string) string, root string) []string {
	if getenv == nil {
		getenv = os.Getenv
	}
	valueFor := map[string]string{
		"PATH":            getenv("PATH"),
		"TERM":            firstNonEmpty(getenv("TERM"), "xterm-256color"),
		"LANG":            firstNonEmpty(getenv("LANG"), "C"),
		"LC_ALL":          firstNonEmpty(getenv("LC_ALL"), "C"),
		"HOME":            filepath.Join(root, "home"),
		"USERPROFILE":     filepath.Join(root, "home"),
		"XDG_CONFIG_HOME": filepath.Join(root, "xdg-config"),
		"XDG_CACHE_HOME":  filepath.Join(root, "xdg-cache"),
		"XDG_STATE_HOME":  filepath.Join(root, "xdg-state"),
		"APPDATA":         filepath.Join(root, "appdata"),
		"LOCALAPPDATA":    filepath.Join(root, "localappdata"),
		"TEMP":            filepath.Join(root, "tmp"),
		"TMP":             filepath.Join(root, "tmp"),
		"TMPDIR":          filepath.Join(root, "tmp"),
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"DISABLE_AUTOUPDATER":                      "1",
	}
	var env []string
	for _, key := range claudeQuotaEnvKeys() {
		if probeEnvNameDenied(key) && !strings.HasPrefix(key, "CLAUDE_CODE_DISABLE_") {
			continue
		}
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		if value := valueFor[key]; value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func parseClaudeUsageSurface(output, cliVersion, locale string, width int, now time.Time) (claudeQuotaSurface, error) {
	if len(output) > claudeQuotaOutputBytes {
		return claudeQuotaSurface{}, ErrClaudeQuotaTruncated
	}
	if credentialMaterialLike(output) {
		return claudeQuotaSurface{}, ErrQuotaCredentialMaterial
	}
	ansi := ansiPattern.MatchString(output)
	normalized := ansiPattern.ReplaceAllString(strings.ReplaceAll(output, "\r\n", "\n"), "")
	if !claudeUsageHeaderPattern.MatchString(normalized) {
		return claudeQuotaSurface{}, fmt.Errorf("%w: missing usage header", ErrClaudeQuotaMalformed)
	}
	var rows []claudeQuotaRow
	for _, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		match := claudeUsageLinePattern.FindStringSubmatch(line)
		if len(match) != 4 {
			continue
		}
		percent, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil || percent < 0 || percent > 100 {
			return claudeQuotaSurface{}, fmt.Errorf("%w: invalid percentage", ErrClaudeQuotaMalformed)
		}
		resetAt, ok := parseClaudeResetTime(match[3], now)
		if !ok {
			return claudeQuotaSurface{}, fmt.Errorf("%w: invalid reset", ErrClaudeQuotaMalformed)
		}
		name := strings.ToLower(strings.Join(strings.Fields(match[1]), "-"))
		row := claudeQuotaRow{Name: name, Percent: percent, ResetAt: resetAt}
		if strings.Contains(name, "week") {
			row.WindowKind = WindowFixedWeek
			row.ScopePart = "weekly"
		} else {
			row.WindowKind = WindowProviderDefined
			row.ScopePart = "current-session"
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return claudeQuotaSurface{}, fmt.Errorf("%w: no supported usage windows", ErrClaudeQuotaMalformed)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ScopePart < rows[j].ScopePart })
	return claudeQuotaSurface{
		Rows:       rows,
		ANSI:       ansi,
		Locale:     firstNonEmpty(locale, "C"),
		Width:      width,
		CLIVersion: safeSummary(cliVersion),
		RawHash:    rawSourceHash([]byte(normalized)),
	}, nil
}

func snapshotsFromClaudeUsageSurface(source QuotaTelemetrySource, installationID *string, surface claudeQuotaSurface, now time.Time) []QuotaSnapshot {
	var out []QuotaSnapshot
	for _, row := range surface.Rows {
		used := row.Percent
		remaining := int64(100) - used
		resetAt := formatTime(row.ResetAt)
		windowStart := ""
		windowEnd := ""
		if row.WindowKind == WindowFixedWeek {
			windowStart = formatTime(row.ResetAt.Add(-7 * 24 * time.Hour))
			windowEnd = resetAt
		}
		scope := "provider:claude/scope:" + row.ScopePart
		diag := fmt.Sprintf("claude usage parser %s cli version %s locale %s terminal width %d ansi %t rendered status", claudeQuotaSourceSchema, surface.CLIVersion, safeSummary(surface.Locale), surface.Width, surface.ANSI)
		out = append(out, normalizeQuotaSnapshot(QuotaSnapshot{
			QuotaSnapshotID:        quotaSnapshotID("claude", source.QuotaSourceID, scope, string(row.WindowKind), resetAt, formatTime(now)),
			QuotaSourceID:          source.QuotaSourceID,
			SourceKind:             source.SourceKind,
			AdapterID:              "claude",
			ProviderInstallationID: installationID,
			ScopeKey:               scope,
			QuantityKind:           QuantityProviderDefined,
			ProviderQuantityName:   row.ScopePart + "_used_percent",
			Unit:                   "percent",
			WindowKind:             row.WindowKind,
			WindowStart:            windowStart,
			WindowEnd:              windowEnd,
			ResetAt:                resetAt,
			ResetSemantics:         claudeResetSemantics(row.WindowKind),
			UsedValue:              &used,
			RemainingValue:         &remaining,
			ValueScale:             0,
			Confidence:             ConfidenceExact,
			FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceExact, "remaining_value": ConfidenceEstimated, "reset_at": ConfidenceExact},
			FreshnessState:         FreshnessFresh,
			CapturedAt:             formatTime(now),
			ValidUntil:             resetAt,
			StaleAfter:             resetAt,
			RawSourceHash:          surface.RawHash,
			RedactedDiagnostics:    diag,
			ConflictSet:            []string{},
			GapReasons:             []string{"remaining-derived-from-used-percent", "rendered-status-surface"},
			CreatedAt:              formatTime(now),
			UpdatedAt:              formatTime(now),
			PolicyVersion:          PolicyVersion,
		}))
	}
	return out
}

func claudeResetSemantics(kind WindowKind) ResetSemantics {
	if kind == WindowFixedWeek {
		return ResetWindowBoundary
	}
	return ResetProviderDefined
}

func parseClaudeResetTime(value string, now time.Time) (time.Time, bool) {
	value = strings.TrimSpace(strings.Trim(value, " .);]"))
	if value == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05 MST",
		"2006-01-02 15:04 MST",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"Jan 2, 2006 3:04 PM MST",
		"Jan 2, 2006 3:04 PM",
		"January 2, 2006 3:04 PM MST",
		"January 2, 2006 3:04 PM",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), true
		}
		if t, err := time.ParseInLocation(layout, value, now.Location()); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func claudeQuotaUnsupportedVersion(version string) bool {
	parts := firstVersionParts(version)
	if len(parts) == 0 {
		return false
	}
	return semanticVersionLess(version, []int{1, 0, 0})
}

func localeFromEnv(env []string) string {
	for _, key := range []string{"LC_ALL", "LANG"} {
		prefix := key + "="
		for _, entry := range env {
			if strings.HasPrefix(entry, prefix) {
				return strings.TrimPrefix(entry, prefix)
			}
		}
	}
	return "C"
}

func claudeQuotaReason(err error) string {
	switch {
	case errors.Is(err, ErrClaudeQuotaUnsupported):
		return "unsupported-cli-version"
	case errors.Is(err, ErrClaudeQuotaTruncated):
		return "quota-output-truncated"
	case errors.Is(err, ErrQuotaCredentialMaterial):
		return "credential-material-redacted"
	case errors.Is(err, ErrClaudeQuotaMalformed):
		return "unsupported-usage-surface"
	default:
		return "quota-probe-failed"
	}
}

func claudeQuotaTerminal(err error) string {
	switch {
	case errors.Is(err, ErrClaudeQuotaUnsupported):
		return "ErrUnsupportedVersion"
	case errors.Is(err, ErrClaudeQuotaTruncated):
		return "ErrClaudeQuotaOutputTruncated"
	case errors.Is(err, ErrQuotaCredentialMaterial):
		return "ErrQuotaCredentialMaterial"
	case errors.Is(err, ErrClaudeQuotaMalformed):
		return "ErrClaudeQuotaMalformedSurface"
	default:
		return "ErrClaudeQuotaExecutionFailed"
	}
}
