package providerinventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	codexQuotaSourceSchema = "codex.app_server.rate_limits.v1"
	codexQuotaTimeout      = 10 * time.Second
	codexQuotaOutputBytes  = 128 * 1024
)

var (
	ErrCodexQuotaGrantRequired = errors.New("ErrQuotaCollectionGrantRequired")
	ErrCodexQuotaUnsupported   = errors.New("ErrUnsupportedVersion")
	ErrCodexQuotaMalformed     = errors.New("ErrCodexQuotaMalformedFrame")
	ErrCodexQuotaRPC           = errors.New("ErrCodexQuotaRPCError")
	ErrCodexQuotaTimeout       = errors.New("ErrCodexQuotaTimeout")
)

type CodexAppServerRequest struct {
	Argv               []string
	Env                []string
	Timeout            time.Duration
	StdoutLimitBytes   int
	StderrLimitBytes   int
	CombinedLimitBytes int
}

type CodexAppServerResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Killed   bool
}

type jsonRPCMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func inspectCodexQuota(ctx context.Context, discovery *discoveryContext, adapter AdapterDeclaration, candidate candidate, installation ProviderInstallation, now time.Time, deps Deps) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
	source := codexQuotaSource(now)
	installationID := installation.ProviderInstallationID
	probe := baseProbe(adapter, now, deps)
	probe.ProviderInstallationID = &installationID
	probe.ProbeKind = "quota"
	probe.ProbeCommandID = "codex-app-server-rate-limits"
	probe.ProbeMethod = ProbeMethodMachineJSON
	probe.TimeoutMS = int(codexQuotaTimeout / time.Millisecond)
	probe.StdoutLimitBytes = codexQuotaOutputBytes
	probe.StderrLimitBytes = StdoutLimitBytes
	probe.CombinedOutputLimitBytes = codexQuotaOutputBytes + StdoutLimitBytes
	probe.StaleAfter = formatTime(now.Add(30 * time.Minute))
	probe.NetworkDeclared = true
	probe.NetworkPermission = networkPermissionFor(discovery, adapter, NetworkPurposeQuotaTelemetry, true)
	argv := []string{candidate.path, "-s", "read-only", "-a", "untrusted", "app-server"}
	env := probeEnvironment(deps.Getenv)
	probe.Argv = redactArgv(argv)
	probe.EnvironmentKeys = environmentKeys(env)
	probe.Source = SourceDescriptor{Kind: "command", AdapterID: adapter.AdapterID, ProbeCommandID: probe.ProbeCommandID, DiscoverySource: string(candidate.source), ExecutableName: filepath.Base(candidate.path)}
	probe.Evidence = EvidenceSummary{Kind: "bounded-codex-app-server-json-rpc", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	unavailable := func(reason, terminal string) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
		snapshot := codexQuotaUnavailableSnapshot(source, &installationID, now, reason, terminal)
		probe.Outcome = OutcomeProbeFailed
		probe.Confidence = ConfidenceUnavailable
		probe.FreshnessState = FreshnessNotApplicable
		probe.GapReasons = []string{reason}
		probe.TerminalErrorCode = terminal
		return source, []QuotaSnapshot{snapshot}, probe
	}

	if !codexQuotaVersionSupported(installation.Version) {
		return unavailable("unsupported-cli-version", "ErrUnsupportedVersion")
	}
	if probe.NetworkPermission != NetworkGranted {
		probe.SideEffectClass = "not-run"
		return unavailable("quota-collection-not-granted", "ErrQuotaCollectionGrantRequired")
	}

	result, err := deps.RunCodexRPC(ctx, CodexAppServerRequest{
		Argv:               argv,
		Env:                env,
		Timeout:            codexQuotaTimeout,
		StdoutLimitBytes:   codexQuotaOutputBytes,
		StderrLimitBytes:   StdoutLimitBytes,
		CombinedLimitBytes: codexQuotaOutputBytes + StdoutLimitBytes,
	})
	stdout, stdoutFindings := redactProviderOutputBeforeTruncate(result.Stdout, 4096)
	stderr, stderrFindings := redactProviderOutputBeforeTruncate(result.Stderr, 4096)
	probe.StdoutSummary = stdout
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	if err != nil || result.TimedOut || result.Killed {
		if result.TimedOut {
			return unavailable("quota-probe-timeout", "ErrCodexQuotaTimeout")
		}
		return unavailable("quota-probe-failed", "ErrCodexQuotaExecutionFailed")
	}
	if result.ExitCode != 0 {
		return unavailable("quota-probe-nonzero-exit", "ErrCodexQuotaNonZeroExit")
	}
	if credentialMaterialLike(result.Stdout) || credentialMaterialLike(result.Stderr) {
		return unavailable("credential-material-redacted", "ErrQuotaCredentialMaterial")
	}

	account, limits, frames, err := decodeCodexQuotaRPC(result.Stdout)
	if err != nil {
		return unavailable(codexQuotaReason(err), codexQuotaTerminal(err))
	}
	snapshots, err := snapshotsFromCodexRateLimits(source, &installationID, installation.Version, account, limits, frames, now)
	if err != nil {
		return unavailable(codexQuotaReason(err), codexQuotaTerminal(err))
	}
	probe.Outcome = OutcomeInstalled
	probe.Confidence = ConfidenceExact
	probe.setParsedFields(map[string]string{
		"parser":         codexQuotaSourceSchema,
		"snapshot_count": strconv.Itoa(len(snapshots)),
	})
	return source, snapshots, probe
}

