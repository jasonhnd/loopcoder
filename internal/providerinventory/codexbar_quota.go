package providerinventory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	codexBarExecutableName = "codexbar"
	codexBarQuotaSchema    = "codexbar.quota_bridge.v1"
	codexBarQuotaTimeout   = 10 * time.Second
	codexBarStdoutBytes    = 128 * 1024
	codexBarStderrBytes    = 16 * 1024
)

var (
	ErrCodexBarAbsent             = errors.New("ErrCodexBarAbsent")
	ErrCodexBarUnsupportedVersion = errors.New("ErrCodexBarUnsupportedVersion")
	ErrCodexBarSchema             = errors.New("ErrCodexBarSchema")
	ErrCodexBarMalformed          = errors.New("ErrCodexBarMalformedJSON")
	ErrCodexBarTimeout            = errors.New("ErrCodexBarTimeout")
	ErrCodexBarTruncated          = errors.New("ErrCodexBarOutputTruncated")
	ErrCodexBarTrustDenied        = errors.New("ErrCodexBarTrustDenied")
	ErrCodexBarPartialFailure     = errors.New("ErrCodexBarPartialProviderFailure")
)

type CodexBarRequest struct {
	Argv               []string
	Env                []string
	Cwd                string
	Timeout            time.Duration
	StdoutLimitBytes   int
	StderrLimitBytes   int
	CombinedLimitBytes int
}

type CodexBarResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Killed    bool
	Truncated bool
}

type codexBarProviderOpt struct {
	provider   string
	trustClass string
}

type codexBarBridge struct {
	path        string
	version     string
	versionOK   bool
	fingerprint string
}

type codexBarPayload struct {
	SchemaVersion   string                `json:"schema_version"`
	CodexBarVersion string                `json:"codexbar_version"`
	Provider        string                `json:"provider"`
	Source          codexBarSourcePayload `json:"source"`
	Snapshots       []codexBarSnapshot    `json:"snapshots"`
	Error           *codexBarErrorPayload `json:"error"`
}

type codexBarSourcePayload struct {
	TrustClass string `json:"trust_class"`
	Confidence string `json:"confidence"`
	Freshness  string `json:"freshness"`
}

type codexBarErrorPayload struct {
	Code string `json:"code"`
	Type string `json:"type"`
}

type codexBarSnapshot struct {
	Scope                codexBarScope     `json:"scope"`
	QuantityKind         string            `json:"quantity_kind"`
	ProviderQuantityName string            `json:"provider_quantity_name"`
	Unit                 string            `json:"unit"`
	Window               codexBarWindow    `json:"window"`
	LimitValue           *int64            `json:"limit_value"`
	UsedValue            *int64            `json:"used_value"`
	RemainingValue       *int64            `json:"remaining_value"`
	ReservedValue        *int64            `json:"reserved_value"`
	ValueScale           int               `json:"value_scale"`
	Confidence           string            `json:"confidence"`
	FieldConfidences     map[string]string `json:"field_confidences"`
	FreshnessState       string            `json:"freshness_state"`
	ValidUntil           string            `json:"valid_until"`
	StaleAfter           string            `json:"stale_after"`
	ResetAt              string            `json:"reset_at"`
	ResetSemantics       string            `json:"reset_semantics"`
	GapReasons           []string          `json:"gap_reasons"`
}

type codexBarScope struct {
	AccountProfileID  string `json:"account_profile_id"`
	ModelCapabilityID string `json:"model_capability_id"`
	ScopeKey          string `json:"scope_key"`
}

type codexBarWindow struct {
	Kind              string `json:"kind"`
	Start             string `json:"start"`
	End               string `json:"end"`
	RollingDurationMS int64  `json:"rolling_duration_ms"`
	ResetAt           string `json:"reset_at"`
	ResetSemantics    string `json:"reset_semantics"`
}

