package goalrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func eventLogPath(home, project, runID string) string {
	return filepath.Join(home, "projects", project, "runs", runID, "workflow-events.jsonl")
}

func writeEventLines(t *testing.T, path string, events []workflowrun.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range events {
		if e.Schema == "" {
			e.Schema = workflowrun.EventSchema
		}
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func countLines(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	n := 0
	for _, c := range raw {
		if c == '\n' {
			n++
		}
	}
	if raw[len(raw)-1] != '\n' {
		n++
	}
	return n
}

// oneImplChild is a single-item graph for event-log resume proofs.
func oneImplChild(now time.Time, graphID string) func(workgraph.DecomposeOptions) (workgraph.Graph, error) {
	return func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: graphID, Version: 1,
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
}

func probeOneImplDigests(t *testing.T, now time.Time, projectID, goal, graphID string) (planDig, graphDig, class, ccd, gID string) {
	t.Helper()
	env := newProductEnv(t, now, "codex")
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID + "-probe", RunID: "run_probe_ev",
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: env.Home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, graphID),
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: workflowrun.FakeChildExecutor{HomeDir: env.Home, Now: func() time.Time { return now }},
	})
	if res.PlanDigest == "" {
		t.Fatalf("probe: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == "wi_only" && c.ChildContractDigest != "" {
			return res.PlanDigest, res.GraphDigest, c.TaskClass, c.ChildContractDigest, res.GraphID
		}
	}
	t.Fatalf("probe missing wi_only: %+v", res.Workflow.Children)
	return "", "", "", "", ""
}

// TestEventLogAdvancedG1WhileCheckpointLagged_ResumesExactG2: production interrupt
// then clear AttemptGeneration (lag) while WorkflowKids+Aborted remain; resume gN+1.
func TestEventLogAdvancedG1WhileCheckpointLagged_ResumesExactG2(t *testing.T) {
	now := time.Date(2026, 7, 24, 20, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-ev-g1lag"
	runID := "run_ev_g1lag"
	goal := "implement event log g1 lag resume to g2"

	ctx1, cancel1 := context.WithCancel(context.Background())
	_, err1 := goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_g1lag"),
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
	_ = err1
	cp, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	aborted := ""
	for _, k := range cp.WorkflowKids {
		if k.WorkItemID == "wi_only" && k.Terminal == "cancelled" {
			aborted = k.AttemptID
		}
	}
	if aborted == "" {
		t.Fatalf("no cancelled kid: %+v", cp.WorkflowKids)
	}
	// Lag: clear AttemptGeneration only.
	cp.AttemptGeneration = nil
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}
	wantG2 := workflowrun.AttemptID("wi_only", cp.PlanDigest, runID, workflowrun.ParseAttemptGeneration(aborted)+1)
	attG1 := aborted
	class, ccd := "", ""
	_ = class
	_ = ccd

	calls := map[string]int{}
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     oneImplChild(now, "g_ev_g1lag"),
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls,
		},
	})
	if err != nil {
		t.Fatalf("resume: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if calls["wi_only"] != 1 {
		t.Fatalf("calls=%+v want 1", calls)
	}
	var got string
	var maxGen int
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == "wi_only" && c.Generation > maxGen {
			maxGen = c.Generation
			got = c.AttemptID
		}
	}
	if got != wantG2 {
		t.Fatalf("attempt=%q want exact g2 %q (open was g1 %q)",
			got, wantG2, attG1)
	}
	elogPath := eventLogPath(home, projectID, runID)
	beforeLines := 0
	_ = beforeLines
	// Never a second g1 launch.
	afterRaw, err := os.ReadFile(elogPath)
	if err != nil {
		t.Fatal(err)
	}
	events, err := workflowrun.ParseEventJSONLStrict(string(afterRaw), projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	g1Launch, g2Launch := 0, 0
	for _, e := range events {
		if e.Kind != "launch" || e.WorkItemID != "wi_only" {
			continue
		}
		if e.AttemptID == attG1 {
			g1Launch++
		}
		if e.AttemptID == wantG2 {
			g2Launch++
		}
	}
	if g1Launch != 1 {
		t.Fatalf("g1 launches=%d want 1 (no second g1)", g1Launch)
	}
	if g2Launch != 1 {
		t.Fatalf("g2 launches=%d want 1", g2Launch)
	}
	// Recovery may append interrupt; g1 line count must not grow via re-launch.
	_ = beforeLines
}

