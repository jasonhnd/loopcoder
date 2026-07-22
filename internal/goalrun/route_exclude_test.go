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
}