func inspectCodexBarQuota(ctx context.Context, cfg config.CodexBar, now time.Time, deps Deps) ([]QuotaTelemetrySource, []QuotaSnapshot, []ProbeResult) {
	providers := configuredCodexBarProviders(cfg)
	if len(providers) == 0 {
		return nil, nil, nil
	}
	bridge := discoverCodexBar(ctx, deps)
	var sources []QuotaTelemetrySource
	var snapshots []QuotaSnapshot
	var probes []ProbeResult
	for _, provider := range providers {
		source := codexBarQuotaSource(provider.provider, provider.trustClass, now)
		probe := codexBarProbe(provider.provider, source, bridge, now, deps)
		unavailable := func(reason, terminal string) {
			sources = append(sources, source)
			snapshots = append(snapshots, codexBarUnavailableSnapshot(source, provider.provider, now, reason, terminal, bridge, provider.trustClass))
			probe.Outcome = OutcomeProbeFailed
			probe.Confidence = ConfidenceUnavailable
			probe.FreshnessState = FreshnessNotApplicable
			probe.GapReasons = []string{reason}
			probe.TerminalErrorCode = terminal
			probes = append(probes, probe)
		}
		if provider.trustClass == "" || !codexBarTrustClassAllowed(provider.trustClass) {
			probe.SideEffectClass = "not-run"
			unavailable("codexbar-trust-denied", "ErrCodexBarTrustDenied")
			continue
		}
		if bridge.path == "" {
			probe.SideEffectClass = "not-run"
			unavailable("codexbar-not-installed", "ErrCodexBarAbsent")
			continue
		}
		if !bridge.versionOK {
			probe.SideEffectClass = "not-run"
			unavailable("codexbar-unsupported-version", "ErrCodexBarUnsupportedVersion")
			continue
		}
		argv := codexBarQuotaArgv(bridge.path, provider.provider)
		env := codexBarEnv(deps.Getenv)
		probe.Argv = redactArgv(argv)
		probe.EnvironmentKeys = environmentKeys(env)
		probe.WorkingDirectory = "neutral-temp"
		probe.setParsedFields(map[string]string{
			"schema_version":      codexBarQuotaSchema,
			"provider":            provider.provider,
			"trust_class_config":  provider.trustClass,
			"codexbar_version":    bridge.version,
			"command_fingerprint": bridge.fingerprint,
		})
		result, err := deps.RunCodexBar(ctx, CodexBarRequest{
			Argv:               argv,
			Env:                env,
			Cwd:                os.TempDir(),
			Timeout:            codexBarQuotaTimeout,
			StdoutLimitBytes:   codexBarStdoutBytes,
			StderrLimitBytes:   codexBarStderrBytes,
			CombinedLimitBytes: codexBarStdoutBytes + codexBarStderrBytes,
		})
		stdout, stdoutFindings := codexBarOutputSummary(result.Stdout)
		stderr, stderrFindings := redactProviderOutputBeforeTruncate(result.Stderr, 4096)
		probe.StdoutSummary = stdout
		probe.StderrSummary = stderr
		probe.SecretFindingCount = stdoutFindings + stderrFindings
		probe.TimedOut = result.TimedOut
		probe.Killed = result.Killed
		probe.ExitCode = &result.ExitCode
		if result.Truncated {
			unavailable("codexbar-output-truncated", "ErrCodexBarOutputTruncated")
			continue
		}
		if err != nil || result.TimedOut || result.Killed {
			if result.TimedOut {
				unavailable("codexbar-timeout", "ErrCodexBarTimeout")
				continue
			}
			unavailable("codexbar-execution-failed", "ErrCodexBarExecutionFailed")
			continue
		}
		if result.ExitCode != 0 {
			unavailable("codexbar-nonzero-exit", "ErrCodexBarNonZeroExit")
			continue
		}
		if codexBarCredentialMaterialLike(result.Stdout) || codexBarCredentialMaterialLike(result.Stderr) {
			unavailable("credential-material-redacted", "ErrQuotaCredentialMaterial")
			continue
		}
		parsed, err := decodeCodexBarPayload(result.Stdout, provider.provider, provider.trustClass, bridge.version)
		if err != nil {
			unavailable(codexBarReason(err), codexBarTerminal(err))
			continue
		}
		if parsed.Error != nil {
			unavailable("codexbar-provider-failed", "ErrCodexBarPartialProviderFailure")
			continue
		}
		out, err := snapshotsFromCodexBar(source, parsed, bridge, provider.trustClass, now)
		if err != nil {
			unavailable(codexBarReason(err), codexBarTerminal(err))
			continue
		}
		source.ConfidenceContract = codexBarConfidenceContract(out)
		sources = append(sources, source)
		snapshots = append(snapshots, out...)
		probe.Outcome = OutcomeInstalled
		probe.Confidence = confidenceForCodexBarSnapshots(out)
		probe.FreshnessState = FreshnessFresh
		probe.setParsedFields(map[string]string{
			"parser":              codexBarQuotaSchema,
			"provider":            provider.provider,
			"trust_class":         provider.trustClass,
			"codexbar_version":    bridge.version,
			"command_fingerprint": bridge.fingerprint,
			"snapshot_count":      strconv.Itoa(len(out)),
		})
		probes = append(probes, probe)
	}
	return sources, snapshots, probes
}

