package providerinventory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	grokNativeFederationSourceSchema = "grok.native_agent_capability.v1"
	grokNativeFederationProtocol     = "grok-native-agent.v1"
	grokNativeFederationTimeout      = 5 * time.Second
)

var grokNativeFederationRequiredControls = []string{
	"durable_child_identity_registration",
	"exact_parent_run_task_scope",
	"ownership_generation_lease_fencing",
	"accepted_budget_authority",
	"cancellation_acknowledgement",
	"terminal_state",
	"recovery_replay_signals",
}

type grokNativeFederationCapability struct {
	SchemaVersion         string         `json:"schema_version"`
	ProtocolVersion       string         `json:"protocol_version"`
	ExecutableFingerprint string         `json:"executable_fingerprint"`
	NativeSubagents       bool           `json:"native_subagents"`
	Controls              map[string]any `json:"controls"`
}

func inspectGrokNativeFederation(ctx context.Context, discovery *discoveryContext, adapter AdapterDeclaration, candidate candidate, installation ProviderInstallation, now time.Time, deps Deps) ProbeResult {
	installationID := installation.ProviderInstallationID
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "native-federation"
	probe.ProbeCommandID = "grok-agent-stdio-native-federation"
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.TimeoutMS = int(grokNativeFederationTimeout / time.Millisecond)
	probe.StdoutLimitBytes = StdoutLimitBytes
	probe.StderrLimitBytes = StderrLimitBytes
	probe.CombinedOutputLimitBytes = CombinedLimitBytes
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.NetworkDeclared = false
	probe.NetworkPermission = NetworkNotNeeded
	argv := []string{candidate.path, "agent", "stdio", "--help"}
	env := probeEnvironment(deps.Getenv)
	probe.Argv = redactArgv(argv)
	probe.EnvironmentKeys = environmentKeys(env)
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path)}
	probe.Evidence = EvidenceSummary{Kind: "bounded-grok-native-federation-capability", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	unsupported := func(reason, terminal string) ProbeResult {
		probe.Outcome = OutcomeInstalledUnusable
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = []string{reason}
		probe.TerminalErrorCode = terminal
		probe.StdoutSummary = "grok native federation unsupported: " + reason
		return probe
	}
	if installation.InstallationState != InstallationInstalled {
		probe.SideEffectClass = "not-run"
		return unsupported("installation-not-usable", firstNonEmpty(installation.TerminalErrorCode, "ErrInstallationNotUsable"))
	}

	result, err := sharedProbeResult(ctx, discovery, deps, ProbeExecution{
		Argv:               argv,
		Env:                env,
		Timeout:            grokNativeFederationTimeout,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	_, stdoutFindings := redactProviderOutputNoTruncate(result.Stdout)
	stderr, stderrFindings := redactProviderOutputBeforeTruncate(result.Stderr, 4096)
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	if err != nil || result.TimedOut || result.Killed {
		if result.TimedOut {
			return unsupported("native-federation-probe-timeout", "ErrGrokNativeFederationProbeTimeout")
		}
		return unsupported("native-federation-probe-failed", "ErrGrokNativeFederationProbeFailed")
	}
	if result.ExitCode != 0 {
		return unsupported("native-federation-probe-nonzero-exit", "ErrGrokNativeFederationProbeNonZeroExit")
	}
	if credentialMaterialLike(result.Stdout) || credentialMaterialLike(result.Stderr) {
		return unsupported("credential-material-redacted", "ErrGrokNativeFederationCredentialMaterial")
	}

	capability, gaps, terminal := parseGrokNativeFederationCapability(result.Stdout + "\n" + result.Stderr)
	if len(gaps) > 0 {
		probe.GapReasons = gaps
		probe.TerminalErrorCode = terminal
		probe.StdoutSummary = "grok native federation unsupported: " + strings.Join(gaps, ",")
		probe.Outcome = OutcomeInstalledUnusable
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.setParsedFields(map[string]string{
			"parser":               grokNativeFederationSourceSchema,
			"unsupported_reason":   strings.Join(gaps, ","),
			"provider_cli_version": installation.Version,
		})
		return probe
	}
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.StdoutSummary = "grok native federation supported by " + capability.ProtocolVersion
	probe.setParsedFields(map[string]string{
		"parser":                 grokNativeFederationSourceSchema,
		"protocol_version":       capability.ProtocolVersion,
		"provider_cli_version":   installation.Version,
		"executable_fingerprint": capability.ExecutableFingerprint,
		"required_controls":      strings.Join(grokNativeFederationRequiredControls, ","),
		"native_federation":      "true",
	})
	return probe
}

func parseGrokNativeFederationCapability(output string) (grokNativeFederationCapability, []string, string) {
	var decoded grokNativeFederationCapability
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &decoded); err != nil {
		return grokNativeFederationCapability{}, []string{"native-federation-machine-contract-missing"}, "ErrGrokNativeFederationContractMissing"
	}
	var gaps []string
	if decoded.SchemaVersion != grokNativeFederationSourceSchema {
		gaps = append(gaps, "schema-version-missing-or-unsupported")
	}
	if decoded.ProtocolVersion != grokNativeFederationProtocol {
		gaps = append(gaps, "protocol-version-missing-or-unsupported")
	}
	if !strings.HasPrefix(strings.TrimSpace(decoded.ExecutableFingerprint), "sha256:") {
		gaps = append(gaps, "executable-fingerprint-missing")
	}
	if !decoded.NativeSubagents {
		gaps = append(gaps, "native-subagents-not-advertised")
	}
	missing := missingGrokNativeControls(decoded.Controls)
	for _, control := range missing {
		gaps = append(gaps, "missing-control:"+control)
	}
	if len(gaps) > 0 {
		sort.Strings(gaps)
		return decoded, gaps, "ErrGrokNativeFederationUnsupported"
	}
	return decoded, nil, ""
}

func missingGrokNativeControls(controls map[string]any) []string {
	var missing []string
	for _, control := range grokNativeFederationRequiredControls {
		if !controlTruthy(controls[control]) {
			missing = append(missing, control)
		}
	}
	return missing
}

func controlTruthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "supported", "yes", "required":
			return true
		}
	case map[string]any:
		return controlTruthy(typed["supported"]) || controlTruthy(typed["available"])
	}
	return false
}
