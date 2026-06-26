package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatTimestampUsesUTCRFC3339(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	input := time.Date(2026, 6, 26, 12, 34, 56, 987654321, jst)

	got := FormatTimestamp(input)
	want := "2026-06-26T03:34:56Z"
	if got != want {
		t.Fatalf("FormatTimestamp() = %q, want %q", got, want)
	}
}

func TestParseTimestampAcceptsPowerShellRoundTripFormat(t *testing.T) {
	got, err := ParseTimestamp("2026-06-26T12:34:56.1234567+09:00")
	if err != nil {
		t.Fatalf("ParseTimestamp returned error: %v", err)
	}

	want := time.Date(2026, 6, 26, 3, 34, 56, 123456700, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("ParseTimestamp() = %s, want %s", got, want)
	}
}

func TestTimestampFormatParseRoundTrip(t *testing.T) {
	input := time.Date(2026, 6, 26, 12, 34, 56, 0, time.FixedZone("offset", -7*60*60))

	formatted := FormatTimestamp(input)
	parsed, err := ParseTimestamp(formatted)
	if err != nil {
		t.Fatalf("ParseTimestamp(%q) returned error: %v", formatted, err)
	}

	if !parsed.Equal(input.UTC()) {
		t.Fatalf("parsed timestamp = %s, want %s", parsed, input.UTC())
	}
}

func TestRunIDForIssueUsesDocumentedShape(t *testing.T) {
	got := RunIDForIssue(91, time.Date(2026, 6, 26, 12, 0, 0, 0, time.FixedZone("JST", 9*60*60)))
	want := "run-20260626T030000Z-issue-91"
	if got != want {
		t.Fatalf("RunIDForIssue() = %q, want %q", got, want)
	}
	if !IsRunID(got) {
		t.Fatalf("IsRunID(%q) = false, want true", got)
	}
}

