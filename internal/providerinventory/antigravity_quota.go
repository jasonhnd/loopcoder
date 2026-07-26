package providerinventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	antigravityCodexBarUsageSchema = "codexbar.usage.antigravity.v1"
	antigravityQuotaTimeout        = 20 * time.Second
	antigravityQuotaStdoutBytes    = 128 * 1024
	antigravityQuotaStderrBytes    = 16 * 1024
)

// inspectAntigravityQuota collects real quota windows from the machine-readable
// CodexBar usage surface for Antigravity:
//
//	codexbar usage --provider antigravity --format json
//
// This is not capacity invention: only windows with observed usedPercent and
// optional resetsAt are emitted. Auth-ready alone never becomes remaining.
func inspectAntigravityQuota(ctx context.Context, discovery *discoveryContext, adapter AdapterDeclaration, installation ProviderInstallation, now time.Time, deps Deps) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
	now = now.UTC()
	source := normalizeQuotaTelemetrySource(QuotaTelemetrySource{
		AdapterID:           "antigravity",
		SourceKind:          QuotaSourceOfficialCLICommand,
		SourceKey:           "codexbar-usage-antigravity",
		SourceSchemaVersion: antigravityCodexBarUsageSchema,
		SupportedQuantities: []QuantityKind{QuantityProviderDefined},
		SupportedWindows:    []WindowKind{WindowFixedHour, WindowFixedWeek, WindowRolling, WindowProviderDefined, WindowUnknown},
		ScopeDimensions:     []string{"provider", "account", "window"},
		ConfidenceContract: map[string]Confidence{
			"used_value":      ConfidenceExact,
			"remaining_value": ConfidenceEstimated, // derived as 100-usedPercent
			"limit_value":     ConfidenceExact,     // percent scale 100
			"reset_at":        ConfidenceExact,
		},
		NetworkDeclared:        true,
		NetworkPermissionScope: "provider:antigravity/action:quota-read/source:codexbar-usage",
		Argv:                   []string{codexBarExecutableName, "usage", "--provider", "antigravity", "--format", "json"},
		EnvironmentKeys:        codexBarEnvKeys(),
		TimeoutMS:              int(antigravityQuotaTimeout / time.Millisecond),
		OutputLimits: OutputLimits{
			StdoutBytes: antigravityQuotaStdoutBytes, StderrBytes: antigravityQuotaStderrBytes,
			CombinedBytes: antigravityQuotaStdoutBytes + antigravityQuotaStderrBytes,
			DecodedBytes:  antigravityQuotaStdoutBytes,
		},
		ClassificationRules: []string{
			"codexbar-usage-json-field-allowlist",
			"usedPercent-observed-only",
			"remaining-derived-from-used-percent",
			"no-credential-material",
			"no-raw-email-persistence",
		},
		CreatedAt:     formatTime(now),
		UpdatedAt:     formatTime(now),
		PolicyVersion: PolicyVersion,
		GapReasons:    []string{},
	})

	probe := baseProbe(adapter, now, deps)
	probe.ProbeKind = "quota"
	probe.ProbeCommandID = "codexbar-usage-antigravity"
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.NetworkDeclared = true
	probe.NetworkPermission = networkPermissionFor(discovery, adapter, NetworkPurposeQuotaTelemetry, true)
	probe.TimeoutMS = int(antigravityQuotaTimeout / time.Millisecond)
	probe.StdoutLimitBytes = antigravityQuotaStdoutBytes
	probe.StderrLimitBytes = antigravityQuotaStderrBytes
	probe.CombinedOutputLimitBytes = antigravityQuotaStdoutBytes + antigravityQuotaStderrBytes
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.Source = SourceDescriptor{
		Kind: "command", AdapterID: "antigravity", ProbeCommandID: probe.ProbeCommandID,
		DiscoverySource: string(DiscoveryPath), ExecutableName: codexBarExecutableName,
	}
	probe.Evidence = EvidenceSummary{
		Kind: "bounded-codexbar-usage-json", CommandBounded: true, NoShell: true,
		RepositoryMutation: false, SecretMaterialRetained: false,
	}
	probe.Argv = redactArgv(source.Argv)
	probe.EnvironmentKeys = codexBarEnvKeys()
	installID := installation.ProviderInstallationID
	var installIDPtr *string
	if installID != "" {
		installIDPtr = &installID
		probe.ProviderInstallationID = installIDPtr
	}
	unavailable := func(reason, terminal string) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
		source.GapReasons = append([]string{}, reason, "not-collected")
		snap := normalizeQuotaSnapshot(QuotaSnapshot{
			QuotaSnapshotID:        quotaSnapshotID("antigravity", source.QuotaSourceID, reason, formatTime(now)),
			QuotaSourceID:          source.QuotaSourceID,
			SourceKind:             source.SourceKind,
			AdapterID:              "antigravity",
			ProviderInstallationID: installIDPtr,
			ScopeKey:               "provider:antigravity",
			QuantityKind:           QuantityProviderDefined,
			ProviderQuantityName:   "quota",
			Unit:                   "percent",
			WindowKind:             WindowUnknown,
			ResetSemantics:         ResetUnknown,
			Confidence:             ConfidenceUnavailable,
			FieldConfidences: map[string]Confidence{
				"limit_value": ConfidenceUnavailable, "used_value": ConfidenceUnavailable,
				"remaining_value": ConfidenceUnavailable, "reset_at": ConfidenceUnavailable,
			},
			FreshnessState: FreshnessNotApplicable,
			CapturedAt:     formatTime(now),
			// Prefer phrase form over "reason: token" shapes that can look credential-like.
			RedactedDiagnostics: "antigravity quota via codexbar usage unavailable due to " + reason,
			GapReasons:          []string{reason, "not-collected"},
			TerminalErrorCode:   terminal,
			CreatedAt:           formatTime(now),
			UpdatedAt:           formatTime(now),
			PolicyVersion:       PolicyVersion,
		})
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = []string{reason}
		probe.TerminalErrorCode = terminal
		return source, []QuotaSnapshot{snap}, probe
	}

	if probe.NetworkPermission != NetworkGranted {
		return unavailable("network-permission-denied", "ErrNetworkPermissionDenied")
	}

	bar := discoverCodexBar(ctx, deps)
	if bar.path == "" {
		return unavailable("codexbar-not-installed", "ErrCodexBarAbsent")
	}
	if !bar.versionOK {
		// "CodexBar unknown" still runs usage successfully; only hard-fail if path missing.
		// Keep going with path present.
	}
	argv := []string{bar.path, "usage", "--provider", "antigravity", "--format", "json"}
	probe.Argv = redactArgv(argv)
	probe.Source.ExecutableName = filepath.Base(bar.path)

	env := codexBarEnv(deps.Getenv)
	run := deps.RunCodexBar
	if run == nil {
		run = runCodexBarQuota
	}
	result, err := run(ctx, CodexBarRequest{
		Argv: argv, Env: env, Cwd: os.TempDir(),
		Timeout:            antigravityQuotaTimeout,
		StdoutLimitBytes:   antigravityQuotaStdoutBytes,
		StderrLimitBytes:   antigravityQuotaStderrBytes,
		CombinedLimitBytes: antigravityQuotaStdoutBytes + antigravityQuotaStderrBytes,
	})
	stdout, n1 := redactProviderOutputBeforeTruncate(result.Stdout, 4096)
	stderr, n2 := redactProviderOutputBeforeTruncate(result.Stderr, 2048)
	probe.StdoutSummary = stdout
	probe.StderrSummary = stderr
	probe.SecretFindingCount = n1 + n2
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	if result.Truncated {
		return unavailable("codexbar-output-truncated", "ErrCodexBarOutputTruncated")
	}
	if err != nil || result.TimedOut || result.Killed {
		if result.TimedOut {
			return unavailable("codexbar-timeout", "ErrCodexBarTimeout")
		}
		return unavailable("codexbar-execution-failed", "ErrCodexBarExecutionFailed")
	}
	if result.ExitCode != 0 {
		return unavailable("codexbar-nonzero-exit", "ErrCodexBarNonZeroExit")
	}
	if codexBarCredentialMaterialLike(result.Stdout) {
		return unavailable("credential-material-redacted", "ErrQuotaCredentialMaterial")
	}

	snaps, perr := parseAntigravityCodexBarUsage(result.Stdout, source, installIDPtr, now)
	if perr != nil {
		return unavailable(perr.Error(), "ErrAntigravityQuotaMalformed")
	}
	if len(snaps) == 0 {
		return unavailable("no-usable-windows", "ErrAntigravityQuotaEmpty")
	}
	source.ConfidenceContract = map[string]Confidence{
		"used_value": ConfidenceExact, "remaining_value": ConfidenceEstimated,
		"limit_value": ConfidenceExact, "reset_at": ConfidenceExact,
	}
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.FreshnessState = FreshnessFresh
	probe.setParsedFields(map[string]string{
		"parser": antigravityCodexBarUsageSchema, "provider": "antigravity",
		"snapshot_count": strconv.Itoa(len(snaps)), "codexbar_version": bar.version,
	})
	return source, snaps, probe
}

