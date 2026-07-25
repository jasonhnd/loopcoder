package goalrun_test

import (
	"context"
	"encoding/json"
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
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestTamperedPartialWorkflowKids_ZeroSpendBeforeFail: resume preflight must
// reject tampered durable kids before inventory/ledger/eventlog mutation/claim/exec.
// Coherent dual-tamper of kid CCD + matching event CCD still fails (current graph CCD wins).
func TestTamperedPartialWorkflowKids_ZeroSpendBeforeFail(t *testing.T) {
	now := time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-tamper-prior"
	runID := "run_tamper_prior_1"
	goal := "implement tamper prior outcomes zero spend"
	oneChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_tamper_prior", Version: 1,
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
	_ = os.Remove(filepath.Join(home, "projects", projectID, "runs", runID, "goal-checkpoint.json"))
	part, err := workflowrun.LoadPartialPrior(home, projectID, runID)
	if err != nil {
		t.Fatalf("partial: %v", err)
	}
	if len(part.WorkflowKids) == 0 {
		t.Fatal("pass1 partial WorkflowKids empty")
	}
	runDir := filepath.Join(home, "projects", projectID, "runs", runID)
	elogPath := filepath.Join(runDir, "workflow-events.jsonl")
	claimsPath := filepath.Join(runDir, "workclaims.json")
	beforeElog, _ := os.ReadFile(elogPath)
	beforeClaims, _ := os.ReadFile(claimsPath)

	// Coherent dual-tamper: same forged CCD on kid AND all lifecycle events for that attempt.
	// Must still fail zero-spend because current graph CCD does not match.
	forgedCCD := "sha256:" + strings.Repeat("00", 32)
	part.WorkflowKids[0].ChildContractDigest = forgedCCD
	raw, _ := json.MarshalIndent(part, "", "  ")
	if err := os.WriteFile(filepath.Join(runDir, "workflow-partial.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Rewrite event log lines: replace original CCD with forged for the aborted attempt.
	origCCD := ""
	// recover original from beforeElog
	for _, line := range strings.Split(string(beforeElog), "\n") {
		if strings.Contains(line, part.WorkflowKids[0].AttemptID) && strings.Contains(line, "child_contract_digest") {
			// extract one sha256 ccd
			idx := strings.Index(line, "sha256:")
			if idx >= 0 && len(line) >= idx+7+64 {
				// find first sha256 in line that is long enough — use regex-like scan
			}
		}
	}
	// Simpler: rewrite every event JSON for the attempt to use forged CCD.
	var lines []string
	for _, line := range strings.Split(string(beforeElog), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			lines = append(lines, line)
			continue
		}
		if att, _ := m["attempt_id"].(string); att == part.WorkflowKids[0].AttemptID {
			if ccd, ok := m["child_contract_digest"].(string); ok && ccd != "" {
				origCCD = ccd
			}
			m["child_contract_digest"] = forgedCCD
			b, _ := json.Marshal(m)
			lines = append(lines, string(b))
			continue
		}
		lines = append(lines, line)
	}
	if origCCD == "" {
		t.Fatal("could not find original CCD on attempt events")
	}
	if err := os.WriteFile(elogPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Update beforeElog snapshot after our dual-tamper for mutation checks.
	beforeElog, _ = os.ReadFile(elogPath)
	beforePart := raw

	invCalls, ledCalls := 0, 0
	execCalls := map[string]int{}
	_, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose: oneChild,
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			invCalls++
			return env.loadInv()(ctx, repo, at)
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			ledCalls++
			return env.openLed()(nowFn)
		},
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: execCalls,
		},
	})
	if err2 == nil {
		t.Fatal("coherent dual-tamper CCD must fail closed before spend")
	}
	if invCalls != 0 {
		t.Fatalf("inventory must not run: invCalls=%d err=%v", invCalls, err2)
	}
	if ledCalls != 0 {
		t.Fatalf("ledger must not open: ledCalls=%d err=%v", ledCalls, err2)
	}
	if len(execCalls) != 0 {
		t.Fatalf("executor Calls must be zero: %+v err=%v", execCalls, err2)
	}
	afterElog, _ := os.ReadFile(elogPath)
	afterClaims, _ := os.ReadFile(claimsPath)
	afterPart, _ := os.ReadFile(filepath.Join(runDir, "workflow-partial.json"))
	if string(beforeElog) != string(afterElog) {
		t.Fatal("event log mutated after tamper resume fail")
	}
	if string(beforeClaims) != string(afterClaims) {
		t.Fatal("claims mutated after tamper resume fail")
	}
	if string(afterPart) != string(beforePart) {
		t.Fatal("partial rewritten by failed resume")
	}
	msg := err2.Error()
	if !strings.Contains(msg, "CCD") && !strings.Contains(msg, "child_contract") &&
		!strings.Contains(msg, "PriorOutcomes") {
		t.Logf("fail-closed error: %v", err2)
	}
}
