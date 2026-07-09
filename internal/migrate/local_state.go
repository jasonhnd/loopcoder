// Package migrate imports legacy repo-local loopcoder state into v0.7 storage.
package migrate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/reportquery"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	statusCompleted             = "completed"
	statusCompletedWithWarnings = "completed-with-warnings"
	maxLegacyFileBytes          = 4 * 1024 * 1024
	maxLegacyLineBytes          = 1024 * 1024
)

// Options controls repo-local state import.
type Options struct {
	RepoPath     string
	DatabasePath string
	Now          func() time.Time
}

// Deps contains injectable migration dependencies.
type Deps struct {
	Getenv      func(string) string
	UserHomeDir func() (string, error)
	Register    func(context.Context, registry.Options, registry.Deps) (registry.RegisterResult, error)
	OpenStore   func(context.Context, storage.Options) (storage.Store, error)
}

// Result summarizes one import pass.
type Result struct {
	RepoPath        string           `json:"repo_path"`
	ProjectID       string           `json:"project_id"`
	DatabasePath    string           `json:"database_path"`
	Status          string           `json:"status"`
	ScannedCount    int              `json:"scanned_count"`
	ImportedCount   int              `json:"imported_count"`
	SkippedCount    int              `json:"skipped_count"`
	MalformedCount  int              `json:"malformed_count"`
	ReportCount     int              `json:"report_count"`
	Diagnostics     []Diagnostic     `json:"diagnostics,omitempty"`
	ImportStartedAt string           `json:"import_started_at"`
	ImportEndedAt   string           `json:"import_ended_at"`
	Project         registry.Project `json:"project"`
}

// Diagnostic reports a malformed or unreadable legacy record without failing
// the whole import.
type Diagnostic struct {
	SourcePath string `json:"source_path"`
	Line       int    `json:"line,omitempty"`
	Message    string `json:"message"`
}

type importContext struct {
	opts    Options
	now     time.Time
	repo    string
	project registry.Project
	store   storage.Store
	result  *Result
}

type legacyEvent struct {
	Timestamp string          `json:"ts"`
	Event     string          `json:"event"`
	Phase     string          `json:"phase"`
	Status    string          `json:"status"`
	Issue     int             `json:"issue"`
	Payload   json.RawMessage `json:"-"`
}

// DefaultDeps returns production migration dependencies.
func DefaultDeps() Deps {
	return Deps{
		Getenv:      os.Getenv,
		UserHomeDir: os.UserHomeDir,
		Register:    registry.Register,
		OpenStore:   storage.Open,
	}
}

