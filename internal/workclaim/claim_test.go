package workclaim

import (
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func testGraph() workgraph.Graph {
	g := workgraph.Graph{
		Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
		GraphID: "g1", Version: 1,
		Source: workgraph.SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "owner",
		Items: []workgraph.WorkItem{
			{Schema: workgraph.SchemaItem, ID: "wi_a", Intent: "A", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1},
			{Schema: workgraph.SchemaItem, ID: "wi_b", Intent: "B", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 2},
		},
		Dependencies: []workgraph.Dependency{
			{Schema: workgraph.SchemaDep, From: "wi_a", To: "wi_b", Kind: workgraph.DepFinishToStart},
		},
		Limits: workgraph.DefaultLimits(),
	}
	g.PlanDigest = workgraph.DigestGraph(g)
	return g
}

func baseReq(item, attempt, exec string) ClaimRequest {
	return ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: item,
		AttemptID: attempt, ExecutorID: exec, Lease: time.Minute,
	}
}

func TestConcurrentClaimOneWinner(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	r1, r2 := ConcurrentClaimBarrier(s,
		baseReq("wi_a", "att1", "ex1"),
		baseReq("wi_a", "att2", "ex2"),
	)
	codes := []ResultCode{r1.Code, r2.Code}
	var claimed, already int
	for _, c := range codes {
		switch c {
		case ResultClaimed:
			claimed++
		case ResultAlreadyRunning:
			already++
		}
	}
	if claimed != 1 || already != 1 {
		t.Fatalf("claimed=%d already=%d r1=%+v r2=%+v", claimed, already, r1, r2)
	}
}

func TestAtomicManyClaimers(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]ClaimResult, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			results[i], _ = s.Claim(baseReq("wi_a", "att", "ex"))
		}()
	}
	close(start)
	wg.Wait()
	var won int
	for _, r := range results {
		if r.Code == ResultClaimed {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("winners=%d", won)
	}
}

func TestBlockedAndNotReady(t *testing.T) {
	s := NewStore(time.Now)
	// wi_b blocked until a done
	r, err := s.Claim(baseReq("wi_b", "att", "ex"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Code != ResultBlocked && r.Code != ResultNotReady {
		t.Fatalf("%+v", r)
	}
}

func TestStaleGenerationCannotClose(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	r, err := s.Claim(baseReq("wi_a", "att1", "ex1"))
	if err != nil || r.Code != ResultClaimed {
		t.Fatalf("%+v %v", r, err)
	}
	// wrong generation
	_, err = s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation + 1,
		ExecutorID: "ex1", AttemptID: "att1", Terminal: workgraph.TermSucceeded, OutputEvidence: "out",
	})
	if err == nil {
		t.Fatal("expected stale")
	}
	// wrong executor
	cr, err := s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "other", AttemptID: "att1", Terminal: workgraph.TermSucceeded, OutputEvidence: "out",
	})
	if err == nil && cr.Code != ResultStaleGeneration {
		// Close returns ErrStale
		t.Fatalf("%+v %v", cr, err)
	}
}

func TestCloseSuccessRequiresEvidenceIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	r, _ := s.Claim(baseReq("wi_a", "att1", "ex1"))
	cr, err := s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att1", Terminal: workgraph.TermSucceeded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cr.Code != ResultConflict {
		t.Fatalf("need evidence: %+v", cr)
	}
	cr, err = s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att1", Terminal: workgraph.TermSucceeded, OutputEvidence: "sha:out",
	})
	if err != nil || cr.Code != ResultClosed {
		t.Fatalf("%+v %v", cr, err)
	}
	// idempotent
	cr2, err := s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att1", Terminal: workgraph.TermSucceeded, OutputEvidence: "sha:out",
	})
	if err != nil || cr2.Code != ResultIdempotentClose {
		t.Fatalf("%+v %v", cr2, err)
	}
	// readiness observes accepted terminal
	ev := s.AcceptedTerminals("p1", "g1", 1)
	if ev["wi_a"] != workgraph.TermSucceeded {
		t.Fatalf("%v", ev)
	}
	ready := workgraph.EvaluateReady(testGraph(), ev)
	if !workgraph.ReadyContains(ready, "wi_b") {
		t.Fatalf("b should be ready: %v", ready.Ready)
	}
	// terminal reused
	r2, _ := s.Claim(baseReq("wi_a", "att9", "ex9"))
	if r2.Code != ResultTerminalReused {
		t.Fatalf("%+v", r2)
	}
}

