// Package localcleanup applies the retention policy for gitignored loopcoder
// local state under .loopcoder.
package localcleanup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	lcdefaults "github.com/jasonhnd/loopcoder/internal/defaults"
	"github.com/jasonhnd/loopcoder/internal/process"
	"github.com/jasonhnd/loopcoder/internal/reporter"
)

const (
	KindRun              = "run"
	KindWorktreeLiveness = "worktree-liveness"
	KindRelayLedger      = "relay-ledger"
	KindAuditLog         = "audit-log"
)

const (
	defaultRunRetention              = 30 * 24 * time.Hour
	defaultMinimumRunDirectories     = 50
	defaultRelayRetention            = 30 * 24 * time.Hour
	defaultMinimumRelayLedgers       = 100
	defaultAuditRetention            = 30 * 24 * time.Hour
	defaultWorktreeLivenessRetention = 7 * 24 * time.Hour
	defaultMaxEntries                = lcdefaults.RunStatusMaxDirectoryEntries
	defaultMaxFileBytes              = lcdefaults.RunStatusMaxRecordBytes
)

type Policy struct {
	RunRetention              time.Duration
	MinimumRunDirectories     int
	RelayRetention            time.Duration
	MinimumRelayLedgers       int
	AuditRetention            time.Duration
	WorktreeLivenessRetention time.Duration
}

type Options struct {
	RepoPath     string
	Now          time.Time
	Policy       Policy
	Apply        bool
	ProcessAlive func(pid int) bool

	MaxEntries   int
	MaxFileBytes int64
}

type Action struct {
	Kind   string
	Path   string
	Reason string
}

type Result struct {
	Planned     []Action
	Removed     []Action
	Retained    []Action
	Diagnostics []string
}

func DefaultPolicy() Policy {
	return Policy{
		RunRetention:              defaultRunRetention,
		MinimumRunDirectories:     defaultMinimumRunDirectories,
		RelayRetention:            defaultRelayRetention,
		MinimumRelayLedgers:       defaultMinimumRelayLedgers,
		AuditRetention:            defaultAuditRetention,
		WorktreeLivenessRetention: defaultWorktreeLivenessRetention,
	}
}

func Plan(opts Options) (Result, error) {
	opts.Apply = false
	return Cleanup(opts)
}

func Cleanup(opts Options) (Result, error) {
	opts = normalizeOptions(opts)
	loopRoot := filepath.Join(opts.RepoPath, ".loopcoder")
	result := Result{}

	pending := scanPendingRelayRuns(loopRoot, opts, &result)
	cleanupRuns(loopRoot, pending, opts, &result)
	cleanupWorktreeLiveness(loopRoot, opts, &result)
	cleanupRelayLedgers(loopRoot, opts, &result)
	cleanupAuditLogs(loopRoot, opts, &result)

	sortActions(result.Planned)
	sortActions(result.Removed)
	sortActions(result.Retained)
	sort.Strings(result.Diagnostics)
	return result, nil
}

func normalizeOptions(opts Options) Options {
	opts.RepoPath = strings.TrimSpace(opts.RepoPath)
	if opts.RepoPath == "" {
		opts.RepoPath = "."
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	} else {
		opts.Now = opts.Now.UTC()
	}
	if opts.Policy == (Policy{}) {
		opts.Policy = DefaultPolicy()
	} else {
		defaultPolicy := DefaultPolicy()
		if opts.Policy.RunRetention <= 0 {
			opts.Policy.RunRetention = defaultPolicy.RunRetention
		}
		if opts.Policy.RelayRetention <= 0 {
			opts.Policy.RelayRetention = defaultPolicy.RelayRetention
		}
		if opts.Policy.AuditRetention <= 0 {
			opts.Policy.AuditRetention = defaultPolicy.AuditRetention
		}
		if opts.Policy.WorktreeLivenessRetention <= 0 {
			opts.Policy.WorktreeLivenessRetention = defaultPolicy.WorktreeLivenessRetention
		}
		if opts.Policy.MinimumRunDirectories < 0 {
			opts.Policy.MinimumRunDirectories = defaultPolicy.MinimumRunDirectories
		}
		if opts.Policy.MinimumRelayLedgers < 0 {
			opts.Policy.MinimumRelayLedgers = defaultPolicy.MinimumRelayLedgers
		}
	}
	if opts.ProcessAlive == nil {
		opts.ProcessAlive = process.Alive
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = defaultMaxEntries
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = defaultMaxFileBytes
	}
	return opts
}