// LocalState imports legacy repo-local .loopcoder state into SQLite.
func LocalState(ctx context.Context, opts Options, deps Deps) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deps = normalizeDeps(deps)
	now := normalizeNow(opts.Now)()
	repoPath := strings.TrimSpace(opts.RepoPath)
	if repoPath == "" {
		repoPath = "."
	}
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return Result{}, fmt.Errorf("migrate local state: resolve repo: %w", err)
	}
	info, err := os.Stat(absRepo)
	if err != nil {
		return Result{}, fmt.Errorf("migrate local state: inspect repo: %w", err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("migrate local state: repo path is not a directory: %s", absRepo)
	}
	absRepo = filepath.Clean(absRepo)
	dbPath, err := databasePath(opts.DatabasePath, deps)
	if err != nil {
		return Result{}, fmt.Errorf("migrate local state: resolve storage: %w", err)
	}

	reg, err := deps.Register(ctx, registry.Options{
		RepoPath:     absRepo,
		DatabasePath: dbPath,
		Now:          opts.Now,
	}, registry.DefaultDeps())
	if err != nil {
		return Result{}, fmt.Errorf("migrate local state: register project: %w", err)
	}

	store, err := deps.OpenStore(ctx, storage.Options{Path: dbPath, Now: opts.Now})
	if err != nil {
		return Result{}, fmt.Errorf("migrate local state: open storage: %w", err)
	}
	defer store.Close()

	result := Result{
		RepoPath:        absRepo,
		ProjectID:       reg.Project.ProjectID,
		DatabasePath:    store.Path(),
		Status:          statusCompleted,
		ImportStartedAt: formatTimestamp(now),
		Project:         reg.Project,
	}
	ic := importContext{
		opts:    opts,
		now:     now,
		repo:    absRepo,
		project: reg.Project,
		store:   store,
		result:  &result,
	}
	if err := ic.importRuns(ctx); err != nil {
		return Result{}, err
	}
	if err := ic.importRelayRecords(ctx); err != nil {
		return Result{}, err
	}
	if err := ic.importReports(ctx); err != nil {
		return Result{}, err
	}
	result.MalformedCount = len(result.Diagnostics)
	if result.MalformedCount > 0 {
		result.Status = statusCompletedWithWarnings
	}
	result.ImportEndedAt = formatTimestamp(normalizeNow(opts.Now)())
	if err := ic.writeStatus(ctx); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (ic importContext) importRuns(ctx context.Context) error {
	runsRoot := state.RunsRoot(ic.repo)
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("migrate local state: read runs directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		if err := ic.ensureRun(ctx, runID, 0, "", "", ""); err != nil {
			return err
		}
		runPath := state.RunPath(ic.repo, runID)
		if err := ic.importAttemptFiles(ctx, runID); err != nil {
			return err
		}
		if err := ic.importEventFile(ctx, runID); err != nil {
			return err
		}
		if err := ic.importRecoveryBriefs(ctx, runID); err != nil {
			return err
		}
		if err := ic.importGenericRunFiles(ctx, runPath, runID); err != nil {
			return err
		}
	}
	return nil
}

func (ic importContext) importAttemptFiles(ctx context.Context, runID string) error {
	workersDir := state.WorkersPath(ic.repo, runID)
	entries, err := os.ReadDir(workersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		ic.addDiagnostic(workersDir, 0, fmt.Sprintf("read workers directory: %v", err))
		return nil
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".attempt.json") {
			continue
		}
		path := filepath.Join(workersDir, entry.Name())
		data, ok := ic.readLegacyFile(path)
		if !ok {
			continue
		}
		var attempt state.Attempt
		if err := json.Unmarshal(data, &attempt); err != nil {
			ic.addDiagnostic(path, 0, fmt.Sprintf("malformed attempt JSON: %v", err))
			continue
		}
		if err := ic.ensureRun(ctx, runID, attempt.Issue, attempt.Status, attempt.StartedAt, firstNonEmpty(attempt.LastProgressAt, attempt.HeartbeatAt, attempt.StartedAt)); err != nil {
			return err
		}
		if err := ic.insertLegacyRecord(ctx, "attempt", runID, path, 0, data); err != nil {
			return err
		}
	}
	return nil
}

func (ic importContext) importEventFile(ctx context.Context, runID string) error {
	path := state.EventsPath(ic.repo, runID)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		ic.addDiagnostic(path, 0, fmt.Sprintf("read events file: %v", err))
		return nil
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), maxLegacyLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		ic.result.ScannedCount++
		var event legacyEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			ic.addDiagnostic(path, line, fmt.Sprintf("malformed event JSONL: %v", err))
			continue
		}
		event.Payload = append(event.Payload[:0], raw...)
		if err := ic.insertRunEvent(ctx, runID, line, event); err != nil {
			return err
		}
		if err := ic.insertLegacyRecordCounted(ctx, "event", runID, path, line, raw); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		ic.addDiagnostic(path, line, fmt.Sprintf("read events JSONL: %v", err))
	}
	return nil
}

func (ic importContext) importRecoveryBriefs(ctx context.Context, runID string) error {
	recoveryDir := state.RecoveryPath(ic.repo, runID)
	if info, err := os.Stat(recoveryDir); err != nil || !info.IsDir() {
		return nil
	}
	err := filepath.WalkDir(recoveryDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			ic.addDiagnostic(path, 0, err.Error())
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}
		data, ok := ic.readLegacyFile(path)
		if !ok {
			return nil
		}
		payload, err := json.Marshal(map[string]string{"text": string(data)})
		if err != nil {
			return err
		}
		return ic.insertLegacyRecord(ctx, "recovery_brief", runID, path, 0, payload)
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("migrate local state: scan recovery briefs: %w", err)
	}
	return nil
}

