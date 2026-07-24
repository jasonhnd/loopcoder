package goalrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestPartialOnlyAbortedG1ResumesAsExactG2: partial aborted attempt g1 must
// resume launch as exact canonical g2 (never relaunch g1, never hardcode g1).
func TestPartialOnlyAbortedG1ResumesAsExactG2(t *testing.T) {
	now := time.Date(2026, 7, 24, 16, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-partial-g1"
	runID := "run_partial_g1"
	goal := "implement partial aborted g1 to g2"

	// Single-item graph so aborted item is the only child.
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

	// Materialize digests via a dry first call? Use childContractForGoal-style for single item
	// by running once with fail, or compute via Execute dry... Simpler: first successful path
	// interrupted mid-flight would leave partial. Seed partial with known digests from a
	// planning-only approach: Execute once with Hang then cancel is heavy. Instead seed
	// partial using digests from a first blocked run without resume.
	// Compute digests by running Execute once without partial to get plan from result,
	// then write partial and resume. Actually use goalrun.Execute with FailIDs first
	// to get digests, then write partial with aborted g1 for a *fresh* runID.

	// Use childContract pattern: goalrun Decompose is injected, so digests from
	// workflowdef materialize of same graph. Mirror childContractForGoal with one item.
	planDig, graphDig, class, ccd, graphID := partialOneChildDigests(t, oneChild, projectID, "wi_only", now, goal)

	// Aborted g1 canonical under this plan/run.
	abortedG1 := workflowrun.AttemptID("wi_only", planDig, runID, 1)
	wantG2 := workflowrun.AttemptID("wi_only", planDig, runID, 2)

	// No checkpoint — partial only.
	dir := filepath.Join(home, "projects", projectID, "runs", runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	part := map[string]any{
		"schema": workflowrun.PartialSchema, "project_id": projectID, "run_id": runID,
		"plan_digest": planDig, "execution_plan_digest": planDig, "graph_digest": graphDig,
		"saved_at": now.Format(time.RFC3339), "interrupted": true,
		"prior_succeeded": map[string]any{},
		"aborted_attempts": map[string]string{
			"wi_only": abortedG1,
		},
	}
	raw, _ := json.MarshalIndent(part, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "workflow-partial.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Pre-seed only a marker run.start. Partial aborted_attempts alone drives g2
	// selection — do not leave an open launch without authority (recover fail-closed).
	elog, err := workflowrun.OpenEventLog(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	must := func(e workflowrun.Event) {
		e.ProjectID, e.RunID = projectID, runID
		if _, err := elog.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	must(workflowrun.Event{Kind: "run.start", Message: "prior process"})

	calls := map[string]int{}
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
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
	if err != nil {
		t.Fatalf("resume: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if calls["wi_only"] != 1 {
		t.Fatalf("calls=%+v want 1", calls)
	}
	var got string
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == "wi_only" {
			got = c.AttemptID
		}
	}
	if got != wantG2 {
		t.Fatalf("attempt=%q want exact g2 %q (aborted was %q); graphID=%s class=%s ccd=%s",
			got, wantG2, abortedG1, graphID, class, ccd)
	}
	// Must never re-launch g1.
	events, err := elog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	g1Launch := 0
	g2Launch := 0
	for _, e := range events {
		if e.Kind != "launch" || e.WorkItemID != "wi_only" {
			continue
		}
		if e.AttemptID == abortedG1 {
			g1Launch++
		}
		if e.AttemptID == wantG2 {
			g2Launch++
		}
	}
	// No seed launch for aborted g1 (would be no_authority corruption); only g2 launches.
	if g1Launch != 0 {
		t.Fatalf("g1 launch count=%d want 0 (aborted via partial only, never re-launched)", g1Launch)
	}
	if g2Launch != 1 {
		t.Fatalf("g2 launch count=%d want 1", g2Launch)
	}
}

// TestPartialMalformedAbortedFailsBeforeSideEffects: cross-plan or malformed
// aborted ID fails closed before eventlog spend / claim / launch on resume.
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
	planDig, graphDig, _, _, _ := partialOneChildDigests(t, oneChild, projectID, "wi_only", now, goal)

	cases := []struct {
		name string
		att  string
		sub  string
	}{
		{"malformed", "att-wi_only-notageneration", "malformed"},
		{"cross_plan", "att-wi_only-deadbeefdead-g1", "canonical"}, // wrong short hash for this plan
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run_bad_" + tc.name
			dir := filepath.Join(home, "projects", projectID, "runs", runID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			// Snapshot: no events yet for this run.
			eventPath := filepath.Join(dir, "workflow-events.jsonl")
			if _, err := os.Stat(eventPath); err == nil {
				t.Fatal("precondition: no event log")
			}
			part := map[string]any{
				"schema": workflowrun.PartialSchema, "project_id": projectID, "run_id": runID,
				"plan_digest": planDig, "execution_plan_digest": planDig, "graph_digest": graphDig,
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
			if !stringsContainsCI(err.Error(), tc.sub) && !stringsContainsCI(err.Error(), "aborted") {
				t.Fatalf("err %q should mention %q or aborted", err.Error(), tc.sub)
			}
			if calls["wi_only"] != 0 {
				t.Fatalf("must not launch: %+v", calls)
			}
			// Envelope/abort fail must happen before eventlog creation for this run.
			// (partial write created run dir; event log must still be absent)
			if _, serr := os.Stat(eventPath); serr == nil {
				t.Fatal("workflow-events.jsonl must not be created on abort validation fail")
			}
			if _, serr := os.Stat(filepath.Join(dir, "workclaims.json")); serr == nil {
				t.Fatal("workclaims must not be created on abort validation fail")
			}
		})
	}
}

func partialOneChildDigests(
	t *testing.T,
	decompose func(workgraph.DecomposeOptions) (workgraph.Graph, error),
	projectID, workItemID string,
	now time.Time,
	goal string,
) (planDig, graphDig, class, ccd, graphID string) {
	t.Helper()
	// Run a non-resume Execute to materialize digests, then discard (separate run).
	// Cheaper: directly materialize via goalrun's path by executing dry-ish with FailIDs.
	env := newProductEnv(t, now, "codex")
	home := env.Home
	runID := "run_digest_probe_" + workItemID
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID + "-probe", RunID: runID,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     decompose,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now },
		},
	})
	if err != nil && res.PlanDigest == "" {
		t.Fatalf("probe execute: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if res.PlanDigest == "" || res.GraphDigest == "" {
		t.Fatalf("empty digests: %+v", res)
	}
	// Class/CCD from succeeded or any child with contract.
	for _, c := range res.Children {
		if c.ChildID == workItemID && c.ChildContractDigest != "" {
			return res.PlanDigest, res.GraphDigest, c.TaskClass, c.ChildContractDigest, res.GraphID
		}
	}
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == workItemID && c.ChildContractDigest != "" {
			return res.PlanDigest, res.GraphDigest, c.TaskClass, c.ChildContractDigest, res.GraphID
		}
	}
	t.Fatalf("no child contract on probe: children=%+v wf=%+v", res.Children, res.Workflow.Children)
	return "", "", "", "", ""
}
