package goalrun

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/artifactqual"
	"github.com/jasonhnd/loopcoder/internal/capacityledger"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

func TestCanaryChildrenFromReports_PropagatesCapacityResetAt(t *testing.T) {
	reset := time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)
	r := reset
	res := Result{
		Children: []ChildReport{{
			ChildID: "wi_implement", Provider: "codex", Model: "gpt-5.5", Depth: "medium",
			Terminal: "succeeded", AttemptID: "att-i", TaskClass: "tera",
			AccountRef: "acct-x", InstallRef: "pinst-x", WindowKind: "fixed_week",
			CapacityBefore: floatPtr(0.9), CapacityReserved: floatPtr(0.05),
			CapacityAfter: floatPtr(0.85), CapacityActual: floatPtr(0.05),
			CapacityBeforeSource: "codexbar", CapacityBeforeFreshness: "fresh",
			CapacityBeforeConfidence: "exact", CapacityBeforeCapturedAt: reset.Add(-time.Hour),
			CapacityBeforeInventoryDigest: "sha256:inventory-before",
			CapacityAfterSource:           "codexbar", CapacityAfterFreshness: "fresh",
			CapacityAfterConfidence: "exact", CapacityAfterState: "observed",
			CapacityAfterObservedAt:      reset.Add(-30 * time.Minute),
			CapacityAfterInventoryDigest: "sha256:inventory-after",
			CapacityResetAt:              &r,
			ActualSource:                 "estimated_group_delta_token_weighted:obs",
			CapacityActualConfidence:     "estimated",
			ArgvDigest:                   "sha256:deadbeefcafebabe0123456789abcdef0123456789abcdef0123456789abcdef",
			ActualSources: workflowrun.ActualRouteSources{
				Model: "accepted_invocation", Effort: "accepted_invocation", Permission: "accepted_invocation",
				Account: "auth_binding", Install: "install_binding",
			},
		}},
	}
	kids := canaryChildrenFromReports(res)
	if len(kids) != 1 {
		t.Fatalf("kids=%d", len(kids))
	}
	if kids[0].ResetAt == nil || !kids[0].ResetAt.Equal(reset.UTC()) {
		t.Fatalf("ResetAt=%v want %v", kids[0].ResetAt, reset.UTC())
	}
	obs := canaryProviderObsFromReports(res)
	if len(obs) != 2 {
		t.Fatalf("provider obs=%v want exact before and after", obs)
	}
	for _, observation := range obs {
		if observation.ResetAt == nil || !observation.ResetAt.Equal(reset.UTC()) {
			t.Fatalf("provider obs ResetAt=%v", obs)
		}
	}
}

func TestCanaryProviderObsFromReports_PreservesDistinctResumeSnapshots(t *testing.T) {
	before1 := 0.93
	before2 := 0.91
	after2 := 0.90
	t1 := time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	t3 := t2.Add(time.Minute)
	res := Result{Children: []ChildReport{
		{
			ChildID: "wi_research", Provider: "codex", AccountRef: "acct",
			InstallRef: "install", WindowKind: "weekly",
			CapacityBefore: &before1, CapacityBeforeSource: "official",
			CapacityBeforeFreshness: "fresh", CapacityBeforeConfidence: "exact",
			CapacityBeforeInventoryDigest: "digest-before-1", CapacityBeforeCapturedAt: t1,
		},
		{
			ChildID: "wi_tests", Provider: "codex", AccountRef: "acct",
			InstallRef: "install", WindowKind: "weekly",
			CapacityBefore: &before2, CapacityBeforeSource: "official",
			CapacityBeforeFreshness: "fresh", CapacityBeforeConfidence: "exact",
			CapacityBeforeInventoryDigest: "digest-before-2", CapacityBeforeCapturedAt: t2,
			CapacityAfter: &after2, CapacityAfterState: "observed",
			CapacityAfterSource: "official", CapacityAfterFreshness: "fresh",
			CapacityAfterConfidence: "exact", CapacityAfterInventoryDigest: "digest-after-2",
			CapacityAfterObservedAt: t3,
		},
	}}
	obs := canaryProviderObsFromReports(res)
	if len(obs) != 3 {
		t.Fatalf("observations=%d want distinct before1,before2,after2: %+v", len(obs), obs)
	}
	got := map[string]bool{}
	for _, observation := range obs {
		got[observation.InventoryReportDigest] = true
	}
	for _, digest := range []string{"digest-before-1", "digest-before-2", "digest-after-2"} {
		if !got[digest] {
			t.Fatalf("missing exact snapshot %s: %+v", digest, obs)
		}
	}
}

func TestApplyCapacityFromEntry_CopiesResetAtUTC(t *testing.T) {
	reset := time.Date(2026, 7, 25, 20, 0, 0, 0, time.FixedZone("X", 3600))
	r := reset
	entry := capacityledger.Entry{
		Before: 0.9, Reserved: 0.05, BeforeSource: "codexbar", Freshness: "fresh",
		Confidence: "exact", ResetAt: &r,
		After: floatPtr(0.85), AfterSource: "codexbar", AfterFreshness: "fresh",
		AfterConfidence: "exact", AfterState: capacityledger.AfterStateObserved,
	}
	var cr ChildReport
	applyCapacityBeforeFromEntry(&cr, entry)
	if cr.CapacityResetAt == nil {
		t.Fatal("before path missing ResetAt")
	}
	if cr.CapacityResetAt.Location() != time.UTC {
		t.Fatalf("want UTC, got %v", cr.CapacityResetAt.Location())
	}
	if !cr.CapacityResetAt.Equal(reset.UTC()) {
		t.Fatalf("reset=%v want %v", cr.CapacityResetAt, reset.UTC())
	}
	// Clear and re-apply via after path.
	cr.CapacityResetAt = nil
	applyCapacityAfterFromEntry(&cr, entry)
	if cr.CapacityResetAt == nil || !cr.CapacityResetAt.Equal(reset.UTC()) {
		t.Fatalf("after path ResetAt=%v", cr.CapacityResetAt)
	}
}

func floatPtr(f float64) *float64 { return &f }

// silence unused import if artifactqual only used for types in other tests
var _ = artifactqual.SchemaCanaryEvidence
