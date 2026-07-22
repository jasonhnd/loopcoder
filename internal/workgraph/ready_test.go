package workgraph

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func item(id string, status ItemStatus, order int) WorkItem {
	return WorkItem{
		Schema: SchemaItem, ID: id, Intent: "intent-" + id, Status: status,
		Owner: "worker", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: order,
	}
}

func multi(items []WorkItem, deps []Dependency) Graph {
	g := Graph{
		Schema: SchemaGraph, ContractVersion: ContractVersion,
		GraphID: "g_test", Version: 1,
		Source: SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "owner",
		Items: items, Dependencies: deps, Limits: DefaultLimits(),
	}
	g.PlanDigest = DigestGraph(g)
	return g
}

func TestValidateExecutableRejectsCyclesAndLimits(t *testing.T) {
	// self-loop
	g := multi([]WorkItem{item("a", ItemRequired, 1)}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "a", Kind: DepFinishToStart},
	})
	if err := ValidateExecutable(g); err == nil {
		t.Fatal("self-loop")
	}

	// multi-node cycle
	g = multi([]WorkItem{item("a", ItemRequired, 1), item("b", ItemRequired, 2)}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "b", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "b", To: "a", Kind: DepFinishToStart},
	})
	if err := ValidateExecutable(g); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle: %v", err)
	}

	// missing endpoint
	g = multi([]WorkItem{item("a", ItemRequired, 1)}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "missing", Kind: DepFinishToStart},
	})
	if err := ValidateExecutable(g); err == nil {
		t.Fatal("missing endpoint")
	}

	// duplicate edge
	g = multi([]WorkItem{item("a", ItemRequired, 1), item("b", ItemRequired, 2)}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "b", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "a", To: "b", Kind: DepFinishToStart},
	})
	if err := ValidateExecutable(g); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("dup: %v", err)
	}

	// required depends on optional
	g = multi([]WorkItem{item("opt", ItemOptional, 1), item("req", ItemRequired, 2)}, []Dependency{
		{Schema: SchemaDep, From: "opt", To: "req", Kind: DepFinishToStart},
	})
	if err := ValidateExecutable(g); err == nil || !strings.Contains(err.Error(), "optional") {
		t.Fatalf("req<-opt: %v", err)
	}

	// node count limit
	g = multi([]WorkItem{item("a", ItemRequired, 1), item("b", ItemRequired, 2)}, nil)
	g.Limits = Limits{Schema: SchemaLimits, MaxItems: 1, MaxDepth: 8, MaxParallel: 4}
	if err := ValidateExecutable(g); !errors.Is(err, ErrLimits) {
		t.Fatalf("limits: %v", err)
	}

	// output order: producer must integrate before consumer
	g = multi([]WorkItem{item("prod", ItemRequired, 2), item("cons", ItemRequired, 1)}, []Dependency{
		{Schema: SchemaDep, From: "prod", To: "cons", Kind: DepOutput},
	})
	if err := ValidateExecutable(g); err == nil {
		t.Fatal("output order")
	}
}

func TestReadySetDeterministic(t *testing.T) {
	g := multi([]WorkItem{
		item("a", ItemRequired, 1),
		item("b", ItemRequired, 2),
		item("c", ItemRequired, 3),
	}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "b", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "b", To: "c", Kind: DepFinishToStart},
	})
	r1 := EvaluateReady(g, nil)
	r2 := EvaluateReady(g, nil)
	if !r1.Valid || r1.Digest != r2.Digest {
		t.Fatalf("digest %s vs %s", r1.Digest, r2.Digest)
	}
	// only a ready initially
	if len(r1.Ready) != 1 || r1.Ready[0] != "a" {
		t.Fatalf("ready %v", r1.Ready)
	}
	// after a succeeds, b ready
	r3 := EvaluateReady(g, TerminalEvidence{"a": TermSucceeded})
	if len(r3.Ready) != 1 || r3.Ready[0] != "b" {
		t.Fatalf("after a: %v", r3.Ready)
	}
	// after a+b, c ready
	r4 := EvaluateReady(g, TerminalEvidence{"a": TermSucceeded, "b": TermSucceeded})
	if len(r4.Ready) != 1 || r4.Ready[0] != "c" {
		t.Fatalf("after ab: %v", r4.Ready)
	}
	// all terminal → empty ready
	r5 := EvaluateReady(g, TerminalEvidence{"a": TermSucceeded, "b": TermSucceeded, "c": TermSucceeded})
	if len(r5.Ready) != 0 {
		t.Fatalf("all done: %v", r5.Ready)
	}
}

