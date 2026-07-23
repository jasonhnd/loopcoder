package providerinventory

import (
	"bufio"
	"bytes"
	"context"
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

	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	grokACPBillingSourceSchema = "grok.acp.billing.v1"
	grokACPBillingTimeout      = 10 * time.Second
	grokACPOutputBytes         = 128 * 1024
	grokACPLineBytes           = 64 * 1024
)

var (
	ErrGrokACPBillingUnsupported = errors.New("ErrGrokACPBillingUnsupported")
	ErrGrokACPBillingMalformed   = errors.New("ErrGrokACPBillingMalformed")
	ErrGrokACPBillingRPC         = errors.New("ErrGrokACPBillingRPC")
	ErrGrokACPBillingTimeout     = errors.New("ErrGrokACPBillingTimeout")
)

type GrokACPBillingRequest struct {
	Argv               []string
	Env                []string
	Timeout            time.Duration
	StdoutLimitBytes   int
	StderrLimitBytes   int
	CombinedLimitBytes int
}

type GrokACPBillingResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Killed   bool
}

type grokACPJSONRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

func inspectGrokACPBilling(ctx context.Context, discovery *discoveryContext, adapter AdapterDeclaration, candidate candidate, installation ProviderInstallation, now time.Time, deps Deps) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
	// Primary: official CLI-owned credits billing HTTP endpoint. Current Grok
	// Build CLI does not advertise ACP billing/read; the CLI-owned
	// /v1/billing?format=credits path is the truthful observation source.
	if source, snaps, probe, err := collectGrokCreditsBilling(ctx, discovery, adapter, installation, now, deps); err == nil && len(snaps) > 0 {
		return source, snaps, probe
	} else if err != nil {
		// Remember credits failure for final gap when ACP also unavailable.
		// Network grant denials short-circuit without ACP attempt.
		if probe.NetworkPermission != NetworkGranted || containsString(probe.GapReasons, "quota-collection-not-granted") {
			return source, snaps, probe
		}
		// Fall through to ACP only when credits failed for non-grant reasons.
		_ = err
	}

	source := grokACPBillingSource(now)
	installationID := installation.ProviderInstallationID
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "quota"
	probe.ProbeCommandID = "grok-acp-billing"
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.TimeoutMS = int(grokACPBillingTimeout / time.Millisecond)
	probe.StdoutLimitBytes = grokACPOutputBytes
	probe.StderrLimitBytes = StdoutLimitBytes
	probe.CombinedOutputLimitBytes = grokACPOutputBytes + StdoutLimitBytes
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.NetworkDeclared = true
	probe.NetworkPermission = grokQuotaNetworkPermissionFor(discovery, adapter)
	argv := []string{candidate.path, "--no-auto-update", "agent", "stdio"}
	env := probeEnvironment(deps.Getenv)
	probe.Argv = redactArgv(argv)
	probe.EnvironmentKeys = environmentKeys(env)
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path)}
	probe.Evidence = EvidenceSummary{Kind: "bounded-grok-acp-json-rpc-billing", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	unavailable := func(reason, terminal string) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
		snapshot := grokACPUnavailableSnapshot(source, &installationID, now, reason, terminal)
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = []string{reason}
		probe.TerminalErrorCode = terminal
		return source, []QuotaSnapshot{snapshot}, probe
	}

	if probe.NetworkPermission != NetworkGranted {
		probe.SideEffectClass = "not-run"
		return unavailable("quota-collection-not-granted", "ErrQuotaCollectionGrantRequired")
	}
	if !grokACPBillingCapabilityAdvertised(ctx, discovery, candidate, env, deps) {
		// ACP unavailable (current CLI). Prefer credits-auth-missing when that
		// was the primary path failure; otherwise surface both gaps.
		probe.SideEffectClass = "local-read"
		// Re-attempt credits only for reason reporting is unnecessary; surface
		// billing-capability-not-advertised and note credits path when auth missing.
		home, _ := deps.UserHomeDir()
		if h := strings.TrimSpace(deps.Getenv("HOME")); h != "" {
			home = h
		}
		if _, _, aerr := loadGrokCLIAuthToken(home, deps.Getenv); aerr != nil {
			return unavailable("credits-auth-missing", "ErrGrokCreditsAuthMissing")
		}
		// Auth present but credits collect failed earlier and ACP not advertised.
		return unavailable("billing-capability-not-advertised", "ErrQuotaSourceUnsupported")
	}

	result, err := deps.RunGrokACP(ctx, GrokACPBillingRequest{
		Argv:               argv,
		Env:                env,
		Timeout:            grokACPBillingTimeout,
		StdoutLimitBytes:   grokACPOutputBytes,
		StderrLimitBytes:   StdoutLimitBytes,
		CombinedLimitBytes: grokACPOutputBytes + StdoutLimitBytes,
	})
	_, stdoutFindings := redactProviderOutputNoTruncate(result.Stdout)
	stderr, stderrFindings := redactProviderOutputBeforeTruncate(result.Stderr, 4096)
	probe.StdoutSummary = grokACPProtocolSummary(result.Stdout)
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	if err != nil && grokACPBillingProtocolError(err) {
		return unavailable(grokACPBillingReason(err), grokACPBillingTerminal(err))
	}
	if err != nil || result.TimedOut || result.Killed {
		if result.TimedOut {
			return unavailable("quota-probe-timeout", "ErrGrokACPBillingTimeout")
		}
		return unavailable("quota-probe-failed", "ErrGrokACPBillingExecutionFailed")
	}
	if result.ExitCode != 0 {
		return unavailable("quota-probe-nonzero-exit", "ErrGrokACPBillingNonZeroExit")
	}
	if credentialMaterialLike(result.Stdout) || credentialMaterialLike(result.Stderr) {
		return unavailable("credential-material-redacted", "ErrQuotaCredentialMaterial")
	}

	billing, frames, err := decodeGrokACPBilling(result.Stdout)
	if err != nil {
		return unavailable(grokACPBillingReason(err), grokACPBillingTerminal(err))
	}
	snapshots, err := snapshotsFromGrokACPBilling(source, &installationID, installation.Version, billing, frames, now)
	if err != nil {
		return unavailable(grokACPBillingReason(err), grokACPBillingTerminal(err))
	}
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.setParsedFields(map[string]string{
		"parser":         grokACPBillingSourceSchema,
		"snapshot_count": strconv.Itoa(len(snapshots)),
	})
	return source, snapshots, probe
}

