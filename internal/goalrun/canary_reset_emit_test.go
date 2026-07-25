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
	if len(obs) != 1 || obs[0].ResetAt == nil || !obs[0].ResetAt.Equal(reset.UTC()) {
		t.Fatalf("provider obs ResetAt=%v", obs)
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