func codexQuotaSource(now time.Time) QuotaTelemetrySource {
	now = now.UTC()
	return normalizeQuotaTelemetrySource(QuotaTelemetrySource{
		AdapterID:              "codex",
		SourceKind:             QuotaSourceOfficialCLICommand,
		SourceKey:              "codex-app-server-rate-limits-v1",
		SourceSchemaVersion:    codexQuotaSourceSchema,
		SupportedQuantities:    []QuantityKind{QuantityRequests, QuantityProviderDefined},
		SupportedWindows:       []WindowKind{WindowRolling, WindowFixedWeek, WindowProviderDefined, WindowUnbounded, WindowUnknown},
		ScopeDimensions:        []string{"provider", "account", "scope"},
		ConfidenceContract:     map[string]Confidence{"limit_value": ConfidenceExact, "used_value": ConfidenceExact, "remaining_value": ConfidenceExact, "reset_at": ConfidenceExact},
		NetworkDeclared:        true,
		NetworkPermissionScope: "provider:codex/action:quota-read/side-effect:read/freshness:interactive",
		Argv:                   []string{"codex", "-s", "read-only", "-a", "untrusted", "app-server"},
		EnvironmentKeys:        allowedProbeEnvKeys(),
		TimeoutMS:              int(codexQuotaTimeout / time.Millisecond),
		OutputLimits:           OutputLimits{StdoutBytes: codexQuotaOutputBytes, StderrBytes: StdoutLimitBytes, CombinedBytes: codexQuotaOutputBytes + StdoutLimitBytes, DecodedBytes: codexQuotaOutputBytes},
		ClassificationRules:    []string{"json-rpc-field-allowlist", "redact-before-truncate", "no-credential-material", "no-login-update-or-provider-work"},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
		GapReasons:             []string{},
	})
}