func grokACPBillingSource(now time.Time) QuotaTelemetrySource {
	now = now.UTC()
	return normalizeQuotaTelemetrySource(QuotaTelemetrySource{
		AdapterID:              "grok",
		SourceKind:             QuotaSourceOfficialCLICommand,
		SourceKey:              "grok-acp-billing-v1",
		SourceSchemaVersion:    grokACPBillingSourceSchema,
		SupportedQuantities:    []QuantityKind{QuantityInputTokens, QuantityOutputTokens, QuantityTotalTokens, QuantityRequests, QuantityProviderDefined},
		SupportedWindows:       []WindowKind{WindowFixedWeek, WindowRolling, WindowProviderDefined, WindowUnbounded, WindowUnknown},
		ScopeDimensions:        []string{"provider", "installation", "account", "model", "authority"},
		ConfidenceContract:     map[string]Confidence{"limit_value": ConfidenceExact, "used_value": ConfidenceExact, "remaining_value": ConfidenceExact, "reset_at": ConfidenceExact},
		NetworkDeclared:        true,
		NetworkPermissionScope: "provider:grok/action:quota-read/side-effect:read/freshness:interactive",
		Argv:                   []string{"grok", "--no-auto-update", "agent", "stdio"},
		EnvironmentKeys:        allowedProbeEnvKeys(),
		TimeoutMS:              int(grokACPBillingTimeout / time.Millisecond),
		OutputLimits:           OutputLimits{StdoutBytes: grokACPOutputBytes, StderrBytes: StdoutLimitBytes, CombinedBytes: grokACPOutputBytes + StdoutLimitBytes, DecodedBytes: grokACPOutputBytes},
		ClassificationRules:    []string{"json-rpc-field-allowlist", "redact-before-truncate", "no-credential-material", "no-login-update-or-provider-work"},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
		GapReasons:             []string{},
	})
}

func grokQuotaNetworkPermissionFor(discovery *discoveryContext, adapter AdapterDeclaration) NetworkPermission {
	if discovery == nil {
		return NetworkDenied
	}
	exact := NetworkGrant{ProviderID: adapter.AdapterID, Purpose: NetworkPurposeQuotaTelemetry, Scope: NetworkScopeMachineInventory}
	if discovery.networkGrants[exact] {
		return NetworkGranted
	}
	return NetworkDenied
}

