package providerinventory

import (
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

	"github.com/jasonhnd/loopcoder/internal/codexauth"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

const (
	codexQuotaSourceSchema = "codex.app_server.protocol.v2.rate_limits.v1"
	codexQuotaTimeout      = 10 * time.Second
	codexQuotaOutputBytes  = 128 * 1024
	codexQuotaLineBytes    = 64 * 1024
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
	// Drive, when non-nil, runs the JSON-RPC session after process start.
	// When nil, the default quota session (account/read + rateLimits/read) is used.
	// Catalog collection supplies its own Drive (account/read + model/list pages).
	Drive func(ctx context.Context, stdin io.Writer, events <-chan codexProtocolStdoutEvent) error
}

type CodexAppServerResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Killed   bool
}

type codexProtocolStdoutEvent struct {
	line string
	err  error
}

// codexProtocolCapture lets os/exec drain stdout before Wait returns. Using
// Cmd.StdoutPipe while Wait runs concurrently can lose a fast process's final
// protocol frame when Wait closes the pipe first.
type codexProtocolCapture struct {
	ctx      context.Context
	events   chan codexProtocolStdoutEvent
	retained *boundedBuffer
	pending  []byte
	terminal bool
}

type jsonRPCMessage struct {
	ID     any             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params any             `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *jsonRPCError   `json:"error,omitempty"`

	idPresent bool
}

type jsonRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`

	codePresent    bool
	messagePresent bool
}

