package workflowrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PartialPrior is a durable mid-run snapshot so forced process kill can resume.
const PartialSchema = "loopcoder.workflow.partial.v1"

// PartialCheckpoint is written after each child terminal (fsync).
type PartialCheckpoint struct {
	Schema    string `json:"schema"`
	ProjectID string `json:"project_id"`
	RunID     string `json:"run_id"`
	// PlanDigest / ExecutionPlanDigest are the canonical workflowdef.Normalize
	// digest (same value). Populated from Result top-level — child outcomes alone
	// are insufficient for durable parent identity.
	PlanDigest          string `json:"plan_digest,omitempty"`
	ExecutionPlanDigest string `json:"execution_plan_digest,omitempty"`
	// GraphDigest is workgraph.DigestGraph (never conflated with PlanDigest).
	GraphDigest    string                  `json:"graph_digest,omitempty"`
	SavedAt        time.Time               `json:"saved_at"`
	Interrupted    bool                    `json:"interrupted,omitempty"`
	PriorSucceeded map[string]ChildOutcome `json:"prior_succeeded,omitempty"`
	Aborted        map[string]string       `json:"aborted_attempts,omitempty"`
	EventLogPath   string                  `json:"event_log_path,omitempty"`
}

// writePartialPriorPath is the durable path for a partial checkpoint under the
// run directory. Pure derivation — does not create directories.
func writePartialPriorPath(homeDir, projectID, runID string) (string, error) {
	dir, err := RunDurableDir(homeDir, projectID, runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "workflow-partial.json"), nil
}

func writePartialPrior(homeDir, projectID, runID string, out Result) error {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if projectID == "" || runID == "" {
		return nil
	}
	// Write path: ensure run directory exists via event log open (same root as events).
	elog, err := OpenEventLog(homeDir, projectID, runID)
	if err != nil {
		return err
	}
	prior := map[string]ChildOutcome{}
	for _, c := range out.Children {
		if strings.EqualFold(c.Terminal, "succeeded") &&
			strings.TrimSpace(c.AttemptID) != "" && strings.TrimSpace(c.OutputEvidence) != "" {
			prior[c.WorkItemID] = c
		}
	}
	planDig := strings.TrimSpace(out.PlanDigest)
	cp := PartialCheckpoint{
		Schema: PartialSchema, ProjectID: projectID, RunID: runID,
		PlanDigest: planDig, ExecutionPlanDigest: planDig,
		GraphDigest: strings.TrimSpace(out.GraphDigest),
		SavedAt:     time.Now().UTC(), Interrupted: out.Interrupted,
		PriorSucceeded: prior, Aborted: out.AbortedAttempts, EventLogPath: elog.Path(),
	}
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(elog.Path()), "workflow-partial.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// best-effort fsync via reopen
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
	return nil
}

// LoadPartialPrior loads mid-run snapshot if present.
// Read-only: never calls OpenEventLog, never creates directories/files.
// Missing path returns an os.ErrNotExist-compatible error and leaves the
// filesystem byte-for-byte/path-for-path unchanged.
func LoadPartialPrior(homeDir, projectID, runID string) (PartialCheckpoint, error) {
	path, err := writePartialPriorPath(homeDir, projectID, runID)
	if err != nil {
		return PartialCheckpoint{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return PartialCheckpoint{}, err
	}
	var cp PartialCheckpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return PartialCheckpoint{}, err
	}
	return cp, nil
}
