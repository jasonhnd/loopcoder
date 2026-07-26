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
)

// TestResumePriorIdentityMismatchFailClosed refuses reuse when prior durable
// identity does not exactly equal the current materialized child contract.
// Present-but-invalid priors (empty attempt/evidence/terminal, failed terminal,
// missing plan/class/CCD/work-item) must fail closed before route/reserve/exec
// with zero provider calls and zero new ledger reservations.
func TestResumePriorIdentityMismatchFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-resume-mm"
	runID := "run_resume_mm"
	goal := "implement resume mismatch matrix"
	planDig, graphDig, class, ccd, _, graphID := childContractForGoal(t, goal, "1397", "owner", projectID, "wi_research", now)
	canonAtt := workflowrun.AttemptID("wi_research", planDig, runID, 0)

	basePrior := workflowrun.ChildOutcome{
		WorkItemID: "wi_research", Terminal: "succeeded",
		AttemptID: canonAtt, OutputEvidence: "sha256:ev",
		Provider: "codex", Model: "gpt-5.5", Depth: "low", Permission: "read-only",
		TaskClass: class, ExecutionPlanDigest: planDig,
		ChildContractDigest: ccd, Generation: 1,
	}

	cases := []struct {
		name string
		mut  func(*workflowrun.ChildOutcome)
		sub  string
	}{
		{"plan", func(p *workflowrun.ChildOutcome) { p.ExecutionPlanDigest = "sha256:wrongplan" }, "execution_plan_digest"},
		{"ccd", func(p *workflowrun.ChildOutcome) {
			p.ChildContractDigest = "sha256:" + "c" + ccd[len("sha256:")+1:]
		}, "child_contract_digest"},
		{"class", func(p *workflowrun.ChildOutcome) { p.TaskClass = "tera" }, "task_class"},
		{"work_item", func(p *workflowrun.ChildOutcome) { p.WorkItemID = "wi_other" }, "work_item_id"},
		{"generation_zero", func(p *workflowrun.ChildOutcome) { p.Generation = 0 }, "generation"},
		{"missing_class", func(p *workflowrun.ChildOutcome) { p.TaskClass = "" }, "task_class"},
		{"missing_ccd", func(p *workflowrun.ChildOutcome) { p.ChildContractDigest = "" }, "child_contract_digest"},
		{"missing_plan", func(p *workflowrun.ChildOutcome) { p.ExecutionPlanDigest = "" }, "execution_plan_digest"},
		{"attempt_empty", func(p *workflowrun.ChildOutcome) { p.AttemptID = "" }, "attempt_id"},
		{"evidence_empty", func(p *workflowrun.ChildOutcome) { p.OutputEvidence = "" }, "output_evidence"},
		{"terminal_empty", func(p *workflowrun.ChildOutcome) { p.Terminal = "" }, "terminal"},
		{"terminal_failed", func(p *workflowrun.ChildOutcome) { p.Terminal = "failed" }, "terminal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prior := basePrior
			tc.mut(&prior)
			thisRun := runID + "_" + tc.name
			// Canonical attempt is run-scoped; recompute for this case run id.
			prior.AttemptID = workflowrun.AttemptID("wi_research", planDig, thisRun, prior.Generation-1)
			if tc.name == "attempt_empty" {
				prior.AttemptID = ""
			}
			if tc.name == "generation_zero" {
				// keep Generation=0; AttemptID may still be set for message checks
				prior.AttemptID = workflowrun.AttemptID("wi_research", planDig, thisRun, 0)
			}
			cp := goalrun.Checkpoint{
				Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: thisRun,
				GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
				Goal: goal, Issue: "1397", Actor: "owner",
				Status: "blocked", Interrupted: true,
				PriorSucceeded: map[string]workflowrun.ChildOutcome{"wi_research": prior},
				// No unbound AbortedAttempts without WorkflowKids (forgeable gen seed).
				SavedAt: now,
			}
			if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
				t.Fatal(err)
			}
			// Fresh ledger path per case so entry counts are unambiguous.
			ledgerPath := filepath.Join(t.TempDir(), "cap-"+tc.name+".json")
			openLed := func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
				return capacityledger.OpenPath(ledgerPath, nowFn)
			}
			calls := map[string]int{}
			_, err := goalrun.Execute(context.Background(), goalrun.Request{
				ProjectID: projectID, RunID: thisRun, Resume: true,
				Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
				Provider: "codex", Model: "gpt-5.5",
				HomeDir: home, Now: func() time.Time { return now },
				LoadInventory: env.loadInv(), OpenLedger: openLed,
				Executor: workflowrun.FakeChildExecutor{
					HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
				},
			})
			if err == nil {
				t.Fatalf("expected fail closed for %s", tc.name)
			}
			if !stringsContainsCI(err.Error(), tc.sub) {
				t.Fatalf("error %q should mention %q", err.Error(), tc.sub)
			}
			// Zero provider/executor calls — present-invalid must not fall through to re-exec.
			for id, n := range calls {
				if n != 0 {
					t.Fatalf("must not re-exec any child on present-invalid prior %s: calls=%+v (hit %s=%d)",
						tc.name, calls, id, n)
				}
			}
			if calls["wi_research"] != 0 {
				t.Fatalf("must not re-exec research on mismatch: %+v", calls)
			}
			// Zero new ledger reservations for this run.
			raw, rerr := os.ReadFile(ledgerPath)
			if rerr != nil && !os.IsNotExist(rerr) {
				t.Fatalf("read ledger: %v", rerr)
			}
			if rerr == nil && len(raw) > 0 {
				var doc struct {
					Entries []capacityledger.Entry `json:"entries"`
				}
				if jerr := json.Unmarshal(raw, &doc); jerr != nil {
					t.Fatalf("ledger json: %v", jerr)
				}
				for _, e := range doc.Entries {
					if e.RunID == thisRun {
						t.Fatalf("present-invalid prior must not create ledger reservation: %+v", e)
					}
					if e.ProjectID == projectID && e.RunID == thisRun {
						t.Fatalf("zero reservations required; got entry %+v", e)
					}
				}
				if len(doc.Entries) != 0 {
					// Any entry at all for this fresh ledger is a re-spend.
					t.Fatalf("expected zero ledger entries after present-invalid fail-closed, got %d: %+v",
						len(doc.Entries), doc.Entries)
				}
			}
		})
	}
}

func stringsContainsCI(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
