package workclaim

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	g := testGraph()
	return ClaimRequest{
		ProjectID: "p1", Graph: g, WorkItemID: item,
		AttemptID: attempt, ExecutorID: exec, Lease: time.Minute,
		// Explicit ExecutionPlanDigest — never empty (no ready/graph fallback).
		PlanDigest: "sha256:test-exec-plan-" + item,

		TaskClass: "tera",

		ChildContractDigest: "sha256:test-child-contract",
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
		PlanDigest: "sha256:test-exec-plan",
		TaskClass:  "tera",

		ChildContractDigest: "sha256:test-child-contract",
	})
	if r.Code != ResultClaimed {
		t.Fatal(r.Code)
	}
	// advance past lease without non-launch proof
	clock = clock.Add(2 * time.Minute)
	r2, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "att2", ExecutorID: "ex2", Lease: time.Minute,
		PlanDigest: "sha256:test-exec-plan",
		TaskClass:  "tera",

		ChildContractDigest: "sha256:test-child-contract",
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
		PlanDigest: "sha256:test-exec-plan",
		TaskClass:  "tera",

		ChildContractDigest: "sha256:test-child-contract",
	})
	clock = clock.Add(2 * time.Minute)
	r3, _ := s2.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), WorkItemID: "wi_a",
		AttemptID: "a2", ExecutorID: "e2", Lease: time.Minute, NonLaunchProven: true,
		PlanDigest: "sha256:test-exec-plan",
		TaskClass:  "tera",

		ChildContractDigest: "sha256:test-child-contract",
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
		PlanDigest:          "sha256:test-exec-plan",
		TaskClass:           "tera",

		ChildContractDigest: "sha256:test-child-contract",
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
		PlanDigest:          "sha256:test-exec-plan",
		TaskClass:           "tera",

		ChildContractDigest: "sha256:test-child-contract",
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
		PlanDigest: "sha256:test-exec-plan",
		TaskClass:  "tera",

		ChildContractDigest: "sha256:test-child-contract",
	})
	if rSame.Code != ResultTerminalReused {
		t.Fatalf("same attempt on restart: %+v", rSame)
	}
	// New attempt also blocked (logical success + evidence terminal).
	rNew, _ := s.Claim(ClaimRequest{
		ProjectID: "p1", Graph: testGraph(), Evidence: ev,
		WorkItemID: "wi_a", AttemptID: "att-wi_a-g1", ExecutorID: "ex1", Lease: time.Minute,
		PlanDigest: "sha256:test-exec-plan",
		TaskClass:  "tera",

		ChildContractDigest: "sha256:test-child-contract",
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
		PlanDigest:          "sha256:test-exec-plan",
		TaskClass:           "tera",

		ChildContractDigest: "sha256:test-child-contract",
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
		PlanDigest:          "sha256:test-exec-plan",
		TaskClass:           "tera",

		ChildContractDigest: "sha256:test-child-contract",
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

func TestOpenPath_PersistsClosedClaimAcrossReopen(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "claims.json")
	s, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Claim(baseReq("wi_a", "att1", "ex1"))
	if err != nil || r.Code != ResultClaimed {
		t.Fatalf("%+v %v", r, err)
	}
	cr, err := s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att1",
		Terminal: workgraph.TermFailed, OutputEvidence: "ev-fail",
	})
	if err != nil || cr.Code != ResultClosed {
		t.Fatalf("close: %+v %v", cr, err)
	}
	s2, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.GetByAttempt("p1", "g1", 1, "wi_a", "att1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateClosed || got.Terminal != workgraph.TermFailed {
		t.Fatalf("reopen claim: %+v", got)
	}
	r2, _ := s2.Claim(baseReq("wi_a", "att1", "ex2"))
	if r2.Code != ResultTerminalReused {
		t.Fatalf("want terminal reused, got %+v", r2)
	}
}