func TestExpiredAmbiguousNeedsHuman(t *testing.T) {
	clock := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return clock })
	r, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "att", ExecutorID: "ex", Lease: time.Minute,
	})
	if r.Code != ResultClaimed {
		t.Fatal(r.Code)
	}
	// advance past lease without non-launch proof
	clock = clock.Add(2 * time.Minute)
	r2, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "att2", ExecutorID: "ex2", Lease: time.Minute,
	})
	if r2.Code != ResultNeedsHuman {
		t.Fatalf("%+v", r2)
	}
	// non-launch proven reclaim: need fresh claim first after release path
	// create new item graph with only wi_a for simplicity — use wi still ambiguous
	// For non-launch: claim again with NonLaunchProven when state was claimed+expired
	// Reset: new store for clean reclaim test
	clock = time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	s2 := NewStore(func() time.Time { return clock })
	s2.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "a", ExecutorID: "e", Lease: time.Minute,
	})
	clock = clock.Add(2 * time.Minute)
	r3, _ := s2.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "a2", ExecutorID: "e2", Lease: time.Minute, NonLaunchProven: true,
	})
	if r3.Code != ResultClaimed {
		t.Fatalf("reclaim %+v", r3)
	}
}

func TestRenewFence(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	r, _ := s.Claim(baseReq("wi_a", "att", "ex"))
	_, err := s.Renew(r.Claim.ClaimID, r.Claim.Generation, "wrong", "att", time.Minute)
	if err == nil {
		t.Fatal("stale renew")
	}
	rr, err := s.Renew(r.Claim.ClaimID, r.Claim.Generation, "ex", "att", time.Minute)
	if err != nil || rr.Claim.State != StateRunning {
		t.Fatalf("%+v %v", rr, err)
	}
}

func TestClosedAttemptImmutable_NewGenerationClaimsOnce(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	// Attempt g0 claims and fails (model_unavailable class at workflow layer).
	r0, err := s.Claim(baseReq("wi_a", "att-wi_a-g0", "ex1"))
	if err != nil || r0.Code != ResultClaimed {
		t.Fatalf("g0 claim: %+v %v", r0, err)
	}
	cr0, err := s.Close(CloseRequest{
		ClaimID: r0.Claim.ClaimID, Generation: r0.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att-wi_a-g0",
		Terminal: workgraph.TermFailed, OutputEvidence: "failed:model_unavailable:wi_a",
	})
	if err != nil || cr0.Code != ResultClosed {
		t.Fatalf("g0 close: %+v %v", cr0, err)
	}
	// Old attempt cannot re-exec: same AttemptID is immutable.
	rReplay, _ := s.Claim(baseReq("wi_a", "att-wi_a-g0", "ex1"))
	if rReplay.Code != ResultTerminalReused {
		t.Fatalf("replay g0 want terminal_reused, got %+v", rReplay)
	}
	// Prior claim terminal unchanged.
	got0, err := s.GetByAttempt("p1", "g1", 1, "wi_a", "att-wi_a-g0")
	if err != nil || got0.Terminal != workgraph.TermFailed || got0.State != StateClosed {
		t.Fatalf("g0 immutable: %+v %v", got0, err)
	}
	// New generation claims once with explicit supersedes relation.
	r1, err := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "att-wi_a-g1", ExecutorID: "ex1", Lease: time.Minute,
		SupersedesAttemptID: "att-wi_a-g0",
	})
	if err != nil || r1.Code != ResultClaimed {
		t.Fatalf("g1 claim: %+v %v", r1, err)
	}
	if r1.Claim.SupersedesAttemptID != "att-wi_a-g0" {
		t.Fatalf("supersedes relation: %+v", r1.Claim)
	}
	if r1.Claim.Generation <= r0.Claim.Generation {
		t.Fatalf("generation must bump: g0=%d g1=%d", r0.Claim.Generation, r1.Claim.Generation)
	}
	// Second claim of g1 while live → already running.
	r1b, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "att-wi_a-g1", ExecutorID: "ex2", Lease: time.Minute,
		SupersedesAttemptID: "att-wi_a-g0",
	})
	if r1b.Code != ResultAlreadyRunning {
		t.Fatalf("g1 double-claim: %+v", r1b)
	}
	// Close g1 success.
	if _, err := s.Close(CloseRequest{
		ClaimID: r1.Claim.ClaimID, Generation: r1.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att-wi_a-g1",
		Terminal: workgraph.TermSucceeded, OutputEvidence: "sha:ok",
	}); err != nil {
		t.Fatal(err)
	}
	// g0 still failed immutable; g1 succeeded.
	got0, _ = s.GetByAttempt("p1", "g1", 1, "wi_a", "att-wi_a-g0")
	if got0.Terminal != workgraph.TermFailed {
		t.Fatalf("g0 mutated: %+v", got0)
	}
	got1, _ := s.GetByAttempt("p1", "g1", 1, "wi_a", "att-wi_a-g1")
	if got1.Terminal != workgraph.TermSucceeded {
		t.Fatalf("g1: %+v", got1)
	}
	// Further attempts blocked by logical success.
	r2, _ := s.Claim(baseReq("wi_a", "att-wi_a-g2", "ex1"))
	if r2.Code != ResultTerminalReused {
		t.Fatalf("after success want terminal_reused, got %+v", r2)
	}
}

