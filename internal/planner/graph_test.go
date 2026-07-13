package planner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
	"github.com/jasonhnd/loopcoder/internal/taskrequirements"
)

func TestBuildGraphProposalDeterministicAndClassifierBacked(t *testing.T) {
	input := graphInput()
	input.Tasks = []TaskInput{
		{TaskKey: "verify", Title: "Verify change", Scope: taskrequirements.Scope{Paths: []string{"internal/foo/foo_test.go"}, Tests: true, AllowsRepoWrite: true}},
		{TaskKey: "implement", Title: "Implement change", Scope: taskrequirements.Scope{Paths: []string{"internal/foo/foo.go"}, AllowsRepoWrite: true}},
		{TaskKey: "docs", Title: "Document change", Scope: taskrequirements.Scope{Paths: []string{"docs/reference/usage.md"}, Documentation: true, AllowsRepoWrite: true}},
	}
	input.Edges = []EdgeInput{
		{FromTaskKey: "implement", ToTaskKey: "docs", EdgeKind: "orders-after"},
		{FromTaskKey: "implement", ToTaskKey: "verify", EdgeKind: "requires"},
	}
	first, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}
	reordered := input
	reordered.Tasks = []TaskInput{input.Tasks[2], input.Tasks[0], input.Tasks[1]}
	reordered.Edges = []EdgeInput{input.Edges[1], input.Edges[0]}
	second, err := BuildGraphProposal(reordered)
	if err != nil {
		t.Fatalf("BuildGraphProposal() reordered error = %v", err)
	}
	if second.PlanFingerprint != first.PlanFingerprint || second.AuthorizationFingerprint != first.AuthorizationFingerprint {
		t.Fatalf("fingerprints changed for equivalent input: %s/%s then %s/%s", first.PlanFingerprint, first.AuthorizationFingerprint, second.PlanFingerprint, second.AuthorizationFingerprint)
	}
	if first.Validation.ValidationStatus != "passed" || first.Validation.TaskCount != 3 || first.Validation.EdgeCount != 2 {
		t.Fatalf("validation = %#v, want passed 3 task 2 edge graph", first.Validation)
	}
	if first.Validation.MaxObservedDepth != 2 || len(first.Validation.ParallelReadyWidths) != 2 || first.Validation.ParallelReadyWidths[0] != 1 || first.Validation.ParallelReadyWidths[1] != 2 {
		t.Fatalf("validation layers = %#v, depth=%d; want [1 2] depth 2", first.Validation.ParallelReadyWidths, first.Validation.MaxObservedDepth)
	}
	if first.Tasks[0].Requirement.TaskRequirementID == "" || first.Tasks[0].TaskRequirementPayloadHash == "" {
		t.Fatalf("task proposal missing requirement identity: %#v", first.Tasks[0])
	}
	if first.Tasks[0].Task.PlanFingerprint != first.PlanFingerprint || first.Edges[0].PlanFingerprint != first.PlanFingerprint {
		t.Fatalf("task/edge plan fingerprint not rebound to proposal fingerprint")
	}
	if first.ApprovalRequirement != "required" {
		t.Fatalf("approval requirement = %q, want required for repo-write proposal", first.ApprovalRequirement)
	}
}

func TestBuildGraphProposalRejectsBoundsBeforeProposal(t *testing.T) {
	input := graphInput()
	input.GraphBounds.MaxTasks = 1
	input.Tasks = []TaskInput{
		{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{}},
		{TaskKey: "b", Title: "B", Scope: taskrequirements.Scope{}},
	}
	_, err := BuildGraphProposal(input)
	if !errors.Is(err, taskrequirements.ErrGraphBoundExceeded) {
		t.Fatalf("BuildGraphProposal() error = %v, want ErrGraphBoundExceeded", err)
	}
}

