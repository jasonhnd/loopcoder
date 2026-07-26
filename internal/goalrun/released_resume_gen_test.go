package goalrun_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestReleasedUnlaunchedG0_ResumeBumpsCanonicalGeneration: middle-child interrupt
// releases never-launched later siblings under g0; fresh Resume must leave those
// g0 entries immutable (Actual nil) and reserve unique g1 attempts for each.
func TestReleasedUnlaunchedG0_ResumeBumpsCanonicalGeneration(t *testing.T) {
	now := time.Date(2026, 7, 25, 8, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-released-resume"
	runID := "run_released_resume_1"
	repo := initDisposableGitRepo(t)
	goal := "implement middle interrupt released reservation recovery with tests and verify"
	linear5 := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_rel_res", Version: 1,
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
				{Schema: workgraph.SchemaItem, ID: "wi_tests", Status: workgraph.ItemRequired,
					Intent: "tests", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 3, OutputContract: "test_pass",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write"},
				{Schema: workgraph.SchemaItem, ID: "wi_verify", Status: workgraph.ItemRequired,
					Intent: "verify", Owner: "verifier", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 4, OutputContract: "verification_verdict",
					RouteRequirement: "class=soul,depth=high,permission=read-only"},
				{Schema: workgraph.SchemaItem, ID: "wi_docs", Status: workgraph.ItemRequired,
					Intent: "operator guide", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 5, OutputContract: "docs_diff",
					RouteRequirement: "class=luna,depth=low,permission=bounded_write"},
			},
			Dependencies: []workgraph.Dependency{
				{Schema: workgraph.SchemaDep, From: "wi_research", To: "wi_implement", Kind: workgraph.DepFinishToStart},
				{Schema: workgraph.SchemaDep, From: "wi_implement", To: "wi_tests", Kind: workgraph.DepFinishToStart},
				{Schema: workgraph.SchemaDep, From: "wi_tests", To: "wi_verify", Kind: workgraph.DepFinishToStart},
				{Schema: workgraph.SchemaDep, From: "wi_verify", To: "wi_docs", Kind: workgraph.DepFinishToStart},
			},
			Limits:    workgraph.Limits{Schema: workgraph.SchemaLimits, MaxItems: 32, MaxDepth: 8, MaxParallel: 1, MaxAutomaticReplan: 1},
			CreatedAt: now,
		}
		g.PlanDigest = workgraph.DigestGraph(g)
		return g, nil
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	res1, err1 := goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID, Goal: goal, Issue: "1397",
		Actor: "owner", Owner: "worker", Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, RepoPath: repo, Now: func() time.Time { return now },
		Decompose: linear5, LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			HangIDs: map[string]bool{"wi_tests": true},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == "wi_tests" && pid > 0 {
					cancel1()
				}
			},
		},
	})
	if err1 == nil && res1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("want interrupt: %+v", res1)
	}

	// Snapshot g0 ledger rows for later children before resume.
	led1, err := capacityledger.OpenPath(env.LedgerPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// Need plan digest from checkpoint/result.
	cp, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	planDig := cp.PlanDigest
	if planDig == "" {
		t.Fatal("checkpoint missing plan digest")
	}
	g0Snap := map[string]capacityledger.Entry{}
	for _, id := range []string{"wi_tests", "wi_verify", "wi_docs"} {
		att := workflowrun.AttemptID(id, planDig, runID, 0)
		e, ok := led1.Get(projectID, runID, att)
		if !ok || e.State != "released" {
			t.Fatalf("%s g0 want released after interrupt, ok=%v state=%q att=%s", id, ok, e.State, att)
		}
		if e.Actual != nil {
			t.Fatalf("%s g0 Actual must be nil (unknown), got %v", id, *e.Actual)
		}
		g0Snap[id] = e
	}
	// Prior successes present.
	if _, ok := cp.PriorSucceeded["wi_research"]; !ok {
		t.Fatalf("research prior missing: %+v", cp.PriorSucceeded)
	}
	if _, ok := cp.PriorSucceeded["wi_implement"]; !ok {
		t.Fatalf("implement prior missing: %+v", cp.PriorSucceeded)
	}

	// No launch of later children in pass1.
	elog, err := workflowrun.OpenEventLog(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := elog.ReadAll()
	for _, e := range evs {
		if e.Kind == "launch" && (e.WorkItemID == "wi_verify" || e.WorkItemID == "wi_docs") {
			t.Fatalf("pass1 launched later child: %+v", e)
		}
	}

	calls2 := map[string]int{}
	res2, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, RepoPath: repo, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose: linear5, LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2,
		},
	})
	if err2 != nil {
		t.Fatalf("resume: %v status=%s msg=%s", err2, res2.Status, res2.Message)
	}
	if calls2["wi_research"] != 0 || calls2["wi_implement"] != 0 {
		t.Fatalf("prior success re-exec: %+v", calls2)
	}
	if calls2["wi_tests"] != 1 || calls2["wi_verify"] != 1 || calls2["wi_docs"] != 1 {
		t.Fatalf("want tests/verify/docs once each: %+v", calls2)
	}

	// g0 immutable: same state released, Actual still nil, no rewrite to succeeded.
	led2, err := capacityledger.OpenPath(env.LedgerPath, func() time.Time { return now.Add(2 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	resIDs := map[string]bool{}
	for _, id := range []string{"wi_tests", "wi_verify", "wi_docs"} {
		g0 := workflowrun.AttemptID(id, planDig, runID, 0)
		g1 := workflowrun.AttemptID(id, planDig, runID, 1)
		e0, ok0 := led2.Get(projectID, runID, g0)
		if !ok0 || e0.State != "released" {
			t.Fatalf("%s g0 mutated: ok=%v state=%q", id, ok0, e0.State)
		}
		if e0.Actual != nil {
			t.Fatalf("%s g0 Actual invented after resume: %v", id, *e0.Actual)
		}
		// ReservationID of g0 must match pre-resume snapshot (immutable).
		if e0.ReservationID != g0Snap[id].ReservationID {
			t.Fatalf("%s g0 reservation_id changed: before=%s after=%s", id, g0Snap[id].ReservationID, e0.ReservationID)
		}
		e1, ok1 := led2.Get(projectID, runID, g1)
		if !ok1 {
			t.Fatalf("%s missing g1 reservation", id)
		}
		if e1.ReservationID == "" || e1.ReservationID == e0.ReservationID {
			t.Fatalf("%s g1 reservation not unique: g0=%s g1=%s", id, e0.ReservationID, e1.ReservationID)
		}
		if resIDs[e1.ReservationID] {
			t.Fatalf("duplicate g1 reservation_id %s", e1.ReservationID)
		}
		resIDs[e1.ReservationID] = true
		// Prior success g0 attempts must be unchanged and not re-reserved at g1.
	}
	for _, id := range []string{"wi_research", "wi_implement"} {
		prior := cp.PriorSucceeded[id]
		e, ok := led2.Get(projectID, runID, prior.AttemptID)
		if !ok {
			t.Fatalf("prior %s ledger entry missing", id)
		}
		// Still reconciled or released (success path); not wiped.
		if e.State != "reconciled" && e.State != "released" {
			t.Fatalf("prior %s state=%q", id, e.State)
		}
		g1 := workflowrun.AttemptID(id, planDig, runID, 1)
		if _, ok := led2.Get(projectID, runID, g1); ok {
			t.Fatalf("prior success %s must not get g1 reserve", id)
		}
	}

	// Event log: each later child launches once under g1.
	evs2, _ := elog.ReadAll()
	for _, id := range []string{"wi_verify", "wi_docs"} {
		n := 0
		for _, e := range evs2 {
			if e.Kind == "launch" && e.WorkItemID == id {
				n++
				if strings.HasSuffix(e.AttemptID, "-g0") {
					t.Fatalf("%s launched g0 after resume: %s", id, e.AttemptID)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%s launch count=%d want 1", id, n)
		}
	}
}
