package goalrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestPartialOnlyTwoRestart_G0G1AbortedG2Succeeded: remove goal-checkpoint after
// each interrupted pass; resume solely from workflow-partial.json which must
// persist the complete WorkflowKids attempt set across g0 interrupt → g1
// interrupt → g2 success (three unique rows, one launch each).
func TestPartialOnlyTwoRestart_G0G1AbortedG2Succeeded(t *testing.T) {
	now := time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-partial-two-restart"
	runID := "run_partial_two_restart_1"
	goal := "implement partial-only two restart attempt retention"
	oneChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_partial_two", Version: 1,
			Source: workgraph.SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: "owner",
			Items: []workgraph.WorkItem{{
				Schema: workgraph.SchemaItem, ID: "wi_only", Status: workgraph.ItemRequired,
				Intent: "implement", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
				IntegrationOrder: 1, OutputContract: "diff",
				RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
			}},
			Limits: workgraph.DefaultLimits(), CreatedAt: now,
		}
		g.PlanDigest = workgraph.DigestGraph(g)
		return g, nil
	}
	dropGoalCP := func() {
		t.Helper()
		cpPath := filepath.Join(home, "projects", projectID, "runs", runID, "goal-checkpoint.json")
		_ = os.Remove(cpPath)
		if _, _, err := goalrun.LoadCheckpoint(home, projectID, runID); err == nil {
			t.Fatal("goal-checkpoint must be absent for partial-only resume")
		}
	}
	requirePartialKids := func(label string, wantAtts ...string) {
		t.Helper()
		part, err := workflowrun.LoadPartialPrior(home, projectID, runID)
		if err != nil {
			t.Fatalf("%s load partial: %v", label, err)
		}
		if len(part.WorkflowKids) == 0 {
			t.Fatalf("%s partial.WorkflowKids empty (legacy partial cannot qualify history-rich resume)", label)
		}
		for _, att := range wantAtts {
			found := false
			for _, k := range part.WorkflowKids {
				if k.AttemptID == att {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("%s partial missing attempt %s kids=%+v", label, att, part.WorkflowKids)
			}
		}
	}

	// --- Pass 1: interrupt g0; drop goal-checkpoint; partial must hold g0 ---
	ctx1, cancel1 := context.WithCancel(context.Background())
	calls1 := map[string]int{}
	res1, err1 := goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls: calls1, HangIDs: map[string]bool{"wi_only": true},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == "wi_only" {
					cancel1()
				}
			},
		},
	})
	if err1 == nil && res1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("pass1 expected interrupt: status=%s", res1.Status)
	}
	if calls1["wi_only"] != 1 {
		t.Fatalf("pass1 calls=%+v", calls1)
	}
	attG0 := attemptBySuffix(t, res1.Workflow.Children, "wi_only", "-g0")
	if attG0 == "" {
		elog, _ := workflowrun.OpenEventLog(home, projectID, runID)
		evs, _ := elog.ReadAll()
		for _, e := range evs {
			if e.Kind == "launch" && e.WorkItemID == "wi_only" && strings.HasSuffix(e.AttemptID, "-g0") {
				attG0 = e.AttemptID
			}
		}
	}
	if attG0 == "" {
		t.Fatalf("pass1 g0 missing: %+v", res1.Workflow.Children)
	}
	dropGoalCP()
	requirePartialKids("pass1", attG0)

	// --- Pass 2: partial-only resume, interrupt g1 ---
	ctx2, cancel2 := context.WithCancel(context.Background())
	calls2 := map[string]int{}
	res2, err2 := goalrun.Execute(ctx2, goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     oneChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2, HangIDs: map[string]bool{"wi_only": true},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == "wi_only" {
					cancel2()
				}
			},
		},
	})
	if err2 == nil && res2.Status == workflowrun.StatusHumanGate {
		t.Fatalf("pass2 expected interrupt: status=%s msg=%s", res2.Status, res2.Message)
	}
	if !res2.Resumed {
		t.Fatalf("pass2 expected Resumed from partial")
	}
	if calls2["wi_only"] != 1 {
		t.Fatalf("pass2 calls=%+v want 1", calls2)
	}
	assertAttemptPresentNonSuccess(t, res2.Workflow.Children, attG0, "pass2 workflow")
	attG1 := maxGenAttempt(t, res2.Workflow.Children, "wi_only")
	if attG1 == "" || attG1 == attG0 || !strings.HasSuffix(attG1, "-g1") {
		t.Fatalf("pass2 want g1 got %q children=%+v", attG1, res2.Workflow.Children)
	}
	dropGoalCP()
	requirePartialKids("pass2", attG0, attG1)

	// --- Pass 3: partial-only resume, succeed g2 ---
	calls3 := map[string]int{}
	res3, err3 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(2 * time.Minute) },
		Decompose:     oneChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(2 * time.Minute) },
			Calls: calls3,
		},
	})
	if err3 != nil {
		t.Fatalf("pass3: %v status=%s msg=%s", err3, res3.Status, res3.Message)
	}
	if calls3["wi_only"] != 1 {
		t.Fatalf("pass3 calls=%+v want 1", calls3)
	}
	assertAttemptPresentNonSuccess(t, res3.Workflow.Children, attG0, "pass3 g0")
	assertAttemptPresentNonSuccess(t, res3.Workflow.Children, attG1, "pass3 g1")
	attG2 := maxGenAttempt(t, res3.Workflow.Children, "wi_only")
	if attG2 == "" || !strings.HasSuffix(attG2, "-g2") {
		t.Fatalf("pass3 want g2 got %q children=%+v", attG2, res3.Workflow.Children)
	}
	if !strings.EqualFold(terminalOf(res3.Workflow.Children, attG2), "succeeded") {
		t.Fatalf("pass3 g2 terminal=%q", terminalOf(res3.Workflow.Children, attG2))
	}
	seen := map[string]int{}
	for _, c := range res3.Workflow.Children {
		if c.WorkItemID == "wi_only" {
			seen[c.AttemptID]++
		}
	}
	for att, n := range seen {
		if n != 1 {
			t.Fatalf("duplicate %s count=%d", att, n)
		}
	}
	if len(seen) < 3 {
		t.Fatalf("want 3 attempts got %d: %v", len(seen), seen)
	}
	// Reports + partial agree.
	for _, att := range []string{attG0, attG1, attG2} {
		assertReportAttempt(t, res3.Children, att, "pass3 report")
	}
	part3, err := workflowrun.LoadPartialPrior(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, att := range []string{attG0, attG1, attG2} {
		found := false
		for _, k := range part3.WorkflowKids {
			if k.AttemptID == att {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("final partial missing %s kids=%+v", att, part3.WorkflowKids)
		}
	}
	// One launch each.
	elog, err := workflowrun.OpenEventLog(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := elog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	launches := map[string]int{}
	for _, e := range evs {
		if e.Kind == "launch" && e.WorkItemID == "wi_only" {
			launches[e.AttemptID]++
		}
	}
	for _, att := range []string{attG0, attG1, attG2} {
		if launches[att] != 1 {
			t.Fatalf("launch %s count=%d want 1: %v", att, launches[att], launches)
		}
	}
}

// TestLegacyPartialWithoutWorkflowKids_HistoryRichFailsClosed: partial with only
// PriorSucceeded/Aborted and no WorkflowKids cannot resume when event log has
// historical aborted attempts that need kid rows.
func TestLegacyPartialWithoutWorkflowKids_HistoryRichFailsClosed(t *testing.T) {
	now := time.Date(2026, 7, 25, 15, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-legacy-partial"
	runID := "run_legacy_partial"
	goal := "implement legacy partial fail closed"
	// Build a real interrupted pass first to get digests + events.
	oneChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_legacy_part", Version: 1,
			Source: workgraph.SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: "owner",
			Items: []workgraph.WorkItem{{
				Schema: workgraph.SchemaItem, ID: "wi_only", Status: workgraph.ItemRequired,
				Intent: "implement", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
				IntegrationOrder: 1, OutputContract: "diff",
				RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
			}},
			Limits: workgraph.DefaultLimits(), CreatedAt: now,
		}
		g.PlanDigest = workgraph.DigestGraph(g)
		return g, nil
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	_, _ = goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			HangIDs: map[string]bool{"wi_only": true},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == "wi_only" {
					cancel1()
				}
			},
		},
	})
	// Strip goal-checkpoint and rewrite partial WITHOUT WorkflowKids (legacy shape).
	_ = os.Remove(filepath.Join(home, "projects", projectID, "runs", runID, "goal-checkpoint.json"))
	part, err := workflowrun.LoadPartialPrior(home, projectID, runID)
	if err != nil {
		t.Fatalf("load partial after pass1: %v", err)
	}
	if len(part.WorkflowKids) == 0 {
		t.Fatal("pass1 partial WorkflowKids empty — cannot prove legacy strip (required mutation)")
	}
	legacy := workflowrun.PartialCheckpoint{
		Schema: workflowrun.PartialSchema, ProjectID: part.ProjectID, RunID: part.RunID,
		PlanDigest: part.PlanDigest, ExecutionPlanDigest: part.ExecutionPlanDigest,
		GraphDigest: part.GraphDigest, GraphID: part.GraphID, GraphVersion: part.GraphVersion,
		SavedAt: part.SavedAt, Interrupted: true,
		PriorSucceeded: part.PriorSucceeded, Aborted: part.Aborted,
		EventLogPath: part.EventLogPath,
		// WorkflowKids intentionally omitted — legacy.
	}
	raw, _ := json.MarshalIndent(legacy, "", "  ")
	partPath := filepath.Join(home, "projects", projectID, "runs", runID, "workflow-partial.json")
	if err := os.WriteFile(partPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Resume must fail closed (cannot invent g0 row from events alone).
	_, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     oneChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		},
	})
	if err2 == nil {
		t.Fatal("legacy partial without WorkflowKids must fail closed on history-rich resume")
	}
	if !strings.Contains(err2.Error(), "ChildOutcome") &&
		!strings.Contains(err2.Error(), "no matching") &&
		!strings.Contains(err2.Error(), "fail closed") &&
		!strings.Contains(err2.Error(), "attempt") {
		t.Logf("got error (acceptable fail-closed): %v", err2)
	}
}