func TestBuildGraphProposalRejectsCycles(t *testing.T) {
	input := graphInput()
	input.Tasks = []TaskInput{
		{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{}},
		{TaskKey: "b", Title: "B", Scope: taskrequirements.Scope{}},
	}
	input.Edges = []EdgeInput{
		{FromTaskKey: "a", ToTaskKey: "b", EdgeKind: "requires"},
		{FromTaskKey: "b", ToTaskKey: "a", EdgeKind: "orders-after"},
	}
	_, err := BuildGraphProposal(input)
	if !errors.Is(err, taskrequirements.ErrGraphCycleDetected) {
		t.Fatalf("BuildGraphProposal() error = %v, want ErrGraphCycleDetected", err)
	}
}

func TestBuildGraphProposalRejectsDisconnectedFromExplicitRoot(t *testing.T) {
	input := graphInput()
	input.RootTaskKeys = []string{"a"}
	input.Tasks = []TaskInput{
		{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{}},
		{TaskKey: "b", Title: "B", Scope: taskrequirements.Scope{}},
	}
	_, err := BuildGraphProposal(input)
	if !errors.Is(err, taskrequirements.ErrGraphDisconnected) {
		t.Fatalf("BuildGraphProposal() error = %v, want ErrGraphDisconnected", err)
	}
}

func TestBuildGraphProposalRejectsLaunchClassAboveProposalLimit(t *testing.T) {
	input := graphInput()
	input.MaxSideEffectClass = string(taskrequirements.SideEffectRepoWrite)
	input.Tasks = []TaskInput{
		{TaskKey: "launch", Title: "Launch provider", Scope: taskrequirements.Scope{AllowsProviderLaunch: true}},
	}
	_, err := BuildGraphProposal(input)
	if !errors.Is(err, taskrequirements.ErrGraphBoundExceeded) {
		t.Fatalf("BuildGraphProposal() error = %v, want ErrGraphBoundExceeded", err)
	}
}

func TestBuildGraphProposalIsSideEffectFree(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return fixedGraphTime() }})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer store.Close()
	before := countDeliveryRows(t, ctx, store)
	input := graphInput()
	input.Tasks = []TaskInput{{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{Paths: []string{"README.md"}, Documentation: true}}}
	if _, err := BuildGraphProposal(input); err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}
	after := countDeliveryRows(t, ctx, store)
	if after != before {
		t.Fatalf("BuildGraphProposal mutated storage: before=%d after=%d", before, after)
	}
}

func graphInput() ProposalInput {
	now := fixedGraphTime()
	return ProposalInput{
		ProjectID:          "proj_graph",
		DeliveryRunID:      "run_graph",
		IntentSummary:      "ship graph planner",
		PolicyVersion:      taskrequirements.PolicyVersion,
		MaxSideEffectClass: string(taskrequirements.SideEffectExternalWrite),
		CreatedBy:          graphActor(),
		Host:               graphHost(),
		Now:                now,
	}
}

func fixedGraphTime() time.Time {
	return time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
}

func graphActor() delivery.Actor {
	return delivery.Actor{
		ActorKind:         "planner",
		ActorID:           "planner-test",
		Display:           "Planner Test",
		DecisionAuthority: "planner",
		Source:            "test",
	}
}

func graphHost() delivery.Host {
	return delivery.Host{
		HostKind:         "test",
		HostID:           "host-test",
		SessionID:        "session-test",
		ProcessID:        1,
		LoopcoderVersion: "test",
		Platform:         "test",
	}
}

func countDeliveryRows(t *testing.T, ctx context.Context, store storage.Store) int {
	t.Helper()
	tables := []string{
		"delivery_runs",
		"delivery_plan_fingerprints",
		"delivery_tasks",
		"delivery_dependency_edges",
		"delivery_attempts",
		"delivery_decisions",
		"delivery_approvals",
		"delivery_overrides",
		"delivery_idempotency",
		"task_requirements",
		"task_requirement_overrides",
	}
	total := 0
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		for _, table := range tables {
			var count int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
				return err
			}
			total += count
		}
		return nil
	}); err != nil {
		t.Fatalf("count delivery rows: %v", err)
	}
	return total
}
