// Package conductorhooks is a faithful Go port of loopcoder's two Claude Code
// conductor hooks (hooks/conductor-attest.js and hooks/conductor-relay-guard.js).
//
// Both hooks fail open: on ANY error (malformed JSON, missing session id, any
// filesystem error, or a panic) they return an allow result (exit code 0). The
// only non-zero exits are the two explicit block cases in RunAttest (Stop
// without a recorded conductor attestation) and RunRelayGuard (Stop with a
// pending, un-surfaced relay attestation).
package conductorhooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Result is the outcome of running a hook.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Options injects environment and clock dependencies for testing.
type Options struct {
	Env func(key string) string // nil => os.Getenv
	Now func() time.Time        // nil => time.Now
}

const (
	maxStateFiles       = 128
	maxStateAgeMillis   = int64(7) * 24 * 60 * 60 * 1000
	maxTextField        = 4096
	maxStateRecords     = 256
	maxLedgerFiles      = 256
	maxLedgerBytes      = int64(256 * 1024)
	recentLedgerGraceMs = int64(10 * 60 * 1000)
)

// allow is the fail-open / permit result.
func allow() Result {
	return Result{ExitCode: 0, Stdout: "", Stderr: ""}
}

// block is the enforcement result.
func block(stderr string) Result {
	return Result{ExitCode: 2, Stdout: "", Stderr: stderr}
}

// envLookup resolves the effective environment lookup function.
func (o Options) envLookup() func(string) string {
	if o.Env != nil {
		return o.Env
	}
	return os.Getenv
}

// nowFn resolves the effective clock.
func (o Options) nowFn() func() time.Time {
	if o.Now != nil {
		return o.Now
	}
	return time.Now
}

