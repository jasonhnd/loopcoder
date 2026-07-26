package goalrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestProjectMissingAttemptRows_AssertionOnly_RequiresPersistedRow proves
// goalrun never invents ChildOutcome from events: missing AbortedAttempts
// backing row fails closed; when the exact row is already present, only a
// ChildReport is projected.
func TestProjectMissingAttemptRows_AssertionOnly_RequiresPersistedRow(t *testing.T) {
	now := time.Date(2026, 7, 25, 22, 0, 0, 0, time.UTC)
	home := t.TempDir()
	projectID := "proj-abort-proj"
	runID := "run_abort_1"
	planDig := "sha256:" + strings.Repeat("ab", 32)
	graphDig := "sha256:" + strings.Repeat("cd", 32)
	graphID := "g_abort"
	ccd := "sha256:" + strings.Repeat("ef", 32)
	attG0 := workflowrun.AttemptID("wi_tests", planDig, runID, 0)
	dir := filepath.Join(home, "projects", projectID, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "workflow-events.jsonl")
	mk := func(kind string, extra map[string]any) workflowrun.Event {
		e := workflowrun.Event{
			Schema: workflowrun.EventSchema, ProjectID: projectID, RunID: runID,
			Kind: kind, WorkItemID: "wi_tests", AttemptID: attG0, Generation: 1,
			ExecutionPlanDigest: planDig, GraphDigest: graphDig,
			GraphID: graphID, GraphVersion: 1,
			TaskClass: "tera", ChildContractDigest: ccd,
			EventID: "wev_" + kind + "_g0", At: now,
		}
		if kind == "interrupt" || kind == "terminal" {
			e.Terminal = "cancelled"
			e.FailureClass = "forced_interrupt"
		}
		if kind == "pid" {
			e.PID = 4242
		}
		if extra != nil {
			raw, _ := json.Marshal(extra)
			e.Payload = raw
		}
		return e
	}
	var b strings.Builder
	for _, e := range []workflowrun.Event{
		{Schema: workflowrun.EventSchema, ProjectID: projectID, RunID: runID, Kind: "run.start", EventID: "wev_start", At: now},
		mk("claim", map[string]any{"provider": "codex", "model": "gpt-5.5", "depth": "medium", "permission": "bounded_write"}),
		mk("launch", map[string]any{"provider": "codex", "model": "gpt-5.5", "depth": "medium", "permission": "bounded_write"}),
		mk("pid", nil),
		mk("interrupt", nil),
		mk("terminal", nil),
	} {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	id := lifecycleBindIdentity{
		ProjectID: projectID, RunID: runID,
		PlanDigest: planDig, GraphDigest: graphDig,
		GraphID: graphID, GraphVersion: 1,
	}
	// Missing persisted aborted row → assertion-only project fails (no event invent).
	wresMissing := workflowrun.Result{
		EventLogPath: logPath,
		Children: []workflowrun.ChildOutcome{{
			WorkItemID: "wi_docs", AttemptID: "att-other-g0", Generation: 1,
			TaskClass: "luna", ExecutionPlanDigest: planDig, ChildContractDigest: ccd,
			Terminal: "succeeded", Permission: "bounded_write",
		}},
		AbortedAttempts: map[string]string{"wi_tests": attG0},
	}
	byAttempt := map[string]workflowrun.ChildOutcome{
		"att-other-g0": wresMissing.Children[0],
	}
	children := []ChildReport{{
		ChildID: "wi_docs", AttemptID: "att-other-g0", Generation: 1,
		TaskClass: "luna", ExecutionPlanDigest: planDig, ChildContractDigest: ccd,
		Terminal: "succeeded", Permission: "bounded_write", Stage: "terminal",
	}}
	if _, _, _, err := projectMissingAttemptRows(children, wresMissing, byAttempt, id); err == nil ||
		!strings.Contains(err.Error(), "missing exact persisted ChildOutcome") {
		t.Fatalf("want missing persisted ChildOutcome fail closed, got %v", err)
	}

	// Workflowrun-persisted complete aborted row already present → report only.
	abortedCO := workflowrun.ChildOutcome{
		WorkItemID: "wi_tests", AttemptID: attG0, Generation: 1,
		TaskClass: "tera", ExecutionPlanDigest: planDig, ChildContractDigest: ccd,
		Provider: "codex", Model: "gpt-5.5", Depth: "medium", Permission: "bounded_write",
		Terminal: "cancelled", FailureClass: "forced_interrupt",
		OutputEvidence: "failed:forced_interrupt:wi_tests",
	}
	wres := workflowrun.Result{
		EventLogPath:    logPath,
		Children:        []workflowrun.ChildOutcome{abortedCO},
		AbortedAttempts: map[string]string{"wi_tests": attG0},
	}
	children2, wres2, byAttempt2, err := projectMissingAttemptRows(nil, wres, nil, id)
	if err != nil {
		t.Fatalf("project with persisted row: %v", err)
	}
	if len(wres2.Children) != 1 || wres2.Children[0].AttemptID != attG0 {
		t.Fatalf("must not invent/extra outcomes: %+v", wres2.Children)
	}
	found := 0
	for _, c := range children2 {
		if c.AttemptID == attG0 {
			found++
			if c.Terminal != "cancelled" {
				t.Fatalf("report terminal=%q", c.Terminal)
			}
			if c.IntegrateCommitSHA != "" || c.Stage == "integrated" {
				t.Fatalf("aborted report must not integrate: %+v", c)
			}
			if c.ChildID != "wi_tests" {
				t.Fatalf("work item: %+v", c)
			}
		}
	}
	if found != 1 {
		t.Fatalf("want exactly 1 aborted ChildReport for %s, found %d children=%+v", attG0, found, children2)
	}
	if _, ok := byAttempt2[attG0]; !ok {
		t.Fatal("byAttempt missing aborted outcome")
	}
	abortOnlyKids := []ChildReport{}
	for _, c := range children2 {
		if c.AttemptID == attG0 {
			abortOnlyKids = append(abortOnlyKids, c)
		}
	}
	abortOnlyByAtt := map[string]workflowrun.ChildOutcome{attG0: byAttempt2[attG0]}
	wAbort := workflowrun.Result{EventLogPath: logPath, Children: []workflowrun.ChildOutcome{byAttempt2[attG0]}}
	if err := bindAttemptLifecycleEvidence(abortOnlyKids, wAbort, abortOnlyByAtt, map[string]bool{}, id); err != nil {
		t.Fatalf("bind after assertion-only project: %v", err)
	}
	if err := requireUniqueAttemptIDs(children2); err != nil {
		t.Fatal(err)
	}
}

// TestProjectMissing_EmptyAbortedIsNoop: empty AbortedAttempts never invents rows
// from event-only terminal/MU lines.
func TestProjectMissing_EmptyAbortedIsNoop(t *testing.T) {
	now := time.Date(2026, 7, 25, 22, 30, 0, 0, time.UTC)
	home := t.TempDir()
	projectID := "proj-unlist"
	runID := "run_unlist"
	planDig := "sha256:" + strings.Repeat("aa", 32)
	graphDig := "sha256:" + strings.Repeat("bb", 32)
	graphID := "g_unlist"
	ccd := "sha256:" + strings.Repeat("cc", 32)
	attOther := workflowrun.AttemptID("wi_other", planDig, runID, 0)
	dir := filepath.Join(home, "projects", projectID, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "workflow-events.jsonl")
	mk := func(kind, term, fc string) workflowrun.Event {
		return workflowrun.Event{
			Schema: workflowrun.EventSchema, ProjectID: projectID, RunID: runID,
			Kind: kind, WorkItemID: "wi_other", AttemptID: attOther, Generation: 1,
			ExecutionPlanDigest: planDig, GraphDigest: graphDig,
			GraphID: graphID, GraphVersion: 1,
			TaskClass: "tera", ChildContractDigest: ccd,
			Terminal: term, FailureClass: fc,
			EventID: "wev_" + kind, At: now,
		}
	}
	var b strings.Builder
	for _, e := range []workflowrun.Event{
		{Schema: workflowrun.EventSchema, ProjectID: projectID, RunID: runID, Kind: "run.start", EventID: "wev_start", At: now},
		mk("launch", "", ""),
		mk("terminal", "failed", "unknown_class"),
		mk("model_unavailable", "failed", "model_unavailable"),
	} {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	id := lifecycleBindIdentity{
		ProjectID: projectID, RunID: runID,
		PlanDigest: planDig, GraphDigest: graphDig,
		GraphID: graphID, GraphVersion: 1,
	}
	wres := workflowrun.Result{EventLogPath: logPath, Children: nil, AbortedAttempts: nil}
	children, wres2, byAtt, err := projectMissingAttemptRows(nil, wres, nil, id)
	if err != nil {
		t.Fatalf("empty aborted: %v", err)
	}
	if len(children) != 0 || len(wres2.Children) != 0 || len(byAtt) != 0 {
		t.Fatalf("unlisted events must not materialize: children=%+v wres=%+v byAtt=%+v", children, wres2.Children, byAtt)
	}
}