func configuredCodexBarProviders(cfg config.CodexBar) []codexBarProviderOpt {
	if !cfg.Enabled {
		return nil
	}
	out := make([]codexBarProviderOpt, 0, len(cfg.Providers))
	seen := map[string]bool{}
	for _, provider := range cfg.Providers {
		name := strings.TrimSpace(provider.Provider)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, codexBarProviderOpt{provider: name, trustClass: strings.TrimSpace(provider.TrustClass)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].provider < out[j].provider })
	return out
}

func discoverCodexBar(ctx context.Context, deps Deps) codexBarBridge {
	adapter := AdapterDeclaration{AdapterID: "codexbar", DisplayName: "CodexBar", ExecutableNames: []string{codexBarExecutableName}, VersionArgv: []string{"--version"}}
	candidates := discoverCandidates(adapter, deps)
	if len(candidates) == 0 {
		return codexBarBridge{}
	}
	candidate := candidates[0]
	version := ""
	versionOK := false
	result, err := deps.RunProbe(ctx, ProbeExecution{
		Argv:               []string{candidate.path, "--version"},
		Env:                codexBarEnv(deps.Getenv),
		Timeout:            InstallProbeTimeout,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	if err == nil && !result.TimedOut && !result.Killed && result.ExitCode == 0 {
		version = parseVersion(result.Stdout, result.Stderr)
		versionOK = strings.TrimSpace(version) != "" && !strings.Contains(strings.ToLower(version), "unsupported")
	}
	return codexBarBridge{path: candidate.path, version: version, versionOK: versionOK, fingerprint: codexBarCommandFingerprint(candidate.path, version)}
}

func codexBarQuotaSource(provider, trustClass string, now time.Time) QuotaTelemetrySource {
	now = now.UTC()
	// CodexBar is a third-party trusted bridge — never OfficialCLICommand and
	// never operator policy overlay (semantically different). Distinct source
	// kind carries trust_class / version / fingerprint in diagnostics.
	sourceKind := QuotaSourceTrustedThirdPartyBridge
	return normalizeQuotaTelemetrySource(QuotaTelemetrySource{
		AdapterID:           provider,
		SourceKind:          sourceKind,
		SourceKey:           "codexbar-third-party-bridge-" + safeSourceToken(trustClass),
		SourceSchemaVersion: codexBarQuotaSchema,
		SupportedQuantities: []QuantityKind{QuantityInputTokens, QuantityOutputTokens, QuantityTotalTokens, QuantityRequests, QuantityProviderDefined},
		SupportedWindows:    []WindowKind{WindowFixedHour, WindowFixedDay, WindowFixedWeek, WindowRolling, WindowProviderDefined, WindowUnbounded, WindowUnknown},
		ScopeDimensions:     []string{"provider", "account", "model", "scope"},
		ConfidenceContract: map[string]Confidence{
			"limit_value":     ConfidenceUnknown,
			"used_value":      ConfidenceUnknown,
			"remaining_value": ConfidenceUnknown,
			"reset_at":        ConfidenceUnknown,
		},
		NetworkDeclared:        true,
		NetworkPermissionScope: "provider:" + provider + "/action:quota-read/source:codexbar/trust:" + safeSourceToken(trustClass),
		Argv:                   []string{codexBarExecutableName, "quota", "--schema", codexBarQuotaSchema, "--provider", provider, "--json"},
		EnvironmentKeys:        codexBarEnvKeys(),
		TimeoutMS:              int(codexBarQuotaTimeout / time.Millisecond),
		OutputLimits:           OutputLimits{StdoutBytes: codexBarStdoutBytes, StderrBytes: codexBarStderrBytes, CombinedBytes: codexBarStdoutBytes + codexBarStderrBytes, DecodedBytes: codexBarStdoutBytes},
		ClassificationRules:    []string{"codexbar-third-party-bridge", "not-provider-official-cli", "codexbar-json-field-allowlist", "one-provider-per-command", "configured-trust-class-required", "redact-before-truncate", "no-credential-material", "no-raw-output-persistence", "trust_class=" + safeSourceToken(trustClass)},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
		GapReasons:             []string{},
	})
}

func codexBarProbe(provider string, source QuotaTelemetrySource, bridge codexBarBridge, now time.Time, deps Deps) ProbeResult {
	adapter := AdapterDeclaration{AdapterID: provider, DisplayName: provider}
	probe := baseProbe(adapter, now, deps)
	probe.ProbeKind = "quota"
	probe.ProbeCommandID = "codexbar-json-bridge"
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.NetworkDeclared = true
	probe.NetworkPermission = NetworkGranted
	probe.TimeoutMS = int(codexBarQuotaTimeout / time.Millisecond)
	probe.StdoutLimitBytes = codexBarStdoutBytes
	probe.StderrLimitBytes = codexBarStderrBytes
	probe.CombinedOutputLimitBytes = codexBarStdoutBytes + codexBarStderrBytes
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: provider, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: string(DiscoveryPath), ExecutableName: codexBarExecutableName}
	if bridge.path != "" {
		probe.Source.ExecutableName = filepath.Base(bridge.path)
	}
	probe.Evidence = EvidenceSummary{Kind: "bounded-codexbar-versioned-json", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}
	probe.Argv = redactArgv(source.Argv)
	probe.EnvironmentKeys = codexBarEnvKeys()
	return probe
}

func codexBarQuotaArgv(path, provider string) []string {
	return []string{path, "quota", "--schema", codexBarQuotaSchema, "--provider", provider, "--json"}
}

func codexBarUnavailableSnapshot(source QuotaTelemetrySource, provider string, now time.Time, reason, terminal string, bridge codexBarBridge, trustClass string) QuotaSnapshot {
	diag := fmt.Sprintf("codexbar quota bridge unavailable due to %s schema %s trust %s", reason, codexBarQuotaSchema, safeSummary(trustClass))
	if bridge.version != "" {
		diag += " version " + safeSummary(bridge.version)
	}
	if bridge.fingerprint != "" {
		diag += " command " + bridge.fingerprint
	}
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:      quotaSnapshotID(provider, source.QuotaSourceID, reason, formatTime(now)),
		QuotaSourceID:        source.QuotaSourceID,
		SourceKind:           source.SourceKind,
		AdapterID:            provider,
		ScopeKey:             "provider:" + provider,
		QuantityKind:         QuantityProviderDefined,
		ProviderQuantityName: "quota",
		Unit:                 "provider-defined",
		WindowKind:           WindowUnknown,
		ResetSemantics:       ResetUnknown,
		ValueScale:           0,
		Confidence:           ConfidenceUnavailable,
		FieldConfidences:     map[string]Confidence{"limit_value": ConfidenceUnavailable, "used_value": ConfidenceUnavailable, "remaining_value": ConfidenceUnavailable, "reset_at": ConfidenceUnavailable},
		FreshnessState:       FreshnessNotApplicable,
		CapturedAt:           formatTime(now),
		RedactedDiagnostics:  diag,
		ConflictSet:          []string{},
		GapReasons:           []string{reason, "not-collected"},
		TerminalErrorCode:    terminal,
		CreatedAt:            formatTime(now),
		UpdatedAt:            formatTime(now),
		PolicyVersion:        PolicyVersion,
	})
}

