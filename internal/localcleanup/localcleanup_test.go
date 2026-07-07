package localcleanup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/relaygate"
)

func TestCleanupRetainsActiveLivePIDAndPendingRuns(t *testing.T) {
	repo := t.TempDir()
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	old := now.Add(-60 * 24 * time.Hour)
	policy := DefaultPolicy()
	policy.MinimumRunDirectories = 0

	staleTerminal := "run-20260501T000000Z-issue-1"
	active := "run-20260502T000000Z-issue-2"
	livePID := "run-20260503T000000Z-issue-3"
	pending := "run-20260504T000000Z-issue-4"
	pid := 4242

	writeRun(t, repo, staleTerminal, "succeeded", nil, old)
	writeRun(t, repo, active, "running", nil, old)
	writeRun(t, repo, livePID, "succeeded", &pid, old)
	writeRun(t, repo, pending, "failed", nil, old)
	pendingPath, err := relaygate.Write(relaygate.WriteOptions{
		RepoPath: repo,
		RunID:    pending,
		Role:     "worker",
		PRNumber: 4,
		Block:    "[reporter] role=worker\n",
	})
	if err != nil {
		t.Fatalf("write pending relay: %v", err)
	}

	result, err := Cleanup(Options{
		RepoPath: repo,
		Now:      now,
		Policy:   policy,
		Apply:    true,
		ProcessAlive: func(got int) bool {
			return got == pid
		},
	})
	if err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}

	assertMissing(t, filepath.Join(repo, ".loopcoder", "runs", staleTerminal))
	assertExists(t, filepath.Join(repo, ".loopcoder", "runs", active))
	assertExists(t, filepath.Join(repo, ".loopcoder", "runs", livePID))
	assertExists(t, filepath.Join(repo, ".loopcoder", "runs", pending))
	assertExists(t, pendingPath)
	assertPlanned(t, result, KindRun, staleTerminal)
	assertRetained(t, result, KindRun, active, "non-terminal")
	assertRetained(t, result, KindRun, livePID, "still live")
	assertRetained(t, result, KindRun, pending, "pending relay")
}

func TestCleanupAppliesRetentionTargetsWithoutRewritingRetainedLedgers(t *testing.T) {
	repo := t.TempDir()
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	old := now.Add(-60 * 24 * time.Hour)
	newerOld := now.Add(-50 * 24 * time.Hour)
	fresh := now.Add(-24 * time.Hour)
	policy := DefaultPolicy()
	policy.MinimumRunDirectories = 0
	policy.MinimumRelayLedgers = 2

	livenessOld := filepath.Join(repo, ".loopcoder", "worktree-liveness", "old", "stale.json")
	livenessFresh := filepath.Join(repo, ".loopcoder", "worktree-liveness", "fresh.json")
	writeFile(t, livenessOld, "old", old)
	writeFile(t, livenessFresh, "fresh", fresh)

	relayDelete := filepath.Join(repo, ".loopcoder", "relay", "old-run", "delete.attest")
	relayKeepOld := filepath.Join(repo, ".loopcoder", "relay", "old-run", "keep.attest")
	relayFresh := filepath.Join(repo, ".loopcoder", "relay", "fresh-run", "fresh.attest")
	writeFile(t, relayDelete, "delete ledger\n", old)
	writeFile(t, relayKeepOld, "keep ledger\n", newerOld)
	writeFile(t, relayFresh, "fresh ledger\n", fresh)
	if _, err := relaygate.Write(relaygate.WriteOptions{
		RepoPath: repo,
		RunID:    "run-pending",
		Role:     "verifier",
		PRNumber: 0,
		Block:    "[reporter] role=verifier\n",
	}); err != nil {
		t.Fatalf("write pending relay: %v", err)
	}

	auditDelete := filepath.Join(repo, ".loopcoder", "audit", "llm-delete.log")
	auditReferenced := filepath.Join(repo, ".loopcoder", "audit", "llm-referenced.log")
	auditFresh := filepath.Join(repo, ".loopcoder", "audit", "llm-fresh.log")
	writeFile(t, auditDelete, "delete", old)
	writeFile(t, auditReferenced, "referenced", old)
	writeFile(t, auditFresh, "fresh", fresh)
	writeFile(t, filepath.Join(repo, ".loopcoder", "audit", "current-output.txt"), "see llm-referenced.log", fresh)

	result, err := Cleanup(Options{
		RepoPath:     repo,
		Now:          now,
		Policy:       policy,
		Apply:        true,
		ProcessAlive: func(int) bool { return false },
		MaxFileBytes: 1024 * 1024,
		MaxEntries:   100,
	})
	if err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}

	assertMissing(t, livenessOld)
	assertExists(t, livenessFresh)
	assertMissing(t, relayDelete)
	assertExists(t, relayKeepOld)
	assertExists(t, relayFresh)
	if got := string(readFile(t, relayKeepOld)); got != "keep ledger\n" {
		t.Fatalf("retained .attest ledger was rewritten: %q", got)
	}
	assertExists(t, filepath.Join(repo, ".loopcoder", "relay", "pending"))
	assertMissing(t, auditDelete)
	assertExists(t, auditReferenced)
	assertExists(t, auditFresh)
	assertPlanned(t, result, KindWorktreeLiveness, "stale.json")
	assertPlanned(t, result, KindRelayLedger, "delete.attest")
	assertPlanned(t, result, KindAuditLog, "llm-delete.log")
	assertRetained(t, result, KindRelayLedger, "keep.attest", "newest")
	assertRetained(t, result, KindAuditLog, "llm-referenced.log", "referenced")
}

