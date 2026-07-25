package workflowrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// PartialPrior is a durable mid-run snapshot so forced process kill can resume.
const PartialSchema = "loopcoder.workflow.partial.v1"

// PartialCheckpoint is written after each child terminal (fsync).
// It is a durable resume authority equal in attempt-set rank to goal-checkpoint
// for history retention: WorkflowKids holds every known ChildOutcome attempt
// (succeeded/failed/cancelled/interrupted/MU), not only PriorSucceeded + one
// Aborted map entry. Legacy partials without WorkflowKids cannot qualify a
// history-rich multi-attempt resume (fail closed — never invent from events alone).
//
// Persistence never reads/merges a prior partial file: callers (Service) must
// already hold the complete validated attempt set in Result.Children
// (Request.PriorOutcomes seeded + current-pass appends).
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
	GraphDigest string `json:"graph_digest,omitempty"`
	// GraphID / GraphVersion stamp current workgraph identity (exact envelope).
	GraphID      string    `json:"graph_id,omitempty"`
	GraphVersion int       `json:"graph_version,omitempty"`
	SavedAt      time.Time `json:"saved_at"`
	Interrupted  bool      `json:"interrupted,omitempty"`
	// PriorSucceeded is the succeeded-only reuse seed (evidence required).
	PriorSucceeded map[string]ChildOutcome `json:"prior_succeeded,omitempty"`
	// Aborted authorizes only the latest open/recovered attempt per work item
	// for generation bump + optional materialization of that single attempt.
	Aborted map[string]string `json:"aborted_attempts,omitempty"`
	// WorkflowKids is the complete attempt set known at write time — every
	// ChildOutcome with a nonempty AttemptID (all terminals). Required for
	// multi-restart partial-only resume of older aborted rows.
	WorkflowKids []ChildOutcome `json:"workflow_children,omitempty"`
	EventLogPath string         `json:"event_log_path,omitempty"`
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

// requirePartialWriteIdentity fails closed unless the partial would carry a
// complete resume authority identity. Never persist identity-less partials.
func requirePartialWriteIdentity(projectID, runID string, out Result) error {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if projectID == "" {
		return fmt.Errorf("workflowrun: partial checkpoint requires nonempty project_id")
	}
	if runID == "" {
		return fmt.Errorf("workflowrun: partial checkpoint requires nonempty run_id")
	}
	plan := strings.TrimSpace(out.PlanDigest)
	if plan == "" {
		return fmt.Errorf("workflowrun: partial checkpoint requires nonempty plan_digest")
	}
	graphDig := strings.TrimSpace(out.GraphDigest)
	if graphDig == "" {
		return fmt.Errorf("workflowrun: partial checkpoint requires nonempty graph_digest")
	}
	graphID := strings.TrimSpace(out.GraphID)
	if graphID == "" {
		return fmt.Errorf("workflowrun: partial checkpoint requires nonempty graph_id")
	}
	if out.GraphVersion <= 0 {
		return fmt.Errorf("workflowrun: partial checkpoint requires positive graph_version, got %d", out.GraphVersion)
	}
	return nil
}

// partialFS hooks allow package tests to inject open/sync/close failures for
// durability mutations. Production defaults are real os.File methods.
var (
	partialOpenDir   = func(name string) (*os.File, error) { return os.Open(name) }
	partialSyncFile  = func(f *os.File) error { return f.Sync() }
	partialCloseFile = func(f *os.File) error { return f.Close() }
)

