package goalrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// CheckpointSchema is the durable forced-restart document for a goal run.
const CheckpointSchema = "loopcoder.goalrun.checkpoint.v1"

// Checkpoint is durable parent state for interrupt → resume exactly-once.
// Succeeded children carry attempt_id + output_evidence so resume reuses them
// without a second claim, provider call, file write, or capacity deduction.
type Checkpoint struct {
	Schema    string `json:"schema"`
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
	GraphID   string `json:"graph_id"`
	// PlanDigest is the canonical ExecutionPlanDigest (workflowdef.Normalize).
	PlanDigest string `json:"plan_digest"`
	// GraphDigest is the separate workgraph.DigestGraph identity.
	GraphDigest  string                     `json:"graph_digest,omitempty"`
	Goal         string                     `json:"goal,omitempty"`
	Issue        string                     `json:"issue,omitempty"`
	Actor        string                     `json:"actor,omitempty"`
	Status       string                     `json:"status"`
	Message      string                     `json:"message,omitempty"`
	Children     []ChildReport              `json:"children,omitempty"`
	WorkflowKids []workflowrun.ChildOutcome `json:"workflow_children,omitempty"`
	WorktreePeak int                        `json:"worktree_peak,omitempty"`
	ProcessPeak  int                        `json:"process_peak,omitempty"`
	ReuseCount   int                        `json:"reuse_count,omitempty"`
	ClaimCount   int                        `json:"claim_count,omitempty"`
	LaunchCount  int                        `json:"launch_count,omitempty"`
	SavedAt      time.Time                  `json:"saved_at"`
	// PriorSucceeded is the exact map seed for workflowrun.Request.PriorSucceeded.
	PriorSucceeded map[string]workflowrun.ChildOutcome `json:"prior_succeeded,omitempty"`
	// Interrupted is true only when a forced interrupt event was recorded.
	Interrupted bool `json:"interrupted,omitempty"`
	// AbortedAttempts: work_item_id → aborted attempt_id (must bump generation).
	AbortedAttempts map[string]string `json:"aborted_attempts,omitempty"`
	// AttemptGeneration for resume (aborted items get gen+1).
	AttemptGeneration map[string]int `json:"attempt_generation,omitempty"`
	// EventLogPath is the append-only raw event ledger for evidence derivation.
	EventLogPath string `json:"event_log_path,omitempty"`
	// CapacityTransitions are final ledger-backed prior/alternate transitions
	// (post applyChildOutcomes). Required for model_unavailable canary proof on resume.
	CapacityTransitions []workflowrun.CapacityTransition `json:"capacity_transitions,omitempty"`
}

