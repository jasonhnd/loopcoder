package goalrun

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestCollectUsage_PlannedAndPreSpawnFailNeverCount(t *testing.T) {
	// Research integrated success counts; Grok install_ref mismatch before PID
	// does not; planned codex pins do not. Multi-provider must not claim grok.
	act := 0.07
	children := []ChildReport{
		{
			ChildID: "wi_research", Provider: "codex", Model: "codex-auto-review", Depth: "low",
			Stage: "integrated", Terminal: "succeeded",
			ArgvDigest: "sha256:abc", CapacityActual: &act,
			ActualSources: workflowrun.ActualRouteSources{
				Model: "provider_stream", Install: "install_binding",
			},
		},
		{
			ChildID: "wi_implement", Provider: "grok", Model: "grok-4.5", Depth: "medium",
			Stage: "terminal", Terminal: "failed",
			// Pre-spawn route_mismatch: no argv, no capacity actual, empty sources.
			OutputEvidence: "failed:executor_error:wi_implement",
		},
		{
			ChildID: "wi_tests", Provider: "codex", Model: "codex-auto-review", Depth: "medium",
			Stage: "planned",
		},
	}
	provs, models, depths := collectUsage(children)
	if len(provs) != 1 || provs[0] != "codex" {
		t.Fatalf("providers=%v want only codex (grok pre-spawn fail must not count)", provs)
	}
	if len(models) != 1 || models[0] != "codex-auto-review" {
		t.Fatalf("models=%v", models)
	}
	if len(depths) != 1 || depths[0] != "low" {
		t.Fatalf("depths=%v", depths)
	}
	if len(provs) >= 2 {
		t.Fatal("must not report multi-provider from planned/failed-launch rows")
	}
}

func TestChildActuallyExecutedProvider_IntegratedSuccessWithoutArgv(t *testing.T) {
	c := ChildReport{
		ChildID: "wi_docs", Provider: "codex", Stage: "integrated", Terminal: "succeeded",
	}
	if !childActuallyExecutedProvider(c) {
		t.Fatal("integrated success must count as actual execution")
	}
	c.Stage = "planned"
	c.Terminal = ""
	if childActuallyExecutedProvider(c) {
		t.Fatal("planned must not count")
	}
}
