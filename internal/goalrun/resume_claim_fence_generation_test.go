package goalrun_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestResumePreviouslyUnlaunchedSiblingWithLaggingClaimFence covers the real
// RC54 recovery shape: a middle-child interrupt bumps the run-wide attempt
// suffix, so a previously unlaunched later sibling first claims attempt -g1
// with claim-fence generation 1 while its event/workflow generation is 2. A
// further resume must validate both generation domains and reuse every success
// without launching another provider.
func TestResumePreviouslyUnlaunchedSiblingWithLaggingClaimFence(t *testing.T) {
	now := time.Date(2026, 7, 26, 1, 30, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	const (
		projectID = "proj-lagged-claim-fence"
		runID     = "run_lagged_claim_fence"
		goal      = "implement three child restart with later sibling reuse"
	)
	threeChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_lagged_claim_fence", Version: 1,
			Source: workgraph.SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: "owner",
			Items: []workgraph.WorkItem{
				{
					Schema: workgraph.SchemaItem, ID: "wi_research", Status: workgraph.ItemRequired,
					Intent: "research", Owner: "research", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 1, OutputContract: "findings",
					RouteRequirement: "class=luna,depth=low,permission=read-only",
				},
				{
					Schema: workgraph.SchemaItem, ID: "wi_implement", Status: workgraph.ItemRequired,
					Intent: "implement", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 2, OutputContract: "diff",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
				},
				{
					Schema: workgraph.SchemaItem, ID: "wi_tests", Status: workgraph.ItemRequired,
					Intent: "tests", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 3, OutputContract: "tests",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
				},
			},
			Dependencies: []workgraph.Dependency{
				{
					Schema: workgraph.SchemaDep, From: "wi_research", To: "wi_implement",
					Kind: workgraph.DepFinishToStart,
				},
				{
					Schema: workgraph.SchemaDep, From: "wi_implement", To: "wi_tests",
					Kind: workgraph.DepFinishToStart,
				},
			},
			Limits: workgraph.DefaultLimits(), CreatedAt: now,
		}
		g.PlanDigest = workgraph.DigestGraph(g)
		return g, nil
	}

	// Pass 1: research succeeds, implement is interrupted, tests never launch.
	ctx1, cancel1 := context.WithCancel(context.Background())
	calls1 := map[string]int{}
	res1, err1 := goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1437", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose: threeChild, LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls1,
			HangIDs: map[string]bool{"wi_implement": true},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == "wi_implement" {
					cancel1()
				}
			},
		},
	})
	if err1 == nil && res1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("pass1 must stop on the forced interrupt: status=%s", res1.Status)
	}
	if calls1["wi_research"] != 1 || calls1["wi_implement"] != 1 || calls1["wi_tests"] != 0 {
		t.Fatalf("pass1 calls=%+v", calls1)
	}

	// Pass 2: research reuses, implement retries at -g1, and the previously
	// unlaunched tests child first claims at -g1 with claim fence generation 1.
	calls2 := map[string]int{}
	res2, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1437", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose: threeChild, LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) }, Calls: calls2,
		},
	})
	if err2 != nil || res2.Status != workflowrun.StatusHumanGate {
		t.Fatalf("pass2: err=%v status=%s message=%s", err2, res2.Status, res2.Message)
	}
	if calls2["wi_research"] != 0 || calls2["wi_implement"] != 1 || calls2["wi_tests"] != 1 {
		t.Fatalf("pass2 calls=%+v", calls2)
	}
	testsAttempt := ""
	for _, child := range res2.Workflow.Children {
		if child.WorkItemID != "wi_tests" || child.Terminal != "succeeded" {
			continue
		}
		testsAttempt = child.AttemptID
		if child.Generation != 2 || !strings.HasSuffix(child.AttemptID, "-g1") {
			t.Fatalf("tests outcome must use event generation 2 at -g1: %+v", child)
		}
	}
	if testsAttempt == "" {
		t.Fatal("pass2 missing succeeded tests child")
	}

	runDir := filepath.Join(home, "projects", projectID, "runs", runID)
	claims, err := workclaim.OpenPath(filepath.Join(runDir, "workclaims.json"), func() time.Time {
		return now.Add(2 * time.Minute)
	})
	if err != nil {
		t.Fatal(err)
	}
	testsClaim, err := claims.GetByAttempt(projectID, "g_lagged_claim_fence", 1, "wi_tests", testsAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if testsClaim.Generation != 1 || testsClaim.State != workclaim.StateClosed {
		t.Fatalf("tests claim fence must independently be closed generation 1: %+v", testsClaim)
	}
	authorityStore, err := workflowrun.OpenAuthorityStore(
		context.Background(), home, projectID, runID, func() time.Time { return now.Add(2 * time.Minute) },
	)
	if err != nil {
		t.Fatal(err)
	}
	defer authorityStore.Close()
	testsAuthority, err := storage.LoadProviderExecutionAuthority(
		context.Background(), authorityStore, projectID, runID, testsAttempt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if testsAuthority.ClaimGeneration != testsClaim.Generation {
		t.Fatalf("authority fence=%d claim fence=%d", testsAuthority.ClaimGeneration, testsClaim.Generation)
	}

	eventLog, err := workflowrun.OpenEventLog(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := eventLog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	launchesBefore := 0
	interruptsBefore := 0
	testsBoundEvents := 0
	for _, event := range beforeEvents {
		if event.Kind == "launch" {
			launchesBefore++
		}
		if event.Kind == "interrupt" {
			interruptsBefore++
		}
		if event.AttemptID == testsAttempt &&
			(event.Kind == "claim" || event.Kind == "launch" || event.Kind == "terminal") {
			testsBoundEvents++
			if event.Generation != 2 {
				t.Fatalf("tests %s event generation=%d want 2", event.Kind, event.Generation)
			}
		}
	}
	if testsBoundEvents != 3 || interruptsBefore != 1 {
		t.Fatalf("tests bound events=%d interrupts=%d", testsBoundEvents, interruptsBefore)
	}

	// Pass 3 reproduces the RC54 boundary. Recovery must accept the two exact
	// generation domains and perform only durable terminal reuse.
	calls3 := map[string]int{}
	res3, err3 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1437", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(2 * time.Minute) },
		Decompose: threeChild, LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(2 * time.Minute) }, Calls: calls3,
		},
	})
	if err3 != nil || res3.Status != workflowrun.StatusHumanGate {
		t.Fatalf("pass3: err=%v status=%s message=%s", err3, res3.Status, res3.Message)
	}
	if calls3["wi_research"] != 0 || calls3["wi_implement"] != 0 || calls3["wi_tests"] != 0 {
		t.Fatalf("pass3 re-executed a provider: %+v", calls3)
	}
	if res3.Workflow.ReuseCount != 3 {
		t.Fatalf("pass3 reuse_count=%d want 3", res3.Workflow.ReuseCount)
	}
	afterEvents, err := eventLog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	launchesAfter := 0
	interruptsAfter := 0
	for _, event := range afterEvents {
		if event.Kind == "launch" {
			launchesAfter++
		}
		if event.Kind == "interrupt" {
			interruptsAfter++
		}
	}
	if launchesAfter != launchesBefore || interruptsAfter != 1 {
		t.Fatalf("pass3 launches %d→%d interrupts=%d", launchesBefore, launchesAfter, interruptsAfter)
	}
}
