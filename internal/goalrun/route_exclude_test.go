package goalrun_test

import (
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
)

func TestBuildUnavailableRetryRequiresUnclaimedExclude(t *testing.T) {
	if got := goalrun.BuildUnavailableRetryEvidence(nil, ""); got != nil {
		t.Fatal(got)
	}
	// Only claimed excludes → insufficient (cannot prove no-claim exclude).
	got := goalrun.BuildUnavailableRetryEvidence([]goalrun.RouteExclude{
		{ChildID: "a", Provider: "claude", Reason: "unavailable", Claimed: true},
	}, "")
	if got != nil {
		t.Fatalf("claimed-only must not invent evidence: %+v", got)
	}
	// eligible_not_chosen alone never satisfies unavailable_retry.
	if got := goalrun.BuildUnavailableRetryEvidence([]goalrun.RouteExclude{
		{ChildID: "a", Provider: "grok", Reason: "eligible_not_chosen", Claimed: false, Message: "non-winner"},
	}, "att-x"); got != nil {
		t.Fatalf("eligible_not_chosen must not satisfy: %+v", got)
	}
	got = goalrun.BuildUnavailableRetryEvidence([]goalrun.RouteExclude{
		{ChildID: "a", Provider: "claude", Reason: "unavailable", Claimed: false, Message: "probe timeout"},
		{ChildID: "b", Provider: "codex", Reason: "exhausted", Claimed: false},
		{ChildID: "c", Provider: "grok", Reason: "eligible_not_chosen", Claimed: false},
	}, "att-retry-1")
	if got == nil {
		t.Fatal("want evidence")
	}
	if got.ExcludedProvider != "claude" || got.ExcludedReason != "unavailable" {
		t.Fatalf("%+v", got)
	}
	if !got.NoDuplicateClaim || got.EvidenceRef == "" || !strings.Contains(got.EvidenceRef, "route_exclude_set") {
		t.Fatalf("%+v", got)
	}
	if got.RetryAttemptID != "att-retry-1" {
		t.Fatalf("retry %q", got.RetryAttemptID)
	}
}

func TestClassifyExcludeReason(t *testing.T) {
	if goalrun.ClassifyExcludeReason("capacity_refused", "", "") != "exhausted" {
		t.Fatal("capacity")
	}
	if goalrun.ClassifyExcludeReason("", "", "stale window") != "stale" {
		t.Fatal("stale")
	}
	if goalrun.ClassifyExcludeReason("", "", "soft excluded reserve.breach") != "soft_excluded" {
		t.Fatal("soft")
	}
}

func TestSoftExcludedEligibleExcludesAfterSuccessfulRoute(t *testing.T) {
	// Winner codex; antigravity hard-eligible but soft-excluded → Claimed=false exclude.
	got := goalrun.SoftExcludedEligibleExcludes("wi_implement", "codex", []goalrun.SoftExcludedCandidate{
		{Provider: "codex", Model: "gpt-5.5", HardEligible: true, SoftExcluded: false},
		{Provider: "antigravity", Model: "GPT-OSS 120B", HardEligible: true, SoftExcluded: true},
		{Provider: "antigravity", Model: "Gemini 3.1 Pro", HardEligible: true, SoftExcluded: true}, // dedup
		{Provider: "grok", Model: "x", HardEligible: false, SoftExcluded: true},                    // hard-ineligible ignored
	})
	if len(got) != 1 {
		t.Fatalf("want 1 exclude, got %+v", got)
	}
	if got[0].Provider != "antigravity" || got[0].Reason != "soft_excluded" || got[0].Claimed {
		t.Fatalf("%+v", got[0])
	}
	if !got[0].HardEligible || !got[0].SoftExcluded {
		t.Fatalf("flags %+v", got[0])
	}
	// soft_excluded is not hard unavailability — must not satisfy canary metric.
	if ev := goalrun.BuildUnavailableRetryEvidence(got, "att-wi_tests-1"); ev != nil {
		t.Fatalf("soft_excluded must not build unavailable_retry: %+v", ev)
	}
	// Winner-only decision → no exclude invented.
	if goalrun.SoftExcludedEligibleExcludes("wi_x", "codex", []goalrun.SoftExcludedCandidate{
		{Provider: "codex", HardEligible: true, SoftExcluded: false},
	}) != nil {
		t.Fatal("must not invent")
	}
}

func TestHardEligibleNonWinnerExcludesIncludesNotChosen(t *testing.T) {
	// Both hard-eligible SoftExcluded=false; winner codex → antigravity eligible_not_chosen.
	got := goalrun.HardEligibleNonWinnerExcludes("wi_research", "codex", []goalrun.SoftExcludedCandidate{
		{Provider: "codex", Model: "gpt-5.5", HardEligible: true, SoftExcluded: false},
		{Provider: "antigravity", Model: "GPT-OSS 120B", HardEligible: true, SoftExcluded: false},
		{Provider: "grok", Model: "x", HardEligible: false, SoftExcluded: false},
	})
	if len(got) != 1 {
		t.Fatalf("want 1 exclude, got %+v", got)
	}
	if got[0].Provider != "antigravity" || got[0].Reason != "eligible_not_chosen" || got[0].Claimed || got[0].SoftExcluded {
		t.Fatalf("%+v", got[0])
	}
	if !got[0].HardEligible {
		t.Fatalf("hard flag %+v", got[0])
	}
	// eligible_not_chosen is diversity measurement — must NOT satisfy unavailable_retry.
	if ev := goalrun.BuildUnavailableRetryEvidence(got, "att-wi_tests-1"); ev != nil {
		t.Fatalf("eligible_not_chosen must not build unavailable_retry: %+v", ev)
	}
	// SoftExcluded still preferred when present.
	got2 := goalrun.HardEligibleNonWinnerExcludes("wi_x", "codex", []goalrun.SoftExcludedCandidate{
		{Provider: "codex", HardEligible: true, SoftExcluded: false},
		{Provider: "antigravity", HardEligible: true, SoftExcluded: true},
	})
	if len(got2) != 1 || got2[0].Reason != "soft_excluded" {
		t.Fatalf("%+v", got2)
	}
}
