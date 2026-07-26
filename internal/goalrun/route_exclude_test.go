package goalrun_test

import (
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
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

func TestClaimedModelUnavailableTakesPrecedenceOverUnclaimed(t *testing.T) {
	// Claimed MU + unclaimed exhausted → must not satisfy via unclaimed path when proof nil.
	ex := []goalrun.RouteExclude{
		{ChildID: "wi_x", Provider: "antigravity", Reason: "model_unavailable", Claimed: true},
		{ChildID: "wi_x", Provider: "codex", Reason: "exhausted", Claimed: false},
	}
	if got := goalrun.BuildUnavailableRetryEvidenceWithProof(ex, "att-g1", nil); got != nil {
		t.Fatalf("claimed MU without proof must nil (not unclaimed fallback): %+v", got)
	}
	// Two claimed MU → fail (exactly one required).
	ex2 := []goalrun.RouteExclude{
		{ChildID: "a", Provider: "antigravity", Reason: "model_unavailable", Claimed: true},
		{ChildID: "b", Provider: "codex", Reason: "model_unavailable", Claimed: true},
	}
	if got := goalrun.BuildUnavailableRetryEvidenceWithProof(ex2, "att", &goalrun.UnavailableRetryProof{
		FailedAttemptID: "x", RetryAttemptID: "y", WorkItemID: "a", FailedProvider: "antigravity",
	}); got != nil {
		t.Fatalf("two claimed MU must nil: %+v", got)
	}
}

func TestBuildUnavailableRetryFromClaimedModelUnavailableRequiresProof(t *testing.T) {
	// Claimed-only prose/event_ref → nil (must not invent no_dup flags).
	if got := goalrun.BuildUnavailableRetryEvidence([]goalrun.RouteExclude{
		{ChildID: "wi_x", Provider: "antigravity", Reason: "model_unavailable", Claimed: true,
			Message: "event_id=wev1;event_id=wev2;supersedes=att-g0"},
	}, "att-g1"); got != nil {
		t.Fatalf("prose/event_ref only must not invent: %+v", got)
	}
	// Concrete proof required.
	proof := &goalrun.UnavailableRetryProof{
		FailedAttemptID: "att-g0", RetryAttemptID: "att-g1",
		WorkItemID: "wi_x", FailedProvider: "antigravity",
		FailedClaimCount: 1, RetryClaimCount: 1,
		FailedLaunchCount: 1, RetryLaunchCount: 1,
		FailedIntegrateCount: 0, RetryIntegrateCount: 1,
		FailedTerminalCount: 1, RetryTerminalCount: 1,
		FailedClaimClosed: true, RetryClaimClosed: true,
		FailedIntegrated: false, RetryIntegrated: true,
		FailedProductFiles: []string{"notes/bad.md"},
		RetryProductFiles:  []string{"notes/good.md"},
		PriorTransition: workflowrun.CapacityTransition{
			AttemptID: "att-g0", Role: "prior", State: "released",
			Provider: "antigravity", Model: "bad", Depth: "medium",
			AccountRef: "acct-ag", WindowKind: "five_hour",
			ReservationID: "res-prior", Actual: nil, Source: "",
		},
		AlternateTransition: workflowrun.CapacityTransition{
			AttemptID: "att-g1", Role: "alternate", State: "released",
			Provider: "codex", Model: "gpt-5.5", Depth: "medium",
			AccountRef: "acct-codex", WindowKind: "five_hour",
			ReservationID: "res-alt", Actual: nil, Source: "",
		},
		ModelUnavailableEvent: goalrun.EventSnapshot{EventID: "wev_1", Kind: "model_unavailable", AttemptID: "att-g0"},
		FailedTerminalEvent:   goalrun.EventSnapshot{EventID: "wev_0", Kind: "terminal", AttemptID: "att-g0"},
		ClaimEvent:            goalrun.EventSnapshot{EventID: "wev_2", Kind: "claim", AttemptID: "att-g1"},
		RerouteEvent:          goalrun.EventSnapshot{EventID: "wev_3", Kind: "reroute", AttemptID: "att-g1"},
		LaunchEvent:           goalrun.EventSnapshot{EventID: "wev_4", Kind: "launch", AttemptID: "att-g1"},
		RetryTerminalEvent:    goalrun.EventSnapshot{EventID: "wev_5", Kind: "terminal", AttemptID: "att-g1"},
		IntegrateEvent:        goalrun.EventSnapshot{EventID: "wev_6", Kind: "integrate", AttemptID: "att-g1"},
	}
	got := goalrun.BuildUnavailableRetryEvidenceWithProof([]goalrun.RouteExclude{
		{ChildID: "wi_x", Provider: "antigravity", Reason: "model_unavailable", Claimed: true, Message: "typed failure"},
	}, "att-g1", proof)
	if got == nil {
		t.Fatal("want evidence from concrete proof")
	}
	if got.ExcludedProvider != "antigravity" || !got.NoDuplicateClaim || !got.NoDuplicateFiles || !got.NoDoubleCapacity {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got.EvidenceRef, "event_ids=wev_1") {
		t.Fatalf("evidence ref: %q", got.EvidenceRef)
	}
	// Overlapping product files → nil (no invent true).
	proof.RetryProductFiles = []string{"notes/bad.md"}
	if got := goalrun.BuildUnavailableRetryEvidenceWithProof([]goalrun.RouteExclude{
		{ChildID: "wi_x", Provider: "antigravity", Reason: "model_unavailable", Claimed: true},
	}, "att-g1", proof); got != nil {
		t.Fatalf("overlapping files must not green: %+v", got)
	}
}
