package conductorhooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	relayScopeEnv    = "LOOPCODER_RELAY_GUARD_SCOPE"
	relayStateDirEnv = "LOOPCODER_RELAY_GUARD_STATE_DIR"
	relayStateSub    = "conductor-relay-guard"
)

var (
	ledgerHeaderRe     = regexp.MustCompile(`(?m)^\[attestation\]\s+role=(worker|verifier)\b`)
	trailingNewlinesRe = regexp.MustCompile(`(?:\r?\n)*$`)
	verifiedTrueJSONRe = regexp.MustCompile(`"verified"\s*:\s*true`)
)

// relayState is the persisted relay-guard state (JSON object with a records map
// keyed by record id).
type relayState struct {
	Version       int                     `json:"version"`
	SessionIDHash string                  `json:"session_id_hash"`
	CreatedAt     string                  `json:"created_at"`
	UpdatedAt     string                  `json:"updated_at"`
	Records       map[string]*relayRecord `json:"records"`
}

// relayRecord tracks one discovered ledger attestation and its surfacing status.
type relayRecord struct {
	ID         string `json:"id"`
	Role       string `json:"role"`
	Command    string `json:"command"`
	Status     string `json:"status"`
	LedgerPath string `json:"ledger_path"`
	Header     string `json:"header"`
	RecordedAt string `json:"recorded_at"`
	UpdatedAt  string `json:"updated_at"`
}

// ledgerRecord is a discovered *.attest ledger entry parsed from disk.
type ledgerRecord struct {
	id      string
	path    string
	mtimeMs int64
	command string
	role    string
	header  string
	block   string
}

// relayTarget describes the enforced role/kind derived from a relay command.
type relayTarget struct {
	kind string
	role string
}

// RunRelayGuard is the Go port of conductor-relay-guard.js runHook. It fails
// open on any error or panic (see the deferred recover).
func RunRelayGuard(input []byte, opts Options) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = allow()
		}
	}()

	env := opts.envLookup()
	now := opts.nowFn()

	in, ok := parseInput(input, env)
	if !ok {
		return allow()
	}

	if in.SessionID == "" || !shouldEnforce(env(relayScopeEnv), in.HookEventName, in.CWD) {
		return allow()
	}

	statePath, err := stateFilePath(in.SessionID, in.CWD, env(relayStateDirEnv), relayStateSub)
	if err != nil {
		return allow()
	}
	pruneStateDir(dirOf(statePath), now())

	if isToolCompleteEvent(in.HookEventName) {
		return handleToolComplete(in, statePath, now())
	}
	if isStopEvent(in.HookEventName) {
		// Honor Claude Code's stop-loop escape valve as a safety net; the guard
		// also self-clears each record after printing it once.
		if in.StopHookActive {
			return allow()
		}
		return handleStop(statePath, now())
	}
	return allow()
}

// handleToolComplete records newly discovered relay ledger attestations and
// flags whether each was surfaced in the visible command output.
func handleToolComplete(in hookInput, statePath string, now time.Time) Result {
	if !isShellTool(in.ToolName) {
		return allow()
	}

	enforced := relayCommand(in.ToolInput.Command)
	if enforced == nil {
		return allow()
	}

	state, err := readRelayState(statePath, in.SessionID)
	if err != nil {
		return allow()
	}

	responseText := collectStringsFromRaw(in.ToolResponse)
	seenAt := isoTimestamp(now)
	stateChanged := markSurfacedPendingRecords(state, enforced, responseText, seenAt)

	cutoffMs := now.UnixMilli() - recentLedgerGraceMs
	discovered, err := discoverLedgerRecords(in.CWD)
	if err != nil {
		return allow()
	}

	var records []ledgerRecord
	for _, rec := range discovered {
		if rec.mtimeMs < cutoffMs {
			continue
		}
		if rec.role != enforced.role || !recordCommandMatches(enforced.kind, rec) {
			continue
		}
		if _, exists := state.Records[rec.id]; exists {
			continue
		}
		records = append(records, rec)
	}

	if len(records) == 0 && !stateChanged {
		return allow()
	}

	for _, rec := range records {
		command := rec.command
		if command == "" {
			command = enforced.kind
		}
		status := "pending"
		if containsSurfacedAttestation(responseText, rec) {
			status = "surfaced"
		}
		state.Records[rec.id] = &relayRecord{
			ID:         rec.id,
			Role:       rec.role,
			Command:    command,
			Status:     status,
			LedgerPath: rec.path,
			Header:     rec.header,
			RecordedAt: seenAt,
			UpdatedAt:  seenAt,
		}
	}
	state.UpdatedAt = seenAt
	trimStateRecords(state)
	if err := writeStateJSON(statePath, state); err != nil {
		return allow()
	}
	return allow()
}