// TestEventLogGhostLaunch_FailBeforeSpend_LogUnchanged.
func TestEventLogGhostLaunch_FailBeforeSpend_LogUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 24, 20, 30, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-ev-ghost"
	runID := "run_ev_ghost"
	goal := "implement event ghost launch fail closed"
	planDig, graphDig, class, ccd, graphID := probeOneImplDigests(t, now, projectID, goal, "g_ev_ghost")

	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked",
		SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}

	// Ghost work item launch with forged-looking attempt.
	ghostAtt := workflowrun.AttemptID("wi_ghost", planDig, runID, 0)
	elogPath := eventLogPath(home, projectID, runID)
	writeEventLines(t, elogPath, []workflowrun.Event{
		{ProjectID: projectID, RunID: runID, Kind: "run.start", EventID: "wev_0"},
		{ProjectID: projectID, RunID: runID, Kind: "launch", WorkItemID: "wi_ghost",
			AttemptID: ghostAtt, Generation: 1, EventID: "wev_1"}, // gen 1 = g0 suffix+1; ghost fails on work item
	})
	before, err := os.ReadFile(elogPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeLines := countLines(before)

	ledgerPath := filepath.Join(t.TempDir(), "cap-ev-ghost.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err = goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_ghost"),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected ghost launch fail")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err should mention ghost: %v", err)
	}
	assertNoInventoryLedgerExecutor(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
	// Event log bytes and line count unchanged (no recovery interrupt).
	after, err := os.ReadFile(elogPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("event log mutated on invalid ghost; beforeLines=%d afterLines=%d",
			beforeLines, countLines(after))
	}
	if countLines(after) != beforeLines {
		t.Fatalf("line count %d -> %d", beforeLines, countLines(after))
	}
	_ = class
	_ = ccd
}

// assertNoInventoryLedgerExecutor requires zero spend; allows pre-existing event log.
func assertNoInventoryLedgerExecutor(
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
	if _, err := os.Stat(filepath.Join(home, "projects", projectID, "runs", runID, "workclaims.json")); err == nil {
		t.Fatal("workclaims must not exist")
	}
	if _, err := os.Stat(ledgerPath); err == nil {
		t.Fatalf("ledger file must not exist (OpenLedger must be 0): %s", ledgerPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("ledger stat: %v", err)
	}
}

// TestEventLogMalformedAttempt_FailBeforeSpend_LogUnchanged.
func TestEventLogMalformedAttempt_FailBeforeSpend_LogUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 24, 21, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-ev-mal"
	runID := "run_ev_mal"
	goal := "implement event malformed attempt fail closed"
	planDig, graphDig, _, _, graphID := probeOneImplDigests(t, now, projectID, goal, "g_ev_mal")

	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked", SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}
	elogPath := eventLogPath(home, projectID, runID)
	writeEventLines(t, elogPath, []workflowrun.Event{
		{ProjectID: projectID, RunID: runID, Kind: "run.start", EventID: "wev_0"},
		{ProjectID: projectID, RunID: runID, Kind: "launch", WorkItemID: "wi_only",
			AttemptID: "att-wi_only-not-a-generation", Generation: 1, EventID: "wev_1"},
	})
	before, _ := os.ReadFile(elogPath)
	beforeLines := countLines(before)

	ledgerPath := filepath.Join(t.TempDir(), "cap-ev-mal.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_mal"),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected malformed attempt fail")
	}
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "malformed") &&
		!strings.Contains(msg, "attempt_id") &&
		!strings.Contains(msg, "event-only") {
		t.Fatalf("err=%v", err)
	}
	assertNoInventoryLedgerExecutor(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
	after, _ := os.ReadFile(elogPath)
	if string(after) != string(before) || countLines(after) != beforeLines {
		t.Fatal("event log mutated on malformed attempt")
	}
}