func decodeCodexBarPayload(output, provider, trustClass, version string) (codexBarPayload, error) {
	var payload codexBarPayload
	if len(output) > codexBarStdoutBytes {
		return payload, fmt.Errorf("%w: output exceeded limit", ErrCodexBarMalformed)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(output)))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return payload, fmt.Errorf("%w: %v", ErrCodexBarMalformed, err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return payload, fmt.Errorf("%w: multiple JSON values", ErrCodexBarMalformed)
	}
	if payload.SchemaVersion != codexBarQuotaSchema {
		return payload, fmt.Errorf("%w: schema_version", ErrCodexBarSchema)
	}
	if payload.Provider != provider {
		return payload, fmt.Errorf("%w: provider mismatch", ErrCodexBarMalformed)
	}
	if payload.Source.TrustClass != trustClass || !codexBarTrustClassAllowed(payload.Source.TrustClass) {
		return payload, fmt.Errorf("%w: trust_class", ErrCodexBarTrustDenied)
	}
	if payload.CodexBarVersion != "" && version != "" && payload.CodexBarVersion != version {
		return payload, fmt.Errorf("%w: version mismatch", ErrCodexBarUnsupportedVersion)
	}
	if payload.Error != nil {
		if !safeErrorCode(firstNonEmpty(payload.Error.Code, payload.Error.Type)) {
			return payload, fmt.Errorf("%w: error code", ErrCodexBarMalformed)
		}
		return payload, nil
	}
	if len(payload.Snapshots) == 0 {
		return payload, fmt.Errorf("%w: no snapshots", ErrCodexBarMalformed)
	}
	return payload, nil
}

