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
	got = goalrun.BuildUnavailableRetryEvidence([]goalrun.RouteExclude{
		{ChildID: "a", Provider: "claude", Reason: "unavailable", Claimed: false, Message: "probe timeout"},
		{ChildID: "b", Provider: "codex", Reason: "exhausted", Claimed: false},
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
	// Feeds BuildUnavailableRetryEvidence.
	ev := goalrun.BuildUnavailableRetryEvidence(got, "att-wi_tests-1")
	if ev == nil || ev.ExcludedProvider != "antigravity" || ev.ExcludedReason != "soft_excluded" {
		t.Fatalf("unavail %+v", ev)
	}
	if !ev.NoDuplicateClaim || !ev.NoDuplicateFiles || !ev.NoDoubleCapacity || ev.EvidenceRef == "" {
		t.Fatalf("dup flags %+v", ev)
	}
	// Winner-only decision → no exclude invented.
	if goalrun.SoftExcludedEligibleExcludes("wi_x", "codex", []goalrun.SoftExcludedCandidate{
		{Provider: "codex", HardEligible: true, SoftExcluded: false},
	}) != nil {
		t.Fatal("must not invent")
	}
}