func TestLatestRunIDSelectsNewestRunDirectory(t *testing.T) {
	repo := t.TempDir()
	runsRoot := filepath.Join(repo, ".loopcoder", "runs")
	if err := os.MkdirAll(filepath.Join(runsRoot, "run-old"), 0o755); err != nil {
		t.Fatalf("MkdirAll old run: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(runsRoot, "run-new"), 0o755); err != nil {
		t.Fatalf("MkdirAll new run: %v", err)
	}

	oldTime := time.Date(2026, 6, 26, 1, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 6, 26, 2, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(runsRoot, "run-old"), oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes old run: %v", err)
	}
	if err := os.Chtimes(filepath.Join(runsRoot, "run-new"), newTime, newTime); err != nil {
		t.Fatalf("Chtimes new run: %v", err)
	}

	got, err := LatestRunID(repo)
	if err != nil {
		t.Fatalf("LatestRunID returned error: %v", err)
	}
	if got != "run-new" {
		t.Fatalf("LatestRunID() = %q, want run-new", got)
	}
}

func TestLatestRunIDReturnsEmptyWhenRunsRootMissing(t *testing.T) {
	got, err := LatestRunID(t.TempDir())
	if err != nil {
		t.Fatalf("LatestRunID returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("LatestRunID() = %q, want empty", got)
	}
}

func TestLoadAttemptsInfersOptionalFields(t *testing.T) {
	repo := t.TempDir()
	workers := filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers")
	if err := os.MkdirAll(workers, 0o755); err != nil {
		t.Fatalf("MkdirAll workers: %v", err)
	}
	attemptPath := filepath.Join(workers, "job-42-1234.attempt.json")
	data := []byte(`{
  "version": 1,
  "issue": "42",
  "attempt": 2,
  "provider": "codex",
  "pid": "1234",
  "phase": "codex_started",
  "status": "running",
  "recovery_context_path": ".loopcoder/runs/run-test/recovery/job-42-1234-context.md",
  "heartbeat_at": "2026-06-26T12:01:00Z",
  "last_progress_at": "2026-06-26T12:01:00Z",
  "exit_code": null
}`)
	if err := os.WriteFile(attemptPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile attempt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workers, "bad.attempt.json"), []byte(`{bad`), 0o644); err != nil {
		t.Fatalf("WriteFile bad attempt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workers, "missing-issue.attempt.json"), []byte(`{"attempt":1}`), 0o644); err != nil {
		t.Fatalf("WriteFile missing issue attempt: %v", err)
	}

	attempts, err := LoadAttempts(repo, "run-test")
	if err != nil {
		t.Fatalf("LoadAttempts returned error: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("LoadAttempts returned %d attempts, want 1: %#v", len(attempts), attempts)
	}
	got := attempts[0]
	if got.JobID != "job-42-1234" {
		t.Fatalf("JobID = %q, want job-42-1234", got.JobID)
	}
	if got.Issue != 42 || got.Attempt != 2 {
		t.Fatalf("Issue/Attempt = %d/%d, want 42/2", got.Issue, got.Attempt)
	}
	if got.Branch != "loop/issue-42-retry-2" {
		t.Fatalf("Branch = %q, want loop/issue-42-retry-2", got.Branch)
	}
	if got.RecoveryContextPath != ".loopcoder/runs/run-test/recovery/job-42-1234-context.md" {
		t.Fatalf("RecoveryContextPath = %q", got.RecoveryContextPath)
	}
	if got.PID == nil || *got.PID != 1234 {
		t.Fatalf("PID = %#v, want 1234", got.PID)
	}
	if got.ExitCode != nil {
		t.Fatalf("ExitCode = %#v, want nil", got.ExitCode)
	}
	if got.Path != attemptPath {
		t.Fatalf("Path = %q, want %q", got.Path, attemptPath)
	}
}

func TestCountEventsCountsNonBlankLines(t *testing.T) {
	repo := t.TempDir()
	runPath := filepath.Join(repo, ".loopcoder", "runs", "run-test")
	if err := os.MkdirAll(runPath, 0o755); err != nil {
		t.Fatalf("MkdirAll run path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runPath, "events.jsonl"), []byte("{\"event\":\"one\"}\n\n{\"event\":\"two\"}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile events: %v", err)
	}

	got, err := CountEvents(repo, "run-test")
	if err != nil {
		t.Fatalf("CountEvents returned error: %v", err)
	}
	if got != 2 {
		t.Fatalf("CountEvents() = %d, want 2", got)
	}
}

func TestCountEventsReturnsZeroWhenMissing(t *testing.T) {
	got, err := CountEvents(t.TempDir(), "run-missing")
	if err != nil {
		t.Fatalf("CountEvents returned error: %v", err)
	}
	if got != 0 {
		t.Fatalf("CountEvents() = %d, want 0", got)
	}
}

func TestWriteAttemptWritesCompactSidecar(t *testing.T) {
	repo := t.TempDir()
	exitCode := 0
	errText := "failed password=hunter2"

	path, err := WriteAttempt(repo, "run-test", AttemptRecord{
		Version:        1,
		JobID:          "job-101-1234",
		Issue:          101,
		Attempt:        2,
		Provider:       "codex",
		PID:            1234,
		Phase:          "codex_exited",
		Status:         "failed",
		Branch:         "loop/issue-101-retry-2",
		StartedAt:      "2026-06-26T12:00:00Z",
		HeartbeatAt:    "2026-06-26T12:01:00Z",
		LastProgressAt: "2026-06-26T12:01:00Z",
		LogBytes:       123,
		ExitCode:       &exitCode,
		Error:          &errText,
	})
	if err != nil {
		t.Fatalf("WriteAttempt returned error: %v", err)
	}
	if path != filepath.Join(repo, ".loopcoder", "runs", "run-test", "workers", "job-101-1234.attempt.json") {
		t.Fatalf("path = %q", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile attempt: %v", err)
	}
	if strings.Contains(string(data), "\n") {
		t.Fatalf("attempt JSON is not compact: %q", string(data))
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("attempt JSON invalid: %v", err)
	}
	for _, key := range []string{"version", "job_id", "issue", "attempt", "provider", "pid", "phase", "status", "branch", "started_at", "heartbeat_at", "last_progress_at", "log_bytes", "exit_code", "error"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("attempt JSON missing key %q: %s", key, string(data))
		}
	}
	if got["branch"] != "loop/issue-101-retry-2" {
		t.Fatalf("branch = %#v", got["branch"])
	}
}

func TestAppendEventWritesCompactJSONLine(t *testing.T) {
	repo := t.TempDir()
	exitCode := 0

	err := AppendEvent(repo, "run-test", Event{
		Timestamp: "2026-06-26T12:00:00Z",
		RunID:     "run-test",
		JobID:     "job-101-1234",
		Issue:     101,
		Phase:     "cleanup",
		Status:    "succeeded",
		LogBytes:  99,
		ExitCode:  &exitCode,
		Error:     nil,
	})
	if err != nil {
		t.Fatalf("AppendEvent returned error: %v", err)
	}

	data, err := os.ReadFile(EventsPath(repo, "run-test"))
	if err != nil {
		t.Fatalf("ReadFile events: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("event lines = %d, want 1: %q", len(lines), string(data))
	}
	if strings.Contains(lines[0], " ") {
		t.Fatalf("event JSON line is not compact: %q", lines[0])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("event JSON invalid: %v", err)
	}
	for _, key := range []string{"ts", "run_id", "job_id", "issue", "phase", "status", "log_bytes", "exit_code", "error"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("event JSON missing key %q: %s", key, lines[0])
		}
	}
	if got["error"] != nil {
		t.Fatalf("error = %#v, want nil", got["error"])
	}
}