func markSurfacedPendingRecords(state *relayState, enforced *relayTarget, responseText, seenAt string) bool {
	if responseText == "" {
		return false
	}
	changed := false
	for _, rec := range state.Records {
		if rec == nil || rec.Status != "pending" || rec.Role != enforced.role {
			continue
		}
		if rec.Command != "" && rec.Command != enforced.kind {
			continue
		}
		if !containsSurfacedAttestation(responseText, ledgerRecord{role: rec.Role, header: rec.Header}) {
			continue
		}
		rec.Status = "surfaced"
		rec.UpdatedAt = seenAt
		changed = true
	}
	return changed
}

// handleStop blocks completion when any recorded relay attestation is still
// pending (never surfaced in visible output), printing each missing ledger block
// verbatim.
func handleStop(statePath string, now time.Time) Result {
	data, err := readStateBytes(statePath)
	if err != nil {
		return allow()
	}
	if data == nil {
		return allow()
	}
	var state relayState
	if err := json.Unmarshal(data, &state); err != nil {
		return allow()
	}
	if state.Records == nil {
		return allow()
	}

	pending := pendingRecords(&state)
	if len(pending) == 0 {
		return allow()
	}

	blocks := make([]string, 0, len(pending))
	readable := make([]*relayRecord, 0, len(pending))
	skipped := make([]*relayRecord, 0)
	skipNotes := make([]string, 0)
	for _, rec := range pending {
		lr, err := readLedgerRecord(rec.LedgerPath)
		if err != nil {
			skipped = append(skipped, rec)
			skipNotes = append(skipNotes, fmt.Sprintf("- skipped unreadable relay ledger %s: %v", filepath.ToSlash(rec.LedgerPath), err))
			continue
		}
		if lr == nil {
			skipped = append(skipped, rec)
			skipNotes = append(skipNotes, fmt.Sprintf("- skipped unreadable relay ledger %s: no attestation block found", filepath.ToSlash(rec.LedgerPath)))
			continue
		}
		blocks = append(blocks, lr.block)
		readable = append(readable, rec)
	}

	surfacedAt := isoTimestamp(now)
	for _, rec := range readable {
		state.Records[rec.ID].Status = "surfaced_by_hook"
		state.Records[rec.ID].UpdatedAt = surfacedAt
	}
	for _, rec := range skipped {
		state.Records[rec.ID].Status = "skipped_unreadable_by_hook"
		state.Records[rec.ID].UpdatedAt = surfacedAt
	}
	if len(readable) > 0 || len(skipped) > 0 {
		state.UpdatedAt = surfacedAt
		trimStateRecords(&state)
		if err := writeStateJSON(statePath, &state); err != nil {
			return allow()
		}
	}

	if len(blocks) == 0 {
		return allow()
	}

	lines := []string{
		"loopcoder local verbatim attestation relay was missing from the completed command output.",
		"The missing local-only attestation block(s) are printed below:",
		"",
	}
	if len(skipNotes) > 0 {
		lines = append(lines, "Some pending relay records could not be read and were skipped:")
		lines = append(lines, skipNotes...)
		lines = append(lines, "")
	}
	lines = append(lines,
		strings.Join(blocks, "\n"),
		"Keep these attestations local-only: do not copy them into PRs, issues, comments, commits, merge artifacts, docs, or tracked files.",
	)
	return block(strings.Join(lines, "\n"))
}

