package goalrun_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// twoChildGraph: research (legitimate) + implement — proves sibling reservation
// cannot occur when parent preflight rejects a ghost/mismatched seed.
func twoChildGraph(now time.Time) func(workgraph.DecomposeOptions) (workgraph.Graph, error) {
	return func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_parent_pf", Version: 1,
			Source: workgraph.SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: "owner",
			Items: []workgraph.WorkItem{
				{Schema: workgraph.SchemaItem, ID: "wi_research", Status: workgraph.ItemRequired,
					Intent: "research", Owner: "research", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 1, OutputContract: "findings",
					RouteRequirement: "class=luna,depth=low,permission=read-only"},
				{Schema: workgraph.SchemaItem, ID: "wi_implement", Status: workgraph.ItemRequired,
					Intent: "implement", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 2, OutputContract: "diff",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write"},
			},
			Dependencies: []workgraph.Dependency{{
				Schema: workgraph.SchemaDep, From: "wi_research", To: "wi_implement", Kind: workgraph.DepFinishToStart,
			}},
			Limits: workgraph.DefaultLimits(), CreatedAt: now,
		}
		g.PlanDigest = workgraph.DigestGraph(g)
		return g, nil
	}
}

func parentPFDigests(t *testing.T, now time.Time, projectID, goal string) (planDig, graphDig, class, ccd, graphID string) {
	t.Helper()
	// Seed digests for wi_research via a probe execute (separate project).
	env := newProductEnv(t, now, "codex")
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID + "-probe", RunID: "run_probe_pf",
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: env.Home, Now: func() time.Time { return now },
		Decompose:     twoChildGraph(now),
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: env.Home, Now: func() time.Time { return now },
			FailIDs: map[string]bool{"wi_implement": true},
		},
	})
	if res.PlanDigest == "" {
		t.Fatalf("probe: err=%v status=%s msg=%s", err, res.Status, res.Message)
	}
	for _, c := range res.Children {
		if c.ChildID == "wi_research" && c.ChildContractDigest != "" {
			return res.PlanDigest, res.GraphDigest, c.TaskClass, c.ChildContractDigest, res.GraphID
		}
	}
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == "wi_research" && c.ChildContractDigest != "" {
			return res.PlanDigest, res.GraphDigest, c.TaskClass, c.ChildContractDigest, res.GraphID
		}
	}
	t.Fatalf("probe missing research CCD: %+v", res.Children)
	return "", "", "", "", ""
}

// assertParentPreflightZeroSideEffects asserts no inventory/ledger/event/claim/executor spend.
func assertParentPreflightZeroSideEffects(
	t *testing.T,
	home, projectID, runID, ledgerPath string,
	invCalls, ledCalls *int,
	execCalls map[string]int,
) {
	t.Helper()
	if invCalls != nil && *invCalls != 0 {
		t.Fatalf("LoadInventory calls=%d want 0", *invCalls)
	}
	if ledCalls != nil && *ledCalls != 0 {
		t.Fatalf("OpenLedger calls=%d want 0", *ledCalls)
	}
	for id, n := range execCalls {
		if n != 0 {
			t.Fatalf("executor calls[%s]=%d want 0", id, n)
		}
	}
	runDir := filepath.Join(home, "projects", projectID, "runs", runID)
	if _, err := os.Stat(filepath.Join(runDir, "workflow-events.jsonl")); err == nil {
		t.Fatal("workflow-events.jsonl must not exist after parent preflight fail")
	}
	if _, err := os.Stat(filepath.Join(runDir, "workclaims.json")); err == nil {
		t.Fatal("workclaims must not exist after parent preflight fail")
	}
	// Ledger path must not exist — OpenLedger was never called (strict IsNotExist).
	if _, err := os.Stat(ledgerPath); err == nil {
		t.Fatalf("ledger file must not exist after parent preflight fail (OpenLedger must be 0): %s", ledgerPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("ledger stat: %v", err)
	}
}

func countingInv(env productEnv, invCalls *int) func(ctx context.Context, repo string, now time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
	base := env.loadInv()
	return func(ctx context.Context, repo string, now time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
		*invCalls++
		return base(ctx, repo, now)
	}
}

func countingLed(path string, ledCalls *int) func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
	return func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
		*ledCalls++
		return capacityledger.OpenPath(path, nowFn)
	}
}

