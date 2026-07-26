package providerinventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/config"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	codexBarExecutableName = "codexbar"
	codexBarDisabledSchema = "codexbar.generic.disabled.v1"
	codexBarQuotaTimeout   = 10 * time.Second
	codexBarStdoutBytes    = 128 * 1024
	codexBarStderrBytes    = 16 * 1024
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

func inspectCodexBarQuota(ctx context.Context, cfg config.CodexBar, now time.Time, deps Deps) ([]QuotaTelemetrySource, []QuotaSnapshot, []ProbeResult) {
	providers := configuredCodexBarProviders(cfg)
	if len(providers) == 0 {
		return nil, nil, nil
	}
	// The generic protocol is not implemented by released CodexBar. Do not
	// discover or execute CodexBar for this disabled compatibility surface.
	// ctx is retained in the signature for API compatibility.
	_ = ctx
	bridge := codexBarBridge{}
	var sources []QuotaTelemetrySource
	var snapshots []QuotaSnapshot
	var probes []ProbeResult
	for _, provider := range providers {
		source := codexBarQuotaSource(provider.provider, provider.trustClass, now)
		probe := codexBarProbe(provider.provider, source, bridge, now, deps)
		unavailable := func(reason, terminal string) {
			source.GapReasons = dedupeStrings(append(source.GapReasons, reason, "not-collected"))
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
		// The historical generic `codexbar quota --schema ...` protocol is not
		// implemented by released CodexBar (including 0.45.2). Never launch or
		// advertise that fixture-only command. Provider-specific production
		// adapters may consume an evidenced `codexbar usage` schema instead.
		probe.SideEffectClass = "not-run"
		probe.setParsedFields(map[string]string{
			"provider":    provider.provider,
			"replacement": "provider-specific-codexbar-usage-parser-required",
		})
		unavailable("codexbar-generic-protocol-unsupported", "ErrCodexBarGenericProtocolUnsupported")
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
		SourceSchemaVersion: codexBarDisabledSchema,
		SupportedQuantities: []QuantityKind{QuantityInputTokens, QuantityOutputTokens, QuantityTotalTokens, QuantityRequests, QuantityProviderDefined},
		SupportedWindows:    []WindowKind{WindowFixedHour, WindowFixedDay, WindowFixedWeek, WindowRolling, WindowProviderDefined, WindowUnbounded, WindowUnknown},
		ScopeDimensions:     []string{"provider", "account", "model", "scope"},
		ConfidenceContract: map[string]Confidence{
			"limit_value":     ConfidenceUnknown,
			"used_value":      ConfidenceUnknown,
			"remaining_value": ConfidenceUnknown,
			"reset_at":        ConfidenceUnknown,
		},
		NetworkDeclared:        false,
		NetworkPermissionScope: "",
		Argv:                   []string{},
		EnvironmentKeys:        []string{},
		TimeoutMS:              0,
		OutputLimits:           OutputLimits{},
		ClassificationRules:    []string{"codexbar-third-party-bridge", "not-provider-official-cli", "generic-protocol-disabled", "provider-specific-usage-parser-required", "configured-trust-class-required", "no-process-launch", "no-credential-material", "no-raw-output-persistence", "trust_class=" + safeSourceToken(trustClass)},
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
	probe.ProbeCommandID = "codexbar-generic-bridge-disabled"
	probe.ProbeMethod = ProbeMethodConfigured
	probe.NetworkDeclared = false
	probe.NetworkPermission = NetworkNotNeeded
	probe.TimeoutMS = 0
	probe.StdoutLimitBytes = 0
	probe.StderrLimitBytes = 0
	probe.CombinedOutputLimitBytes = 0
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.Source = SourceDescriptor{Kind: "configuration", AdapterID: provider, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: string(DiscoveryExplicitConfig)}
	probe.Evidence = EvidenceSummary{Kind: "codexbar-generic-protocol-not-run", CommandBounded: false, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}
	probe.Argv = redactArgv(source.Argv)
	probe.EnvironmentKeys = []string{}
	return probe
}

func codexBarUnavailableSnapshot(source QuotaTelemetrySource, provider string, now time.Time, reason, terminal string, bridge codexBarBridge, trustClass string) QuotaSnapshot {
	diag := fmt.Sprintf("codexbar generic bridge unavailable due to %s trust %s", reason, safeSummary(trustClass))
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

func runCodexBarQuota(ctx context.Context, req CodexBarRequest) (CodexBarResult, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return CodexBarResult{ExitCode: -1}, errors.New("codexbar argv is empty")
	}
	budget := newOutputBudget(req.CombinedLimitBytes)
	stdout := newBoundedBuffer(req.StdoutLimitBytes, budget)
	stderr := newBoundedBuffer(req.StderrLimitBytes, budget)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...) // #nosec G204 -- argv is a discovered CodexBar path plus a fixed provider-specific machine-JSON command.
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
		out.Stderr += "[loopcoder] codexbar command output truncated"
	}
	return out, err
}

func codexBarEnv(getenv func(string) string) []string {
	return probeEnvironmentFromKeys(getenv, codexBarEnvKeys())
}

func codexBarEnvKeys() []string {
	// CodexBar 0.45.2 needs USER when its Claude provider resolves the current
	// macOS login/keychain context. Without it, the real machine-readable
	// `codexbar usage --provider claude --format json` command returns a typed
	// provider error even though the same account succeeds in the host
	// environment. USER is an account selector, not credential material; its
	// value is passed only to the bounded child and is never persisted. Keep
	// HOME and every token/cookie/key variable outside this allowlist.
	return []string{"PATH", "USER", "PATHEXT", "SystemRoot", "windir", "ComSpec", "TEMP", "TMP"}
}

func codexBarTrustClassAllowed(value string) bool {
	switch value {
	case "local-machine", "credential-delegated", "internal-protocol", "browser-session":
		return true
	default:
		return false
	}
}

func codexBarCommandFingerprint(path, version string) string {
	// Fingerprint only the discovered executable identity. Provider-specific
	// probes declare their actual argv and schema separately; never bind this
	// fingerprint to the removed generic quota-bridge protocol.
	sum := sha256.Sum256([]byte(strings.Join([]string{filepath.Base(path), hashHex(path), version}, "\x00")))
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

func codexBarSafeScopeToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) || emailPattern.MatchString(value) || strings.ContainsAny(value, "/ \t\r\n:=") {
		return false
	}
	return len(value) <= 80
}

func safeProviderQuantityName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) || strings.ContainsAny(value, " \t\r\n;&|`$<>/") {
		return "provider_defined"
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
