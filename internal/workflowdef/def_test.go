package workflowdef

import (
	"bytes"
	"testing"
	"time"
)

func sampleDef() Definition {
	return Definition{
		SchemaVersion: 1,
		GraphID:       "g_demo",
		Source:        "explicit_definition",
		Items: []DefItem{
			{ID: "wi_b", Intent: "second", Status: "required", Owner: "worker", IntegrationOrder: 2},
			{ID: "wi_a", Intent: "first", Status: "required", Owner: "worker", IntegrationOrder: 1},
		},
		Deps: []DefDep{{From: "wi_a", To: "wi_b", Kind: "finish_to_start"}},
	}
}

func TestNormalizeStableDigest(t *testing.T) {
	d := sampleDef()
	p1, err := Normalize(d)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Normalize(d)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Digest == "" || p1.Digest != p2.Digest {
		t.Fatalf("%s vs %s", p1.Digest, p2.Digest)
	}
	j1, _, err := DryRunJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	j2, _, err := DryRunJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(j1, j2) {
		t.Fatal("dry-run not byte-stable")
	}
}

func TestYAMLParse(t *testing.T) {
	raw := []byte(`
schema_version: 1
graph_id: g_yaml
source: explicit_definition
items:
  - id: only
    intent: solo work
    status: required
`)
	d, err := ParseYAML(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Normalize(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || p.Items[0].IntegrationOrder != 1 {
		t.Fatalf("%+v", p)
	}
}

func TestInvalidPaths(t *testing.T) {
	d := sampleDef()
	d.Items[0].Status = "nope"
	if _, err := Normalize(d); err == nil {
		t.Fatal("status")
	}
	d = sampleDef()
	d.Deps = append(d.Deps, DefDep{From: "wi_a", To: "missing", Kind: "finish_to_start"})
	if _, err := Normalize(d); err == nil {
		t.Fatal("missing endpoint")
	}
}

func TestForbiddenSources(t *testing.T) {
	for _, src := range []string{"roadmap_compile", "self_bootstrap", "synthetic_epic"} {
		d := sampleDef()
		d.Source = src
		if _, err := Normalize(d); err == nil {
			t.Fatalf("%s", src)
		}
	}
	if err := RejectImplicitSources("ROADMAP.md"); err == nil {
		t.Fatal("roadmap")
	}
	if err := RejectImplicitSources("github_epic"); err == nil {
		t.Fatal("epic")
	}
}

func TestMaterializeRequiresApprovalDigest(t *testing.T) {
	reg := NewRegistry()
	d := sampleDef()
	plan, err := Normalize(d)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	// wrong digest
	_, err = reg.Materialize("proj", d, Approval{Digest: "sha256:wrong", Actor: "owner"}, now)
	if err == nil {
		t.Fatal("mismatch")
	}
	ap, err := Approve(plan.Digest, "owner", "looks good", now)
	if err != nil {
		t.Fatal(err)
	}
	m1, err := reg.Materialize("proj", d, ap, now)
	if err != nil || m1.Idempotent {
		t.Fatalf("%+v %v", m1, err)
	}
	if m1.Graph.Version != 1 || m1.Graph.ApprovedBy != "owner" {
		t.Fatalf("%+v", m1.Graph)
	}
	// idempotent
	m2, err := reg.Materialize("proj", d, ap, now)
	if err != nil || !m2.Idempotent {
		t.Fatalf("%+v %v", m2, err)
	}
	// changed definition needs new approval
	d2 := d
	d2.Items = append(d2.Items, DefItem{ID: "wi_c", Intent: "third", Status: "optional", IntegrationOrder: 3})
	plan2, _ := Normalize(d2)
	if plan2.Digest == plan.Digest {
		t.Fatal("digest should change")
	}
	_, err = reg.Materialize("proj", d2, ap, now)
	if err == nil {
		t.Fatal("old approval must not apply")
	}
	ap2, _ := Approve(plan2.Digest, "owner", "v2", now)
	m3, err := reg.Materialize("proj", d2, ap2, now)
	if err != nil || m3.Idempotent {
		t.Fatalf("%+v %v", m3, err)
	}
}

func TestDefaultOrders(t *testing.T) {
	d := Definition{
		SchemaVersion: 1,
		Source:        "explicit_definition",
		Items: []DefItem{
			{ID: "z", Intent: "z"},
			{ID: "a", Intent: "a"},
		},
	}
	p, err := Normalize(d)
	if err != nil {
		t.Fatal(err)
	}
	// assigned by id sort: a then z
	if p.Items[0].ID != "a" || p.Items[0].IntegrationOrder != 1 {
		t.Fatalf("%+v", p.Items)
	}
}