// TestParentPreflightGhostPrior_ZeroSideEffects: valid envelope + ghost PriorSucceeded
// key fails before inventory/ledger/eventlog; sibling research cannot reserve.
func TestParentPreflightGhostPrior_ZeroSideEffects(t *testing.T) {
	now := time.Date(2026, 7, 24, 18, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-parent-ghost"
	runID := "run_parent_ghost"
	goal := "implement parent preflight ghost prior boundary"
	planDig, graphDig, class, ccd, graphID := parentPFDigests(t, now, projectID, goal)

	// Valid research prior + ghost key not in graph.
	researchAtt := workflowrun.AttemptID("wi_research", planDig, runID, 0)
	ghostAtt := workflowrun.AttemptID("wi_ghost", planDig, runID, 0)
	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked",
		PriorSucceeded: map[string]workflowrun.ChildOutcome{
			"wi_research": {
				WorkItemID: "wi_research", Terminal: "succeeded", AttemptID: researchAtt,
				OutputEvidence: "sha256:ev", Provider: "codex", Model: "gpt-5.5",
				Depth: "low", Permission: "read-only", TaskClass: class,
				ExecutionPlanDigest: planDig, ChildContractDigest: ccd, Generation: 1,
			},
			"wi_ghost": {
				WorkItemID: "wi_ghost", Terminal: "succeeded", AttemptID: ghostAtt,
				OutputEvidence: "sha256:ghost", Provider: "codex", Model: "gpt-5.5",
				Depth: "medium", Permission: "bounded_write", TaskClass: "tera",
				ExecutionPlanDigest: planDig, ChildContractDigest: ccd, Generation: 1,
			},
		},
		SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}

	ledgerPath := filepath.Join(t.TempDir(), "cap-ghost.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     twoChildGraph(now),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected parent preflight fail on ghost prior")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "ghost") {
		t.Fatalf("err should mention ghost: %v", err)
	}
	assertParentPreflightZeroSideEffects(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
}

// TestParentPreflightKeyValueMismatch_ZeroSideEffects: map key != WorkItemID.
func TestParentPreflightKeyValueMismatch_ZeroSideEffects(t *testing.T) {
	now := time.Date(2026, 7, 24, 18, 30, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-parent-kv"
	runID := "run_parent_kv"
	goal := "implement parent preflight key value mismatch"
	planDig, graphDig, class, ccd, graphID := parentPFDigests(t, now, projectID, goal)
	att := workflowrun.AttemptID("wi_research", planDig, runID, 0)
	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked",
		PriorSucceeded: map[string]workflowrun.ChildOutcome{
			"wi_research": {
				WorkItemID: "wi_other", // mismatch
				Terminal:   "succeeded", AttemptID: att,
				OutputEvidence: "sha256:ev", Provider: "codex", Model: "gpt-5.5",
				Depth: "low", Permission: "read-only", TaskClass: class,
				ExecutionPlanDigest: planDig, ChildContractDigest: ccd, Generation: 1,
			},
		},
		SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "cap-kv.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     twoChildGraph(now),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected key/value mismatch fail")
	}
	if !strings.Contains(err.Error(), "WorkItemID") && !strings.Contains(err.Error(), "work_item") {
		t.Fatalf("err should mention WorkItemID mismatch: %v", err)
	}
	assertParentPreflightZeroSideEffects(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
}

// TestParentPreflightGhostAttemptGeneration_ZeroSideEffects: aborted/gen key not in graph.
func TestParentPreflightGhostAttemptGeneration_ZeroSideEffects(t *testing.T) {
	now := time.Date(2026, 7, 24, 19, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-parent-ghostgen"
	runID := "run_parent_ghostgen"
	goal := "implement parent preflight ghost attempt generation"
	planDig, graphDig, class, ccd, graphID := parentPFDigests(t, now, projectID, goal)
	researchAtt := workflowrun.AttemptID("wi_research", planDig, runID, 0)
	// Ghost aborted id that is canonical for a non-graph work item.
	ghostAbort := workflowrun.AttemptID("wi_not_in_graph", planDig, runID, 0)
	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked",
		PriorSucceeded: map[string]workflowrun.ChildOutcome{
			"wi_research": {
				WorkItemID: "wi_research", Terminal: "succeeded", AttemptID: researchAtt,
				OutputEvidence: "sha256:ev", Provider: "codex", Model: "gpt-5.5",
				Depth: "low", Permission: "read-only", TaskClass: class,
				ExecutionPlanDigest: planDig, ChildContractDigest: ccd, Generation: 1,
			},
		},
		// Ghost AttemptGeneration only — unbound AbortedAttempts without
		// PriorOutcome would fail earlier; this fixture targets gen ghost keys.
		AttemptGeneration: map[string]int{"wi_not_in_graph": 0},
		SavedAt:           now,
	}
	_ = ghostAbort
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(t.TempDir(), "cap-ghostgen.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     twoChildGraph(now),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected ghost AttemptGeneration/aborted fail")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "ghost") &&
		!strings.Contains(err.Error(), "not in current graph") {
		t.Fatalf("err should mention ghost/not in graph: %v", err)
	}
	assertParentPreflightZeroSideEffects(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
}