func snapshotsFromCodexBar(source QuotaTelemetrySource, payload codexBarPayload, bridge codexBarBridge, trustClass string, now time.Time) ([]QuotaSnapshot, error) {
	out := make([]QuotaSnapshot, 0, len(payload.Snapshots))
	for index, snapshot := range payload.Snapshots {
		quantity, ok := codexBarQuantity(snapshot.QuantityKind)
		if !ok {
			return nil, fmt.Errorf("%w: quantity_kind", ErrCodexBarMalformed)
		}
		window, ok := codexBarWindowKind(firstNonEmpty(snapshot.Window.Kind, "unknown"))
		if !ok {
			return nil, fmt.Errorf("%w: window_kind", ErrCodexBarMalformed)
		}
		if err := validateCodexBarSnapshotValues(snapshot, window); err != nil {
			return nil, err
		}
		confidence, ok := codexBarConfidence(firstNonEmpty(snapshot.Confidence, payload.Source.Confidence))
		if !ok {
			return nil, fmt.Errorf("%w: confidence", ErrCodexBarMalformed)
		}
		// Third-party bridge must never claim exact: cap to estimated under OperatorOverlay.
		confidence = capConfidenceForSource(source.SourceKind, confidence)
		freshness, ok := codexBarFreshness(firstNonEmpty(snapshot.FreshnessState, payload.Source.Freshness))
		if !ok {
			return nil, fmt.Errorf("%w: freshness", ErrCodexBarMalformed)
		}
		reset := firstNonEmpty(snapshot.ResetAt, snapshot.Window.ResetAt)
		resetSemantics, ok := codexBarResetSemantics(firstNonEmpty(snapshot.ResetSemantics, snapshot.Window.ResetSemantics, "unknown"))
		if !ok {
			return nil, fmt.Errorf("%w: reset_semantics", ErrCodexBarMalformed)
		}
		if reset != "" {
			var err error
			reset, err = codexBarTime(reset)
			if err != nil {
				return nil, fmt.Errorf("%w: reset_at", ErrCodexBarMalformed)
			}
		}
		windowStart, err := codexBarOptionalTime(snapshot.Window.Start)
		if err != nil {
			return nil, fmt.Errorf("%w: window_start", ErrCodexBarMalformed)
		}
		windowEnd, err := codexBarOptionalTime(snapshot.Window.End)
		if err != nil {
			return nil, fmt.Errorf("%w: window_end", ErrCodexBarMalformed)
		}
		validUntil, err := codexBarOptionalTime(snapshot.ValidUntil)
		if err != nil {
			return nil, fmt.Errorf("%w: valid_until", ErrCodexBarMalformed)
		}
		staleAfter, err := codexBarOptionalTime(snapshot.StaleAfter)
		if err != nil {
			return nil, fmt.Errorf("%w: stale_after", ErrCodexBarMalformed)
		}
		fieldConfidences, err := codexBarFieldConfidences(snapshot.FieldConfidences, confidence)
		if err != nil {
			return nil, err
		}
		for field, fc := range fieldConfidences {
			fieldConfidences[field] = capConfidenceForSource(source.SourceKind, fc)
		}
		scope := codexBarScopeKey(payload.Provider, snapshot.Scope, index)
		diag := fmt.Sprintf("codexbar quota bridge schema %s version %s trust %s command %s", codexBarQuotaSchema, safeSummary(firstNonEmpty(payload.CodexBarVersion, bridge.version)), safeSummary(trustClass), bridge.fingerprint)
		out = append(out, normalizeQuotaSnapshot(QuotaSnapshot{
			QuotaSnapshotID:      quotaSnapshotID(payload.Provider, source.QuotaSourceID, scope, firstNonEmpty(snapshot.ProviderQuantityName, string(quantity)), string(window), formatTime(now), strconv.Itoa(index)),
			QuotaSourceID:        source.QuotaSourceID,
			SourceKind:           source.SourceKind,
			AdapterID:            payload.Provider,
			AccountProfileID:     codexBarSafePtr(snapshot.Scope.AccountProfileID),
			ModelCapabilityID:    codexBarSafePtr(snapshot.Scope.ModelCapabilityID),
			ScopeKey:             scope,
			QuantityKind:         quantity,
			ProviderQuantityName: safeProviderQuantityName(firstNonEmpty(snapshot.ProviderQuantityName, string(quantity))),
			Unit:                 firstNonEmpty(safeUnit(snapshot.Unit), unitForQuantity(quantity, snapshot.ProviderQuantityName)),
			WindowKind:           window,
			WindowStart:          windowStart,
			WindowEnd:            windowEnd,
			RollingDurationMS:    snapshot.Window.RollingDurationMS,
			ResetAt:              reset,
			ResetSemantics:       resetSemantics,
			LimitValue:           snapshot.LimitValue,
			UsedValue:            snapshot.UsedValue,
			RemainingValue:       snapshot.RemainingValue,
			ReservedValue:        snapshot.ReservedValue,
			ValueScale:           snapshot.ValueScale,
			Confidence:           confidence,
			FieldConfidences:     fieldConfidences,
			FreshnessState:       freshness,
			CapturedAt:           formatTime(now),
			ValidUntil:           validUntil,
			StaleAfter:           staleAfter,
			RedactedDiagnostics:  diag,
			ConflictSet:          []string{},
			GapReasons:           codexBarGapReasons(snapshot.GapReasons),
			CreatedAt:            formatTime(now),
			UpdatedAt:            formatTime(now),
			PolicyVersion:        PolicyVersion,
		}))
	}
	return out, nil
}