type agUsageAccount struct {
	Provider string      `json:"provider"`
	Source   string      `json:"source"`
	Usage    agUsageBody `json:"usage"`
}

type agUsageBody struct {
	LoginMethod      string          `json:"loginMethod"`
	Primary          *agUsageWindow  `json:"primary"`
	Secondary        *agUsageWindow  `json:"secondary"`
	ExtraRateWindows []agExtraWindow `json:"extraRateWindows"`
	UpdatedAt        string          `json:"updatedAt"`
	DataConfidence   string          `json:"dataConfidence"`
}

type agExtraWindow struct {
	ID     string         `json:"id"`
	Title  string         `json:"title"`
	Window *agUsageWindow `json:"window"`
}

type agUsageWindow struct {
	UsedPercent      *float64 `json:"usedPercent"`
	ResetsAt         string   `json:"resetsAt"`
	WindowMinutes    int      `json:"windowMinutes"`
	ResetDescription string   `json:"resetDescription"`
}

func parseAntigravityCodexBarUsage(raw string, source QuotaTelemetrySource, installID *string, now time.Time) ([]QuotaSnapshot, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty usage payload")
	}
	var accounts []agUsageAccount
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.UseNumber()
	if err := dec.Decode(&accounts); err != nil {
		// Some versions may emit a single object.
		var one agUsageAccount
		if err2 := json.Unmarshal([]byte(raw), &one); err2 != nil {
			return nil, fmt.Errorf("malformed usage json: %v", err)
		}
		accounts = []agUsageAccount{one}
	}
	var out []QuotaSnapshot
	for _, acc := range accounts {
		prov := strings.ToLower(strings.TrimSpace(acc.Provider))
		if prov != "" && prov != "antigravity" {
			continue
		}
		// Prefer explicit extra windows (named) then primary/secondary.
		for _, extra := range acc.Usage.ExtraRateWindows {
			if extra.Window == nil {
				continue
			}
			name := firstNonEmpty(extra.ID, extra.Title, "window")
			if snap, ok := agWindowToSnapshot(extra.Window, source, installID, name, acc.Usage.UpdatedAt, acc.Usage.DataConfidence, now); ok {
				out = append(out, snap)
			}
		}
		if acc.Usage.Primary != nil {
			if snap, ok := agWindowToSnapshot(acc.Usage.Primary, source, installID, "primary_5h", acc.Usage.UpdatedAt, acc.Usage.DataConfidence, now); ok {
				out = append(out, snap)
			}
		}
		if acc.Usage.Secondary != nil {
			if snap, ok := agWindowToSnapshot(acc.Usage.Secondary, source, installID, "secondary", acc.Usage.UpdatedAt, acc.Usage.DataConfidence, now); ok {
				out = append(out, snap)
			}
		}
	}
	return out, nil
}