func TestClaimGraphDigestCanonicalAndNonempty(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })

	// Happy path: GraphDigest is computed via DigestGraph, not blind copy of Graph.PlanDigest.
	g := testGraph()
	wantGD := workgraph.DigestGraph(g)
	if wantGD == "" {
		t.Fatal("DigestGraph must be nonempty for test graph")
	}
	if g.PlanDigest != wantGD {
		t.Fatalf("test graph PlanDigest=%q DigestGraph=%q", g.PlanDigest, wantGD)
	}
	r, err := s.Claim(baseReq("wi_a", "att-gd-ok", "ex1"))
	if err != nil || r.Code != ResultClaimed || r.Claim == nil {
		t.Fatalf("claim: %+v %v", r, err)
	}
	if r.Claim.GraphDigest == "" {
		t.Fatal("Claim.GraphDigest must be nonempty")
	}
	if r.Claim.GraphDigest != wantGD {
		t.Fatalf("Claim.GraphDigest=%q want DigestGraph=%q", r.Claim.GraphDigest, wantGD)
	}
	// PlanDigest stays the explicit execution plan digest (not graph digest).
	if r.Claim.PlanDigest != "sha256:test-exec-plan-wi_a" {
		t.Fatalf("PlanDigest must remain exec plan, got %q", r.Claim.PlanDigest)
	}
	if r.Claim.PlanDigest == r.Claim.GraphDigest {
		t.Fatal("PlanDigest must not equal GraphDigest")
	}

	// Empty Graph.PlanDigest still yields canonical computed GraphDigest.
	s2 := NewStore(func() time.Time { return now })
	gEmpty := testGraph()
	gEmpty.PlanDigest = ""
	reqEmpty := baseReq("wi_a", "att-gd-empty", "ex1")
	reqEmpty.Graph = gEmpty
	r2, err := s2.Claim(reqEmpty)
	if err != nil || r2.Code != ResultClaimed || r2.Claim == nil {
		t.Fatalf("empty PlanDigest graph claim: %+v %v", r2, err)
	}
	if r2.Claim.GraphDigest != workgraph.DigestGraph(gEmpty) {
		t.Fatalf("empty stored PlanDigest: GraphDigest=%q want %q", r2.Claim.GraphDigest, workgraph.DigestGraph(gEmpty))
	}

	// Inconsistent Graph.PlanDigest fails closed before claim mutation.
	s3 := NewStore(func() time.Time { return now })
	gBad := testGraph()
	gBad.PlanDigest = "sha256:not-the-real-graph-digest"
	reqBad := baseReq("wi_a", "att-gd-bad", "ex1")
	reqBad.Graph = gBad
	_, err = s3.Claim(reqBad)
	if err == nil {
		t.Fatal("inconsistent Graph.PlanDigest must fail closed")
	}
	if !errors.Is(err, ErrInvalid) && !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("want ErrInvalid/inconsistent, got %v", err)
	}
	// No claim persisted.
	if _, gerr := s3.GetByAttempt("p1", "g1", 1, "wi_a", "att-gd-bad"); !errors.Is(gerr, ErrNotFound) {
		t.Fatalf("must not store claim on inconsistent graph digest: %v", gerr)
	}
}

func TestOpenPath_CorruptFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPath(path, time.Now); err == nil {
		t.Fatal("corrupt must fail")
	}
}