func codexQuotaUnavailableSnapshot(source QuotaTelemetrySource, installationID *string, now time.Time, reason, terminal string) QuotaSnapshot {
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("codex", source.QuotaSourceID, reason, formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "codex",
		ProviderInstallationID: installationID,
		ScopeKey:               "provider:codex",
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
		RedactedDiagnostics:    "codex app-server quota unavailable due to " + reason,
		ConflictSet:            []string{},
		GapReasons:             []string{reason, "not-collected"},
		TerminalErrorCode:      terminal,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func runCodexAppServer(ctx context.Context, req CodexAppServerRequest) (CodexAppServerResult, error) {
	if len(req.Argv) == 0 || strings.TrimSpace(req.Argv[0]) == "" {
		return CodexAppServerResult{ExitCode: -1}, errors.New("codex app-server argv is empty")
	}
	input := encodeJSONRPCFrame(jsonRPCMessage{JSONRPC: "2.0", ID: 1, Method: "account/read"}) +
		encodeJSONRPCFrame(jsonRPCMessage{JSONRPC: "2.0", ID: 2, Method: "account/rateLimits/read"})
	budget := newOutputBudget(req.CombinedLimitBytes)
	stdout := newBoundedBuffer(req.StdoutLimitBytes, budget)
	stderr := newBoundedBuffer(req.StderrLimitBytes, budget)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = os.TempDir()
	cmd.Env = append([]string{}, req.Env...)
	cmd.Stdin = strings.NewReader(input)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	result, err := supervisedexec.Run(ctx, cmd, supervisedexec.Options{HardCap: req.Timeout, LivenessMode: supervisedexec.LivenessModeLogOnly, Role: "codex-quota-app-server"})
	exitCode := result.ExitCode
	if (err != nil || result.Outcome == supervisedexec.OutcomeDeadline || result.Killed) && exitCode == 0 {
		exitCode = -1
	}
	out := CodexAppServerResult{
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
		out.Stderr += "[loopcoder] codex app-server output truncated"
	}
	return out, err
}

func encodeJSONRPCFrame(message jsonRPCMessage) string {
	payload, _ := json.Marshal(message)
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(payload), payload)
}

func decodeCodexQuotaRPC(output string) (map[string]any, map[string]any, []json.RawMessage, error) {
	if len(output) > codexQuotaOutputBytes {
		return nil, nil, nil, fmt.Errorf("%w: decoded output exceeded limit", ErrCodexQuotaMalformed)
	}
	messages, raws, err := decodeJSONRPCMessages([]byte(output))
	if err != nil {
		return nil, nil, nil, err
	}
	var account map[string]any
	var limits map[string]any
	for _, msg := range messages {
		if msg.Method != "" && msg.ID == nil {
			continue
		}
		if msg.Error != nil {
			return nil, nil, nil, fmt.Errorf("%w: %s", ErrCodexQuotaRPC, msg.Error.Message)
		}
		switch jsonRPCID(msg.ID) {
		case "1":
			if err := json.Unmarshal(msg.Result, &account); err != nil {
				return nil, nil, nil, fmt.Errorf("%w: account/read result", ErrCodexQuotaMalformed)
			}
		case "2":
			if err := json.Unmarshal(msg.Result, &limits); err != nil {
				return nil, nil, nil, fmt.Errorf("%w: account/rateLimits/read result", ErrCodexQuotaMalformed)
			}
		}
	}
	if account == nil || limits == nil {
		return nil, nil, nil, fmt.Errorf("%w: missing account/read or account/rateLimits/read response", ErrCodexQuotaMalformed)
	}
	return account, limits, raws, nil
}

func decodeJSONRPCMessages(data []byte) ([]jsonRPCMessage, []json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("%w: empty output", ErrCodexQuotaMalformed)
	}
	if bytes.HasPrefix(trimmed, []byte("Content-Length:")) {
		return decodeContentLengthFrames(trimmed)
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	messages := make([]jsonRPCMessage, 0, len(lines))
	raws := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var msg jsonRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, nil, fmt.Errorf("%w: newline-delimited json", ErrCodexQuotaMalformed)
		}
		messages = append(messages, msg)
		raws = append(raws, append(json.RawMessage(nil), line...))
	}
	return messages, raws, nil
}

func decodeContentLengthFrames(data []byte) ([]jsonRPCMessage, []json.RawMessage, error) {
	var messages []jsonRPCMessage
	var raws []json.RawMessage
	for len(bytes.TrimSpace(data)) > 0 {
		headerEnd := bytes.Index(data, []byte("\r\n\r\n"))
		sepLen := 4
		if headerEnd < 0 {
			headerEnd = bytes.Index(data, []byte("\n\n"))
			sepLen = 2
		}
		if headerEnd < 0 {
			return nil, nil, fmt.Errorf("%w: missing frame header terminator", ErrCodexQuotaMalformed)
		}
		headers := string(data[:headerEnd])
		length := -1
		for _, line := range strings.Split(strings.ReplaceAll(headers, "\r\n", "\n"), "\n") {
			name, value, ok := strings.Cut(line, ":")
			if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil || n < 0 || n > codexQuotaOutputBytes {
					return nil, nil, fmt.Errorf("%w: invalid content length", ErrCodexQuotaMalformed)
				}
				length = n
			}
		}
		if length < 0 {
			return nil, nil, fmt.Errorf("%w: missing content length", ErrCodexQuotaMalformed)
		}
		start := headerEnd + sepLen
		if len(data) < start+length {
			return nil, nil, fmt.Errorf("%w: truncated frame", ErrCodexQuotaMalformed)
		}
		payload := data[start : start+length]
		var msg jsonRPCMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			return nil, nil, fmt.Errorf("%w: frame payload", ErrCodexQuotaMalformed)
		}
		messages = append(messages, msg)
		raws = append(raws, append(json.RawMessage(nil), payload...))
		data = bytes.TrimLeft(data[start+length:], "\r\n\t ")
	}
	return messages, raws, nil
}

