package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/process"
)

func writeAttemptSidecar(t *testing.T, repo, runID, job, body string) {
	t.Helper()
	workers := filepath.Join(repo, ".loopcoder", "runs", runID, "workers")
	if err := os.MkdirAll(workers, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workers, job+".attempt.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadManagedProcessesReadsRunningAttempt(t *testing.T) {
	repo := t.TempDir()
	pid := os.Getpid() // this test process is guaranteed alive
	body := fmt.Sprintf(`{"version":1,"job_id":"job-42-1","issue":42,"provider":"codex","pid":%d,"status":"running","started_at":"2026-01-01T00:00:00Z"}`, pid)
	writeAttemptSidecar(t, repo, "run-20260101T000000Z-issue-42", "job-42-1", body)

	rows := loadManagedProcesses(repo)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if rows[0].PID != pid || rows[0].Issue != 42 || rows[0].Provider != "codex" || rows[0].Status != "running" {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestLoadManagedProcessesSkipsDeadAndNonRunning(t *testing.T) {
	repo := t.TempDir()
	// running but a pid that cannot be alive
	writeAttemptSidecar(t, repo, "run-20260101T000000Z-issue-9", "dead",
		`{"issue":9,"pid":2147480000,"status":"running","started_at":"t"}`)
	// alive pid but already succeeded
	writeAttemptSidecar(t, repo, "run-20260101T000000Z-issue-9", "done",
		fmt.Sprintf(`{"issue":9,"pid":%d,"status":"succeeded","started_at":"t"}`, os.Getpid()))

	if rows := loadManagedProcesses(repo); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}

func TestRunPsEmptyRepo(t *testing.T) {
	var out, errb bytes.Buffer
	code := runPs([]string{"--repo", t.TempDir()}, &out, &errb, Deps{})
	if code != 0 {
		t.Fatalf("runPs exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "no loopcoder-managed processes running") {
		t.Fatalf("runPs output = %q", out.String())
	}
}

func TestRunKillRequiresScope(t *testing.T) {
	var out, errb bytes.Buffer
	code := runKill([]string{"--repo", t.TempDir()}, &out, &errb, Deps{})
	if code != 2 {
		t.Fatalf("runKill without --run/--all exit = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "--run") {
		t.Fatalf("runKill error = %q", errb.String())
	}
}

func TestRunKillRunFilterMatchesNothingIsSafe(t *testing.T) {
	repo := t.TempDir()
	// A running attempt under a DIFFERENT run id must not be touched when killing
	// a non-matching run (this test process's own pid).
	body := fmt.Sprintf(`{"issue":7,"pid":%d,"status":"running","started_at":"t"}`, os.Getpid())
	writeAttemptSidecar(t, repo, "run-20260101T000000Z-issue-7", "job-7-1", body)

	var out, errb bytes.Buffer
	code := runKill([]string{"--repo", repo, "--run", "run-20990101T000000Z-issue-999"}, &out, &errb, Deps{})
	if code != 0 {
		t.Fatalf("runKill exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "terminated 0 loopcoder-managed process tree(s)") {
		t.Fatalf("runKill should have matched nothing: %q", out.String())
	}
}

func TestRunKillTerminatesMatchingRun(t *testing.T) {
	if os.Getenv("LC_MANAGE_KILL_HELPER") == "1" {
		time.Sleep(60 * time.Second) // parent kills this well before it returns
		return
	}
	// Spawn a real throwaway child (this test binary re-executed into the helper
	// branch; its output goes to the null device) and register it as a running
	// attempt, then terminate it by run id and confirm it actually died.
	child := exec.Command(os.Args[0], "-test.run=^TestRunKillTerminatesMatchingRun$")
	child.Env = append(os.Environ(), "LC_MANAGE_KILL_HELPER=1")
	if err := child.Start(); err != nil {
		t.Fatalf("start helper child: %v", err)
	}
	pid := child.Process.Pid
	waited := make(chan struct{})
	go func() { _ = child.Wait(); close(waited) }()
	defer func() { _ = child.Process.Kill() }() // safety net if the assertion path fails

	deadline := time.Now().Add(5 * time.Second)
	for !process.Alive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d never became alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	repo := t.TempDir()
	runID := "run-20260101T000000Z-issue-5"
	body := fmt.Sprintf(`{"issue":5,"provider":"codex","pid":%d,"status":"running","started_at":"t"}`, pid)
	writeAttemptSidecar(t, repo, runID, "job-5-1", body)

	var out, errb bytes.Buffer
	if code := runKill([]string{"--repo", repo, "--run", runID}, &out, &errb, Deps{}); code != 0 {
		t.Fatalf("runKill = %d, stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "terminated 1 loopcoder-managed process tree(s)") {
		t.Fatalf("runKill output = %q", out.String())
	}
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("child was not terminated by runKill")
	}
}
