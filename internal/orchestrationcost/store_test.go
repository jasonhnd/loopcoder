package orchestrationcost

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/reporter"
)

func TestLedgerRoundTripPreservesUnknownUsage(t *testing.T) {
	repo := t.TempDir()
	report, err := Build("run-ledger", DefaultPolicy(), []Event{
		EventFromReport("worker-1", RoleWorker, true, &reporter.Report{}),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := Write(repo, report); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, found, err := Load(repo, "run-ledger")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found || loaded.Totals.UsageState != UsageUnknown || loaded.Totals.Tokens != nil || loaded.Totals.ModelCalls != 1 {
		t.Fatalf("loaded report = %#v, found=%v", loaded, found)
	}
	path, err := ledgerPath(repo, "run-ledger")
	if err != nil {
		t.Fatalf("ledgerPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %o, want 600", info.Mode().Perm())
	}
}

func TestLedgerWriteRendersCurrentBudgetRefusal(t *testing.T) {
	repo := t.TempDir()
	tokens := int64(10)
	policy := Policy{MaxModelCalls: 1, MaxTokens: 100, MaxOverheadPercent: 10}
	report, err := Build("run-current-refusal", policy, []Event{
		EventFromReport("worker-1", RoleWorker, true, &reporter.Report{Usage: reporter.Usage{TotalTokens: &tokens}}),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	report = ApplyBudgetDecision(report, CheckBeforeModelCall(report, 1))
	if err := Write(repo, report); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path, err := ledgerPath(repo, report.RunID)
	if err != nil {
		t.Fatalf("ledgerPath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), `"status": "needs-human"`) || !strings.Contains(string(data), `"reason": "model-call-budget"`) {
		t.Fatalf("ledger = %s", data)
	}
}

func TestLedgerRoundTripPreservesDuplicateSuppressions(t *testing.T) {
	repo := t.TempDir()
	event := DeterministicEvent("same", RoleWaiting, ActivityContextPacket, "packet")
	report, err := Build("run-dedupe-ledger", DefaultPolicy(), []Event{event, event})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := Write(repo, report); err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, found, err := Load(repo, "run-dedupe-ledger")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found || loaded.Totals.DuplicateSuppressions != 1 {
		t.Fatalf("loaded report = %#v, found=%v", loaded, found)
	}
}

func TestLedgerRejectsCorruption(t *testing.T) {
	repo := t.TempDir()
	path, err := ledgerPath(repo, "run-corrupt")
	if err != nil {
		t.Fatalf("ledgerPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := Load(repo, "run-corrupt"); err == nil {
		t.Fatal("Load accepted a corrupt ledger")
	}
}

func TestLedgerRejectsRunIDPathTraversal(t *testing.T) {
	if _, _, err := Load(t.TempDir(), "../escape"); err == nil {
		t.Fatal("Load accepted a traversal run id")
	}
}

func TestRunLockSerializesWritersForOneRun(t *testing.T) {
	repo := t.TempDir()
	first, err := AcquireRunLock(repo, "run-locked", 0)
	if err != nil {
		t.Fatalf("AcquireRunLock first: %v", err)
	}
	if _, err := AcquireRunLock(repo, "run-locked", 0); err == nil {
		t.Fatal("second writer acquired the same run lock")
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	second, err := AcquireRunLock(repo, "run-locked", 0)
	if err != nil {
		t.Fatalf("AcquireRunLock after release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
}

func TestRunLockCanonicalizesRepositorySymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows")
	}
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatalf("Mkdir repo: %v", err)
	}
	alias := filepath.Join(parent, "repo-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	first, err := AcquireRunLock(repo, "run-alias", 0)
	if err != nil {
		t.Fatalf("AcquireRunLock real path: %v", err)
	}
	defer first.Release()
	if _, err := AcquireRunLock(alias, "run-alias", 0); err == nil {
		t.Fatal("repository symlink bypassed the per-run writer lock")
	}
}
