package migrate

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/reportquery"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestLocalStateImportsV06RunRecordsAndReports(t *testing.T) {
	repo, dbPath := initImportFixture(t)

	result, err := LocalState(context.Background(), Options{RepoPath: repo, Now: fixedImportNow}, importDeps(t))
	if err != nil {
		t.Fatalf("LocalState returned error: %v", err)
	}
	if result.ProjectID == "" || result.DatabasePath != dbPath {
		t.Fatalf("result project/db = %q %q, want registered project and %q", result.ProjectID, result.DatabasePath, dbPath)
	}
	if result.Status != statusCompletedWithWarnings {
		t.Fatalf("status = %q, want warnings for malformed JSONL", result.Status)
	}
	if result.ReportCount != 1 {
		t.Fatalf("ReportCount = %d, want 1", result.ReportCount)
	}
	if result.MalformedCount != 1 || !strings.Contains(result.Diagnostics[0].SourcePath, "events.jsonl") || result.Diagnostics[0].Line != 2 {
		t.Fatalf("diagnostics = %#v, want malformed events.jsonl line 2", result.Diagnostics)
	}

	assertDBCount(t, dbPath, `SELECT COUNT(*) FROM reports WHERE project_id = ?`, result.ProjectID, 1)
	assertDBCount(t, dbPath, `SELECT COUNT(*) FROM legacy_import_status WHERE project_id = ? AND status = ?`, result.ProjectID, statusCompletedWithWarnings, 1)
	assertDBCount(t, dbPath, `SELECT COUNT(*) FROM legacy_import_records WHERE project_id = ? AND record_type = 'event'`, result.ProjectID, 1)
	assertDBCount(t, dbPath, `SELECT COUNT(*) FROM legacy_import_records WHERE project_id = ? AND record_type = 'recovery_brief'`, result.ProjectID, 1)
}

func TestLocalStateImportIsIdempotent(t *testing.T) {
	repo, dbPath := initImportFixture(t)

	first, err := LocalState(context.Background(), Options{RepoPath: repo, Now: fixedImportNow}, importDeps(t))
	if err != nil {
		t.Fatalf("LocalState first returned error: %v", err)
	}
	second, err := LocalState(context.Background(), Options{RepoPath: repo, Now: fixedImportNow}, importDeps(t))
	if err != nil {
		t.Fatalf("LocalState second returned error: %v", err)
	}
	if second.ImportedCount != 0 {
		t.Fatalf("second ImportedCount = %d, want 0; first=%#v second=%#v", second.ImportedCount, first, second)
	}
	if second.SkippedCount == 0 {
		t.Fatalf("second SkippedCount = 0, want existing records skipped")
	}
	assertDBCount(t, dbPath, `SELECT COUNT(*) FROM reports WHERE project_id = ?`, first.ProjectID, 1)
	assertDBCount(t, dbPath, `SELECT COUNT(*) FROM legacy_import_status WHERE project_id = ?`, first.ProjectID, 1)
}

func TestLocalStateDryRunReportsCandidatesWithoutWritingStorage(t *testing.T) {
	repo, dbPath := initImportFixture(t)

	result, err := LocalState(context.Background(), Options{RepoPath: repo, DryRun: true, Now: fixedImportNow}, importDeps(t))
	if err != nil {
		t.Fatalf("LocalState dry run returned error: %v", err)
	}
	if !result.DryRun {
		t.Fatalf("DryRun = false, want true")
	}
	if result.Status != statusDryRunWithWarnings {
		t.Fatalf("Status = %q, want dry-run warnings for malformed JSONL", result.Status)
	}
	if result.ProjectID != "" {
		t.Fatalf("ProjectID = %q, want empty because dry run does not register the project", result.ProjectID)
	}
	if result.DatabasePath != dbPath {
		t.Fatalf("DatabasePath = %q, want %q", result.DatabasePath, dbPath)
	}
	if result.ScannedCount == 0 || result.ImportedCount == 0 || result.ReportCount != 1 || result.MalformedCount != 1 {
		t.Fatalf("dry-run counts = scanned %d imported %d reports %d malformed %d, want candidates and malformed record reported", result.ScannedCount, result.ImportedCount, result.ReportCount, result.MalformedCount)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("dry run created or touched database path %s: %v", dbPath, err)
	}
}

