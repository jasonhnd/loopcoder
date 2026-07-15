package orchestrationcost

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/lockfile"
	"github.com/jasonhnd/loopcoder/internal/state"
)

const ledgerFilename = "orchestration-cost.json"

type RunLock interface {
	Release() error
}

// AcquireRunLock serializes all writers for one durable run ledger. Holding
// this lock for a tick makes load/check/reserve/write a single cross-process
// critical section instead of a last-writer-wins sequence.
func AcquireRunLock(repoPath, runID string, timeout time.Duration) (RunLock, error) {
	if _, err := ledgerPath(repoPath, runID); err != nil {
		return nil, err
	}
	absRepo, err := filepath.Abs(strings.TrimSpace(repoPath))
	if err != nil {
		return nil, fmt.Errorf("canonicalize orchestration cost repository: %w", err)
	}
	canonicalRepo, err := filepath.EvalSymlinks(absRepo)
	if err != nil {
		return nil, fmt.Errorf("resolve orchestration cost repository: %w", err)
	}
	identity := filepath.Join(canonicalRepo, ".loopcoder-run-locks", strings.TrimSpace(runID))
	return lockfile.Acquire(identity, timeout)
}

func ledgerPath(repoPath, runID string) (string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || filepath.IsAbs(runID) || filepath.Base(runID) != runID || runID == "." || runID == ".." {
		return "", fmt.Errorf("invalid orchestration cost run_id %q", runID)
	}
	return filepath.Join(state.RunPath(repoPath, runID), ledgerFilename), nil
}

func Load(repoPath, runID string) (Report, bool, error) {
	path, err := ledgerPath(repoPath, runID)
	if err != nil {
		return Report{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Report{}, false, nil
	}
	if err != nil {
		return Report{}, false, fmt.Errorf("read orchestration cost ledger: %w", err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, false, fmt.Errorf("decode orchestration cost ledger: %w", err)
	}
	if report.SchemaVersion != SchemaVersion {
		return Report{}, false, fmt.Errorf("unsupported orchestration cost schema %q", report.SchemaVersion)
	}
	if report.RunID != strings.TrimSpace(runID) {
		return Report{}, false, fmt.Errorf("orchestration cost ledger run_id %q does not match %q", report.RunID, runID)
	}
	normalized, err := Build(report.RunID, report.Policy, report.Events)
	if err != nil {
		return Report{}, false, fmt.Errorf("validate orchestration cost ledger: %w", err)
	}
	normalized = RestoreDecisionState(normalized, report.BudgetDecisions, report.ReleaseGate)
	return normalized, true, nil
}

func Write(repoPath string, report Report) error {
	if report.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported orchestration cost schema %q", report.SchemaVersion)
	}
	normalized, err := Build(report.RunID, report.Policy, report.Events)
	if err != nil {
		return fmt.Errorf("validate orchestration cost ledger: %w", err)
	}
	normalized = RestoreDecisionState(normalized, report.BudgetDecisions, report.ReleaseGate)
	if report.Status == StatusNeedsHuman && report.ReleaseGate == nil {
		for i := len(report.BudgetDecisions) - 1; i >= 0; i-- {
			decision := report.BudgetDecisions[i]
			if !decision.Allowed && decision.Reason == report.Reason {
				normalized = ReapplyBudgetDecision(normalized, decision)
				break
			}
		}
	}
	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return fmt.Errorf("encode orchestration cost ledger: %w", err)
	}
	data = append(data, '\n')
	path, err := ledgerPath(repoPath, report.RunID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create orchestration cost ledger directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".orchestration-cost-*.tmp")
	if err != nil {
		return fmt.Errorf("create orchestration cost ledger temp file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("chmod orchestration cost ledger: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write orchestration cost ledger: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync orchestration cost ledger: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close orchestration cost ledger: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace orchestration cost ledger: %w", err)
	}
	return nil
}