// hookInput mirrors the shared JSON shape consumed by both hooks. tool_response
// is intentionally left as raw JSON because it is arbitrary nested data walked
// by collectStrings.
type hookInput struct {
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolInput      toolInput       `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	StopHookActive bool            `json:"stop_hook_active"`
}

type toolInput struct {
	Command string `json:"command"`
}

// parseInput decodes the shared hook input, applying the cwd fallback chain
// (input.cwd => env PWD => os.Getwd). Returns ok=false on malformed JSON.
func parseInput(input []byte, env func(string) string) (hookInput, bool) {
	text := input
	if len(strings.TrimSpace(string(text))) == 0 {
		text = []byte("{}")
	}
	var in hookInput
	if err := json.Unmarshal(text, &in); err != nil {
		return hookInput{}, false
	}
	if strings.TrimSpace(in.CWD) == "" {
		if pwd := env("PWD"); pwd != "" {
			in.CWD = pwd
		} else if wd, err := os.Getwd(); err == nil {
			in.CWD = wd
		}
	}
	return in, true
}

// --- event classification ---------------------------------------------------

func isToolCompleteEvent(eventName string) bool {
	return eventName == "PostToolUse" || eventName == "AfterTool"
}

func isStopEvent(eventName string) bool {
	return eventName == "Stop" || eventName == "AfterAgent"
}

// --- scope / enforcement -----------------------------------------------------

// shouldEnforce evaluates a hook's scope env var. Unknown values fall through to
// "auto": enforce only for tool-complete/stop events inside a detected
// conductor workspace.
func shouldEnforce(scopeEnv, eventName, cwd string) bool {
	scope := strings.ToLower(scopeEnv)
	if scope == "" {
		scope = "auto"
	}
	switch scope {
	case "0", "false", "off", "never":
		return false
	case "1", "true", "on", "always":
		return true
	}
	if !isToolCompleteEvent(eventName) && !isStopEvent(eventName) {
		return false
	}
	return looksLikeLoopcoderConductorWorkspace(cwd)
}

// looksLikeLoopcoderConductorWorkspace detects a loopcoder conductor workspace
// via any of the documented signals. Missing/unreadable files simply mean the
// corresponding signal is false.
func looksLikeLoopcoderConductorWorkspace(cwd string) bool {
	if fileContains(filepath.Join(cwd, "SKILL.md"), "loopcoder Conductor Playbook") {
		return true
	}
	if fileContains(filepath.Join(cwd, "AGENTS.md"), "loopcoder Codex CLI Entrypoint") {
		return true
	}
	if fileContains(filepath.Join(cwd, ".delivery.yml"), "conductor:") &&
		fileContains(filepath.Join(cwd, ".delivery.yml"), "worker:") {
		return true
	}
	// NEW signal (not in the JS): an explicit marker file whose presence alone
	// marks the workspace as a conductor workspace.
	if fi, err := os.Stat(filepath.Join(cwd, ".loopcoder", "conductor-workspace")); err == nil && !fi.IsDir() {
		return true
	}
	return false
}

func fileContains(filePath, needle string) bool {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}

// --- state directory + pruning ----------------------------------------------

// stateFilePath builds the per-session state file path and ensures its dir
// exists. baseEnv is the value of the hook-specific STATE_DIR env var; when
// empty the default under <cwd>/.loopcoder/hooks/<sub> is used.
func stateFilePath(sessionID, cwd, baseEnv, sub string) (string, error) {
	baseDir := baseEnv
	if baseDir == "" {
		root := cwd
		if root == "" {
			root = os.TempDir()
		}
		baseDir = filepath.Join(root, ".loopcoder", "hooks", sub)
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", err
	}
	hash := hashText(sessionID)[:32]
	return filepath.Join(baseDir, "session-"+hash+".json"), nil
}

type stateFileEntry struct {
	fullPath string
	mtimeMs  int64
}

// pruneStateDir deletes state files older than 7 days, then keeps only the
// newest maxStateFiles by mtime. Best-effort: read/stat/unlink errors are
// swallowed (mirrors the JS try/catch behaviour).
func pruneStateDir(dirPath string, now time.Time) {
	dirEntries, err := os.ReadDir(dirPath)
	if err != nil {
		return
	}
	var entries []stateFileEntry
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(dirPath, e.Name())
		var mtimeMs int64
		if info, statErr := os.Stat(full); statErr == nil {
			mtimeMs = info.ModTime().UnixMilli()
		}
		entries = append(entries, stateFileEntry{fullPath: full, mtimeMs: mtimeMs})
	}

	cutoff := now.UnixMilli() - maxStateAgeMillis
	for _, e := range entries {
		if e.mtimeMs < cutoff {
			safeUnlink(e.fullPath)
		}
	}

	var remaining []stateFileEntry
	for _, e := range entries {
		if e.mtimeMs >= cutoff {
			remaining = append(remaining, e)
		}
	}
	sort.SliceStable(remaining, func(i, j int) bool {
		return remaining[i].mtimeMs < remaining[j].mtimeMs
	})
	deleteCount := len(remaining) - maxStateFiles
	for i := 0; i < deleteCount; i++ {
		safeUnlink(remaining[i].fullPath)
	}
}

func safeUnlink(filePath string) {
	_ = os.Remove(filePath)
}

// readStateBytes reads a state file. Returns (nil, nil) when absent, matching
// the JS ENOENT => null contract; other read errors propagate.
func readStateBytes(statePath string) ([]byte, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// writeStateJSON serialises v with two-space indentation and a trailing newline
// (matching JSON.stringify(state, null, 2) + "\n"), mode 0600.
func writeStateJSON(statePath string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(statePath, data, 0o600)
}

// --- string / hashing helpers -----------------------------------------------

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// collectStrings walks arbitrary decoded JSON (from json.Unmarshal into any)
// collecting every string value, depth-limited to 8, exactly like the JS
// collectStrings.
func collectStrings(value any, out []string, depth int) []string {
	if depth > 8 || value == nil {
		return out
	}
	switch v := value.(type) {
	case string:
		return append(out, v)
	case []any:
		for _, item := range v {
			out = collectStrings(item, out, depth+1)
		}
		return out
	case map[string]any:
		for _, item := range v {
			out = collectStrings(item, out, depth+1)
		}
		return out
	default:
		return out
	}
}

// collectStringsFromRaw decodes raw JSON and returns all nested string values
// joined by "\n". Invalid/empty JSON yields "".
func collectStringsFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	return strings.Join(collectStrings(decoded, nil, 0), "\n")
}

func firstMatchingLine(text string, pattern *regexp.Regexp) string {
	for _, line := range splitLines(text) {
		if pattern.MatchString(line) {
			return line
		}
	}
	return ""
}

// splitLines splits on \r\n or \n, matching JS /\r?\n/.
func splitLines(text string) []string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	return strings.Split(normalized, "\n")
}

func truncate(value string, maxLength int) string {
	if len(value) > maxLength {
		return value[:maxLength]
	}
	return value
}

// isoTimestamp formats t like JavaScript's Date.prototype.toISOString():
// UTC, millisecond precision, trailing "Z" (e.g. 2026-06-28T00:00:00.000Z).
func isoTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// --- shell tokenisation ------------------------------------------------------

func isShellTool(toolName string) bool {
	switch toolName {
	case "Bash", "run_shell_command", "shell_command":
		return true
	}
	return false
}

// shellWords tokenises a command string like the JS shellWords: it honours
// single/double quotes, splits on whitespace, and emits the separators
// ; | & && as their own tokens.
func shellWords(command string) []string {
	var words []string
	var current strings.Builder
	var quote byte

	pushCurrent := func() {
		if current.Len() > 0 {
			words = append(words, current.String())
			current.Reset()
		}
	}

	for i := 0; i < len(command); i++ {
		ch := command[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				current.WriteByte(ch)
			}
			continue
		}
		if ch == '"' || ch == '\'' {
			quote = ch
			continue
		}
		if isSpace(ch) {
			pushCurrent()
			continue
		}
		if ch == ';' || ch == '|' {
			pushCurrent()
			words = append(words, string(ch))
			continue
		}
		if ch == '&' {
			pushCurrent()
			if i+1 < len(command) && command[i+1] == '&' {
				words = append(words, "&&")
				i++
			} else {
				words = append(words, "&")
			}
			continue
		}
		current.WriteByte(ch)
	}
	pushCurrent()
	return words
}

// isSpace mirrors the JS /\s/ character class for the ASCII whitespace that can
// realistically appear in a command line.
func isSpace(ch byte) bool {
	switch ch {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

var loopcoderBinRe = regexp.MustCompile(`(?i)\bLOOPCODER_BIN\b`)

// isLoopcoderToken reports whether a shell token refers to the loopcoder binary
// (via the LOOPCODER_BIN env reference or a loopcoder / loopcoder.exe basename).
func isLoopcoderToken(token string) bool {
	if loopcoderBinRe.MatchString(token) {
		return true
	}
	normalized := strings.ReplaceAll(token, "\\", "/")
	base := normalized[strings.LastIndex(normalized, "/")+1:]
	base = strings.ToLower(base)
	return base == "loopcoder" || base == "loopcoder.exe"
}

// nextSeparatorIndex returns the index of the next shell separator at/after
// start, or len(words) if none.
func nextSeparatorIndex(words []string, start int) int {
	for i := start; i < len(words); i++ {
		switch words[i] {
		case ";", "|", "&&", "||", "&":
			return i
		}
	}
	return len(words)
}

// dirOf returns the directory component of a path (filepath.Dir wrapper for
// readability at call sites).
func dirOf(p string) string {
	return filepath.Dir(p)
}

// decodeResponseObject decodes tool_response into a JSON object. It returns nil
// when the response is absent or is not a JSON object (mirroring the JS guard
// `response && typeof response === 'object'`, which excludes arrays via the
// property lookups it performs).
func decodeResponseObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	return obj
}

// truthy mirrors JavaScript truthiness for the response fields the hooks probe.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	default:
		return true
	}
}

// firstDefined returns the first present (non-nil) value among the given keys,
// mirroring the JS `a ?? b ?? c` chain used for exit codes.
func firstDefined(obj map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := obj[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

// --- exit code / numeric helpers --------------------------------------------

// numericExitIsNonZero interprets a JSON exit_code/exitCode/status value the way
// the JS Number(exitCode) !== 0 check does. It returns (present, nonZero).
func numericExitIsNonZero(v any) (present bool, nonZero bool) {
	if v == nil {
		return false, false
	}
	switch n := v.(type) {
	case float64:
		return true, n != 0
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			// Number("abc") === NaN, and NaN !== 0 is true in JS.
			return true, true
		}
		return true, f != 0
	case bool:
		// Number(true) === 1, Number(false) === 0.
		if n {
			return true, true
		}
		return true, false
	default:
		return true, true
	}
}