func TestImportedReportsRemainQueryableWhenRepoLocalFilesAreAbsent(t *testing.T) {
	repo, _ := initImportFixture(t)
	result, err := LocalState(context.Background(), Options{RepoPath: repo, Now: fixedImportNow}, importDeps(t))
	if err != nil {
		t.Fatalf("LocalState returned error: %v", err)
	}
	if err := os.Rename(filepath.Join(repo, ".loopcoder"), filepath.Join(repo, ".loopcoder.bak")); err != nil {
		t.Fatalf("rename .loopcoder away: %v", err)
	}

	records, err := reportquery.List(reportquery.Options{RepoPath: repo, Limit: 10})
	if err != nil {
		t.Fatalf("reportquery.List returned error: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1 imported report: %#v", len(records), records)
	}
	if records[0].Source != "imported:attempt" || records[0].RunID != "run-20260710T000000Z-issue-642" || records[0].Report.Issue != 642 {
		t.Fatalf("imported record = %#v, project=%s", records[0], result.ProjectID)
	}
}

func initImportFixture(t *testing.T) (repo string, dbPath string) {
	t.Helper()
	repo = t.TempDir()
	homeDir := filepath.Join(t.TempDir(), "home")
	t.Setenv(home.EnvHome, homeDir)
	dbPath = home.New(homeDir).DatabasePath()

	total := int64(123)
	report := reporter.Report{
		WorkID:      "run-20260710T000000Z-issue-642",
		Issue:       642,
		Branch:      "loop/issue-642",
		Round:       1,
		Role:        reporter.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-5",
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionWrite,
		Action:      "implement issue #642",
		ExitCode:    0,
		StartedAt:   "2026-07-10T00:00:00Z",
		EndedAt:     "2026-07-10T00:01:00Z",
		DurationMS:  60000,
		Usage: reporter.Usage{
			TotalTokens: &total,
		},
		Verified: true,
	}
	runID := "run-20260710T000000Z-issue-642"
	if _, err := state.WriteAttempt(repo, runID, state.AttemptRecord{
		Version:        1,
		JobID:          "job-642-1",
		Issue:          642,
		Attempt:        1,
		Provider:       "codex",
		PID:            4242,
		Phase:          "done",
		Status:         state.StatusSucceeded,
		Branch:         "loop/issue-642",
		StartedAt:      "2026-07-10T00:00:00Z",
		HeartbeatAt:    "2026-07-10T00:01:00Z",
		LastProgressAt: "2026-07-10T00:01:00Z",
		LogBytes:       1024,
		Report:         &report,
	}); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	if err := state.AppendEvent(repo, runID, state.Event{
		Timestamp: "2026-07-10T00:00:01Z",
		RunID:     runID,
		JobID:     "job-642-1",
		Issue:     642,
		Phase:     "running",
		Status:    state.StatusRunning,
		Event:     "phase",
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	eventsPath := state.EventsPath(repo, runID)
	file, err := os.OpenFile(eventsPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open events for malformed append: %v", err)
	}
	if _, err := file.WriteString("{malformed-jsonl\n"); err != nil {
		_ = file.Close()
		t.Fatalf("append malformed event: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close events: %v", err)
	}
	if err := os.MkdirAll(state.RecoveryPath(repo, runID), 0o755); err != nil {
		t.Fatalf("mkdir recovery: %v", err)
	}
	if err := os.WriteFile(state.RecoveryBriefPath(repo, runID, "job-642-1"), []byte("brief"), 0o644); err != nil {
		t.Fatalf("write recovery brief: %v", err)
	}
	return repo, dbPath
}

func importDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Getenv:      os.Getenv,
		UserHomeDir: os.UserHomeDir,
		Register:    DefaultDeps().Register,
		OpenStore:   storage.Open,
	}
}

func assertDBCount(t *testing.T, dbPath, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if count != 1 {
		t.Fatalf("count for %q = %d, want 1", query, count)
	}
}

func fixedImportNow() time.Time {
	return time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC)
}
