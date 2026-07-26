package integrationreceipt

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/waveschedule"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func tree() WorktreeState {
	return WorktreeState{Path: "/tmp/integ", Branch: "integrate", Head: "parent0", Available: true}
}

func candsOutOfOrder() []Candidate {
	return []Candidate{
		{ID: "c_c", WorkItemID: "wi_c", SourceCommit: "src_c", IntegrationOrder: 3, Terminal: workgraph.TermSucceeded},
		{ID: "c_a", WorkItemID: "wi_a", SourceCommit: "src_a", IntegrationOrder: 1, Terminal: workgraph.TermSucceeded},
		{ID: "c_b", WorkItemID: "wi_b", SourceCommit: "src_b", IntegrationOrder: 2, Terminal: workgraph.TermSucceeded},
	}
}

func TestIntentOrderIndependentOfFinish(t *testing.T) {
	in1, err := BuildIntent(tree(), MethodApplyPatch, candsOutOfOrder(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	// reverse input order
	rev := []Candidate{candsOutOfOrder()[2], candsOutOfOrder()[1], candsOutOfOrder()[0]}
	in2, err := BuildIntent(tree(), MethodApplyPatch, rev, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if in1.Digest != in2.Digest {
		t.Fatalf("digest %s vs %s ids %v vs %v", in1.Digest, in2.Digest, in1.CandidateIDs, in2.CandidateIDs)
	}
	if in1.CandidateIDs[0] != "c_a" || in1.CandidateIDs[1] != "c_b" || in1.CandidateIDs[2] != "c_c" {
		t.Fatalf("%v", in1.CandidateIDs)
	}
}

func TestApplyOrderedIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	e := NewEngine(tree(), "integ-1", nil, func() time.Time { return now })
	cs := candsOutOfOrder()
	in, _ := BuildIntent(tree(), MethodApplyPatch, cs, "idem1")
	r1, err := e.Run(in, cs)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1) != 3 {
		t.Fatalf("%d", len(r1))
	}
	for _, r := range r1 {
		if r.Status != "applied" || !r.WorkItemClosed {
			t.Fatalf("%+v", r)
		}
	}
	// retry identical — adopt applied, no duplicate
	head := e.Head()
	r2, err := e.Run(in, cs)
	if err != nil {
		t.Fatal(err)
	}
	if e.Head() != head {
		t.Fatal("head moved on retry")
	}
	if len(r2) != 3 {
		t.Fatalf("%d", len(r2))
	}
}

func TestConflictStopsWithAttention(t *testing.T) {
	now := time.Now().UTC()
	e := NewEngine(tree(), "integ-1", nil, func() time.Time { return now })
	cs := []Candidate{
		{ID: "c1", WorkItemID: "w1", SourceCommit: "ok1", IntegrationOrder: 1, Terminal: workgraph.TermSucceeded},
		{ID: "c2", WorkItemID: "w2", SourceCommit: "CONFLICT_x", IntegrationOrder: 2, Terminal: workgraph.TermSucceeded},
		{ID: "c3", WorkItemID: "w3", SourceCommit: "ok3", IntegrationOrder: 3, Terminal: workgraph.TermSucceeded},
	}
	in, _ := BuildIntent(tree(), MethodCherryPick, cs, "k")
	rs, err := e.Run(in, cs)
	if err == nil {
		t.Fatal("expected stop")
	}
	if !errors.Is(err, ErrStop) {
		t.Fatalf("%v", err)
	}
	// first applied, second conflict, third not processed
	if len(rs) != 2 || rs[0].Status != "applied" || rs[1].Status != "conflict" {
		t.Fatalf("%+v", rs)
	}
	if !e.IsClosed("w1") || e.IsClosed("w2") || e.IsClosed("w3") {
		t.Fatal("close state")
	}
	atts := e.Attentions()
	if len(atts) == 0 || !atts[0].RequiresOwner {
		t.Fatalf("%+v", atts)
	}
}

func TestChangedParentStops(t *testing.T) {
	e := NewEngine(tree(), "integ-1", nil, time.Now)
	cs := []Candidate{{ID: "c1", WorkItemID: "w1", SourceCommit: "s", IntegrationOrder: 1, Terminal: workgraph.TermSucceeded}}
	in, _ := BuildIntent(WorktreeState{Path: "/tmp/integ", Branch: "b", Head: "parent0"}, MethodApplyPatch, cs, "k")
	// mutate engine head before run
	e.tree.Head = "parentOTHER"
	_, err := e.Run(in, cs)
	if err == nil {
		t.Fatal("expected parent change stop")
	}
}

func TestDirtyStops(t *testing.T) {
	tr := tree()
	tr.Dirty = true
	e := NewEngine(tr, "integ-1", nil, time.Now)
	cs := []Candidate{{ID: "c1", WorkItemID: "w1", SourceCommit: "s", IntegrationOrder: 1, Terminal: workgraph.TermSucceeded}}
	in, _ := BuildIntent(tree(), MethodApplyPatch, cs, "k")
	_, err := e.Run(in, cs)
	if err == nil {
		t.Fatal("dirty")
	}
}

func TestTwoIntegratorsRejected(t *testing.T) {
	tr := tree()
	tr.OwnerID = "other"
	e := NewEngine(tr, "integ-1", nil, time.Now)
	cs := []Candidate{{ID: "c1", WorkItemID: "w1", SourceCommit: "s", IntegrationOrder: 1, Terminal: workgraph.TermSucceeded}}
	in, _ := BuildIntent(tree(), MethodApplyPatch, cs, "k")
	_, err := e.Run(in, cs)
	if err == nil {
		t.Fatal("two integrators")
	}
}

func TestFromWaveCandidates(t *testing.T) {
	cs := []waveschedule.CompletionCandidate{
		{Digest: "d1", WorkItemID: "a", Terminal: workgraph.TermSucceeded, OutputEvidence: "o1", IntegrationOrder: 2},
		{Digest: "d2", WorkItemID: "b", Terminal: workgraph.TermFailed, OutputEvidence: "o2", IntegrationOrder: 1},
	}
	out := FromWaveCandidates(cs)
	if len(out) != 1 || out[0].WorkItemID != "a" {
		t.Fatalf("%+v", out)
	}
}

func TestMissingCommit(t *testing.T) {
	e := NewEngine(tree(), "i", nil, time.Now)
	cs := []Candidate{{ID: "c1", WorkItemID: "w1", SourceCommit: "MISSING_x", IntegrationOrder: 1, Terminal: workgraph.TermSucceeded}}
	in, _ := BuildIntent(tree(), MethodApplyPatch, cs, "k")
	rs, err := e.Run(in, cs)
	if err == nil || len(rs) != 1 || rs[0].Status != "failed" {
		t.Fatalf("%+v %v", rs, err)
	}
}
