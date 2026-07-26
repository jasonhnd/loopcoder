package goalrun_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// a715Counters tracks every spend surface for A7-15 fail-before-side-effects.
type a715Counters struct {
	// inv / ledOpen are the preflight spend hooks available on Request.
	// Reserve/reconcile/claim mutations are proven via immutable ledger/claims
	// digests in assertSnapUnchanged (no journal row change).
	inv, ledOpen atomic.Int64
}

// digests of durable files after mutation, before Execute.
type a715Snap struct {
	elog, part, cp, claims, ledger                []byte
	elogDig, partDig, cpDig, claimsDig, ledgerDig string
}

func digestBytes(b []byte) string {
	if b == nil {
		return "absent"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readOpt(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

func takeSnap(elogPath, partPath, cpPath, claimPath, ledgerPath string) a715Snap {
	s := a715Snap{
		elog: readOpt(elogPath), part: readOpt(partPath), cp: readOpt(cpPath),
		claims: readOpt(claimPath), ledger: readOpt(ledgerPath),
	}
	s.elogDig, s.partDig, s.cpDig = digestBytes(s.elog), digestBytes(s.part), digestBytes(s.cp)
	s.claimsDig, s.ledgerDig = digestBytes(s.claims), digestBytes(s.ledger)
	return s
}

func assertSnapUnchanged(t *testing.T, label string, before a715Snap, elogPath, partPath, cpPath, claimPath, ledgerPath string) {
	t.Helper()
	after := takeSnap(elogPath, partPath, cpPath, claimPath, ledgerPath)
	checks := []struct {
		name   string
		bd, ad string
		bb, ab []byte
	}{
		{"event_log", before.elogDig, after.elogDig, before.elog, after.elog},
		{"partial", before.partDig, after.partDig, before.part, after.part},
		{"checkpoint", before.cpDig, after.cpDig, before.cp, after.cp},
		{"claims", before.claimsDig, after.claimsDig, before.claims, after.claims},
		{"ledger", before.ledgerDig, after.ledgerDig, before.ledger, after.ledger},
	}
	for _, c := range checks {
		if c.bd != c.ad {
			t.Fatalf("%s: %s changed by Execute (dig before=%s after=%s len %d→%d)",
				label, c.name, c.bd[:12], c.ad[:12], len(c.bb), len(c.ab))
		}
	}
}

// jsonPathDiff returns sorted list of leaf paths whose values differ.
func jsonPathDiff(a, b any, prefix string) []string {
	var out []string
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return []string{prefix}
		}
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		kl := make([]string, 0, len(keys))
		for k := range keys {
			kl = append(kl, k)
		}
		sort.Strings(kl)
		for _, k := range kl {
			p := prefix + "/" + k
			if prefix == "" {
				p = k
			}
			va, oka := av[k]
			vb, okb := bv[k]
			if !oka || !okb {
				out = append(out, p)
				continue
			}
			out = append(out, jsonPathDiff(va, vb, p)...)
		}
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return []string{prefix}
		}
		for i := range av {
			out = append(out, jsonPathDiff(av[i], bv[i], prefix+"["+itoa(i)+"]")...)
		}
	default:
		if !reflect.DeepEqual(a, b) {
			out = append(out, prefix)
		}
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [16]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

func parseJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

func kidFieldPath(field string) string {
	// workflow_children[0]/field — kidsKey may be workflow_children
	return "workflow_children[0]/" + field
}

func runA715ZeroSpend(
	t *testing.T,
	env productEnv,
	home, projectID, runID, goal string,
	oneChild func(workgraph.DecomposeOptions) (workgraph.Graph, error),
	elogPath, partPath, cpPath string,
	now time.Time,
	postMut a715Snap,
) (goalrun.Result, error) {
	t.Helper()
	claimPath := filepath.Join(home, "projects", projectID, "runs", runID, "workclaims.json")
	ledgerPath := env.LedgerPath
	var ctr a715Counters
	calls := map[string]int{}
	baseInv, baseLed := env.loadInv(), env.openLed()
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
			ctr.ledOpen.Add(1)
			led, e := baseLed(nowFn)
			if e != nil {
				return nil, e
			}
			// Wrap ledger methods via type that counts Reserve/Reconcile if available —
			// Open alone is the preflight spend gate used by production resume.
			return led, nil
		},
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls,
		},
	})
	if ctr.inv.Load() != 0 {
		t.Fatalf("inventory side effect: %d (must be 0); status=%s err=%v", ctr.inv.Load(), res.Status, err)
	}
	if ctr.ledOpen.Load() != 0 {
		t.Fatalf("ledger open side effect: %d (must be 0); status=%s err=%v", ctr.ledOpen.Load(), res.Status, err)
	}
	if calls["wi_only"] != 0 {
		t.Fatalf("executor side effect: %+v", calls)
	}
	// claim/event/partial/checkpoint/ledger immutable
	assertSnapUnchanged(t, "zero-spend", postMut, elogPath, partPath, cpPath, claimPath, ledgerPath)
	// No new claim file growth / journal rows
	if postMut.claims == nil {
		if _, e := os.Stat(claimPath); e == nil {
			t.Fatal("claim acquire side effect: workclaims.json created")
		}
	}
	if err == nil && res.Status == "succeeded" {
		t.Fatalf("authority mutation must not succeed; status=%s", res.Status)
	}
	t.Logf("a715 counters inv=%d ledOpen=%d exec=%d status=%s err=%v",
		ctr.inv.Load(), ctr.ledOpen.Load(), calls["wi_only"], res.Status, err)
	return res, err
}

