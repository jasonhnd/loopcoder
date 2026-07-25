package goalrun

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// successProof constructs a ChildReport with durable accepted-invocation proof.
// Unit tests only — never add FakeChildExecutor backdoors.
func successProof(id, provider, model, depth string) ChildReport {
	return ChildReport{
		ChildID: id, Provider: provider, Model: model, Depth: depth,
		Stage: "integrated", Terminal: "succeeded",
		ArgvDigest: "sha256:deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcdef",
		ActualSources: workflowrun.ActualRouteSources{
			Model:      "accepted_invocation",
			Effort:     "accepted_invocation",
			Permission: "accepted_invocation",
		},
	}
}

func TestCollectUsage_FakeIntegratedWithoutProofExcluded(t *testing.T) {
	// FakeChildExecutor shape: integrated+succeeded, no ArgvDigest, empty sources.
	fake := ChildReport{
		ChildID: "wi_research", Provider: "codex", Model: "gpt-5.5", Depth: "low",
		Stage: "integrated", Terminal: "succeeded",
	}
	if childActuallyExecutedProvider(fake) {
		t.Fatal("fake integrated success without ArgvDigest must not count")
	}
	provs, _, _ := collectUsage([]ChildReport{fake})
	if len(provs) != 0 {
		t.Fatalf("providers=%v want empty", provs)
	}
}

func TestCollectUsage_AuthInstallOnlyPreSpawnExcluded(t *testing.T) {
	// Terminal failed with only auth/install bindings — never actual execution.
	c := ChildReport{
		ChildID: "wi_implement", Provider: "grok", Model: "grok-4.5", Depth: "medium",
		Stage: "terminal", Terminal: "failed",
		ActualSources: workflowrun.ActualRouteSources{
			Account: "auth_binding", Install: "install_binding",
		},
	}
	if childActuallyExecutedProvider(c) {
		t.Fatal("auth/install-only failed row must not count")
	}
}

func TestCollectUsage_FailedWithCapacityActualExcluded(t *testing.T) {
	act := 0.1
	c := ChildReport{
		ChildID: "wi_x", Provider: "codex", Terminal: "failed", Stage: "terminal",
		CapacityActual: &act,
		ArgvDigest:     "sha256:abc",
		ActualSources:  workflowrun.ActualRouteSources{Model: "accepted_invocation"},
	}
	if childActuallyExecutedProvider(c) {
		t.Fatal("failed terminal must never count even with capacity actual + sources")
	}
}

func TestCollectUsage_RealAcceptedInvocationIncluded(t *testing.T) {
	c := successProof("wi_research", "codex", "gpt-5.5", "low")
	if !childActuallyExecutedProvider(c) {
		t.Fatal("real accepted invocation must count")
	}
	provs, models, depths := collectUsage([]ChildReport{c})
	if len(provs) != 1 || provs[0] != "codex" {
		t.Fatalf("providers=%v", provs)
	}
	if len(models) != 1 || models[0] != "gpt-5.5" {
		t.Fatalf("models=%v", models)
	}
	if len(depths) != 1 || depths[0] != "low" {
		t.Fatalf("depths=%v", depths)
	}
}

func TestCollectUsage_MultiFlagsFalseUntilTwoProven(t *testing.T) {
	one := successProof("wi_a", "codex", "gpt-5.5", "low")
	provs, models, depths := collectUsage([]ChildReport{one})
	if len(provs) >= 2 || len(models) >= 2 || len(depths) >= 2 {
		t.Fatal("single proven execution is not multi")
	}
	two := successProof("wi_b", "grok", "grok-4.5", "medium")
	provs, models, depths = collectUsage([]ChildReport{one, two})
	if len(provs) < 2 {
		t.Fatalf("want multi provider, got %v", provs)
	}
	if len(models) < 2 {
		t.Fatalf("want multi model, got %v", models)
	}
	if len(depths) < 2 {
		t.Fatalf("want multi depth, got %v", depths)
	}
}

func TestCollectUsage_PlannedAndInstallRefMismatchExcluded(t *testing.T) {
	act := 0.07
	children := []ChildReport{
		successProof("wi_research", "codex", "codex-auto-review", "low"),
		{
			ChildID: "wi_implement", Provider: "grok", Model: "grok-4.5", Depth: "medium",
			Stage: "terminal", Terminal: "failed",
			OutputEvidence: "failed:executor_error:wi_implement",
		},
		{
			ChildID: "wi_tests", Provider: "codex", Model: "codex-auto-review", Depth: "medium",
			Stage: "planned",
		},
		// capacity actual alone on failed must not count
		{
			ChildID: "wi_docs", Provider: "codex", Terminal: "failed", CapacityActual: &act,
		},
	}
	// Fix first child capacity for realism (optional)
	children[0].CapacityActual = &act
	provs, _, _ := collectUsage(children)
	if len(provs) != 1 || provs[0] != "codex" {
		t.Fatalf("providers=%v want only proven codex", provs)
	}
}

func TestCollectUsage_ModelWithoutTruthfulSourceExcludedFromDiversity(t *testing.T) {
	c := successProof("wi_x", "codex", "gpt-5.5", "high")
	// Strip model source — still executed (permission source remains) but model
	// must not enter ModelsUsed without truthful source.
	c.ActualSources.Model = ""
	c.ActualSources.Effort = ""
	// Keep permission as accepted_invocation so execution still counts.
	if !childActuallyExecutedProvider(c) {
		t.Fatal("permission accepted_invocation should still prove execution")
	}
	_, models, depths := collectUsage([]ChildReport{c})
	if len(models) != 0 {
		t.Fatalf("models without truthful source must be empty, got %v", models)
	}
	if len(depths) != 0 {
		t.Fatalf("depths without effort source must be empty, got %v", depths)
	}
}
