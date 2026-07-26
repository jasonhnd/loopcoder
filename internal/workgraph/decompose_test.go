package workgraph_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func TestDecomposeGoalAtLeastFourChildren(t *testing.T) {
	now := time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC)
	g, err := workgraph.DecomposeGoal(workgraph.DecomposeOptions{
		Goal:  "implement capacity-aware routing for multi-provider run",
		Issue: "1341", Actor: "owner", Owner: "worker", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Items) < 4 {
		t.Fatalf("items=%d", len(g.Items))
	}
	if g.Source != workgraph.SourceGoalDecompose {
		t.Fatalf("source=%s", g.Source)
	}
	if g.Limits.MaxParallel > 2 {
		t.Fatalf("max parallel=%d", g.Limits.MaxParallel)
	}
	if g.PlanDigest == "" {
		t.Fatal("missing plan digest")
	}
	// required roles
	ids := map[string]bool{}
	for _, it := range g.Items {
		ids[it.ID] = true
		if it.ID == "wi_tests" {
			for _, want := range []string{
				"implement capacity-aware routing for multi-provider run",
				"run the repository's relevant test commands",
				"retain generated dependency lock evidence",
			} {
				if !strings.Contains(it.Intent, want) {
					t.Fatalf("tests intent %q missing original goal/validation requirement %q", it.Intent, want)
				}
			}
		}
		if it.Ownership != workgraph.OwnLoopCoderWorkItem {
			t.Fatalf("ownership %s", it.Ownership)
		}
	}
	for _, id := range []string{"wi_research", "wi_implement", "wi_tests", "wi_verify"} {
		if !ids[id] {
			t.Fatalf("missing %s", id)
		}
	}
	// stable
	g2, err := workgraph.DecomposeGoal(workgraph.DecomposeOptions{
		Goal:  "implement capacity-aware routing for multi-provider run",
		Issue: "1341", Actor: "owner", Owner: "worker", Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.PlanDigest != g2.PlanDigest {
		t.Fatalf("digest unstable %s vs %s", g.PlanDigest, g2.PlanDigest)
	}
}

func TestDecomposeDocsSkipsOptionalDocsChild(t *testing.T) {
	g, err := workgraph.DecomposeGoal(workgraph.DecomposeOptions{
		Goal: "docs: fix README typo", Actor: "owner", Now: time.Date(2026, 7, 22, 20, 1, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Items) < 4 {
		t.Fatalf("still need ≥4 required children, got %d", len(g.Items))
	}
	for _, it := range g.Items {
		if it.ID == "wi_docs" {
			t.Fatal("docs goal should not add optional docs child")
		}
	}
}

func TestDecomposeTestsChildRetainsLateGoalValidationRequirements(t *testing.T) {
	goal := strings.Repeat("bounded implementation scope ", 12) +
		"finally run go mod tidy and go test ./... and commit go.sum"
	if len(goal) <= 120 {
		t.Fatalf("fixture must exceed old 120-byte truncation: %d", len(goal))
	}
	g, err := workgraph.DecomposeGoal(workgraph.DecomposeOptions{
		Goal: goal, Actor: "owner", Now: time.Date(2026, 7, 22, 20, 2, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range g.Items {
		if item.ID != "wi_tests" {
			continue
		}
		if !strings.Contains(item.Intent, "run go mod tidy and go test ./... and commit go.sum") {
			t.Fatalf("tests child lost late goal requirements: %q", item.Intent)
		}
		return
	}
	t.Fatal("missing wi_tests")
}

func TestDecomposeRejectsEmpty(t *testing.T) {
	_, err := workgraph.DecomposeGoal(workgraph.DecomposeOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
}