func grokACPUnavailableSnapshot(source QuotaTelemetrySource, installationID *string, now time.Time, reason, terminal string) QuotaSnapshot {
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, reason, formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "grok",
		ProviderInstallationID: installationID,
		ScopeKey:               "provider:grok",
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   "billing",
		Unit:                   "provider-defined",
		WindowKind:             WindowUnknown,
		ResetSemantics:         ResetUnknown,
		ValueScale:             0,
		Confidence:             ConfidenceUnavailable,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnavailable, "used_value": ConfidenceUnavailable, "remaining_value": ConfidenceUnavailable, "reset_at": ConfidenceUnavailable},
		FreshnessState:         FreshnessNotApplicable,
		CapturedAt:             formatTime(now),
		RedactedDiagnostics:    "grok ACP billing unavailable due to " + reason,
		ConflictSet:            []string{},
		GapReasons:             []string{reason, "not-collected"},
		TerminalErrorCode:      terminal,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func grokACPBillingCapabilityAdvertised(ctx context.Context, discovery *discoveryContext, candidate candidate, env []string, deps Deps) bool {
	result, err := sharedProbeResult(ctx, discovery, deps, ProbeExecution{
		Argv:               []string{candidate.path, "agent", "stdio", "--help"},
		Env:                env,
		Timeout:            5 * time.Second,
		StdoutLimitBytes:   StdoutLimitBytes,
		StderrLimitBytes:   StderrLimitBytes,
		CombinedLimitBytes: CombinedLimitBytes,
	})
	if err != nil || result.ExitCode != 0 || result.TimedOut || result.Killed {
		return false
	}
	help := strings.ToLower(result.Stdout + "\n" + result.Stderr)
	return strings.Contains(help, "billing") && strings.Contains(help, "agent") && strings.Contains(help, "stdio")
}

func runGrokACPBilling(ctx context.Context, req GrokACPBillingRequest) (GrokACPBillingResult, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return GrokACPBillingResult{ExitCode: -1}, errors.New("grok ACP argv is empty")
	}
	budget := newOutputBudget(req.CombinedLimitBytes)
	stdout := newBoundedBuffer(req.StdoutLimitBytes, budget)
	stderr := newBoundedBuffer(req.StderrLimitBytes, budget)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...) // #nosec G204 -- argv is the discovered Grok CLI path and fixed ACP billing subcommand.
	cmd.Dir = os.TempDir()
	cmd.Env = append([]string{}, req.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return GrokACPBillingResult{ExitCode: -1}, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return GrokACPBillingResult{ExitCode: -1}, err
	}
	cmd.Stderr = stderr
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runCh := make(chan struct {
		result supervisedexec.Result
		err    error
	}, 1)
	go func() {
		result, err := supervisedexec.Run(runCtx, cmd, supervisedexec.Options{HardCap: req.Timeout, LivenessMode: supervisedexec.LivenessModeLogOnly, Role: "grok-acp-billing"})
		runCh <- struct {
			result supervisedexec.Result
			err    error
		}{result: result, err: err}
	}()
	protocolErr := driveGrokACPBillingProtocol(runCtx, stdin, stdoutPipe, stdout)
	_ = stdin.Close()
	if protocolErr != nil {
		cancel()
	}
	run := <-runCh
	result := run.result
	err = run.err
	exitCode := result.ExitCode
	if (err != nil || result.Outcome == supervisedexec.OutcomeDeadline || result.Killed) && exitCode == 0 {
		exitCode = -1
	}
	out := GrokACPBillingResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: exitCode,
		TimedOut: result.Outcome == supervisedexec.OutcomeDeadline,
		Killed:   result.Killed,
	}
	if stdout.Truncated() || stderr.Truncated() {
		if out.Stderr != "" {
			out.Stderr += "\n"
		}
		out.Stderr += "[loopcoder] grok ACP billing output truncated"
	}
	if protocolErr != nil && (exitCode == 0 || result.Killed) {
		err = protocolErr
	}
	return out, err
}

