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
