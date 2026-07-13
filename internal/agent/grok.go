package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/supervisedexec"
)

type GrokRunner struct{}

func init() {
	registry["grok"] = GrokRunner{}
}

const (
	grokMinVersionMajor = 0
	grokMinVersionMinor = 1
	grokMaxFrameBytes   = 64 * 1024
	grokMaxLogBytes     = 1024 * 1024
	grokMaxSummaryBytes = 8192
	grokProbeTimeout    = 5 * time.Second
)

type GrokErrorCode string

const (
	GrokErrUnsupportedCapability GrokErrorCode = "unsupported-capability"
	GrokErrMalformedFrame        GrokErrorCode = "malformed-frame"
	GrokErrOutputFlood           GrokErrorCode = "output-flood"
	GrokErrTimeout               GrokErrorCode = "timeout"
	GrokErrCanceled              GrokErrorCode = "canceled"
	GrokErrTransportLoss         GrokErrorCode = "transport-loss"
	GrokErrProviderError         GrokErrorCode = "provider-error"
	GrokErrNonzeroExit           GrokErrorCode = "nonzero-exit"
)

type GrokError struct {
	Code    GrokErrorCode
	Message string
	Err     error
}

func (e *GrokError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("grok %s: %s", e.Code, e.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("grok %s: %v", e.Code, e.Err)
	}
	return "grok " + string(e.Code)
}

func (e *GrokError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type grokProbeResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

var runGrokProbe = runGrokProbeCommand

func BuildGrokArgs(inv Invocation) []string {
	args := []string{
		"--no-auto-update",
		"-p", inv.Prompt,
		"--cwd", inv.WorktreePath,
		"--output-format", "streaming-json",
		"--no-alt-screen",
		"--disable-web-search",
		"--no-subagents",
		"--no-memory",
	}
	if inv.ReadOnly {
		args = append(args,
			"--sandbox", "read-only",
			"--deny", "write:*",
			"--deny", "shell:*",
			"--disallowed-tools", "write,edit,shell,terminal",
		)
	} else {
		args = append(args, "--always-approve")
	}
	if strings.TrimSpace(inv.Model) != "" {
		args = append(args, "-m", inv.Model)
	}
	if strings.TrimSpace(inv.Effort) != "" {
		args = append(args, "--effort", inv.Effort)
	}
	return args
}

func (GrokRunner) Run(ctx context.Context, inv Invocation) (Result, error) {
	if strings.TrimSpace(inv.LogPath) == "" {
		return Result{ExitCode: -1}, errors.New("grok log path is required")
	}
	if _, err := mcpServersForInvocation(inv); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("grok MCP configuration: %w", err)
	}
	if len(mcpServersForArgs(inv)) > 0 {
		return Result{ExitCode: -1}, grokError(GrokErrUnsupportedCapability, "MCP configuration is not supported by the bounded Grok runner", nil)
	}
	if strings.TrimSpace(inv.OutputSchema) != "" {
		return Result{ExitCode: -1}, grokError(GrokErrUnsupportedCapability, "schema-enforced JSON output is not advertised by Grok headless", nil)
	}

	logFile, err := createSensitiveFile(inv.LogPath)
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("open grok log: %w", err)
	}
	defer logFile.Close()

	capability, err := negotiateGrokCapability(ctx, inv)
	if err != nil {
		return Result{ExitCode: -1}, err
	}

	sink := newGrokStreamSink(logFile, grokMaxLogBytes, grokMaxFrameBytes)
	sink.writeRecord(grokNormalizedRecord{
		Kind:           "start",
		Provider:       "grok",
		AdapterVersion: capability.Version,
		Attempt:        inv.RunID,
		PermissionMode: grokPermissionMode(inv.ReadOnly),
		Workspace:      inv.WorktreePath,
	})

	cmd := exec.CommandContext(ctx, "grok", BuildGrokArgs(inv)...)
	cmd.Dir = inv.WorktreePath
	cmd.Env = grokBoundedEnv(os.Environ(), inv)
	cmd.Stdin = nil
	cmd.Stdout = sink.stdoutWriter()
	cmd.Stderr = sink.stderrWriter()

	startedAt := time.Now()
	supervision, runErr := runProviderCommand(ctx, cmd, inv, "grok")
	endedAt := time.Now()
	sink.close()
	_ = logFile.Sync()

	metadata := sink.metadata(inv)
	metadata.AdapterVersion = capability.Version
	result := resultWithSupervision(supervisedExitCode(supervision, runErr), sink.summary(), metadata.invocationMetadata, startedAt, endedAt, supervision, runErr, ctx)
	result.AdapterVersion = capability.Version
	result.ExternalSessionRef = metadata.ExternalSessionRef

	if err := grokSupervisionError(supervision, runErr, ctx); err != nil {
		return result, err
	}
	if err := sink.err(); err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		return result, grokError(GrokErrNonzeroExit, fmt.Sprintf("process exited with code %d", result.ExitCode), nil)
	}
	if !sink.terminalSeen() {
		return result, grokError(GrokErrTransportLoss, "stream ended before a terminal result frame", nil)
	}
	if strings.TrimSpace(result.Model) == "" {
		return result, grokError(GrokErrMalformedFrame, "terminal result did not identify a model", nil)
	}
	return result, nil
}