// joinCloseRemove joins the primary error with close/remove cleanup failures.
// Never ignore close/remove after a failed fsync/write path.
func joinCloseRemove(primary error, closeErr, removeErr error) error {
	var errs []error
	if primary != nil {
		errs = append(errs, primary)
	}
	if closeErr != nil {
		errs = append(errs, fmt.Errorf("close: %w", closeErr))
	}
	if removeErr != nil {
		errs = append(errs, fmt.Errorf("remove: %w", removeErr))
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// writePartialPrior persists a mid-run snapshot with write+fsync+close+atomic rename
// and required directory fsync. It writes ONLY Result.Children as the complete
// WorkflowKids set — it never loads or merges an on-disk prior partial (stale or
// tampered kids must be rejected in resume preflight before spend, not trusted
// here). Empty/incomplete identity is fatal.
func writePartialPrior(homeDir, projectID, runID string, out Result) error {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if err := requirePartialWriteIdentity(projectID, runID, out); err != nil {
		return err
	}
	// Ensure run directory exists via event log open (same root as events).
	elog, err := OpenEventLog(homeDir, projectID, runID)
	if err != nil {
		return fmt.Errorf("workflowrun: partial checkpoint event log open: %w", err)
	}
	// Complete attempt set is exactly out.Children (caller seeded PriorOutcomes +
	// current-pass appends). Empty AttemptID/WorkItemID/terminal is fatal (never skip).
	// One row per AttemptID; full-struct equality on dupes. Exact canonical terminal.
	// At most one succeeded+evidence row per work item (deterministic; reject >1).
	seenAtt := map[string]ChildOutcome{}
	for i, c := range out.Children {
		if c.AttemptID == "" {
			return fmt.Errorf("workflowrun: partial checkpoint Children[%d] empty AttemptID", i)
		}
		if c.WorkItemID == "" {
			return fmt.Errorf("workflowrun: partial checkpoint Children[%d] empty WorkItemID", i)
		}
		if c.Terminal == "" {
			return fmt.Errorf("workflowrun: partial checkpoint Children[%d] empty terminal", i)
		}
		switch c.Terminal {
		case "succeeded", "failed", "cancelled", "skipped":
		default:
			return fmt.Errorf("workflowrun: partial checkpoint Children[%d] invalid terminal %q (exact succeeded|failed|cancelled|skipped)", i, c.Terminal)
		}
		if prev, ok := seenAtt[c.AttemptID]; ok {
			if !reflect.DeepEqual(prev, c) {
				return fmt.Errorf("workflowrun: partial checkpoint conflicting ChildOutcome for attempt %s", c.AttemptID)
			}
			continue
		}
		seenAtt[c.AttemptID] = c
	}
	atts := make([]string, 0, len(seenAtt))
	for att := range seenAtt {
		atts = append(atts, att)
	}
	sort.Strings(atts)
	kids := make([]ChildOutcome, 0, len(atts))
	prior := map[string]ChildOutcome{}
	succeededAttByWI := map[string]string{}
	for _, att := range atts {
		c := seenAtt[att]
		kids = append(kids, c)
		if c.Terminal == "succeeded" && strings.TrimSpace(c.OutputEvidence) != "" {
			wi := c.WorkItemID
			if prevAtt, ok := succeededAttByWI[wi]; ok && prevAtt != att {
				return fmt.Errorf("workflowrun: partial checkpoint multiple succeeded rows for work_item %s (%s and %s)", wi, prevAtt, att)
			}
			succeededAttByWI[wi] = att
			prior[wi] = c
		}
	}
	planDig := strings.TrimSpace(out.PlanDigest)
	cp := PartialCheckpoint{
		Schema: PartialSchema, ProjectID: projectID, RunID: runID,
		PlanDigest: planDig, ExecutionPlanDigest: planDig,
		GraphDigest: strings.TrimSpace(out.GraphDigest),
		GraphID:     strings.TrimSpace(out.GraphID), GraphVersion: out.GraphVersion,
		SavedAt: time.Now().UTC(), Interrupted: out.Interrupted,
		PriorSucceeded: prior, Aborted: out.AbortedAttempts,
		WorkflowKids: kids, EventLogPath: elog.Path(),
	}
	if err := requirePartialDocumentIdentity(cp); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return fmt.Errorf("workflowrun: partial checkpoint marshal: %w", err)
	}
	path := filepath.Join(filepath.Dir(elog.Path()), "workflow-partial.json")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("workflowrun: partial checkpoint create tmp: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		cerr := f.Close()
		rerr := os.Remove(tmp)
		return fmt.Errorf("workflowrun: partial checkpoint write: %w", joinCloseRemove(err, cerr, rerr))
	}
	if err := partialSyncFile(f); err != nil {
		cerr := f.Close()
		rerr := os.Remove(tmp)
		return fmt.Errorf("workflowrun: partial checkpoint fsync tmp: %w", joinCloseRemove(err, cerr, rerr))
	}
	if err := partialCloseFile(f); err != nil {
		rerr := os.Remove(tmp)
		return fmt.Errorf("workflowrun: partial checkpoint close tmp: %w", joinCloseRemove(err, nil, rerr))
	}
	if err := os.Rename(tmp, path); err != nil {
		rerr := os.Remove(tmp)
		return fmt.Errorf("workflowrun: partial checkpoint rename: %w", joinCloseRemove(err, nil, rerr))
	}
	// fsync final path so durability is observable on restart.
	f2, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("workflowrun: partial checkpoint reopen: %w", err)
	}
	if err := partialSyncFile(f2); err != nil {
		cerr := f2.Close()
		return fmt.Errorf("workflowrun: partial checkpoint fsync final: %w", joinCloseRemove(err, cerr, nil))
	}
	if err := partialCloseFile(f2); err != nil {
		return fmt.Errorf("workflowrun: partial checkpoint close final: %w", err)
	}
	// Directory open/Sync/Close errors are fatal — never best-effort ignore.
	dirPath := filepath.Dir(path)
	dir, err := partialOpenDir(dirPath)
	if err != nil {
		return fmt.Errorf("workflowrun: partial checkpoint open dir: %w", err)
	}
	if err := partialSyncFile(dir); err != nil {
		cerr := dir.Close()
		return fmt.Errorf("workflowrun: partial checkpoint fsync dir: %w", joinCloseRemove(err, cerr, nil))
	}
	if err := partialCloseFile(dir); err != nil {
		return fmt.Errorf("workflowrun: partial checkpoint close dir: %w", err)
	}
	return nil
}