func (ic importContext) importGenericRunFiles(ctx context.Context, runPath, runID string) error {
	err := filepath.WalkDir(runPath, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			ic.addDiagnostic(path, 0, err.Error())
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".attempt.json") || name == "events.jsonl" || strings.HasSuffix(name, ".md") {
			return nil
		}
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if strings.HasSuffix(name, ".jsonl") {
			return ic.scanJSONLLegacy(ctx, path, runID, "run_jsonl")
		}
		data, ok := ic.readLegacyFile(path)
		if !ok {
			return nil
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			ic.addDiagnostic(path, 0, fmt.Sprintf("malformed run JSON: %v", err))
			return nil
		}
		return ic.insertLegacyRecord(ctx, "run_json", runID, path, 0, data)
	})
	if err != nil {
		return fmt.Errorf("migrate local state: scan run files: %w", err)
	}
	return nil
}

func (ic importContext) importRelayRecords(ctx context.Context) error {
	relayRoot := filepath.Join(ic.repo, ".loopcoder", "relay")
	if info, err := os.Stat(relayRoot); err != nil || !info.IsDir() {
		return nil
	}
	err := filepath.WalkDir(relayRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			ic.addDiagnostic(path, 0, err.Error())
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		switch {
		case strings.HasSuffix(name, ".jsonl"):
			return ic.scanJSONLLegacy(ctx, path, "", "relay_jsonl")
		case strings.HasSuffix(name, ".json"):
			data, ok := ic.readLegacyFile(path)
			if !ok {
				return nil
			}
			var value any
			if err := json.Unmarshal(data, &value); err != nil {
				ic.addDiagnostic(path, 0, fmt.Sprintf("malformed relay JSON: %v", err))
				return nil
			}
			return ic.insertLegacyRecord(ctx, "relay_json", "", path, 0, data)
		case strings.HasSuffix(name, ".attest"):
			data, ok := ic.readLegacyFile(path)
			if !ok {
				return nil
			}
			payload, err := json.Marshal(map[string]string{"text": string(data)})
			if err != nil {
				return err
			}
			return ic.insertLegacyRecord(ctx, "relay_ledger", "", path, 0, payload)
		default:
			return nil
		}
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("migrate local state: scan relay records: %w", err)
	}
	return nil
}

func (ic importContext) scanJSONLLegacy(ctx context.Context, path, runID, recordType string) error {
	file, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			ic.addDiagnostic(path, 0, fmt.Sprintf("read JSONL: %v", err))
		}
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), maxLegacyLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		raw := bytes.TrimSpace(scanner.Bytes())
		if len(raw) == 0 {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			ic.addDiagnostic(path, line, fmt.Sprintf("malformed JSONL: %v", err))
			continue
		}
		if err := ic.insertLegacyRecord(ctx, recordType, runID, path, line, raw); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		ic.addDiagnostic(path, line, fmt.Sprintf("read JSONL: %v", err))
	}
	return nil
}

func (ic importContext) importReports(ctx context.Context) error {
	records, err := reportquery.List(reportquery.Options{RepoPath: ic.repo, Limit: math.MaxInt, SkipImported: true})
	if err != nil {
		return fmt.Errorf("migrate local state: list reports: %w", err)
	}
	for _, record := range records {
		if err := ic.insertReport(ctx, record); err != nil {
			return err
		}
	}
	ic.result.ReportCount = len(records)
	return nil
}