// pendingRecords returns records whose status is "pending", ordered by the time
// they were first recorded (RecordedAt), with the record id as a stable
// tiebreak. RecordedAt is an ISO-8601 timestamp, so lexical order is
// chronological. This mirrors the JS hook's insertion-order iteration over its
// state object, so multiple missing ledger blocks print in first-seen order.
func pendingRecords(state *relayState) []*relayRecord {
	out := make([]*relayRecord, 0, len(state.Records))
	for _, rec := range state.Records {
		if rec != nil && rec.Status == "pending" {
			out = append(out, rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RecordedAt != out[j].RecordedAt {
			return out[i].RecordedAt < out[j].RecordedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// readRelayState reads existing relay state or returns a fresh one.
func readRelayState(statePath, sessionID string) (*relayState, error) {
	data, err := readStateBytes(statePath)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return freshRelayState(sessionID), nil
	}
	var state relayState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	if state.Records == nil {
		state.Records = map[string]*relayRecord{}
	}
	return &state, nil
}

func freshRelayState(sessionID string) *relayState {
	nowISO := isoTimestamp(time.Now())
	return &relayState{
		Version:       1,
		SessionIDHash: hashText(sessionID)[:32],
		CreatedAt:     nowISO,
		UpdatedAt:     nowISO,
		Records:       map[string]*relayRecord{},
	}
}

// trimStateRecords keeps only the newest maxStateRecords records ordered by
// updated_at (lexicographic on ISO timestamps, matching the JS localeCompare).
func trimStateRecords(state *relayState) {
	type kv struct {
		id  string
		rec *relayRecord
	}
	entries := make([]kv, 0, len(state.Records))
	for id, rec := range state.Records {
		entries = append(entries, kv{id: id, rec: rec})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return updatedAtOf(entries[i].rec) < updatedAtOf(entries[j].rec)
	})
	start := len(entries) - maxStateRecords
	if start < 0 {
		start = 0
	}
	kept := make(map[string]*relayRecord, len(entries)-start)
	for _, e := range entries[start:] {
		kept[e.id] = e.rec
	}
	state.Records = kept
}

func updatedAtOf(rec *relayRecord) string {
	if rec == nil {
		return ""
	}
	return rec.UpdatedAt
}

// discoverLedgerRecords walks <cwd>/.loopcoder/relay for *.attest files,
// newest-first, capped at maxLedgerFiles, and parses each into a ledgerRecord.
// It returns an error if any ledger file exceeds the size cap (fail-open at the
// caller).
func discoverLedgerRecords(cwd string) ([]ledgerRecord, error) {
	root := filepath.Join(cwd, ".loopcoder", "relay")
	files, err := walkLedger(root)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].mtimeMs > files[j].mtimeMs
	})
	if len(files) > maxLedgerFiles {
		files = files[:maxLedgerFiles]
	}
	records := make([]ledgerRecord, 0, len(files))
	for _, f := range files {
		lr, err := readLedgerRecord(f.path)
		if err != nil {
			return nil, err
		}
		if lr == nil {
			continue
		}
		records = append(records, *lr)
	}
	return records, nil
}

type ledgerFileEntry struct {
	path    string
	mtimeMs int64
}

// walkLedger recursively collects *.attest files under dirPath. A missing root
// yields no files; an oversized ledger file yields an error.
func walkLedger(dirPath string) ([]ledgerFileEntry, error) {
	var out []ledgerFileEntry
	var walk func(string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, entry := range entries {
			full := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if err := walk(full); err != nil {
					return err
				}
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".attest") {
				continue
			}
			info, err := os.Stat(full)
			if err != nil {
				return err
			}
			if info.Size() > maxLedgerBytes {
				return &ledgerTooLargeError{path: full}
			}
			out = append(out, ledgerFileEntry{path: full, mtimeMs: info.ModTime().UnixMilli()})
		}
		return nil
	}
	if err := walk(dirPath); err != nil {
		return nil, err
	}
	return out, nil
}

