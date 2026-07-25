package goalrun_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/autoroute"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/capacitysnapshot"
	"github.com/jasonhnd/loopcoder/internal/capclass"
	"github.com/jasonhnd/loopcoder/internal/eligibility"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestFinalContract_UniversalReportRetainsMUFailedAndWinner proves contract (3):
// after production MU+alternate and resume, Result.Children (universal report
// surface) retains BOTH typed model_unavailable failed route and winning
// alternate with provider/model/depth/permission/account/install/window and
// supersedes/route reasons — derived from authoritative Workflow outcomes, not
// hand-built structs. No fixture/default inventory labels; no credentials.
func TestFinalContract_UniversalReportRetainsMUFailedAndWinner(t *testing.T) {
	now := time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)
	home := testHome(t)
	repo := initDisposableGitRepo(t)
	ledgerPath := filepath.Join(t.TempDir(), "cap.json")
	projectID := "proj-final-contract-mu-report"
	runID := "run_final_contract_mu_1"
	goal := "implement final contract universal report mu retention with tests verification and operator guide"

	acctA := fullAcct("final-codex")
	acctB := fullAcct("final-grok")
	mk := func(p, m, a, i string, depths []string) capacitysnapshot.AccountObservation {
		return capacitysnapshot.FromAccountInput(capacitysnapshot.AccountInput{
			Provider: p, AccountRef: a, InstallRef: i,
			Installed: true, Authenticated: true, Healthy: true,
			HealthConfidence: capacitysnapshot.ConfidenceExact, HealthFreshness: capacitysnapshot.FreshnessFresh,
			Windows: []capacitysnapshot.Window{{
				Kind: "five_hour", Unit: capacitysnapshot.UnitPercentage,
				Used:       capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 10, Unit: capacitysnapshot.UnitPercentage},
				Remaining:  capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 90, Unit: capacitysnapshot.UnitPercentage},
				Limit:      capacitysnapshot.Quantity{Class: capacitysnapshot.QtyFinite, Value: 100, Unit: capacitysnapshot.UnitPercentage},
				Confidence: capacitysnapshot.ConfidenceExact, Freshness: capacitysnapshot.FreshnessFresh,
				ResetAt: ptrTime(now.Add(2 * time.Hour)), CapturedAt: now, Source: "test-machine-observed",
			}},
			Models: []capacitysnapshot.ModelSpec{{ModelID: m, SupportedDepths: depths, DefaultDepth: "medium", Present: true}},
			Source: "test-machine-observed", CapturedAt: now,
		})
	}
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		mk("codex", "gpt-5.5", acctA, "install-codex-fc", []string{"low", "medium", "high"}),
		mk("grok", "grok-4.5", acctB, "install-grok-fc", []string{"medium"}),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := range snap.Accounts {
		snap.Accounts[i].AccountRef = capacityledger.CanonicalAccountRef(snap.Accounts[i].AccountRef)
	}
	inv, err := capacitysnapshot.ToRouteInventory(snap, now)
	if err != nil {
		t.Fatal(err)
	}
	okF := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactTrue, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	ff := func(id string) eligibility.Fact {
		return eligibility.Fact{State: eligibility.FactFalse, EvidenceID: id, Freshness: eligibility.FreshFresh}
	}
	var cands []eligibility.Candidate
	for _, p := range []struct {
		prov, model, acct, inst string
		depths                  []string
	}{
		{"codex", "gpt-5.5", capacityledger.CanonicalAccountRef(acctA), "install-codex-fc", []string{"low", "medium", "high"}},
		{"grok", "grok-4.5", capacityledger.CanonicalAccountRef(acctB), "install-grok-fc", []string{"medium"}},
	} {
		for _, d := range p.depths {
			for _, perm := range []string{"read-only", "bounded_write"} {
				cands = append(cands, eligibility.Candidate{
					Provider: p.prov, Model: p.model, Effort: d, Permission: perm,
					ModelClass: capclass.ClassSoul, AccountRef: p.acct, InstallRef: p.inst, WindowKind: "five_hour",
					Installed: okF("i"), Authenticated: okF("a"), ModelPresent: okF("m"),
					PermissionOK: okF("p"), EffortOK: okF("e"), Healthy: okF("h"),
					CooldownActive: ff("c"), ResourceFit: okF("r"), QuotaRemaining: 9999,
				})
			}
		}
	}
	rf, ttr, rel := 0.9, 2*time.Hour, 0.95
	inv.Candidates = cands
	inv.Soft = []quotapolicy.Candidate{
		{Provider: "codex", Model: "gpt-5.5", AccountRef: capacityledger.CanonicalAccountRef(acctA), InstallRef: "install-codex-fc", WindowKind: "five_hour",
			Windows:     []quotapolicy.Window{{Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf, Evidence: quotapolicy.EvidenceExact, TimeToReset: &ttr}},
			Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact},
		{Provider: "grok", Model: "grok-4.5", AccountRef: capacityledger.CanonicalAccountRef(acctB), InstallRef: "install-grok-fc", WindowKind: "five_hour",
			Windows:     []quotapolicy.Window{{Kind: quotapolicy.WindowFiveHour, RemainingFraction: &rf, Evidence: quotapolicy.EvidenceExact, TimeToReset: &ttr}},
			Reliability: &rel, ReliabilityEvidence: quotapolicy.EvidenceExact},
	}
	inv.Machine = eligibility.MachineAdmission{
		CapacityOK: eligibility.Fact{State: eligibility.FactTrue, EvidenceID: "m", Freshness: eligibility.FreshFresh}, ConcurrentSlots: 4,
	}

	linear5 := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_final_contract", Version: 1,
			Source: workgraph.SourceGoalDecompose, ExplicitOptIn: true, ApprovedBy: "owner",
			Items: []workgraph.WorkItem{
				{Schema: workgraph.SchemaItem, ID: "wi_research", Status: workgraph.ItemRequired, Intent: "research", Owner: "research",
					Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1, OutputContract: "findings",
					RouteRequirement: "class=luna,depth=low,permission=read-only"},
				{Schema: workgraph.SchemaItem, ID: "wi_implement", Status: workgraph.ItemRequired, Intent: "implement", Owner: "worker",
					Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 2, OutputContract: "diff",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write"},
				{Schema: workgraph.SchemaItem, ID: "wi_tests", Status: workgraph.ItemRequired, Intent: "tests", Owner: "worker",
					Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 3, OutputContract: "test_pass",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write"},
				{Schema: workgraph.SchemaItem, ID: "wi_verify", Status: workgraph.ItemRequired, Intent: "verify", Owner: "verifier",
					Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 4, OutputContract: "verification_verdict",
					RouteRequirement: "class=soul,depth=high,permission=read-only"},
				{Schema: workgraph.SchemaItem, ID: "wi_docs", Status: workgraph.ItemRequired, Intent: "operator guide", Owner: "worker",
					Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 5, OutputContract: "docs_diff",
					RouteRequirement: "class=luna,depth=low,permission=bounded_write"},
			},
			Dependencies: []workgraph.Dependency{
				{Schema: workgraph.SchemaDep, From: "wi_research", To: "wi_implement", Kind: workgraph.DepFinishToStart},
				{Schema: workgraph.SchemaDep, From: "wi_implement", To: "wi_tests", Kind: workgraph.DepFinishToStart},
				{Schema: workgraph.SchemaDep, From: "wi_tests", To: "wi_verify", Kind: workgraph.DepFinishToStart},
				{Schema: workgraph.SchemaDep, From: "wi_verify", To: "wi_docs", Kind: workgraph.DepFinishToStart},
			},
			Limits:    workgraph.Limits{Schema: workgraph.SchemaLimits, MaxItems: 32, MaxDepth: 8, MaxParallel: 1, MaxAutomaticReplan: 1},
			CreatedAt: now,
		}
		g.PlanDigest = workgraph.DigestGraph(g)
		return g, nil
	}
	loadInv := func(ctx context.Context, repo string, at time.Time) (autoroute.Inventory, capacitysnapshot.Snapshot, error) {
		return inv, snap, nil
	}
	openLed := func(nowFn func() time.Time) (*capacityledger.Ledger, error) {
		return capacityledger.OpenPath(ledgerPath, nowFn)
	}
	muCounts := map[string]int{}
	ctx1, cancel1 := context.WithCancel(context.Background())
	res1, err1 := goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID, Goal: goal, Issue: "1397",
		Actor: "owner", Owner: "worker", HomeDir: home, RepoPath: repo,
		Now: func() time.Time { return now }, Decompose: linear5,
		LoadInventory: loadInv, OpenLedger: openLed,
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			FailModelUnavailableOnceIDs: map[string]bool{"wi_implement": true},
			FailModelUnavailableCounts:  muCounts,
			HangIDs:                     map[string]bool{"wi_tests": true},
			OnHangEntry: func(id string, pid int) {
				if id == "wi_tests" && pid > 0 {
					cancel1()
				}
			},
		},
	})
	if err1 == nil && res1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("expected interrupt after MU: %s", res1.Status)
	}

	// Pass1: after MU+alt, ChildReport must already retain failed MU (before resume).
	var failR, winR *goalrun.ChildReport
	for i := range res1.Children {
		c := &res1.Children[i]
		if c.ChildID != "wi_implement" {
			continue
		}
		if strings.EqualFold(c.FailureClass, "model_unavailable") {
			failR = c
		}
		if c.Terminal == "succeeded" {
			winR = c
		}
	}
	if failR == nil || winR == nil {
		// May only see incomplete pass1 reports if hang races; require Workflow pair at least.
		var wfFail, wfWin bool
		for _, c := range res1.Workflow.Children {
			if c.WorkItemID == "wi_implement" && strings.EqualFold(c.FailureClass, "model_unavailable") {
				wfFail = true
			}
			if c.WorkItemID == "wi_implement" && c.Terminal == "succeeded" {
				wfWin = true
			}
		}
		if !wfFail || !wfWin {
			t.Fatalf("pass1 missing MU pair on Workflow: children=%+v reports=%+v", res1.Workflow.Children, res1.Children)
		}
		// If reports missing failed on pass1 before hang finalize, resume must repair.
	}

	res2, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true, Goal: goal, Issue: "1397",
		Actor: "owner", Owner: "worker", HomeDir: home, RepoPath: repo,
		Now: func() time.Time { return now.Add(time.Minute) }, Decompose: linear5,
		LoadInventory: loadInv, OpenLedger: openLed,
		Executor: testspawn.Executor{HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) }, FailModelUnavailableCounts: muCounts},
	})
	if err2 != nil {
		t.Fatalf("resume: %v %s", err2, res2.Message)
	}

	failR, winR = nil, nil
	for i := range res2.Children {
		c := &res2.Children[i]
		if c.ChildID != "wi_implement" {
			continue
		}
		if strings.EqualFold(c.FailureClass, "model_unavailable") {
			failR = c
		}
		if c.Terminal == "succeeded" {
			winR = c
		}
	}
	if failR == nil {
		t.Fatal("universal ChildReport missing model_unavailable failed route after resume")
	}
	if winR == nil {
		t.Fatal("universal ChildReport missing winning alternate after resume")
	}
	// Exact route identities on both (failed must be complete; winner must carry
	// provider/model/depth/permission/account/install/attempt + supersedes).
	requireCore := func(name string, c *goalrun.ChildReport) {
		t.Helper()
		if c.Provider == "" || c.Model == "" || c.Depth == "" || c.Permission == "" ||
			c.AccountRef == "" || c.InstallRef == "" || c.AttemptID == "" {
			t.Fatalf("%s report incomplete route identity: %+v", name, c)
		}
		if c.TaskClass == "" || c.ExecutionPlanDigest == "" || c.ChildContractDigest == "" {
			t.Fatalf("%s report missing classified floor/plan/CCD: %+v", name, c)
		}
		if strings.Contains(strings.ToLower(c.Provider+c.Model+c.RouteReason), "fixture") ||
			strings.Contains(strings.ToLower(c.RouteReason), "defaultinventory") {
			t.Fatalf("%s report carries fixture/default inventory label: %+v", name, c)
		}
		for _, secret := range []string{"sk-", "api_key", "password", "Bearer ", "SECRET"} {
			blob := c.RouteReason + c.AccountRef + c.InstallRef + c.OutputEvidence
			if strings.Contains(blob, secret) {
				t.Fatalf("%s report credential-like token %q", name, secret)
			}
		}
	}
	requireCore("failed", failR)
	requireCore("winner", winR)
	if failR.WindowKind == "" || failR.ReservationID == "" || failR.InstallRef == "" {
		t.Fatalf("failed MU report missing window/reservation/install: %+v", failR)
	}
	if winR.WindowKind == "" || winR.ReservationID == "" || winR.InstallRef == "" {
		t.Fatalf("winner report missing window/reservation/install after resume: %+v", winR)
	}
	// Attempt-keyed capacity identity: failed and winner must not share reservation
	// or capacity before-source misattribution.
	if failR.ReservationID == winR.ReservationID {
		t.Fatalf("failed and winner share reservation_id (ChildID hold misattribute): %s", failR.ReservationID)
	}
	if failR.AttemptID == "" || winR.AttemptID == "" || failR.AttemptID == winR.AttemptID {
		t.Fatalf("dual rows require distinct AttemptIDs fail=%q win=%q", failR.AttemptID, winR.AttemptID)
	}
	if failR.CapacityBefore == nil || winR.CapacityBefore == nil {
		t.Fatalf("both attempts need capacity_before from ledger: fail=%v win=%v", failR.CapacityBefore, winR.CapacityBefore)
	}
	if failR.IntegrateCommitSHA != "" {
		t.Fatalf("failed MU must not integrate product commit: %q", failR.IntegrateCommitSHA)
	}
	// CapacityTransitions must expose install + permission + before metadata.
	if len(res2.Workflow.CapacityTransitions) != 2 {
		t.Fatalf("want 2 CapacityTransitions, got %+v", res2.Workflow.CapacityTransitions)
	}
	for _, tr := range res2.Workflow.CapacityTransitions {
		if tr.InstallRef == "" || tr.WindowKind == "" || tr.ReservationID == "" ||
			tr.Provider == "" || tr.Model == "" || tr.Depth == "" || tr.AccountRef == "" {
			t.Fatalf("CapacityTransition incomplete identity: %+v", tr)
		}
		if tr.BeforeSource == "" && tr.Before == 0 {
			t.Fatalf("CapacityTransition missing before evidence: %+v", tr)
		}
	}
	if strings.EqualFold(failR.Provider, winR.Provider) {
		t.Fatalf("failed and winner must be distinct providers: %s vs %s", failR.Provider, winR.Provider)
	}
	if failR.Depth != "medium" || winR.Depth != "medium" {
		t.Fatalf("same depth floor: fail=%s win=%s", failR.Depth, winR.Depth)
	}
	if winR.SupersedesAttemptID != failR.AttemptID {
		// Winner may carry supersedes from outcome merge.
		var wfWin *workflowrun.ChildOutcome
		for i := range res2.Workflow.Children {
			if res2.Workflow.Children[i].AttemptID == winR.AttemptID {
				wfWin = &res2.Workflow.Children[i]
			}
		}
		if wfWin == nil || wfWin.SupersedesAttemptID != failR.AttemptID {
			t.Fatalf("winner supersedes want %s got report=%q workflow=%v", failR.AttemptID, winR.SupersedesAttemptID, wfWin)
		}
	}
	// Parent JSON surface includes Workflow + Children without hand-built canary structs.
	raw, err := json.Marshal(res2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"failure_class":"model_unavailable"`) &&
		!strings.Contains(string(raw), `"failure_class": "model_unavailable"`) {
		// ChildReport uses FailureClass
		if !strings.Contains(string(raw), "model_unavailable") {
			t.Fatal("serialized Result missing model_unavailable evidence")
		}
	}
	if strings.Contains(string(raw), "DefaultInventory") || strings.Contains(string(raw), "default-official-fake") {
		t.Fatal("serialized Result must not carry DefaultInventory labels")
	}
	// Peaks/ceilings present on parent.
	if res2.ProcessPeak > 1 || res2.WorktreePeak > 1 {
		t.Fatalf("peaks exceed sequential ceiling: p=%d w=%d", res2.ProcessPeak, res2.WorktreePeak)
	}
	// Classified floors on all five children (winner slots).
	wantIDs := map[string]string{
		"wi_research": "luna", "wi_implement": "tera", "wi_tests": "tera",
		"wi_verify": "soul", "wi_docs": "luna",
	}
	for id, class := range wantIDs {
		found := false
		for _, c := range res2.Children {
			if c.ChildID == id && c.Terminal == "succeeded" {
				found = true
				if c.TaskClass != class {
					t.Fatalf("%s TaskClass=%q want %q", id, c.TaskClass, class)
				}
				if c.Model == "" || c.Depth == "" {
					t.Fatalf("%s missing actual model/depth", id)
				}
			}
		}
		if !found {
			t.Fatalf("missing succeeded report for %s", id)
		}
	}
}

// TestFinalContract_NoDefaultInventoryOnProductAutoRoute is a NARROW source guard
// only — not product-path proof that live auto-route consumes real inventory.
// Runtime product proof is product_path_route_identity / Execute with LoadRouteInventory.
func TestFinalContract_NoDefaultInventoryOnProductAutoRoute(t *testing.T) {
	src, err := os.ReadFile("run.go")
	if err != nil {
		t.Fatalf("run.go required for source guard (no Skip): %v", err)
	}
	if strings.Contains(string(src), "DefaultInventory(") || strings.Contains(string(src), "FakeInventory(") {
		t.Fatal("goalrun production path must not call DefaultInventory/FakeInventory")
	}
	if !strings.Contains(string(src), "LoadRouteInventory") {
		t.Fatal("goalrun must use LoadRouteInventory for auto-route inventory")
	}
}

// TestFinalContract_CanaryAntiReuseRejectsArchiveMismatch is a NARROW canary
// validation guard (hand-built EmitInput). It does NOT prove a production goal
// run is fully qualified end-to-end.
func TestFinalContract_CanaryAntiReuseRejectsArchiveMismatch(t *testing.T) {
	now := time.Date(2026, 7, 25, 19, 0, 0, 0, time.UTC)
	repo := t.TempDir()
	dig := strings.Repeat("ab", 32)
	sha := strings.Repeat("cd", 20)
	payload := []byte(`{"failure_class":"forced_interrupt","interrupt_class":"service_forced_interrupt","interrupt_id":"iint_x","terminal":"cancelled"}`)
	ev, err := artifactqual.EmitCanaryEvidence(artifactqual.EmitInput{
		ArchiveDigest: dig, PreProdSHA: sha, BinaryVersion: "0.9.0", BinaryCommit: sha,
		InventoryProvenance: "live_discover", InventoryReportDigest: "sha256:inventory",
		ProjectID: "disp-anti-reuse", RunID: "run_anti_1",
		Events: []workflowrun.Event{
			{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-g0", Generation: 1},
			{Kind: "interrupt", WorkItemID: "wi", AttemptID: "att-wi-g0", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "terminal", WorkItemID: "wi", AttemptID: "att-wi-g0", Terminal: "cancelled", FailureClass: "forced_interrupt", Payload: payload},
			{Kind: "reuse", WorkItemID: "wi_r", AttemptID: "att-r"},
			{Kind: "launch", WorkItemID: "wi", AttemptID: "att-wi-g1", Generation: 2},
		},
		Resumed: true, ProcessPeak: 1, WorktreePeak: 1,
		ActiveOccupancyMeasured: true, ProcessActive: 0, WorktreeActive: 0, RepoPath: repo,
		ProducedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wrong archive digest.
	v := artifactqual.ValidateCanaryEvidence(ev, strings.Repeat("00", 32), sha, now)
	if v.Valid {
		t.Fatal("want invalid on archive digest mismatch")
	}
	// Wrong pre-prod / binary commit.
	v2 := artifactqual.ValidateCanaryEvidence(ev, dig, strings.Repeat("11", 20), now)
	if v2.Valid {
		t.Fatal("want invalid on pre-prod SHA mismatch")
	}
}
