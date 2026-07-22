package waveschedule

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func graphAB() workgraph.Graph {
	g := workgraph.Graph{
		Schema: workgraph.SchemaGraph, ContractVersion: workgraph.ContractVersion,
		GraphID: "g1", Version: 1, Source: workgraph.SourceOwnerApproved,
		ExplicitOptIn: true, ApprovedBy: "owner",
		Items: []workgraph.WorkItem{
			{Schema: workgraph.SchemaItem, ID: "a", Intent: "A", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 1},
			{Schema: workgraph.SchemaItem, ID: "b", Intent: "B", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 2},
			{Schema: workgraph.SchemaItem, ID: "c", Intent: "C", Status: workgraph.ItemRequired,
				Owner: "w", Ownership: workgraph.OwnLoopCoderWorkItem, IntegrationOrder: 3},
		},
		Dependencies: []workgraph.Dependency{
			{Schema: workgraph.SchemaDep, From: "a", To: "b", Kind: workgraph.DepFinishToStart},
		},
		Limits: workgraph.DefaultLimits(),
	}
	g.PlanDigest = workgraph.DigestGraph(g)
	return g
}

func TestPlanDeterministic(t *testing.T) {
	snap := Snapshot{Graph: graphAB(), Bounds: DefaultBounds(), WaveSeq: 1}
	p1, err := PlanWave(snap)
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := PlanWave(snap)
	if p1.Digest != p2.Digest || len(p1.Members) == 0 {
		t.Fatalf("%+v", p1)
	}
	// a and c ready (c has no deps); default WIP=1 → first by order is a
	if p1.Members[0].WorkItemID != "a" {
		t.Fatalf("want a first got %v", p1.Members)
	}
}

func TestParallelAdmitsDisjoint(t *testing.T) {
	b := DefaultBounds()
	b.MaxActiveWorkers = 2
	snap := Snapshot{Graph: graphAB(), Bounds: b, WaveSeq: 1}
	p, _ := PlanWave(snap)
	if len(p.Members) != 2 {
		t.Fatalf("members %v", p.Members)
	}
	// distinct worktrees
	if p.Members[0].WorktreeKey == p.Members[1].WorktreeKey {
		t.Fatal("same worktree")
	}
}

func TestWIPLimit(t *testing.T) {
	snap := Snapshot{
		Graph: graphAB(), Bounds: DefaultBounds(), WaveSeq: 1,
		ActiveWorkItemIDs: []string{"a"},
	}
	p, _ := PlanWave(snap)
	if p.EmptyReason != "wip_full" && len(p.Members) != 0 {
		// with WIP 1 and a active, no more
		t.Fatalf("%+v", p)
	}
}

func TestNoReadyExplanation(t *testing.T) {
	g := graphAB()
	// complete all
	ev := workgraph.TerminalEvidence{"a": workgraph.TermSucceeded, "b": workgraph.TermSucceeded, "c": workgraph.TermSucceeded}
	p, _ := PlanWave(Snapshot{Graph: g, Evidence: ev, Bounds: DefaultBounds(), WaveSeq: 3})
	if p.EmptyReason != "no_ready" {
		t.Fatalf("%+v", p)
	}
	ex := ExplainEmpty(p)
	if ex == "" {
		t.Fatal("explain")
	}
}

func TestPersistAndResume(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	p, _ := PlanWave(Snapshot{Graph: graphAB(), Bounds: DefaultBounds(), WaveSeq: 1})
	p.CreatedAt = now
	got, err := s.PersistPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	// resume same
	got2, err := s.PersistPlan(p)
	if err != nil || got2.Digest != got.Digest {
		t.Fatalf("%+v %v", got2, err)
	}
	// membership change rejected
	p2 := p
	p2.Members = append(p2.Members, WaveMember{WorkItemID: "x", WorktreeKey: "wt:x"})
	p2.Digest = ""
	_, err = s.PersistPlan(p2)
	if err == nil {
		t.Fatal("expected conflict")
	}
}

func TestOutOfOrderCompletionCandidates(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	clock := now
	s := NewStore(func() time.Time { return clock })
	// finish c before a (out of order)
	clock = now.Add(time.Second)
	_, _ = s.Complete("g1", 1, 1, "c", "att_c", workgraph.TermSucceeded, "out_c", 3)
	clock = now.Add(2 * time.Second)
	_, _ = s.Complete("g1", 1, 1, "a", "att_a", workgraph.TermSucceeded, "out_a", 1)
	cands := s.IntegrationCandidates()
	if len(cands) != 2 || cands[0].WorkItemID != "a" || cands[1].WorkItemID != "c" {
		t.Fatalf("ordered by integration: %+v", cands)
	}
	// no mutation of graph/terminals by scheduler — candidates only
}

func TestWorktreeExclusive(t *testing.T) {
	snap := Snapshot{
		Graph: graphAB(), Bounds: ResourceBounds{MaxActiveWorkers: 2, MachineSlots: 8, ProviderSlots: 8, WorktreeAvailable: true},
		WaveSeq:           1,
		AssignedWorktrees: map[string]string{"a": "shared", "c": "shared"},
	}
	p, _ := PlanWave(snap)
	// only one of a/c can take shared tree
	if len(p.Members) != 1 {
		t.Fatalf("want 1 member due to shared tree, got %+v", p.Members)
	}
}
