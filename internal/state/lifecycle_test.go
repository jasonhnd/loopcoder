package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateLifecycleTransitionRejectsInvalidTerminalMove(t *testing.T) {
	if err := ValidateLifecycleTransition(StatePlanned, StateQueued); err != nil {
		t.Fatalf("planned -> queued rejected: %v", err)
	}
	if err := ValidateLifecycleTransition(StateSucceeded, StateRunning); err == nil || !strings.Contains(err.Error(), "invalid lifecycle transition") {
		t.Fatalf("succeeded -> running error = %v, want invalid transition", err)
	}
	if err := ValidateLifecycleTransition(StateRunning, "mystery"); err == nil || !strings.Contains(err.Error(), "invalid lifecycle state") {
		t.Fatalf("invalid state error = %v", err)
	}
}

func TestAppendLifecycleTransitionPersistsAndReloadsCurrentState(t *testing.T) {
	repo := t.TempDir()
	runID := "run-test"

	if err := AppendLifecycleTransition(repo, LifecycleTransition{
		Timestamp: "2026-07-09T00:00:00Z",
		RunID:     runID,
		State:     StatePlanned,
		Reason:    "planned by conductor",
		Source:    "test",
	}); err != nil {
		t.Fatalf("append planned: %v", err)
	}
	if err := AppendLifecycleTransition(repo, LifecycleTransition{
		Timestamp:   "2026-07-09T00:00:01Z",
		RunID:       runID,
		ParentRunID: "run-parent",
		State:       StateQueued,
		ChildRunID:  "run-child",
	}); err != nil {
		t.Fatalf("append queued: %v", err)
	}
	if err := AppendLifecycleTransition(repo, LifecycleTransition{
		Timestamp: "2026-07-09T00:00:02Z",
		RunID:     runID,
		State:     StateRunning,
	}); err != nil {
		t.Fatalf("append running: %v", err)
	}

	data, err := os.ReadFile(LifecyclePath(repo, runID))
	if err != nil {
		t.Fatalf("read lifecycle file: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lifecycle lines = %d, want 3: %s", len(lines), string(data))
	}
	if strings.Contains(lines[0], ": ") || strings.Contains(lines[0], ", ") {
		t.Fatalf("lifecycle JSON line is not compact: %q", lines[0])
	}
	if !strings.Contains(lines[1], `"previous_state":"planned"`) {
		t.Fatalf("second transition missing previous_state planned: %s", lines[1])
	}

	got, err := LoadLifecycle(repo, runID)
	if err != nil {
		t.Fatalf("LoadLifecycle: %v", err)
	}
	if got.State != StateRunning || got.ParentRunID != "run-parent" || got.Source != "lifecycle" {
		t.Fatalf("lifecycle = %#v, want running parent/source", got)
	}
	if len(got.History) != 3 {
		t.Fatalf("history len = %d, want 3", len(got.History))
	}
	if len(got.ChildRunIDs) != 1 || got.ChildRunIDs[0] != "run-child" {
		t.Fatalf("children = %#v, want run-child", got.ChildRunIDs)
	}
}

func TestAppendLifecycleTransitionRejectsInvalidPersistedMove(t *testing.T) {
	repo := t.TempDir()
	runID := "run-invalid"
	if err := AppendLifecycleTransition(repo, LifecycleTransition{
		Timestamp: "2026-07-09T00:00:00Z",
		RunID:     runID,
		State:     StateSucceeded,
	}); err != nil {
		t.Fatalf("append succeeded: %v", err)
	}
	err := AppendLifecycleTransition(repo, LifecycleTransition{
		Timestamp: "2026-07-09T00:00:01Z",
		RunID:     runID,
		State:     StateRunning,
	})
	if err == nil || !strings.Contains(err.Error(), "succeeded -> running") {
		t.Fatalf("invalid append error = %v", err)
	}
}

func TestLoadLifecycleHistoryRejectsCorruptSequence(t *testing.T) {
	repo := t.TempDir()
	runID := "run-corrupt"
	path := LifecyclePath(repo, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll lifecycle: %v", err)
	}
	data := strings.Join([]string{
		`{"version":1,"ts":"2026-07-09T00:00:00Z","run_id":"run-corrupt","state":"succeeded"}`,
		`{"version":1,"ts":"2026-07-09T00:00:01Z","run_id":"run-corrupt","previous_state":"succeeded","state":"running"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("WriteFile lifecycle: %v", err)
	}

	_, err := LoadLifecycleHistory(repo, runID)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "succeeded -> running") {
		t.Fatalf("corrupt history error = %v", err)
	}
}

func TestImportLegacyLifecycleMapsV06EventsAndAttempts(t *testing.T) {
	repo := t.TempDir()
	runID := "run-legacy"
	if err := AppendEvent(repo, runID, Event{
		Timestamp: "2026-07-09T00:00:00Z",
		RunID:     runID,
		JobID:     "job-1",
		Issue:     647,
		Phase:     "worker_started",
		Status:    "running",
	}); err != nil {
		t.Fatalf("append legacy event: %v", err)
	}
	exitCode := 1
	if _, err := WriteAttempt(repo, runID, AttemptRecord{
		Version:        1,
		JobID:          "job-1",
		Issue:          647,
		Attempt:        1,
		Provider:       "codex",
		PID:            123,
		Phase:          "codex_exited",
		Status:         "failed",
		StartedAt:      "2026-07-09T00:00:00Z",
		HeartbeatAt:    "2026-07-09T00:01:00Z",
		LastProgressAt: "2026-07-09T00:01:00Z",
		LogBytes:       10,
		ExitCode:       &exitCode,
	}); err != nil {
		t.Fatalf("write attempt: %v", err)
	}

	got, err := LoadLifecycle(repo, runID)
	if err != nil {
		t.Fatalf("LoadLifecycle: %v", err)
	}
	if got.Source != "legacy" || got.State != StateFailed {
		t.Fatalf("legacy lifecycle = %#v, want failed legacy", got)
	}
	if len(got.History) != 2 {
		t.Fatalf("legacy history len = %d, want 2: %#v", len(got.History), got.History)
	}
	if got.History[0].State != StateRunning || got.History[1].PreviousState != StateRunning || got.History[1].Source != "legacy-attempt" {
		t.Fatalf("legacy transitions = %#v", got.History)
	}
}

func TestImportLegacyLifecycleUsesAttemptMTimeWhenNoTimestamp(t *testing.T) {
	repo := t.TempDir()
	runID := "run-mtime"
	if _, err := WriteAttempt(repo, runID, AttemptRecord{
		Version:  1,
		JobID:    "job-1",
		Issue:    647,
		Attempt:  1,
		Provider: "codex",
		PID:      123,
		Phase:    "worker_started",
		Status:   "running",
	}); err != nil {
		t.Fatalf("write attempt: %v", err)
	}
	mtime := time.Date(2026, 7, 9, 1, 2, 3, 0, time.UTC)
	path := AttemptPath(repo, runID, "job-1")
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes attempt: %v", err)
	}

	got, err := ImportLegacyLifecycle(repo, runID)
	if err != nil {
		t.Fatalf("ImportLegacyLifecycle: %v", err)
	}
	if len(got) != 1 || got[0].Timestamp != "2026-07-09T01:02:03Z" {
		t.Fatalf("legacy timestamp = %#v, want mtime", got)
	}
}