func driveGrokACPBillingProtocol(ctx context.Context, stdin io.Writer, stdout io.Reader, retained *boundedBuffer) error {
	events := scanGrokACPStdout(ctx, stdout, retained)
	if err := writeGrokACPJSONL(stdin, grokACPJSONRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}}); err != nil {
		return err
	}
	initialized := false
	for {
		select {
		case event := <-events:
			if event.err != nil {
				if errors.Is(event.err, io.EOF) {
					return fmt.Errorf("%w: eof before billing response", ErrGrokACPBillingMalformed)
				}
				return event.err
			}
			msg, err := decodeGrokACPJSONLMessage(event.line)
			if err != nil {
				return err
			}
			if msg.Method != "" && jsonRPCID(msg.ID) == "" {
				continue
			}
			if msg.Error != nil {
				return grokACPRPCError(msg.Error)
			}
			switch jsonRPCID(msg.ID) {
			case "1":
				if !grokACPInitializeAdvertisesBilling(msg.Result) {
					return fmt.Errorf("%w: initialize did not advertise billing", ErrGrokACPBillingUnsupported)
				}
				initialized = true
				if err := writeGrokACPJSONL(stdin, grokACPJSONRPCRequest{JSONRPC: "2.0", ID: 2, Method: "billing/read", Params: map[string]any{}}); err != nil {
					return err
				}
			case "2":
				if !initialized {
					return fmt.Errorf("%w: billing/read before initialize", ErrGrokACPBillingMalformed)
				}
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func writeGrokACPJSONL(w io.Writer, message grokACPJSONRPCRequest) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", payload)
	return err
}

type grokACPStdoutEvent struct {
	line string
	err  error
}

func scanGrokACPStdout(ctx context.Context, stdout io.Reader, retained *boundedBuffer) <-chan grokACPStdoutEvent {
	events := make(chan grokACPStdoutEvent, 16)
	send := func(event grokACPStdoutEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 4096), grokACPLineBytes)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if retained != nil {
				_, _ = retained.Write([]byte(line + "\n"))
			}
			if !send(grokACPStdoutEvent{line: line}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = send(grokACPStdoutEvent{err: fmt.Errorf("%w: jsonl read: %v", ErrGrokACPBillingMalformed, err)})
			return
		}
		_ = send(grokACPStdoutEvent{err: io.EOF})
	}()
	return events
}

func decodeGrokACPBilling(output string) (map[string]any, []json.RawMessage, error) {
	if len(output) > grokACPOutputBytes {
		return nil, nil, fmt.Errorf("%w: decoded output exceeded limit", ErrGrokACPBillingMalformed)
	}
	messages, raws, err := decodeGrokACPJSONLMessages([]byte(output))
	if err != nil {
		return nil, nil, err
	}
	initialized := false
	var billing map[string]any
	for _, msg := range messages {
		if msg.Method != "" && jsonRPCID(msg.ID) == "" {
			continue
		}
		if msg.Error != nil {
			return nil, nil, grokACPRPCError(msg.Error)
		}
		switch jsonRPCID(msg.ID) {
		case "1":
			if !grokACPInitializeAdvertisesBilling(msg.Result) {
				return nil, nil, fmt.Errorf("%w: initialize did not advertise billing", ErrGrokACPBillingUnsupported)
			}
			initialized = true
		case "2":
			if !initialized {
				return nil, nil, fmt.Errorf("%w: billing/read before initialize", ErrGrokACPBillingMalformed)
			}
			if err := json.Unmarshal(msg.Result, &billing); err != nil {
				return nil, nil, fmt.Errorf("%w: billing/read result", ErrGrokACPBillingMalformed)
			}
		}
	}
	if billing == nil {
		return nil, nil, fmt.Errorf("%w: missing billing/read response", ErrGrokACPBillingMalformed)
	}
	return billing, raws, nil
}

func decodeGrokACPJSONLMessages(data []byte) ([]jsonRPCMessage, []json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("%w: empty output", ErrGrokACPBillingMalformed)
	}
	if bytes.HasPrefix(trimmed, []byte("Content-Length:")) {
		return nil, nil, fmt.Errorf("%w: content-length frames are unsupported", ErrGrokACPBillingMalformed)
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	messages := make([]jsonRPCMessage, 0, len(lines))
	raws := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(line) > grokACPLineBytes {
			return nil, nil, fmt.Errorf("%w: oversized jsonl line", ErrGrokACPBillingMalformed)
		}
		msg, err := decodeGrokACPJSONLMessage(string(line))
		if err != nil {
			return nil, nil, err
		}
		messages = append(messages, msg)
		raws = append(raws, append(json.RawMessage(nil), line...))
	}
	return messages, raws, nil
}