type grokCapability struct {
	Version string
}

func negotiateGrokCapability(ctx context.Context, inv Invocation) (grokCapability, error) {
	env := grokBoundedEnv(os.Environ(), inv)
	versionResult, err := runGrokProbe(ctx, []string{"grok", "version"}, inv.WorktreePath, env, grokProbeTimeout, 32*1024)
	if err != nil {
		return grokCapability{}, grokError(GrokErrUnsupportedCapability, "version probe failed", err)
	}
	if versionResult.ExitCode != 0 {
		return grokCapability{}, grokError(GrokErrUnsupportedCapability, fmt.Sprintf("version probe exited with code %d", versionResult.ExitCode), nil)
	}
	version := parseGrokVersion(versionResult.Stdout + "\n" + versionResult.Stderr)
	if version == "" {
		return grokCapability{}, grokError(GrokErrUnsupportedCapability, "version probe did not return a parseable version", nil)
	}
	if grokVersionLess(version, []int{grokMinVersionMajor, grokMinVersionMinor, 0}) {
		return grokCapability{}, grokError(GrokErrUnsupportedCapability, "installed version "+version+" does not support bounded headless execution", nil)
	}

	helpResult, err := runGrokProbe(ctx, []string{"grok", "--help"}, inv.WorktreePath, env, grokProbeTimeout, 128*1024)
	if err != nil {
		return grokCapability{}, grokError(GrokErrUnsupportedCapability, "help probe failed", err)
	}
	if helpResult.ExitCode != 0 {
		return grokCapability{}, grokError(GrokErrUnsupportedCapability, fmt.Sprintf("help probe exited with code %d", helpResult.ExitCode), nil)
	}
	help := helpResult.Stdout + "\n" + helpResult.Stderr
	required := []string{"-p", "--cwd", "--output-format", "--no-auto-update", "--no-alt-screen"}
	if inv.ReadOnly {
		required = append(required, "--sandbox", "--deny", "--disallowed-tools")
	} else {
		required = append(required, "--always-approve")
	}
	var missing []string
	for _, flag := range required {
		if !strings.Contains(help, flag) {
			missing = append(missing, flag)
		}
	}
	if len(missing) > 0 {
		return grokCapability{}, grokError(GrokErrUnsupportedCapability, "installed Grok help is missing required flags: "+strings.Join(missing, ", "), nil)
	}
	return grokCapability{Version: version}, nil
}

func runGrokProbeCommand(ctx context.Context, argv []string, cwd string, env []string, timeout time.Duration, outputCap int64) (grokProbeResult, error) {
	if len(argv) == 0 {
		return grokProbeResult{}, errors.New("empty argv")
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = env
	var stdout, stderr cappedProbeBuffer
	stdout.cap = outputCap
	stderr.cap = outputCap
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := grokProbeResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if probeCtx.Err() != nil {
		return result, probeCtx.Err()
	}
	if stdout.overflow || stderr.overflow {
		return result, grokError(GrokErrOutputFlood, "probe output exceeded cap", nil)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return result, nil
		}
		return result, err
	}
	return result, nil
}