func validateCodexBarSnapshotValues(snapshot codexBarSnapshot, window WindowKind) error {
	if snapshot.ValueScale < 0 {
		return fmt.Errorf("%w: value_scale", ErrCodexBarMalformed)
	}
	for field, value := range map[string]*int64{
		"limit_value":     snapshot.LimitValue,
		"used_value":      snapshot.UsedValue,
		"remaining_value": snapshot.RemainingValue,
		"reserved_value":  snapshot.ReservedValue,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%w: %s", ErrCodexBarMalformed, field)
		}
	}
	if snapshot.Window.RollingDurationMS < 0 || (window == WindowRolling && snapshot.Window.RollingDurationMS == 0) {
		return fmt.Errorf("%w: rolling_duration_ms", ErrCodexBarMalformed)
	}
	return nil
}

func runCodexBarQuota(ctx context.Context, req CodexBarRequest) (CodexBarResult, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return CodexBarResult{ExitCode: -1}, errors.New("codexbar argv is empty")
	}
	budget := newOutputBudget(req.CombinedLimitBytes)
	stdout := newBoundedBuffer(req.StdoutLimitBytes, budget)
	stderr := newBoundedBuffer(req.StderrLimitBytes, budget)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...) // #nosec G204 -- argv is the discovered CodexBar CLI path and fixed quota JSON subcommand.
	cmd.Dir = firstNonEmpty(req.Cwd, os.TempDir())
	cmd.Env = append([]string{}, req.Env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: req.Timeout, LivenessMode: supervisedexec.LivenessModeLogOnly, Role: "codexbar-quota"})
	exitCode := result.ExitCode
	if (err != nil || result.Outcome == supervisedexec.OutcomeDeadline || result.Killed) && exitCode == 0 {
		exitCode = -1
	}
	out := CodexBarResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  exitCode,
		TimedOut:  result.Outcome == supervisedexec.OutcomeDeadline,
		Killed:    result.Killed,
		Truncated: stdout.Truncated() || stderr.Truncated(),
	}
	if out.Truncated {
		if out.Stderr != "" {
			out.Stderr += "\n"
		}
		out.Stderr += "[loopcoder] codexbar quota output truncated"
	}
	return out, err
}

