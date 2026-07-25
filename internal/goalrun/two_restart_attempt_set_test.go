package goalrun_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestTwoRestartAttemptSet_G0G1AbortedG2Succeeded proves multi-restart
// universal attempt retention without event-alone invent or AttemptID reuse:
//
//	pass1: g0 interrupted → checkpoint contains g0 non-success
//	pass2: g0 preserved + g1 interrupted → checkpoint contains g0+g1 non-success
//	pass3: g0+g1 non-success + g2 succeeded; each AttemptID unique; one launch each
func TestTwoRestartAttemptSet_G0G1AbortedG2Succeeded(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-two-restart"
	runID := "run_two_restart_1"
	goal := "implement two restart attempt set retention"
	oneChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_two_restart", Version: 1,
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

	// --- Pass 1: interrupt g0 ---
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
		t.Fatalf("pass1 expected interrupt: status=%s msg=%s", res1.Status, res1.Message)
	}
	if calls1["wi_only"] != 1 {
		t.Fatalf("pass1 calls=%+v want 1", calls1)
	}
	attG0 := attemptBySuffix(t, res1.Workflow.Children, "wi_only", "-g0")
	if attG0 == "" {
		// Fall back to event log launch.
		elog, err := workflowrun.OpenEventLog(home, projectID, runID)
		if err != nil {
			t.Fatal(err)
		}
		evs, err := elog.ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Kind == "launch" && e.WorkItemID == "wi_only" && strings.HasSuffix(e.AttemptID, "-g0") {
				attG0 = e.AttemptID
			}
		}
	}
	if attG0 == "" {
		t.Fatalf("pass1 g0 attempt missing children=%+v", res1.Workflow.Children)
	}
	cp1, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("pass1 checkpoint: %v", err)
	}
	assertAttemptPresentNonSuccess(t, cp1.WorkflowKids, attG0, "pass1 checkpoint")
	if _, ok := cp1.PriorSucceeded["wi_only"]; ok {
		t.Fatalf("pass1 must not list wi_only as PriorSucceeded: %+v", cp1.PriorSucceeded)
	}

	// --- Pass 2: resume, interrupt g1; must retain g0 ---
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
	if calls2["wi_only"] != 1 {
		t.Fatalf("pass2 calls=%+v want 1 (exactly one new execution)", calls2)
	}
	assertAttemptPresentNonSuccess(t, res2.Workflow.Children, attG0, "pass2 workflow")
	attG1 := maxGenAttempt(t, res2.Workflow.Children, "wi_only")
	if attG1 == "" || attG1 == attG0 || !strings.HasSuffix(attG1, "-g1") {
		t.Fatalf("pass2 max-gen want g1 got %q g0=%q children=%+v", attG1, attG0, res2.Workflow.Children)
	}
	if strings.EqualFold(terminalOf(res2.Workflow.Children, attG1), "succeeded") {
		t.Fatalf("pass2 g1 must not succeed")
	}
	cp2, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("pass2 checkpoint: %v", err)
	}
	assertAttemptPresentNonSuccess(t, cp2.WorkflowKids, attG0, "pass2 checkpoint g0")
	assertAttemptPresentNonSuccess(t, cp2.WorkflowKids, attG1, "pass2 checkpoint g1")
	if _, ok := cp2.PriorSucceeded["wi_only"]; ok {
		t.Fatalf("pass2 must not list wi_only PriorSucceeded: %+v", cp2.PriorSucceeded)
	}

	// --- Pass 3: resume, succeed g2; retain g0+g1 non-success ---
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
	assertAttemptPresentNonSuccess(t, res3.Workflow.Children, attG0, "pass3 workflow g0")
	assertAttemptPresentNonSuccess(t, res3.Workflow.Children, attG1, "pass3 workflow g1")
	attG2 := maxGenAttempt(t, res3.Workflow.Children, "wi_only")
	if attG2 == "" || !strings.HasSuffix(attG2, "-g2") {
		t.Fatalf("pass3 max-gen want g2 got %q children=%+v", attG2, res3.Workflow.Children)
	}
	if !strings.EqualFold(terminalOf(res3.Workflow.Children, attG2), "succeeded") {
		t.Fatalf("pass3 g2 want succeeded got %q", terminalOf(res3.Workflow.Children, attG2))
	}
	// Unique AttemptIDs; no duplicate work.
	seen := map[string]int{}
	for _, c := range res3.Workflow.Children {
		if c.WorkItemID != "wi_only" {
			continue
		}
		seen[c.AttemptID]++
	}
	for att, n := range seen {
		if n != 1 {
			t.Fatalf("duplicate AttemptID %s count=%d", att, n)
		}
	}
	if len(seen) < 3 {
		t.Fatalf("want >=3 distinct attempts (g0,g1,g2) got %d: %v", len(seen), seen)
	}
	// Event log: exactly one launch per generation attempt.
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
			t.Fatalf("launch count for %s = %d want 1 (no duplicate execution) launches=%v", att, launches[att], launches)
		}
	}
	// Reports and checkpoint agree on complete set.
	cp3, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("pass3 checkpoint: %v", err)
	}
	for _, att := range []string{attG0, attG1, attG2} {
		assertAttemptPresent(t, cp3.WorkflowKids, att, "pass3 checkpoint")
		assertReportAttempt(t, res3.Children, att, "pass3 reports")
	}
	if ps, ok := cp3.PriorSucceeded["wi_only"]; !ok || ps.AttemptID != attG2 {
		t.Fatalf("pass3 PriorSucceeded want g2 %s got %+v", attG2, cp3.PriorSucceeded)
	}
}

func attemptBySuffix(t *testing.T, kids []workflowrun.ChildOutcome, wi, suffix string) string {
	t.Helper()
	for _, c := range kids {
		if c.WorkItemID == wi && strings.HasSuffix(c.AttemptID, suffix) {
			return c.AttemptID
		}
	}
	return ""
}

func maxGenAttempt(t *testing.T, kids []workflowrun.ChildOutcome, wi string) string {
	t.Helper()
	var best string
	var maxG int
	for _, c := range kids {
		if c.WorkItemID != wi {
			continue
		}
		if c.Generation > maxG {
			maxG = c.Generation
			best = c.AttemptID
		}
	}
	return best
}

func terminalOf(kids []workflowrun.ChildOutcome, att string) string {
	for _, c := range kids {
		if c.AttemptID == att {
			return c.Terminal
		}
	}
	return ""
}

func assertAttemptPresentNonSuccess(t *testing.T, kids []workflowrun.ChildOutcome, att, label string) {
	t.Helper()
	for _, c := range kids {
		if c.AttemptID != att {
			continue
		}
		if strings.EqualFold(c.Terminal, "succeeded") {
			t.Fatalf("%s: attempt %s is succeeded (want non-success)", label, att)
		}
		return
	}
	t.Fatalf("%s: missing attempt %s in %+v", label, att, kids)
}

func assertAttemptPresent(t *testing.T, kids []workflowrun.ChildOutcome, att, label string) {
	t.Helper()
	for _, c := range kids {
		if c.AttemptID == att {
			return
		}
	}
	t.Fatalf("%s: missing attempt %s in %+v", label, att, kids)
}

func assertReportAttempt(t *testing.T, reps []goalrun.ChildReport, att, label string) {
	t.Helper()
	for _, c := range reps {
		if c.AttemptID == att {
			return
		}
	}
	t.Fatalf("%s: missing report attempt %s", label, att)
}