func snapshotsFromCodexRateLimits(source QuotaTelemetrySource, installationID *string, cliVersion string, account, limits map[string]any, frames []json.RawMessage, now time.Time) ([]QuotaSnapshot, error) {
	accountScope := codexAccountScope(account)
	items := codexLimitItems(limits)
	if len(items) == 0 {
		return nil, fmt.Errorf("%w: no recognized rate limit windows", ErrCodexQuotaMalformed)
	}
	raw := bytes.Join(rawJSONFrames(frames), []byte("\n"))
	if credentialMaterialLike(string(raw)) {
		return nil, ErrQuotaCredentialMaterial
	}
	rawHash := rawSourceHash(raw)
	var snapshots []QuotaSnapshot
	for _, item := range items {
		snapshot := codexSnapshotFromItem(source, installationID, cliVersion, accountScope, item, rawHash, now)
		if snapshot.ScopeKey == "" {
			continue
		}
		snapshots = append(snapshots, snapshot)
	}
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("%w: no supported quota fields", ErrCodexQuotaMalformed)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].QuotaSnapshotID < snapshots[j].QuotaSnapshotID
	})
	return snapshots, nil
}

type codexLimitItem struct {
	Name   string
	Scope  string
	Fields map[string]any
}

func codexLimitItems(value any) []codexLimitItem {
	var out []codexLimitItem
	collectCodexLimitItems("", value, &out)
	return out
}

func collectCodexLimitItems(name string, value any, out *[]codexLimitItem) {
	switch typed := value.(type) {
	case map[string]any:
		if codexMapHasQuotaFields(typed) {
			*out = append(*out, codexLimitItem{Name: firstNonEmpty(codexStringField(typed, "name", "window", "window_kind", "period", "type"), name), Scope: codexStringField(typed, "scope", "scope_key"), Fields: typed})
			return
		}
		for _, key := range sortedMapKeys(typed) {
			collectCodexLimitItems(key, typed[key], out)
		}
	case []any:
		for _, item := range typed {
			collectCodexLimitItems(name, item, out)
		}
	}
}

func codexMapHasQuotaFields(fields map[string]any) bool {
	for _, key := range []string{"limit", "limit_value", "used", "used_value", "remaining", "remaining_value", "reset_at", "resetAt", "window_start", "windowStart", "credits", "balance"} {
		if _, ok := fields[key]; ok {
			return true
		}
	}
	return false
}

