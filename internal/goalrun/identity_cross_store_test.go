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
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestCrossStoreCanonicalIdentity asserts one goalrun child carries identical
// canonical ExecutionPlanDigest, GraphDigest, ChildContractDigest, and AttemptID
// across ChildReport, workflow ChildOutcome, capacity ledger Entry (explicit
// fields — not AttemptID-derived), workclaim Claim, launch+terminal events,
// workflow partial, and goal checkpoint. Full 64-hex contract digest and
// positive generation required. Alternate events (if any) keep the same contract.
func TestCrossStoreCanonicalIdentity(t *testing.T) {
	now := time.Date(2026, 7, 22, 22, 30, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	req := env.pinRequest("implement cross-store identity proof", "1397")
	req.ProjectID = "proj-xstore"
	req.RunID = "run_xstore"
	req.HomeDir = env.Home

	res, err := goalrun.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("execute: %v status=%s msg=%s", err, res.Status, res.Message)
	}
	if res.PlanDigest == "" || res.GraphDigest == "" {
		t.Fatalf("result digests empty: plan=%q graph=%q", res.PlanDigest, res.GraphDigest)
	}
	if res.PlanDigest == res.GraphDigest {
		t.Fatalf("PlanDigest must not equal GraphDigest: %q", res.PlanDigest)
	}

	// Prefer implementation child; fall back to first succeeded with contract.
	var report goalrun.ChildReport
	found := false
	for _, c := range res.Children {
		if c.ChildID == "wi_implement" && c.Terminal == "succeeded" {
			report = c
			found = true
			break
		}
	}
	if !found {
		for _, c := range res.Children {
			if c.Terminal == "succeeded" && c.ChildContractDigest != "" && c.AttemptID != "" {
				report = c
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatalf("no succeeded child with contract: %+v", res.Children)
	}

	wantPlan := res.PlanDigest
	wantGraph := res.GraphDigest
	wantCCD := report.ChildContractDigest
	wantAtt := report.AttemptID
	wantClass := report.TaskClass
	wantGen := report.Generation

	// --- ChildReport ---
	assertFullCCD(t, "ChildReport", wantCCD)
	if report.ExecutionPlanDigest != wantPlan {
		t.Fatalf("ChildReport plan: %q want %q", report.ExecutionPlanDigest, wantPlan)
	}
	if report.AttemptID == "" {
		t.Fatal("ChildReport AttemptID empty")
	}
	if wantClass == "" {
		t.Fatal("ChildReport TaskClass empty")
	}
	if wantGen < 1 {
		t.Fatalf("ChildReport.Generation must be positive, got %d", wantGen)
	}

	// --- Workflow ChildOutcome ---
	var outcome workflowrun.ChildOutcome
	foundOut := false
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == report.ChildID && c.AttemptID == wantAtt {
			outcome = c
			foundOut = true
			break
		}
	}
	if !foundOut {
		for _, c := range res.Workflow.Children {
			if c.WorkItemID == report.ChildID {
				outcome = c
				foundOut = true
				break
			}
		}
	}
	if !foundOut {
		t.Fatalf("workflow ChildOutcome missing for %s", report.ChildID)
	}
	if outcome.ExecutionPlanDigest != wantPlan {
		t.Fatalf("ChildOutcome plan: %q want %q", outcome.ExecutionPlanDigest, wantPlan)
	}
	if outcome.ChildContractDigest != wantCCD {
		t.Fatalf("ChildOutcome ccd: %q want %q", outcome.ChildContractDigest, wantCCD)
	}
	if outcome.AttemptID != wantAtt {
		t.Fatalf("ChildOutcome attempt: %q want %q", outcome.AttemptID, wantAtt)
	}
	if outcome.TaskClass != wantClass {
		t.Fatalf("ChildOutcome class: %q want %q", outcome.TaskClass, wantClass)
	}
	if outcome.Generation < 1 {
		t.Fatalf("ChildOutcome Generation must be positive: %d", outcome.Generation)
	}
	wantGen = outcome.Generation
	if report.Generation != wantGen {
		t.Fatalf("ChildReport gen %d != outcome %d", report.Generation, wantGen)
	}
	if res.Workflow.GraphDigest != wantGraph {
		t.Fatalf("workflow GraphDigest: %q want %q", res.Workflow.GraphDigest, wantGraph)
	}
	if res.Workflow.PlanDigest != wantPlan {
		t.Fatalf("workflow PlanDigest: %q want %q", res.Workflow.PlanDigest, wantPlan)
	}

	// --- Capacity ledger Entry (explicit fields) ---
	led, err := capacityledger.OpenPath(env.LedgerPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := led.Get(res.ProjectID, res.RunID, wantAtt)
	if !ok {
		t.Fatalf("ledger entry missing for attempt %q project=%s run=%s", wantAtt, res.ProjectID, res.RunID)
	}
	if entry.PlanDigest != wantPlan {
		t.Fatalf("ledger PlanDigest: %q want %q (must not derive from AttemptID)", entry.PlanDigest, wantPlan)
	}
	if entry.GraphDigest != wantGraph {
		t.Fatalf("ledger GraphDigest: %q want %q", entry.GraphDigest, wantGraph)
	}
	if entry.ChildContractDigest != wantCCD {
		t.Fatalf("ledger ChildContractDigest: %q want %q", entry.ChildContractDigest, wantCCD)
	}
	if entry.TaskClass != wantClass {
		t.Fatalf("ledger TaskClass: %q want %q", entry.TaskClass, wantClass)
	}
	if entry.AttemptID != wantAtt {
		t.Fatalf("ledger AttemptID: %q want %q", entry.AttemptID, wantAtt)
	}

	// --- workclaim Claim ---
	claimPath := filepath.Join(env.Home, "projects", res.ProjectID, "runs", res.RunID, "workclaims.json")
	cs, err := workclaim.OpenPath(claimPath, func() time.Time { return now })
	if err != nil {
		t.Fatalf("open claims %s: %v", claimPath, err)
	}
	cl, err := cs.GetByAttempt(res.ProjectID, res.GraphID, res.Workflow.GraphVersion, report.ChildID, wantAtt)
	if err != nil {
		t.Fatalf("GetByAttempt: %v", err)
	}
	if cl.PlanDigest != wantPlan {
		t.Fatalf("claim PlanDigest: %q want %q", cl.PlanDigest, wantPlan)
	}
	if cl.GraphDigest != wantGraph {
		t.Fatalf("claim GraphDigest: %q want %q", cl.GraphDigest, wantGraph)
	}
	if cl.ChildContractDigest != wantCCD {
		t.Fatalf("claim ChildContractDigest: %q want %q", cl.ChildContractDigest, wantCCD)
	}
	if cl.TaskClass != wantClass {
		t.Fatalf("claim TaskClass: %q want %q", cl.TaskClass, wantClass)
	}
	if cl.Generation < 1 {
		t.Fatalf("claim Generation must be positive: %d", cl.Generation)
	}
	if int(cl.Generation) != wantGen {
		t.Fatalf("claim Generation %d != outcome %d", cl.Generation, wantGen)
	}

	// --- launch + terminal events ---
	elogPath := res.Workflow.EventLogPath
	if elogPath == "" {
		elogPath = filepath.Join(env.Home, "projects", res.ProjectID, "runs", res.RunID, "workflow-events.jsonl")
	}
	raw, err := os.ReadFile(elogPath)
	if err != nil {
		t.Fatal(err)
	}
	events, err := workflowrun.ParseEventJSONLStrict(string(raw), res.ProjectID, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var sawLaunch, sawTerminal bool
	for _, ev := range events {
		if ev.AttemptID != wantAtt {
			// Alternate attempts: same contract digest when present.
			if ev.ChildContractDigest != "" && ev.WorkItemID == report.ChildID {
				if ev.ChildContractDigest != wantCCD {
					t.Fatalf("event kind=%s attempt=%s ccd=%q want same %q",
						ev.Kind, ev.AttemptID, ev.ChildContractDigest, wantCCD)
				}
				if ev.ExecutionPlanDigest != "" && ev.ExecutionPlanDigest != wantPlan {
					t.Fatalf("event kind=%s plan %q want %q", ev.Kind, ev.ExecutionPlanDigest, wantPlan)
				}
				if ev.GraphDigest != "" && ev.GraphDigest != wantGraph {
					t.Fatalf("event kind=%s graph %q want %q", ev.Kind, ev.GraphDigest, wantGraph)
				}
			}
			continue
		}
		switch ev.Kind {
		case "launch":
			sawLaunch = true
		case "terminal":
			sawTerminal = true
		case "claim", "reuse", "model_unavailable", "reroute", "integrate":
			// identity required
		default:
			continue
		}
		if ev.ExecutionPlanDigest != wantPlan {
			t.Fatalf("event %s plan: %q want %q", ev.Kind, ev.ExecutionPlanDigest, wantPlan)
		}
		if ev.GraphDigest != wantGraph {
			t.Fatalf("event %s graph: %q want %q", ev.Kind, ev.GraphDigest, wantGraph)
		}
		if ev.ChildContractDigest != wantCCD {
			t.Fatalf("event %s ccd: %q want %q", ev.Kind, ev.ChildContractDigest, wantCCD)
		}
		if ev.TaskClass != wantClass {
			t.Fatalf("event %s class: %q want %q", ev.Kind, ev.TaskClass, wantClass)
		}
		// Payload reconstruction fields.
		if len(ev.Payload) > 0 {
			var m map[string]string
			if json.Unmarshal(ev.Payload, &m) == nil {
				if m["child_contract_digest"] != "" && m["child_contract_digest"] != wantCCD {
					t.Fatalf("event %s payload ccd: %q", ev.Kind, m["child_contract_digest"])
				}
				if m["execution_plan_digest"] != "" && m["execution_plan_digest"] != wantPlan {
					t.Fatalf("event %s payload plan: %q", ev.Kind, m["execution_plan_digest"])
				}
			}
		}
	}
	if !sawLaunch {
		t.Fatal("missing launch event for attempt")
	}
	if !sawTerminal {
		t.Fatal("missing terminal event for attempt")
	}

	// --- workflow partial ---
	partial, err := workflowrun.LoadPartialPrior(env.Home, res.ProjectID, res.RunID)
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	if partial.PlanDigest != wantPlan && partial.ExecutionPlanDigest != wantPlan {
		t.Fatalf("partial plan: plan=%q exec=%q want %q", partial.PlanDigest, partial.ExecutionPlanDigest, wantPlan)
	}
	if partial.GraphDigest != wantGraph {
		t.Fatalf("partial graph: %q want %q", partial.GraphDigest, wantGraph)
	}
	ps, ok := partial.PriorSucceeded[report.ChildID]
	if !ok {
		t.Fatalf("partial.PriorSucceeded missing child %s (required for succeeded child)", report.ChildID)
	}
	if ps.ChildContractDigest != wantCCD {
		t.Fatalf("partial child ccd: %q want %q", ps.ChildContractDigest, wantCCD)
	}
	if ps.ExecutionPlanDigest != wantPlan {
		t.Fatalf("partial child plan: %q want %q", ps.ExecutionPlanDigest, wantPlan)
	}
	if ps.AttemptID != wantAtt {
		t.Fatalf("partial child attempt: %q want %q", ps.AttemptID, wantAtt)
	}
	if ps.Generation != wantGen {
		t.Fatalf("partial child gen %d want %d", ps.Generation, wantGen)
	}

	// --- goal checkpoint ---
	cp, _, err := goalrun.LoadCheckpoint(env.Home, res.ProjectID, res.RunID)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp.PlanDigest != wantPlan {
		t.Fatalf("checkpoint PlanDigest: %q want %q", cp.PlanDigest, wantPlan)
	}
	if cp.GraphDigest != wantGraph {
		t.Fatalf("checkpoint GraphDigest: %q want %q", cp.GraphDigest, wantGraph)
	}
	// Dual ChildReports (MU failed + winner) are valid; pin winner by AttemptID.
	foundCP := false
	for _, c := range cp.Children {
		if c.ChildID != report.ChildID {
			continue
		}
		if c.AttemptID != wantAtt {
			// Non-winner row (e.g. model_unavailable failed) may coexist.
			continue
		}
		foundCP = true
		if c.ChildContractDigest != wantCCD {
			t.Fatalf("checkpoint child ccd: %q want %q", c.ChildContractDigest, wantCCD)
		}
		if c.ExecutionPlanDigest != wantPlan {
			t.Fatalf("checkpoint child plan: %q want %q", c.ExecutionPlanDigest, wantPlan)
		}
	}
	if !foundCP {
		t.Fatalf("checkpoint missing winner child %s attempt %s", report.ChildID, wantAtt)
	}
	foundWK := false
	for _, c := range cp.WorkflowKids {
		if c.WorkItemID != report.ChildID {
			continue
		}
		foundWK = true
		if c.ChildContractDigest != wantCCD {
			t.Fatalf("checkpoint workflow kid ccd: %q", c.ChildContractDigest)
		}
		if c.ExecutionPlanDigest != wantPlan {
			t.Fatalf("checkpoint workflow kid plan: %q", c.ExecutionPlanDigest)
		}
		if c.AttemptID != wantAtt {
			t.Fatalf("checkpoint workflow kid attempt: %q want %q", c.AttemptID, wantAtt)
		}
		if c.Generation != wantGen {
			t.Fatalf("checkpoint workflow kid gen %d want %d", c.Generation, wantGen)
		}
	}
	if !foundWK {
		t.Fatalf("checkpoint WorkflowKids missing child %s", report.ChildID)
	}
}

func assertFullCCD(t *testing.T, where, ccd string) {
	t.Helper()
	const prefix = "sha256:"
	if !strings.HasPrefix(ccd, prefix) {
		t.Fatalf("%s ccd must start with %q: %q", where, prefix, ccd)
	}
	hexPart := strings.TrimPrefix(ccd, prefix)
	if len(hexPart) != 64 {
		t.Fatalf("%s ccd must be full 64-hex sha256, len=%d: %q", where, len(hexPart), ccd)
	}
	for _, c := range hexPart {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("%s ccd hex must be lowercase [0-9a-f]: %q", where, ccd)
		}
	}
}