// TestEventLogCrossPlanAttempt_FailBeforeSpend_LogUnchanged.
func TestEventLogCrossPlanAttempt_FailBeforeSpend_LogUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 24, 21, 30, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-ev-xplan"
	runID := "run_ev_xplan"
	goal := "implement event cross-plan attempt fail closed"
	planDig, graphDig, _, _, graphID := probeOneImplDigests(t, now, projectID, goal, "g_ev_xplan")

	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked", SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}
	// Wrong short-hash for this plan/run.
	badAtt := "att-wi_only-deadbeefdead-g1"
	elogPath := eventLogPath(home, projectID, runID)
	writeEventLines(t, elogPath, []workflowrun.Event{
		{ProjectID: projectID, RunID: runID, Kind: "run.start", EventID: "wev_0"},
		{ProjectID: projectID, RunID: runID, Kind: "launch", WorkItemID: "wi_only",
			AttemptID: badAtt, Generation: 2, EventID: "wev_1"},
	})
	before, _ := os.ReadFile(elogPath)
	beforeLines := countLines(before)

	ledgerPath := filepath.Join(t.TempDir(), "cap-ev-xplan.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_xplan"),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected cross-plan fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "canonical") && !strings.Contains(msg, "cross-plan") &&
		!strings.Contains(msg, "event-only") {
		t.Fatalf("err=%v", err)
	}
	assertNoInventoryLedgerExecutor(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
	after, _ := os.ReadFile(elogPath)
	if string(after) != string(before) || countLines(after) != beforeLines {
		t.Fatal("event log mutated on cross-plan attempt")
	}
}

// TestEventLogCorruptJSON_FailBeforeSpend_LogUnchanged.
func TestEventLogCorruptJSON_FailBeforeSpend_LogUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 24, 22, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-ev-corrupt"
	runID := "run_ev_corrupt"
	goal := "implement event corrupt json fail closed"
	planDig, graphDig, _, _, graphID := probeOneImplDigests(t, now, projectID, goal, "g_ev_corrupt")

	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked", SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}
	elogPath := eventLogPath(home, projectID, runID)
	if err := os.MkdirAll(filepath.Dir(elogPath), 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{\"schema\":\"loopcoder.workflow.event.v1\",\"kind\":\"run.start\"}\n{not-json\n")
	if err := os.WriteFile(elogPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), corrupt...)
	beforeLines := countLines(before)

	ledgerPath := filepath.Join(t.TempDir(), "cap-ev-corrupt.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_corrupt"),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected corrupt JSON fail")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "parse") &&
		!strings.Contains(strings.ToLower(err.Error()), "json") &&
		!strings.Contains(strings.ToLower(err.Error()), "event log") {
		t.Fatalf("err=%v", err)
	}
	assertNoInventoryLedgerExecutor(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
	after, _ := os.ReadFile(elogPath)
	if string(after) != string(before) || countLines(after) != beforeLines {
		t.Fatal("event log mutated on corrupt JSON")
	}
}

// TestEventLogMissingBothIDs_FailBeforeRecovery_LogUnchanged.
func TestEventLogMissingBothIDs_FailBeforeRecovery_LogUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 24, 22, 30, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-ev-emptyid"
	runID := "run_ev_emptyid"
	goal := "implement event empty both ids fail closed"
	planDig, graphDig, _, _, graphID := probeOneImplDigests(t, now, projectID, goal, "g_ev_emptyid")
	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked", SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}
	elogPath := eventLogPath(home, projectID, runID)
	writeEventLines(t, elogPath, []workflowrun.Event{
		{ProjectID: projectID, RunID: runID, Kind: "run.start", EventID: "wev_0"},
		// Child launch with both IDs empty — must fail closed before recovery.
		{ProjectID: projectID, RunID: runID, Kind: "launch", EventID: "wev_1"},
	})
	before, _ := os.ReadFile(elogPath)
	beforeLines := countLines(before)
	ledgerPath := filepath.Join(t.TempDir(), "cap-emptyid.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_emptyid"),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected empty-identity fail")
	}
	assertNoInventoryLedgerExecutor(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
	after, _ := os.ReadFile(elogPath)
	if string(after) != string(before) || countLines(after) != beforeLines {
		t.Fatal("event log mutated")
	}
}