// CheckpointPath returns the durable write path and ensures parent directories exist.
// Write/ensure only — never use for read-only load.
func CheckpointPath(homeDir, projectID, runID string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if projectID == "" || runID == "" {
		return "", fmt.Errorf("goalrun: checkpoint requires project_id and run_id")
	}
	var (
		layout home.V09Layout
		err    error
	)
	if homeDir != "" {
		if err := os.MkdirAll(homeDir, 0o700); err != nil {
			return "", err
		}
		_ = os.Chmod(homeDir, 0o700)
		layout, err = home.EnsureMinimumLayout(homeDir, projectID)
	} else {
		layout, err = home.ResolveV09(home.Deps{})
		if err == nil {
			err = layout.EnsureProject(projectID)
		}
	}
	if err != nil {
		return "", err
	}
	runs, err := layout.ProjectRunsDir(projectID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(runs, sanitizeRunKey(runID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "goal-checkpoint.json"), nil
}

// checkpointPathRead derives the checkpoint path without creating any directory
// or file. Missing parents are fine — callers treat missing file as ErrNotExist.
func checkpointPathRead(homeDir, projectID, runID string) (string, error) {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if projectID == "" || runID == "" {
		return "", fmt.Errorf("goalrun: checkpoint requires project_id and run_id")
	}
	root, err := workflowrun.ResolveDurableHome(homeDir)
	if err != nil {
		return "", err
	}
	// Pure path derivation — no MkdirAll / EnsureProject / EnsureMinimumLayout.
	dir := filepath.Join(root, "projects", projectID, "runs", sanitizeRunKey(runID))
	return filepath.Join(dir, "goal-checkpoint.json"), nil
}

// SaveCheckpoint writes durable resume state (owner-only file mode).
func SaveCheckpoint(homeDir string, cp Checkpoint) (string, error) {
	if strings.TrimSpace(cp.Schema) == "" {
		cp.Schema = CheckpointSchema
	}
	path, err := CheckpointPath(homeDir, cp.ProjectID, cp.RunID)
	if err != nil {
		return "", err
	}
	// Write-side fill only: never run during Load.
	if cp.PriorSucceeded == nil {
		cp.PriorSucceeded = PriorSucceededFrom(cp.WorkflowKids, cp.Children)
	}
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// LoadCheckpoint reads durable resume state when present.
// Read-only: never MkdirAll, EnsureProject, create home/project/run/file, or
// derive/repair PriorSucceeded. Missing path returns an os.ErrNotExist-compatible
// error and leaves the filesystem unchanged.
func LoadCheckpoint(homeDir, projectID, runID string) (Checkpoint, string, error) {
	path, err := checkpointPathRead(homeDir, projectID, runID)
	if err != nil {
		return Checkpoint{}, "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// Preserve ErrNotExist semantics for missing path.
		return Checkpoint{}, path, err
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return Checkpoint{}, path, err
	}
	// Do not derive or repair PriorSucceeded on load — reusable seed must already
	// be durable. Legacy derivation is audit-only via AuditPriorSucceededFrom.
	return cp, path, nil
}

// PriorSucceededFrom builds a resume seed from terminal outcomes (write-side /
// audit helper). LoadCheckpoint never calls this — product resume requires an
// explicit durable PriorSucceeded map on the checkpoint/partial document.
func PriorSucceededFrom(wf []workflowrun.ChildOutcome, reports []ChildReport) map[string]workflowrun.ChildOutcome {
	return AuditPriorSucceededFrom(wf, reports)
}

// AuditPriorSucceededFrom is the explicit audit-only derivation of prior
// succeeded outcomes from workflow kids + child reports. Never used as a
// product resume seed path (LoadCheckpoint does not call it).
func AuditPriorSucceededFrom(wf []workflowrun.ChildOutcome, reports []ChildReport) map[string]workflowrun.ChildOutcome {
	out := map[string]workflowrun.ChildOutcome{}
	for _, c := range wf {
		if !isResumeEligible(c.Terminal, c.AttemptID, c.OutputEvidence) {
			continue
		}
		// Copy without mutating the caller's slice element identity fields
		// beyond terminal normalization for the audit map value.
		cc := c
		cc.Terminal = "succeeded"
		out[c.WorkItemID] = cc
	}
	for _, r := range reports {
		if _, ok := out[r.ChildID]; ok {
			continue
		}
		if !isResumeEligible(r.Terminal, r.AttemptID, r.OutputEvidence) {
			continue
		}
		out[r.ChildID] = workflowrun.ChildOutcome{
			WorkItemID: r.ChildID, Provider: r.Provider, Model: r.Model, Depth: r.Depth,
			AccountRef: r.AccountRef, InstallRef: r.InstallRef, WindowKind: r.WindowKind,
			Permission: r.Permission, RouteReason: r.RouteReason,
			TaskClass: r.TaskClass, ExecutionPlanDigest: r.ExecutionPlanDigest,
			ChildContractDigest: r.ChildContractDigest, Generation: r.Generation,
			Terminal: "succeeded", OutputEvidence: r.OutputEvidence,
			WorktreePath: r.WorktreePath, AttemptID: r.AttemptID,
			ActualCapacity: r.CapacityActual, ActualSource: r.ActualSource,
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isResumeEligible(terminal, attemptID, evidence string) bool {
	return strings.EqualFold(strings.TrimSpace(terminal), "succeeded") &&
		strings.TrimSpace(attemptID) != "" &&
		strings.TrimSpace(evidence) != ""
}

func sanitizeRunKey(runID string) string {
	runID = strings.TrimSpace(runID)
	var b strings.Builder
	for _, r := range runID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		return "run"
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
