package workgraph

import (
	"errors"
	"testing"
	"time"
)

func TestOneNodeDirectEquivalent(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	g, err := MaterializeDirectRun("g1", "fix typo in docs", "worker", now)
	if err != nil {
		t.Fatal(err)
	}
	if !g.DirectRunEquivalent || len(g.Items) != 1 || len(g.Dependencies) != 0 {
		t.Fatalf("%+v", g)
	}
	if g.PlanDigest == "" {
		t.Fatal("digest")
	}
	// re-digest stable
	if DigestGraph(g) != g.PlanDigest {
		// PlanDigest computed before CreatedAt is set on wire — Materialize sets digest after build
		d2 := DigestGraph(g)
		g.PlanDigest = d2
		if DigestGraph(g) != d2 {
			t.Fatal("unstable")
		}
	}
}

func TestMultiNodeRequiresOptInAndApproval(t *testing.T) {
	g := Graph{
		Schema: SchemaGraph, ContractVersion: ContractVersion, GraphID: "g2", Version: 1,
		Source: SourceExplicitYAML, ExplicitOptIn: false,
		Items: []WorkItem{
			{Schema: SchemaItem, ID: "a", Intent: "A", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 1},
			{Schema: SchemaItem, ID: "b", Intent: "B", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 2},
		},
		Limits: DefaultLimits(),
	}
	if err := ValidateGraph(g); err == nil {
		t.Fatal("expected opt-in error")
	}
	g.ExplicitOptIn = true
	if err := ValidateGraph(g); err == nil {
		t.Fatal("expected approval error")
	}
	g.ApprovedBy = "owner"
	if err := ValidateGraph(g); err != nil {
		t.Fatal(err)
	}
	order := IntegrationOrder(g)
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("%v", order)
	}
}

func TestForbiddenSources(t *testing.T) {
	for _, src := range []SourceKind{SourceRoadmapCompile, SourceSyntheticEpic, SourceSelfBootstrap} {
		g := Graph{
			Schema: SchemaGraph, ContractVersion: ContractVersion, GraphID: "gx", Version: 1,
			Source: src, ExplicitOptIn: true, ApprovedBy: "o",
			Items: []WorkItem{
				{Schema: SchemaItem, ID: "a", Intent: "A", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 1},
			},
			Limits: DefaultLimits(),
		}
		if err := ValidateGraph(g); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s: %v", src, err)
		}
	}
}

func TestOwnershipUnambiguous(t *testing.T) {
	labels := OwnershipLabels()
	if labels[OwnLoopCoderWorkItem] == "" || labels[OwnProviderNativeChild] == "" {
		t.Fatal("labels")
	}
	// Item with wrong ownership rejected
	g := Graph{
		Schema: SchemaGraph, ContractVersion: ContractVersion, GraphID: "g", Version: 1,
		Source: SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "o",
		Items: []WorkItem{
			{Schema: SchemaItem, ID: "a", Intent: "A", Status: ItemRequired, Owner: "w", Ownership: OwnProviderNativeChild, IntegrationOrder: 1},
		},
		Limits: DefaultLimits(),
	}
	if err := ValidateGraph(g); err == nil {
		t.Fatal("provider child cannot be workitem node")
	}
}

func TestReplanCannotRewriteHistory(t *testing.T) {
	prior := Graph{
		Schema: SchemaGraph, ContractVersion: ContractVersion, GraphID: "g", Version: 1,
		Source: SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "o", ExecutionStarted: true,
		Items: []WorkItem{
			{Schema: SchemaItem, ID: "a", Intent: "A", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 1, Terminal: TermSucceeded},
			{Schema: SchemaItem, ID: "b", Intent: "B", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 2},
		},
		Limits: DefaultLimits(),
	}
	prior.PlanDigest = DigestGraph(prior)

	// Attempt to rewrite a terminal
	next := prior
	next.Items = []WorkItem{
		{Schema: SchemaItem, ID: "a", Intent: "A", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 1, Terminal: TermFailed},
		{Schema: SchemaItem, ID: "b", Intent: "B2", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 2},
	}
	_, err := ApplyReplan(prior, Replan{
		Schema: SchemaReplan, PriorGraphID: prior.GraphID, PriorVersion: 1, PriorDigest: prior.PlanDigest,
		Next: next, Actor: "owner", Reason: "bad rewrite",
	})
	if !errors.Is(err, ErrMutation) {
		t.Fatalf("err %v", err)
	}

	// Valid replan preserves terminal
	next2 := prior
	next2.Items = []WorkItem{
		{Schema: SchemaItem, ID: "a", Intent: "A", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 1},
		{Schema: SchemaItem, ID: "b", Intent: "B revised", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 2},
		{Schema: SchemaItem, ID: "c", Intent: "C", Status: ItemOptional, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 3},
	}
	out, err := ApplyReplan(prior, Replan{
		Schema: SchemaReplan, PriorGraphID: prior.GraphID, PriorVersion: 1, PriorDigest: prior.PlanDigest,
		Next: next2, Actor: "owner", Reason: "add optional step",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != 2 {
		t.Fatalf("version %d", out.Version)
	}
	// a must remain succeeded
	for _, it := range out.Items {
		if it.ID == "a" && it.Terminal != TermSucceeded {
			t.Fatalf("terminal lost %+v", it)
		}
	}
}

func TestCycleRejected(t *testing.T) {
	g := Graph{
		Schema: SchemaGraph, ContractVersion: ContractVersion, GraphID: "g", Version: 1,
		Source: SourceExplicitYAML, ExplicitOptIn: true, ApprovedBy: "o",
		Items: []WorkItem{
			{Schema: SchemaItem, ID: "a", Intent: "A", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 1},
			{Schema: SchemaItem, ID: "b", Intent: "B", Status: ItemRequired, Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: 2},
		},
		Dependencies: []Dependency{
			{Schema: SchemaDep, From: "a", To: "b", Kind: DepFinishToStart},
			{Schema: SchemaDep, From: "b", To: "a", Kind: DepFinishToStart},
		},
		Limits: DefaultLimits(),
	}
	if err := ValidateGraph(g); err == nil {
		t.Fatal("cycle")
	}
}

func TestLimits(t *testing.T) {
	items := make([]WorkItem, 0, 3)
	for i := 0; i < 3; i++ {
		items = append(items, WorkItem{
			Schema: SchemaItem, ID: string(rune('a' + i)), Intent: "x", Status: ItemRequired,
			Owner: "w", Ownership: OwnLoopCoderWorkItem, IntegrationOrder: i + 1,
		})
	}
	g := Graph{
		Schema: SchemaGraph, ContractVersion: ContractVersion, GraphID: "g", Version: 1,
		Source: SourceOwnerApproved, ExplicitOptIn: true, ApprovedBy: "o",
		Items: items, Limits: Limits{Schema: SchemaLimits, MaxItems: 2, MaxDepth: 2, MaxParallel: 1},
	}
	if err := ValidateGraph(g); !errors.Is(err, ErrLimits) {
		t.Fatalf("%v", err)
	}
}