// requirePartialDocumentIdentity validates a PartialCheckpoint document before
// or after marshal (write path and tests).
func requirePartialDocumentIdentity(cp PartialCheckpoint) error {
	if strings.TrimSpace(cp.ProjectID) == "" {
		return fmt.Errorf("workflowrun: partial document project_id empty")
	}
	if strings.TrimSpace(cp.RunID) == "" {
		return fmt.Errorf("workflowrun: partial document run_id empty")
	}
	plan := strings.TrimSpace(cp.PlanDigest)
	exec := strings.TrimSpace(cp.ExecutionPlanDigest)
	if plan == "" || exec == "" {
		return fmt.Errorf("workflowrun: partial document plan digests empty")
	}
	if plan != exec {
		return fmt.Errorf("workflowrun: partial document plan_digest %q != execution_plan_digest %q", plan, exec)
	}
	if strings.TrimSpace(cp.GraphDigest) == "" {
		return fmt.Errorf("workflowrun: partial document graph_digest empty")
	}
	if strings.TrimSpace(cp.GraphID) == "" {
		return fmt.Errorf("workflowrun: partial document graph_id empty")
	}
	if cp.GraphVersion <= 0 {
		return fmt.Errorf("workflowrun: partial document graph_version invalid: %d", cp.GraphVersion)
	}
	return nil
}

// LoadPartialPrior loads mid-run snapshot if present.
// Read-only: never calls OpenEventLog, never creates directories/files.
// Missing path returns an os.ErrNotExist-compatible error and leaves the
// filesystem byte-for-byte/path-for-path unchanged.
func LoadPartialPrior(homeDir, projectID, runID string) (PartialCheckpoint, error) {
	projectID = strings.TrimSpace(projectID)
	runID = strings.TrimSpace(runID)
	if projectID == "" || runID == "" {
		return PartialCheckpoint{}, fmt.Errorf("workflowrun: load partial requires nonempty project_id and run_id")
	}
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
