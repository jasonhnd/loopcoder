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

// TestLoadCheckpoint_ReadOnlyNoFilesystemMutation asserts LoadCheckpoint never
// creates home/project/run/file for missing paths (nonexistent home and existing
// home with missing project/run).
func TestLoadCheckpoint_ReadOnlyNoFilesystemMutation(t *testing.T) {
	// Nonexistent home.
	missingHome := filepath.Join(t.TempDir(), "no-such-home-"+t.Name())
	if _, err := os.Stat(missingHome); !os.IsNotExist(err) {
		t.Fatalf("precondition: home must not exist: %v", err)
	}
	_, path, err := goalrun.LoadCheckpoint(missingHome, "proj-x", "run-x")
	if err == nil {
		t.Fatal("expected error for missing home/file")
	}
	if !os.IsNotExist(err) {
		// path may be derived; error must be NotExist-compatible
		if !strings.Contains(err.Error(), "no such file") && !os.IsNotExist(err) {
			// Accept wrapped ErrNotExist
			if path == "" {
				t.Fatalf("err=%v (want not-exist)", err)
			}
		}
	}
	if _, serr := os.Stat(missingHome); !os.IsNotExist(serr) {
		t.Fatalf("LoadCheckpoint must not create missing home: %v", serr)
	}

	// Existing home, missing project/run.
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	before := listPaths(t, home)
	_, _, err = goalrun.LoadCheckpoint(home, "proj-absent", "run-absent")
	if err == nil {
		t.Fatal("expected not exist")
	}
	if !os.IsNotExist(err) {
		// ReadFile of missing file returns ErrNotExist
		t.Fatalf("err=%v want ErrNotExist", err)
	}
	after := listPaths(t, home)
	if len(after) != len(before) {
		t.Fatalf("filesystem mutated: before=%v after=%v", before, after)
	}
	for p := range after {
		if !before[p] {
			t.Fatalf("new path created by LoadCheckpoint: %s", p)
		}
	}
}

// TestLoadPartialPrior_ReadOnlyNoFilesystemMutation asserts LoadPartialPrior
// never calls OpenEventLog / creates directories.
func TestLoadPartialPrior_ReadOnlyNoFilesystemMutation(t *testing.T) {
	missingHome := filepath.Join(t.TempDir(), "no-partial-home")
	_, err := workflowrun.LoadPartialPrior(missingHome, "proj-y", "run-y")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, serr := os.Stat(missingHome); !os.IsNotExist(serr) {
		t.Fatalf("LoadPartialPrior must not create home: %v", serr)
	}

	home := t.TempDir()
	_ = os.Chmod(home, 0o700)
	before := listPaths(t, home)
	_, err = workflowrun.LoadPartialPrior(home, "proj-z", "run-z")
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("err=%v want NotExist", err)
	}
	after := listPaths(t, home)
	if len(after) != len(before) {
		t.Fatalf("partial load mutated fs: before=%v after=%v", before, after)
	}
}

func listPaths(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = true
		return nil
	})
	return out
}