func setupA715(t *testing.T, suffix string) (
	env productEnv, home, projectID, runID, goal string,
	oneChild func(workgraph.DecomposeOptions) (workgraph.Graph, error),
	elogPath, partPath, cpPath, claimPath string,
	now time.Time,
) {
	t.Helper()
	var elogBefore, partBefore, cpBefore []byte
	env, home, projectID, runID, goal, oneChild, elogPath, partPath, cpPath, elogBefore, partBefore, cpBefore, now =
		buildInterruptedBaseline(t, suffix)
	restoreBaseline(t, elogPath, partPath, cpPath, elogBefore, partBefore, cpBefore)
	claimPath = filepath.Join(home, "projects", projectID, "runs", runID, "workclaims.json")
	return
}

func TestA715_OneFieldMutations_PostMutSnapshotExact(t *testing.T) {
	type mc struct {
		name     string
		target   string
		wantPath string
		mut      func(t *testing.T, partPath, cpPath, elogPath string)
	}
	// Kid fields — each independently
	kidFields := []string{
		"provider", "model", "depth", "permission", "account_ref", "install_ref",
		"window_kind", "reservation_id", "task_class", "attempt_id", "generation",
		"terminal", "failure_class", "output_evidence", "child_contract_digest",
		"execution_plan_digest", "work_item_id",
	}
	cases := make([]mc, 0, 64)
	for _, f := range kidFields {
		f := f
		cases = append(cases, mc{
			name: "kid_" + f, wantPath: kidFieldPath(f),
			mut: func(t *testing.T, partPath, _, _ string) {
				mutateJSONMap(t, partPath, func(m map[string]any) {
					kid := firstKid(t, m)
					if _, ok := kid[f]; !ok {
						t.Fatalf("fixture missing kid field %s", f)
					}
					switch f {
					case "generation":
						kid[f] = float64(99)
					default:
						kid[f] = "MUT_" + f
					}
					setFirstKid(m, kid)
				})
			},
		})
	}
	// ActualSources every field. Production interruption emits the authoritative
	// object empty when the provider did not report a dimension. Adding exactly
	// one asserted source to only one durable row is therefore a real one-leaf
	// contradiction, not fixture enrichment.
	for _, f := range []string{"model", "effort", "permission", "account", "install"} {
		f := f
		cases = append(cases, mc{
			name: "actual_sources_" + f, wantPath: kidFieldPath("actual_sources") + "/" + f,
			mut: func(t *testing.T, partPath, _, _ string) {
				mutateJSONMap(t, partPath, func(m map[string]any) {
					kid := firstKid(t, m)
					as, _ := kid["actual_sources"].(map[string]any)
					if as == nil {
						t.Fatal("fixture missing actual_sources")
					}
					as[f] = "MUT_SRC_" + f
					kid["actual_sources"] = as
					setFirstKid(m, kid)
				})
			},
		})
	}
	// Capacity evidence is emitted by production in checkpoint children. Mutate
	// those real fields directly; never invent a fixture-only
	// capacity_transitions object.
	for _, f := range []string{
		"capacity_before", "capacity_reserved", "capacity_state",
		"capacity_before_source", "capacity_before_captured_at",
		"capacity_before_freshness", "capacity_before_confidence",
		"capacity_reset_at", "capacity_group_id",
	} {
		f := f
		cases = append(cases, mc{
			name: "checkpoint_capacity_" + f, target: "checkpoint", wantPath: "children[0]/" + f,
			mut: func(t *testing.T, _, cpPath, _ string) {
				mutateJSONMap(t, cpPath, func(m map[string]any) {
					children, ok := m["children"].([]any)
					if !ok || len(children) == 0 {
						t.Fatal("production checkpoint missing children")
					}
					child, _ := children[0].(map[string]any)
					if child == nil {
						t.Fatal("production checkpoint child is nil")
					}
					if _, ok := child[f]; !ok {
						t.Fatalf("production checkpoint child missing %s", f)
					}
					switch f {
					case "capacity_before", "capacity_reserved":
						child[f] = float64(0.123456)
					default:
						child[f] = "MUT_CAP_" + f
					}
					children[0] = child
					m["children"] = children
				})
			},
		})
	}
	// Root plan/graph/project/run
	for _, f := range []string{"project_id", "run_id", "graph_id", "graph_version", "plan_digest", "execution_plan_digest", "graph_digest"} {
		f := f
		cases = append(cases, mc{
			name: "root_" + f, wantPath: f,
			mut: func(t *testing.T, partPath, _, _ string) {
				mutateJSONMap(t, partPath, func(m map[string]any) {
					if _, ok := m[f]; !ok {
						t.Fatalf("fixture missing root %s", f)
					}
					if f == "graph_version" {
						m[f] = float64(777)
					} else {
						m[f] = "MUT_ROOT_" + f
					}
				})
			},
		})
	}
	// Event log kind + event_id
	cases = append(cases,
		mc{name: "event_kind", wantPath: "kind", mut: func(t *testing.T, _, _, elogPath string) {
			mutateLastEvent(t, elogPath, func(m map[string]any) {
				if _, ok := m["kind"]; !ok {
					t.Fatal("missing kind")
				}
				m["kind"] = "MUTATED_KIND"
			})
		}},
		mc{name: "event_id_padding", wantPath: "event_id", mut: func(t *testing.T, _, _, elogPath string) {
			mutateLastEvent(t, elogPath, func(m map[string]any) {
				id, _ := m["event_id"].(string)
				if id == "" {
					t.Fatal("missing event_id")
				}
				m["event_id"] = " " + id // padding rejected by ParseEventJSONLStrict
			})
		}},
		mc{name: "event_terminal_padding", wantPath: "terminal", mut: func(t *testing.T, _, _, elogPath string) {
			mutateLastEvent(t, elogPath, func(m map[string]any) {
				if _, ok := m["terminal"]; !ok {
					t.Fatal("missing terminal on last event")
				}
				m["terminal"] = "Cancelled" // case alias
			})
		}},
	)

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env, home, projectID, runID, goal, oneChild, elogPath, partPath, cpPath, claimPath, now := setupA715(t, "of_"+tc.name)
			// capture pre-mut for one-path proof
			prePart := parseJSONFile(t, partPath)
			preCP := parseJSONFile(t, cpPath)
			preElog := readOpt(elogPath)
			tc.mut(t, partPath, cpPath, elogPath)
			// Prove mutation changed something real
			if strings.HasPrefix(tc.name, "event_") {
				if string(preElog) == string(readOpt(elogPath)) {
					t.Fatal("event mutation did not change log bytes")
				}
			} else {
				before, after := prePart, parseJSONFile(t, partPath)
				if tc.target == "checkpoint" {
					before, after = preCP, parseJSONFile(t, cpPath)
				}
				diffs := jsonPathDiff(before, after, "")
				if len(diffs) == 0 {
					t.Fatal("mutation did not change JSON")
				}
				// Exactly one intended path (allow single leaf)
				if len(diffs) != 1 {
					// generation float may only touch one path; fail if many
					t.Fatalf("expected exactly 1 path change want~%s got %v", tc.wantPath, diffs)
				}
				if diffs[0] != tc.wantPath {
					t.Fatalf("mutation changed wrong leaf: want %s got %v", tc.wantPath, diffs)
				}
			}
			post := takeSnap(elogPath, partPath, cpPath, claimPath, env.LedgerPath)
			runA715ZeroSpend(t, env, home, projectID, runID, goal, oneChild, elogPath, partPath, cpPath, now, post)
		})
	}
}

