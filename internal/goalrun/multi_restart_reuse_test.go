package goalrun_test

import (
	"context"
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

// TestMultiRestart_ASucceedsBInterruptedTwice: A succeeds once and is reused on
// every resume (executor calls(A)==1); B forced-interrupts twice then succeeds
// (g0/g1/g2 each once). Full WorkflowKids survives checkpoint and partial-only paths.
func TestMultiRestart_ASucceedsBInterruptedTwice(t *testing.T) {
	now := time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-multi-reuse"
	runID := "run_multi_reuse_1"
	goal := "implement multi restart A succeed B interrupt twice"
	twoChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_multi_reuse", Version: 1,
			Source: workgraph.SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: "owner",
			Items: []workgraph.WorkItem{
				{Schema: workgraph.SchemaItem, ID: "wi_a", Status: workgraph.ItemRequired,
					Intent: "research", Owner: "research", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 1, OutputContract: "findings",
					RouteRequirement: "class=luna,depth=low,permission=read-only"},
				{Schema: workgraph.SchemaItem, ID: "wi_b", Status: workgraph.ItemRequired,
					Intent: "implement", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 2, OutputContract: "diff",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write"},
			},
			Dependencies: []workgraph.Dependency{{
				Schema: workgraph.SchemaDep, From: "wi_a", To: "wi_b", Kind: workgraph.DepFinishToStart,
			}},
			Limits: workgraph.DefaultLimits(), CreatedAt: now,
		}
		g.PlanDigest = workgraph.DigestGraph(g)
		return g, nil
	}

	// Pass 1: A succeeds, B hangs → interrupt.
	ctx1, cancel1 := context.WithCancel(context.Background())
	calls1 := map[string]int{}
	res1, err1 := goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     twoChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls: calls1, HangIDs: map[string]bool{"wi_b": true},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == "wi_b" {
					cancel1()
				}
			},
		},
	})
	if err1 == nil && res1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("pass1 expected interrupt: status=%s", res1.Status)
	}
	if calls1["wi_a"] != 1 || calls1["wi_b"] != 1 {
		t.Fatalf("pass1 calls=%+v", calls1)
	}
	attA := ""
	attBG0 := ""
	for _, c := range res1.Workflow.Children {
		if c.WorkItemID == "wi_a" && c.Terminal == "succeeded" {
			attA = c.AttemptID
		}
		if c.WorkItemID == "wi_b" {
			attBG0 = c.AttemptID
		}
	}
	if attA == "" || attBG0 == "" {
		t.Fatalf("pass1 missing A/B attempts: A=%q B=%q children=%+v", attA, attBG0, res1.Workflow.Children)
	}

	// Pass 2: resume; A reused; B interrupt again.
	ctx2, cancel2 := context.WithCancel(context.Background())
	calls2 := map[string]int{}
	res2, err2 := goalrun.Execute(ctx2, goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     twoChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2, HangIDs: map[string]bool{"wi_b": true},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == "wi_b" {
					cancel2()
				}
			},
		},
	})
	if err2 == nil && res2.Status == workflowrun.StatusHumanGate {
		t.Fatalf("pass2 expected interrupt: %v status=%s", err2, res2.Status)
	}
	if calls2["wi_a"] != 0 {
		t.Fatalf("pass2 must not re-exec A: calls2=%+v", calls2)
	}
	if calls2["wi_b"] != 1 {
		t.Fatalf("pass2 B calls=%d want 1: %+v", calls2["wi_b"], calls2)
	}
	attBG1 := ""
	for _, c := range res2.Workflow.Children {
		if c.WorkItemID == "wi_a" && c.Terminal == "succeeded" && c.AttemptID != attA {
			t.Fatalf("A attempt drift: want %s got %s", attA, c.AttemptID)
		}
		if c.WorkItemID == "wi_b" && c.AttemptID != attBG0 && strings.HasSuffix(c.AttemptID, "-g1") {
			attBG1 = c.AttemptID
		}
	}
	if attBG1 == "" {
		// pick max gen for B
		var maxG int
		for _, c := range res2.Workflow.Children {
			if c.WorkItemID == "wi_b" && c.Generation > maxG {
				maxG = c.Generation
				attBG1 = c.AttemptID
			}
		}
	}
	if attBG1 == "" || attBG1 == attBG0 {
		t.Fatalf("pass2 want B g1, got g0=%q g1=%q children=%+v", attBG0, attBG1, res2.Workflow.Children)
	}

	// After pass 2: partial must retain exact A + B-g0 + B-g1 (no extra B gens).
	part, err := workflowrun.LoadPartialPrior(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	attsPart := map[string]int{}
	for _, k := range part.WorkflowKids {
		attsPart[k.AttemptID]++
	}
	if attsPart[attA] != 1 || attsPart[attBG0] != 1 || attsPart[attBG1] != 1 {
		t.Fatalf("partial after pass2 want A+B-g0+B-g1 each once: A=%d B0=%d B1=%d kids=%+v",
			attsPart[attA], attsPart[attBG0], attsPart[attBG1], part.WorkflowKids)
	}
	for att, n := range attsPart {
		if strings.Contains(att, "wi_b") && att != attBG0 && att != attBG1 {
			t.Fatalf("partial after pass2 extra B attempt %s count=%d", att, n)
		}
		if n != 1 {
			t.Fatalf("partial after pass2 duplicate %s count=%d", att, n)
		}
	}
	if len(attsPart) != 3 {
		t.Fatalf("partial after pass2 want exactly 3 kids got %d: %+v", len(attsPart), attsPart)
	}

	// Drop goal-checkpoint; partial-only pass 3 must still recover full set.
	_ = os.Remove(filepath.Join(home, "projects", projectID, "runs", runID, "goal-checkpoint.json"))

	calls3 := map[string]int{}
	res3, err3 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(2 * time.Minute) },
		Decompose:     twoChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(2 * time.Minute) },
			Calls: calls3,
		},
	})
	if err3 != nil {
		t.Fatalf("pass3: %v status=%s msg=%s", err3, res3.Status, res3.Message)
	}
	if calls3["wi_a"] != 0 {
		t.Fatalf("pass3 must not re-exec A: %+v", calls3)
	}
	if calls3["wi_b"] != 1 {
		t.Fatalf("pass3 B calls=%d want 1", calls3["wi_b"])
	}
	// Exact canonical A once + B g0/g1/g2 each exactly once.
	seenA, seenB := map[string]int{}, map[string]int{}
	attBG2 := ""
	for _, c := range res3.Workflow.Children {
		if c.WorkItemID == "wi_a" {
			seenA[c.AttemptID]++
			if c.Terminal == "succeeded" && c.AttemptID != attA {
				t.Fatalf("A succeeded attempt drift %s != %s", c.AttemptID, attA)
			}
		}
		if c.WorkItemID == "wi_b" {
			seenB[c.AttemptID]++
			if c.AttemptID != attBG0 && c.AttemptID != attBG1 && strings.HasSuffix(c.AttemptID, "-g2") {
				attBG2 = c.AttemptID
			}
		}
	}
	if len(seenA) != 1 || seenA[attA] != 1 {
		t.Fatalf("A attempts=%+v want exact %s once", seenA, attA)
	}
	if attBG2 == "" {
		// max gen for B
		var maxG int
		for _, c := range res3.Workflow.Children {
			if c.WorkItemID == "wi_b" && c.Generation > maxG {
				maxG = c.Generation
				attBG2 = c.AttemptID
			}
		}
	}
	if attBG2 == "" || attBG2 == attBG0 || attBG2 == attBG1 {
		t.Fatalf("want distinct B g2 got g0=%q g1=%q g2=%q seenB=%+v", attBG0, attBG1, attBG2, seenB)
	}
	if len(seenB) != 3 || seenB[attBG0] != 1 || seenB[attBG1] != 1 || seenB[attBG2] != 1 {
		t.Fatalf("B want exact g0/g1/g2 each once got %+v (g0=%s g1=%s g2=%s)", seenB, attBG0, attBG1, attBG2)
	}
	// Event launches: A once total; each B attempt exactly once; reuse A exactly twice.
	elog, err := workflowrun.OpenEventLog(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := elog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	launches := map[string]int{}
	reusesA := 0
	for _, e := range evs {
		if e.Kind == "launch" {
			launches[e.AttemptID]++
		}
		if e.Kind == "reuse" && e.AttemptID == attA {
			reusesA++
		}
	}
	if launches[attA] != 1 {
		t.Fatalf("A launch count=%d want 1", launches[attA])
	}
	if reusesA != 2 {
		t.Fatalf("A reuse events=%d want exactly 2 (two resumes)", reusesA)
	}
	if launches[attBG0] != 1 || launches[attBG1] != 1 || launches[attBG2] != 1 {
		t.Fatalf("B launches want 1 each: g0=%d g1=%d g2=%d", launches[attBG0], launches[attBG1], launches[attBG2])
	}
	for att, n := range launches {
		if strings.Contains(att, "wi_b") && att != attBG0 && att != attBG1 && att != attBG2 {
			t.Fatalf("extra B launch %s count=%d", att, n)
		}
	}
	// Executor totals: A=1 across all passes; B each pass exactly once.
	if calls1["wi_a"]+calls2["wi_a"]+calls3["wi_a"] != 1 {
		t.Fatalf("executor A total want 1 got pass1=%d pass2=%d pass3=%d", calls1["wi_a"], calls2["wi_a"], calls3["wi_a"])
	}
	if calls1["wi_b"] != 1 || calls2["wi_b"] != 1 || calls3["wi_b"] != 1 {
		t.Fatalf("executor B each pass once: p1=%d p2=%d p3=%d", calls1["wi_b"], calls2["wi_b"], calls3["wi_b"])
	}
	if res3.CheckpointPath == "" {
		t.Fatal("pass3 missing CheckpointPath (checkpoint path coverage)")
	}
}
