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

// sideEffectCounters tracks preflight spend surfaces for A7-14 strict zero contract.
type sideEffectCounters struct {
	inv, led atomic.Int64
}

// buildInterruptedBaseline runs one forced-interrupt pass and returns paths + snapshots.
// Each mutation subtest must restore from these bytes (fresh baseline isolation).
func buildInterruptedBaseline(t *testing.T, suffix string) (
	env productEnv, home, projectID, runID, goal string,
	oneChild func(workgraph.DecomposeOptions) (workgraph.Graph, error),
	elogPath, partPath, cpPath string,
	elogBefore, partBefore, cpBefore []byte,
	now time.Time,
) {
	t.Helper()
	now = time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	env = newProductEnv(t, now, "codex")
	home = env.Home
	projectID = "proj-a714-" + suffix
	runID = "run_a714_" + suffix
	goal = "a714 boundary " + suffix
	oneChild = func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_a714_" + suffix, Version: 1,
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
	ctx, cancel := context.WithCancel(context.Background())
	calls := map[string]int{}
	_, _ = goalrun.Execute(ctx, goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		HomeDir: home, Now: func() time.Time { return now },
		Decompose: oneChild, LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls: calls, HangIDs: map[string]bool{"wi_only": true},
			OnHangEntry: func(string, int) { cancel() },
		},
	})
	if calls["wi_only"] != 1 {
		t.Fatalf("baseline calls=%+v", calls)
	}
	elogPath = filepath.Join(home, "projects", projectID, "runs", runID, "workflow-events.jsonl")
	partPath = filepath.Join(home, "projects", projectID, "runs", runID, "workflow-partial.json")
	cpPath = filepath.Join(home, "projects", projectID, "runs", runID, "goal-checkpoint.json")
	var err error
	elogBefore, err = os.ReadFile(elogPath)
	if err != nil {
		t.Fatal(err)
	}
	partBefore, err = os.ReadFile(partPath)
	if err != nil {
		t.Fatal(err)
	}
	cpBefore, _ = os.ReadFile(cpPath)
	return
}