func TestRequiredFailureBlocks(t *testing.T) {
	g := multi([]WorkItem{
		item("a", ItemRequired, 1),
		item("b", ItemRequired, 2),
	}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "b", Kind: DepFinishToStart},
	})
	r := EvaluateReady(g, TerminalEvidence{"a": TermFailed})
	if ReadyContains(r, "b") {
		t.Fatal("b must not be ready")
	}
	var b ItemState
	for _, st := range r.Items {
		if st.ID == "b" {
			b = st
		}
	}
	if b.Life != LifeBlocked {
		t.Fatalf("b life %s reasons %v", b.Life, b.Reasons)
	}
}

func TestOptionalSoftOrderDoesNotBlock(t *testing.T) {
	// soft_order does not gate readiness
	g := multi([]WorkItem{
		item("a", ItemRequired, 1),
		item("b", ItemRequired, 2),
	}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "b", Kind: DepSoft},
	})
	r := EvaluateReady(g, nil)
	if len(r.Ready) != 2 {
		t.Fatalf("both ready with soft only: %v", r.Ready)
	}
	// stable order by integration_order
	if r.Ready[0] != "a" || r.Ready[1] != "b" {
		t.Fatalf("order %v", r.Ready)
	}
}

func TestInvalidMaterializeNoPartial(t *testing.T) {
	g := multi([]WorkItem{item("a", ItemRequired, 1)}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "a", Kind: DepFinishToStart},
	})
	_, err := MaterializeIfValid(g)
	if err == nil {
		t.Fatal("expected invalid")
	}
	// EvaluateReady on invalid yields empty ready and Valid=false
	r := EvaluateReady(g, nil)
	if r.Valid || len(r.Ready) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestReplayAfterRestart(t *testing.T) {
	g := multi([]WorkItem{
		item("z", ItemRequired, 2),
		item("a", ItemRequired, 1),
	}, nil)
	// independent of insertion order
	ev := TerminalEvidence{"a": TermSucceeded}
	r1 := EvaluateReady(g, ev)
	// "reload" graph from digest-stable reconstruction
	g2 := g
	g2.Items = []WorkItem{item("a", ItemRequired, 1), item("z", ItemRequired, 2)}
	g2.PlanDigest = DigestGraph(g2)
	// plan digest may differ if item order in wire sorted — DigestGraph sorts by id
	r2 := EvaluateReady(g2, ev)
	// ready sets should match (only z remaining)
	if len(r1.Ready) != 1 || r1.Ready[0] != "z" {
		t.Fatalf("r1 %v", r1.Ready)
	}
	if len(r2.Ready) != 1 || r2.Ready[0] != "z" {
		t.Fatalf("r2 %v", r2.Ready)
	}
	// digests equal when plan digest equal
	g.PlanDigest = DigestGraph(g)
	g2.PlanDigest = DigestGraph(g2)
	if g.PlanDigest != g2.PlanDigest {
		t.Fatalf("plan digests %s %s", g.PlanDigest, g2.PlanDigest)
	}
	r1 = EvaluateReady(g, ev)
	r2 = EvaluateReady(g2, ev)
	if r1.Digest != r2.Digest {
		t.Fatalf("ready digests %s vs %s", r1.Digest, r2.Digest)
	}
}

func TestFanOutAndDepthLimits(t *testing.T) {
	// depth 3 chain with max_depth 2
	g := multi([]WorkItem{
		item("a", ItemRequired, 1), item("b", ItemRequired, 2), item("c", ItemRequired, 3),
	}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "b", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "b", To: "c", Kind: DepFinishToStart},
	})
	g.Limits = Limits{Schema: SchemaLimits, MaxItems: 32, MaxDepth: 2, MaxParallel: 8}
	if err := ValidateExecutable(g); !errors.Is(err, ErrLimits) {
		t.Fatalf("depth: %v", err)
	}

	// fan-out: a -> b,c,d with max_parallel 2
	g = multi([]WorkItem{
		item("a", ItemRequired, 1), item("b", ItemRequired, 2),
		item("c", ItemRequired, 3), item("d", ItemRequired, 4),
	}, []Dependency{
		{Schema: SchemaDep, From: "a", To: "b", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "a", To: "c", Kind: DepFinishToStart},
		{Schema: SchemaDep, From: "a", To: "d", Kind: DepFinishToStart},
	})
	g.Limits = Limits{Schema: SchemaLimits, MaxItems: 32, MaxDepth: 8, MaxParallel: 2}
	if err := ValidateExecutable(g); !errors.Is(err, ErrLimits) {
		t.Fatalf("fanout: %v", err)
	}
}

func TestOneNodeDirectReady(t *testing.T) {
	g, err := MaterializeDirectRun("g1", "docs", "worker", time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	r := EvaluateReady(g, nil)
	if !r.Valid || len(r.Ready) != 1 {
		t.Fatalf("%+v", r)
	}
}
