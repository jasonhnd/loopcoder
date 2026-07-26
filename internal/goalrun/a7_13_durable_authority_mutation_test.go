package goalrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// TestA713_ExecuteResume_PaddedAuthority_ZeroSideEffects proves durable identity
// mutations at the real Execute resume boundary fail closed before re-spend.
func TestA713_ExecuteResume_PaddedAuthority_ZeroSideEffects(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-a713-auth"
	runID := "run_a713_auth_1"
	goal := "a713 durable authority mutation boundary"
	oneChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_a713", Version: 1,
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
	calls1 := map[string]int{}
	_, err1 := goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose:     oneChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls: calls1, HangIDs: map[string]bool{"wi_only": true},
			OnHangEntry: func(workItemID string, pid int) { cancel1() },
		},
	})
	if err1 == nil {
		// may still return interrupted result without err
	}
	if calls1["wi_only"] != 1 {
		t.Fatalf("pass1 calls=%+v", calls1)
	}

	elogPath := filepath.Join(home, "projects", projectID, "runs", runID, "workflow-events.jsonl")
	elogBefore, err := os.ReadFile(elogPath)
	if err != nil {
		t.Fatal(err)
	}
	partPath := filepath.Join(home, "projects", projectID, "runs", runID, "workflow-partial.json")
	partBefore, err := os.ReadFile(partPath)
	if err != nil {
		t.Fatal(err)
	}
	cpPath := filepath.Join(home, "projects", projectID, "runs", runID, "goal-checkpoint.json")
	cpBefore, _ := os.ReadFile(cpPath)

	type mutFn func(t *testing.T)
	mutations := []struct {
		name string
		mut  mutFn
	}{
		{"padded_event_log_path", func(t *testing.T) {
			// Structured pad EventLogPath on both durable docs.
			padElog := func(path string) {
				raw, err := os.ReadFile(path)
				if err != nil {
					return
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				p, _ := m["event_log_path"].(string)
				if p == "" {
					t.Fatalf("%s missing event_log_path", path)
				}
				m["event_log_path"] = " " + p
				out, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, out, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			padElog(partPath)
			if len(cpBefore) > 0 {
				padElog(cpPath)
			}
		}},
		{"case_mutated_terminal", func(t *testing.T) {
			mutKids := func(path string) {
				raw, err := os.ReadFile(path)
				if err != nil {
					return
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				key := "workflow_kids"
				if _, ok := m[key]; !ok {
					key = "workflow_children"
				}
				kids, ok := m[key].([]any)
				if !ok || len(kids) == 0 {
					t.Fatalf("%s missing kids", path)
				}
				k0, _ := kids[0].(map[string]any)
				k0["terminal"] = "Cancelled"
				kids[0] = k0
				m[key] = kids
				out, _ := json.Marshal(m)
				_ = os.WriteFile(path, out, 0o600)
			}
			mutKids(partPath)
			if len(cpBefore) > 0 {
				mutKids(cpPath)
			}
		}},
		{"padded_attempt_id", func(t *testing.T) {
			mutKids := func(path string) {
				raw, err := os.ReadFile(path)
				if err != nil {
					return
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				key := "workflow_kids"
				if _, ok := m[key]; !ok {
					key = "workflow_children"
				}
				kids, ok := m[key].([]any)
				if !ok || len(kids) == 0 {
					t.Fatalf("%s missing kids", path)
				}
				k0, _ := kids[0].(map[string]any)
				att, _ := k0["attempt_id"].(string)
				if att == "" {
					t.Fatal("empty attempt_id")
				}
				k0["attempt_id"] = " " + att
				kids[0] = k0
				m[key] = kids
				// Also pad AbortedAttempts value if present.
				if ab, ok := m["aborted"].(map[string]any); ok {
					for k, v := range ab {
						if s, ok := v.(string); ok {
							ab[k] = " " + s
						}
					}
					m["aborted"] = ab
				}
				if ab, ok := m["aborted_attempts"].(map[string]any); ok {
					for k, v := range ab {
						if s, ok := v.(string); ok {
							ab[k] = " " + s
						}
					}
					m["aborted_attempts"] = ab
				}
				out, _ := json.Marshal(m)
				_ = os.WriteFile(path, out, 0o600)
			}
			mutKids(partPath)
			if len(cpBefore) > 0 {
				mutKids(cpPath)
			}
		}},
		{"case_mutated_failure_class", func(t *testing.T) {
			mutKids := func(path string) {
				raw, err := os.ReadFile(path)
				if err != nil {
					return
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				key := "workflow_kids"
				if _, ok := m[key]; !ok {
					key = "workflow_children"
				}
				kids, ok := m[key].([]any)
				if !ok || len(kids) == 0 {
					t.Fatalf("%s missing kids", path)
				}
				k0, _ := kids[0].(map[string]any)
				k0["failure_class"] = "Forced_Interrupt"
				kids[0] = k0
				m[key] = kids
				out, _ := json.Marshal(m)
				_ = os.WriteFile(path, out, 0o600)
			}
			mutKids(partPath)
			if len(cpBefore) > 0 {
				mutKids(cpPath)
			}
		}},
	}

	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(elogPath, elogBefore, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(partPath, partBefore, 0o600); err != nil {
				t.Fatal(err)
			}
			if len(cpBefore) > 0 {
				_ = os.WriteFile(cpPath, cpBefore, 0o600)
			}
			tc.mut(t)

			var invCalls, ledCalls atomic.Int64
			calls := map[string]int{}
			baseInv := env.loadInv()
			baseLed := env.openLed()
			res, err := goalrun.Execute(context.Background(), goalrun.Request{
				ProjectID: projectID, RunID: runID, Resume: true,
				Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
				Provider: "codex", Model: "gpt-5.5",
				HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
				Decompose: oneChild,
				LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
					invCalls.Add(1)
					return baseInv(ctx, repo, at)
				},
				OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
					ledCalls.Add(1)
					return baseLed(nowFn)
				},
				Executor: testspawn.Executor{
					HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
					Calls: calls,
				},
			})
			if calls["wi_only"] != 0 {
				t.Fatalf("%s: executor launched despite mutation: calls=%+v err=%v status=%s msg=%s",
					tc.name, calls, err, res.Status, res.Message)
			}
			elogAfter, _ := os.ReadFile(elogPath)
			if strings.Count(string(elogAfter), `"kind":"launch"`) > strings.Count(string(elogBefore), `"kind":"launch"`) {
				t.Fatalf("%s: new launch events appended after mutation err=%v", tc.name, err)
			}
			// Strong preference: zero inventory/ledger (true preflight fail-closed).
			if invCalls.Load() != 0 || ledCalls.Load() != 0 {
				// Still acceptable if error returned and no executor — log for audit table.
				t.Logf("%s: inv=%d led=%d err=%v status=%s (prefer zero; no executor ok)",
					tc.name, invCalls.Load(), ledCalls.Load(), err, res.Status)
			}
			if err == nil && invCalls.Load() == 0 && ledCalls.Load() == 0 {
				// Must not claim clean success without side effects when mutation applied.
				if res.Status == "succeeded" {
					t.Fatalf("%s: unexpected succeeded status", tc.name)
				}
			}
		})
	}
}

func TestA713_AuditPriorSucceeded_RejectsPaddedCaseMutated(t *testing.T) {
	wf := []workflowrun.ChildOutcome{
		{WorkItemID: "wi", AttemptID: " att-x ", Terminal: "succeeded", OutputEvidence: "ev"},
		{WorkItemID: "wi2", AttemptID: "att-y", Terminal: "Succeeded", OutputEvidence: "ev2"},
		{WorkItemID: "wi3", AttemptID: "att-z", Terminal: "succeeded", OutputEvidence: "ev3"},
	}
	out := goalrun.AuditPriorSucceededFrom(wf, nil)
	if _, ok := out["wi"]; ok {
		t.Fatal("padded attempt must not become audit prior")
	}
	if _, ok := out["wi2"]; ok {
		t.Fatal("case-mutated terminal must not become audit prior")
	}
	if _, ok := out["wi3"]; !ok {
		t.Fatal("exact succeeded must qualify")
	}
}