// TestEventLogPartialChildInterrupt_FailBeforeRecovery_LogUnchanged.
func TestEventLogPartialChildInterrupt_FailBeforeRecovery_LogUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 24, 22, 45, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-ev-partint"
	runID := "run_ev_partint"
	goal := "implement event partial child interrupt fail closed"
	planDig, graphDig, _, _, graphID := probeOneImplDigests(t, now, projectID, goal, "g_ev_partint")
	cp := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked", SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
		t.Fatal(err)
	}
	elogPath := eventLogPath(home, projectID, runID)
	att := workflowrun.AttemptID("wi_only", planDig, runID, 0)
	writeEventLines(t, elogPath, []workflowrun.Event{
		{ProjectID: projectID, RunID: runID, Kind: "run.start", EventID: "wev_0"},
		{ProjectID: projectID, RunID: runID, Kind: "launch", WorkItemID: "wi_only", AttemptID: att, Generation: 1, EventID: "wev_1"},
		// Child interrupt with work_item but no attempt_id.
		{ProjectID: projectID, RunID: runID, Kind: "interrupt", WorkItemID: "wi_only", EventID: "wev_2"},
	})
	before, _ := os.ReadFile(elogPath)
	ledgerPath := filepath.Join(t.TempDir(), "cap-partint.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_partint"),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("expected partial interrupt fail")
	}
	assertNoInventoryLedgerExecutor(t, home, projectID, runID, ledgerPath, &invN, &ledN, calls)
	after, _ := os.ReadFile(elogPath)
	if string(after) != string(before) {
		t.Fatal("event log mutated")
	}
}

// TestEventLogG1Generation1Fails_G1Generation2Passes: generation must be suffix+1.
func TestEventLogG1Generation1Fails_G1Generation2Passes(t *testing.T) {
	now := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-ev-genmatch"
	goal := "implement event generation match contract"
	planDig, graphDig, _, _, graphID := probeOneImplDigests(t, now, projectID, goal, "g_ev_genmatch")

	// Fail path: g1 with Generation=1.
	runBad := "run_ev_gen_bad"
	cpBad := goalrun.Checkpoint{
		Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runBad,
		GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
		Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked", SavedAt: now,
	}
	if _, err := goalrun.SaveCheckpoint(home, cpBad); err != nil {
		t.Fatal(err)
	}
	attG1 := workflowrun.AttemptID("wi_only", planDig, runBad, 1)
	elogBad := eventLogPath(home, projectID, runBad)
	writeEventLines(t, elogBad, []workflowrun.Event{
		{ProjectID: projectID, RunID: runBad, Kind: "run.start", EventID: "wev_0"},
		{ProjectID: projectID, RunID: runBad, Kind: "launch", WorkItemID: "wi_only",
			AttemptID: attG1, Generation: 1, EventID: "wev_1"}, // wrong: want 2
	})
	before, _ := os.ReadFile(elogBad)
	ledgerPath := filepath.Join(t.TempDir(), "cap-genbad.json")
	invN, ledN := 0, 0
	calls := map[string]int{}
	_, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runBad, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_genmatch"),
		LoadInventory: countingInv(env, &invN),
		OpenLedger:    countingLed(ledgerPath, &ledN),
		Executor: workflowrun.FakeChildExecutor{
			HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
		},
	})
	if err == nil {
		t.Fatal("g1 with Generation=1 must fail")
	}
	assertNoInventoryLedgerExecutor(t, home, projectID, runBad, ledgerPath, &invN, &ledN, calls)
	after, _ := os.ReadFile(elogBad)
	if string(after) != string(before) {
		t.Fatal("log mutated on gen mismatch")
	}

	// Pass path: production interrupt then resume next gen.
	runOK := "run_ev_gen_ok"
	ctx1, cancel1 := context.WithCancel(context.Background())
	_, _ = goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runOK,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneImplChild(now, "g_ev_genmatch"),
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
	cpOK, _, err := goalrun.LoadCheckpoint(home, projectID, runOK)
	if err != nil {
		t.Fatal(err)
	}
	aborted := ""
	for _, k := range cpOK.WorkflowKids {
		if k.WorkItemID == "wi_only" && k.Terminal == "cancelled" {
			aborted = k.AttemptID
		}
	}
	if aborted == "" {
		t.Fatalf("no cancelled kid: %+v", cpOK.WorkflowKids)
	}
	wantG2 := workflowrun.AttemptID("wi_only", cpOK.PlanDigest, runOK, workflowrun.ParseAttemptGeneration(aborted)+1)
	calls2 := map[string]int{}
	res, err := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runOK, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     oneImplChild(now, "g_ev_genmatch"),
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2,
		},
	})
	if err != nil {
		t.Fatalf("g1 gen=2 should pass: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	var got string
	var maxGen int
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == "wi_only" && c.Generation > maxGen {
			maxGen = c.Generation
			got = c.AttemptID
		}
	}
	if got != wantG2 {
		t.Fatalf("attempt=%q want %q children=%+v", got, wantG2, res.Workflow.Children)
	}
	_ = planDig
	_ = graphDig
}

// Ensure capacityledger import used (assert helper paths).
var _ = capacityledger.Entry{}