func codexSnapshotFromItem(source QuotaTelemetrySource, installationID *string, cliVersion, accountScope string, item codexLimitItem, rawHash string, now time.Time) QuotaSnapshot {
	name := normalizeQuotaName(item.Name)
	quantity := QuantityRequests
	providerName := name
	if strings.Contains(name, "credit") || strings.Contains(name, "balance") {
		quantity = QuantityProviderDefined
		providerName = "credits"
	}
	windowKind, rolling := codexWindowKind(name, item.Fields)
	windowStart := codexTimeField(item.Fields, "window_start", "windowStart", "start", "starts_at", "startsAt")
	windowEnd := codexTimeField(item.Fields, "window_end", "windowEnd", "end", "ends_at", "endsAt", "expires_at", "expiresAt")
	resetAt := codexTimeField(item.Fields, "reset_at", "resetAt", "resets_at", "resetsAt")
	if resetAt == "" {
		resetAt = windowEnd
	}
	resetSemantics := ResetUnknown
	if resetAt != "" {
		resetSemantics = ResetProviderDefined
		if windowEnd != "" && resetAt == windowEnd {
			resetSemantics = ResetWindowBoundary
		}
	}
	if windowKind == WindowFixedWeek && (windowStart == "" || windowEnd == "") {
		windowKind = WindowRolling
		rolling = int64((7 * 24 * time.Hour).Milliseconds())
	}
	if windowKind == WindowRolling && rolling == 0 {
		rolling = int64((5 * time.Hour).Milliseconds())
	}
	limit, limitOK := codexIntField(item.Fields, "limit", "limit_value", "limitValue", "max", "total")
	used, usedOK := codexIntField(item.Fields, "used", "used_value", "usedValue", "current")
	remaining, remainingOK := codexIntField(item.Fields, "remaining", "remaining_value", "remainingValue", "available", "balance")
	fieldConf := map[string]Confidence{
		"limit_value":     ConfidenceUnknown,
		"used_value":      ConfidenceUnknown,
		"remaining_value": ConfidenceUnknown,
		"reset_at":        ConfidenceUnknown,
	}
	var gaps []string
	if limitOK {
		fieldConf["limit_value"] = ConfidenceExact
	} else {
		gaps = append(gaps, "missing-limit")
	}
	if usedOK {
		fieldConf["used_value"] = ConfidenceExact
	} else {
		gaps = append(gaps, "missing-used")
	}
	if remainingOK {
		fieldConf["remaining_value"] = ConfidenceExact
	} else {
		gaps = append(gaps, "missing-remaining")
	}
	if resetAt != "" {
		fieldConf["reset_at"] = ConfidenceExact
	}
	scope := "provider:codex"
	if accountScope != "" {
		scope += "/account:" + accountScope
	}
	if item.Scope != "" && safeScopeToken(item.Scope) {
		scope += "/scope:" + item.Scope
	}
	if scope == "provider:codex" && item.Scope != "" {
		gaps = append(gaps, "scope-redacted")
	}
	confidence := ConfidenceExact
	if !limitOK && !usedOK && !remainingOK && resetAt == "" {
		confidence = ConfidenceUnknown
		gaps = append(gaps, "absent-fields")
	}
	diag := fmt.Sprintf("codex app-server parser %s cli version %s protocol jsonrpc-2.0 window %s", codexQuotaSourceSchema, safeSummary(cliVersion), name)
	snapshot := normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("codex", source.QuotaSourceID, scope, string(quantity), string(windowKind), name, formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "codex",
		ProviderInstallationID: installationID,
		ScopeKey:               scope,
		QuantityKind:           quantity,
		ProviderQuantityName:   providerName,
		WindowKind:             windowKind,
		WindowStart:            windowStart,
		WindowEnd:              windowEnd,
		RollingDurationMS:      rolling,
		ResetAt:                resetAt,
		ResetSemantics:         resetSemantics,
		LimitValue:             limit,
		UsedValue:              used,
		RemainingValue:         remaining,
		ValueScale:             0,
		Confidence:             confidence,
		FieldConfidences:       fieldConf,
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		ValidUntil:             firstNonEmpty(resetAt, windowEnd),
		StaleAfter:             firstNonEmpty(resetAt, windowEnd, formatTime(now.Add(30*time.Minute))),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    diag,
		ConflictSet:            []string{},
		GapReasons:             gaps,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
	return snapshot
}

func codexQuotaVersionSupported(version string) bool {
	parts := firstVersionParts(version)
	if len(parts) == 0 {
		return false
	}
	if parts[0] > 0 {
		return true
	}
	if len(parts) > 1 && parts[1] >= 8 {
		return true
	}
	return false
}

func codexWindowKind(name string, fields map[string]any) (WindowKind, int64) {
	period := normalizeQuotaName(firstNonEmpty(name, codexStringField(fields, "period", "window", "window_kind", "windowKind")))
	switch {
	case strings.Contains(period, "five") || strings.Contains(period, "5h") || strings.Contains(period, "5_hour"):
		return WindowRolling, int64((5 * time.Hour).Milliseconds())
	case strings.Contains(period, "week"):
		return WindowFixedWeek, 0
	case strings.Contains(period, "credit") || strings.Contains(period, "balance"):
		return WindowUnbounded, 0
	case period != "":
		return WindowProviderDefined, 0
	default:
		return WindowUnknown, 0
	}
}

func codexAccountScope(account map[string]any) string {
	for _, container := range []map[string]any{account, codexMapField(account, "account"), codexMapField(account, "profile")} {
		if container == nil {
			continue
		}
		value := codexStringField(container, "id", "account_id", "accountID", "profile_id", "profileID")
		if safeScopeToken(value) {
			return value
		}
	}
	return "unknown"
}

func codexIntField(fields map[string]any, keys ...string) (*int64, bool) {
	for _, key := range keys {
		value, ok := fields[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			if math.Trunc(typed) == typed {
				n := int64(typed)
				return &n, true
			}
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64); err == nil {
				return &n, true
			}
		case json.Number:
			if n, err := typed.Int64(); err == nil {
				return &n, true
			}
		}
	}
	return nil, false
}