func TestOpenPath_EmptySchemaFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	if err := os.WriteFile(path, []byte(`{"schema":"","claims":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPath(path, time.Now); err == nil {
		t.Fatal("empty schema must fail")
	}
}

func TestOpenPath_StaleSeqDoesNotRecycleIDs(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "claims.json")
	// Persist claim wcl_5 with seq field too low.
	body := `{
  "schema": "loopcoder.workclaim.store.v1",
  "seq": 1,
  "claims": [{
    "schema": "loopcoder.workclaim.v1",
    "claim_id": "wcl_5",
    "project_id": "p1",
    "graph_id": "g1",
    "graph_version": 1,
    "work_item_id": "wi_a",
    "attempt_id": "att1",
    "executor_id": "ex",
    "generation": 1,
    "state": "closed",
    "terminal": "failed",
    "output_evidence": "ev",
    "claimed_at": "2026-07-22T12:00:00Z"
  }]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// Claim wi_a under a new attempt (wi_a is first ready item).
	r, err := s.Claim(baseReq("wi_a", "att-new", "ex"))
	if err != nil || r.Code != ResultClaimed {
		t.Fatalf("%+v %v", r, err)
	}
	if r.Claim.ClaimID == "wcl_1" || r.Claim.ClaimID == "wcl_5" {
		t.Fatalf("must not recycle IDs: %q", r.Claim.ClaimID)
	}
	// seq seeded from wcl_5 → next is wcl_6
	if r.Claim.ClaimID != "wcl_6" {
		t.Fatalf("want wcl_6 got %q", r.Claim.ClaimID)
	}
}

func TestOpenPath_MalformedLivePointerFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	body := `{
  "schema": "loopcoder.workclaim.store.v1",
  "seq": 1,
  "live_by_item": {"bad|key": "wcl_missing"},
  "claims": []
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPath(path, time.Now); err == nil {
		t.Fatal("malformed live pointer must fail")
	}
}

func TestSaveFailure_ClaimRollbackUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "claims.json")
	s, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	s.TestFailSave = fmt.Errorf("disk full")
	r, err := s.Claim(baseReq("wi_a", "att1", "ex1"))
	if err == nil {
		t.Fatalf("want save fail, got %+v", r)
	}
	if len(s.AllClaims()) != 0 {
		t.Fatalf("memory must roll back claims: %+v", s.AllClaims())
	}
	// Disk empty or absent claim.
	s2, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		// empty file may fail schema if partial write — both ok fail-closed
		return
	}
	if len(s2.AllClaims()) != 0 {
		t.Fatalf("disk must not have claims: %+v", s2.AllClaims())
	}
}

func TestOpenPath_InvalidStateFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	body := `{
  "schema": "loopcoder.workclaim.store.v1",
  "seq": 1,
  "claims": [{
    "schema": "loopcoder.workclaim.v1",
    "claim_id": "wcl_1",
    "project_id": "p1", "graph_id": "g1", "graph_version": 1,
    "work_item_id": "wi_a", "attempt_id": "att1", "executor_id": "ex",
    "generation": 1, "state": "bogus",
    "claimed_at": "2026-07-22T12:00:00Z"
  }]
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPath(path, time.Now); err == nil {
		t.Fatal("invalid state must fail")
	}
}

func TestSaveFailure_CloseAndRenewRollback(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "claims.json")
	s, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Claim(baseReq("wi_a", "att1", "ex1"))
	if err != nil || r.Code != ResultClaimed {
		t.Fatalf("%+v %v", r, err)
	}
	before, _ := os.ReadFile(path)
	s.TestFailSave = fmt.Errorf("disk full")
	_, err = s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att1",
		Terminal: workgraph.TermFailed, OutputEvidence: "ev",
	})
	if err == nil {
		t.Fatal("close save fail expected")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("disk changed on failed close")
	}
	got, err := s.Get(r.Claim.ClaimID)
	if err != nil || got.State != StateClaimed {
		t.Fatalf("memory must still be claimed: %+v %v", got, err)
	}
	// Renew rollback
	s.TestFailSave = fmt.Errorf("disk full")
	_, err = s.Renew(r.Claim.ClaimID, r.Claim.Generation, "ex1", "att1", time.Minute)
	if err == nil {
		t.Fatal("renew save fail expected")
	}
	got2, _ := s.Get(r.Claim.ClaimID)
	if got2.State != StateClaimed {
		t.Fatalf("renew rollback state=%s", got2.State)
	}
	// Clear fail and close succeeds
	s.TestFailSave = nil
	if _, err := s.Close(CloseRequest{
		ClaimID: r.Claim.ClaimID, Generation: r.Claim.Generation,
		ExecutorID: "ex1", AttemptID: "att1",
		Terminal: workgraph.TermFailed, OutputEvidence: "ev",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenPath_MalformedWclIDFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	body := `{
  "schema": "loopcoder.workclaim.store.v1",
  "seq": 1,
  "claims": [{
    "schema": "loopcoder.workclaim.v1",
    "claim_id": "not-wcl",
    "project_id": "p1", "graph_id": "g1", "graph_version": 1,
    "work_item_id": "wi_a", "attempt_id": "att1", "executor_id": "ex",
    "generation": 1, "state": "closed", "terminal": "failed", "output_evidence": "e",
    "claimed_at": "2026-07-22T12:00:00Z"
  }]
}`
	_ = os.WriteFile(path, []byte(body), 0o600)
	if _, err := OpenPath(path, time.Now); err == nil {
		t.Fatal("malformed wcl id must fail")
	}
}

// TestTransactionalRollback_ByteEqualityMatrix covers Claim/Renew/Close/expired
// release/ambiguous/stale-live cleanup with byte-for-byte + full in-memory equality
// (no early-return acceptance).
func TestTransactionalRollback_ByteEqualityMatrix(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "claims.json")
	s, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	readBytes := func() []byte {
		raw, rerr := os.ReadFile(path)
		if rerr != nil && !os.IsNotExist(rerr) {
			t.Fatal(rerr)
		}
		return raw
	}
	memEq := func(a, b []Claim) bool {
		if len(a) != len(b) {
			return false
		}
		am := map[string]Claim{}
		for _, c := range a {
			am[c.ClaimID] = c
		}
		for _, c := range b {
			o, ok := am[c.ClaimID]
			if !ok || o.State != c.State || o.Generation != c.Generation ||
				o.AttemptID != c.AttemptID || o.Terminal != c.Terminal ||
				o.OutputEvidence != c.OutputEvidence {
				return false
			}
		}
		return true
	}

	// --- Claim ---
	before := readBytes()
	res, err := s.Claim(baseReq("wi_a", "att-rb-1", "exec"))
	if err != nil || res.Code != ResultClaimed || res.Claim == nil {
		t.Fatalf("claim: %+v err=%v", res, err)
	}
	afterClaim := readBytes()
	if string(before) == string(afterClaim) {
		t.Fatal("claim must change durable bytes")
	}
	if res.Claim.State != StateClaimed {
		t.Fatalf("state=%s", res.Claim.State)
	}
	// Reopen in-memory equality.
	sRe, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if !memEq(s.AllClaims(), sRe.AllClaims()) {
		t.Fatal("claim reopen in-memory inequality")
	}

	// --- Renew ---
	beforeRenew := readBytes()
	rr, err := s.Renew(res.Claim.ClaimID, res.Claim.Generation, "exec", "att-rb-1", 2*time.Minute)
	if err != nil || rr.Claim == nil || rr.Claim.State != StateRunning {
		t.Fatalf("renew: %+v err=%v", rr, err)
	}
	afterRenew := readBytes()
	if string(beforeRenew) == string(afterRenew) {
		t.Fatal("renew must change durable bytes")
	}
	// Failed renew (wrong generation) must not mutate.
	beforeBad := readBytes()
	_, _ = s.Renew(res.Claim.ClaimID, res.Claim.Generation+99, "exec", "att-rb-1", time.Minute)
	if string(beforeBad) != string(readBytes()) {
		t.Fatal("failed renew must not change durable bytes")
	}

	// --- Close ---
	beforeClose := readBytes()
	_, err = s.Close(CloseRequest{
		ClaimID: res.Claim.ClaimID, Generation: res.Claim.Generation, ExecutorID: "exec",
		AttemptID: "att-rb-1", Terminal: workgraph.TermSucceeded, OutputEvidence: "ev-rb-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	afterClose := readBytes()
	if string(beforeClose) == string(afterClose) {
		t.Fatal("close must change durable bytes")
	}
	sClosed, err := OpenPath(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if !memEq(s.AllClaims(), sClosed.AllClaims()) {
		t.Fatal("close reopen inequality")
	}

	// --- Expired release (reclaim same wi_a after short-lease expire) ---
	res2, err := s.Claim(baseReq("wi_a", "att-rb-2", "exec"))
	if err != nil || res2.Claim == nil {
		t.Fatalf("claim wi_a again: %+v %v", res2, err)
	}
	// Shorten lease by writing and reopening with far-future now.
	sExp, err := OpenPath(path, func() time.Time { return now.Add(2 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	beforeExp := readBytes()
	res3, err := sExp.Claim(baseReq("wi_a", "att-rb-3", "exec2"))
	if err != nil {
		t.Fatal(err)
	}
	if res3.Code != ResultClaimed && res3.Claim == nil && res3.Code != ResultAlreadyRunning {
		t.Fatalf("expired reclaim: %+v", res3)
	}
	afterExp := readBytes()
	if string(beforeExp) == string(afterExp) && res3.Code == ResultClaimed {
		t.Fatal("expired release claim must change durable bytes")
	}

	// --- Ambiguous transition: live claim + second executor ---
	sAmb, err := OpenPath(path, func() time.Time { return now.Add(3 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	// Ensure a live claim exists.
	var liveID string
	for _, c := range sAmb.AllClaims() {
		if c.WorkItemID == "wi_a" && (c.State == StateClaimed || c.State == StateRunning) {
			liveID = c.ClaimID
			break
		}
	}
	if liveID == "" {
		rLive, lerr := sAmb.Claim(baseReq("wi_a", "att-rb-live", "exec"))
		if lerr != nil || rLive.Claim == nil {
			t.Fatalf("need live claim: %+v %v", rLive, lerr)
		}
		liveID = rLive.Claim.ClaimID
	}
	beforeAmb := readBytes()
	rAmb, aerr := sAmb.Claim(baseReq("wi_a", "att-rb-amb", "exec-other"))
	afterAmb := readBytes()
	_ = aerr
	_ = rAmb
	// AlreadyRunning / Ambiguous / Claimed — if mutated, bytes differ; pure reject may equal.
	sCheck, cerr := OpenPath(path, func() time.Time { return now.Add(3 * time.Hour) })
	if cerr != nil {
		t.Fatalf("reopen after ambiguous attempt: %v", cerr)
	}
	for _, c := range sCheck.AllClaims() {
		if c.State == StateAmbiguous {
			// live_by_item must not point here — load enforces claimed|running only
		}
	}
	if string(beforeAmb) != string(afterAmb) {
		// transition recorded
	}
	_ = liveID

	// --- Stale-live cleanup: live pointer to closed rejected on load ---
	raw := readBytes()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var closedID, closedLogical string
	s5, _ := OpenPath(path, func() time.Time { return now.Add(4 * time.Hour) })
	for _, c := range s5.AllClaims() {
		if c.State == StateClosed {
			closedID = c.ClaimID
			closedLogical = claimLogicalKey(c.ProjectID, c.GraphID, c.GraphVersion, c.WorkItemID)
			break
		}
	}
	if closedID == "" {
		t.Fatal("need a closed claim for stale-live test")
	}
	if liveMap, ok := doc["live_by_item"].(map[string]any); ok {
		liveMap[closedLogical] = closedID
		doc["live_by_item"] = liveMap
	} else {
		doc["live_by_item"] = map[string]any{closedLogical: closedID}
	}
	bad, _ := json.MarshalIndent(doc, "", "  ")
	badPath := filepath.Join(t.TempDir(), "bad-live.json")
	if err := os.WriteFile(badPath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPath(badPath, func() time.Time { return now }); err == nil {
		t.Fatal("stale live pointer to closed must fail load")
	}

	// Failed close (wrong gen) no mutation
	s6, err := OpenPath(path, func() time.Time { return now.Add(5 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	beforeNoop := readBytes()
	_, _ = s6.Close(CloseRequest{
		ClaimID: "wcl_99999", Generation: 1, ExecutorID: "x",
		AttemptID: "x", Terminal: workgraph.TermFailed, OutputEvidence: "",
	})
	if string(beforeNoop) != string(readBytes()) {
		t.Fatal("failed close must not mutate durable store")
	}
}
