package runstatus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/state"
)

func TestRenderNormalRunWithWorkerAndVerifierRecords(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test"
	writeAttempt(t, repo, runID, 101, 1, "job-101-1", workerReport(101, usageSplit(100, 50, 150)))
	writeEventLine(t, repo, runID, `{"ts":"2026-07-01T00:00:43Z","run_id":"run-test","job_id":"job-101-1","issue":101,"phase":"pr_created","status":"succeeded","pr":"https://github.com/owner/repo/pull/501"}`)
	writeVerifierRecord(t, repo, runID, map[string]any{
		"issue":       101,
		"pr":          "https://github.com/owner/repo/pull/501",
		"verdict":     "pass",
		"attestation": verifierReport(501, usageSplit(20, 10, 30)),
	})

	report, err := Load(Options{RepoPath: repo, RunID: runID})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	got := Render(report)

	for _, want := range []string{
		"RUN STATUS",
		"RunId: run-test (requested run)",
		"Events: 1",
		"Verifier records: 1",
		"Lifecycle: succeeded (source=legacy entries=1)",
		"| #101 | job-101-1 | https://github.com/owner/repo/pull/501 | codex | gpt-5.5 | parsed | xhigh | write | 42s | 100 | 50 | 150 | true | codex_exited | succeeded | pass | claude | claude-sonnet-4-5 | parsed | high | read-only | 7s | 20 | 10 | 30 | true |",
		"status is read-only and local-only",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered status missing %q:\n%s", want, got)
		}
	}
}

func TestRenderLifecycleRecordWithParentAndChild(t *testing.T) {
	repo := t.TempDir()
	runID := "run-lifecycle"
	writeAttempt(t, repo, runID, 103, 1, "job-103-1", workerReport(103, usageTotal(1030)))
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:   "2026-07-09T00:00:00Z",
		RunID:       runID,
		ParentRunID: "run-parent",
		State:       state.StatePlanned,
	}); err != nil {
		t.Fatalf("append planned lifecycle: %v", err)
	}
	if err := state.AppendLifecycleTransition(repo, state.LifecycleTransition{
		Timestamp:  "2026-07-09T00:00:01Z",
		RunID:      runID,
		State:      state.StateQueued,
		ChildRunID: "run-child",
	}); err != nil {
		t.Fatalf("append queued lifecycle: %v", err)
	}

	report, err := Load(Options{RepoPath: repo, RunID: runID})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	got := Render(report)
	for _, want := range []string{
		"Lifecycle: queued (source=lifecycle entries=2)",
		"ParentRunId: run-parent",
		"ChildRunIds: run-child",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered status missing %q:\n%s", want, got)
		}
	}
}

func TestRenderTotalOnlyTokensAsNotReportedSplit(t *testing.T) {
	repo := t.TempDir()
	runID := "run-total"
	writeAttempt(t, repo, runID, 102, 1, "job-102-1", workerReport(102, usageTotal(102585)))

	report, err := Load(Options{RepoPath: repo, RunID: runID})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	got := Render(report)

	want := "| #102 | job-102-1 | not reported | codex | gpt-5.5 | parsed | xhigh | write | 42s | not reported | not reported | 102585 | true | codex_exited | succeeded | not reported |"
	if !strings.Contains(got, want) {
		t.Fatalf("rendered status missing total-only token row %q:\n%s", want, got)
	}
}

func TestLoadMissingAndEmptyRunReturnClearErrors(t *testing.T) {
	repo := t.TempDir()
	if _, err := Load(Options{RepoPath: repo, RunID: "run-missing"}); err == nil || !strings.Contains(err.Error(), `run "run-missing" not found`) {
		t.Fatalf("missing run error = %v", err)
	}

	if err := os.MkdirAll(state.RunPath(repo, "run-empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll empty run: %v", err)
	}
	if _, err := Load(Options{RepoPath: repo, RunID: "run-empty"}); err == nil || !strings.Contains(err.Error(), `run "run-empty" has no local status records`) {
		t.Fatalf("empty run error = %v", err)
	}
}

