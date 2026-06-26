package state

import (
	"os"
	"path/filepath"
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