func decodeGrokACPJSONLMessage(line string) (jsonRPCMessage, error) {
	msg, err := decodeCodexJSONLMessage(line)
	if err != nil {
		return msg, fmt.Errorf("%w: json-rpc envelope", ErrGrokACPBillingMalformed)
	}
	return msg, nil
}

func grokACPInitializeAdvertisesBilling(raw json.RawMessage) bool {
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return false
	}
	return grokJSONContainsBillingCapability(value)
}

func grokJSONContainsBillingCapability(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "billing") || strings.Contains(lower, "quota") || strings.Contains(lower, "usage") {
				if boolLike(nested) || grokJSONContainsBillingCapability(nested) {
					return true
				}
			}
			switch nested.(type) {
			case map[string]any, []any:
				if grokJSONContainsBillingCapability(nested) {
					return true
				}
			}
			if methods, ok := nested.(string); ok && grokJSONContainsBillingCapability(methods) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if grokJSONContainsBillingCapability(item) {
				return true
			}
		}
	case string:
		lower := strings.ToLower(strings.TrimSpace(typed))
		return lower == "billing/read" || lower == "billing" || lower == "quota" || lower == "usage"
	}
	return false
}

func boolLike(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true") || strings.EqualFold(strings.TrimSpace(typed), "yes") || strings.Contains(strings.ToLower(typed), "billing")
	case []any, map[string]any:
		return grokJSONContainsBillingCapability(value)
	default:
		return false
	}
}

func grokACPRPCError(err *jsonRPCError) error {
	message := strings.TrimSpace(err.Message)
	if err.Code == -32601 || strings.Contains(strings.ToLower(message), "method not found") {
		return fmt.Errorf("%w: %s", ErrGrokACPBillingUnsupported, firstNonEmpty(message, "method not found"))
	}
	return fmt.Errorf("%w: %s", ErrGrokACPBillingRPC, message)
}

func snapshotsFromGrokACPBilling(source QuotaTelemetrySource, installationID *string, cliVersion string, billing map[string]any, frames []json.RawMessage, now time.Time) ([]QuotaSnapshot, error) {
	raw := bytes.Join(rawJSONFrames(frames), []byte("\n"))
	if credentialMaterialLike(string(raw)) {
		return nil, ErrQuotaCredentialMaterial
	}
	rawHash := rawSourceHash(raw)
	account := grokBillingAccountScope(billing)
	model := firstJSONText(billing, "model", "model_id", "modelId")
	if model != "" && !safeScopeToken(model) {
		model = ""
	}
	var snapshots []QuotaSnapshot
	usage := firstMapAny(billing, "usage", "token_usage", "tokenUsage")
	if len(usage) > 0 {
		snapshots = append(snapshots, grokTokenSnapshot(source, installationID, cliVersion, account, model, usage, "input_tokens", QuantityInputTokens, rawHash, now))
		snapshots = append(snapshots, grokTokenSnapshot(source, installationID, cliVersion, account, model, usage, "output_tokens", QuantityOutputTokens, rawHash, now))
		snapshots = append(snapshots, grokTokenSnapshot(source, installationID, cliVersion, account, model, usage, "total_tokens", QuantityTotalTokens, rawHash, now))
	}
	if cost := firstDecimalAny(billing, "cost_usd", "costUsd", "costUSD"); cost.ok {
		snapshots = append(snapshots, grokCostSnapshot(source, installationID, cliVersion, account, model, cost.value, cost.scale, rawHash, now))
	}
	for _, spec := range []struct {
		field string
		name  string
		unit  string
	}{
		{field: "consumer_weekly_allowance", name: "consumer_weekly_allowance", unit: "request"},
		{field: "build_session_allowance", name: "build_session_allowance", unit: "request"},
		{field: "api_credits", name: "api_credits", unit: "credit"},
		{field: "credits", name: "api_credits", unit: "credit"},
		{field: "rate_limit", name: "rate_limit_remaining", unit: "request"},
	} {
		if quantity := firstMapAny(billing, spec.field); len(quantity) > 0 {
			snapshots = append(snapshots, grokAllowanceSnapshot(source, installationID, cliVersion, account, model, spec.name, spec.unit, quantity, rawHash, now))
		}
	}
	if len(snapshots) == 0 || !containsGrokProviderWideAllowance(snapshots) {
		snapshots = append(snapshots, grokUnknownProviderAllowanceSnapshot(source, installationID, cliVersion, account, model, rawHash, now))
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].QuotaSnapshotID < snapshots[j].QuotaSnapshotID })
	return snapshots, nil
}

func grokTokenSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, account, model string, usage map[string]any, field string, kind QuantityKind, rawHash string, now time.Time) QuotaSnapshot {
	value, ok := integerField(usage, field, camelTokenField(field))
	conf := ConfidenceUnknown
	gaps := []string{}
	if ok && value != nil {
		conf = ConfidenceExact
	} else {
		gaps = append(gaps, "missing-"+strings.ReplaceAll(field, "_", "-"))
	}
	scope := grokScope(account, model, "execution_usage")
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, scope, field, formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "grok",
		ProviderInstallationID: installationID,
		ScopeKey:               scope,
		QuantityKind:           kind,
		ProviderQuantityName:   field,
		Unit:                   "token",
		WindowKind:             WindowUnknown,
		ResetSemantics:         ResetUnknown,
		UsedValue:              value,
		ValueScale:             0,
		Confidence:             conf,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": conf, "remaining_value": ConfidenceUnknown, "reset_at": ConfidenceUnknown},
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		StaleAfter:             formatTime(now.Add(30 * time.Minute)),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    fmt.Sprintf("grok ACP billing parser %s cli version %s official execution usage field %s", grokACPBillingSourceSchema, safeSummary(cliVersion), field),
		ConflictSet:            []string{},
		GapReasons:             gaps,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func grokCostSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, account, model string, value int64, scale int, rawHash string, now time.Time) QuotaSnapshot {
	scope := grokScope(account, model, "execution_cost")
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, scope, "cost_usd", formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "grok",
		ProviderInstallationID: installationID,
		ScopeKey:               scope,
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   "cost_usd",
		Unit:                   "usd",
		WindowKind:             WindowUnbounded,
		ResetSemantics:         ResetNone,
		UsedValue:              &value,
		ValueScale:             scale,
		Confidence:             ConfidenceExact,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceExact, "remaining_value": ConfidenceUnknown, "reset_at": ConfidenceUnknown},
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		StaleAfter:             formatTime(now.Add(30 * time.Minute)),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    fmt.Sprintf("grok ACP billing parser %s cli version %s official execution cost", grokACPBillingSourceSchema, safeSummary(cliVersion)),
		ConflictSet:            []string{},
		GapReasons:             []string{},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func grokAllowanceSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, account, model, name, unit string, quantity map[string]any, rawHash string, now time.Time) QuotaSnapshot {
	limit, limitOK := integerField(quantity, "limit", "limit_value", "limitValue", "allowance")
	used, usedOK := integerField(quantity, "used", "used_value", "usedValue")
	remaining, remainingOK := integerField(quantity, "remaining", "remaining_value", "remainingValue", "balance")
	resetAt := parseGrokResetAt(quantity)
	windowKind := grokAllowanceWindowKind(name, quantity)
	gaps := []string{}
	fieldConf := map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceUnknown, "remaining_value": ConfidenceUnknown, "reset_at": ConfidenceUnknown}
	conf := ConfidenceExact
	if limitOK {
		fieldConf["limit_value"] = ConfidenceExact
	} else {
		gaps = append(gaps, "missing-limit")
	}
	if usedOK {
		fieldConf["used_value"] = ConfidenceExact
	}
	if remainingOK {
		fieldConf["remaining_value"] = ConfidenceExact
	} else {
		gaps = append(gaps, "missing-remaining")
	}
	if resetAt != "" {
		fieldConf["reset_at"] = ConfidenceExact
	}
	if !limitOK && !usedOK && !remainingOK && resetAt == "" {
		conf = ConfidenceUnknown
		gaps = append(gaps, "absent-fields")
	}
	scope := grokScope(account, model, name)
	terminal := ""
	if remaining != nil && *remaining <= 0 {
		if name == "rate_limit_remaining" {
			terminal = "ErrRateLimited"
			gaps = append(gaps, "rate-limited-429")
		} else {
			terminal = "ErrQuotaExhausted"
			gaps = append(gaps, "quota-exhausted")
		}
	}
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, scope, name, formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "grok",
		ProviderInstallationID: installationID,
		ScopeKey:               scope,
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   name,
		Unit:                   unit,
		WindowKind:             windowKind,
		ResetAt:                resetAt,
		ResetSemantics:         grokResetSemantics(resetAt, windowKind),
		LimitValue:             limit,
		UsedValue:              used,
		RemainingValue:         remaining,
		ValueScale:             0,
		Confidence:             conf,
		FieldConfidences:       fieldConf,
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		ValidUntil:             resetAt,
		StaleAfter:             firstNonEmpty(resetAt, formatTime(now.Add(30*time.Minute))),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    fmt.Sprintf("grok ACP billing parser %s cli version %s distinct authority %s", grokACPBillingSourceSchema, safeSummary(cliVersion), name),
		ConflictSet:            []string{},
		GapReasons:             gaps,
		TerminalErrorCode:      terminal,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func grokUnknownProviderAllowanceSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, account, model, rawHash string, now time.Time) QuotaSnapshot {
	scope := grokScope(account, model, "provider_wide_allowance")
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("grok", source.QuotaSourceID, scope, "provider_wide_allowance", formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "grok",
		ProviderInstallationID: installationID,
		ScopeKey:               scope,
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   "provider_wide_allowance",
		Unit:                   "provider-defined",
		WindowKind:             WindowUnknown,
		ResetSemantics:         ResetUnknown,
		ValueScale:             0,
		Confidence:             ConfidenceUnknown,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceUnknown, "remaining_value": ConfidenceUnknown, "reset_at": ConfidenceUnknown},
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		StaleAfter:             formatTime(now.Add(30 * time.Minute)),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    fmt.Sprintf("grok ACP billing parser %s cli version %s provider-wide allowance absent", grokACPBillingSourceSchema, safeSummary(cliVersion)),
		ConflictSet:            []string{},
		GapReasons:             []string{"provider-wide-allowance-absent"},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func grokBillingAccountScope(billing map[string]any) string {
	for _, key := range []string{"account_id", "accountId", "account"} {
		value, ok := billing[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if safeScopeToken(typed) {
				return typed
			}
		case map[string]any:
			if id := firstJSONText(typed, "id", "account_id", "accountId"); safeScopeToken(id) {
				return id
			}
		}
	}
	return "unknown"
}

func grokScope(account, model, detail string) string {
	parts := []string{"provider:grok"}
	if account != "" {
		parts = append(parts, "account:"+account)
	}
	if model != "" {
		parts = append(parts, "model:"+model)
	}
	if detail != "" {
		parts = append(parts, "detail:"+detail)
	}
	return strings.Join(parts, "/")
}

type decimalField struct {
	value int64
	scale int
	ok    bool
}

func firstDecimalAny(values map[string]any, keys ...string) decimalField {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case json.Number:
			if parsed, scale, ok := parseScaledDecimal(typed.String()); ok {
				return decimalField{value: parsed, scale: scale, ok: true}
			}
		case float64:
			if parsed, scale, ok := parseScaledDecimal(strconv.FormatFloat(typed, 'f', -1, 64)); ok {
				return decimalField{value: parsed, scale: scale, ok: true}
			}
		case string:
			if parsed, scale, ok := parseScaledDecimal(typed); ok {
				return decimalField{value: parsed, scale: scale, ok: true}
			}
		}
	}
	return decimalField{}
}