func TestPlanDoesNotRemoveEligibleFiles(t *testing.T) {
	repo := t.TempDir()
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	old := now.Add(-60 * 24 * time.Hour)
	policy := DefaultPolicy()
	policy.MinimumRunDirectories = 0
	runID := "run-20260501T000000Z-issue-5"
	writeRun(t, repo, runID, "failed", nil, old)

	result, err := Plan(Options{
		RepoPath:     repo,
		Now:          now,
		Policy:       policy,
		ProcessAlive: func(int) bool { return false },
	})
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}
	assertExists(t, filepath.Join(repo, ".loopcoder", "runs", runID))
	assertPlanned(t, result, KindRun, runID)
	if len(result.Removed) != 0 {
		t.Fatalf("Plan removed %d paths, want 0", len(result.Removed))
	}
}

func TestCleanupDoesNotFollowSymlinkEscape(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.log")
	writeFile(t, outside, "do not remove", time.Now().UTC())
	link := filepath.Join(repo, ".loopcoder", "worktree-liveness", "outside.log")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir symlink parent: %v", err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, err := Cleanup(Options{
		RepoPath: repo,
		Now:      time.Now().UTC(),
		Apply:    true,
		Policy:   DefaultPolicy(),
	})
	if err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}
	assertExists(t, outside)
	assertExists(t, link)
}

func TestCleanupHonorsDirectoryEntryLimit(t *testing.T) {
	repo := t.TempDir()
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	old := now.Add(-60 * 24 * time.Hour)
	first := filepath.Join(repo, ".loopcoder", "worktree-liveness", "a.json")
	second := filepath.Join(repo, ".loopcoder", "worktree-liveness", "b.json")
	writeFile(t, first, "a", old)
	writeFile(t, second, "b", old)

	result, err := Cleanup(Options{
		RepoPath:   repo,
		Now:        now,
		Policy:     DefaultPolicy(),
		Apply:      true,
		MaxEntries: 1,
	})
	if err != nil {
		t.Fatalf("Cleanup returned error: %v", err)
	}
	assertExists(t, first)
	assertExists(t, second)
	if !diagnosticsContain(result, "directory entry limit exceeded") {
		t.Fatalf("diagnostics = %#v, want directory entry limit diagnostic", result.Diagnostics)
	}
}

func writeRun(t *testing.T, repo, runID, status string, pid *int, mod time.Time) {
	t.Helper()
	runDir := filepath.Join(repo, ".loopcoder", "runs", runID)
	workersDir := filepath.Join(runDir, "workers")
	if err := os.MkdirAll(workersDir, 0o755); err != nil {
		t.Fatalf("mkdir run: %v", err)
	}
	record := map[string]any{
		"version": 1,
		"job_id":  "job-test",
		"issue":   1,
		"attempt": 1,
		"status":  status,
	}
	if pid != nil {
		record["pid"] = *pid
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("marshal attempt: %v", err)
	}
	attemptPath := filepath.Join(workersDir, "job-test.attempt.json")
	if err := os.WriteFile(attemptPath, data, 0o600); err != nil {
		t.Fatalf("write attempt: %v", err)
	}
	touch(t, attemptPath, mod)
	touch(t, workersDir, mod)
	touch(t, runDir, mod)
}

func writeFile(t *testing.T, path, content string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	touch(t, path, mod)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func touch(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s does not exist: %v", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists or returned unexpected error: %v", path, err)
	}
}

func assertPlanned(t *testing.T, result Result, kind, pathPart string) {
	t.Helper()
	if !actionsContain(result.Planned, kind, pathPart, "") {
		t.Fatalf("planned actions = %#v, want %s containing %q", result.Planned, kind, pathPart)
	}
}

func assertRetained(t *testing.T, result Result, kind, pathPart, reasonPart string) {
	t.Helper()
	if !actionsContain(result.Retained, kind, pathPart, reasonPart) {
		t.Fatalf("retained actions = %#v, want %s containing %q reason %q", result.Retained, kind, pathPart, reasonPart)
	}
}

func actionsContain(actions []Action, kind, pathPart, reasonPart string) bool {
	for _, action := range actions {
		if action.Kind != kind {
			continue
		}
		if !strings.Contains(filepath.ToSlash(action.Path), filepath.ToSlash(pathPart)) {
			continue
		}
		if reasonPart != "" && !strings.Contains(action.Reason, reasonPart) {
			continue
		}
		return true
	}
	return false
}

func diagnosticsContain(result Result, text string) bool {
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic, text) {
			return true
		}
	}
	return false
}