func (m *jsonRPCMessage) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	type wireMessage struct {
		ID     any             `json:"id,omitempty"`
		Method string          `json:"method,omitempty"`
		Params any             `json:"params,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  *jsonRPCError   `json:"error,omitempty"`
	}
	var wire wireMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&wire); err != nil {
		return err
	}
	*m = jsonRPCMessage{
		ID:        wire.ID,
		Method:    wire.Method,
		Params:    wire.Params,
		Result:    wire.Result,
		Error:     wire.Error,
		idPresent: raw["id"] != nil,
	}
	return nil
}

func (e *jsonRPCError) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	type wireError struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	}
	var wire wireError
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*e = jsonRPCError{
		Code:           wire.Code,
		Message:        wire.Message,
		Data:           wire.Data,
		codePresent:    raw["code"] != nil,
		messagePresent: raw["message"] != nil,
	}
	return nil
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
	probe.Evidence = EvidenceSummary{Kind: "bounded-codex-app-server-jsonl-rpc", CommandBounded: true, NoShell: true, RepositoryMutation: false, SecretMaterialRetained: false}

	unavailable := func(reason, terminal string) (QuotaTelemetrySource, []QuotaSnapshot, ProbeResult) {
		snapshot := codexQuotaUnavailableSnapshot(source, &installationID, now, reason, terminal)
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

	result, err := deps.RunCodexRPC(ctx, CodexAppServerRequest{
		Argv:               argv,
		Env:                env,
		Timeout:            codexQuotaTimeout,
		StdoutLimitBytes:   codexQuotaOutputBytes,
		StderrLimitBytes:   StdoutLimitBytes,
		CombinedLimitBytes: codexQuotaOutputBytes + StdoutLimitBytes,
	})
	_, stdoutFindings := redactProviderOutputNoTruncate(result.Stdout)
	stderr, stderrFindings := redactProviderOutputBeforeTruncate(result.Stderr, 4096)
	probe.StdoutSummary = codexProtocolSummary(result.Stdout)
	probe.StderrSummary = stderr
	probe.SecretFindingCount = stdoutFindings + stderrFindings
	probe.TimedOut = result.TimedOut
	probe.Killed = result.Killed
	probe.ExitCode = &result.ExitCode
	if err != nil && codexQuotaProtocolError(err) {
		return unavailable(codexQuotaReason(err), codexQuotaTerminal(err))
	}
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
		SupportedQuantities:    []QuantityKind{QuantityProviderDefined},
		SupportedWindows:       []WindowKind{WindowRolling, WindowFixedWeek, WindowProviderDefined, WindowUnbounded, WindowUnknown},
		ScopeDimensions:        []string{"provider", "account", "scope"},
		ConfidenceContract:     map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceExact, "remaining_value": ConfidenceEstimated, "reset_at": ConfidenceExact},
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
	budget := newOutputBudget(req.CombinedLimitBytes)
	stdout := newBoundedBuffer(req.StdoutLimitBytes, budget)
	stderr := newBoundedBuffer(req.StderrLimitBytes, budget)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = os.TempDir()
	cmd.Env = append([]string{}, req.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return CodexAppServerResult{ExitCode: -1}, err
	}
	cmd.Stderr = stderr
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	captureCtx, stopCapture := context.WithCancel(runCtx)
	defer stopCapture()
	capture := &codexProtocolCapture{
		ctx:      captureCtx,
		events:   make(chan codexProtocolStdoutEvent, 16),
		retained: stdout,
	}
	cmd.Stdout = capture
	runCh := make(chan struct {
		result supervisedexec.Result
		err    error
	}, 1)
	go func() {
		role := "codex-app-server"
		if req.Drive == nil {
			role = "codex-quota-app-server"
		}
		result, err := supervisedexec.Run(runCtx, cmd, supervisedexec.Options{HardCap: req.Timeout, LivenessMode: supervisedexec.LivenessModeLogOnly, Role: role})
		capture.close()
		runCh <- struct {
			result supervisedexec.Result
			err    error
		}{result: result, err: err}
	}()
	drive := req.Drive
	if drive == nil {
		drive = driveCodexAppServerProtocolEvents
	}
	protocolErr := drive(runCtx, stdin, capture.events)
	stopCapture()
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
	if protocolErr != nil && (exitCode == 0 || result.Killed) {
		err = protocolErr
	}
	return out, err
}

func (c *codexProtocolCapture) Write(data []byte) (int, error) {
	written := len(data)
	if c == nil || c.terminal || c.ctx.Err() != nil {
		return written, nil
	}
	c.pending = append(c.pending, data...)
	for {
		newline := bytes.IndexByte(c.pending, '\n')
		if newline < 0 {
			break
		}
		line := append([]byte(nil), c.pending[:newline]...)
		c.pending = c.pending[newline+1:]
		if !c.emitLine(line) {
			return written, nil
		}
	}
	if len(c.pending) > codexQuotaLineBytes {
		c.pending = nil
		c.terminal = true
		c.send(codexProtocolStdoutEvent{err: fmt.Errorf("%w: oversized jsonl line", ErrCodexQuotaMalformed)})
	}
	return written, nil
}

func (c *codexProtocolCapture) close() {
	if c == nil || c.terminal || c.ctx.Err() != nil {
		return
	}
	if len(c.pending) > 0 && !c.emitLine(c.pending) {
		return
	}
	c.pending = nil
	c.send(codexProtocolStdoutEvent{err: io.EOF})
}

func (c *codexProtocolCapture) emitLine(raw []byte) bool {
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return true
	}
	if len(line) > codexQuotaLineBytes {
		c.terminal = true
		return c.send(codexProtocolStdoutEvent{err: fmt.Errorf("%w: oversized jsonl line", ErrCodexQuotaMalformed)})
	}
	if c.retained != nil {
		_, _ = c.retained.Write([]byte(line + "\n"))
	}
	return c.send(codexProtocolStdoutEvent{line: line})
}

func (c *codexProtocolCapture) send(event codexProtocolStdoutEvent) bool {
	select {
	case c.events <- event:
		return true
	case <-c.ctx.Done():
		return false
	}
}

func driveCodexAppServerProtocolEvents(ctx context.Context, stdin io.Writer, events <-chan codexProtocolStdoutEvent) error {
	if err := writeJSONL(stdin, jsonRPCMessage{ID: 1, Method: "initialize", Params: map[string]any{
		"clientInfo": map[string]any{"name": "loopcoder", "version": PolicyVersion},
		"capabilities": map[string]any{
			"experimentalApi":           false,
			"optOutNotificationMethods": []string{"thread/started", "thread/status_changed", "account/rate_limits/updated"},
		},
	}}); err != nil {
		return err
	}
	initialized := false
	accountSeen := false
	limitsSeen := false
	for {
		select {
		case event := <-events:
			if event.err != nil {
				if errors.Is(event.err, io.EOF) {
					return fmt.Errorf("%w: eof before required responses", ErrCodexQuotaMalformed)
				}
				return event.err
			}
			line := event.line
			msg, err := decodeCodexJSONLMessage(line)
			if err != nil {
				return err
			}
			if msg.Error != nil {
				return fmt.Errorf("%w: %s", ErrCodexQuotaRPC, msg.Error.Message)
			}
			switch jsonRPCID(msg.ID) {
			case "1":
				if initialized {
					continue
				}
				if err := validateCodexInitializeResponse(msg.Result); err != nil {
					return err
				}
				if err := writeJSONL(stdin, jsonRPCMessage{Method: "initialized"}); err != nil {
					return err
				}
				initialized = true
				if err := writeJSONL(stdin, jsonRPCMessage{ID: 2, Method: "account/read", Params: map[string]any{"refreshToken": false}}); err != nil {
					return err
				}
				if err := writeJSONL(stdin, jsonRPCMessage{ID: 3, Method: "account/rateLimits/read", Params: map[string]any{}}); err != nil {
					return err
				}
			case "2":
				if !initialized {
					return fmt.Errorf("%w: account/read before initialize", ErrCodexQuotaUnsupported)
				}
				accountSeen = true
			case "3":
				if !initialized {
					return fmt.Errorf("%w: account/rateLimits/read before initialize", ErrCodexQuotaUnsupported)
				}
				limitsSeen = true
			}
			if initialized && accountSeen && limitsSeen {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func writeJSONL(w io.Writer, message jsonRPCMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", payload)
	return err
}

func decodeCodexQuotaRPC(output string) (map[string]any, map[string]any, []json.RawMessage, error) {
	if len(output) > codexQuotaOutputBytes {
		return nil, nil, nil, fmt.Errorf("%w: decoded output exceeded limit", ErrCodexQuotaMalformed)
	}
	messages, raws, err := decodeJSONLMessages([]byte(output))
	if err != nil {
		return nil, nil, nil, err
	}
	var account map[string]any
	var limits map[string]any
	for _, msg := range messages {
		if msg.Method != "" && jsonRPCID(msg.ID) == "" {
			continue
		}
		if msg.Error != nil {
			return nil, nil, nil, fmt.Errorf("%w: %s", ErrCodexQuotaRPC, msg.Error.Message)
		}
		switch jsonRPCID(msg.ID) {
		case "1":
			if err := validateCodexInitializeResponse(msg.Result); err != nil {
				return nil, nil, nil, err
			}
		case "2":
			if err := json.Unmarshal(msg.Result, &account); err != nil {
				return nil, nil, nil, fmt.Errorf("%w: account/read result", ErrCodexQuotaMalformed)
			}
		case "3":
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

func decodeJSONLMessages(data []byte) ([]jsonRPCMessage, []json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("%w: empty output", ErrCodexQuotaMalformed)
	}
	if bytes.HasPrefix(trimmed, []byte("Content-Length:")) {
		return nil, nil, fmt.Errorf("%w: content-length frames are unsupported", ErrCodexQuotaMalformed)
	}
	lines := bytes.Split(trimmed, []byte("\n"))
	messages := make([]jsonRPCMessage, 0, len(lines))
	raws := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if len(line) > codexQuotaLineBytes {
			return nil, nil, fmt.Errorf("%w: oversized jsonl line", ErrCodexQuotaMalformed)
		}
		msg, err := decodeCodexJSONLMessage(string(line))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: newline-delimited json", ErrCodexQuotaMalformed)
		}
		messages = append(messages, msg)
		raws = append(raws, append(json.RawMessage(nil), line...))
	}
	return messages, raws, nil
}

func decodeCodexJSONLMessage(line string) (jsonRPCMessage, error) {
	var msg jsonRPCMessage
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&msg); err != nil {
		return msg, fmt.Errorf("%w: jsonl message", ErrCodexQuotaMalformed)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return msg, fmt.Errorf("%w: jsonl trailing token", ErrCodexQuotaMalformed)
	}
	if err := validateCodexAppServerEnvelope(msg); err != nil {
		return msg, err
	}
	return msg, nil
}

func validateCodexAppServerEnvelope(msg jsonRPCMessage) error {
	id, validID := jsonRPCIDValue(msg.ID)
	if msg.idPresent && !validID {
		return fmt.Errorf("%w: json-rpc id envelope", ErrCodexQuotaMalformed)
	}
	hasID := msg.idPresent && id != ""
	hasMethod := strings.TrimSpace(msg.Method) != ""
	hasResult := msg.Result != nil
	hasError := msg.Error != nil
	validError := hasError && msg.Error.codePresent && msg.Error.messagePresent && strings.TrimSpace(msg.Error.Message) != ""
	switch {
	case hasMethod && hasID && !hasResult && !hasError:
		return nil
	case hasMethod && !hasID && !hasResult && !hasError:
		return nil
	case hasID && !hasMethod && hasResult && !hasError:
		return nil
	case hasID && !hasMethod && !hasResult && validError:
		return nil
	case hasError && !validError:
		return fmt.Errorf("%w: json-rpc error envelope", ErrCodexQuotaMalformed)
	default:
		return fmt.Errorf("%w: app-server envelope", ErrCodexQuotaMalformed)
	}
}

func validateCodexInitializeResponse(result json.RawMessage) error {
	var payload struct {
		CodexHome      string `json:"codexHome"`
		PlatformFamily string `json:"platformFamily"`
		PlatformOS     string `json:"platformOs"`
		UserAgent      string `json:"userAgent"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return fmt.Errorf("%w: initialize result", ErrCodexQuotaUnsupported)
	}
	if strings.TrimSpace(payload.CodexHome) == "" || strings.TrimSpace(payload.PlatformFamily) == "" || strings.TrimSpace(payload.PlatformOS) == "" || strings.TrimSpace(payload.UserAgent) == "" {
		return fmt.Errorf("%w: initialize schema", ErrCodexQuotaUnsupported)
	}
	return nil
}