type cappedProbeBuffer struct {
	buf      bytes.Buffer
	cap      int64
	overflow bool
}

func (b *cappedProbeBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	if b.cap <= 0 {
		return originalLen, nil
	}
	remaining := b.cap - int64(b.buf.Len())
	if remaining <= 0 {
		b.overflow = true
		return originalLen, nil
	}
	if int64(len(p)) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	redacted, _ := redactGrokOutput(string(p))
	_, _ = b.buf.WriteString(redacted)
	return originalLen, nil
}

func (b *cappedProbeBuffer) String() string {
	return b.buf.String()
}

type grokStreamSink struct {
	mu          sync.Mutex
	log         io.Writer
	maxLogBytes int
	maxFrame    int
	written     int
	stdoutLine  []byte
	stderrLine  []byte
	firstErr    error
	terminal    bool
	summaryText boundedString
	meta        grokMetadata
}

type grokMetadata struct {
	invocationMetadata
	AdapterVersion     string
	ExternalSessionRef string
}

type boundedString struct {
	value string
	limit int
}

func (b *boundedString) append(value string) {
	value, _ = redactGrokOutput(value)
	if b.limit <= 0 {
		b.limit = grokMaxSummaryBytes
	}
	if len(b.value) >= b.limit {
		return
	}
	remaining := b.limit - len(b.value)
	if len(value) > remaining {
		value = value[:remaining]
	}
	b.value += value
}

func newGrokStreamSink(log io.Writer, maxLogBytes, maxFrameBytes int) *grokStreamSink {
	return &grokStreamSink{
		log:         log,
		maxLogBytes: maxLogBytes,
		maxFrame:    maxFrameBytes,
		summaryText: boundedString{limit: grokMaxSummaryBytes},
	}
}

func (s *grokStreamSink) stdoutWriter() io.Writer {
	return grokStreamWriter{sink: s, stream: "stdout"}
}

func (s *grokStreamSink) stderrWriter() io.Writer {
	return grokStreamWriter{sink: s, stream: "stderr"}
}

func (s *grokStreamSink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLineLocked("stdout", true)
	s.flushLineLocked("stderr", true)
}

func (s *grokStreamSink) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr
}

func (s *grokStreamSink) terminalSeen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal
}

func (s *grokStreamSink) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.summaryText.value)
}

func (s *grokStreamSink) metadata(inv Invocation) grokMetadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	metadata := s.meta
	if metadata.Model == "" {
		metadata.Model = strings.TrimSpace(inv.Model)
	}
	if metadata.Effort == "" {
		metadata.Effort = strings.TrimSpace(inv.Effort)
	}
	return metadata
}

type grokStreamWriter struct {
	sink   *grokStreamSink
	stream string
}

func (w grokStreamWriter) Write(p []byte) (int, error) {
	w.sink.write(w.stream, p)
	return len(p), nil
}

func (s *grokStreamSink) write(stream string, p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(p) > 0 {
		index := bytes.IndexByte(p, '\n')
		if index < 0 {
			s.appendPartialLocked(stream, p)
			return
		}
		s.appendPartialLocked(stream, p[:index])
		s.flushLineLocked(stream, false)
		p = p[index+1:]
	}
}

func (s *grokStreamSink) appendPartialLocked(stream string, p []byte) {
	target := &s.stdoutLine
	if stream == "stderr" {
		target = &s.stderrLine
	}
	*target = append(*target, p...)
	if len(*target) > s.maxFrame {
		s.setErrLocked(grokError(GrokErrOutputFlood, fmt.Sprintf("%s frame exceeded %d bytes", stream, s.maxFrame), nil))
		*target = (*target)[:s.maxFrame]
	}
}