func firstMapAny(values map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := values[key].(map[string]any); ok {
			return value
		}
	}
	return nil
}

func integerField(values map[string]any, keys ...string) (*int64, bool) {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case json.Number:
			parsed, err := strconv.ParseInt(typed.String(), 10, 64)
			if err == nil {
				return &parsed, true
			}
			if decimal, scale, ok := parseScaledDecimal(typed.String()); ok && scale == 0 {
				return &decimal, true
			}
		case float64:
			parsed := int64(typed)
			if float64(parsed) == typed {
				return &parsed, true
			}
		case string:
			if decimal, scale, ok := parseScaledDecimal(typed); ok && scale == 0 {
				return &decimal, true
			}
		}
		return nil, false
	}
	return nil, false
}

func camelTokenField(field string) string {
	switch field {
	case "input_tokens":
		return "inputTokens"
	case "output_tokens":
		return "outputTokens"
	case "total_tokens":
		return "totalTokens"
	default:
		return field
	}
}

func parseGrokResetAt(values map[string]any) string {
	for _, key := range []string{"reset_at", "resetAt", "resets_at", "resetsAt"} {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(typed)); err == nil {
				return formatTime(parsed)
			}
		case json.Number:
			if seconds, err := strconv.ParseInt(typed.String(), 10, 64); err == nil {
				return formatTime(time.Unix(seconds, 0))
			}
		case float64:
			seconds := int64(typed)
			if float64(seconds) == typed {
				return formatTime(time.Unix(seconds, 0))
			}
		}
	}
	return ""
}

