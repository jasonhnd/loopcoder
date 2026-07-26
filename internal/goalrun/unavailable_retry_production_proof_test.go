package goalrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
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
	"github.com/jasonhnd/loopcoder/internal/quotapolicy"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestGoalrunModelUnavailable_UnavailableRetryProductionProof is the A7-7
// production-path proof:
//
//   - fresh disposable git repo + HomeDir
//   - real testspawn process spawns (PID) for every child launch
//   - fresh two-provider decision set (codex + grok), same medium depth floor
//   - normal 5-child linear graph (MaxParallel=1)
//   - middle wi_implement: primary launches then typed model_unavailable;
//     same-depth alternate launches once and succeeds
//   - middle wi_tests: forced interrupt after hang; fresh Resume completes
//     tests g1 + verify + docs (so canary Restart section is present)
//   - EmitCanaryFromResult reads authoritative event log + capacity transitions
//   - ValidateCanaryEvidence UnavailableRetryOK=true; RestartOK=true when measured
//
// No DefaultInventory, FakeChildExecutor success-as-proof, planned-only
// excludes, hand-written RouteExcludes, or synthetic Event slices.
func TestGoalrunModelUnavailable_UnavailableRetryProductionProof(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	home := testHome(t)
	repo := initDisposableGitRepo(t)
	ledgerPath := filepath.Join(t.TempDir(), "capacity-ledger.json")
	projectID := "proj-a77-mu-proof"
	runID := "run_a77_mu_proof_1"
	goal := "implement unavailable route retry production proof with tests verification and operator guide"

	// Exact-routable full account refs; two distinct provider companies.
	acctCodex := fullAcct("a77-codex-billing")
	acctGrok := fullAcct("a77-grok-billing")
	instCodex, instGrok := "install-codex-a77", "install-grok-a77"

	mkAcc := func(provider, model, acct, inst string, depths []string, rem float64) capacitysnapshot.AccountObservation {
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
				ModelID: model, SupportedDepths: append([]string(nil), depths...), DefaultDepth: "medium", Present: true,
			}},
			Source: "test-machine-observed", CapturedAt: now,
		})
	}
	// codex: low/medium/high; grok: medium only (distinct company, same medium floor).
	snap, err := capacitysnapshot.Build([]capacitysnapshot.AccountObservation{
		mkAcc("codex", "gpt-5.5", acctCodex, instCodex, []string{"low", "medium", "high"}, 90),
		mkAcc("grok", "grok-4.5", acctGrok, instGrok, []string{"medium"}, 90),
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
	var softs []quotapolicy.Candidate
	type row struct {
		prov, model, acct, inst string
		depths                  []string
		class                   capclass.Class
	}
	for _, p := range []row{
		{"codex", "gpt-5.5", capacityledger.CanonicalAccountRef(acctCodex), instCodex, []string{"low", "medium", "high"}, capclass.ClassSoul},
		{"grok", "grok-4.5", capacityledger.CanonicalAccountRef(acctGrok), instGrok, []string{"medium"}, capclass.ClassSoul},
	} {
		for _, depth := range p.depths {
			for _, perm := range []string{"read-only", "bounded_write"} {
				cands = append(cands, eligibility.Candidate{
					Provider: p.prov, Model: p.model, Effort: depth, Permission: perm,
					ModelClass: p.class, AccountRef: p.acct, InstallRef: p.inst, WindowKind: "five_hour",
					Installed: okF(p.prov + "-i"), Authenticated: okF(p.prov + "-a"), ModelPresent: okF(p.prov + "-m"),
					PermissionOK: okF(p.prov + "-p"), EffortOK: okF(p.prov + "-e"), Healthy: okF(p.prov + "-h"),
					CooldownActive: ff(p.prov + "-cd"), ResourceFit: okF(p.prov + "-r"), QuotaRemaining: 9999,
				})
			}
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
		CapacityOK:      eligibility.Fact{State: eligibility.FactTrue, EvidenceID: "mach", Freshness: eligibility.FreshFresh},
		ConcurrentSlots: 4,
	}
	inv.EvidenceDigest = "a77-mu-two-provider-fresh"

	linear5 := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_a77_mu", Version: 1,
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
				{Schema: workgraph.SchemaItem, ID: "wi_tests", Status: workgraph.ItemRequired,
					Intent: "tests", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 3, OutputContract: "test_pass",
					RouteRequirement: "class=tera,depth=medium,permission=bounded_write"},
				{Schema: workgraph.SchemaItem, ID: "wi_verify", Status: workgraph.ItemRequired,
					Intent: "verify", Owner: "verifier", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 4, OutputContract: "verification_verdict",
					RouteRequirement: "class=soul,depth=high,permission=read-only"},
				{Schema: workgraph.SchemaItem, ID: "wi_docs", Status: workgraph.ItemRequired,
					Intent: "operator guide", Owner: "worker", Ownership: workgraph.OwnLoopCoderWorkItem,
					IntegrationOrder: 5, OutputContract: "docs_diff",
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

	// Pass 1: MU once on wi_implement; hang-interrupt on wi_tests.
	muCounts := map[string]int{}
	calls1 := map[string]int{}
	ctx1, cancel1 := context.WithCancel(context.Background())
	res1, err1 := Execute(ctx1, Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		InventoryProvenance: InventoryProvenanceLiveDiscover,
		// Auto-route so both providers participate in decision set / same-depth alts.
		HomeDir: home, RepoPath: repo, Now: func() time.Time { return now },
		Decompose:     linear5,
		LoadInventory: loadInv, OpenLedger: openLed,
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls:                       calls1,
			FailModelUnavailableOnceIDs: map[string]bool{"wi_implement": true},
			FailModelUnavailableCounts:  muCounts,
			HangIDs:                     map[string]bool{"wi_tests": true},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == "wi_tests" && pid > 0 {
					cancel1()
				}
			},
		},
	})
	if err1 == nil && res1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("expected interrupt after MU+alt on implement: status=%s msg=%s children=%+v",
			res1.Status, res1.Message, res1.Workflow.Children)
	}

	// --- Contract 1–3: MU failed + alternate success on wi_implement ---
	var muFailed, muOK *workflowrun.ChildOutcome
	for i := range res1.Workflow.Children {
		c := &res1.Workflow.Children[i]
		if c.WorkItemID != "wi_implement" {
			continue
		}
		if strings.EqualFold(c.FailureClass, "model_unavailable") {
			muFailed = c
		}
		if c.Terminal == "succeeded" {
			muOK = c
		}
	}
	if muFailed == nil || muOK == nil {
		t.Fatalf("want MU failed + alternate success on wi_implement: children=%+v calls=%v err=%v msg=%s",
			res1.Workflow.Children, calls1, err1, res1.Message)
	}
	if muFailed.AttemptID == "" || muOK.AttemptID == "" || muFailed.AttemptID == muOK.AttemptID {
		t.Fatalf("distinct attempts required: fail=%s ok=%s", muFailed.AttemptID, muOK.AttemptID)
	}
	if muOK.SupersedesAttemptID != muFailed.AttemptID {
		t.Fatalf("supersedes=%q want %q", muOK.SupersedesAttemptID, muFailed.AttemptID)
	}
	if strings.EqualFold(muFailed.Provider, muOK.Provider) {
		t.Fatalf("alternate must be distinct provider company: fail=%s/%s ok=%s/%s",
			muFailed.Provider, muFailed.Model, muOK.Provider, muOK.Model)
	}
	if muFailed.Depth != "medium" || muOK.Depth != "medium" {
		t.Fatalf("same depth floor medium required: fail=%s ok=%s", muFailed.Depth, muOK.Depth)
	}
	if muFailed.Permission == "" || muOK.Permission == "" ||
		!strings.EqualFold(muFailed.Permission, muOK.Permission) {
		t.Fatalf("permission floor: fail=%q ok=%q", muFailed.Permission, muOK.Permission)
	}
	if muFailed.AccountRef == "" || muOK.AccountRef == "" || muFailed.AccountRef == muOK.AccountRef {
		t.Fatalf("distinct account identity: fail=%q ok=%q", muFailed.AccountRef, muOK.AccountRef)
	}
	if muFailed.InstallRef == "" || muOK.InstallRef == "" || muFailed.InstallRef == muOK.InstallRef {
		t.Fatalf("distinct install identity: fail=%q ok=%q", muFailed.InstallRef, muOK.InstallRef)
	}
	if muFailed.WindowKind == "" || muOK.WindowKind != muFailed.WindowKind {
		t.Fatalf("window: fail=%q ok=%q", muFailed.WindowKind, muOK.WindowKind)
	}
	// Real PIDs proven via durable pid events below (ChildOutcome has no ProcessPID field).
	if muFailed.IntegrateCommitSHA != "" {
		t.Fatalf("primary must not integrate: %s", muFailed.IntegrateCommitSHA)
	}
	if muOK.IntegrateCommitSHA == "" && len(res1.Workflow.IntegrateCommits) == 0 {
		// May integrate only after full success; implement should have integrated before tests hang.
		// Check integrate events / commits for succeeded attempt.
	}
	// Implement alternate must have integrated product (before tests hang).
	implIntegrated := false
	for _, ic := range res1.Workflow.IntegrateCommits {
		if ic.AttemptID == muOK.AttemptID && ic.CommitSHA != "" {
			implIntegrated = true
		}
		if ic.AttemptID == muFailed.AttemptID {
			t.Fatalf("failed attempt must not appear in IntegrateCommits: %+v", ic)
		}
	}
	if !implIntegrated {
		// Fall back to outcome IntegrateCommitSHA
		if strings.TrimSpace(muOK.IntegrateCommitSHA) == "" {
			t.Fatalf("alternate implement must integrate: commits=%+v ok=%+v", res1.Workflow.IntegrateCommits, muOK)
		}
	}

	// Capacity: prior released/reconciled once; alternate unique reservation.
	led, err := capacityledger.OpenPath(ledgerPath, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	eFail, okFail := led.Get(projectID, runID, muFailed.AttemptID)
	eOK, okAlt := led.Get(projectID, runID, muOK.AttemptID)
	if !okFail || !okAlt {
		t.Fatalf("ledger missing fail=%v ok=%v", okFail, okAlt)
	}
	if eFail.State != "released" && eFail.State != "reconciled" {
		t.Fatalf("primary reservation state=%q want released|reconciled", eFail.State)
	}
	if eFail.ReservationID == "" || eOK.ReservationID == "" || eFail.ReservationID == eOK.ReservationID {
		t.Fatalf("unique reservations fail=%s ok=%s", eFail.ReservationID, eOK.ReservationID)
	}
	// Never reopen primary: second Get after later resume still same state.
	priorFailState, priorFailRes := eFail.State, eFail.ReservationID

	// Event log: exactly one MU, one reroute; claim/launch/terminal counts.
	if res1.Workflow.EventLogPath == "" {
		t.Fatal("event log path required")
	}
	raw1, err := os.ReadFile(res1.Workflow.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	events1, err := workflowrun.ParseEventJSONLStrict(string(raw1), projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	countKindAtt := func(events []workflowrun.Event, kind, att string) int {
		n := 0
		for _, e := range events {
			if e.Kind == kind && e.AttemptID == att {
				n++
			}
		}
		return n
	}
	if countKindAtt(events1, "model_unavailable", muFailed.AttemptID) != 1 {
		t.Fatalf("MU events on failed attempt want 1")
	}
	if countKindAtt(events1, "claim", muFailed.AttemptID) != 1 || countKindAtt(events1, "launch", muFailed.AttemptID) != 1 {
		t.Fatalf("primary claim/launch want 1 each")
	}
	if countKindAtt(events1, "terminal", muFailed.AttemptID) != 1 {
		t.Fatalf("primary terminal want 1")
	}
	if countKindAtt(events1, "integrate", muFailed.AttemptID) != 0 {
		t.Fatal("primary must not integrate")
	}
	if countKindAtt(events1, "claim", muOK.AttemptID) != 1 || countKindAtt(events1, "launch", muOK.AttemptID) != 1 {
		t.Fatalf("alternate claim/launch want 1")
	}
	if countKindAtt(events1, "reroute", muOK.AttemptID) != 1 {
		t.Fatal("want exactly one reroute on alternate attempt")
	}
	if countKindAtt(events1, "terminal", muOK.AttemptID) != 1 {
		t.Fatal("alternate terminal want 1")
	}
	// PID events for both attempts.
	pidFail, pidOK := 0, 0
	for _, e := range events1 {
		if e.Kind != "pid" {
			continue
		}
		if e.AttemptID == muFailed.AttemptID && e.PID > 0 {
			pidFail++
		}
		if e.AttemptID == muOK.AttemptID && e.PID > 0 {
			pidOK++
		}
	}
	if pidFail < 1 || pidOK < 1 {
		t.Fatalf("PID events fail=%d ok=%d", pidFail, pidOK)
	}

	// Forced interrupt on wi_tests present.
	testsInterrupted := false
	var testsG0 string
	for _, e := range events1 {
		if e.WorkItemID == "wi_tests" && e.Kind == "interrupt" {
			testsInterrupted = true
			testsG0 = e.AttemptID
		}
		if e.WorkItemID == "wi_tests" && e.Kind == "launch" && testsG0 == "" {
			testsG0 = e.AttemptID
		}
	}
	if !testsInterrupted || testsG0 == "" {
		t.Fatalf("want forced interrupt on wi_tests; g0=%q interrupted=%v", testsG0, testsInterrupted)
	}

	// Capacity transitions for MU path (prior+alternate).
	if len(res1.Workflow.CapacityTransitions) < 2 {
		t.Fatalf("want prior+alternate capacity transitions, got %d", len(res1.Workflow.CapacityTransitions))
	}

	// --- Pass 2: fresh Resume completes remaining children ---
	calls2 := map[string]int{}
	canaryOut := filepath.Join(t.TempDir(), "canary_a77.json")
	archiveDig := strings.Repeat("ef", 32)
	preProdSHA := strings.Repeat("ab", 20)
	res2, err2 := Execute(context.Background(), Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397", Actor: "owner", Owner: "worker",
		InventoryProvenance: InventoryProvenanceLiveDiscover,
		HomeDir:             home, RepoPath: repo, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     linear5,
		LoadInventory: loadInv, OpenLedger: openLed,
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls:                      calls2,
			FailModelUnavailableCounts: muCounts, // no once-IDs → no MU re-fail
		},
		CanaryEmit: &CanaryEmitOptions{
			OutPath: canaryOut, HomeDir: home, RepoPath: repo,
			ArchiveDigest: archiveDig, PreProdSHA: preProdSHA,
			BinaryVersion: "0.9.0-a77", BinaryCommit: preProdSHA,
			InventoryProvenance: InventoryProvenanceLiveDiscover,
		},
	})
	if err2 != nil {
		t.Fatalf("resume: %v status=%s msg=%s", err2, res2.Status, res2.Message)
	}
	if !res2.Resumed {
		t.Fatal("expected Resumed=true")
	}
	// Prior success not re-exec; implement not re-launched.
	if calls2["wi_research"] != 0 || calls2["wi_implement"] != 0 {
		t.Fatalf("prior success re-exec: %+v", calls2)
	}
	if calls2["wi_tests"] != 1 {
		t.Fatalf("tests resume calls=%d want 1", calls2["wi_tests"])
	}

	// Primary MU reservation still immutable after resume.
	eFail2, ok := led.Get(projectID, runID, muFailed.AttemptID)
	if !ok || eFail2.State != priorFailState || eFail2.ReservationID != priorFailRes {
		t.Fatalf("primary reservation reopened/mutated: before state=%s res=%s after=%+v",
			priorFailState, priorFailRes, eFail2)
	}

	// Peaks / occupancy.
	procPeak := res2.ProcessPeak
	if procPeak < 1 {
		procPeak = res2.Workflow.ProcessPeak
	}
	wtPeak := res2.WorktreePeak
	if wtPeak < 1 {
		wtPeak = res2.Workflow.WorktreePeak
	}
	if procPeak > 1 || wtPeak > 1 {
		t.Fatalf("peaks over sequential ceiling: process=%d worktree=%d", procPeak, wtPeak)
	}
	if res2.Workflow.ProcessActive != 0 || res2.Workflow.WorktreeActive != 0 {
		t.Fatalf("active occupancy non-zero: p=%d w=%d", res2.Workflow.ProcessActive, res2.Workflow.WorktreeActive)
	}

	// Child reports retain both failed and winning route reasons (no credentials).
	var repOK *ChildReport
	for i := range res2.Children {
		if res2.Children[i].ChildID == "wi_implement" && res2.Children[i].Terminal == "succeeded" {
			repOK = &res2.Children[i]
		}
	}
	if repOK == nil {
		t.Fatalf("ChildReport missing implement success: %+v", res2.Children)
	}
	if !strings.Contains(strings.ToLower(repOK.RouteReason+muOK.RouteReason+muFailed.RouteReason), "model_unavailable") &&
		!strings.Contains(strings.ToLower(muOK.RouteReason), "reroute") {
		// Winning route reason should mention reroute / alternate.
		if !strings.Contains(strings.ToLower(muOK.RouteReason), "reroute") &&
			!strings.Contains(strings.ToLower(muOK.RouteReason), "model_unavailable") {
			t.Fatalf("route reasons missing MU/reroute evidence: report=%q ok=%q fail=%q",
				repOK.RouteReason, muOK.RouteReason, muFailed.RouteReason)
		}
	}
	blob := repOK.RouteReason + muOK.RouteReason + muFailed.RouteReason + eFail.RouteReason + eOK.RouteReason
	for _, secret := range []string{"sk-", "password", "api_key", "Bearer "} {
		if strings.Contains(blob, secret) {
			t.Fatalf("credential-like token in reports: %s", secret)
		}
	}

	// Claimed model_unavailable exclude present (production recordModelUnavailableExcludes).
	claimedMU := 0
	for _, ex := range res2.RouteExcludes {
		if ex.Claimed && strings.EqualFold(ex.Reason, "model_unavailable") {
			claimedMU++
			if ex.Provider != muFailed.Provider {
				t.Fatalf("claimed exclude provider=%s want %s", ex.Provider, muFailed.Provider)
			}
			if ex.ChildID != "wi_implement" {
				t.Fatalf("claimed exclude child=%s", ex.ChildID)
			}
		}
		// eligible_not_chosen must not be the sole unavailable path.
		if strings.EqualFold(ex.Reason, "eligible_not_chosen") && ex.Claimed {
			t.Fatal("eligible_not_chosen must never be claimed unavailable")
		}
	}
	if claimedMU != 1 {
		t.Fatalf("want exactly one claimed model_unavailable exclude, got %d excludes=%+v", claimedMU, res2.RouteExcludes)
	}

	// --- Canary emit + Validate UnavailableRetryOK ---
	if res2.CanaryEvidencePath == "" {
		t.Fatalf("CanaryEvidencePath empty; msg=%s", res2.Message)
	}
	rawCanary, err := os.ReadFile(res2.CanaryEvidencePath)
	if err != nil {
		t.Fatal(err)
	}
	var cev artifactqual.CanaryEvidence
	if err := json.Unmarshal(rawCanary, &cev); err != nil {
		t.Fatal(err)
	}
	if cev.UnavailableRetry == nil {
		t.Fatalf("UnavailableRetry nil; children=%d transitions=%d excludes=%+v",
			len(res2.Workflow.Children), len(res2.Workflow.CapacityTransitions), res2.RouteExcludes)
	}
	u := cev.UnavailableRetry
	if !strings.EqualFold(u.ExcludedReason, "model_unavailable") {
		t.Fatalf("ExcludedReason=%q", u.ExcludedReason)
	}
	if u.ExcludedProvider != muFailed.Provider {
		t.Fatalf("ExcludedProvider=%s want %s", u.ExcludedProvider, muFailed.Provider)
	}
	if !u.NoDuplicateClaim || !u.NoDuplicateFiles || !u.NoDoubleCapacity {
		t.Fatalf("no-dup flags: %+v", u)
	}
	if strings.TrimSpace(u.EvidenceRef) == "" || strings.Contains(strings.ToLower(u.EvidenceRef), "pending") {
		t.Fatalf("EvidenceRef=%q", u.EvidenceRef)
	}
	if strings.EqualFold(u.ExcludedReason, "eligible_not_chosen") {
		t.Fatal("eligible_not_chosen must never count as unavailable")
	}

	// Restart section present from middle-child interrupt+resume (preferred path).
	if cev.Restart == nil {
		t.Fatal("Restart section required for combined MU+interrupt run (prefer full eligibility)")
	}
	if !cev.Restart.Interrupted || !cev.Restart.ResumedFromDurable {
		t.Fatalf("Restart flags: %+v", cev.Restart)
	}

	v := artifactqual.ValidateCanaryEvidence(cev, archiveDig, preProdSHA, now.Add(2*time.Minute))
	if !v.UnavailableRetryOK {
		t.Fatalf("UnavailableRetryOK=false reasons=%v unavailable=%+v", v.Reasons, u)
	}
	if !v.RestartOK {
		t.Fatalf("RestartOK must be true for combined MU+interrupt production proof; reasons=%v restart=%+v", v.Reasons, cev.Restart)
	}
	if !v.UnavailableRetryOK {
		t.Fatalf("UnavailableRetryOK must be true; reasons=%v", v.Reasons)
	}

	// --- Proof / merge mutation fail-closed ---
	events2, err := workflowrun.ParseEventJSONLStrict(string(mustRead(t, res2.Workflow.EventLogPath)), projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	proof := ProofFromResultForTest(res2, events2)
	if proof == nil || !proof.ValidForClaimedModelUnavailable() {
		t.Fatalf("production proofFromResult nil/invalid; transitions=%d children=%d",
			len(res2.Workflow.CapacityTransitions), len(res2.Workflow.Children))
	}
	// Event loss: drop model_unavailable → proof fails.
	mut := make([]workflowrun.Event, 0, len(events2))
	for _, e := range events2 {
		if e.Kind == "model_unavailable" && e.AttemptID == muFailed.AttemptID {
			continue
		}
		mut = append(mut, e)
	}
	if p2 := ProofFromResultForTest(res2, mut); p2 != nil && p2.ValidForClaimedModelUnavailable() {
		t.Fatal("proof must fail closed after removing model_unavailable event")
	}
	// Supersedes link loss.
	resMut := res2
	resMut.Workflow.Children = append([]workflowrun.ChildOutcome(nil), res2.Workflow.Children...)
	for i := range resMut.Workflow.Children {
		if resMut.Workflow.Children[i].AttemptID == muOK.AttemptID {
			resMut.Workflow.Children[i].SupersedesAttemptID = "att-forged-g9"
		}
	}
	if p3 := ProofFromResultForTest(resMut, events2); p3 != nil && p3.ValidForClaimedModelUnavailable() {
		t.Fatal("proof must fail closed after supersedes mutation")
	}
	// Transition loss.
	resMut2 := res2
	resMut2.Workflow.CapacityTransitions = nil
	if p4 := ProofFromResultForTest(resMut2, events2); p4 != nil && p4.ValidForClaimedModelUnavailable() {
		t.Fatal("proof must fail closed without capacity transitions")
	}
	// --- Durable merge mutations (real JSONL + checkpoint) ---
	cpOK, _, err := LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(cpOK.EventLogPath) == "" {
		t.Fatal("checkpoint EventLogPath must be nonempty after production run")
	}
	if strings.TrimSpace(cpOK.GraphID) == "" {
		t.Fatal("checkpoint GraphID must be nonempty")
	}
	ledMerge, err := capacityledger.OpenPath(ledgerPath, func() time.Time { return now.Add(3 * time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	baseMergeRes := func() Result {
		r := res2
		r.Workflow.Children = append([]workflowrun.ChildOutcome(nil), res2.Workflow.Children...)
		var stripped []workflowrun.ChildOutcome
		for _, c := range r.Workflow.Children {
			if strings.EqualFold(c.FailureClass, "model_unavailable") {
				continue
			}
			stripped = append(stripped, c)
		}
		r.Workflow.Children = stripped
		r.Workflow.CapacityTransitions = nil
		return r
	}
	mergeCall := func(cp Checkpoint, r Result, plan, graph, gid string, gver int) error {
		return mergeDurableModelUnavailableOnResume(
			&r, cp, ledMerge, projectID, runID, plan, graph, gid, gver,
		)
	}
	// Copy authoritative JSONL to temp existing path for mutation tests.
	writeMutatedJSONL := func(t *testing.T, mutator func([]workflowrun.Event) []workflowrun.Event) string {
		t.Helper()
		raw := mustRead(t, res2.Workflow.EventLogPath)
		evs, err := workflowrun.ParseEventJSONLStrict(string(raw), projectID, runID)
		if err != nil {
			t.Fatal(err)
		}
		evs = mutator(evs)
		tmp := filepath.Join(t.TempDir(), "workflow-events-mut.jsonl")
		var b strings.Builder
		for _, e := range evs {
			line, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(line)
			b.WriteByte('\n')
		}
		if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return tmp
	}
	// (1) Empty checkpoint EventLogPath.
	cpEmptyPath := cpOK
	cpEmptyPath.EventLogPath = ""
	if merr := mergeCall(cpEmptyPath, baseMergeRes(), res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on empty checkpoint EventLogPath")
	}
	// (2) Empty / forged checkpoint GraphID.
	cpEmptyGID := cpOK
	cpEmptyGID.GraphID = ""
	if merr := mergeCall(cpEmptyGID, baseMergeRes(), res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on empty checkpoint GraphID")
	}
	cpForgedGID := cpOK
	cpForgedGID.GraphID = "g_forged_not_current"
	if merr := mergeCall(cpForgedGID, baseMergeRes(), res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on forged checkpoint GraphID")
	}
	// (3) Real JSONL: forge nonempty reroute model_unavailable_event_id / claim_event_id.
	// Point BOTH checkpoint and result EventLogPath at the mutated copy.
	forgeRerouteField := func(field, forged string) {
		t.Helper()
		path := writeMutatedJSONL(t, func(evs []workflowrun.Event) []workflowrun.Event {
			for i := range evs {
				if evs[i].Kind != "reroute" || evs[i].AttemptID != muOK.AttemptID {
					continue
				}
				var m map[string]string
				if err := json.Unmarshal(evs[i].Payload, &m); err != nil {
					t.Fatal(err)
				}
				m[field] = forged
				raw, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				evs[i].Payload = raw
			}
			return evs
		})
		cpM := cpOK
		cpM.EventLogPath = path
		rM := baseMergeRes()
		rM.Workflow.EventLogPath = path
		if merr := mergeCall(cpM, rM, res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
			t.Fatalf("merge must fail closed on forged reroute %s", field)
		}
	}
	forgeRerouteField("model_unavailable_event_id", "wev_forged_mu_event_id_nonempty")
	forgeRerouteField("claim_event_id", "wev_forged_claim_event_id_nonempty")
	// Empty required EventIDs on MU / claim events.
	pathEmptyMU := writeMutatedJSONL(t, func(evs []workflowrun.Event) []workflowrun.Event {
		for i := range evs {
			if evs[i].Kind == "model_unavailable" && evs[i].AttemptID == muFailed.AttemptID {
				evs[i].EventID = ""
			}
		}
		return evs
	})
	cpE := cpOK
	cpE.EventLogPath = pathEmptyMU
	rE := baseMergeRes()
	rE.Workflow.EventLogPath = pathEmptyMU
	if merr := mergeCall(cpE, rE, res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on empty MU event_id")
	}
	// Duplicate EventID across two required pair events.
	pathDup := writeMutatedJSONL(t, func(evs []workflowrun.Event) []workflowrun.Event {
		var muID string
		for i := range evs {
			if evs[i].Kind == "model_unavailable" && evs[i].AttemptID == muFailed.AttemptID {
				muID = evs[i].EventID
			}
		}
		for i := range evs {
			if evs[i].Kind == "claim" && evs[i].AttemptID == muOK.AttemptID {
				evs[i].EventID = muID // duplicate
			}
		}
		return evs
	})
	cpD := cpOK
	cpD.EventLogPath = pathDup
	rD := baseMergeRes()
	rD.Workflow.EventLogPath = pathDup
	if merr := mergeCall(cpD, rD, res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on duplicate event_id")
	}
	// Real JSONL graph_digest / graph_id / graph_version mutations (not just current param).
	pathGD := writeMutatedJSONL(t, func(evs []workflowrun.Event) []workflowrun.Event {
		for i := range evs {
			if (evs[i].AttemptID == muFailed.AttemptID || evs[i].AttemptID == muOK.AttemptID) &&
				(evs[i].Kind == "model_unavailable" || evs[i].Kind == "reroute" || evs[i].Kind == "claim") {
				evs[i].GraphDigest = "sha256:" + strings.Repeat("aa", 16)
			}
		}
		return evs
	})
	cpGD := cpOK
	cpGD.EventLogPath = pathGD
	rGD := baseMergeRes()
	rGD.Workflow.EventLogPath = pathGD
	if merr := mergeCall(cpGD, rGD, res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on forged event graph_digest in JSONL")
	}
	pathGID := writeMutatedJSONL(t, func(evs []workflowrun.Event) []workflowrun.Event {
		for i := range evs {
			if evs[i].AttemptID == muOK.AttemptID && evs[i].Kind == "reroute" {
				evs[i].GraphID = "g_forged_event"
			}
		}
		return evs
	})
	cpGID := cpOK
	cpGID.EventLogPath = pathGID
	rGID := baseMergeRes()
	rGID.Workflow.EventLogPath = pathGID
	if merr := mergeCall(cpGID, rGID, res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on forged event graph_id in JSONL")
	}
	pathGVer := writeMutatedJSONL(t, func(evs []workflowrun.Event) []workflowrun.Event {
		for i := range evs {
			if evs[i].AttemptID == muOK.AttemptID && evs[i].Kind == "claim" {
				evs[i].GraphVersion = 9999
			}
		}
		return evs
	})
	cpGV := cpOK
	cpGV.EventLogPath = pathGVer
	rGV := baseMergeRes()
	rGV.Workflow.EventLogPath = pathGVer
	if merr := mergeCall(cpGV, rGV, res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on forged event graph_version in JSONL")
	}
	// Checkpoint CCD tamper.
	cpTamper := cpOK
	cpTamper.WorkflowKids = append([]workflowrun.ChildOutcome(nil), cpOK.WorkflowKids...)
	for i := range cpTamper.WorkflowKids {
		if strings.EqualFold(cpTamper.WorkflowKids[i].FailureClass, "model_unavailable") {
			cpTamper.WorkflowKids[i].ChildContractDigest = "sha256:" + strings.Repeat("00", 32)
			break
		}
	}
	if merr := mergeCall(cpTamper, baseMergeRes(), res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on checkpoint CCD tamper")
	}
	// Whole-struct Message conflict (DeepEqual).
	resConflict := baseMergeRes()
	for i := range resConflict.Workflow.Children {
		if resConflict.Workflow.Children[i].AttemptID == muOK.AttemptID {
			resConflict.Workflow.Children[i].Message = "forged-message-for-deep-equal-conflict"
		}
	}
	for _, c := range res2.Workflow.Children {
		if strings.EqualFold(c.FailureClass, "model_unavailable") {
			resConflict.Workflow.Children = append(resConflict.Workflow.Children, c)
		}
	}
	if merr := mergeCall(cpOK, resConflict, res2.PlanDigest, res2.GraphDigest, res2.GraphID, res2.Workflow.GraphVersion); merr == nil {
		t.Fatal("merge must fail closed on whole-struct Message conflict (DeepEqual)")
	}
	// Exact-once integrate: exactly one IntegrateCommit + exactly one integrate event, same SHA; failed zero.
	wantSHA := strings.TrimSpace(muOK.IntegrateCommitSHA)
	if wantSHA == "" {
		t.Fatal("winner IntegrateCommitSHA empty")
	}
	intCommitN, intEventN, failedInt := 0, 0, 0
	for _, ic := range res2.Workflow.IntegrateCommits {
		if ic.AttemptID == muOK.AttemptID {
			if strings.TrimSpace(ic.CommitSHA) != wantSHA {
				t.Fatalf("integrate commit SHA mismatch row=%s want=%s", ic.CommitSHA, wantSHA)
			}
			intCommitN++
		}
		if ic.AttemptID == muFailed.AttemptID {
			failedInt++
		}
	}
	for _, e := range events2 {
		if e.Kind == "integrate" && e.AttemptID == muOK.AttemptID {
			if strings.TrimSpace(e.CommitSHA) != wantSHA {
				t.Fatalf("integrate event SHA mismatch %s want %s", e.CommitSHA, wantSHA)
			}
			intEventN++
		}
		if e.Kind == "integrate" && e.AttemptID == muFailed.AttemptID {
			failedInt++
		}
	}
	if intCommitN != 1 || intEventN != 1 {
		t.Fatalf("exactly-once integrate: commits=%d events=%d want 1/1 sha=%s", intCommitN, intEventN, wantSHA)
	}
	if failedInt != 0 {
		t.Fatalf("failed attempt integrate rows/events=%d want 0", failedInt)
	}
	// Physical: if failed worktree still exists, git status errors fail; porcelain must be clean.
	failedWT := ""
	for _, c := range res2.Workflow.Children {
		if c.AttemptID == muFailed.AttemptID {
			failedWT = c.WorktreePath
		}
	}
	if failedWT != "" {
		if st, err := os.Stat(failedWT); err == nil && st.IsDir() {
			cmd := exec.Command("git", "-C", failedWT, "status", "--porcelain", "--untracked-files=all")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("failed MU worktree git status error (must fail closed): %v %s", err, out)
			}
			if strings.TrimSpace(string(out)) != "" {
				t.Fatalf("failed MU worktree still dirty after scrub:\n%s", out)
			}
		}
	}
	// eligible_not_chosen never greens unavailable.
	bogus := BuildUnavailableRetryEvidence([]RouteExclude{{
		ChildID: "wi_implement", Provider: "codex", Reason: "eligible_not_chosen",
		HardEligible: true, Claimed: false,
	}}, muOK.AttemptID)
	if bogus != nil && strings.EqualFold(bogus.ExcludedReason, "eligible_not_chosen") {
		cevBad := cev
		cevBad.UnavailableRetry = bogus
		vb := artifactqual.ValidateCanaryEvidence(cevBad, archiveDig, preProdSHA, now.Add(2*time.Minute))
		if vb.UnavailableRetryOK {
			t.Fatal("eligible_not_chosen must not set UnavailableRetryOK")
		}
	}

	// Final matrix log for STOP report.
	t.Logf("A7-7 matrix: fail=%s/%s att=%s pid_events=%d res=%s state=%s | alt=%s/%s att=%s pid_events=%d res=%s supersedes=%s | tests_g0=%s reuse=%d peaks=%d/%d UnavailableRetryOK=%v RestartOK=%v Valid=%v",
		muFailed.Provider, muFailed.Model, muFailed.AttemptID, pidFail, eFail.ReservationID, eFail.State,
		muOK.Provider, muOK.Model, muOK.AttemptID, pidOK, eOK.ReservationID, muOK.SupersedesAttemptID,
		testsG0, res2.ReuseCount, procPeak, wtPeak, v.UnavailableRetryOK, v.RestartOK, v.Valid)
	t.Logf("UnavailableRetry: provider=%s reason=%s retry=%s noDupClaim=%v noDupFiles=%v noDupCap=%v ref=%s",
		u.ExcludedProvider, u.ExcludedReason, u.RetryAttemptID, u.NoDuplicateClaim, u.NoDuplicateFiles, u.NoDoubleCapacity, u.EvidenceRef)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Ensure sha helper available (fullAcct lives in mu_cross_store_test.go same package).
var _ = sha256.Sum256
var _ = hex.EncodeToString