type pendingRelayRuns struct {
	runIDs  map[string]bool
	unknown bool
}

func scanPendingRelayRuns(loopRoot string, opts Options, result *Result) pendingRelayRuns {
	out := pendingRelayRuns{runIDs: map[string]bool{}}
	pendingRoot := filepath.Join(loopRoot, "relay", "pending")
	entries, ok := readDirLimited(pendingRoot, opts, result)
	if !ok {
		if pathExists(pendingRoot) {
			out.unknown = true
		}
		return out
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			result.addDiagnostic("%s: skipped symlink pending relay record", slash(filepath.Join(pendingRoot, entry.Name())))
			out.unknown = true
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(pendingRoot, entry.Name())
		info, err := entry.Info()
		if err != nil {
			result.addDiagnostic("%s: %v", slash(path), err)
			out.unknown = true
			continue
		}
		if !fileWithinBounds(path, info, opts, result) {
			out.unknown = true
			continue
		}
		data, err := readBounded(path, opts.MaxFileBytes)
		if err != nil {
			result.addDiagnostic("%s: %v", slash(path), err)
			out.unknown = true
			continue
		}
		runID, ok := pendingRunID(data)
		if !ok {
			result.addDiagnostic("%s: pending relay run reference could not be resolved; retaining all runs", slash(path))
			out.unknown = true
			continue
		}
		out.runIDs[runID] = true
	}
	return out
}

func pendingRunID(data []byte) (string, bool) {
	var record struct {
		RunID  string           `json:"run_id"`
		Report *reporter.Report `json:"report"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return "", false
	}
	if runID := strings.TrimSpace(record.RunID); runID != "" {
		return runID, true
	}
	if record.Report != nil {
		if workID := strings.TrimSpace(record.Report.WorkID); workID != "" {
			return workID, true
		}
	}
	return "", false
}

type runDir struct {
	id   string
	path string
	mod  time.Time
}

func cleanupRuns(loopRoot string, pending pendingRelayRuns, opts Options, result *Result) {
	runsRoot := filepath.Join(loopRoot, "runs")
	entries, ok := readDirLimited(runsRoot, opts, result)
	if !ok {
		return
	}
	runs := make([]runDir, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(runsRoot, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			result.addDiagnostic("%s: skipped symlink run directory", slash(path))
			continue
		}
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			result.addDiagnostic("%s: %v", slash(path), err)
			continue
		}
		runs = append(runs, runDir{id: entry.Name(), path: path, mod: info.ModTime().UTC()})
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].mod.Equal(runs[j].mod) {
			return runs[i].mod.After(runs[j].mod)
		}
		return runs[i].id > runs[j].id
	})
	newest := map[string]bool{}
	for i := 0; i < len(runs) && i < opts.Policy.MinimumRunDirectories; i++ {
		newest[runs[i].id] = true
	}

	cutoff := opts.Now.Add(-opts.Policy.RunRetention)
	for _, run := range runs {
		switch {
		case !run.mod.Before(cutoff):
			result.retain(KindRun, run.path, "inside run retention window")
			continue
		case newest[run.id]:
			result.retain(KindRun, run.path, "among newest retained run directories")
			continue
		case pending.unknown:
			result.retain(KindRun, run.path, "pending relay state contains an unresolved run reference")
			continue
		case pending.runIDs[run.id]:
			result.retain(KindRun, run.path, "referenced by pending relay state")
			continue
		}
		if active, reason := runIsActiveOrUnknown(run, opts, result); active {
			result.retain(KindRun, run.path, reason)
			continue
		}
		if keep, reason := runHasRecentRecovery(run, cutoff, opts, result); keep {
			result.retain(KindRun, run.path, reason)
			continue
		}
		result.remove(KindRun, run.path, "outside retention and all attempt records are terminal", true, loopRoot, opts)
	}
}

func runIsActiveOrUnknown(run runDir, opts Options, result *Result) (bool, string) {
	workersDir := filepath.Join(run.path, "workers")
	entries, ok := readDirLimited(workersDir, opts, result)
	if !ok {
		if pathExists(workersDir) {
			return true, "worker attempt records could not be scanned safely"
		}
		return true, "no terminal attempt records found"
	}
	attemptFiles := 0
	for _, entry := range entries {
		path := filepath.Join(workersDir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			result.addDiagnostic("%s: skipped symlink attempt record", slash(path))
			return true, "worker attempt records include a symlink"
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".attempt.json") {
			continue
		}
		attemptFiles++
		info, err := entry.Info()
		if err != nil {
			result.addDiagnostic("%s: %v", slash(path), err)
			return true, "worker attempt records could not be scanned safely"
		}
		if !fileWithinBounds(path, info, opts, result) {
			return true, "worker attempt records exceed cleanup bounds"
		}
		status, pid, ok := readAttemptStatus(path, opts, result)
		if !ok {
			return true, "worker attempt status could not be read"
		}
		if !terminalStatus(status) {
			return true, fmt.Sprintf("contains non-terminal attempt status %q", firstNonEmpty(status, "(missing)"))
		}
		if pid != nil && *pid > 0 && opts.ProcessAlive(*pid) {
			return true, fmt.Sprintf("recorded pid %d is still live", *pid)
		}
	}
	if attemptFiles == 0 {
		return true, "no terminal attempt records found"
	}
	return false, ""
}

func readAttemptStatus(path string, opts Options, result *Result) (string, *int, bool) {
	data, err := readBounded(path, opts.MaxFileBytes)
	if err != nil {
		result.addDiagnostic("%s: %v", slash(path), err)
		return "", nil, false
	}
	var raw struct {
		Status string          `json:"status"`
		PID    json.RawMessage `json:"pid"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		result.addDiagnostic("%s: parse attempt record: %v", slash(path), err)
		return "", nil, false
	}
	return strings.TrimSpace(raw.Status), rawIntPtr(raw.PID), true
}

func terminalStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "succeeded", "failed", "hung", "idle", "blocked", "needs-human":
		return true
	default:
		return false
	}
}

func runHasRecentRecovery(run runDir, cutoff time.Time, opts Options, result *Result) (bool, string) {
	recoveryRoot := filepath.Join(run.path, "recovery")
	if !pathExists(recoveryRoot) {
		return false, ""
	}
	keep := false
	scanned := false
	walkFiles(recoveryRoot, opts, result, func(path string, info os.FileInfo) {
		scanned = true
		if !info.ModTime().UTC().Before(cutoff) {
			keep = true
		}
	})
	if keep {
		return true, "contains recovery brief inside run retention window"
	}
	if !scanned && pathExists(recoveryRoot) {
		return true, "recovery briefs could not be scanned safely"
	}
	return false, ""
}

func cleanupWorktreeLiveness(loopRoot string, opts Options, result *Result) {
	root := filepath.Join(loopRoot, "worktree-liveness")
	cutoff := opts.Now.Add(-opts.Policy.WorktreeLivenessRetention)
	walkFiles(root, opts, result, func(path string, info os.FileInfo) {
		if info.ModTime().UTC().Before(cutoff) {
			result.remove(KindWorktreeLiveness, path, "outside worktree-liveness retention", false, loopRoot, opts)
			return
		}
		result.retain(KindWorktreeLiveness, path, "inside worktree-liveness retention")
	})
}

type ledgerFile struct {
	path string
	mod  time.Time
}

func cleanupRelayLedgers(loopRoot string, opts Options, result *Result) {
	relayRoot := filepath.Join(loopRoot, "relay")
	pendingRoot := filepath.Join(relayRoot, "pending")
	var ledgers []ledgerFile
	walkFilesSkipping(relayRoot, opts, result, func(path string, info os.FileInfo) bool {
		return samePath(path, pendingRoot)
	}, func(path string, info os.FileInfo) {
		if strings.HasSuffix(filepath.Base(path), ".attest") {
			ledgers = append(ledgers, ledgerFile{path: path, mod: info.ModTime().UTC()})
		}
	})
	sort.SliceStable(ledgers, func(i, j int) bool {
		if !ledgers[i].mod.Equal(ledgers[j].mod) {
			return ledgers[i].mod.After(ledgers[j].mod)
		}
		return ledgers[i].path > ledgers[j].path
	})
	newest := map[string]bool{}
	for i := 0; i < len(ledgers) && i < opts.Policy.MinimumRelayLedgers; i++ {
		newest[ledgers[i].path] = true
	}
	cutoff := opts.Now.Add(-opts.Policy.RelayRetention)
	for _, ledger := range ledgers {
		switch {
		case !ledger.mod.Before(cutoff):
			result.retain(KindRelayLedger, ledger.path, "inside relay ledger retention")
		case newest[ledger.path]:
			result.retain(KindRelayLedger, ledger.path, "among newest retained relay ledgers")
		default:
			result.remove(KindRelayLedger, ledger.path, "acknowledged relay ledger outside retention", false, loopRoot, opts)
		}
	}
}