func (ic importContext) ensureRun(ctx context.Context, runID string, issue int, status, startedAt, updatedAt string) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil
	}
	if strings.TrimSpace(updatedAt) == "" {
		updatedAt = formatTimestamp(ic.now)
	}
	err := ic.store.WithTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO runs(id, project_id, issue_number, status, started_at, updated_at)
			VALUES (?, ?, NULLIF(?, 0), ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				project_id = COALESCE(runs.project_id, excluded.project_id),
				issue_number = COALESCE(runs.issue_number, excluded.issue_number),
				status = CASE WHEN excluded.status <> '' THEN excluded.status ELSE runs.status END,
				started_at = COALESCE(NULLIF(runs.started_at, ''), NULLIF(excluded.started_at, '')),
				updated_at = CASE WHEN excluded.updated_at <> '' THEN excluded.updated_at ELSE runs.updated_at END`,
			runID, ic.project.ProjectID, issue, strings.TrimSpace(status), strings.TrimSpace(startedAt), strings.TrimSpace(updatedAt))
		return err
	})
	return err
}

func (ic importContext) insertRunEvent(ctx context.Context, runID string, sequence int, event legacyEvent) error {
	eventType := firstNonEmpty(event.Event, event.Phase, event.Status, "legacy")
	ts := firstNonEmpty(event.Timestamp, formatTimestamp(ic.now))
	id := stableID("legacy_event", ic.project.ProjectID, runID, fmt.Sprint(sequence), string(event.Payload))
	return ic.store.WithTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO run_events(id, run_id, sequence, ts, event_type, payload_json) VALUES (?, ?, ?, ?, ?, ?)`,
			id, runID, sequence, ts, eventType, string(event.Payload))
		return err
	})
}

func (ic importContext) insertReport(ctx context.Context, record reportquery.Record) error {
	payload, err := json.Marshal(record.Report)
	if err != nil {
		return fmt.Errorf("migrate local state: marshal report: %w", err)
	}
	if strings.TrimSpace(record.RunID) != "" {
		if err := ic.ensureRun(ctx, record.RunID, record.Report.Issue, "", record.Report.StartedAt, record.Report.EndedAt); err != nil {
			return err
		}
	}
	sourcePath := ic.sourcePath(record.Path)
	sourceHash := hashHex(payload)
	id := stableID("legacy_report", ic.project.ProjectID, sourcePath, record.RunID, record.Source, sourceHash)
	inserted := false
	err = ic.store.WithTx(ctx, func(tx storage.Tx) error {
		res, err := tx.Exec(ctx, `INSERT OR IGNORE INTO reports(id, project_id, run_id, role, provider, model, started_at, ended_at, payload_json, created_at, source_path, source_hash, source_kind)
			VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id,
			ic.project.ProjectID,
			record.RunID,
			string(record.Report.Role),
			record.Report.Provider,
			record.Report.Model,
			record.Report.StartedAt,
			record.Report.EndedAt,
			string(payload),
			formatTimestamp(ic.now),
			sourcePath,
			sourceHash,
			record.Source)
		if err != nil {
			return err
		}
		if affected, err := res.RowsAffected(); err == nil && affected > 0 {
			inserted = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("migrate local state: insert report: %w", err)
	}
	if err := ic.insertLegacyRecord(ctx, "report", record.RunID, record.Path, 0, payload); err != nil {
		return err
	}
	if inserted {
		ic.result.ImportedCount++
	} else {
		ic.result.SkippedCount++
	}
	return nil
}

func (ic importContext) insertLegacyRecord(ctx context.Context, recordType, runID, path string, line int, payload []byte) error {
	ic.result.ScannedCount++
	return ic.insertLegacyRecordCounted(ctx, recordType, runID, path, line, payload)
}

func (ic importContext) insertLegacyRecordCounted(ctx context.Context, recordType, runID, path string, line int, payload []byte) error {
	sourcePath := ic.sourcePath(path)
	sourceHash := hashHex(payload)
	id := stableID("legacy_record", ic.project.ProjectID, recordType, sourcePath, fmt.Sprint(line), sourceHash)
	inserted := false
	err := ic.store.WithTx(ctx, func(tx storage.Tx) error {
		res, err := tx.Exec(ctx, `INSERT OR IGNORE INTO legacy_import_records(id, project_id, run_id, record_type, source_path, source_line, source_hash, payload_json, imported_at)
			VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?)`,
			id, ic.project.ProjectID, runID, recordType, sourcePath, line, sourceHash, ensureJSONPayload(payload), formatTimestamp(ic.now))
		if err != nil {
			return err
		}
		if affected, err := res.RowsAffected(); err == nil && affected > 0 {
			inserted = true
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("migrate local state: insert legacy record %s: %w", recordType, err)
	}
	if inserted {
		ic.result.ImportedCount++
	} else {
		ic.result.SkippedCount++
	}
	return nil
}

func (ic importContext) writeStatus(ctx context.Context) error {
	message := "import completed"
	if ic.result.MalformedCount > 0 {
		message = fmt.Sprintf("import completed with %d malformed record(s)", ic.result.MalformedCount)
	}
	return ic.store.WithTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO legacy_import_status(project_id, repo_path, started_at, completed_at, status, scanned_count, imported_count, skipped_count, malformed_count, message)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id) DO UPDATE SET
				repo_path = excluded.repo_path,
				started_at = excluded.started_at,
				completed_at = excluded.completed_at,
				status = excluded.status,
				scanned_count = excluded.scanned_count,
				imported_count = excluded.imported_count,
				skipped_count = excluded.skipped_count,
				malformed_count = excluded.malformed_count,
				message = excluded.message`,
			ic.project.ProjectID,
			ic.repo,
			ic.result.ImportStartedAt,
			ic.result.ImportEndedAt,
			ic.result.Status,
			ic.result.ScannedCount,
			ic.result.ImportedCount,
			ic.result.SkippedCount,
			ic.result.MalformedCount,
			message)
		return err
	})
}