func restoreBaseline(t *testing.T, elogPath, partPath, cpPath string, elogBefore, partBefore, cpBefore []byte) {
	t.Helper()
	if err := os.WriteFile(elogPath, elogBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, partBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(cpBefore) > 0 {
		_ = os.WriteFile(cpPath, cpBefore, 0o600)
	}
}

func mutateJSONMap(t *testing.T, path string, mut func(m map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(m)
	mut(m)
	after, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("mutation did not change JSON")
	}
	if err := os.WriteFile(path, after, 0o600); err != nil {
		t.Fatal(err)
	}
}

func kidsKey(m map[string]any) string {
	if _, ok := m["workflow_kids"]; ok {
		return "workflow_kids"
	}
	return "workflow_children"
}

func firstKid(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	k := kidsKey(m)
	kids, ok := m[k].([]any)
	if !ok || len(kids) == 0 {
		t.Fatalf("missing kids under %s", k)
	}
	kid, ok := kids[0].(map[string]any)
	if !ok {
		t.Fatal("kid not object")
	}
	return kid
}

func setFirstKid(m map[string]any, kid map[string]any) {
	k := kidsKey(m)
	kids := m[k].([]any)
	kids[0] = kid
	m[k] = kids
}

// TestA714_ExecuteResume_AuthorityMutations_StrictZeroSideEffects requires
// zero inventory, zero ledger open, zero executor, zero event-log growth for
// every durable authority mutation — no log-and-pass.
func TestA714_ExecuteResume_AuthorityMutations_StrictZeroSideEffects(t *testing.T) {
	type mutSpec struct {
		name string
		mut  func(t *testing.T, partPath, cpPath string)
	}
	mutations := []mutSpec{
		{"padded_event_log_path", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					ep, _ := m["event_log_path"].(string)
					if ep == "" {
						t.Fatal("missing event_log_path")
					}
					m["event_log_path"] = " " + ep
				})
			}
		}},
		{"case_mutated_terminal", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					kid := firstKid(t, m)
					kid["terminal"] = "Cancelled"
					setFirstKid(m, kid)
				})
			}
		}},
		{"padded_attempt_id", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					kid := firstKid(t, m)
					att, _ := kid["attempt_id"].(string)
					if att == "" {
						t.Fatal("empty attempt")
					}
					kid["attempt_id"] = " " + att
					setFirstKid(m, kid)
					for _, abKey := range []string{"aborted", "aborted_attempts"} {
						if ab, ok := m[abKey].(map[string]any); ok {
							for k, v := range ab {
								if s, ok := v.(string); ok {
									ab[k] = " " + s
								}
							}
							m[abKey] = ab
						}
					}
				})
			}
		}},
		{"case_mutated_failure_class", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					kid := firstKid(t, m)
					kid["failure_class"] = "Forced_Interrupt"
					setFirstKid(m, kid)
				})
			}
		}},
		{"padded_work_item", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					kid := firstKid(t, m)
					wi, _ := kid["work_item_id"].(string)
					if wi == "" {
						t.Fatal("empty work_item")
					}
					kid["work_item_id"] = " " + wi
					setFirstKid(m, kid)
				})
			}
		}},
		{"mutated_plan_digest", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					kid := firstKid(t, m)
					kid["execution_plan_digest"] = "sha256:" + strings.Repeat("ff", 32)
					setFirstKid(m, kid)
					if _, ok := m["plan_digest"]; ok {
						m["plan_digest"] = "sha256:" + strings.Repeat("ff", 32)
					}
					if _, ok := m["execution_plan_digest"]; ok {
						m["execution_plan_digest"] = "sha256:" + strings.Repeat("ff", 32)
					}
				})
			}
		}},
		{"mutated_graph_digest", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					if _, ok := m["graph_digest"]; ok {
						m["graph_digest"] = "sha256:" + strings.Repeat("ee", 32)
					}
					kid := firstKid(t, m)
					// kid may not carry graph_digest; stamp envelope is enough
					_ = kid
					setFirstKid(m, kid)
				})
			}
		}},
		{"mutated_ccd", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					kid := firstKid(t, m)
					kid["child_contract_digest"] = "sha256:" + strings.Repeat("dd", 32)
					setFirstKid(m, kid)
				})
			}
		}},
		{"mutated_generation", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					kid := firstKid(t, m)
					kid["generation"] = float64(99)
					setFirstKid(m, kid)
				})
			}
		}},
		{"mutated_actual_sources_model_partial_only", func(t *testing.T, partPath, cpPath string) {
			// Dual-source conflict: mutate partial only so cp vs partial DeepEqual fails
			// before spend (full-row equality includes ActualSources).
			mutateJSONMap(t, partPath, func(m map[string]any) {
				kid := firstKid(t, m)
				as, _ := kid["actual_sources"].(map[string]any)
				if as == nil {
					as = map[string]any{}
				}
				as["model"] = "provider_usage_MUTATED"
				as["effort"] = "provider_usage_MUTATED"
				as["permission"] = "provider_usage_MUTATED"
				as["account"] = "auth_binding_MUTATED"
				as["install"] = "install_binding_MUTATED"
				kid["actual_sources"] = as
				kid["actual_source"] = "accepted_invocation_MUTATED"
				setFirstKid(m, kid)
			})
		}},
		{"mutated_capacity_transition_ids", func(t *testing.T, partPath, cpPath string) {
			// If capacity transitions present, pad attempt IDs; else force dual-source
			// reservation_id conflict.
			mutateJSONMap(t, partPath, func(m map[string]any) {
				if tr, ok := m["capacity_transitions"].([]any); ok && len(tr) > 0 {
					t0, _ := tr[0].(map[string]any)
					if t0 != nil {
						if a, ok := t0["attempt_id"].(string); ok && a != "" {
							t0["attempt_id"] = " " + a
						} else {
							t0["reservation_id"] = "sres_MUTATED"
						}
						tr[0] = t0
						m["capacity_transitions"] = tr
						return
					}
				}
				kid := firstKid(t, m)
				kid["reservation_id"] = "sres_MUTATED_PARTIAL"
				setFirstKid(m, kid)
			})
		}},
		{"mutated_project_id_stamp", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					if _, ok := m["project_id"]; !ok {
						t.Fatal("missing project_id")
					}
					m["project_id"] = "proj-MUTATED"
				})
			}
		}},
		{"mutated_run_id_stamp", func(t *testing.T, partPath, cpPath string) {
			for _, p := range []string{partPath, cpPath} {
				if _, err := os.Stat(p); err != nil {
					continue
				}
				mutateJSONMap(t, p, func(m map[string]any) {
					if _, ok := m["run_id"]; !ok {
						t.Fatal("missing run_id")
					}
					m["run_id"] = "run_MUTATED"
				})
			}
		}},
	}

	for _, tc := range mutations {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Fresh baseline per subtest — no contamination.
			env, home, projectID, runID, goal, oneChild, elogPath, partPath, cpPath, elogBefore, partBefore, cpBefore, now :=
				buildInterruptedBaseline(t, tc.name)
			restoreBaseline(t, elogPath, partPath, cpPath, elogBefore, partBefore, cpBefore)
			tc.mut(t, partPath, cpPath)

			// Post-mutation pre-Execute immutable snapshots (A7-15: not pristine baseline).
			elogPostMut, err := os.ReadFile(elogPath)
			if err != nil {
				t.Fatal(err)
			}
			partPostMut, err := os.ReadFile(partPath)
			if err != nil {
				t.Fatal(err)
			}
			var cpPostMut []byte
			if _, err := os.Stat(cpPath); err == nil {
				cpPostMut, _ = os.ReadFile(cpPath)
			}
			claimPath := filepath.Join(home, "projects", projectID, "runs", runID, "workclaims.json")
			var claimPostMut []byte
			if b, err := os.ReadFile(claimPath); err == nil {
				claimPostMut = b
			}

			var ctr sideEffectCounters
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
					ctr.inv.Add(1)
					return baseInv(ctx, repo, at)
				},
				OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
					ctr.led.Add(1)
					return baseLed(nowFn)
				},
				Executor: testspawn.Executor{
					HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
					Calls: calls,
				},
			})
			// STRICT zero inventory and zero ledger open.
			if ctr.inv.Load() != 0 {
				t.Fatalf("inventory called %d times (must be 0); err=%v status=%s msg=%s",
					ctr.inv.Load(), err, res.Status, res.Message)
			}
			if ctr.led.Load() != 0 {
				t.Fatalf("ledger open called %d times (must be 0); err=%v status=%s",
					ctr.led.Load(), err, res.Status)
			}
			if calls["wi_only"] != 0 {
				t.Fatalf("executor launched: %+v", calls)
			}
			// A7-15: exact bytes vs post-mutation pre-Execute snapshot (not pristine baseline).
			elogAfter, _ := os.ReadFile(elogPath)
			if string(elogAfter) != string(elogPostMut) {
				t.Fatalf("event log mutated by Execute: before=%d after=%d", len(elogPostMut), len(elogAfter))
			}
			partAfter, _ := os.ReadFile(partPath)
			if string(partAfter) != string(partPostMut) {
				t.Fatalf("partial mutated by Execute: before=%d after=%d", len(partPostMut), len(partAfter))
			}
			if cpPostMut != nil {
				cpAfter, _ := os.ReadFile(cpPath)
				if string(cpAfter) != string(cpPostMut) {
					t.Fatalf("checkpoint mutated by Execute: before=%d after=%d", len(cpPostMut), len(cpAfter))
				}
			}
			claimAfter, claimErr := os.ReadFile(claimPath)
			if claimPostMut == nil {
				if claimErr == nil {
					t.Fatalf("workclaims.json created by Execute (spend): %d bytes", len(claimAfter))
				}
			} else if string(claimAfter) != string(claimPostMut) {
				t.Fatalf("workclaims mutated by Execute: before=%d after=%d", len(claimPostMut), len(claimAfter))
			}
			if err == nil && res.Status == "succeeded" {
				t.Fatalf("unexpected succeeded status on authority mutation")
			}
		})
	}

}

// TestA714_AuditPriorSucceeded_ExactOnly remains a non-spend unit gate.
func TestA714_AuditPriorSucceeded_ExactOnly(t *testing.T) {
	wf := []workflowrun.ChildOutcome{
		{WorkItemID: "wi", AttemptID: " att-x ", Terminal: "succeeded", OutputEvidence: "ev"},
		{WorkItemID: "wi2", AttemptID: "att-y", Terminal: "Succeeded", OutputEvidence: "ev2"},
		{WorkItemID: "wi3", AttemptID: "att-z", Terminal: "succeeded", OutputEvidence: "ev3"},
	}
	out := goalrun.AuditPriorSucceededFrom(wf, nil)
	if _, ok := out["wi"]; ok {
		t.Fatal("padded attempt must not qualify")
	}
	if _, ok := out["wi2"]; ok {
		t.Fatal("case-mutated terminal must not qualify")
	}
	if _, ok := out["wi3"]; !ok {
		t.Fatal("exact succeeded must qualify")
	}
}