type auditLog struct {
	path string
	name string
	mod  time.Time
}

func cleanupAuditLogs(loopRoot string, opts Options, result *Result) {
	auditRoot := filepath.Join(loopRoot, "audit")
	entries, ok := readDirLimited(auditRoot, opts, result)
	if !ok {
		return
	}
	logs := make([]auditLog, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(auditRoot, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			result.addDiagnostic("%s: skipped symlink audit log", slash(path))
			continue
		}
		if entry.IsDir() || !isAuditLLMLog(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			result.addDiagnostic("%s: %v", slash(path), err)
			continue
		}
		logs = append(logs, auditLog{path: path, name: entry.Name(), mod: info.ModTime().UTC()})
	}
	refs, incomplete := auditLogReferences(loopRoot, logs, opts, result)
	cutoff := opts.Now.Add(-opts.Policy.AuditRetention)
	for _, log := range logs {
		switch {
		case !log.mod.Before(cutoff):
			result.retain(KindAuditLog, log.path, "inside audit log retention")
		case incomplete:
			result.retain(KindAuditLog, log.path, "audit output references could not be scanned safely")
		case refs[log.name]:
			result.retain(KindAuditLog, log.path, "referenced by current audit output")
		default:
			result.remove(KindAuditLog, log.path, "outside audit log retention and unreferenced", false, loopRoot, opts)
		}
	}
}

func isAuditLLMLog(name string) bool {
	return strings.HasPrefix(name, "llm-") && strings.HasSuffix(name, ".log")
}

func auditLogReferences(loopRoot string, logs []auditLog, opts Options, result *Result) (map[string]bool, bool) {
	refs := map[string]bool{}
	if len(logs) == 0 {
		return refs, false
	}
	names := make([]string, 0, len(logs))
	for _, log := range logs {
		names = append(names, log.name)
	}
	incomplete := false
	diagnosticsBefore := len(result.Diagnostics)
	walkFiles(loopRoot, opts, result, func(path string, info os.FileInfo) {
		if isAuditLLMLog(filepath.Base(path)) && samePath(filepath.Dir(path), filepath.Join(loopRoot, "audit")) {
			return
		}
		if !fileWithinBounds(path, info, opts, result) {
			incomplete = true
			return
		}
		data, err := readBounded(path, opts.MaxFileBytes)
		if err != nil {
			result.addDiagnostic("%s: %v", slash(path), err)
			incomplete = true
			return
		}
		for _, name := range names {
			if bytes.Contains(data, []byte(name)) {
				refs[name] = true
			}
		}
	})
	if len(result.Diagnostics) > diagnosticsBefore {
		incomplete = true
	}
	return refs, incomplete
}

func walkFiles(root string, opts Options, result *Result, visit func(path string, info os.FileInfo)) {
	walkFilesSkipping(root, opts, result, nil, visit)
}

func walkFilesSkipping(root string, opts Options, result *Result, skipDir func(path string, info os.FileInfo) bool, visit func(path string, info os.FileInfo)) {
	if strings.TrimSpace(root) == "" || !pathExists(root) {
		return
	}
	seen := 0
	stop := false
	var walk func(string)
	walk = func(dir string) {
		if stop {
			return
		}
		entries, ok := readDirLimited(dir, opts, result)
		if !ok {
			return
		}
		for _, entry := range entries {
			if stop {
				return
			}
			path := filepath.Join(dir, entry.Name())
			seen++
			if seen > opts.MaxEntries {
				result.addDiagnostic("%s: cleanup entry limit exceeded (%d)", slash(root), opts.MaxEntries)
				stop = true
				return
			}
			if entry.Type()&os.ModeSymlink != 0 {
				result.addDiagnostic("%s: skipped symlink", slash(path))
				continue
			}
			info, err := entry.Info()
			if err != nil {
				result.addDiagnostic("%s: %v", slash(path), err)
				continue
			}
			if info.IsDir() {
				if skipDir != nil && skipDir(path, info) {
					continue
				}
				walk(path)
				continue
			}
			visit(path, info)
		}
	}
	walk(root)
}