func (s *grokStreamSink) flushLineLocked(stream string, final bool) {
	target := &s.stdoutLine
	if stream == "stderr" {
		target = &s.stderrLine
	}
	line := bytes.TrimSpace(*target)
	*target = nil
	if len(line) == 0 {
		return
	}
	if stream == "stderr" {
		redacted, truncated := redactGrokOutput(string(line))
		if truncated {
			s.setErrLocked(grokError(GrokErrOutputFlood, "stderr diagnostic exceeded frame cap after redaction", nil))
		}
		s.writeRecordLocked(grokNormalizedRecord{Kind: "diagnostic", Stream: "stderr", Message: redacted})
		return
	}
	if len(line) > s.maxFrame {
		s.setErrLocked(grokError(GrokErrOutputFlood, fmt.Sprintf("stdout frame exceeded %d bytes", s.maxFrame), nil))
		line = line[:s.maxFrame]
	}
	if err := s.handleJSONLineLocked(line); err != nil {
		if final {
			s.setErrLocked(grokError(GrokErrMalformedFrame, "unterminated stdout frame", err))
		} else {
			s.setErrLocked(err)
		}
	}
}

func (s *grokStreamSink) handleJSONLineLocked(line []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		redacted, _ := redactGrokOutput(string(line))
		s.writeRecordLocked(grokNormalizedRecord{Kind: "malformed", Stream: "stdout", Message: redacted})
		return grokError(GrokErrMalformedFrame, "invalid streaming-json frame", err)
	}
	record := normalizeGrokFrame(payload)
	if record.SessionRef != "" {
		s.meta.ExternalSessionRef = record.SessionRef
	}
	if record.Model != "" {
		s.meta.Model = record.Model
	}
	if record.Effort != "" {
		s.meta.Effort = record.Effort
	}
	mergeUsage(&s.meta.Usage, record.Usage)
	if record.CostUSD != nil || record.Cost != "" {
		// Cost has no reporter field in the current schema; keep it in the
		// bounded normalized stream only when the provider supplied it.
	}
	if record.Text != "" {
		s.summaryText.append(record.Text)
	}
	if record.StructuredOutput != nil {
		s.summaryText.value = string(record.StructuredOutput)
	}
	if record.Kind == "terminal" {
		s.terminal = true
	}
	if record.ErrorCode != "" || record.ErrorMessage != "" {
		s.setErrLocked(grokError(GrokErrProviderError, firstNonEmptyGrok(record.ErrorMessage, record.ErrorCode), nil))
	}
	s.writeRecordLocked(record)
	return nil
}

func (s *grokStreamSink) writeRecord(record grokNormalizedRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeRecordLocked(record)
}

func (s *grokStreamSink) writeRecordLocked(record grokNormalizedRecord) {
	data, err := json.Marshal(record)
	if err != nil {
		s.setErrLocked(err)
		return
	}
	data = append(data, '\n')
	if s.written+len(data) > s.maxLogBytes {
		s.setErrLocked(grokError(GrokErrOutputFlood, fmt.Sprintf("normalized log exceeded %d bytes", s.maxLogBytes), nil))
		return
	}
	if _, err := s.log.Write(data); err != nil {
		s.setErrLocked(err)
		return
	}
	s.written += len(data)
}

func (s *grokStreamSink) setErrLocked(err error) {
	if err != nil && s.firstErr == nil {
		s.firstErr = err
	}
}