func codexTimeField(fields map[string]any, keys ...string) string {
	value := codexStringField(fields, keys...)
	if value == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	return formatTime(parsed)
}

func codexStringField(fields map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := fields[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok {
			text = strings.TrimSpace(text)
			if text != "" && !secretLike(text) {
				return boundedToken(text, 80)
			}
		}
	}
	return ""
}

func codexMapField(fields map[string]any, key string) map[string]any {
	if fields == nil {
		return nil
	}
	if nested, ok := fields[key].(map[string]any); ok {
		return nested
	}
	return nil
}

func jsonRPCID(id any) string {
	switch typed := id.(type) {
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func rawJSONFrames(frames []json.RawMessage) [][]byte {
	out := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		out = append(out, []byte(frame))
	}
	return out
}

func normalizeQuotaName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "_")
	value = strings.ReplaceAll(value, "-", "_")
	return value
}

func safeScopeToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) || strings.ContainsAny(value, "/ \t\r\n:=") {
		return false
	}
	return len(value) <= 80
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func codexQuotaReason(err error) string {
	switch {
	case errors.Is(err, ErrQuotaCredentialMaterial):
		return "credential-material-redacted"
	case errors.Is(err, ErrCodexQuotaRPC):
		return "rpc-error"
	case errors.Is(err, ErrCodexQuotaMalformed):
		return "malformed-frame"
	case errors.Is(err, ErrCodexQuotaTimeout):
		return "quota-probe-timeout"
	case errors.Is(err, ErrCodexQuotaUnsupported):
		return "unsupported-cli-version"
	default:
		return "quota-probe-failed"
	}
}

func codexQuotaTerminal(err error) string {
	switch {
	case errors.Is(err, ErrQuotaCredentialMaterial):
		return "ErrQuotaCredentialMaterial"
	case errors.Is(err, ErrCodexQuotaRPC):
		return "ErrCodexQuotaRPCError"
	case errors.Is(err, ErrCodexQuotaMalformed):
		return "ErrCodexQuotaMalformedFrame"
	case errors.Is(err, ErrCodexQuotaTimeout):
		return "ErrCodexQuotaTimeout"
	case errors.Is(err, ErrCodexQuotaUnsupported):
		return "ErrUnsupportedVersion"
	default:
		return "ErrCodexQuotaExecutionFailed"
	}
}

func redactProviderOutputBeforeTruncate(output string, limit int) (string, int) {
	redacted, findings := redactProviderOutputNoTruncate(output)
	if limit > 0 && len(redacted) > limit {
		redacted = redacted[:limit] + "\n[loopcoder] summary truncated"
	}
	return redacted, findings
}

func redactProviderOutputNoTruncate(output string) (string, int) {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	output = strings.TrimSpace(output)
	findings := 0
	for _, pattern := range secretPatterns {
		matches := pattern.FindAllStringIndex(output, -1)
		if len(matches) == 0 {
			continue
		}
		findings += len(matches)
		output = pattern.ReplaceAllString(output, "[REDACTED]")
	}
	output = genericKeyValueSecretPattern.ReplaceAllStringFunc(output, func(match string) string {
		submatches := genericKeyValueSecretPattern.FindStringSubmatch(match)
		if len(submatches) < 3 || !looksLikeOpaqueSecretValue(submatches[2]) {
			return match
		}
		findings++
		return "[REDACTED]"
	})
	output = emailPattern.ReplaceAllStringFunc(output, redactEmail)
	return output, findings
}

func credentialMaterialLike(value string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	for _, match := range genericKeyValueSecretPattern.FindAllStringSubmatch(value, -1) {
		if len(match) < 3 {
			continue
		}
		key := strings.ToLower(match[1])
		if strings.Contains(key, "key") || strings.Contains(key, "secret") || strings.Contains(key, "token") || strings.Contains(key, "password") || strings.Contains(key, "credential") {
			if looksLikeOpaqueSecretValue(match[2]) {
				return true
			}
		}
	}
	return false
}
