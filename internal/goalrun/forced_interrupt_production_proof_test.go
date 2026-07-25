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
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun/testspawn"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

// TestForcedInterruptProductionProof_KillMidChildResumeEmitCanary is the A7-6B
// end-to-end production proof on the normal 5-child goal graph
// (research → implement → tests → verify → docs):
//
//  1. forced interrupt is a typed durable event (project/run/work-item/attempt/generation)
//  2. a later fresh goalrun.Execute resumes the same run and launches only gN+1
//     for the aborted child; succeeded siblings are not re-executed
//  3. aborted gN never succeeds/integrates; no duplicate launch/success/integration
//  4. ProcessPeak/WorktreePeak from production hooks, active==0, limits<=1
//  5. repo-local runtime measured: fresh lstat proves <repo>/.loopcoder does NOT exist
//  6. CanaryRestart derived via EmitCanaryFromResult; ValidateCanaryEvidence RestartOK==true
//
// Helpers may hang/kill a disposable testspawn child; they must not bypass
// production persistence, recovery, emission, or validator policy.
func TestForcedInterruptProductionProof_KillMidChildResumeEmitCanary(t *testing.T) {
	now := time.Date(2026, 7, 24, 15, 0, 0, 0, time.UTC)
	env := newProductEnv(t, now, "codex")
	home := env.Home
	projectID := "proj-a76b-forced-proof"
	runID := "run_a76b_forced_proof_1"
	repoPath := initDisposableGitRepo(t)
	// Avoid the substring "docs" so looksLikeDocs is false; still materialize the
	// normal 5-node product graph (research → implement → tests → verify → docs).
	goal := "implement forced restart production proof with tests verification and operator guide updates"
	fiveChild := func(opts workgraph.DecomposeOptions) (workgraph.Graph, error) {
		g := workgraph.Graph{
			Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
			GraphID: "g_a76b_five", Version: 1,
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
			// Fully linear chain so MaxParallel=1 is valid (no fan-out) and
			// production sequential ProcessPeak/WorktreePeak stay at ceiling 1.
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

	// --- Pass 1: hang MIDDLE child wi_tests after research+implement succeed ---
	// verify/docs must NOT launch in pass1; resume must gen-bump released g0
	// reservations for unlaunched later siblings (product recovery, not last-node only).
	const hangWI = "wi_tests"
	ctx1, cancel1 := context.WithCancel(context.Background())
	calls1 := map[string]int{}
	var hangPID int
	res1, err1 := goalrun.Execute(ctx1, goalrun.Request{
		ProjectID: projectID, RunID: runID,
		Goal: goal, Issue: "1397",
		Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		InventoryProvenance: goalrun.InventoryProvenanceLiveDiscover,
		HomeDir:             home, RepoPath: repoPath, Now: func() time.Time { return now },
		Decompose:     fiveChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now },
			Calls: calls1, HangIDs: map[string]bool{hangWI: true},
			// Reproduce the production cancellation shape: the runner observed
			// the invocation but returned before stamping permission.
			MutateInterruptedRoute: func(route workflowrun.ChildRoute) workflowrun.ChildRoute {
				route.Permission = ""
				return route
			},
			OnHangEntry: func(workItemID string, pid int) {
				if workItemID == hangWI && pid > 0 {
					hangPID = pid
					cancel1()
				}
			},
		},
	})
	if err1 == nil && res1.Status == workflowrun.StatusHumanGate {
		t.Fatalf("expected forced interrupt before full success: status=%s msg=%s", res1.Status, res1.Message)
	}
	if hangPID <= 0 {
		t.Fatalf("hang entry never recorded real pid; err1=%v calls1=%+v status=%s msg=%s res1=%+v",
			err1, calls1, res1.Status, res1.Message, res1)
	}

	// Item 1: typed durable forced_interrupt pair tied to exact identity.
	elog, err := workflowrun.OpenEventLog(home, projectID, runID)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	events1, err := elog.ReadAll()
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	var hangG0 string
	priorSucceededIDs := map[string]string{} // work item → attempt
	var foundInterrupt, foundCancelled bool
	var interruptID, interruptWI, interruptAtt string
	var interruptGen int
	for _, e := range events1 {
		if e.ProjectID != projectID || e.RunID != runID {
			t.Fatalf("event identity drift: project=%q run=%q want %s/%s kind=%s",
				e.ProjectID, e.RunID, projectID, runID, e.Kind)
		}
		if e.Kind == "launch" && e.WorkItemID == hangWI && hangG0 == "" {
			hangG0 = e.AttemptID
		}
		if e.Kind == "terminal" {
			term := e.Terminal
			if term == "" && len(e.Payload) > 0 {
				pm := map[string]string{}
				_ = json.Unmarshal(e.Payload, &pm)
				term = pm["terminal"]
			}
			if term == "succeeded" && e.WorkItemID != "" && e.AttemptID != "" {
				priorSucceededIDs[e.WorkItemID] = e.AttemptID
			}
		}
		if e.Kind == "interrupt" && e.WorkItemID == hangWI {
			pm := map[string]string{}
			_ = json.Unmarshal(e.Payload, &pm)
			fc := firstNonEmptyStr(e.FailureClass, pm["failure_class"])
			ic := pm["interrupt_class"]
			id := pm["interrupt_id"]
			if fc != "forced_interrupt" {
				t.Fatalf("interrupt failure_class=%q want forced_interrupt payload=%v", fc, pm)
			}
			if ic != "service_forced_interrupt" && ic != workflowrun.InterruptClassServiceForced {
				t.Fatalf("interrupt_class=%q want service_forced_interrupt", ic)
			}
			if strings.TrimSpace(id) == "" {
				t.Fatal("interrupt_id empty on durable interrupt event")
			}
			if strings.TrimSpace(e.AttemptID) == "" || e.Generation <= 0 {
				t.Fatalf("interrupt missing attempt/generation: %+v", e)
			}
			foundInterrupt = true
			interruptID, interruptWI, interruptAtt, interruptGen = id, e.WorkItemID, e.AttemptID, e.Generation
			if hangG0 == "" {
				hangG0 = e.AttemptID
			}
		}
		if e.Kind == "terminal" && e.WorkItemID == hangWI {
			pm := map[string]string{}
			_ = json.Unmarshal(e.Payload, &pm)
			term := firstNonEmptyStr(e.Terminal, pm["terminal"])
			if term == "cancelled" {
				fc := firstNonEmptyStr(e.FailureClass, pm["failure_class"])
				if fc != "forced_interrupt" {
					t.Fatalf("cancelled terminal failure_class=%q", fc)
				}
				if strings.TrimSpace(pm["interrupt_id"]) == "" && interruptID == "" {
					t.Fatalf("cancelled terminal missing interrupt_id pair: %v", pm)
				}
				if e.AttemptID != "" && hangG0 != "" && e.AttemptID != hangG0 {
					t.Fatalf("cancelled attempt %s != launched g0 %s", e.AttemptID, hangG0)
				}
				foundCancelled = true
			}
			if term == "succeeded" {
				t.Fatalf("aborted %s must not succeed on pass1: %+v payload=%v", hangWI, e, pm)
			}
		}
		if e.Kind == "integrate" && e.WorkItemID == hangWI {
			t.Fatalf("aborted %s must not integrate on pass1: %+v", hangWI, e)
		}
	}
	if !foundInterrupt || !foundCancelled {
		t.Fatalf("want typed interrupt+cancelled pair; interrupt=%v cancelled=%v events=%d",
			foundInterrupt, foundCancelled, len(events1))
	}
	if hangG0 == "" || !strings.HasSuffix(hangG0, "-g0") {
		t.Fatalf("%s g0 attempt missing/malformed: %q", hangWI, hangG0)
	}
	if interruptWI != hangWI || interruptAtt != hangG0 || interruptGen <= 0 {
		t.Fatalf("interrupt identity wi=%s att=%s gen=%d want %s/%s/>0",
			interruptWI, interruptAtt, interruptGen, hangWI, hangG0)
	}
	for _, need := range []string{"wi_research", "wi_implement"} {
		if _, ok := priorSucceededIDs[need]; !ok {
			t.Fatalf("%s must succeed before %s hang: prior=%v", need, hangWI, priorSucceededIDs)
		}
	}
	// A7-12: workflowrun must persist the exact complete aborted ChildOutcome on
	// result/partial BEFORE any goalrun projection. Resume never invents from events.
	foundAbortedRow := false
	for _, c := range res1.Workflow.Children {
		if c.AttemptID == hangG0 && c.WorkItemID == hangWI {
			foundAbortedRow = true
			if c.Terminal != "cancelled" {
				t.Fatalf("pass1 result aborted row terminal=%q want cancelled (exact)", c.Terminal)
			}
			if c.FailureClass != "forced_interrupt" {
				t.Fatalf("pass1 result aborted row failure_class=%q", c.FailureClass)
			}
			if c.Generation < 1 || c.TaskClass == "" || c.ChildContractDigest == "" {
				t.Fatalf("pass1 aborted row incomplete identity: %+v", c)
			}
			if c.Permission != "bounded_write" {
				t.Fatalf("pass1 aborted row permission=%q want exact selected bounded_write: %+v", c.Permission, c)
			}
		}
	}
	if !foundAbortedRow {
		t.Fatalf("pass1 Workflow.Children missing exact complete aborted row for %s before goalrun projection", hangG0)
	}
	part1, perr := workflowrun.LoadPartialPrior(home, projectID, runID)
	if perr != nil {
		t.Fatalf("pass1 partial load: %v", perr)
	}
	foundPart := false
	for _, k := range part1.WorkflowKids {
		if k.AttemptID == hangG0 && k.WorkItemID == hangWI {
			foundPart = true
			if k.Terminal != "cancelled" || k.FailureClass != "forced_interrupt" {
				t.Fatalf("partial aborted row incomplete: %+v", k)
			}
			if k.Permission != "bounded_write" {
				t.Fatalf("partial aborted row permission=%q want exact selected bounded_write: %+v", k.Permission, k)
			}
		}
	}
	if !foundPart {
		t.Fatalf("pass1 partial missing exact complete aborted WorkflowKids row for %s", hangG0)
	}
	// Later children must not have launched in pass 1.
	for _, later := range []string{"wi_verify", "wi_docs"} {
		if _, ok := priorSucceededIDs[later]; ok {
			t.Fatalf("%s must not succeed in pass1: prior=%v", later, priorSucceededIDs)
		}
	}
	for _, e := range events1 {
		if e.Kind == "launch" && (e.WorkItemID == "wi_verify" || e.WorkItemID == "wi_docs") {
			t.Fatalf("pass1 must not launch %s: %+v", e.WorkItemID, e)
		}
	}
	cp, _, err := goalrun.LoadCheckpoint(home, projectID, runID)
	if err != nil {
		t.Fatalf("checkpoint after interrupt: %v (status=%s)", err, res1.Status)
	}
	if len(cp.PriorSucceeded) < 1 {
		t.Fatalf("checkpoint prior empty: %+v", cp.PriorSucceeded)
	}

	// Default integrate ledger under HomeDir; no repo-local pollution after pass1.
	if _, err := os.Lstat(filepath.Join(repoPath, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("pass1 created <repo>/.loopcoder: %v", err)
	}
	ledgerDir, err := workflowrun.DefaultIntegrateLedgerDir(home, projectID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ledgerDir, "integrate-ledger.json")); err != nil {
		t.Fatalf("durable integrate ledger missing after pass1: %v dir=%s", err, ledgerDir)
	}

	// --- Pass 2: fresh process invocation resumes same run ---
	calls2 := map[string]int{}
	canaryOut := filepath.Join(t.TempDir(), "canary_evidence.json")
	archiveDig := strings.Repeat("ab", 32)
	preProdSHA := strings.Repeat("cd", 20)
	res2, err2 := goalrun.Execute(context.Background(), goalrun.Request{
		ProjectID: projectID, RunID: runID, Resume: true,
		Goal: goal, Issue: "1397",
		Actor: "owner", Owner: "worker",
		Provider: "codex", Model: "gpt-5.5",
		InventoryProvenance: goalrun.InventoryProvenanceLiveDiscover,
		HomeDir:             home, RepoPath: repoPath, Now: func() time.Time { return now.Add(time.Minute) },
		Decompose:     fiveChild,
		LoadInventory: env.loadInv(), OpenLedger: env.openLed(),
		Executor: testspawn.Executor{
			HomeDir: home, Now: func() time.Time { return now.Add(time.Minute) },
			Calls: calls2,
		},
		CanaryEmit: &goalrun.CanaryEmitOptions{
			OutPath: canaryOut, HomeDir: home, RepoPath: repoPath,
			ArchiveDigest: archiveDig, PreProdSHA: preProdSHA,
			BinaryVersion: "0.9.0-a76b", BinaryCommit: preProdSHA,
			InventoryProvenance: goalrun.InventoryProvenanceLiveDiscover,
		},
	})
	if err2 != nil {
		t.Fatalf("resume execute: %v status=%s msg=%s", err2, res2.Status, res2.Message)
	}
	if !res2.Resumed {
		t.Fatalf("expected Resumed=true: %+v", res2)
	}

	// Item 2/3: only gN+1 for aborted child; no re-exec of succeeded siblings.
	for id := range cp.PriorSucceeded {
		if calls2[id] != 0 {
			t.Fatalf("prior-succeeded %s re-executed on resume: calls2=%+v", id, calls2)
		}
	}
	if calls2[hangWI] != 1 {
		t.Fatalf("%s resume calls=%d want 1: %+v", hangWI, calls2[hangWI], calls2)
	}
	if calls2["wi_verify"] != 1 {
		t.Fatalf("wi_verify resume calls=%d want 1: %+v", calls2["wi_verify"], calls2)
	}
	if calls2["wi_docs"] != 1 {
		t.Fatalf("wi_docs resume calls=%d want 1: %+v", calls2["wi_docs"], calls2)
	}
	reuseN := res2.ReuseCount
	if reuseN < 1 {
		reuseN = res2.Workflow.ReuseCount
	}
	if reuseN < 2 {
		t.Fatalf("reuse_count=%d want >=2 (research+implement)", reuseN)
	}
	// Universal multi-attempt surface: aborted g0 cancelled + successor g1 present.
	// Choose max Generation explicitly — never loop-order overwrite.
	var hangFinal string
	var hangG0Row, hangMaxGen int
	var hangG0Term string
	succeededUseful := 0
	for _, c := range res2.Workflow.Children {
		if prior, ok := cp.PriorSucceeded[c.WorkItemID]; ok {
			// Prior-succeeded rows keep exact attempt; may also retain historical
			// non-success attempts for the same work item after multi-restart.
			if strings.EqualFold(c.Terminal, "succeeded") && c.AttemptID != prior.AttemptID {
				t.Fatalf("%s succeeded identity drift: want %s got %s", c.WorkItemID, prior.AttemptID, c.AttemptID)
			}
		}
		if c.WorkItemID == hangWI {
			if c.AttemptID == hangG0 || strings.HasSuffix(c.AttemptID, "-g0") {
				hangG0Row++
				hangG0Term = c.Terminal
				if strings.EqualFold(c.Terminal, "succeeded") {
					t.Fatalf("%s g0 must not succeed: %+v", hangWI, c)
				}
			}
			if c.Generation > hangMaxGen {
				hangMaxGen = c.Generation
				hangFinal = c.AttemptID
			}
		}
		if c.Terminal == "succeeded" {
			succeededUseful++
		}
	}
	if hangG0Row < 1 {
		t.Fatalf("%s missing aborted g0 row in Workflow.Children (universal reports)", hangWI)
	}
	if hangG0Term != "" && strings.EqualFold(hangG0Term, "succeeded") {
		t.Fatalf("%s g0 terminal succeeded", hangWI)
	}
	if hangFinal == "" || hangFinal == hangG0 || strings.HasSuffix(hangFinal, "-g0") {
		t.Fatalf("%s max-gen successor missing or still g0: final=%q g0=%q children=%+v",
			hangWI, hangFinal, hangG0, res2.Workflow.Children)
	}
	if !strings.Contains(hangFinal, "-g1") {
		t.Fatalf("%s want g1 successor attempt, got %s", hangWI, hangFinal)
	}
	if succeededUseful < 4 {
		t.Fatalf("want >=4 succeeded children after resume, got %d children=%+v", succeededUseful, res2.Workflow.Children)
	}

	events2, err := elog.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	g0Launch, g0Success, g0Integrate := 0, 0, 0
	g1Launch := 0
	for _, e := range events2 {
		if e.WorkItemID != hangWI {
			continue
		}
		if e.Kind == "launch" && e.AttemptID == hangG0 {
			g0Launch++
		}
		if e.Kind == "launch" && e.AttemptID == hangFinal {
			g1Launch++
		}
		if e.AttemptID == hangG0 {
			term := e.Terminal
			if term == "" && len(e.Payload) > 0 {
				pm := map[string]string{}
				_ = json.Unmarshal(e.Payload, &pm)
				term = pm["terminal"]
			}
			if e.Kind == "terminal" && term == "succeeded" {
				g0Success++
			}
			if e.Kind == "integrate" {
				g0Integrate++
			}
		}
	}
	if g0Launch != 1 {
		t.Fatalf("%s g0 launch count=%d want 1 (no duplicate launch)", hangWI, g0Launch)
	}
	if g1Launch != 1 {
		t.Fatalf("%s g1 launch count=%d want 1 final=%s", hangWI, g1Launch, hangFinal)
	}
	if g0Success != 0 || g0Integrate != 0 {
		t.Fatalf("aborted g0 succeeded=%d integrated=%d want 0/0", g0Success, g0Integrate)
	}
	// Later children: exactly one launch each, under non-g0 attempt after resume.
	laterAttByID := map[string]string{}
	for _, later := range []string{"wi_verify", "wi_docs"} {
		launchN, g0Later := 0, 0
		var laterAtt string
		for _, e := range events2 {
			if e.WorkItemID != later || e.Kind != "launch" {
				continue
			}
			launchN++
			laterAtt = e.AttemptID
			if strings.HasSuffix(e.AttemptID, "-g0") {
				g0Later++
			}
		}
		if launchN != 1 {
			t.Fatalf("%s launch count=%d want 1", later, launchN)
		}
		if g0Later != 0 {
			t.Fatalf("%s must not launch under released g0: att=%s", later, laterAtt)
		}
		if !strings.Contains(laterAtt, "-g1") {
			t.Fatalf("%s want g1 attempt after resume, got %s", later, laterAtt)
		}
		laterAttByID[later] = laterAtt
	}

	// Capacity reservation matrix: released g0 immutable; unique g1 reservations.
	led, lerr := capacityledger.OpenPath(env.LedgerPath, func() time.Time { return now.Add(2 * time.Minute) })
	if lerr != nil {
		t.Fatalf("open capacity ledger: %v", lerr)
	}
	planDig := res2.PlanDigest
	if planDig == "" {
		planDig = res2.Workflow.PlanDigest
	}
	type row struct {
		id, g0, g1, g0State, g1State string
		g0ActualNil, g1OK            bool
	}
	matrix := make([]row, 0, 5)
	for _, id := range []string{"wi_research", "wi_implement", "wi_tests", "wi_verify", "wi_docs"} {
		g0 := workflowrun.AttemptID(id, planDig, runID, 0)
		g1 := workflowrun.AttemptID(id, planDig, runID, 1)
		e0, ok0 := led.Get(projectID, runID, g0)
		e1, ok1 := led.Get(projectID, runID, g1)
		r := row{id: id, g0: g0, g1: g1}
		if ok0 {
			r.g0State = e0.State
			r.g0ActualNil = e0.Actual == nil
		}
		if ok1 {
			r.g1State = e1.State
			r.g1OK = e1.State == "reserved" || e1.State == "reconciled" || e1.State == "released"
		}
		matrix = append(matrix, r)
		switch id {
		case "wi_research", "wi_implement":
			// Prior success: g0 terminal spend, no g1 re-reserve required.
			if !ok0 || (e0.State != "reconciled" && e0.State != "released") {
				t.Fatalf("%s g0 want reconciled|released, got ok=%v state=%q", id, ok0, e0.State)
			}
			if ok1 {
				t.Fatalf("%s must not create g1 reservation (prior success): %+v", id, e1)
			}
		case "wi_tests", "wi_verify", "wi_docs":
			// g0 released immutable (unlaunched or aborted); Actual unknown for unlaunched/aborted path.
			if !ok0 || e0.State != "released" {
				t.Fatalf("%s g0 must remain released immutable, ok=%v state=%q", id, ok0, e0.State)
			}
			if e0.Actual != nil {
				t.Fatalf("%s g0 Actual must stay unknown/nil after release, got %v", id, *e0.Actual)
			}
			if !ok1 {
				t.Fatalf("%s missing unique g1 reservation after resume", id)
			}
			if e1.State != "reconciled" && e1.State != "released" {
				t.Fatalf("%s g1 want reconciled|released after success, got %q", id, e1.State)
			}
			if e0.AttemptID == e1.AttemptID {
				t.Fatalf("%s g0/g1 attempt collision", id)
			}
		}
	}
	// Uniqueness of all reservation ids across g1 rows for re-executed children.
	seenRes := map[string]string{}
	for _, id := range []string{"wi_tests", "wi_verify", "wi_docs"} {
		g1 := workflowrun.AttemptID(id, planDig, runID, 1)
		e1, ok := led.Get(projectID, runID, g1)
		if !ok || e1.ReservationID == "" {
			t.Fatalf("%s g1 reservation_id empty", id)
		}
		if prev, ok := seenRes[e1.ReservationID]; ok {
			t.Fatalf("duplicate reservation_id %s for %s and %s", e1.ReservationID, prev, id)
		}
		seenRes[e1.ReservationID] = id
	}
	t.Logf("attempt/reservation matrix: %+v laterAtt=%v", matrix, laterAttByID)

	// Item 4: production peaks + active occupancy (sequential ceiling 1).
	procPeak := res2.ProcessPeak
	if procPeak < 1 {
		procPeak = res2.Workflow.ProcessPeak
	}
	wtPeak := res2.WorktreePeak
	if wtPeak < 1 {
		wtPeak = res2.Workflow.WorktreePeak
	}
	if procPeak < 1 {
		t.Fatalf("ProcessPeak unmeasured: res=%d wf=%d launches=%d",
			res2.ProcessPeak, res2.Workflow.ProcessPeak, res2.Workflow.LaunchCount)
	}
	if wtPeak < 1 {
		t.Fatalf("WorktreePeak unmeasured: res=%d wf=%d", res2.WorktreePeak, res2.Workflow.WorktreePeak)
	}
	if procPeak > artifactqual.ProductionSequentialCeiling {
		t.Fatalf("ProcessPeak=%d > sequential ceiling %d", procPeak, artifactqual.ProductionSequentialCeiling)
	}
	if wtPeak > artifactqual.ProductionSequentialCeiling {
		t.Fatalf("WorktreePeak=%d > sequential ceiling %d", wtPeak, artifactqual.ProductionSequentialCeiling)
	}
	if res2.Workflow.ProcessActive != 0 || res2.Workflow.WorktreeActive != 0 {
		t.Fatalf("active occupancy must end at zero: process=%d worktree=%d",
			res2.Workflow.ProcessActive, res2.Workflow.WorktreeActive)
	}

	// Item 5: fresh lstat — <repo>/.loopcoder MUST NOT exist.
	if _, err := os.Lstat(filepath.Join(repoPath, ".loopcoder")); !os.IsNotExist(err) {
		t.Fatalf("fresh lstat: <repo>/.loopcoder must not exist (err=%v)", err)
	}
	// Same-run durable ledger still present under HomeDir after resume.
	if _, err := os.Stat(filepath.Join(ledgerDir, "integrate-ledger.json")); err != nil {
		t.Fatalf("resume lost durable integrate ledger: %v", err)
	}

	// Item 6: canary derived from production result/events; RestartOK must be true.
	if res2.CanaryEvidencePath == "" {
		t.Fatalf("CanaryEvidencePath empty; emit failed? msg=%s", res2.Message)
	}
	raw, err := os.ReadFile(res2.CanaryEvidencePath)
	if err != nil {
		t.Fatalf("read canary: %v", err)
	}
	var ev artifactqual.CanaryEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("parse canary: %v\n%s", err, raw)
	}
	if ev.ProjectID != projectID || ev.RunID != runID {
		t.Fatalf("canary project/run=%s/%s want %s/%s", ev.ProjectID, ev.RunID, projectID, runID)
	}
	if ev.Restart == nil {
		t.Fatal("canary Restart section missing (must derive from events)")
	}
	r := ev.Restart
	if !r.Interrupted || !r.ResumedFromDurable || !r.ExactlyOnce || !r.LaterGenerationResume {
		t.Fatalf("restart flags incomplete: %+v", r)
	}
	if r.AbortedAttemptCount < 1 || r.ReuseCountMeasured < 1 {
		t.Fatalf("abort/reuse counts: %+v", r)
	}
	if r.DuplicateLaunch || r.DuplicateSuccessIntegrate || r.AbortedAttemptSucceeded {
		t.Fatalf("exactly-once violations: %+v", r)
	}
	if r.ChildCountUseful < 4 {
		t.Fatalf("ChildCountUseful=%d want >=4", r.ChildCountUseful)
	}
	if r.ProcessPeak < 1 || r.WorktreePeak < 1 {
		t.Fatalf("canary peaks unmeasured: %+v", r)
	}
	if r.ProcessLimit != artifactqual.ProductionSequentialCeiling ||
		r.WorktreeLimit != artifactqual.ProductionSequentialCeiling {
		t.Fatalf("limits not production sequential ceiling: proc=%d wt=%d", r.ProcessLimit, r.WorktreeLimit)
	}
	if !r.ProcessCeilingOK || !r.WorktreeCeilingOK {
		t.Fatalf("ceiling OK flags false: %+v", r)
	}
	if !r.ActiveOccupancyMeasured || r.ProcessActive != 0 || r.WorktreeActive != 0 || !r.NoLeakedProcesses {
		t.Fatalf("occupancy/leak measured flags: %+v", r)
	}
	if !r.RepoLocalRuntimeChecked || r.RepoLocalRuntimePresent || !r.NoRepoLocalRuntime {
		t.Fatalf("repo-local must be checked, absent, NoRepoLocal=true: %+v", r)
	}
	if !strings.Contains(r.EvidenceRef, "event") && !strings.Contains(r.EvidenceRef, "workflow-events") {
		t.Fatalf("EvidenceRef not event-ledger bound: %q", r.EvidenceRef)
	}

	v := artifactqual.ValidateCanaryEvidence(ev, archiveDig, preProdSHA, now.Add(2*time.Minute))
	for _, reason := range v.Reasons {
		if strings.Contains(reason, "flag_mismatch") {
			t.Fatalf("validator flag_mismatch: %v full=%v", reason, v.Reasons)
		}
	}
	if !v.RestartOK {
		t.Fatalf("RestartOK must be true; reasons=%v restart=%+v", v.Reasons, r)
	}
	t.Logf("A7-6B production proof OK: interrupt_id=%s hang=%s g0=%s g1=%s peaks=%d/%d reuse=%d useful=%d RestartOK=%v",
		interruptID, hangWI, hangG0, hangFinal, r.ProcessPeak, r.WorktreePeak, r.ReuseCountMeasured, r.ChildCountUseful, v.RestartOK)
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