func (ic importContext) readLegacyFile(path string) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil {
		ic.addDiagnostic(path, 0, fmt.Sprintf("inspect file: %v", err))
		return nil, false
	}
	if info.Size() > maxLegacyFileBytes {
		ic.addDiagnostic(path, 0, fmt.Sprintf("file exceeds import limit of %d bytes", maxLegacyFileBytes))
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		ic.addDiagnostic(path, 0, fmt.Sprintf("read file: %v", err))
		return nil, false
	}
	return data, true
}

func (ic importContext) addDiagnostic(path string, line int, message string) {
	ic.result.Diagnostics = append(ic.result.Diagnostics, Diagnostic{
		SourcePath: ic.sourcePath(path),
		Line:       line,
		Message:    message,
	})
}

func (ic importContext) sourcePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		if rel, relErr := filepath.Rel(ic.repo, abs); relErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		return filepath.ToSlash(filepath.Clean(abs))
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func normalizeDeps(deps Deps) Deps {
	defaults := DefaultDeps()
	if deps.Getenv == nil {
		deps.Getenv = defaults.Getenv
	}
	if deps.UserHomeDir == nil {
		deps.UserHomeDir = defaults.UserHomeDir
	}
	if deps.Register == nil {
		deps.Register = defaults.Register
	}
	if deps.OpenStore == nil {
		deps.OpenStore = defaults.OpenStore
	}
	return deps
}

func databasePath(path string, deps Deps) (string, error) {
	if strings.TrimSpace(path) != "" {
		return filepath.Clean(path), nil
	}
	layout, err := home.Resolve(home.Deps{Getenv: deps.Getenv, UserHomeDir: deps.UserHomeDir})
	if err != nil {
		return "", err
	}
	return layout.DatabasePath(), nil
}

func normalizeNow(now func() time.Time) func() time.Time {
	if now == nil {
		return time.Now
	}
	return now
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stableID(prefix string, parts ...string) string {
	return prefix + "_" + hashStrings(parts...)[:24]
}

func hashStrings(parts ...string) string {
	sum := sha256.New()
	for _, part := range parts {
		sum.Write([]byte(part))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ensureJSONPayload(data []byte) string {
	if json.Valid(data) {
		return string(data)
	}
	wrapped, err := json.Marshal(map[string]string{"text": string(data)})
	if err != nil {
		return "{}"
	}
	return string(wrapped)
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
