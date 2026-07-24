package goalrun_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/goalrun"
	"github.com/jasonhnd/loopcoder/internal/workflowrun"
)

// Checkpoint save/reload must persist FINAL Workflow.CapacityTransitions
// (ledger-backed prior+alternate), not a stale mid-workflow reserved snapshot.
func TestCheckpoint_FinalCapacityTransitionsSurvive(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 7, 23, 21, 0, 0, 0, time.UTC)
	act := 0.03
	final := []workflowrun.CapacityTransition{
		{
			AttemptID: "att-only-abc-g0", Role: "prior", State: "released",
			Provider: "antigravity", AccountRef: "acct-ag", WindowKind: "five_hour",
			ReservationID: "res-prior", Actual: nil, Source: "",
		},
		{
			AttemptID: "att-only-abc-g1", Role: "alternate", State: "reconciled",
			Provider: "codex", AccountRef: "acct-codex", WindowKind: "five_hour",
			ReservationID: "res-alt", Actual: &act, Source: "provider_usage",
		},
	}
	// Simulate saveRunCheckpoint path: write Checkpoint with final transitions.
	cp := goalrun.Checkpoint{
		Schema:              goalrun.CheckpointSchema,
		ProjectID:           "proj-cp",
		RunID:               "run-cp-1",
		GraphID:             "g1",
		PlanDigest:          "digest",
		Status:              "human_gate",
		SavedAt:             now,
		CapacityTransitions: append([]workflowrun.CapacityTransition(nil), final...),
		WorkflowKids: []workflowrun.ChildOutcome{
			{WorkItemID: "only", AttemptID: "att-only-abc-g0", FailureClass: "model_unavailable", Terminal: "failed"},
			{WorkItemID: "only", AttemptID: "att-only-abc-g1", SupersedesAttemptID: "att-only-abc-g0",
				Terminal: "succeeded", OutputEvidence: "sha256:ok"},
		},
		EventLogPath: filepath.Join(home, "events.jsonl"),
	}
	path, err := goalrun.SaveCheckpoint(home, cp)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("empty path")
	}
	loaded, _, err := goalrun.LoadCheckpoint(home, "proj-cp", "run-cp-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.CapacityTransitions) != 2 {
		t.Fatalf("want 2 final transitions, got %+v", loaded.CapacityTransitions)
	}
	if loaded.CapacityTransitions[0].Role != "prior" ||
		loaded.CapacityTransitions[0].AttemptID != "att-only-abc-g0" ||
		loaded.CapacityTransitions[0].State != "released" {
		t.Fatalf("prior: %+v", loaded.CapacityTransitions[0])
	}
	if loaded.CapacityTransitions[0].Actual != nil || loaded.CapacityTransitions[0].Source != "" {
		t.Fatalf("prior honest unknown: %+v", loaded.CapacityTransitions[0])
	}
	if loaded.CapacityTransitions[1].Role != "alternate" ||
		loaded.CapacityTransitions[1].AttemptID != "att-only-abc-g1" ||
		loaded.CapacityTransitions[1].State != "reconciled" {
		t.Fatalf("alternate: %+v", loaded.CapacityTransitions[1])
	}
	if loaded.CapacityTransitions[1].Actual == nil || *loaded.CapacityTransitions[1].Actual != act ||
		loaded.CapacityTransitions[1].Source != "provider_usage" {
		t.Fatalf("alternate actual/source: %+v", loaded.CapacityTransitions[1])
	}
	// Stale mid-workflow "reserved" must not be what we stored as final.
	for _, tr := range loaded.CapacityTransitions {
		if tr.State == "reserved" {
			t.Fatalf("final checkpoint must not carry mid reserved: %+v", tr)
		}
	}
}