func grokAllowanceWindowKind(name string, values map[string]any) WindowKind {
	window := strings.ToLower(firstJSONText(values, "window", "window_kind", "windowKind"))
	switch {
	case strings.Contains(window, "week") || strings.Contains(name, "weekly"):
		return WindowProviderDefined
	case strings.Contains(window, "rolling"):
		return WindowRolling
	case strings.Contains(window, "provider"):
		return WindowProviderDefined
	case strings.Contains(window, "unbounded"):
		return WindowUnbounded
	default:
		return WindowUnknown
	}
}

func grokResetSemantics(resetAt string, window WindowKind) ResetSemantics {
	if resetAt == "" {
		return ResetUnknown
	}
	if window == WindowFixedWeek {
		return ResetWindowBoundary
	}
	return ResetProviderDefined
}

func containsGrokProviderWideAllowance(snapshots []QuotaSnapshot) bool {
	for _, snapshot := range snapshots {
		if snapshot.ProviderQuantityName == "provider_wide_allowance" {
			return true
		}
	}
	return false
}

func grokACPProtocolSummary(output string) string {
	if strings.TrimSpace(output) == "" {
		return ""
	}
	messages, _, err := decodeGrokACPJSONLMessages([]byte(output))
	if err != nil {
		return "malformed-grok-acp-json-rpc"
	}
	var parts []string
	for _, msg := range messages {
		if msg.Method != "" {
			parts = append(parts, "method:"+safeSummary(msg.Method))
			continue
		}
		if msg.Error != nil {
			parts = append(parts, "error:"+strconv.Itoa(msg.Error.Code))
			continue
		}
		if id := jsonRPCID(msg.ID); id != "" {
			parts = append(parts, "response:"+id)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func grokACPBillingProtocolError(err error) bool {
	return errors.Is(err, ErrGrokACPBillingUnsupported) || errors.Is(err, ErrGrokACPBillingMalformed) || errors.Is(err, ErrGrokACPBillingRPC)
}

func grokACPBillingReason(err error) string {
	switch {
	case errors.Is(err, ErrGrokACPBillingUnsupported):
		if strings.Contains(strings.ToLower(err.Error()), "method") {
			return "method-not-found"
		}
		return "unsupported-source"
	case errors.Is(err, ErrGrokACPBillingMalformed):
		return "malformed-frame"
	case errors.Is(err, ErrGrokACPBillingRPC):
		lower := strings.ToLower(err.Error())
		switch {
		case strings.Contains(lower, "rate"):
			return "rate-limited-429"
		case strings.Contains(lower, "auth") || strings.Contains(lower, "expired") || strings.Contains(lower, "unauthorized"):
			return "auth-expired"
		case strings.Contains(lower, "model"):
			return "model-unavailable"
		case strings.Contains(lower, "outage") || strings.Contains(lower, "5xx") || strings.Contains(lower, "unavailable"):
			return "provider-outage"
		case strings.Contains(lower, "quota") || strings.Contains(lower, "credit"):
			return "quota-exhausted"
		default:
			return "rpc-error"
		}
	case errors.Is(err, ErrGrokACPBillingTimeout):
		return "quota-probe-timeout"
	default:
		return "quota-probe-failed"
	}
}

func grokACPBillingTerminal(err error) string {
	switch grokACPBillingReason(err) {
	case "method-not-found", "unsupported-source":
		return "ErrQuotaSourceUnsupported"
	case "malformed-frame":
		return "ErrQuotaSnapshotMalformed"
	case "rate-limited-429":
		return "ErrRateLimited"
	case "auth-expired":
		return "ErrAuthExpired"
	case "model-unavailable":
		return "ErrModelUnavailable"
	case "provider-outage":
		return "ErrProviderOutage"
	case "quota-exhausted":
		return "ErrQuotaExhausted"
	default:
		if errors.Is(err, ErrGrokACPBillingTimeout) {
			return "ErrGrokACPBillingTimeout"
		}
		if errors.Is(err, ErrGrokACPBillingRPC) {
			return "ErrGrokACPBillingRPCError"
		}
		return "ErrGrokACPBillingExecutionFailed"
	}
}