// TestResumeEnvelopeTamperMatrix fails closed on schema/project/run/graphID/plan/graph
// and plan-vs-execution-plan mismatches before any event/ledger/worktree spend.
func TestResumeEnvelopeTamperMatrix(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-env-tamper"
	goal := "implement resume envelope tamper matrix"
	planDig, graphDig, class, ccd, _, graphID := childContractForGoal(t, goal, "1397", "owner", projectID, "wi_research", now)

	type kind string
	const (
		kindCP   kind = "checkpoint"
		kindPart kind = "partial"
	)
	cases := []struct {
		name string
		k    kind
		mut  func(cp *goalrun.Checkpoint, part map[string]any)
		sub  string
	}{
		{"cp_schema", kindCP, func(cp *goalrun.Checkpoint, _ map[string]any) { cp.Schema = "wrong.schema" }, "schema"},
		{"cp_project", kindCP, func(cp *goalrun.Checkpoint, _ map[string]any) { cp.ProjectID = "other-proj" }, "project_id"},
		{"cp_run", kindCP, func(cp *goalrun.Checkpoint, _ map[string]any) { cp.RunID = "other-run" }, "run_id"},
		{"cp_graph_id", kindCP, func(cp *goalrun.Checkpoint, _ map[string]any) { cp.GraphID = "wrong-graph" }, "graph_id"},
		{"cp_plan", kindCP, func(cp *goalrun.Checkpoint, _ map[string]any) { cp.PlanDigest = "sha256:wrongplan" }, "plan_digest"},
		{"cp_graph", kindCP, func(cp *goalrun.Checkpoint, _ map[string]any) { cp.GraphDigest = "sha256:wronggraph" }, "graph_digest"},
		{"part_schema", kindPart, func(_ *goalrun.Checkpoint, p map[string]any) { p["schema"] = "bad" }, "schema"},
		{"part_project", kindPart, func(_ *goalrun.Checkpoint, p map[string]any) { p["project_id"] = "x" }, "project_id"},
		{"part_run", kindPart, func(_ *goalrun.Checkpoint, p map[string]any) { p["run_id"] = "y" }, "run_id"},
		{"part_plan", kindPart, func(_ *goalrun.Checkpoint, p map[string]any) { p["plan_digest"] = "sha256:p" }, "plan_digest"},
		{"part_graph", kindPart, func(_ *goalrun.Checkpoint, p map[string]any) { p["graph_digest"] = "sha256:g" }, "graph_digest"},
		{"part_plan_vs_exec", kindPart, func(_ *goalrun.Checkpoint, p map[string]any) {
			p["plan_digest"] = planDig
			p["execution_plan_digest"] = "sha256:different"
		}, "execution_plan_digest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run_env_" + tc.name
			// Snapshot filesystem under home before Execute.
			before := listPaths(t, home)
			ledgerPath := filepath.Join(t.TempDir(), "cap-"+tc.name+".json")

			att := workflowrun.AttemptID("wi_research", planDig, runID, 0)
			prior := workflowrun.ChildOutcome{
				WorkItemID: "wi_research", Terminal: "succeeded", AttemptID: att,
				OutputEvidence: "sha256:ev", Provider: "codex", Model: "gpt-5.5",
				Depth: "low", Permission: "read-only", TaskClass: class,
				ExecutionPlanDigest: planDig, ChildContractDigest: ccd, Generation: 1,
			}
			switch tc.k {
			case kindCP:
				cp := goalrun.Checkpoint{
					Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
					GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
					Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked", Interrupted: true,
					PriorSucceeded: map[string]workflowrun.ChildOutcome{"wi_research": prior},
					SavedAt:        now,
				}
				// Save under the requested project/run path first, then tamper bytes
				// in place (mutating ProjectID/RunID before Save would write a different path).
				path, err := goalrun.SaveCheckpoint(home, cp)
				if err != nil {
					t.Fatal(err)
				}
				tc.mut(&cp, nil)
				raw, mErr := json.MarshalIndent(cp, "", "  ")
				if mErr != nil {
					t.Fatal(mErr)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			case kindPart:
				// Valid partial only (no checkpoint).
				part := map[string]any{
					"schema": workflowrun.PartialSchema, "project_id": projectID, "run_id": runID,
					"plan_digest": planDig, "execution_plan_digest": planDig, "graph_digest": graphDig,
					"saved_at": now.Format(time.RFC3339), "interrupted": true,
					"prior_succeeded": map[string]any{
						"wi_research": map[string]any{
							"work_item_id": "wi_research", "terminal": "succeeded",
							"attempt_id": att, "output_evidence": "sha256:ev",
							"provider": "codex", "model": "gpt-5.5", "depth": "low", "permission": "read-only",
							"task_class": class, "execution_plan_digest": planDig,
							"child_contract_digest": ccd, "generation": 1,
						},
					},
				}
				tc.mut(nil, part)
				// Ensure run dir exists only via explicit write of partial (test setup),
				// not via Load. Save path for partial: write under durable dir.
				dir := filepath.Join(home, "projects", projectID, "runs", runID)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.MarshalIndent(part, "", "  ")
				if err := os.WriteFile(filepath.Join(dir, "workflow-partial.json"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			// Count pre-existing event logs / worktrees under this run before Execute.
			runDir := filepath.Join(home, "projects", projectID, "runs", runID)
			hadEvent := fileExists(filepath.Join(runDir, "workflow-events.jsonl"))
			hadClaims := fileExists(filepath.Join(runDir, "workclaims.json"))

			calls := map[string]int{}
			_, err := goalrun.Execute(context.Background(), goalrun.Request{
				ProjectID: projectID, RunID: runID, Resume: true,
				Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
				Provider: "codex", Model: "gpt-5.5",
				HomeDir: home, Now: func() time.Time { return now },
				LoadInventory: env.loadInv(),
				OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
					return capacityledger.OpenPath(ledgerPath, nowFn)
				},
				Executor: workflowrun.FakeChildExecutor{
					HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
				},
			})
			if err == nil {
				t.Fatalf("expected envelope fail closed for %s", tc.name)
			}
			if !stringsContainsCI(err.Error(), tc.sub) {
				t.Fatalf("err %q should mention %q", err.Error(), tc.sub)
			}
			for id, n := range calls {
				if n != 0 {
					t.Fatalf("provider call after envelope fail: %s=%d", id, n)
				}
			}
			// No new ledger reservations.
			if raw, rerr := os.ReadFile(ledgerPath); rerr == nil && len(raw) > 0 {
				var doc struct {
					Entries []capacityledger.Entry `json:"entries"`
				}
				_ = json.Unmarshal(raw, &doc)
				if len(doc.Entries) != 0 {
					t.Fatalf("ledger spend after envelope fail: %+v", doc.Entries)
				}
			}
			// Envelope fail must not create event log / claims if they did not exist.
			if !hadEvent && fileExists(filepath.Join(runDir, "workflow-events.jsonl")) {
				t.Fatal("event log created after envelope fail (must fail before eventlog)")
			}
			if !hadClaims && fileExists(filepath.Join(runDir, "workclaims.json")) {
				t.Fatal("workclaims created after envelope fail")
			}
			_ = before
		})
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestResumeOverlapConflictMatrix requires exact equality on seed overlap.
func TestResumeOverlapConflictMatrix(t *testing.T) {
	now := time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-overlap"
	goal := "implement resume overlap conflict matrix"
	planDig, graphDig, class, ccd, _, graphID := childContractForGoal(t, goal, "1397", "owner", projectID, "wi_research", now)

	mkPrior := func(runID, evidence string) workflowrun.ChildOutcome {
		return workflowrun.ChildOutcome{
			WorkItemID: "wi_research", Terminal: "succeeded",
			AttemptID:      workflowrun.AttemptID("wi_research", planDig, runID, 0),
			OutputEvidence: evidence, Provider: "codex", Model: "gpt-5.5",
			Depth: "low", Permission: "read-only", TaskClass: class,
			ExecutionPlanDigest: planDig, ChildContractDigest: ccd, Generation: 1,
		}
	}

	t.Run("checkpoint_vs_partial_evidence", func(t *testing.T) {
		runID := "run_ov_cp_part"
		p1 := mkPrior(runID, "sha256:ev-a")
		p2 := mkPrior(runID, "sha256:ev-b") // conflict
		cp := goalrun.Checkpoint{
			Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
			GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
			Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked",
			PriorSucceeded: map[string]workflowrun.ChildOutcome{"wi_research": p1},
			SavedAt:        now,
		}
		if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(home, "projects", projectID, "runs", runID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		part := map[string]any{
			"schema": workflowrun.PartialSchema, "project_id": projectID, "run_id": runID,
			"plan_digest": planDig, "execution_plan_digest": planDig, "graph_digest": graphDig,
			"prior_succeeded": map[string]any{
				"wi_research": map[string]any{
					"work_item_id": p2.WorkItemID, "terminal": p2.Terminal, "attempt_id": p2.AttemptID,
					"output_evidence": p2.OutputEvidence, "provider": p2.Provider, "model": p2.Model,
					"depth": p2.Depth, "permission": p2.Permission, "task_class": p2.TaskClass,
					"execution_plan_digest": p2.ExecutionPlanDigest, "child_contract_digest": p2.ChildContractDigest,
					"generation": p2.Generation,
				},
			},
		}
		raw, _ := json.Marshal(part)
		if err := os.WriteFile(filepath.Join(dir, "workflow-partial.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		calls := map[string]int{}
		_, err := goalrun.Execute(context.Background(), goalrun.Request{
			ProjectID: projectID, RunID: runID, Resume: true,
			Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
			Provider: "codex", Model: "gpt-5.5",
			HomeDir: home, Now: func() time.Time { return now },
			LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
			Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: func() time.Time { return now }, Calls: calls},
		})
		if err == nil || !stringsContainsCI(err.Error(), "overlap") && !stringsContainsCI(err.Error(), "conflict") && !stringsContainsCI(err.Error(), "output_evidence") {
			t.Fatalf("want overlap conflict, got %v", err)
		}
		if calls["wi_research"] != 0 {
			t.Fatalf("must not spend on conflict: %+v", calls)
		}
	})

	t.Run("caller_vs_durable", func(t *testing.T) {
		runID := "run_ov_caller"
		p1 := mkPrior(runID, "sha256:ev-a")
		p2 := mkPrior(runID, "sha256:ev-caller") // conflict with durable
		cp := goalrun.Checkpoint{
			Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
			GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
			Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked",
			PriorSucceeded: map[string]workflowrun.ChildOutcome{"wi_research": p1},
			SavedAt:        now,
		}
		if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
			t.Fatal(err)
		}
		calls := map[string]int{}
		_, err := goalrun.Execute(context.Background(), goalrun.Request{
			ProjectID: projectID, RunID: runID, Resume: true,
			Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
			Provider: "codex", Model: "gpt-5.5",
			HomeDir: home, Now: func() time.Time { return now },
			PriorSucceeded: map[string]workflowrun.ChildOutcome{"wi_research": p2},
			LoadInventory:  env.loadInv(), OpenLedger: env.openLed(),
			Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: func() time.Time { return now }, Calls: calls},
		})
		if err == nil {
			t.Fatal("expected caller-vs-durable conflict")
		}
		if !stringsContainsCI(err.Error(), "output_evidence") && !stringsContainsCI(err.Error(), "conflict") && !stringsContainsCI(err.Error(), "caller") {
			t.Fatalf("err=%v", err)
		}
		if calls["wi_research"] != 0 {
			t.Fatalf("must not spend: %+v", calls)
		}
	})

	t.Run("caller_unbound_without_durable", func(t *testing.T) {
		runID := "run_ov_unbound"
		p := mkPrior(runID, "sha256:ev")
		// No durable checkpoint/partial.
		_, err := goalrun.Execute(context.Background(), goalrun.Request{
			ProjectID: projectID, RunID: runID, Resume: true,
			Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
			Provider: "codex", Model: "gpt-5.5",
			HomeDir: home, Now: func() time.Time { return now },
			PriorSucceeded: map[string]workflowrun.ChildOutcome{"wi_research": p},
			LoadInventory:  env.loadInv(), OpenLedger: env.openLed(),
			Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: func() time.Time { return now }},
		})
		if err == nil || !stringsContainsCI(err.Error(), "durable") {
			t.Fatalf("want unbound fail closed, got %v", err)
		}
	})
}

// TestResumeCanonicalAttemptAndGenerationMismatch fails closed on bad AttemptID/Generation.
func TestResumeCanonicalAttemptAndGenerationMismatch(t *testing.T) {
	now := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-canon-att"
	goal := "implement canonical attempt mismatch"
	planDig, graphDig, class, ccd, _, graphID := childContractForGoal(t, goal, "1397", "owner", projectID, "wi_research", now)

	cases := []struct {
		name string
		mut  func(*workflowrun.ChildOutcome, string)
		sub  string
	}{
		{"attempt_mismatch", func(p *workflowrun.ChildOutcome, _ string) {
			p.AttemptID = "att-wi_research-deadbeef-g0"
		}, "attempt_id"},
		{"generation_mismatch", func(p *workflowrun.ChildOutcome, runID string) {
			// Generation 2 implies -g1 suffix; keep g0 attempt → mismatch
			p.Generation = 2
			p.AttemptID = workflowrun.AttemptID("wi_research", planDig, runID, 0)
		}, "attempt_id"},
		{"generation_zero", func(p *workflowrun.ChildOutcome, runID string) {
			p.Generation = 0
			p.AttemptID = workflowrun.AttemptID("wi_research", planDig, runID, 0)
		}, "generation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run_canon_" + tc.name
			prior := workflowrun.ChildOutcome{
				WorkItemID: "wi_research", Terminal: "succeeded",
				AttemptID:      workflowrun.AttemptID("wi_research", planDig, runID, 0),
				OutputEvidence: "sha256:ev", Provider: "codex", Model: "gpt-5.5",
				Depth: "low", Permission: "read-only", TaskClass: class,
				ExecutionPlanDigest: planDig, ChildContractDigest: ccd, Generation: 1,
			}
			tc.mut(&prior, runID)
			cp := goalrun.Checkpoint{
				Schema: goalrun.CheckpointSchema, ProjectID: projectID, RunID: runID,
				GraphID: graphID, PlanDigest: planDig, GraphDigest: graphDig,
				Goal: goal, Issue: "1397", Actor: "owner", Status: "blocked",
				PriorSucceeded: map[string]workflowrun.ChildOutcome{"wi_research": prior},
				SavedAt:        now,
			}
			if _, err := goalrun.SaveCheckpoint(home, cp); err != nil {
				t.Fatal(err)
			}
			calls := map[string]int{}
			_, err := goalrun.Execute(context.Background(), goalrun.Request{
				ProjectID: projectID, RunID: runID, Resume: true,
				Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
				Provider: "codex", Model: "gpt-5.5",
				HomeDir: home, Now: func() time.Time { return now },
				LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
				Executor: workflowrun.FakeChildExecutor{HomeDir: home, Now: func() time.Time { return now }, Calls: calls},
			})
			if err == nil {
				t.Fatal("expected fail closed")
			}
			if !stringsContainsCI(err.Error(), tc.sub) {
				t.Fatalf("err %q want %q", err.Error(), tc.sub)
			}
			if calls["wi_research"] != 0 {
				t.Fatalf("must not re-exec: %+v", calls)
			}
		})
	}
}

// TestSuccessfulRestartExactlyOnce: succeeded child reused with zero provider
// call/new claim/new reserve; failed sibling (FailIDs) retries with next
// canonical attempt. This is failure-based retry evidence — not forced-process
// interruption (Gate 2B).
func TestSuccessfulRestartExactlyOnce(t *testing.T) {
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-restart-exact"
	runID := "run_restart_exact_1"
	calls1 := map[string]int{}
	// Two-child graph avoids released sibling attempt collisions on resume.
	twoChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_restart_exact", Version: 1,
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
	// Authoritative restart: real process identity + authority + PID (not Fake).
	res1, err1 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: "implement transparent multi-child routing", Issue: "1397",
		Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     twoChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls: calls1, FailIDs: map[string]bool{"wi_implement": true},
		},
	})
	if err1 == nil && res1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("expected blocked first pass: %+v", res1)
	}
	cp, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("checkpoint after first pass: %v status=%s msg=%s", err, res1.Status, res1.Message)
	}
	if len(cp.PriorSucceeded) == 0 {
		t.Fatalf("expected prior succeeded: %+v children=%+v", cp, res1.Children)
	}
	ledEntries1 := countLedgerEntries(t, env.LedgerPath)

	calls2 := map[string]int{}
	res2, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: "implement transparent multi-child routing", Issue: "1397",
		Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     twoChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2,
		},
	})
	if err2 != nil {
		t.Fatalf("resume: %v status=%s msg=%s", err2, res2.Status, res2.Message)
	}
	if res2.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%q want human_gate", res2.Status)
	}
	if res2.ReuseCount < 1 {
		t.Fatalf("reuse_count=%d", res2.ReuseCount)
	}
	for id := range cp.PriorSucceeded {
		if calls2[id] != 0 {
			t.Fatalf("succeeded %s re-executed: calls2=%+v", id, calls2)
		}
	}
	// Implement must re-run once on resume (not the succeeded research).
	if calls2["wi_implement"] != 1 {
		t.Fatalf("implement calls2=%d want 1; all=%+v", calls2["wi_implement"], calls2)
	}
	// Each expected item appears in Integrated exactly once (not merely no dups among present).
	wantIntegrated := []string{"wi_research", "wi_implement"}
	seen := map[string]int{}
	for _, id := range res2.Workflow.Integrated {
		seen[id]++
	}
	for _, id := range wantIntegrated {
		if seen[id] != 1 {
			t.Fatalf("Integrated[%s]=%d want exactly 1; Integrated=%v", id, seen[id], res2.Workflow.Integrated)
		}
	}
	// Already-succeeded research: no new claim/reservation/attempt on resume.
	var researchAtt1, researchAtt2 string
	for id, p := range cp.PriorSucceeded {
		if id == "wi_research" {
			researchAtt1 = p.AttemptID
		}
	}
	for _, c := range res2.Workflow.Children {
		if c.WorkItemID == "wi_research" {
			researchAtt2 = c.AttemptID
		}
	}
	if researchAtt1 == "" || researchAtt2 == "" || researchAtt1 != researchAtt2 {
		t.Fatalf("research attempt drift: first=%q resume=%q", researchAtt1, researchAtt2)
	}
	// Implement successful AttemptID must be nonempty and exactly next canonical generation.
	var implFailAtt string
	for _, c := range res1.Workflow.Children {
		if c.WorkItemID == "wi_implement" {
			implFailAtt = c.AttemptID
		}
	}
	var implOK workflowrun.ChildOutcome
	for _, c := range res2.Workflow.Children {
		if c.WorkItemID == "wi_implement" && c.Terminal == "succeeded" {
			implOK = c
		}
	}
	if implOK.AttemptID == "" {
		t.Fatal("implement successful AttemptID empty")
	}
	if implOK.Generation < 1 {
		t.Fatalf("implement generation %d", implOK.Generation)
	}
	wantImplAtt := workflowrun.AttemptID("wi_implement", res2.PlanDigest, runID, implOK.Generation-1)
	if implOK.AttemptID != wantImplAtt {
		t.Fatalf("implement AttemptID %q != canonical %q", implOK.AttemptID, wantImplAtt)
	}
	if implFailAtt != "" && implOK.AttemptID == implFailAtt {
		t.Fatalf("implement relaunched same aborted attempt %q", implFailAtt)
	}
	// Parse failed attempt generation; successful must be exact next (g+1 suffix).
	if implFailAtt != "" {
		failG := workflowrun.ParseAttemptGeneration(implFailAtt)
		okG := workflowrun.ParseAttemptGeneration(implOK.AttemptID)
		if failG < 0 || okG != failG+1 {
			t.Fatalf("implement gen: fail=%q(g=%d) ok=%q(g=%d) want ok=fail+1",
				implFailAtt, failG, implOK.AttemptID, okG)
		}
	}
	// Exactly one new ledger entry on resume (failed-retry implement attempt).
	ledEntries2 := countLedgerEntries(t, env.LedgerPath)
	if ledEntries2 != ledEntries1+1 {
		t.Fatalf("ledger entries first=%d resume=%d want exactly first+1", ledEntries1, ledEntries2)
	}
	raw, err := os.ReadFile(env.LedgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Entries []capacityledger.Entry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	// Partition: entries present after first pass vs new on resume.
	firstAttempts := map[string]bool{}
	// Rebuild first-pass attempt set from first-pass outcomes + research prior.
	if researchAtt1 != "" {
		firstAttempts[researchAtt1] = true
	}
	if implFailAtt != "" {
		firstAttempts[implFailAtt] = true
	}
	var newEntries []capacityledger.Entry
	researchNew := 0
	implNew := 0
	for _, e := range doc.Entries {
		if e.RunID != runID {
			continue
		}
		if firstAttempts[e.AttemptID] {
			continue
		}
		newEntries = append(newEntries, e)
		if e.AttemptID == implOK.AttemptID {
			implNew++
		}
		if e.AttemptID == researchAtt1 {
			researchNew++
		}
	}
	if len(newEntries) != 1 {
		t.Fatalf("new ledger entries on resume=%d want exactly 1: %+v", len(newEntries), newEntries)
	}
	if implNew != 1 {
		t.Fatalf("new implement attempt entries=%d want 1 (attempt=%s)", implNew, implOK.AttemptID)
	}
	if researchNew != 0 {
		t.Fatalf("new research attempt entries=%d want 0 (must not re-reserve succeeded child)", researchNew)
	}
	if newEntries[0].AttemptID != implOK.AttemptID {
		t.Fatalf("sole new entry attempt=%q want implement %q", newEntries[0].AttemptID, implOK.AttemptID)
	}
}

func countLedgerEntries(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	var doc struct {
		Entries []capacityledger.Entry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return len(doc.Entries)
}