func TestLoadWithoutRunSelectsLatestModifiedRun(t *testing.T) {
	repo := t.TempDir()
	writeAttempt(t, repo, "run-old", 201, 1, "job-201-1", workerReport(201, usageTotal(2010)))
	writeAttempt(t, repo, "run-new", 202, 1, "job-202-1", workerReport(202, usageTotal(2020)))

	oldTime := time.Date(2026, 7, 1, 1, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC)
	if err := os.Chtimes(state.RunPath(repo, "run-old"), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes old run: %v", err)
	}
	if err := os.Chtimes(state.RunPath(repo, "run-new"), newTime, newTime); err != nil {
		t.Fatalf("Chtimes new run: %v", err)
	}

	report, err := Load(Options{RepoPath: repo})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if report.RunID != "run-new" || report.RunNote != "latest modified run selected" {
		t.Fatalf("selected run = %q (%s), want run-new latest note", report.RunID, report.RunNote)
	}
	if got := Render(report); !strings.Contains(got, "| #202 | job-202-1 |") {
		t.Fatalf("latest run output missing #202 row:\n%s", got)
	}
}

func TestLoadIgnoresArbitraryDeepJSONRecords(t *testing.T) {
	repo := t.TempDir()
	runID := "run-bounded"
	writeAttempt(t, repo, runID, 301, 1, "job-301-1", workerReport(301, usageTotal(3010)))

	deep := filepath.Join(state.RunPath(repo, runID), "scratch", "deep", "evil.json")
	if err := os.MkdirAll(filepath.Dir(deep), 0o755); err != nil {
		t.Fatalf("MkdirAll deep JSON: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"issue":       301,
		"verdict":     "pass",
		"attestation": verifierReport(999, usageSplit(1, 1, 2)),
	})
	if err != nil {
		t.Fatalf("Marshal deep JSON: %v", err)
	}
	if err := os.WriteFile(deep, data, 0o644); err != nil {
		t.Fatalf("WriteFile deep JSON: %v", err)
	}

	report, err := Load(Options{RepoPath: repo, RunID: runID})
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if report.VerifierRecordCount != 0 {
		t.Fatalf("VerifierRecordCount = %d, want 0 for arbitrary deep JSON", report.VerifierRecordCount)
	}
	if got := Render(report); !strings.Contains(got, "| #301 | job-301-1 | not reported |") {
		t.Fatalf("rendered status missing unverified worker row:\n%s", got)
	}
}

func TestLoadRejectsOversizedKnownRunRecords(t *testing.T) {
	repo := t.TempDir()
	runID := "run-oversized"
	writeAttempt(t, repo, runID, 401, 1, "job-401-1", workerReport(401, usageTotal(4010)))

	path := filepath.Join(state.RunPath(repo, runID), "verifiers", "pr-501.loopreview.json")
	writeLargeFile(t, path, maxRunStatusRecordBytes+1)

	_, err := Load(Options{RepoPath: repo, RunID: runID})
	if err == nil {
		t.Fatal("Load returned nil error for oversized verifier record")
	}
	if text := err.Error(); !strings.Contains(text, "file is too large") || !strings.Contains(text, "verifiers/pr-501.loopreview.json") {
		t.Fatalf("oversized error missing clear diagnostic: %v", err)
	}
}

func TestLoadRejectsOversizedWorkerAttemptBeforeStateLoad(t *testing.T) {
	repo := t.TempDir()
	runID := "run-oversized-attempt"
	path := filepath.Join(state.RunPath(repo, runID), "workers", "job-501-1.attempt.json")
	writeLargeFile(t, path, maxRunStatusRecordBytes+1)

	_, err := Load(Options{RepoPath: repo, RunID: runID})
	if err == nil {
		t.Fatal("Load returned nil error for oversized worker attempt")
	}
	if text := err.Error(); !strings.Contains(text, "scan worker records") || !strings.Contains(text, "file is too large") {
		t.Fatalf("oversized attempt error missing clear diagnostic: %v", err)
	}
}

