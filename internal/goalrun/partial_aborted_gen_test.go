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

// TestPartialOnlyAbortedG1ResumesAsExactG2: production interrupt → partial-only
// resume launches next generation with aborted row retained.
func TestPartialOnlyAbortedG1ResumesAsExactG2(t *testing.T) {
	now := time.Date(2026, 7, 24, 16, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-partial-g1"
	runID := "run_partial_g1"
	goal := "implement partial aborted g1 to g2"
	oneChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_partial_g1", Version: 1,
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
	_ = os.Remove(filepath.Join(home, "projects", projectID, "runs", runID, "goal-checkpoint.json"))
	part, err := workflowrun.LoadPartialPrior(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(part.WorkflowKids) == 0 || len(part.Aborted) == 0 {
		t.Fatalf("partial must have kids+aborted: kids=%d aborted=%+v", len(part.WorkflowKids), part.Aborted)
	}
	abortedAtt := part.Aborted["wi_only"]
	if abortedAtt == "" {
		t.Fatalf("aborted empty: %+v", part.Aborted)
	}
	abortedG := workflowrun.ParseAttemptGeneration(abortedAtt)
	wantNext := workflowrun.AttemptID("wi_only", part.PlanDigest, runID, abortedG+1)

	calls2 := map[string]int{}
	res2, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     oneChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2,
		},
	})
	if err2 != nil {
		t.Fatalf("resume: %v status=%s msg=%s", err2, res2.Status, res2.Message)
	}
	if calls2["wi_only"] != 1 {
		t.Fatalf("calls=%+v want 1", calls2)
	}
	var got string
	var maxGen int
	var sawAborted bool
	for _, c := range res2.Workflow.Children {
		if c.WorkItemID != "wi_only" {
			continue
		}
		if c.AttemptID == abortedAtt {
			sawAborted = true
			if c.Terminal == "succeeded" {
				t.Fatalf("aborted must not succeed: %+v", c)
			}
		}
		if c.Generation > maxGen {
			maxGen = c.Generation
			got = c.AttemptID
		}
	}
	if !sawAborted {
		t.Fatal("missing aborted PriorOutcome row after resume")
	}
	if got != wantNext {
		t.Fatalf("max-gen attempt=%q want %q (aborted %q)", got, wantNext, abortedAtt)
	}
}

// TestPartialMalformedAbortedFailsBeforeSideEffects: unbound AbortedAttempts
// without WorkflowKids fails closed before spend.
func TestPartialMalformedAbortedFailsBeforeSideEffects(t *testing.T) {
	now := time.Date(2026, 7, 24, 17, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-partial-bad"
	goal := "implement partial malformed aborted fail closed"
	oneChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_partial_bad", Version: 1,
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
	planDig, graphDig, _, _, graphID := probeOneImplDigests(t, now, projectID, goal, "g_partial_bad")

	for _, tc := range []struct {
		name string
		att  string
	}{
		{"malformed", "att-wi_only-notageneration"},
		{"cross_plan", "att-wi_only-deadbeefdead-g1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run_bad_" + tc.name
			dir := filepath.Join(home, "projects", projectID, "runs", runID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			eventPath := filepath.Join(dir, "workflow-events.jsonl")
			part := map[string]any{
				"schema": workflowrun.PartialSchema, "project_id": projectID, "run_id": runID,
				"plan_digest": planDig, "execution_plan_digest": planDig, "graph_digest": graphDig,
				"graph_id": graphID, "graph_version": 1,
				"aborted_attempts": map[string]string{"wi_only": tc.att},
			}
			raw, _ := json.MarshalIndent(part, "", "  ")
			if err := os.WriteFile(filepath.Join(dir, "workflow-partial.json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			calls := map[string]int{}
			_, err := goalrun.Execute(context.Background(), goalrun.Request{
				ProjectID: projectID, RunID: runID, Resume: true,
				Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
				Provider: "codex", Model: "gpt-5.5",
				HomeDir: home, Now: func() time.Time { return now },
				Decompose:     oneChild,
				LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
				Executor: workflowrun.FakeChildExecutor{
					HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
				},
			})
			if err == nil {
				t.Fatal("expected fail closed")
			}
			msg := err.Error()
			if !strings.Contains(msg, "AbortedAttempts") && !strings.Contains(msg, "WorkflowKids") &&
				!strings.Contains(msg, "aborted") && !strings.Contains(msg, "malformed") &&
				!strings.Contains(msg, "canonical") && !strings.Contains(msg, "forged") {
				t.Fatalf("want abort validation fail-closed, got %v", err)
			}
			if len(calls) != 0 {
				t.Fatalf("zero executor Calls: %+v", calls)
			}
			if _, serr := os.Stat(eventPath); serr == nil {
				t.Fatal("event log must not be created on abort validation fail")
			}
		})
	}
}