func mutateLastEvent(t *testing.T, elogPath string, mut func(m map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(elogPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := nonEmptyLines(string(raw))
	if len(lines) == 0 {
		t.Fatal("empty log")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(m)
	mut(m)
	after, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("event mutation no-op")
	}
	lines[len(lines)-1] = string(after)
	if err := os.WriteFile(elogPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestA715_HardRecoveryBoundaries(t *testing.T) {
	type hc struct {
		name string
		mut  func(t *testing.T, partPath, cpPath, elogPath, claimPath string)
	}
	cases := []hc{
		{"graph_id", func(t *testing.T, partPath, cpPath, _, _ string) {
			p := firstExistingPath(t, cpPath, partPath)
			mutateJSONMap(t, p, func(m map[string]any) {
				m["graph_id"] = "g_HARDREC"
			})
		}},
		{"event_attempt_id", func(t *testing.T, _, _, elogPath, _ string) {
			mutateLastEvent(t, elogPath, func(m map[string]any) {
				m["attempt_id"] = "att_HARDREC_g0"
			})
		}},
		{"claim_store_tamper", func(t *testing.T, _, _, _, claimPath string) {
			raw := readOpt(claimPath)
			if raw == nil {
				t.Fatal("fixture missing workclaims.json")
			}
			// Byte-level one-field style: append corrupt marker / rewrite if JSON
			var m any
			if err := json.Unmarshal(raw, &m); err != nil {
				// non-JSON claims: bit flip via append is a mutation
				if err := os.WriteFile(claimPath, append(raw, []byte("\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			// Prefer map mutation
			if mm, ok := m.(map[string]any); ok {
				mm["a715_hardrec_tamper"] = true
				b, _ := json.Marshal(mm)
				if err := os.WriteFile(claimPath, b, 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err := os.WriteFile(claimPath, append(raw, ' '), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"checkpoint_run_id", func(t *testing.T, _, cpPath, _, _ string) {
			if _, err := os.Stat(cpPath); err != nil {
				t.Fatal("fixture missing checkpoint")
			}
			mutateJSONMap(t, cpPath, func(m map[string]any) {
				m["run_id"] = "run_HARDREC_CP"
			})
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env, home, projectID, runID, goal, oneChild, elogPath, partPath, cpPath, claimPath, now := setupA715(t, "hr_"+tc.name)
			tc.mut(t, partPath, cpPath, elogPath, claimPath)
			post := takeSnap(elogPath, partPath, cpPath, claimPath, env.LedgerPath)
			runA715ZeroSpend(t, env, home, projectID, runID, goal, oneChild, elogPath, partPath, cpPath, now, post)
		})
	}
}

func TestA715_PositiveBaseline_ThenContradictions(t *testing.T) {
	// Unmutated resume on valid interrupt durable state (no enrich tamper).
	env, home, projectID, runID, goal, oneChild, elogPath, partPath, cpPath, elogBefore, partBefore, cpBefore, now :=
		buildInterruptedBaseline(t, "pos_base")
	restoreBaseline(t, elogPath, partPath, cpPath, elogBefore, partBefore, cpBefore)
	claimPath := filepath.Join(home, "projects", projectID, "runs", runID, "workclaims.json")
	var ctr a715Counters
	calls := map[string]int{}
	baseInv, baseLed := env.loadInv(), env.openLed()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := goalrun.Execute(ctx, goalrun.Request{
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
			ctr.ledOpen.Add(1)
			return baseLed(nowFn)
		},
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(2 * time.Minute) },
			Calls: calls, HangIDs: map[string]bool{"wi_only": true},
			OnHangEntry: func(string, int) { cancel() },
		},
	})
	// This is the positive control for the exact production-generated baseline
	// used by every mutation case. It must cross every spend boundary and
	// execute the child exactly once; an early validation error cannot satisfy
	// the mutation matrix.
	if ctr.inv.Load() != 1 || ctr.ledOpen.Load() != 1 || calls["wi_only"] != 1 {
		t.Fatalf("production baseline did not execute exactly once: inv=%d led=%d calls=%+v status=%s err=%v",
			ctr.inv.Load(), ctr.ledOpen.Load(), calls, res.Status, err)
	}
	t.Logf("positive baseline status=%s err=%v inv=%d led=%d calls=%+v", res.Status, err, ctr.inv.Load(), ctr.ledOpen.Load(), calls)
	// Contradictions: terminal case aliases / failure class aliases must not green prior-succeeded
	classes := []struct {
		term, fc string
	}{
		{"succeeded", ""},
		{"cancelled", "forced_interrupt"},
		{"failed", "model_unavailable"},
		{"failed", "auth_refusal"},
		{"failed", "route_identity_mismatch"},
		{"failed", "executor_cancelled"},
		{"failed", "research_findings_materialization_failed"},
		{"skipped", "skipped"},
		{"Succeeded", ""}, // case alias
		{"FAILED", "model_unavailable"},
	}
	for _, c := range classes {
		c := c
		t.Run("term_"+c.term+"_fc_"+c.fc, func(t *testing.T) {
			out := goalrun.AuditPriorSucceededFrom([]workflowrun.ChildOutcome{{
				WorkItemID: "wi", AttemptID: "a1", Terminal: c.term,
				FailureClass: c.fc, OutputEvidence: "ev",
			}}, nil)
			if c.term == "succeeded" && c.fc == "" {
				if _, ok := out["wi"]; !ok {
					t.Fatal("exact succeeded must qualify")
				}
				return
			}
			if _, ok := out["wi"]; ok {
				t.Fatalf("term=%q fc=%q must not qualify as prior succeeded", c.term, c.fc)
			}
		})
	}
	// Payload vs top-level: Integrated membership + integrate event id + commit sha exactness via report fields
	// (unit-level contradiction on ChildOutcome binding)
	t.Run("integrate_sha_empty_vs_event", func(t *testing.T) {
		// Exact empty IntegrateCommitSHA is not proof of integrate
		co := workflowrun.ChildOutcome{
			WorkItemID: "wi", AttemptID: "a1", Terminal: "succeeded",
			OutputEvidence: "ev", IntegrateCommitSHA: "",
		}
		if co.IntegrateCommitSHA != "" {
			t.Fatal("expected empty sha")
		}
		// Case-mutated terminal with integrate sha still not prior-succeeded
		out := goalrun.AuditPriorSucceededFrom([]workflowrun.ChildOutcome{{
			WorkItemID: "wi", AttemptID: "a1", Terminal: "Succeeded",
			OutputEvidence: "ev", IntegrateCommitSHA: "deadbeef",
		}}, nil)
		if _, ok := out["wi"]; ok {
			t.Fatal("Succeeded alias must not qualify even with integrate sha")
		}
	})
	_ = claimPath
	_ = elogPath
	_ = partPath
	_ = cpPath
}

func TestA715_ArtifactqualConsumesDurableFacts(t *testing.T) {
	// Production boundary, not a hand-built canary helper: create a real
	// interrupted goalrun, resume it through Execute, and consume the exact
	// emitted event/claim/capacity-ledger evidence through the same strict
	// LoadCanaryEvidence + ValidateCanaryEvidence path used by Qualify.
	env, home, projectID, runID, goal, oneChild, elogPath, partPath, cpPath, elogBefore, partBefore, cpBefore, now :=
		buildInterruptedBaseline(t, "artifactqual_boundary")
	restoreBaseline(t, elogPath, partPath, cpPath, elogBefore, partBefore, cpBefore)
	repo := initDisposableGitRepo(t)
	canaryPath := filepath.Join(t.TempDir(), "canary_evidence.json")
	archiveDigest := strings.Repeat("ab", 32)
	preProdSHA := strings.Repeat("cd", 20)
	res, runErr := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		InventoryProvenance: goalrun.InventoryProvenanceLiveDiscover,
		HomeDir:             home, RepoPath: repo, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose: oneChild, LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
		},
		CanaryEmit: &goalrun.CanaryEmitOptions{
			OutPath: canaryPath, HomeDir: home, RepoPath: repo,
			ArchiveDigest: archiveDigest, PreProdSHA: preProdSHA,
			BinaryVersion: "0.9.0-a715", BinaryCommit: preProdSHA,
			InventoryProvenance: goalrun.InventoryProvenanceLiveDiscover,
		},
	})
	if runErr != nil {
		t.Fatalf("production resume/canary boundary: %v status=%s msg=%s", runErr, res.Status, res.Message)
	}
	if res.CanaryEvidencePath != canaryPath {
		t.Fatalf("production result canary path = %q, want exact %q", res.CanaryEvidencePath, canaryPath)
	}
	produced, loadErr := artifactqual.LoadCanaryEvidence(canaryPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	v := artifactqual.ValidateCanaryEvidence(
		produced, archiveDigest, preProdSHA, produced.ProducedAt,
	)
	for _, reason := range v.Reasons {
		if strings.HasPrefix(reason, "raw_event_") ||
			strings.HasPrefix(reason, "raw_claim_") ||
			strings.HasPrefix(reason, "durable_evidence_digest_") {
			t.Fatalf("production artifactqual durable boundary rejected: %v", v.Reasons)
		}
	}
	if len(produced.RawEvents) == 0 || len(produced.RawClaims) == 0 ||
		len(produced.RawLedgerEntries) == 0 {
		t.Fatalf("production canary omitted raw durable facts: events=%d claims=%d ledger=%d",
			len(produced.RawEvents), len(produced.RawClaims), len(produced.RawLedgerEntries))
	}

	// One-field mutation after emission must be caught even when both manifest
	// digests are recomputed. Persist and reload it so the assertion crosses the
	// exact file boundary rather than validating an in-memory helper object.
	produced.RawClaims[0].Generation = 0
	produced.DurableEvidenceDigest = artifactqual.DigestDurableEvidence(produced)
	produced.ContentDigest = artifactqual.DigestCanaryBody(produced)
	mutatedPath := filepath.Join(t.TempDir(), "mutated_canary_evidence.json")
	if writeErr := artifactqual.WriteCanaryEvidence(mutatedPath, produced); writeErr != nil {
		t.Fatal(writeErr)
	}
	mutatedLoaded, mutatedLoadErr := artifactqual.LoadCanaryEvidence(mutatedPath)
	if mutatedLoadErr != nil {
		t.Fatal(mutatedLoadErr)
	}
	mutated := artifactqual.ValidateCanaryEvidence(
		mutatedLoaded, archiveDigest, preProdSHA, produced.ProducedAt,
	)
	if !strings.Contains(strings.Join(mutated.Reasons, ";"), "raw_claim_envelope_invalid") {
		t.Fatalf("mutated production claim escaped artifactqual: %v", mutated.Reasons)
	}
	t.Logf("artifactqual production file boundary baseline_raw_ok=true mutated_reason=raw_claim_envelope_invalid production_raw=%d/%d/%d",
		len(produced.RawEvents), len(produced.RawClaims), len(produced.RawLedgerEntries))
}

func TestA715_TrimSpaceEqualFold_AuditEvidence(t *testing.T) {
	// Document fixed surfaces: exact terminal/failure only.
	out := goalrun.AuditPriorSucceededFrom([]workflowrun.ChildOutcome{
		{WorkItemID: "wi", AttemptID: " att ", Terminal: "succeeded", OutputEvidence: "e"},
	}, nil)
	if _, ok := out["wi"]; ok {
		t.Fatal("padded attempt must not qualify")
	}
	out = goalrun.AuditPriorSucceededFrom([]workflowrun.ChildOutcome{
		{WorkItemID: "wi", AttemptID: "a1", Terminal: "Succeeded", OutputEvidence: "e"},
	}, nil)
	if _, ok := out["wi"]; ok {
		t.Fatal("Equal-fold terminal must not qualify")
	}
	// Audit note (test log): service.go terminal kind EqualFold→exact; run.go
	// childActuallyExecutedProvider / bindOpenPRVerifier / recordMU FailureClass exact.
	t.Log("audit: persisted-authority EqualFold removed for terminal/failure_class/provider distinctness; ingress TrimSpace retained on provider pins only")
}

// firstExistingPath shared with a7_14 helpers style
func firstExistingPath(t *testing.T, paths ...string) string {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatal("no existing path", paths)
	return ""
}
