package goalrun

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestProjectMissing_ConflictDeepEqual(t *testing.T) {
	id := testLifeID()
	plan, runID := id.PlanDigest, id.RunID
	ccd := "sha256:" + strings.Repeat("cc", 32)
	att := workflowrun.AttemptID("wi_only", plan, runID, 0)
	base := workflowrun.ChildOutcome{
		WorkItemID: "wi_only", AttemptID: att, Generation: 1,
		TaskClass: "tera", ExecutionPlanDigest: plan, ChildContractDigest: ccd,
		Depth: "medium", Permission: "bounded_write",
		Provider: "codex", Model: "m",
		Terminal: "cancelled", FailureClass: "forced_interrupt",
		OutputEvidence: "failed:forced_interrupt:wi_only",
	}
	drift := base
	drift.Message = "different"
	// byAttempt has base; Children has drift for same AttemptID.
	byAttempt := map[string]workflowrun.ChildOutcome{att: base}
	wres := workflowrun.Result{
		Children:        []workflowrun.ChildOutcome{drift},
		AbortedAttempts: map[string]string{"wi_only": att},
	}
	children, _, _, err := projectMissingAttemptRows(nil, wres, byAttempt, id)
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("want conflicting ChildOutcome fail closed, got err=%v children=%+v", err, children)
	}
	if len(children) != 0 {
		t.Fatalf("no report projection on conflict: %+v", children)
	}
}

func TestProjectMissing_DuplicateChildrenConflict(t *testing.T) {
	id := testLifeID()
	plan, runID := id.PlanDigest, id.RunID
	ccd := "sha256:" + strings.Repeat("cc", 32)
	att := workflowrun.AttemptID("wi_only", plan, runID, 0)
	base := workflowrun.ChildOutcome{
		WorkItemID: "wi_only", AttemptID: att, Generation: 1,
		TaskClass: "tera", ExecutionPlanDigest: plan, ChildContractDigest: ccd,
		Depth: "medium", Permission: "bounded_write",
		Terminal: "cancelled", FailureClass: "forced_interrupt",
		OutputEvidence: "failed:forced_interrupt:wi_only",
	}
	drift := base
	drift.OutputEvidence = "other"
	wres := workflowrun.Result{
		Children:        []workflowrun.ChildOutcome{base, drift},
		AbortedAttempts: map[string]string{"wi_only": att},
	}
	children, _, _, err := projectMissingAttemptRows(nil, wres, nil, id)
	if err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("want duplicate conflict, got err=%v children=%+v", err, children)
	}
}

func TestProjectMissing_RequiresRuntimeIdentity(t *testing.T) {
	id := testLifeID()
	plan, runID := id.PlanDigest, id.RunID
	att := workflowrun.AttemptID("wi_only", plan, runID, 0)
	// Incomplete CCD/class → fail before report projection.
	incomplete := workflowrun.ChildOutcome{
		WorkItemID: "wi_only", AttemptID: att, Generation: 1,
		ExecutionPlanDigest: plan,
		Terminal:            "cancelled", FailureClass: "forced_interrupt",
	}
	wres := workflowrun.Result{
		Children:        []workflowrun.ChildOutcome{incomplete},
		AbortedAttempts: map[string]string{"wi_only": att},
	}
	children, _, _, err := projectMissingAttemptRows(nil, wres, nil, id)
	if err == nil {
		t.Fatal("incomplete identity must fail")
	}
	if len(children) != 0 {
		t.Fatalf("no report projection: %+v", children)
	}
}

func TestProjectMissing_DeepEqualOK_ProjectsReport(t *testing.T) {
	id := testLifeID()
	plan, runID := id.PlanDigest, id.RunID
	ccd := "sha256:" + strings.Repeat("cc", 32)
	att := workflowrun.AttemptID("wi_only", plan, runID, 0)
	base := workflowrun.ChildOutcome{
		WorkItemID: "wi_only", AttemptID: att, Generation: 1,
		TaskClass: "tera", ExecutionPlanDigest: plan, ChildContractDigest: ccd,
		Depth: "medium", Permission: "bounded_write",
		Provider: "codex", Model: "m",
		Terminal: "cancelled", FailureClass: "forced_interrupt",
		OutputEvidence: "failed:forced_interrupt:wi_only",
	}
	byAttempt := map[string]workflowrun.ChildOutcome{att: base}
	wres := workflowrun.Result{
		Children:        []workflowrun.ChildOutcome{base},
		AbortedAttempts: map[string]string{"wi_only": att},
	}
	children, _, byAtt, err := projectMissingAttemptRows(nil, wres, byAttempt, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].AttemptID != att {
		t.Fatalf("want one report projection: %+v", children)
	}
	if !reflect.DeepEqual(byAtt[att], base) {
		t.Fatal("byAttempt must remain exact base")
	}
}

func TestIdExact_ByteExactNoTrim(t *testing.T) {
	if idExact("a", "a") != true {
		t.Fatal("equal")
	}
	if idExact("a", " a") {
		t.Fatal("must not trim-equal")
	}
	if idExact("A", "a") {
		t.Fatal("must not case-fold")
	}
}

func TestIsResumeEligibleExact(t *testing.T) {
	if !isResumeEligibleExact("succeeded", "att-x", "ev") {
		t.Fatal("want accept exact")
	}
	if isResumeEligibleExact("Succeeded", "att-x", "ev") {
		t.Fatal("case-mutated terminal reject")
	}
	if isResumeEligibleExact("succeeded", " att-x", "ev") {
		t.Fatal("padded attempt reject")
	}
	if isResumeEligibleExact("succeeded", "att-x", " ev") {
		t.Fatal("padded evidence reject")
	}
}
