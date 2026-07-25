package goalrun

import (
	"fmt"
	"strings"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// kidsEnvelope is the durable identity stamp of a checkpoint or partial that
// supplies WorkflowKids for multi-restart resume merge.
type kidsEnvelope struct {
	Label        string // "checkpoint" | "partial"
	ProjectID    string
	RunID        string
	PlanDigest   string
	GraphDigest  string
	GraphID      string
	GraphVersion int // when >0, must match current id.GraphVersion
	EventLogPath string
	Kids         []workflowrun.ChildOutcome
}

// mergeValidatedCheckpointWorkflowKids merges goal-checkpoint WorkflowKids.
func mergeValidatedCheckpointWorkflowKids(
	wres workflowrun.Result,
	cp Checkpoint,
	id lifecycleBindIdentity,
) (workflowrun.Result, error) {
	return mergeValidatedKidsEnvelope(wres, kidsEnvelope{
		Label: "checkpoint", ProjectID: cp.ProjectID, RunID: cp.RunID,
		PlanDigest: cp.PlanDigest, GraphDigest: cp.GraphDigest, GraphID: cp.GraphID,
		EventLogPath: cp.EventLogPath, Kids: cp.WorkflowKids,
	}, id)
}

// mergeValidatedPartialWorkflowKids merges workflow-partial.json WorkflowKids
// under the same validation rules as goal-checkpoint. Legacy partials with
// empty WorkflowKids return wres unchanged (history-rich bind will fail closed
// later if events reference unlisted attempts).
func mergeValidatedPartialWorkflowKids(
	wres workflowrun.Result,
	part workflowrun.PartialCheckpoint,
	id lifecycleBindIdentity,
) (workflowrun.Result, error) {
	return mergeValidatedKidsEnvelope(wres, kidsEnvelope{
		Label: "partial", ProjectID: part.ProjectID, RunID: part.RunID,
		PlanDigest:   firstNonEmpty(part.PlanDigest, part.ExecutionPlanDigest),
		GraphDigest:  part.GraphDigest,
		GraphID:      part.GraphID,
		GraphVersion: part.GraphVersion,
		EventLogPath: part.EventLogPath,
		Kids:         part.WorkflowKids,
	}, id)
}

// mergeValidatedKidsEnvelope is the authoritative all-attempt resume projection
// shared by goal-checkpoint and workflow-partial. Every kid is envelope-validated
// against current project/run/plan/graph identity and cross-bound to matching
// event-log lifecycle evidence, then merged by exact AttemptID. Exact-equal
// dedupe; conflicts fail closed. No event alone may create a row.
func mergeValidatedKidsEnvelope(
	wres workflowrun.Result,
	env kidsEnvelope,
	id lifecycleBindIdentity,
) (workflowrun.Result, error) {
	if len(env.Kids) == 0 {
		return wres, nil
	}
	id.ProjectID = strings.TrimSpace(id.ProjectID)
	id.RunID = strings.TrimSpace(id.RunID)
	id.PlanDigest = strings.TrimSpace(id.PlanDigest)
	id.GraphDigest = strings.TrimSpace(id.GraphDigest)
	id.GraphID = strings.TrimSpace(id.GraphID)
	if id.ProjectID == "" || id.RunID == "" {
		return wres, fmt.Errorf("goalrun: %s kids merge: project_id/run_id required", env.Label)
	}
	if id.PlanDigest == "" || id.GraphDigest == "" || id.GraphID == "" || id.GraphVersion <= 0 {
		return wres, fmt.Errorf("goalrun: %s kids merge: plan/graph identity required (never git SHA)", env.Label)
	}
	if strings.TrimSpace(env.ProjectID) != "" && !idExact(env.ProjectID, id.ProjectID) {
		return wres, fmt.Errorf("goalrun: %s kids merge: project_id %q != current %q", env.Label, env.ProjectID, id.ProjectID)
	}
	if strings.TrimSpace(env.RunID) != "" && !idExact(env.RunID, id.RunID) {
		return wres, fmt.Errorf("goalrun: %s kids merge: run_id %q != current %q", env.Label, env.RunID, id.RunID)
	}
	if strings.TrimSpace(env.PlanDigest) != "" && env.PlanDigest != id.PlanDigest {
		return wres, fmt.Errorf("goalrun: %s kids merge: plan_digest mismatch", env.Label)
	}
	if strings.TrimSpace(env.GraphDigest) != "" && env.GraphDigest != id.GraphDigest {
		return wres, fmt.Errorf("goalrun: %s kids merge: graph_digest mismatch", env.Label)
	}
	if strings.TrimSpace(env.GraphID) != "" && env.GraphID != id.GraphID {
		return wres, fmt.Errorf("goalrun: %s kids merge: graph_id %q != current %q", env.Label, env.GraphID, id.GraphID)
	}
	if env.GraphVersion > 0 && env.GraphVersion != id.GraphVersion {
		return wres, fmt.Errorf("goalrun: %s kids merge: graph_version %d != current %d", env.Label, env.GraphVersion, id.GraphVersion)
	}

	// Post-Execute defense-in-depth only: exact-dedupe/assert by AttemptID.
	// Full pre-spend validation already ran on PriorOutcomes; do NOT re-bind
	// events with a softer path or refresh capacity from durable kids.
	idx := map[string]int{}
	for i, c := range wres.Children {
		att := c.AttemptID
		if att == "" {
			return wres, fmt.Errorf("goalrun: %s kids merge: current child missing attempt_id work_item=%q", env.Label, c.WorkItemID)
		}
		if prev, ok := idx[att]; ok {
			return wres, fmt.Errorf("goalrun: %s kids merge: duplicate current AttemptID %q at %d and %d", env.Label, att, prev, i)
		}
		idx[att] = i
	}

	for _, kid := range env.Kids {
		att := kid.AttemptID
		if att == "" || kid.WorkItemID == "" {
			return wres, fmt.Errorf("goalrun: %s kids merge: kid missing work_item/attempt", env.Label)
		}
		if !exactCanonicalTerminal(kid.Terminal) {
			return wres, fmt.Errorf("goalrun: %s kids merge attempt %s invalid terminal %q", env.Label, att, kid.Terminal)
		}
		if i, ok := idx[att]; ok {
			if err := childOutcomesExactlyEqual(wres.Children[i], kid, env.Label+"-vs-current:"+att); err != nil {
				return wres, fmt.Errorf("goalrun: %s kids merge conflicting attempt %s: %w", env.Label, att, err)
			}
			continue
		}
		// Historical row not already seeded — append only after identity envelope check.
		if err := validateCheckpointKidEnvelope(kid, id); err != nil {
			return wres, fmt.Errorf("goalrun: %s kids merge attempt %s: %w", env.Label, att, err)
		}
		wres.Children = append(wres.Children, kid)
		idx[att] = len(wres.Children) - 1
	}
	return wres, nil
}

// validateCheckpointKidEnvelope requires canonical attempt, positive generation,
// exact class/plan/CCD and exact canonical terminal.
func validateCheckpointKidEnvelope(kid workflowrun.ChildOutcome, id lifecycleBindIdentity) error {
	if err := requireTerminalOutcomeIdentity(kid.WorkItemID, kid); err != nil {
		return err
	}
	if err := requireCanonicalOutcomeAttempt(kid, id.PlanDigest, id.RunID); err != nil {
		return err
	}
	if kid.ExecutionPlanDigest != id.PlanDigest {
		return fmt.Errorf("execution_plan_digest %q != current %q", kid.ExecutionPlanDigest, id.PlanDigest)
	}
	if !exactCanonicalTerminal(kid.Terminal) {
		return fmt.Errorf("invalid terminal %q (exact succeeded|failed|cancelled|skipped)", kid.Terminal)
	}
	if kid.Terminal == string(workgraph.TermSucceeded) && kid.OutputEvidence == "" {
		return fmt.Errorf("succeeded kid missing output_evidence")
	}
	return nil
}

// ensureChildReportsForOutcomes appends ChildReport rows for every
// Workflow.Children AttemptID missing from the report list (universal surface).
func ensureChildReportsForOutcomes(children []ChildReport, outcomes []workflowrun.ChildOutcome) []ChildReport {
	seen := map[string]bool{}
	template := map[string]ChildReport{}
	for _, cr := range children {
		if a := cr.AttemptID; a != "" && a == strings.TrimSpace(a) {
			seen[a] = true
		}
		if _, ok := template[cr.ChildID]; !ok {
			template[cr.ChildID] = cr
		}
	}
	for _, co := range outcomes {
		att := co.AttemptID
		if att == "" || att != strings.TrimSpace(att) || seen[att] {
			// Padded attempt_id never projects (caller fails closed on authority paths).
			continue
		}
		cr := childReportFromOutcome(co)
		if base, ok := template[cr.ChildID]; ok {
			if cr.Intent == "" {
				cr.Intent = base.Intent
			}
			if cr.Owner == "" {
				cr.Owner = base.Owner
			}
			if cr.RouteRequirement == "" {
				cr.RouteRequirement = base.RouteRequirement
			}
		}
		children = append(children, cr)
		seen[att] = true
	}
	return children
}