func TestLoadRejectsFutureDatedKnownRunRecord(t *testing.T) {
	repo := t.TempDir()
	runID := "run-future"
	writeAttempt(t, repo, runID, 601, 1, "job-601-1", workerReport(601, usageTotal(6010)))
	path := writeVerifierRecord(t, repo, runID, map[string]any{
		"issue":       601,
		"verdict":     "pass",
		"attestation": verifierReport(601, usageSplit(1, 1, 2)),
	})
	future := time.Now().UTC().Add(maxRunStatusFutureSkew + time.Hour)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("Chtimes verifier: %v", err)
	}

	_, err := Load(Options{RepoPath: repo, RunID: runID})
	if err == nil {
		t.Fatal("Load returned nil error for future-dated verifier record")
	}
	if text := err.Error(); !strings.Contains(text, "mtime") || !strings.Contains(text, "future skew") {
		t.Fatalf("future mtime error missing clear diagnostic: %v", err)
	}
}

func TestLoadRejectsNestedKnownRunRecordDirectory(t *testing.T) {
	repo := t.TempDir()
	runID := "run-nested"
	writeAttempt(t, repo, runID, 701, 1, "job-701-1", workerReport(701, usageTotal(7010)))

	nested := filepath.Join(state.RunPath(repo, runID), "verifiers", "nested", "pr-701.loopreview.json")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatalf("MkdirAll nested verifier: %v", err)
	}
	if err := os.WriteFile(nested, []byte(`{"issue":701,"verdict":"pass"}`), 0o644); err != nil {
		t.Fatalf("WriteFile nested verifier: %v", err)
	}

	_, err := Load(Options{RepoPath: repo, RunID: runID})
	if err == nil {
		t.Fatal("Load returned nil error for nested verifier record directory")
	}
	if text := err.Error(); !strings.Contains(text, "depth limit") || !strings.Contains(text, "verifiers/nested") {
		t.Fatalf("nested directory error missing clear diagnostic: %v", err)
	}
}

func writeAttempt(t *testing.T, repo, runID string, issue, attempt int, jobID string, record reporter.Report) {
	t.Helper()
	exitCode := 0
	_, err := state.WriteAttempt(repo, runID, state.AttemptRecord{
		Version:        1,
		JobID:          jobID,
		Issue:          issue,
		Attempt:        attempt,
		Provider:       string(record.Provider),
		PID:            12345,
		Phase:          "codex_exited",
		Status:         "succeeded",
		StartedAt:      record.StartedAt,
		HeartbeatAt:    record.EndedAt,
		LastProgressAt: record.EndedAt,
		LogBytes:       1234,
		ExitCode:       &exitCode,
		Report:         &record,
	})
	if err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
}

func writeEventLine(t *testing.T, repo, runID, line string) {
	t.Helper()
	path := state.EventsPath(repo, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll events: %v", err)
	}
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile events: %v", err)
	}
}

func writeVerifierRecord(t *testing.T, repo, runID string, record map[string]any) string {
	t.Helper()
	path := filepath.Join(state.RunPath(repo, runID), "verifiers", "pr-501.loopreview.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll verifier: %v", err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal verifier: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile verifier: %v", err)
	}
	return path
}

func writeLargeFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll large file: %v", err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(size)), 0o644); err != nil {
		t.Fatalf("WriteFile large file: %v", err)
	}
}

func workerReport(issue int, usage reporter.Usage) reporter.Report {
	return reporter.Report{
		Role:        reporter.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-5.5",
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "xhigh",
		Permission:  reporter.PermissionWrite,
		Action:      "implement issue #" + strconv.Itoa(issue),
		ExitCode:    0,
		StartedAt:   "2026-07-01T00:00:00Z",
		EndedAt:     "2026-07-01T00:00:42Z",
		DurationMS:  42000,
		Usage:       usage,
		Verified:    true,
	}
}

func verifierReport(pr int, usage reporter.Usage) reporter.Report {
	return reporter.Report{
		Role:        reporter.RoleVerifier,
		Provider:    "claude",
		Model:       "claude-sonnet-4-5",
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionReadOnly,
		Action:      "review PR #" + strconv.Itoa(pr),
		ExitCode:    0,
		StartedAt:   "2026-07-01T00:01:00Z",
		EndedAt:     "2026-07-01T00:01:07Z",
		DurationMS:  7000,
		Usage:       usage,
		Verified:    true,
	}
}

func usageSplit(input, output, total int64) reporter.Usage {
	return reporter.Usage{
		InputTokens:  &input,
		OutputTokens: &output,
		TotalTokens:  &total,
	}
}

func usageTotal(total int64) reporter.Usage {
	return reporter.Usage{
		TotalTokens: &total,
	}
}