func snapshotsFromCodexRateLimits(source QuotaTelemetrySource, installationID *string, cliVersion string, account, limits map[string]any, frames []json.RawMessage, now time.Time) ([]QuotaSnapshot, error) {
	// Account scope intentionally empty for capacity join. Codex AuthReadiness stamps
	// shared codexauth acct-+64hex (parseCodexLoginStatus). Rate-limit RPC must not
	// invent a second principal-derived ID from account/read — empty-account rows
	// rebind only to the sole Ready AuthReadiness AccountProfileID on the same live
	// installation (capacitysnapshot). Nonempty conflicting principals are never
	// overwritten or treated as equivalent.
	// Never emit account:unknown. Never stamp an unrelated opaque hash as AccountProfileID.
	accountScope := codexAccountScope(account)
	// codexCanonicalAccountProfileID remains available for catalog/tests; quota does not stamp it.
	raw := bytes.Join(rawJSONFrames(frames), []byte("\n"))
	if credentialMaterialLike(string(raw)) {
		return nil, ErrQuotaCredentialMaterial
	}
	rawHash := rawSourceHash(raw)
	parsed, err := parseCodexRateLimitsResponse(limits)
	if err != nil {
		return nil, err
	}
	var snapshots []QuotaSnapshot
	snapshots = append(snapshots, snapshotsFromCodexRateLimitSnapshot(source, installationID, cliVersion, accountScope, "default", parsed.RateLimits, rawHash, now)...)
	for _, key := range sortedRateLimitIDs(parsed.RateLimitsByLimitID) {
		snapshots = append(snapshots, snapshotsFromCodexRateLimitSnapshot(source, installationID, cliVersion, accountScope, key, parsed.RateLimitsByLimitID[key], rawHash, now)...)
	}
	if parsed.RateLimitResetCredits != nil {
		snapshots = append(snapshots, codexResetCreditsSnapshot(source, installationID, cliVersion, accountScope, parsed.RateLimitResetCredits, rawHash, now))
	}
	if len(snapshots) == 0 {
		return nil, fmt.Errorf("%w: no supported quota fields", ErrCodexQuotaMalformed)
	}
	// Do NOT stamp AccountProfileID from codexCanonicalAccountProfileID here.
	// That ID scheme is not the AuthReadiness status ID used for capacity routing.
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].QuotaSnapshotID < snapshots[j].QuotaSnapshotID
	})
	return snapshots, nil
}

