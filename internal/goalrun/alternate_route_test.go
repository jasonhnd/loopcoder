package goalrun_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
)

func TestPickAlternateRouteSameDepth_RequiresMatchingEffort(t *testing.T) {
	cands := []goalrun.RouteCandidate{
		{Provider: "antigravity", Model: "gpt-oss-120b-medium", Effort: "medium", HardEligible: true},
		{Provider: "antigravity", Model: "gemini-3.1-pro-low", Effort: "low", HardEligible: true},
		{Provider: "grok", Model: "grok-4.5", Effort: "medium", HardEligible: true},
	}
	// reqDepth=low must not pick gpt-oss-medium and stamp Depth=low.
	got := goalrun.PickAlternateRouteSameDepth(cands, "antigravity", "gemini-3.1-pro-low", "low")
	if got.Provider != "" {
		// Only remaining low-depth candidates after excluding gemini-low itself: none
		// (gpt-oss is medium, grok is medium).
		t.Fatalf("must not pick wrong-depth model: %+v", got)
	}
	got = goalrun.PickAlternateRouteSameDepth(cands, "antigravity", "gpt-oss-120b-medium", "medium")
	if got.Provider != "grok" || got.Model != "grok-4.5" || got.Depth != "medium" {
		t.Fatalf("want grok medium alternate, got %+v", got)
	}
}

func TestPickAlternateRouteSameDepth_NoSilentDepthRewrite(t *testing.T) {
	cands := []goalrun.RouteCandidate{
		{Provider: "antigravity", Model: "gpt-oss-120b-medium", Effort: "medium", HardEligible: true},
	}
	got := goalrun.PickAlternateRouteSameDepth(cands, "codex", "gpt-5.5", "low")
	if got.Provider != "" {
		t.Fatalf("must not rewrite medium candidate to low: %+v", got)
	}
}

func TestPickAlternateRouteSameDepth_SkipsSoftExcludedAndSameRoute(t *testing.T) {
	cands := []goalrun.RouteCandidate{
		{Provider: "antigravity", Model: "gpt-oss-120b-medium", Effort: "medium", HardEligible: true, SoftExcluded: false},
		{Provider: "grok", Model: "grok-4.5", Effort: "medium", HardEligible: true, SoftExcluded: true},
		{Provider: "codex", Model: "gpt-5.5", Effort: "medium", HardEligible: true},
	}
	got := goalrun.PickAlternateRouteSameDepth(cands, "antigravity", "gpt-oss-120b-medium", "medium")
	if got.Provider != "codex" || got.Depth != "medium" {
		t.Fatalf("%+v", got)
	}
	// Same route is not an alternate.
	got = goalrun.PickAlternateRouteSameDepth(cands, "codex", "gpt-5.5", "medium")
	if got.Provider != "antigravity" {
		t.Fatalf("want antigravity as only remaining hard-eligible, got %+v", got)
	}
}

func TestPickAlternateRequiresNonemptyPermissionWhenRequired(t *testing.T) {
	cands := []goalrun.RouteCandidate{
		{Provider: "codex", Model: "gpt-5.5", Effort: "medium", Permission: "", HardEligible: true},
		{Provider: "claude", Model: "c", Effort: "medium", Permission: "bounded_write", HardEligible: true},
	}
	got := goalrun.PickAlternateRouteSameDepthPerm(cands, "antigravity", "x", "medium", "bounded_write")
	if got.Provider != "claude" {
		t.Fatalf("empty perm must skip: %+v", got)
	}
	// When reqPerm empty, empty cand perm may pass depth-only path via PickAlternateRouteSameDepth
	got2 := goalrun.PickAlternateRouteSameDepth(cands, "antigravity", "x", "medium")
	if got2.Provider == "" {
		t.Fatalf("depth-only should still pick: %+v", got2)
	}
}