type grokNormalizedRecord struct {
	Kind             string          `json:"kind"`
	Provider         string          `json:"provider,omitempty"`
	AdapterVersion   string          `json:"adapter_version,omitempty"`
	Attempt          string          `json:"attempt,omitempty"`
	PermissionMode   string          `json:"permission_mode,omitempty"`
	Workspace        string          `json:"workspace,omitempty"`
	Stream           string          `json:"stream,omitempty"`
	Message          string          `json:"message,omitempty"`
	Text             string          `json:"text,omitempty"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Model            string          `json:"model,omitempty"`
	Effort           string          `json:"effort,omitempty"`
	SessionRef       string          `json:"external_session_ref,omitempty"`
	ErrorCode        string          `json:"error_code,omitempty"`
	ErrorMessage     string          `json:"error_message,omitempty"`
	Usage            reporter.Usage  `json:"usage,omitempty"`
	CostUSD          *float64        `json:"cost_usd,omitempty"`
	Cost             string          `json:"cost,omitempty"`
}

func normalizeGrokFrame(payload map[string]any) grokNormalizedRecord {
	kind := strings.ToLower(firstStringValue(payload, "type", "event", "kind"))
	record := grokNormalizedRecord{
		Kind:       normalizeGrokKind(kind),
		Provider:   "grok",
		Model:      firstStringValue(payload, "model", "model_id", "modelId", "modelName"),
		Effort:     firstStringValue(payload, "effort", "reasoning_effort", "reasoningEffort"),
		SessionRef: firstStringValue(payload, "session_id", "sessionId", "session", "conversation_id", "conversationId"),
		Usage:      findGrokUsage(payload),
	}
	record.Text = firstStringValue(payload, "result", "response", "summary", "text", "message")
	if record.Text == "" {
		record.Text = nestedText(payload["content"])
	}
	if record.Text == "" {
		record.Text = nestedText(payload["delta"])
	}
	if rawStructured, ok := payload["structured_output"]; ok {
		record.StructuredOutput = compactJSONValue(rawStructured)
	}
	if record.StructuredOutput == nil {
		if rawStructured, ok := payload["structuredOutput"]; ok {
			record.StructuredOutput = compactJSONValue(rawStructured)
		}
	}
	if errorValue, ok := payload["error"]; ok {
		record.ErrorCode, record.ErrorMessage = grokErrorFields(errorValue)
	}
	if record.ErrorMessage == "" {
		record.ErrorMessage = firstStringValue(payload, "error_message", "errorMessage")
	}
	if record.ErrorCode == "" {
		record.ErrorCode = firstStringValue(payload, "error_code", "errorCode")
	}
	record.CostUSD = firstFloat64Value(payload, "cost_usd", "costUsd", "costUSD")
	record.Cost = firstStringValue(payload, "cost")
	return sanitizeGrokRecord(record)
}

func normalizeGrokKind(kind string) string {
	switch kind {
	case "result", "final", "done", "completed", "completion", "terminal":
		return "terminal"
	case "error":
		return "terminal"
	case "progress", "assistant", "message", "delta", "session/update":
		return "progress"
	case "system", "start", "started":
		return "progress"
	default:
		if strings.TrimSpace(kind) == "" {
			return "progress"
		}
		return "progress"
	}
}

func sanitizeGrokRecord(record grokNormalizedRecord) grokNormalizedRecord {
	record.Message, _ = redactGrokOutput(record.Message)
	record.Text, _ = redactGrokOutput(record.Text)
	record.ErrorMessage, _ = redactGrokOutput(record.ErrorMessage)
	if len(record.Text) > grokMaxSummaryBytes {
		record.Text = record.Text[:grokMaxSummaryBytes]
	}
	if len(record.Message) > grokMaxFrameBytes {
		record.Message = record.Message[:grokMaxFrameBytes]
	}
	return record
}

func findGrokUsage(root map[string]any) reporter.Usage {
	var usage reporter.Usage
	for _, candidate := range []map[string]any{root, mapValue(root, "usage"), mapValue(root, "token_usage"), mapValue(root, "tokenUsage")} {
		if candidate == nil {
			continue
		}
		mergeUsage(&usage, usageFromMap(candidate))
	}
	return usage
}

func nestedText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		if text := firstStringValue(typed, "text", "content", "message"); text != "" {
			return text
		}
	case []any:
		var b strings.Builder
		for _, item := range typed {
			if text := nestedText(item); text != "" {
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

func compactJSONValue(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, data); err != nil {
		return nil
	}
	redacted, _ := redactGrokOutput(compacted.String())
	return json.RawMessage(redacted)
}

func grokErrorFields(value any) (string, string) {
	switch typed := value.(type) {
	case string:
		return "", strings.TrimSpace(typed)
	case map[string]any:
		return firstStringValue(typed, "code", "type"), firstStringValue(typed, "message", "error", "detail")
	default:
		return "", fmt.Sprint(typed)
	}
}

func firstFloat64Value(values map[string]any, keys ...string) *float64 {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case float64:
			return &typed
		case json.Number:
			if parsed, err := typed.Float64(); err == nil {
				return &parsed
			}
		case string:
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64); err == nil {
				return &parsed
			}
		}
	}
	return nil
}

func firstNonEmptyGrok(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func grokBoundedEnv(environ []string, inv Invocation) []string {
	allowed := map[string]bool{
		"ALLUSERSPROFILE":   true,
		"APPDATA":           true,
		"CI":                true,
		"ComSpec":           true,
		"HOME":              true,
		"LANG":              true,
		"LC_ALL":            true,
		"LOCALAPPDATA":      true,
		"PATH":              true,
		"PATHEXT":           true,
		"ProgramData":       true,
		"ProgramFiles":      true,
		"ProgramFiles(x86)": true,
		"PUBLIC":            true,
		"SystemDrive":       true,
		"SystemRoot":        true,
		"TEMP":              true,
		"TMP":               true,
		"TMPDIR":            true,
		"USERPROFILE":       true,
		"XAI_API_KEY":       true,
		"windir":            true,
	}
	values := map[string]string{}
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !allowed[key] {
			continue
		}
		values[key] = value
	}
	values["CI"] = "1"
	values["NO_COLOR"] = "1"
	values["GROK_TELEMETRY_ENABLED"] = "0"
	values["GROK_TELEMETRY_TRACE_UPLOAD"] = "0"
	if strings.TrimSpace(inv.RunID) != "" {
		values["LOOPCODER_RUN_ID"] = strings.TrimSpace(inv.RunID)
	}
	if strings.TrimSpace(inv.Role) != "" {
		values["LOOPCODER_ROLE"] = strings.TrimSpace(inv.Role)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func grokPermissionMode(readOnly bool) string {
	if readOnly {
		return "read-only"
	}
	return "write"
}

func grokSupervisionError(result supervisedexec.Result, runErr error, ctx context.Context) error {
	if runErr != nil {
		if isParentContextCancellation(ctx, runErr) {
			return grokError(GrokErrCanceled, "parent context canceled provider process tree", runErr)
		}
		return runErr
	}
	switch result.Outcome {
	case supervisedexec.OutcomeStalled:
		return grokError(GrokErrTimeout, "provider process tree stalled", nil)
	case supervisedexec.OutcomeDeadline:
		return grokError(GrokErrTimeout, "provider process tree exceeded deadline", nil)
	default:
		return nil
	}
}

func grokError(code GrokErrorCode, message string, err error) *GrokError {
	return &GrokError{Code: code, Message: message, Err: err}
}

func parseGrokVersion(output string) string {
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if parts := firstVersionPartsLocal(line); len(parts) > 0 {
			return strings.TrimSpace(versionPatternLocal.FindString(line))
		}
	}
	return ""
}

func grokVersionLess(value string, minimum []int) bool {
	parts := firstVersionPartsLocal(value)
	if len(parts) == 0 {
		return false
	}
	for len(parts) < len(minimum) {
		parts = append(parts, 0)
	}
	for index, min := range minimum {
		if parts[index] < min {
			return true
		}
		if parts[index] > min {
			return false
		}
	}
	return false
}

func firstVersionPartsLocal(value string) []int {
	match := versionPatternLocal.FindString(value)
	if match == "" {
		return nil
	}
	var parts []int
	for _, piece := range strings.Split(match, ".") {
		n, err := strconv.Atoi(piece)
		if err != nil {
			return nil
		}
		parts = append(parts, n)
	}
	return parts
}

var versionPatternLocal = regexp.MustCompile(`\d+(?:\.\d+){0,3}`)

var grokSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|authorization)\s*[:=]\s*["']?[^"'\s,}]+`),
	regexp.MustCompile(`(?i)bearer\s+[a-z0-9._~+/=-]+`),
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`xai-[A-Za-z0-9._-]{12,}`),
}

func redactGrokOutput(value string) (string, bool) {
	redacted := value
	for _, pattern := range grokSecretPatterns {
		redacted = pattern.ReplaceAllStringFunc(redacted, func(match string) string {
			if strings.Contains(match, "=") {
				key, _, _ := strings.Cut(match, "=")
				return key + "=[REDACTED]"
			}
			if strings.Contains(match, ":") && !strings.HasPrefix(strings.ToLower(match), "bearer ") {
				key, _, _ := strings.Cut(match, ":")
				return key + ":[REDACTED]"
			}
			if strings.HasPrefix(strings.ToLower(match), "bearer ") {
				return "Bearer [REDACTED]"
			}
			return "[REDACTED]"
		})
	}
	if len(redacted) > grokMaxFrameBytes {
		return redacted[:grokMaxFrameBytes], true
	}
	return redacted, false
}