type codexRateLimitsResponse struct {
	RateLimitResetCredits *codexRateLimitResetCreditsSummary `json:"rateLimitResetCredits"`
	RateLimits            codexRateLimitSnapshot             `json:"rateLimits"`
	RateLimitsByLimitID   map[string]codexRateLimitSnapshot  `json:"rateLimitsByLimitId"`
}

type codexRateLimitSnapshot struct {
	Credits              *codexCreditsSnapshot           `json:"credits"`
	IndividualLimit      *codexSpendControlLimitSnapshot `json:"individualLimit"`
	LimitID              *string                         `json:"limitId"`
	LimitName            *string                         `json:"limitName"`
	PlanType             *string                         `json:"planType"`
	Primary              *codexRateLimitWindow           `json:"primary"`
	RateLimitReachedType *string                         `json:"rateLimitReachedType"`
	Secondary            *codexRateLimitWindow           `json:"secondary"`
}

type codexRateLimitWindow struct {
	ResetsAt           *int64 `json:"resetsAt"`
	UsedPercent        *int64 `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
}

type codexCreditsSnapshot struct {
	Balance    *string `json:"balance"`
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
}

type codexSpendControlLimitSnapshot struct {
	Limit            string `json:"limit"`
	RemainingPercent *int64 `json:"remainingPercent"`
	ResetsAt         *int64 `json:"resetsAt"`
	Used             string `json:"used"`
}

type codexRateLimitResetCreditsSummary struct {
	AvailableCount int64                       `json:"availableCount"`
	Credits        []codexRateLimitResetCredit `json:"credits"`
}

type codexRateLimitResetCredit struct {
	ExpiresAt *int64 `json:"expiresAt"`
	GrantedAt int64  `json:"grantedAt"`
	ID        string `json:"id"`
	ResetType string `json:"resetType"`
	Status    string `json:"status"`
}

func parseCodexRateLimitsResponse(limits map[string]any) (codexRateLimitsResponse, error) {
	var parsed codexRateLimitsResponse
	data, err := json.Marshal(limits)
	if err != nil {
		return parsed, fmt.Errorf("%w: rate limits remarshal", ErrCodexQuotaMalformed)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return parsed, fmt.Errorf("%w: rate limits schema", ErrCodexQuotaMalformed)
	}
	if parsed.RateLimitsByLimitID == nil {
		parsed.RateLimitsByLimitID = map[string]codexRateLimitSnapshot{}
	}
	return parsed, nil
}

func sortedRateLimitIDs(values map[string]codexRateLimitSnapshot) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if safeScopeToken(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func snapshotsFromCodexRateLimitSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, accountScope, scopeLabel string, limit codexRateLimitSnapshot, rawHash string, now time.Time) []QuotaSnapshot {
	if scoped := codexStringPtr(limit.LimitID); scoped != "" && safeScopeToken(scoped) {
		scopeLabel = scoped
	}
	var snapshots []QuotaSnapshot
	if limit.Primary != nil {
		snapshots = append(snapshots, codexWindowSnapshot(source, installationID, cliVersion, accountScope, scopeLabel, "primary", limit.Primary, rawHash, now))
	}
	if limit.Secondary != nil {
		snapshots = append(snapshots, codexWindowSnapshot(source, installationID, cliVersion, accountScope, scopeLabel, "secondary", limit.Secondary, rawHash, now))
	}
	if limit.Credits != nil {
		snapshots = append(snapshots, codexCreditsBalanceSnapshot(source, installationID, cliVersion, accountScope, scopeLabel, limit.Credits, rawHash, now))
	}
	if limit.IndividualLimit != nil {
		snapshots = append(snapshots, codexIndividualLimitSnapshot(source, installationID, cliVersion, accountScope, scopeLabel, limit.IndividualLimit, rawHash, now))
	}
	return snapshots
}

func codexWindowSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, accountScope, scopeLabel, windowName string, window *codexRateLimitWindow, rawHash string, now time.Time) QuotaSnapshot {
	durationMS, durationOK := codexWindowDurationMS(window.WindowDurationMins)
	resetTime, resetAt, resetOK := codexUnixSecondsTimestamp(window.ResetsAt)
	windowKind := codexWindowKindFromDuration(window.WindowDurationMins, resetOK)
	windowStart := ""
	windowEnd := ""
	invalidBoundary := false
	if fixedQuotaWindow(windowKind) && durationOK {
		duration := time.Duration(durationMS) * time.Millisecond
		windowStart = codexFormatParseableTime(resetTime.Add(-duration))
		windowEnd = resetAt
		if windowStart == "" || windowEnd == "" {
			windowKind = WindowProviderDefined
			windowStart = ""
			windowEnd = ""
			invalidBoundary = true
		}
	}
	used, usedOK := boundedPercent(window.UsedPercent)
	var remaining *int64
	remainingConfidence := ConfidenceUnknown
	gaps := []string{}
	if usedOK {
		value := int64(100) - *used
		remaining = &value
		remainingConfidence = ConfidenceEstimated
		gaps = append(gaps, "remaining-derived-from-used-percent")
	} else {
		gaps = append(gaps, "missing-used-percent")
	}
	if window.WindowDurationMins == nil {
		gaps = append(gaps, "missing-window-duration")
	} else if !durationOK {
		gaps = append(gaps, "invalid-window-duration")
	} else if invalidBoundary {
		gaps = append(gaps, "invalid-window-boundary")
	}
	fieldConf := map[string]Confidence{
		"limit_value":     ConfidenceUnknown,
		"used_value":      ConfidenceUnknown,
		"remaining_value": remainingConfidence,
		"reset_at":        ConfidenceUnknown,
	}
	if usedOK {
		fieldConf["used_value"] = ConfidenceExact
	}
	if resetAt != "" {
		fieldConf["reset_at"] = ConfidenceExact
	} else {
		if window.ResetsAt == nil {
			gaps = append(gaps, "missing-reset-at")
		} else {
			gaps = append(gaps, "invalid-reset-at")
		}
	}
	confidence := ConfidenceExact
	if !usedOK && resetAt == "" && window.WindowDurationMins == nil {
		confidence = ConfidenceUnknown
		gaps = append(gaps, "absent-fields")
	}
	scope := codexScope(accountScope, scopeLabel, windowName)
	providerName := windowName + "_used_percent"
	diag := fmt.Sprintf("codex app-server parser %s cli version %s protocol jsonl schema v2 window %s percent remaining derived when present", codexQuotaSourceSchema, safeSummary(cliVersion), windowName)
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("codex", source.QuotaSourceID, scope, providerName, string(windowKind), formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "codex",
		ProviderInstallationID: installationID,
		ScopeKey:               scope,
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   providerName,
		Unit:                   "percent",
		WindowKind:             windowKind,
		WindowStart:            windowStart,
		WindowEnd:              windowEnd,
		RollingDurationMS:      durationMS,
		ResetAt:                resetAt,
		ResetSemantics:         codexResetSemantics(resetAt, windowKind),
		UsedValue:              used,
		RemainingValue:         remaining,
		ValueScale:             0,
		Confidence:             confidence,
		FieldConfidences:       fieldConf,
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		ValidUntil:             resetAt,
		StaleAfter:             firstNonEmpty(resetAt, formatTime(now.Add(30*time.Minute))),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    diag,
		ConflictSet:            []string{},
		GapReasons:             gaps,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func codexCreditsBalanceSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, accountScope, scopeLabel string, credits *codexCreditsSnapshot, rawHash string, now time.Time) QuotaSnapshot {
	var remaining *int64
	scale := 0
	conf := ConfidenceUnknown
	gaps := []string{}
	if credits.Balance != nil {
		if parsed, parsedScale, ok := parseScaledDecimal(*credits.Balance); ok {
			remaining = &parsed
			scale = parsedScale
			conf = ConfidenceExact
		} else {
			gaps = append(gaps, "malformed-credit-balance")
		}
	} else if !credits.HasCredits && !credits.Unlimited {
		zero := int64(0)
		remaining = &zero
		conf = ConfidenceExact
	} else {
		gaps = append(gaps, "missing-credit-balance")
	}
	if credits.Unlimited {
		gaps = append(gaps, "credits-unlimited")
	}
	scope := codexScope(accountScope, scopeLabel, "credits")
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("codex", source.QuotaSourceID, scope, "credits_balance", formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "codex",
		ProviderInstallationID: installationID,
		ScopeKey:               scope,
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   "credits_balance",
		Unit:                   "credits",
		WindowKind:             WindowUnbounded,
		ResetSemantics:         ResetNone,
		RemainingValue:         remaining,
		ValueScale:             scale,
		Confidence:             conf,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceUnknown, "remaining_value": conf, "reset_at": ConfidenceUnknown},
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		StaleAfter:             formatTime(now.Add(30 * time.Minute)),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    fmt.Sprintf("codex app-server parser %s cli version %s protocol jsonl schema v2 credits balance", codexQuotaSourceSchema, safeSummary(cliVersion)),
		ConflictSet:            []string{},
		GapReasons:             gaps,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func codexResetCreditsSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, accountScope string, credits *codexRateLimitResetCreditsSummary, rawHash string, now time.Time) QuotaSnapshot {
	count := credits.AvailableCount
	validUntil := earliestCreditExpiry(credits.Credits)
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("codex", source.QuotaSourceID, codexScope(accountScope, "default", "reset_credits"), "rate_limit_reset_credits", formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "codex",
		ProviderInstallationID: installationID,
		ScopeKey:               codexScope(accountScope, "default", "reset_credits"),
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   "rate_limit_reset_credits",
		Unit:                   "credit",
		WindowKind:             WindowUnbounded,
		ResetSemantics:         ResetNone,
		RemainingValue:         &count,
		ValueScale:             0,
		Confidence:             ConfidenceExact,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceUnknown, "remaining_value": ConfidenceExact, "reset_at": ConfidenceUnknown},
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		ValidUntil:             validUntil,
		StaleAfter:             firstNonEmpty(validUntil, formatTime(now.Add(30*time.Minute))),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    fmt.Sprintf("codex app-server parser %s cli version %s protocol jsonl schema v2 reset credits", codexQuotaSourceSchema, safeSummary(cliVersion)),
		ConflictSet:            []string{},
		GapReasons:             []string{},
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func codexIndividualLimitSnapshot(source QuotaTelemetrySource, installationID *string, cliVersion, accountScope, scopeLabel string, limit *codexSpendControlLimitSnapshot, rawHash string, now time.Time) QuotaSnapshot {
	remaining, ok := boundedPercent(limit.RemainingPercent)
	conf := ConfidenceUnknown
	gaps := []string{}
	if ok {
		conf = ConfidenceExact
	} else {
		gaps = append(gaps, "missing-remaining-percent")
	}
	resetAt := codexUnixSecondsTime(limit.ResetsAt)
	if resetAt == "" {
		gaps = append(gaps, "missing-reset-at")
	}
	scope := codexScope(accountScope, scopeLabel, "individual_limit")
	return normalizeQuotaSnapshot(QuotaSnapshot{
		QuotaSnapshotID:        quotaSnapshotID("codex", source.QuotaSourceID, scope, "spend_control_remaining_percent", formatTime(now)),
		QuotaSourceID:          source.QuotaSourceID,
		SourceKind:             source.SourceKind,
		AdapterID:              "codex",
		ProviderInstallationID: installationID,
		ScopeKey:               scope,
		QuantityKind:           QuantityProviderDefined,
		ProviderQuantityName:   "spend_control_remaining_percent",
		Unit:                   "percent",
		WindowKind:             WindowProviderDefined,
		ResetAt:                resetAt,
		ResetSemantics:         codexResetSemantics(resetAt, WindowProviderDefined),
		RemainingValue:         remaining,
		ValueScale:             0,
		Confidence:             conf,
		FieldConfidences:       map[string]Confidence{"limit_value": ConfidenceUnknown, "used_value": ConfidenceUnknown, "remaining_value": conf, "reset_at": confidenceForPresent(resetAt != "")},
		FreshnessState:         FreshnessFresh,
		CapturedAt:             formatTime(now),
		ValidUntil:             resetAt,
		StaleAfter:             firstNonEmpty(resetAt, formatTime(now.Add(30*time.Minute))),
		RawSourceHash:          rawHash,
		RedactedDiagnostics:    fmt.Sprintf("codex app-server parser %s cli version %s protocol jsonl schema v2 individual limit", codexQuotaSourceSchema, safeSummary(cliVersion)),
		ConflictSet:            []string{},
		GapReasons:             gaps,
		CreatedAt:              formatTime(now),
		UpdatedAt:              formatTime(now),
		PolicyVersion:          PolicyVersion,
	})
}

func codexWindowKindFromDuration(minutes *int64, hasReset bool) WindowKind {
	if _, ok := codexWindowDurationMS(minutes); !ok {
		return WindowUnknown
	}
	switch *minutes {
	case 300:
		return WindowRolling
	case 10080:
		if hasReset {
			return WindowFixedWeek
		}
		return WindowProviderDefined
	default:
		return WindowProviderDefined
	}
}

func codexUnixSecondsTime(value *int64) string {
	_, formatted, ok := codexUnixSecondsTimestamp(value)
	if !ok {
		return ""
	}
	return formatted
}

func codexWindowDurationMS(minutes *int64) (int64, bool) {
	if minutes == nil || *minutes <= 0 {
		return 0, false
	}
	const maxDurationMinutes = int64(1<<63-1) / int64(time.Minute)
	if *minutes > maxDurationMinutes {
		return 0, false
	}
	return int64((time.Duration(*minutes) * time.Minute).Milliseconds()), true
}

func codexUnixSecondsTimestamp(value *int64) (time.Time, string, bool) {
	if value == nil || *value <= 0 {
		return time.Time{}, "", false
	}
	t := time.Unix(*value, 0).UTC()
	formatted := codexFormatParseableTime(t)
	if formatted == "" {
		return time.Time{}, "", false
	}
	return t, formatted, true
}

func codexFormatParseableTime(t time.Time) string {
	formatted := formatTime(t)
	if _, err := time.Parse(time.RFC3339Nano, formatted); err != nil {
		return ""
	}
	return formatted
}

func fixedQuotaWindow(kind WindowKind) bool {
	return kind == WindowFixedHour || kind == WindowFixedDay || kind == WindowFixedWeek
}

func boundedPercent(value *int64) (*int64, bool) {
	if value == nil || *value < 0 || *value > 100 {
		return nil, false
	}
	v := *value
	return &v, true
}

func parseScaledDecimal(value string) (int64, int, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, 0, false
	}
	sign := int64(1)
	if strings.HasPrefix(value, "-") {
		sign = -1
		value = strings.TrimPrefix(value, "-")
	}
	whole, frac, hasFrac := strings.Cut(value, ".")
	if whole == "" {
		whole = "0"
	}
	if !allDigits(whole) || (hasFrac && !allDigits(frac)) {
		return 0, 0, false
	}
	scale := 0
	if hasFrac {
		frac = strings.TrimRight(frac, "0")
		scale = len(frac)
	}
	digits := whole + frac
	if digits == "" {
		digits = "0"
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return sign * n, scale, true
}

func allDigits(value string) bool {
	if value == "" {
		return true
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func earliestCreditExpiry(credits []codexRateLimitResetCredit) string {
	var earliest *int64
	for _, credit := range credits {
		if credit.ExpiresAt == nil || *credit.ExpiresAt <= 0 {
			continue
		}
		if earliest == nil || *credit.ExpiresAt < *earliest {
			v := *credit.ExpiresAt
			earliest = &v
		}
	}
	return codexUnixSecondsTime(earliest)
}

func codexScope(accountScope, scopeLabel, detail string) string {
	scope := "provider:codex"
	if accountScope != "" && safeScopeToken(accountScope) {
		scope += "/account:" + accountScope
	}
	if scopeLabel != "" && safeScopeToken(scopeLabel) {
		scope += "/scope:" + scopeLabel
	}
	if detail != "" && safeScopeToken(detail) {
		scope += "/detail:" + detail
	}
	return scope
}

func codexResetSemantics(resetAt string, windowKind WindowKind) ResetSemantics {
	if resetAt == "" {
		return ResetUnknown
	}
	if windowKind == WindowRolling {
		return ResetRolling
	}
	if fixedQuotaWindow(windowKind) {
		return ResetWindowBoundary
	}
	return ResetProviderDefined
}

func confidenceForPresent(present bool) Confidence {
	if present {
		return ConfidenceExact
	}
	return ConfidenceUnknown
}

func codexStringPtr(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func codexProtocolSummary(output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	lines := strings.Count(output, "\n") + 1
	return fmt.Sprintf("codex app-server JSONL protocol messages retained as hash only; lines=%d bytes=%d", lines, len(output))
}

func codexAccountScope(account map[string]any) string {
	// Capacity ScopeKey must not invent an account segment from account/read.
	// AuthReadiness already carries the shared codexauth AccountProfileID; putting a
	// raw principal or alternate hash into ScopeKey can split rate-limit windows from
	// the authenticated install account. Empty is correct; capacity rebinds to sole
	// AuthReadiness AccountProfileID on the same installation when unambiguous.
	// Never emit account:unknown.
	_ = account
	return ""
}

// codexCanonicalAccountProfileID returns the shared opaque acct- binding used by
// inventory and agent execution (internal/codexauth). Empty when not exact-routable.
//
// Only account/read RPC-affirmed identity fields are accepted. Never falls back to
// a local auth.json (that would stamp RPC quota under a different process identity).
// Never invents from email/plan alone. Canonical hash is provider+stable principal
// only (plan is separate evidence).
func codexCanonicalAccountProfileID(account map[string]any) string {
	for _, container := range []map[string]any{account, codexMapField(account, "account"), codexMapField(account, "profile")} {
		if container == nil {
			continue
		}
		id := codexStringField(container,
			"id", "account_id", "accountID", "chatgpt_account_id", "chatgptAccountId",
			"chatgpt_user_id", "chatgptUserId",
		)
		if id == "" || strings.Contains(id, "@") {
			continue
		}
		if strings.EqualFold(id, "unknown") || strings.EqualFold(id, "root") {
			continue
		}
		// plan ignored for identity; codexauth.CanonicalAccountProfileID is principal-only.
		if acct := codexauth.CanonicalAccountProfileID(id, "", ""); acct != "" {
			return acct
		}
	}
	// Fail unknown — do not invent from local auth file or email/planType.
	return ""
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
	value, ok := jsonRPCIDValue(id)
	if !ok {
		return ""
	}
	return value
}

func jsonRPCIDValue(id any) (string, bool) {
	switch typed := id.(type) {
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case string:
		if strings.TrimSpace(typed) == "" {
			return "", false
		}
		return typed, true
	case json.Number:
		value := typed.String()
		if !jsonNumberIsInteger(value) {
			return "", false
		}
		return value, true
	default:
		return "", false
	}
}

func jsonNumberIsInteger(value string) bool {
	if value == "" {
		return false
	}
	if value[0] == '-' {
		value = value[1:]
	}
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func rawJSONFrames(frames []json.RawMessage) [][]byte {
	out := make([][]byte, 0, len(frames))
	for _, frame := range frames {
		out = append(out, []byte(frame))
	}
	return out
}

func safeScopeToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || secretLike(value) || strings.ContainsAny(value, "/ \t\r\n:=") {
		return false
	}
	return len(value) <= 80
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

func codexQuotaProtocolError(err error) bool {
	return errors.Is(err, ErrQuotaCredentialMaterial) ||
		errors.Is(err, ErrCodexQuotaRPC) ||
		errors.Is(err, ErrCodexQuotaMalformed) ||
		errors.Is(err, ErrCodexQuotaTimeout) ||
		errors.Is(err, ErrCodexQuotaUnsupported)
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