func agWindowToSnapshot(w *agUsageWindow, source QuotaTelemetrySource, installID *string, name, updatedAt, dataConf string, now time.Time) (QuotaSnapshot, bool) {
	if w == nil || w.UsedPercent == nil {
		// No observed usedPercent ⇒ not capacity evidence (honest unknown).
		return QuotaSnapshot{}, false
	}
	usedFrac := *w.UsedPercent
	// Normalize: values in (0,1] treated as fractions; (1,100] as percent.
	usedPct := usedFrac
	if usedPct <= 1.0 {
		usedPct = usedPct * 100.0
	}
	if usedPct < 0 {
		usedPct = 0
	}
	if usedPct > 100 {
		usedPct = 100
	}
	used := int64(math.Round(usedPct))
	remaining := int64(100 - used)
	if remaining < 0 {
		remaining = 0
	}
	limit := int64(100)

	// Always provider-defined: we observe usedPercent+reset without explicit
	// window_start/window_end (fixed-* kinds require both bounds).
	windowKind := WindowProviderDefined
	_ = w.WindowMinutes

	gaps := []string{"remaining-derived-from-used-percent", "missing-exact-account-identity"}
	resetAt := strings.TrimSpace(w.ResetsAt)
	resetSemantics := ResetUnknown
	if resetAt != "" {
		if t, err := time.Parse(time.RFC3339, resetAt); err == nil {
			resetAt = t.UTC().Format(time.RFC3339)
			resetSemantics = ResetWindowBoundary
		} else {
			resetAt = ""
			gaps = append(gaps, "invalid-reset-at")
		}
	} else {
		gaps = append(gaps, "missing-reset-at")
	}
	captured := now
	if u := strings.TrimSpace(updatedAt); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			captured = t.UTC()
		} else {
			// The command response is still observed now, but the provider's
			// own updatedAt cannot be asserted as its observation timestamp.
			gaps = append(gaps, "invalid-observed-at")
		}
	}

	// Observed used is exact; remaining is estimated derivation (100-used).
	usedConf := ConfidenceExact
	remConf := ConfidenceEstimated
	if strings.EqualFold(strings.TrimSpace(dataConf), "exact") {
		// Keep remaining estimated — we did not observe remaining directly.
	}
	resetConf := ConfidenceUnknown
	if resetAt != "" {
		resetConf = ConfidenceExact
	}
	terminal := ""
	if remaining == 0 {
		terminal = "ErrQuotaExhausted"
		gaps = append(gaps, "quota-exhausted")
	}

	name = safeProviderQuantityName(name)
	scope := "provider:antigravity|window:" + name
	snap := normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("antigravity", source.QuotaSourceID, scope, formatTime(captured), name),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "antigravity",
		ProviderInstallationID: installID,
		ScopeKey:               scope,
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   name,
		Unit:                   "percent",
		WindowKind:             windowKind,
		RollingDurationMS:      int64(w.WindowMinutes) * 60 * 1000,
		ResetAt:                resetAt,
		ResetSemantics:         resetSemantics,
		LimitValue:             &limit,
		UsedValue:              &used,
		RemainingValue:         &remaining,
		ValueScale:             0,
		Confidence:             ConfidenceExact,
		FieldConfidences: map[string]Confidence{
			"limit_value": ConfidenceExact, "used_value": usedConf,
			"remaining_value": remConf, "reset_at": resetConf,
		},
		FreshnessState: FreshnessFresh,
		CapturedAt:     formatTime(captured),
		ValidUntil:     resetAt,
		StaleAfter:     formatTime(captured.Add(30 * time.Minute)),
		// Avoid key=value token shapes (e.g. used_percent=10) that trip secretLike.
		RedactedDiagnostics: fmt.Sprintf(
			"antigravity codexbar usage window %s used %d percent remaining derived %d percent window minutes %d",
			name, used, remaining, w.WindowMinutes,
		),
		GapReasons:        gaps,
		TerminalErrorCode: terminal,
		CreatedAt:         formatTime(now),
		UpdatedAt:         formatTime(now),
		PolicyVersion:     PolicyVersion,
	})
	return snap, true
}
