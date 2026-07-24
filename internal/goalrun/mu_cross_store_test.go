package goalrun_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestGoalrunModelUnavailable_FullCrossStoreIdentity forces model_unavailable on
// the first launch then succeeds on alternate, using the real test ledger path
// and durable checkpoint. Asserts exact identity across reports, outcomes,
// ledger, claims, events, partial, and checkpoint.
func TestGoalrunModelUnavailable_FullCrossStoreIdentity(t *testing.T) {
	now := time.Date(2026, 7, 22, 21, 0, 0, 0, time.UTC)
	home := testHome(t)
	ledgerPath := filepath.Join(t.TempDir(), "cap-mu.json")

	// Full 64-hex account refs (exact-routable); avoid non-canonical strings that
	// re-hash under CanonicalAccountRef and break alternate reserve equality.
	acctA := fullAcct("codex-billing-alice")
	acctB := fullAcct("grok-billing-bob")
	instA, instB := "install-codex-full-id", "install-grok-full-id"

	mk := func(provider, model, acct, inst string, rem float64) capacitysnapshot.AccountObservation {
		return capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: provider, AccountRef: acct, InstallRef: inst,
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100 - rem, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: rem, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				ResetAt: ptrTime(now.Add(2 * time.Hour)), CapturedAt: now, Source: "test-machine-observed",
			}},
			Models: []capacitysnapshot.ModelSpec{{
				ModelID: model, SupportedDepths: []string{"medium"}, DefaultDepth: "medium", Present: true,
			}},
			Source: "test-machine-observed", CapturedAt: now,
		})
	}
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		mk("codex", "gpt-5.5", acctA, instA, 90),
		mk("grok", "grok-4.5", acctB, instB, 90),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Ensure AccountRef on snap is the canonical form we will use for alternates.
	for i := range snap.Accounts {
		snap.Accounts[i].AccountRef = capacityledger.CanonicalAccountRef(snap.Accounts[i].AccountRef)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	okFact := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	ff := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	var cands []eligibility.Candidate
	var softs []quotapolicy.Candidate
	for _, p := range []struct{ prov, model, acct, inst string }{
		{"codex", "gpt-5.5", capacityledger.CanonicalAccountRef(acctA), instA},
		{"grok", "grok-4.5", capacityledger.CanonicalAccountRef(acctB), instB},
	} {
		for _, perm := range []string{"read-only", "bounded_write"} {
			cands = append(cands, eligibility.Candidate{
				Provider: p.prov, Model: p.model, Effort: "medium", Permission: perm,
				ModelClass: capclass.ClassSoul, AccountRef: p.acct, InstallRef: p.inst, WindowKind: "five_hour",
				Installed: okFact(p.prov + "-i"), Authenticated: okFact(p.prov + "-a"), ModelPresent: okFact(p.prov + "-m"),
				PermissionOK: okFact(p.prov + "-p"), EffortOK: okFact(p.prov + "-e"), Healthy: okFact(p.prov + "-h"),
				CooldownActive: ff(p.prov + "-cd"), ResourceFit: okFact(p.prov + "-r"), QuotaRemaining: 9999,
			})
		}
		rf, ttr, rel := 0.9, 2*time.Hour, 0.95
		softs = append(softs, quotapolicy.Candidate{
			Provider: p.prov, Model: p.model, AccountRef: p.acct, InstallRef: p.inst, WindowKind: "five_hour",
			Windows: []quotapolicy.Window{{
				Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf,
				Evidence: quotapolicy.EvidenceExact, TimeToReset: &ttr,
			}},
			Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact,
		})
	}
	inv.Candidates = cands
	inv.Soft = softs
	inv.Machine = eligibility.MachineAdmission{
		CapacityOK:      eligibility.Fact{State: eligibility.FactTrue, EvidenceID: "m", Freshness: eligibility.FreshFresh},
		ConcurrentSlots: 4,
	}

	calls := map[string]int{}
	exec := &failFirstThenSucceedExec{
		HomeDir: home, Now: func() time.Time { return now }, Calls: calls,
	}

	req := goalrun.Request{
		ProjectID: "proj-mu-xstore", RunID: "run_mu_xstore",
		Goal: "implement mu cross-store identity", Issue: "1397",
		Actor: "owner", Owner: "worker",
		HomeDir: home, Now: func() time.Time { return now },
		// Auto-route: empty pin so both candidates participate and same-depth alts fill.
		LoadInventory: func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
			return inv, snap, nil
		},
		OpenLedger: func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
			return capacityledger.OpenPath(ledgerPath, nowFn)
		},
		Executor: exec,
		Decompose: func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
			g := workgraph.Graph{
				Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
				GraphID: "g_mu_xstore", Version: 1,
				Source: workgraph.SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "owner",
				Items: []workgraph.WorkItem{{
					Schema: workgraph.SchemaItem, ID: "wi_impl", Status: workgraph.ItemRequired,
					Intent: "implementation: " + opts.Goal, Owner: "worker",
					Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1,
					OutputContract:   "branch+diff",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write",
				}},
				Limits: workgraph.DefaultLimits(),
			}
			g.PlanDigest = workgraph.DigestGraph(g)
			return g, nil
		},
	}

	res, err := goalrun.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute err=%v status=%s msg=%s", err, res.Status, res.Message)
	}
	if res.Status != workflowrun.StatusHumanGate {
		t.Fatalf("status=%q want %q msg=%s", res.Status, workflowrun.StatusHumanGate, res.Message)
	}
	if res.PlanDigest == "" || res.GraphDigest == "" {
		t.Fatalf("result digests empty: plan=%q graph=%q status=%s msg=%s",
			res.PlanDigest, res.GraphDigest, res.Status, res.Message)
	}

	var wfFailed, wfOK *workflowrun.ChildOutcome
	for i := range res.Workflow.Children {
		c := &res.Workflow.Children[i]
		if c.WorkItemID != "wi_impl" {
			continue
		}
		if c.FailureClass == "model_unavailable" {
			wfFailed = c
		}
		if c.Terminal == "succeeded" {
			wfOK = c
		}
	}
	if wfFailed == nil || wfOK == nil {
		t.Fatalf("need model_unavailable + succeeded: status=%s msg=%s children=%+v calls=%v",
			res.Status, res.Message, res.Workflow.Children, calls)
	}
	var report *goalrun.ChildReport
	for i := range res.Children {
		if res.Children[i].ChildID == "wi_impl" && res.Children[i].Terminal == "succeeded" {
			report = &res.Children[i]
		}
	}
	if report == nil {
		t.Fatalf("ChildReport missing succeeded wi_impl: %+v", res.Children)
	}

	wantPlan := res.PlanDigest
	wantGraph := res.GraphDigest
	wantCCD := wfFailed.ChildContractDigest
	wantClass := wfFailed.TaskClass
	if wantCCD == "" || len(strings.TrimPrefix(wantCCD, "sha256:")) != 64 {
		t.Fatalf("CCD not full sha256: %q", wantCCD)
	}
	if wfOK.ChildContractDigest != wantCCD || report.ChildContractDigest != wantCCD {
		t.Fatalf("CCD mismatch")
	}
	if wfFailed.ExecutionPlanDigest != wantPlan || wfOK.ExecutionPlanDigest != wantPlan || report.ExecutionPlanDigest != wantPlan {
		t.Fatalf("plan mismatch")
	}
	if wfFailed.TaskClass != wantClass || wfOK.TaskClass != wantClass || report.TaskClass != wantClass {
		t.Fatalf("class mismatch")
	}
	if wfFailed.AttemptID == wfOK.AttemptID {
		t.Fatal("attempt ids must differ")
	}
	if wfFailed.Generation < 1 || wfOK.Generation < 1 || report.Generation < 1 {
		t.Fatalf("positive gen failed=%d ok=%d report=%d", wfFailed.Generation, wfOK.Generation, report.Generation)
	}
	if wfOK.Generation <= wfFailed.Generation {
		t.Fatalf("gen must increase failed=%d ok=%d", wfFailed.Generation, wfOK.Generation)
	}
	if report.Generation != wfOK.Generation || report.AttemptID != wfOK.AttemptID {
		t.Fatalf("report must match succeeded outcome")
	}

	led, err := capacityledger.OpenPath(ledgerPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	eFail, okF := led.Get(res.ProjectID, res.RunID, wfFailed.AttemptID)
	eOK, okO := led.Get(res.ProjectID, res.RunID, wfOK.AttemptID)
	if !okF || !okO {
		t.Fatalf("ledger missing entries fail=%v ok=%v", okF, okO)
	}
	for name, e := range map[string]capacityledger.Entry{"fail": eFail, "ok": eOK} {
		if e.PlanDigest != wantPlan || e.GraphDigest != wantGraph || e.TaskClass != wantClass || e.ChildContractDigest != wantCCD {
			t.Fatalf("ledger %s identity: %+v", name, e)
		}
		if e.ProjectID != res.ProjectID {
			t.Fatalf("ledger project %q want %q", e.ProjectID, res.ProjectID)
		}
	}
	if len(res.Workflow.CapacityTransitions) < 2 {
		t.Fatalf("want prior+alternate capacity transitions, got %d", len(res.Workflow.CapacityTransitions))
	}

	claimPath := filepath.Join(home, "projects", res.ProjectID, "runs", res.RunID, "workclaims.json")
	cs, err := workclaim.OpenPath(claimPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	cf, err := cs.GetByAttempt(res.ProjectID, res.GraphID, res.Workflow.GraphVersion, "wi_impl", wfFailed.AttemptID)
	if err != nil {
		t.Fatalf("failed claim: %v", err)
	}
	ca, err := cs.GetByAttempt(res.ProjectID, res.GraphID, res.Workflow.GraphVersion, "wi_impl", wfOK.AttemptID)
	if err != nil {
		t.Fatalf("ok claim: %v", err)
	}
	// Both claims: full identity, distinct attempts, increasing generation.
	for name, cl := range map[string]workclaim.Claim{"fail": cf, "ok": ca} {
		if cl.PlanDigest != wantPlan || cl.TaskClass != wantClass || cl.ChildContractDigest != wantCCD {
			t.Fatalf("claim %s identity: %+v", name, cl)
		}
		if cl.AttemptID == "" || cl.Generation < 1 {
			t.Fatalf("claim %s attempt/gen: %+v", name, cl)
		}
	}
	if cf.AttemptID == ca.AttemptID {
		t.Fatal("claim attempt ids must differ")
	}
	if ca.Generation <= cf.Generation {
		t.Fatalf("claim gen")
	}

	// Capacity transitions bind both attempts.
	var sawPrior, sawAlt bool
	for _, tr := range res.Workflow.CapacityTransitions {
		if tr.AttemptID == wfFailed.AttemptID {
			sawPrior = true
		}
		if tr.AttemptID == wfOK.AttemptID {
			sawAlt = true
		}
	}
	if !sawPrior || !sawAlt {
		t.Fatalf("capacity transitions must bind both attempts prior=%v alt=%v transitions=%+v",
			sawPrior, sawAlt, res.Workflow.CapacityTransitions)
	}

	raw, err := os.ReadFile(res.Workflow.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	events, err := workflowrun.ParseEventJSONLStrict(string(raw), res.ProjectID, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	// Relevant events must carry nonempty exact plan/class/full CCD, attempt IDs,
	// and positive generation where schema carries them (not optional-empty).
	need := map[string]bool{"model_unavailable": false, "reroute": false, "claim": false, "launch": false, "terminal": false}
	for _, ev := range events {
		if ev.WorkItemID != "wi_impl" {
			continue
		}
		switch ev.Kind {
		case "claim", "launch", "model_unavailable", "reroute", "terminal":
			if ev.ExecutionPlanDigest != wantPlan {
				t.Fatalf("event %s plan %q want %q", ev.Kind, ev.ExecutionPlanDigest, wantPlan)
			}
			if ev.TaskClass != wantClass {
				t.Fatalf("event %s class %q want %q", ev.Kind, ev.TaskClass, wantClass)
			}
			if ev.ChildContractDigest != wantCCD {
				t.Fatalf("event %s ccd %q want %q", ev.Kind, ev.ChildContractDigest, wantCCD)
			}
			if len(strings.TrimPrefix(ev.ChildContractDigest, "sha256:")) != 64 {
				t.Fatalf("event %s ccd not full sha256", ev.Kind)
			}
			if strings.TrimSpace(ev.AttemptID) == "" {
				t.Fatalf("event %s missing attempt_id", ev.Kind)
			}
			if ev.Generation < 1 {
				t.Fatalf("event %s generation=%d want positive", ev.Kind, ev.Generation)
			}
			need[ev.Kind] = true
		}
	}
	for k, v := range need {
		if !v {
			t.Fatalf("missing event kind %s", k)
		}
	}
	// Exactly two launches (original + alternate), not more.
	launchN := 0
	for _, ev := range events {
		if ev.WorkItemID == "wi_impl" && ev.Kind == "launch" {
			launchN++
		}
	}
	if launchN != 2 {
		t.Fatalf("launch events=%d want 2 (no duplicate executor launches)", launchN)
	}
	if calls["wi_impl"] != 2 {
		t.Fatalf("executor calls=%v want 2", calls)
	}

	partial, err := workflowrun.LoadPartialPrior(home, res.ProjectID, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if partial.PlanDigest != wantPlan || partial.GraphDigest != wantGraph {
		t.Fatal("partial digests")
	}
	ps, okP := partial.PriorSucceeded["wi_impl"]
	if !okP {
		t.Fatal("partial missing PriorSucceeded")
	}
	if ps.ExecutionPlanDigest != wantPlan {
		t.Fatalf("partial prior plan %q want %q", ps.ExecutionPlanDigest, wantPlan)
	}
	if ps.TaskClass != wantClass {
		t.Fatalf("partial prior class %q want %q", ps.TaskClass, wantClass)
	}
	if ps.ChildContractDigest != wantCCD {
		t.Fatalf("partial prior ccd %q want %q", ps.ChildContractDigest, wantCCD)
	}
	if ps.AttemptID != wfOK.AttemptID {
		t.Fatalf("partial prior attempt %q want %q", ps.AttemptID, wfOK.AttemptID)
	}
	if ps.Generation != wfOK.Generation || ps.Generation < 1 {
		t.Fatalf("partial prior generation %d want %d (positive)", ps.Generation, wfOK.Generation)
	}

	cp, _, err := goalrun.LoadCheckpoint(home, res.ProjectID, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if cp.PlanDigest != wantPlan || cp.GraphDigest != wantGraph {
		t.Fatal("checkpoint digests")
	}
	foundR, foundW := false, false
	for _, c := range cp.Children {
		if c.ChildID != "wi_impl" {
			continue
		}
		foundR = true
		if c.ExecutionPlanDigest != wantPlan || c.TaskClass != wantClass || c.ChildContractDigest != wantCCD {
			t.Fatalf("checkpoint child plan/class/ccd: %+v", c)
		}
		if c.AttemptID != wfOK.AttemptID || c.Generation != wfOK.Generation {
			t.Fatalf("checkpoint child attempt/gen: %+v", c)
		}
	}
	for _, c := range cp.WorkflowKids {
		if c.WorkItemID != "wi_impl" || c.AttemptID != wfOK.AttemptID {
			continue
		}
		foundW = true
		if c.ExecutionPlanDigest != wantPlan || c.TaskClass != wantClass || c.ChildContractDigest != wantCCD {
			t.Fatalf("checkpoint workflow kid plan/class/ccd: %+v", c)
		}
		if c.Generation != wfOK.Generation {
			t.Fatalf("checkpoint workflow kid gen: %+v", c)
		}
	}
	if !foundR || !foundW {
		t.Fatalf("checkpoint missing report=%v workflowKid=%v", foundR, foundW)
	}

	successN := 0
	for _, c := range res.Workflow.Children {
		if c.WorkItemID == "wi_impl" && c.Terminal == "succeeded" {
			successN++
		}
	}
	if successN != 1 {
		t.Fatalf("want exactly one success, got %d", successN)
	}
	// Exactly one integration of the work item (not merely ≤1).
	intN := 0
	for _, id := range res.Workflow.Integrated {
		if id == "wi_impl" {
			intN++
		}
	}
	if intN != 1 {
		t.Fatalf("integration count of wi_impl=%d want exactly 1; Integrated=%v", intN, res.Workflow.Integrated)
	}
}

func fullAcct(seed string) string {
	sum := sha256.Sum256([]byte("acct|" + seed))
	return "acct-" + hex.EncodeToString(sum[:])
}

// failFirstThenSucceedExec fails the first Execute with model_unavailable, then succeeds.
type failFirstThenSucceedExec struct {
	HomeDir string
	Now     func() time.Time
	Calls   map[string]int
	n       int
}

func (f *failFirstThenSucceedExec) Execute(ctx context.Context, in workflowrun.ChildExecInput) (workflowrun.ChildExecResult, error) {
	base := workflowrun.FakeChildExecutor{HomeDir: f.HomeDir, Now: f.Now, Calls: f.Calls}
	f.n++
	if f.n == 1 {
		r, _ := base.Execute(ctx, in)
		r.Terminal = workgraph.TermFailed
		r.FailureClass = "model_unavailable"
		r.OutputEvidence = "failed:model_unavailable:" + in.WorkItemID
		r.Message = "model unavailable"
		return r, nil
	}
	return base.Execute(ctx, in)
}