func TestRestartReusesCompletedGenerationWithoutDuplication(t *testing.T) {
	now := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	r, err := s.Claim(baseReq("wi_a", "att-wi_a-g0", "ex1"))
	if err != nil || r.Code != ResultClaimed {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att-wi_a-g0",
		Terminal: workgraph.TermSucceeded, OutputEvidence: "sha:prior",
	}); err != nil {
		t.Fatal(err)
	}
	// Restart: accepted terminals show succeeded; claim of any attempt is reused.
	ev := s.AcceptedTerminals("p1", "g1", 1)
	if ev["wi_a"] != workgraph.TermSucceeded {
		t.Fatalf("accepted: %v", ev)
	}
	// Same attempt identity cannot re-exec.
	rSame, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), Evidence: ev,
		WorkItemID: "wi_a", AttemptID: "att-wi_a-g0", ExecutorID: "ex1", Lease: time.Minute,
	})
	if rSame.Code != ResultTerminalReused {
		t.Fatalf("same attempt on restart: %+v", rSame)
	}
	// New attempt also blocked (logical success + evidence terminal).
	rNew, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), Evidence: ev,
		WorkItemID: "wi_a", AttemptID: "att-wi_a-g1", ExecutorID: "ex1", Lease: time.Minute,
	})
	if rNew.Code != ResultTerminalReused {
		t.Fatalf("new attempt after success: %+v", rNew)
	}
	// Without evidence, logical success still blocks (exactly-once).
	rNoEv, _ := s.Claim(baseReq("wi_a", "att-wi_a-g9", "ex9"))
	if rNoEv.Code != ResultTerminalReused {
		t.Fatalf("logical success must block: %+v", rNoEv)
	}
}

func TestFailedAttemptDoesNotReopenByKey(t *testing.T) {
	now := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	r, _ := s.Claim(baseReq("wi_a", "att-fail", "ex"))
	s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex", AttemptID: "att-fail",
		Terminal: workgraph.TermFailed, OutputEvidence: "failed:x",
	})
	// Mutating close with different terminal must conflict, not reopen.
	cr, err := s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex", AttemptID: "att-fail",
		Terminal: workgraph.TermSucceeded, OutputEvidence: "sha:forged",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cr.Code != ResultConflict {
		t.Fatalf("must not reopen closed failed: %+v", cr)
	}
	got, _ := s.Get(r.Claim.ClaimID)
	if got.Terminal != workgraph.TermFailed || got.State != StateClosed {
		t.Fatalf("mutated: %+v", got)
	}
}

func TestAcceptedTerminalsHighestGenerationDeterministic(t *testing.T) {
	now := time.Date(2026, 7, 23, 15, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	// g0 failed
	r0, _ := s.Claim(baseReq("wi_a", "att-g0", "ex"))
	s.Close(CloseRequest{
		ClaimID: r0.Claim.ClaimID, Generation: r0.Claim.Generation,
		ExecutorID: "ex", AttemptID: "att-g0", Terminal: workgraph.TermFailed, OutputEvidence: "f",
	})
	// g1 cancelled (same rank as failed) then would be nondeterministic without generation
	r1, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "att-g1", ExecutorID: "ex", Lease: time.Minute,
		SupersedesAttemptID: "att-g0",
	})
	s.Close(CloseRequest{
		ClaimID: r1.Claim.ClaimID, Generation: r1.Claim.Generation,
		ExecutorID: "ex", AttemptID: "att-g1", Terminal: workgraph.TermCancelled, OutputEvidence: "c",
	})
	// g2 succeeded
	r2, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "att-g2", ExecutorID: "ex", Lease: time.Minute,
		SupersedesAttemptID: "att-g1",
	})
	s.Close(CloseRequest{
		ClaimID: r2.Claim.ClaimID, Generation: r2.Claim.Generation,
		ExecutorID: "ex", AttemptID: "att-g2", Terminal: workgraph.TermSucceeded, OutputEvidence: "sha:ok",
	})
	for i := 0; i < 20; i++ {
		ev := s.AcceptedTerminals("p1", "g1", 1)
		if ev["wi_a"] != workgraph.TermSucceeded {
			t.Fatalf("iter %d: want succeeded from highest gen, got %v", i, ev)
		}
	}
	// Restart: logical success still final.
	r3, _ := s.Claim(baseReq("wi_a", "att-g9", "ex"))
	if r3.Code != ResultTerminalReused {
		t.Fatalf("restart must not re-claim: %+v", r3)
	}
}