func codexBarEnv(getenv func(string) string) []string {
	return probeEnvironmentFromKeys(getenv, codexBarEnvKeys())
}

func codexBarEnvKeys() []string {
	return []string{"PATH", "PATHEXT", "SystemRoot", "windir", "ComSpec", "TEMP", "TMP"}
}

func codexBarTrustClassAllowed(value string) bool {
	switch value {
	case "local-machine", "credential-delegated", "internal-protocol", "browser-session":
		return true
	default:
		return false
	}
}

func codexBarOutputSummary(output string) (string, int) {
	if output == "" {
		return "", 0
	}
	if codexBarCredentialMaterialLike(output) {
		return "[loopcoder] codexbar JSON contained credential-shaped material and was refused", 1
	}
	return "[loopcoder] codexbar JSON accepted as hash-only normalized quota fields", 0
}

func codexBarCommandFingerprint(path, version string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{filepath.Base(path), hashHex(path), version, codexBarQuotaSchema}, "\x00")))
	return "cmdfp_" + hex.EncodeToString(sum[:])
}

func codexBarCredentialMaterialLike(value string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	lower := strings.ToLower(value)
	for _, key := range []string{"credential_path", "credentialpath", "auth_header", "authorization", "cookie", "set-cookie", "access_token", "refresh_token", "api_key", "secret_key"} {
		if strings.Contains(lower, key) {
			return true
		}
	}
	return false
}

func codexBarQuantity(value string) (QuantityKind, bool) {
	switch QuantityKind(value) {
	case QuantityInputTokens, QuantityOutputTokens, QuantityTotalTokens, QuantityRequests, QuantityProviderDefined:
		return QuantityKind(value), true
	default:
		return "", false
	}
}

func codexBarWindowKind(value string) (WindowKind, bool) {
	if value == "" {
		value = string(WindowUnknown)
	}
	kind := WindowKind(value)
	return kind, knownWindowKind(kind)
}

func codexBarConfidence(value string) (Confidence, bool) {
	switch Confidence(value) {
	case ConfidenceExact, ConfidenceEstimated, ConfidenceUnknown, ConfidenceUnavailable:
		return Confidence(value), true
	default:
		return "", false
	}
}

func codexBarFreshness(value string) (FreshnessState, bool) {
	switch FreshnessState(value) {
	case FreshnessFresh, FreshnessStale, FreshnessExpired, FreshnessNotApplicable:
		return FreshnessState(value), true
	default:
		return "", false
	}
}

func codexBarResetSemantics(value string) (ResetSemantics, bool) {
	kind := ResetSemantics(value)
	switch kind {
	case ResetNone, ResetProviderDefined, ResetWindowBoundary, ResetRolling, ResetUnknown:
		return kind, true
	default:
		return "", false
	}
}

func codexBarOptionalTime(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	return codexBarTime(value)
}

func codexBarTime(value string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return formatTime(t), nil
}

func codexBarFieldConfidences(values map[string]string, fallback Confidence) (map[string]Confidence, error) {
	out := map[string]Confidence{}
	for _, field := range []string{"limit_value", "used_value", "remaining_value", "reset_at"} {
		value := strings.TrimSpace(values[field])
		if value == "" {
			out[field] = fallback
			continue
		}
		confidence, ok := codexBarConfidence(value)
		if !ok {
			return nil, fmt.Errorf("%w: field confidence", ErrCodexBarMalformed)
		}
		out[field] = confidence
	}
	return out, nil
}

func codexBarScopeKey(provider string, scope codexBarScope, index int) string {
	if safeScopeKey(scope.ScopeKey) {
		return scope.ScopeKey
	}
	parts := []string{"provider:" + provider}
	if codexBarSafeScopeToken(scope.AccountProfileID) {
		parts = append(parts, "account:"+scope.AccountProfileID)
	}
	if codexBarSafeScopeToken(scope.ModelCapabilityID) {
		parts = append(parts, "model:"+scope.ModelCapabilityID)
	}
	if len(parts) == 1 {
		parts = append(parts, "scope:"+strconv.Itoa(index))
	}
	return strings.Join(parts, "/")
}

func codexBarSafePtr(value string) *string {
	value = strings.TrimSpace(value)
	if !codexBarSafeScopeToken(value) {
		return nil
	}
	return &value
}

func codexBarSafeScopeToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) || emailPattern.MatchString(value) || strings.ContainsAny(value, "/ \t\r\n:=") {
		return false
	}
	return len(value) <= 80
}

func safeScopeKey(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "provider:") && !secretLike(value) && !emailPattern.MatchString(value) && !strings.ContainsAny(value, " \t\r\n;&|`$<>")
}

func safeProviderQuantityName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) || strings.ContainsAny(value, " \t\r\n;&|`$<>/") {
		return "provider_defined"
	}
	return boundedToken(value, 80)
}

func safeUnit(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) || strings.ContainsAny(value, " \t\r\n;&|`$<>/") {
		return ""
	}
	return boundedToken(value, 80)
}

func safeSourceToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) || strings.ContainsAny(value, " \t\r\n;&|`$<>/") {
		return "unknown"
	}
	return value
}

func safeErrorCode(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && !secretLike(value) && !strings.ContainsAny(value, " \t\r\n;&|`$<>/.:=")
}

func codexBarGapReasons(values []string) []string {
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || secretLike(value) || strings.ContainsAny(value, " \t\r\n;&|`$<>/.:=") {
			continue
		}
		out = append(out, value)
	}
	return dedupeStrings(out)
}

func codexBarReason(err error) string {
	switch {
	case errors.Is(err, ErrQuotaCredentialMaterial):
		return "credential-material-redacted"
	case errors.Is(err, ErrCodexBarTrustDenied):
		return "codexbar-trust-denied"
	case errors.Is(err, ErrCodexBarUnsupportedVersion):
		return "codexbar-unsupported-version"
	case errors.Is(err, ErrCodexBarSchema):
		return "codexbar-schema-unsupported"
	case errors.Is(err, ErrCodexBarTruncated):
		return "codexbar-output-truncated"
	case errors.Is(err, ErrCodexBarTimeout):
		return "codexbar-timeout"
	case errors.Is(err, ErrCodexBarPartialFailure):
		return "codexbar-provider-failed"
	default:
		return "codexbar-malformed-json"
	}
}

func codexBarTerminal(err error) string {
	switch {
	case errors.Is(err, ErrQuotaCredentialMaterial):
		return "ErrQuotaCredentialMaterial"
	case errors.Is(err, ErrCodexBarTrustDenied):
		return "ErrCodexBarTrustDenied"
	case errors.Is(err, ErrCodexBarUnsupportedVersion):
		return "ErrCodexBarUnsupportedVersion"
	case errors.Is(err, ErrCodexBarSchema):
		return "ErrCodexBarSchemaUnsupported"
	case errors.Is(err, ErrCodexBarTruncated):
		return "ErrCodexBarOutputTruncated"
	case errors.Is(err, ErrCodexBarTimeout):
		return "ErrCodexBarTimeout"
	case errors.Is(err, ErrCodexBarPartialFailure):
		return "ErrCodexBarPartialProviderFailure"
	default:
		return "ErrCodexBarMalformedJSON"
	}
}

func codexBarConfidenceContract(snapshots []QuotaSnapshot) map[string]Confidence {
	// Max contract for third-party bridge is estimated (never exact).
	out := map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceUnknown, "remaining_value": ConfidenceUnknown, "reset_at": ConfidenceUnknown}
	for _, snapshot := range snapshots {
		for field, confidence := range snapshot.FieldConfidences {
			confidence = capConfidenceForSource(QuotaSourceTrustedThirdPartyBridge, confidence)
			if codexBarConfidenceRank(confidence) > codexBarConfidenceRank(out[field]) {
				out[field] = confidence
			}
		}
	}
	return out
}

func codexBarConfidenceRank(confidence Confidence) int {
	switch confidence {
	case ConfidenceExact:
		return 4
	case ConfidenceEstimated:
		return 3
	case ConfidenceUnknown:
		return 2
	default:
		return 1
	}
}

func confidenceForCodexBarSnapshots(snapshots []QuotaSnapshot) Confidence {
	// Aggregate starts at estimated: third-party bridge never claims exact.
	confidence := ConfidenceEstimated
	for _, snapshot := range snapshots {
		if codexBarConfidenceRank(snapshot.Confidence) < codexBarConfidenceRank(confidence) {
			confidence = snapshot.Confidence
		}
	}
	return confidence
}
