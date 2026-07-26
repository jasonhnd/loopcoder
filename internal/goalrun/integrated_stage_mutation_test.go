package goalrun

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// TestStageIntegrated_RequiresIntegratedListAndIntegrateBinding proves terminal
// success alone never sets Stage=integrated, and structural Fake rows without
// integrate event/SHA cannot satisfy product integrate stage or real provider proof.
func TestStageIntegrated_RequiresIntegratedListAndIntegrateBinding(t *testing.T) {
	// Structural fake success: no ArgvDigest, no integrate binding.
	fake := ChildReport{
		ChildID: "wi_impl", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		Terminal: "succeeded", Stage: "terminal", AttemptID: "att-wi_impl-g0",
	}
	if childActuallyExecutedProvider(fake) {
		t.Fatal("structural fake without ArgvDigest must not claim real provider execution")
	}
	intItems := map[string]bool{"wi_impl": true} // Integrated list alone is insufficient
	if promoteIntegratedStage(&fake, intItems) {
		t.Fatal("Integrated membership without integrate event/SHA must not promote Stage=integrated")
	}
	if fake.Stage == "integrated" {
		t.Fatal("structural succeeded child must not acquire Stage=integrated without integrate binding")
	}

	// Terminal succeeded without Integrated membership.
	termOnly := ChildReport{
		ChildID: "wi_x", Terminal: "succeeded", Stage: "terminal",
		AttemptID: "att-x", IntegrateCommitSHA: "sha256:abc", IntegrateEventID: "wev_int",
	}
	if promoteIntegratedStage(&termOnly, map[string]bool{}) {
		t.Fatal("integrate SHA without Integrated list must not promote")
	}

	// Authoritative promote: Integrated + both bindings.
	ok := ChildReport{
		ChildID: "wi_impl", Terminal: "succeeded", Stage: "terminal",
		AttemptID: "att-ok", IntegrateCommitSHA: "deadbeef", IntegrateEventID: "wev_1",
		ArgvDigest: "sha256:deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcdef",
		ActualSources: workflowrun.ActualRouteSources{
			Model: "accepted_invocation", Effort: "accepted_invocation", Permission: "accepted_invocation",
		},
	}
	if !childActuallyExecutedProvider(ok) {
		t.Fatal("real accepted invocation must claim real provider execution")
	}
	if !promoteIntegratedStage(&ok, map[string]bool{"wi_impl": true}) {
		t.Fatal("Integrated + event + SHA must promote Stage=integrated")
	}
	if ok.Stage != "integrated" {
		t.Fatalf("stage=%q want integrated", ok.Stage)
	}

	// MU failed never promotes even if Integrated contains the work item.
	mu := ChildReport{
		ChildID: "wi_impl", Terminal: "failed", FailureClass: "model_unavailable",
		Stage: "terminal", AttemptID: "att-mu",
		IntegrateCommitSHA: "should-clear", IntegrateEventID: "nope",
	}
	if promoteIntegratedStage(&mu, map[string]bool{"wi_impl": true}) {
		t.Fatal("MU failed must never promote to integrated")
	}
	if mu.Stage == "integrated" {
		t.Fatal("MU failed Stage must not be integrated")
	}

	// Real-provider PID gate: structural has no real claim; real does.
	if claimsRealProviderExecution(fake, workflowrun.ChildOutcome{}, false) {
		t.Fatal("structural fake must not require/claim real PID path")
	}
	if !claimsRealProviderExecution(ok, workflowrun.ChildOutcome{}, false) {
		t.Fatal("real success proof must require PID path")
	}
}

// TestStructuralSuccess_NoPRProductProof via usage gate: fake integrated-looking
// rows never green multi-provider paid-provider execution metrics.
func TestStructuralSuccess_NoMultiProviderGreen(t *testing.T) {
	a := ChildReport{
		ChildID: "wi_a", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
		Terminal: "succeeded", Stage: "terminal", // deliberately not integrated
	}
	b := ChildReport{
		ChildID: "wi_b", Provider: "grok", Model: "grok-4.5", Depth: "medium",
		Terminal: "succeeded", Stage: "terminal",
	}
	provs, _, _ := collectUsage([]ChildReport{a, b})
	if len(provs) != 0 {
		t.Fatalf("structural dual success without launch proof must not green multi-provider: %v", provs)
	}
}