type ledgerTooLargeError struct{ path string }

func (e *ledgerTooLargeError) Error() string { return "relay ledger too large: " + e.path }

// readLedgerRecord reads and parses one ledger file. Returns (nil, nil) when the
// file has no attestation block or no worker/verifier role header. Returns an
// error when the file exceeds the size cap or cannot be read.
func readLedgerRecord(filePath string) (*ledgerRecord, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxLedgerBytes {
		return nil, &ledgerTooLargeError{path: filePath}
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	text := string(data)
	block := ledgerAttestationBlock(text)
	if block == "" {
		return nil, nil
	}
	header := firstLine(block)
	roleMatch := ledgerHeaderRe.FindStringSubmatch(header)
	if roleMatch == nil {
		return nil, nil
	}
	command := firstMetaValue(text, "command")
	abs, err := filepath.Abs(filePath)
	if err != nil {
		return nil, err
	}
	id := hashText(abs + "\x00" + header + "\x00" + block)
	return &ledgerRecord{
		id:      id,
		path:    filePath,
		mtimeMs: info.ModTime().UnixMilli(),
		command: command,
		role:    roleMatch[1],
		header:  header,
		block:   block,
	}, nil
}

// ledgerAttestationBlock returns the attestation block: from the first line
// matching the worker/verifier header to end of text, with trailing newlines
// trimmed and exactly one "\n" appended. Returns "" when no header is present.
func ledgerAttestationBlock(text string) string {
	loc := ledgerHeaderRe.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	slice := text[loc[0]:]
	slice = trailingNewlinesRe.ReplaceAllString(slice, "")
	return slice + "\n"
}

// firstMetaValue returns the trimmed value of the first `# <key>=...` meta line.
func firstMetaValue(text, key string) string {
	pattern := regexp.MustCompile(`(?m)^#\s+` + regexp.QuoteMeta(key) + `=([^\r\n]*)`)
	match := pattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// recordCommandMatches reports whether a ledger record matches the enforced
// relay kind, falling back to role-based matching when the record carries no
// command meta.
func recordCommandMatches(kind string, rec ledgerRecord) bool {
	if rec.command != "" {
		return rec.command == kind
	}
	return (kind == "dispatch" && rec.role == "worker") ||
		(kind == "dispatch-wave" && rec.role == "worker") ||
		(kind == "loopreview" && rec.role == "verifier")
}

// containsSurfacedAttestation reports whether the visible output surfaced the
// record's attestation (by header, role header, or verified role JSON).
func containsSurfacedAttestation(text string, rec ledgerRecord) bool {
	if rec.header != "" && strings.Contains(text, rec.header) {
		return true
	}
	rolePattern := regexp.MustCompile(`\[attestation\]\s+role=` + regexp.QuoteMeta(rec.role) + `\b`)
	if rolePattern.MatchString(text) {
		return true
	}
	roleJSON := regexp.MustCompile(`"role"\s*:\s*"` + regexp.QuoteMeta(rec.role) + `"`)
	return roleJSON.MatchString(text) && verifiedTrueJSONRe.MatchString(text)
}

// relayCommand classifies a command as a worker dispatch or verifier loopreview
// relay invocation, or nil when it is neither.
func relayCommand(command string) *relayTarget {
	words := shellWords(command)
	for i := 0; i < len(words)-1; i++ {
		if !isLoopcoderToken(words[i]) {
			continue
		}
		switch words[i+1] {
		case "dispatch", "dispatch-wave":
			return &relayTarget{kind: words[i+1], role: "worker"}
		case "loopreview":
			return &relayTarget{kind: "loopreview", role: "verifier"}
		}
		i = nextSeparatorIndex(words, i+1)
	}
	return nil
}

// firstLine returns the first line of s (split on \r\n or \n).
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		line := s[:idx]
		return strings.TrimSuffix(line, "\r")
	}
	return s
}