func readDirLimited(path string, opts Options, result *Result) ([]os.DirEntry, bool) {
	info, statErr := os.Lstat(path)
	if statErr != nil {
		if !errors.Is(statErr, os.ErrNotExist) {
			result.addDiagnostic("%s: %v", slash(path), statErr)
		}
		return nil, false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		result.addDiagnostic("%s: skipped symlink directory", slash(path))
		return nil, false
	}
	if !info.IsDir() {
		result.addDiagnostic("%s: expected directory", slash(path))
		return nil, false
	}
	dir, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			result.addDiagnostic("%s: %v", slash(path), err)
		}
		return nil, false
	}
	defer dir.Close()

	entries, err := dir.ReadDir(opts.MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		result.addDiagnostic("%s: %v", slash(path), err)
		return nil, false
	}
	if len(entries) > opts.MaxEntries {
		result.addDiagnostic("%s: directory entry limit exceeded (%d > %d)", slash(path), len(entries), opts.MaxEntries)
		return nil, false
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, true
}

func fileWithinBounds(path string, info os.FileInfo, opts Options, result *Result) bool {
	if info.IsDir() {
		result.addDiagnostic("%s: expected file, found directory", slash(path))
		return false
	}
	if info.Size() > opts.MaxFileBytes {
		result.addDiagnostic("%s: file is too large (%d bytes > %d byte limit)", slash(path), info.Size(), opts.MaxFileBytes)
		return false
	}
	return true
}

func readBounded(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	limited := &io.LimitedReader{R: file, N: maxBytes + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file is too large while reading (%d bytes > %d byte limit)", len(data), maxBytes)
	}
	return data, nil
}

func (r *Result) retain(kind, path, reason string) {
	r.Retained = append(r.Retained, Action{Kind: kind, Path: path, Reason: reason})
}

func (r *Result) remove(kind, path, reason string, dir bool, loopRoot string, opts Options) {
	action := Action{Kind: kind, Path: path, Reason: reason}
	r.Planned = append(r.Planned, action)
	if !opts.Apply {
		return
	}
	var err error
	if dir {
		err = safeRemoveAll(path, loopRoot)
	} else {
		err = safeRemoveFile(path, loopRoot)
	}
	if err != nil {
		r.addDiagnostic("%s: remove %s: %v", slash(path), kind, err)
		return
	}
	r.Removed = append(r.Removed, action)
}

func (r *Result) addDiagnostic(format string, args ...any) {
	r.Diagnostics = append(r.Diagnostics, fmt.Sprintf(format, args...))
}

func safeRemoveFile(path, root string) error {
	if err := ensureWithinRoot(path, root); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove symlink")
	}
	if info.IsDir() {
		return fmt.Errorf("refusing to remove directory as file")
	}
	return os.Remove(path)
}

func safeRemoveAll(path, root string) error {
	if err := ensureWithinRoot(path, root); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to remove symlink")
	}
	if !info.IsDir() {
		return fmt.Errorf("refusing to remove non-directory as directory")
	}
	return os.RemoveAll(path)
}

func ensureWithinRoot(path, root string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing to remove path outside .loopcoder: %s", slash(path))
	}
	return nil
}

func rawIntPtr(raw json.RawMessage) *int {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err == nil {
		return &value
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		return nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(asString))
	if err != nil {
		return nil
	}
	return &parsed
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func slash(path string) string {
	return filepath.ToSlash(path)
}

func sortActions(actions []Action) {
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].Kind != actions[j].Kind {
			return actions[i].Kind < actions[j].Kind
		}
		if actions[i].Path != actions[j].Path {
			return actions[i].Path < actions[j].Path
		}
		return actions[i].Reason < actions[j].Reason
	})
}
