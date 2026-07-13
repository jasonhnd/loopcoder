package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestDeliveryRunLifecycleTable(t *testing.T) {
	tests := []struct {
		from  string
		event string
		want  string
		err   error
	}{
		{RunDraft, "start_planning", RunPlanning, nil},
		{RunPlanning, "plan_ready_requires_approval", RunAwaitingApproval, nil},
		{RunAwaitingApproval, "approve", RunApproved, nil},
		{RunApproved, "queue", RunQueued, nil},
		{RunQueued, "start_execution", RunRunning, nil},
		{RunRunning, "finish_success", RunSucceeded, nil},
		{RunSucceeded, "queue", "", ErrTerminalState},
		{RunAwaitingApproval, "queue", "", ErrApprovalRequired},
		{RunQueued, "plan_ready_no_approval", "", ErrDuplicateRecord},
		{RunRunning, "approve_stale", "", ErrStaleApproval},
	}
	for _, tt := range tests {
		got, err := DeliveryRunTransition(tt.from, tt.event)
		if !errors.Is(err, tt.err) {
			t.Fatalf("DeliveryRunTransition(%q, %q) error = %v, want %v", tt.from, tt.event, err, tt.err)
		}
		if got != tt.want {
			t.Fatalf("DeliveryRunTransition(%q, %q) = %q, want %q", tt.from, tt.event, got, tt.want)
		}
	}
}

func TestTaskLifecycleTable(t *testing.T) {
	tests := []struct {
		from  string
		event string
		want  string
		err   error
	}{
		{TaskPending, "dependencies_ready", TaskReady, nil},
		{TaskReady, "claim", TaskClaimed, nil},
		{TaskClaimed, "start", TaskRunning, nil},
		{TaskRunning, "complete_success", TaskSucceeded, nil},
		{TaskSucceeded, "claim", "", ErrTerminalState},
		{TaskAwaitingApproval, "claim", "", ErrApprovalRequired},
		{TaskRunning, "claim", "", ErrClaimConflict},
		{TaskReady, "approval_stale", "", ErrStaleApproval},
	}
	for _, tt := range tests {
		got, err := TaskTransition(tt.from, tt.event)
		if !errors.Is(err, tt.err) {
			t.Fatalf("TaskTransition(%q, %q) error = %v, want %v", tt.from, tt.event, err, tt.err)
		}
		if got != tt.want {
			t.Fatalf("TaskTransition(%q, %q) = %q, want %q", tt.from, tt.event, got, tt.want)
		}
	}
}

func TestCanonicalFingerprintGoldenAndPathNormalization(t *testing.T) {
	got, canonical, err := DigestCanonicalJSON(map[string]any{
		"b": "e\u0301",
		"a": jsonNumber(t, "2"),
	})
	if err != nil {
		t.Fatalf("DigestCanonicalJSON: %v", err)
	}
	if string(canonical) != `{"a":2,"b":"é"}` {
		t.Fatalf("canonical = %s, want sorted NFC JSON", canonical)
	}
	if got != "sha256:ff3e313366d233bb6858e954b99bef40c4bcd340bd99586794a12c248d9052e3" {
		t.Fatalf("digest = %s", got)
	}
	path, err := NormalizeRepoPath(`docs\specs\0801-delivery-run-contracts.md`)
	if err != nil {
		t.Fatalf("NormalizeRepoPath: %v", err)
	}
	if path != "docs/specs/0801-delivery-run-contracts.md" {
		t.Fatalf("normalized path = %q", path)
	}
	if _, err := NormalizeRepoPath("../secret"); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("escape path error = %v, want ErrInvalidRecord", err)
	}
}

func TestPersistenceAtomicTypedFailuresAndReplay(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	seedProject(t, ctx, store, "proj_b")

	run, taskA, taskB := seedTwoTaskDependencyRun(t, ctx, store, "proj_a", "run_a")
	edgeAB := DependencyEdge{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID, FromTaskID: taskA.TaskID, ToTaskID: taskB.TaskID, PlanFingerprint: run.PlanFingerprint, CreatedBy: actor(), Host: host()}
	if _, err := AddDependencyEdge(ctx, store, edgeAB, PersistOptions{IdempotencyKey: "edge-ab", Now: fixedTime()}); err != nil {
		t.Fatalf("AddDependencyEdge: %v", err)
	}
	if _, err := AddDependencyEdge(ctx, store, edgeAB, PersistOptions{IdempotencyKey: "edge-ab-duplicate", Now: fixedTime()}); !errors.Is(err, ErrDuplicateRecord) {
		t.Fatalf("duplicate edge error = %v, want ErrDuplicateRecord", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_dependency_edges`, 1)

	cross := DependencyEdge{ProjectID: "proj_b", DeliveryRunID: run.DeliveryRunID, FromTaskID: taskA.TaskID, ToTaskID: taskB.TaskID, PlanFingerprint: run.PlanFingerprint, CreatedBy: actor(), Host: host()}
	if _, err := AddDependencyEdge(ctx, store, cross, PersistOptions{IdempotencyKey: "edge-cross", Now: fixedTime()}); !errors.Is(err, ErrCrossProjectReference) {
		t.Fatalf("cross-project error = %v, want ErrCrossProjectReference", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_dependency_edges`, 1)

	stale := approvalFixture(run)
	stale.AuthorizationFingerprint = "sha256:" + stringsRepeat("0", 64)
	if _, err := RecordApproval(ctx, store, stale, PersistOptions{IdempotencyKey: "approval-stale", Now: fixedTime()}); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("stale approval error = %v, want ErrStaleApproval", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_approvals`, 0)

	created, err := RecordApproval(ctx, store, approvalFixture(run), PersistOptions{IdempotencyKey: "approval-ok", Now: fixedTime()})
	if err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	replayed, err := RecordApproval(ctx, store, approvalFixture(run), PersistOptions{IdempotencyKey: "approval-ok", Now: fixedTime()})
	if err != nil {
		t.Fatalf("RecordApproval replay: %v", err)
	}
	if replayed.CreatedAt != created.CreatedAt {
		t.Fatalf("replay changed timestamp: %q != %q", replayed.CreatedAt, created.CreatedAt)
	}
	changed := approvalFixture(run)
	changed.ApprovalKind = "continue"
	if _, err := RecordApproval(ctx, store, changed, PersistOptions{IdempotencyKey: "approval-ok", Now: fixedTime()}); !errors.Is(err, ErrDuplicateReplay) {
		t.Fatalf("duplicate replay error = %v, want ErrDuplicateReplay", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_approvals`, 1)

	decision, err := RecordDecision(ctx, store, decisionFixture(run), PersistOptions{IdempotencyKey: "decision-policy", Now: fixedTime()})
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	staleOverride := overrideFixture(run, decision.DecisionID)
	staleOverride.InputFingerprint = "sha256:" + stringsRepeat("9", 64)
	if _, err := RecordOverride(ctx, store, staleOverride, PersistOptions{IdempotencyKey: "override-stale-input", Now: fixedTime()}); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("stale override input error = %v, want ErrStaleApproval", err)
	}
	staleOverride = overrideFixture(run, decision.DecisionID)
	staleOverride.PlanFingerprint = "sha256:" + stringsRepeat("8", 64)
	if _, err := RecordOverride(ctx, store, staleOverride, PersistOptions{IdempotencyKey: "override-stale-plan", Now: fixedTime()}); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("stale override plan error = %v, want ErrStaleApproval", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_overrides`, 0)
}

func TestDependencyEdgeCycleQueryFailureIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run, taskA, taskB := seedTwoTaskDependencyRun(t, ctx, store, "proj_a", "run_cycle_query_failure")

	edgeAB := DependencyEdge{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID, FromTaskID: taskA.TaskID, ToTaskID: taskB.TaskID, PlanFingerprint: run.PlanFingerprint, CreatedBy: actor(), Host: host()}
	if _, err := AddDependencyEdge(ctx, store, edgeAB, PersistOptions{IdempotencyKey: "edge-ab", Now: fixedTime()}); err != nil {
		t.Fatalf("AddDependencyEdge: %v", err)
	}

	injected := errors.New("cte scan failed")
	badEdge := DependencyEdge{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID, FromTaskID: taskB.TaskID, ToTaskID: taskA.TaskID, EdgeKind: "orders-after", PlanFingerprint: run.PlanFingerprint, CreatedBy: actor(), Host: host()}
	opts := PersistOptions{
		IdempotencyKey: "edge-cycle-query-failure",
		Now:            fixedTime(),
		cycleDetector: func(context.Context, storage.Tx, string, string, string) (bool, error) {
			return false, injected
		},
	}
	if _, err := AddDependencyEdge(ctx, store, badEdge, opts); err == nil || errors.Is(err, ErrCycleDetected) || !errors.Is(err, injected) {
		t.Fatalf("cycle query failure error = %v, want wrapped infrastructure error", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_dependency_edges`, 1)
}

func TestDependencyEdgeCycleDetectedIsAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run, taskA, taskB := seedTwoTaskDependencyRun(t, ctx, store, "proj_a", "run_cycle_detected")

	edgeAB := DependencyEdge{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID, FromTaskID: taskA.TaskID, ToTaskID: taskB.TaskID, PlanFingerprint: run.PlanFingerprint, CreatedBy: actor(), Host: host()}
	if _, err := AddDependencyEdge(ctx, store, edgeAB, PersistOptions{IdempotencyKey: "edge-ab", Now: fixedTime()}); err != nil {
		t.Fatalf("AddDependencyEdge: %v", err)
	}
	edgeBA := DependencyEdge{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID, FromTaskID: taskB.TaskID, ToTaskID: taskA.TaskID, PlanFingerprint: run.PlanFingerprint, CreatedBy: actor(), Host: host()}
	if _, err := AddDependencyEdge(ctx, store, edgeBA, PersistOptions{IdempotencyKey: "edge-ba", Now: fixedTime()}); !errors.Is(err, ErrCycleDetected) {
		t.Fatalf("cycle error = %v, want ErrCycleDetected", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_dependency_edges`, 1)
}

func TestDependencyEdgeConcurrentDuplicateRaceIsTyped(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, tt := range []struct {
		name      string
		winnerKey string
		loserKey  string
		wantErr   error
	}{
		{
			name:      "same idempotency key replays",
			winnerKey: "edge-race-same-key",
			loserKey:  "edge-race-same-key",
		},
		{
			name:      "different idempotency keys conflict",
			winnerKey: "edge-race-winner-key",
			loserKey:  "edge-race-loser-key",
			wantErr:   ErrDuplicateRecord,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "loopcoder.db")
			seedStore, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedTime})
			if err != nil {
				t.Fatalf("Open seed store: %v", err)
			}
			run, taskA, taskB := seedTwoTaskDependencyRun(t, ctx, seedStore, "proj_a", "run_edge_race")
			if err := seedStore.Close(); err != nil {
				t.Fatalf("close seed store: %v", err)
			}

			gate := newDependencyEdgeInsertRaceGate()
			winnerStore := openDependencyEdgeRaceStore(t, dbPath, dependencyEdgeRaceWinner, gate)
			defer winnerStore.Close()
			loserStore := openDependencyEdgeRaceStore(t, dbPath, dependencyEdgeRaceLoser, gate)
			defer loserStore.Close()

			edge := DependencyEdge{
				ProjectID:       run.ProjectID,
				DeliveryRunID:   run.DeliveryRunID,
				FromTaskID:      taskA.TaskID,
				ToTaskID:        taskB.TaskID,
				PlanFingerprint: run.PlanFingerprint,
				CreatedBy:       actor(),
				Host:            host(),
			}
			winnerDone := make(chan dependencyEdgeRaceResult, 1)
			loserDone := make(chan dependencyEdgeRaceResult, 1)
			go func() {
				edge, err := AddDependencyEdge(ctx, winnerStore, edge, PersistOptions{IdempotencyKey: tt.winnerKey, Now: fixedTime()})
				winnerDone <- dependencyEdgeRaceResult{edge: edge, err: err}
			}()
			go func() {
				edge, err := AddDependencyEdge(ctx, loserStore, edge, PersistOptions{IdempotencyKey: tt.loserKey, Now: fixedTime()})
				loserDone <- dependencyEdgeRaceResult{edge: edge, err: err}
			}()

			winner := <-winnerDone
			if winner.err != nil {
				t.Fatalf("winner AddDependencyEdge: %v", winner.err)
			}
			gate.releaseLoser()
			loser := <-loserDone
			if !errors.Is(loser.err, tt.wantErr) {
				t.Fatalf("loser AddDependencyEdge error = %v, want %v", loser.err, tt.wantErr)
			}
			if loser.err != nil {
				errText := loser.err.Error()
				if strings.Contains(errText, "UNIQUE") || strings.Contains(strings.ToLower(errText), "constraint") || strings.Contains(strings.ToLower(errText), "sqlite") {
					t.Fatalf("typed duplicate leaked sqlite text: %v", loser.err)
				}
			} else if loser.edge.EdgeID != winner.edge.EdgeID || loser.edge.CreatedAt != winner.edge.CreatedAt {
				t.Fatalf("idempotent loser edge = %#v, want replay of %#v", loser.edge, winner.edge)
			}

			verifyStore, err := storage.Open(ctx, storage.Options{Path: dbPath, Now: fixedTime})
			if err != nil {
				t.Fatalf("Open verify store: %v", err)
			}
			defer verifyStore.Close()
			assertCount(t, ctx, verifyStore, `SELECT COUNT(*) FROM delivery_dependency_edges`, 1)

			cycleEdge := DependencyEdge{
				ProjectID:       run.ProjectID,
				DeliveryRunID:   run.DeliveryRunID,
				FromTaskID:      taskB.TaskID,
				ToTaskID:        taskA.TaskID,
				PlanFingerprint: run.PlanFingerprint,
				CreatedBy:       actor(),
				Host:            host(),
			}
			if _, err := AddDependencyEdge(ctx, verifyStore, cycleEdge, PersistOptions{IdempotencyKey: tt.name + "-cycle", Now: fixedTime()}); !errors.Is(err, ErrCycleDetected) {
				t.Fatalf("cycle after race error = %v, want ErrCycleDetected", err)
			}
			assertCount(t, ctx, verifyStore, `SELECT COUNT(*) FROM delivery_dependency_edges`, 1)
		})
	}
}

func TestDependencyEdgeInsertInfrastructureFailureIsPreserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run, taskA, taskB := seedTwoTaskDependencyRun(t, ctx, store, "proj_a", "run_edge_insert_failure")

	injected := errors.New("disk write failed")
	edge := DependencyEdge{
		ProjectID:       run.ProjectID,
		DeliveryRunID:   run.DeliveryRunID,
		FromTaskID:      taskA.TaskID,
		ToTaskID:        taskB.TaskID,
		PlanFingerprint: run.PlanFingerprint,
		CreatedBy:       actor(),
		Host:            host(),
	}
	_, err := AddDependencyEdge(ctx, dependencyEdgeInsertFailureStore{Store: store, err: injected}, edge, PersistOptions{IdempotencyKey: "edge-insert-infra-failure", Now: fixedTime()})
	if err == nil || !errors.Is(err, injected) || errors.Is(err, ErrDuplicateRecord) {
		t.Fatalf("insert infrastructure failure = %v, want original non-duplicate error", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_dependency_edges`, 0)
}

func TestClaimTaskIsIdempotentAndFenced(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	seedProject(t, ctx, store, "proj_a")
	run := deliveryRunFixture("proj_a", "run_a")
	run.State = RunQueued
	if _, err := PersistDeliveryRun(ctx, store, run, PersistOptions{Now: fixedTime()}); err != nil {
		t.Fatalf("PersistDeliveryRun: %v", err)
	}
	if _, err := PersistPlanFingerprint(ctx, store, fingerprintFixture(run), PersistOptions{Now: fixedTime()}); err != nil {
		t.Fatalf("PersistPlanFingerprint: %v", err)
	}
	task := taskFixture(run, "a")
	task.State = TaskReady
	var err error
	task, err = PersistTask(ctx, store, task, PersistOptions{Now: fixedTime()})
	if err != nil {
		t.Fatalf("PersistTask: %v", err)
	}
	if _, err := RecordApproval(ctx, store, approvalFixture(run), PersistOptions{Now: fixedTime()}); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	first, err := ClaimTask(ctx, store, run.ProjectID, run.DeliveryRunID, task.TaskID, "executor-a", actor(), host(), PersistOptions{IdempotencyKey: "claim-a", Now: fixedTime()})
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	second, err := ClaimTask(ctx, store, run.ProjectID, run.DeliveryRunID, task.TaskID, "executor-a", actor(), host(), PersistOptions{IdempotencyKey: "claim-a", Now: fixedTime().Add(time.Minute)})
	if err != nil {
		t.Fatalf("ClaimTask replay: %v", err)
	}
	if second.AttemptID != first.AttemptID || second.CreatedAt != first.CreatedAt {
		t.Fatalf("replayed claim = %#v, want original %#v", second, first)
	}
	if _, err := ClaimTask(ctx, store, run.ProjectID, run.DeliveryRunID, task.TaskID, "executor-b", actor(), host(), PersistOptions{IdempotencyKey: "claim-b", Now: fixedTime()}); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("second claim error = %v, want ErrClaimConflict for claimed task", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_attempts`, 1)
}

func TestTransitionsReplayByIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	seedProject(t, ctx, store, "proj_a")
	run := deliveryRunFixture("proj_a", "run_a")
	run.State = RunDraft
	if _, err := PersistDeliveryRun(ctx, store, run, PersistOptions{IdempotencyKey: "run-transition-create", Now: fixedTime()}); err != nil {
		t.Fatalf("PersistDeliveryRun: %v", err)
	}
	first, err := TransitionDeliveryRun(ctx, store, run.ProjectID, run.DeliveryRunID, "start_planning", actor(), host(), PersistOptions{IdempotencyKey: "run-transition", Now: fixedTime()})
	if err != nil {
		t.Fatalf("TransitionDeliveryRun: %v", err)
	}
	second, err := TransitionDeliveryRun(ctx, store, run.ProjectID, run.DeliveryRunID, "start_planning", actor(), host(), PersistOptions{IdempotencyKey: "run-transition", Now: fixedTime().Add(time.Hour)})
	if err != nil {
		t.Fatalf("TransitionDeliveryRun replay: %v", err)
	}
	if first != RunPlanning || second != first {
		t.Fatalf("run transition replay = %q/%q, want planning/planning", first, second)
	}

	task := taskFixture(run, "a")
	task.State = TaskPending
	if _, err := PersistTask(ctx, store, task, PersistOptions{IdempotencyKey: "task-transition-create", Now: fixedTime()}); err != nil {
		t.Fatalf("PersistTask: %v", err)
	}
	taskID := stableID("task", run.ProjectID, run.DeliveryRunID, "a")
	taskFirst, err := TransitionTask(ctx, store, run.ProjectID, run.DeliveryRunID, taskID, "dependencies_ready", actor(), host(), PersistOptions{IdempotencyKey: "task-transition", Now: fixedTime()})
	if err != nil {
		t.Fatalf("TransitionTask: %v", err)
	}
	taskSecond, err := TransitionTask(ctx, store, run.ProjectID, run.DeliveryRunID, taskID, "dependencies_ready", actor(), host(), PersistOptions{IdempotencyKey: "task-transition", Now: fixedTime().Add(time.Hour)})
	if err != nil {
		t.Fatalf("TransitionTask replay: %v", err)
	}
	if taskFirst != TaskReady || taskSecond != taskFirst {
		t.Fatalf("task transition replay = %q/%q, want ready/ready", taskFirst, taskSecond)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_idempotency WHERE operation IN ('transition_delivery_run', 'transition_task')`, 2)
}

func TestPlanIsSideEffectFreeAndDoesNotMutateStorage(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	for _, enforcement := range []HostEnforcement{
		{},
		SupportedHostEnforcement("codex-cli", "fixture"),
		SupportedHostEnforcement("claude-code", "fixture"),
		SupportedHostEnforcement("paseo-style", "fixture"),
		SupportedHostEnforcement("generic-local", "fixture"),
	} {
		before := deliveryTableRows(t, ctx, store)
		proposal, err := Plan(ctx, store, PlanProposalInput{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID, HostEnforcement: enforcement})
		if err != nil {
			t.Fatalf("Plan with host %#v: %v", enforcement, err)
		}
		after := deliveryTableRows(t, ctx, store)
		if after != before {
			t.Fatalf("Plan mutated delivery tables for host %#v: before=%d after=%d", enforcement, before, after)
		}
		if proposal.AuthorizationFingerprint == "" || proposal.FingerprintStatus != "current" || proposal.TaskCount != 1 || proposal.Invocation.Contract.SideEffectClass != "none" {
			t.Fatalf("proposal = %#v, want current side-effect-free fingerprinted one-task plan", proposal)
		}
	}
}

func TestUnsupportedHostContractFailsClosedWithoutMutation(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	enforcement := SupportedHostEnforcement("bad-host", "fixture")
	enforcement.StableJSON = false
	before := deliveryTableRows(t, ctx, store)
	_, err := Plan(ctx, store, PlanProposalInput{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID, HostEnforcement: enforcement})
	if !errors.Is(err, ErrUnsupportedHostCapability) {
		t.Fatalf("Plan unsupported host error = %v, want ErrUnsupportedHostCapability", err)
	}
	after := deliveryTableRows(t, ctx, store)
	if after != before {
		t.Fatalf("unsupported Plan mutated delivery tables: before=%d after=%d", before, after)
	}
}

func TestHostInvocationFingerprintMismatchCannotAuthorizeExecution(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	codex := SupportedHostEnforcement("codex-cli", "fixture")
	proposal, err := Plan(ctx, store, PlanProposalInput{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID, HostEnforcement: codex})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	claude := SupportedHostEnforcement("claude-code", "fixture")
	_, err = Decide(ctx, store, DecisionOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		Action:                           DecisionActionApprove,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "approve-different-host",
		Now:                              fixedTime().Add(time.Minute),
		HostEnforcement:                  claude,
	})
	if !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("Decide with different host contract error = %v, want ErrStaleApproval", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_approvals`, 0)
}

func TestInterruptedContinueLeavesDurableResumableState(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := Continue(cancelled, store, ContinueOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		ExpectedAuthorizationFingerprint: "sha256:" + stringsRepeat("4", 64),
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "interrupted-continue",
		Now:                              fixedTime().Add(time.Minute),
		HostEnforcement:                  SupportedHostEnforcement("codex-cli", "fixture"),
	})
	if !errors.Is(err, ErrInvocationInterrupted) {
		t.Fatalf("Continue interrupted error = %v, want ErrInvocationInterrupted", err)
	}
	var state, output string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM delivery_runs WHERE project_id = ? AND delivery_run_id = ?`, run.ProjectID, run.DeliveryRunID).Scan(&state); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT output_json FROM delivery_decisions WHERE decision_kind = 'terminal-outcome'`).Scan(&output)
	}); err != nil {
		t.Fatalf("query interrupted run: %v", err)
	}
	if state != RunPaused || !strings.Contains(output, string(ErrInvocationInterruptedCode)) {
		t.Fatalf("interrupted state/output = %q/%q, want paused terminal outcome decision", state, output)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_decisions WHERE decision_kind = 'terminal-outcome'`, 1)
}

func TestDecisionApprovalIsBoundToCurrentPlanFingerprint(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	proposal, err := Plan(ctx, store, PlanProposalInput{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	changedTask := taskFixture(run, "a")
	changedTask.State = TaskAwaitingApproval
	changedTask.ScopeJSON = `{"paths":["docs/changed.md"]}`
	changedTask.PlanFingerprint = "sha256:" + stringsRepeat("3", 64)
	if _, err := PersistTask(ctx, store, changedTask, PersistOptions{IdempotencyKey: "material-task-change", Now: fixedTime().Add(time.Minute)}); err != nil {
		t.Fatalf("PersistTask material change: %v", err)
	}
	_, err = Decide(ctx, store, DecisionOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		Action:                           DecisionActionApprove,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "approve-stale",
		Now:                              fixedTime().Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("Decide stale approval error = %v, want ErrStaleApproval", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_approvals`, 0)
}

func TestDecideApproveAndContinueAreIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	proposal, err := Plan(ctx, store, PlanProposalInput{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	approved, err := Decide(ctx, store, DecisionOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		Action:                           DecisionActionApprove,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "approve-current",
		Now:                              fixedTime().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	if approved.RunState != RunApproved || approved.ApprovalStatus != "approved" {
		t.Fatalf("approve result = %#v, want approved", approved)
	}
	first, err := Continue(ctx, store, ContinueOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "continue-current",
		Now:                              fixedTime().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	second, err := Continue(ctx, store, ContinueOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "continue-current",
		Now:                              fixedTime().Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Continue replay: %v", err)
	}
	if first.RunState != RunQueued || second.RunState != first.RunState || second.AuthorizationFingerprint != first.AuthorizationFingerprint {
		t.Fatalf("continue replay = %#v then %#v, want stable queued result", first, second)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_approvals`, 1)
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_plan_fingerprints WHERE fingerprint_id = '`+proposal.AuthorizationFingerprint+`' AND canonical_inputs_json LIKE '%authorized_invocation%'`, 1)
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_attempts`, 0)
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_tasks WHERE state = 'ready'`, 1)
}

func TestRejectedDecisionBlocksContinueWithTypedError(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	proposal, err := Plan(ctx, store, PlanProposalInput{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := Decide(ctx, store, DecisionOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		Action:                           DecisionActionReject,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "reject-current",
		Now:                              fixedTime().Add(time.Minute),
	}); err != nil {
		t.Fatalf("Decide reject: %v", err)
	}
	_, err = Continue(ctx, store, ContinueOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "continue-rejected",
		Now:                              fixedTime().Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Continue rejected run error = %v, want ErrPolicyDenied", err)
	}
}

func TestDerivedIdempotencyKeyFailsOnCanonicalJSONError(t *testing.T) {
	if _, err := idempotencyKey("", "bad", map[string]any{"not_canonical": math.NaN()}); err == nil {
		t.Fatal("idempotencyKey returned nil error for non-canonical request")
	}
}

func TestV9MigrationRecordsBackupAndAmbiguousLegacyState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	createV9Fixture(t, ctx, raw)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	store, err := storage.Open(ctx, storage.Options{Path: path, Now: fixedTime})
	if err != nil {
		t.Fatalf("Open migrated store: %v", err)
	}
	defer store.Close()
	health, err := store.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.SchemaVersion != storage.CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", health.SchemaVersion, storage.CurrentSchemaVersion)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_migration_backups WHERE source_schema_version = 9 AND source_db_hash <> '' AND backup_path <> ''`, 1)
	var state, code, status string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state, terminal_error_code FROM delivery_runs WHERE delivery_run_id = 'run_legacy'`).Scan(&state, &code); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT migration_status FROM delivery_legacy_imports WHERE delivery_run_id = 'run_legacy'`).Scan(&status)
	}); err != nil {
		t.Fatalf("query migrated delivery run: %v", err)
	}
	if state != RunNeedsHuman || code != string(ErrAmbiguousLegacyStateCode) || status != "needs-human" {
		t.Fatalf("migrated state/code/status = %q/%q/%q, want needs-human/ErrAmbiguousLegacyState/needs-human", state, code, status)
	}
	if runtime.GOOS != "windows" {
		assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_runs WHERE project_id = 'proj_v9'`, 1)
	}
}

func TestV9MigrationIDsAreDeterministicForSameSourceState(t *testing.T) {
	ctx := context.Background()
	firstBackup, firstImport := migrateV9FixtureAndReadIDs(t, ctx)
	secondBackup, secondImport := migrateV9FixtureAndReadIDs(t, ctx)
	if firstBackup != secondBackup {
		t.Fatalf("backup id = %q then %q, want deterministic", firstBackup, secondBackup)
	}
	if firstImport != secondImport {
		t.Fatalf("legacy import id = %q then %q, want deterministic", firstImport, secondImport)
	}
}

func migrateV9FixtureAndReadIDs(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	createV9Fixture(t, ctx, raw)
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: fixedTime})
	if err != nil {
		t.Fatalf("Open migrated store: %v", err)
	}
	defer store.Close()
	var backupID, importID string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT backup_id FROM delivery_migration_backups`).Scan(&backupID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT legacy_import_id FROM delivery_legacy_imports WHERE delivery_run_id = 'run_legacy'`).Scan(&importID)
	}); err != nil {
		t.Fatalf("query migration ids: %v", err)
	}
	return backupID, importID
}

func openDeliveryStore(t *testing.T) storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedTime})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func seedTwoTaskDependencyRun(t *testing.T, ctx context.Context, store storage.Store, projectID, runID string) (DeliveryRun, Task, Task) {
	t.Helper()
	seedProject(t, ctx, store, projectID)
	run := deliveryRunFixture(projectID, runID)
	if _, err := PersistDeliveryRun(ctx, store, run, PersistOptions{IdempotencyKey: runID + "-create", Now: fixedTime()}); err != nil {
		t.Fatalf("PersistDeliveryRun: %v", err)
	}
	if _, err := PersistPlanFingerprint(ctx, store, fingerprintFixture(run), PersistOptions{IdempotencyKey: runID + "-fingerprint", Now: fixedTime()}); err != nil {
		t.Fatalf("PersistPlanFingerprint: %v", err)
	}
	taskA, err := PersistTask(ctx, store, taskFixture(run, "a"), PersistOptions{IdempotencyKey: runID + "-task-a", Now: fixedTime()})
	if err != nil {
		t.Fatalf("PersistTask a: %v", err)
	}
	taskB, err := PersistTask(ctx, store, taskFixture(run, "b"), PersistOptions{IdempotencyKey: runID + "-task-b", Now: fixedTime()})
	if err != nil {
		t.Fatalf("PersistTask b: %v", err)
	}
	return run, taskA, taskB
}

func seedProject(t *testing.T, ctx context.Context, store storage.Store, projectID string) {
	t.Helper()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?)`, projectID, "/repo/"+projectID, fixedText(), fixedText())
		return err
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func seedApprovalRun(t *testing.T, ctx context.Context, store storage.Store) DeliveryRun {
	t.Helper()
	seedProject(t, ctx, store, "proj_a")
	run := deliveryRunFixture("proj_a", "run_approval")
	run.State = RunAwaitingApproval
	run.InputFingerprint = ""
	run.PolicyFingerprint = ""
	run.PlanFingerprint = ""
	run.AuthorizationFingerprint = ""
	run.ApprovalStatus = "required"
	if _, err := PersistDeliveryRun(ctx, store, run, PersistOptions{IdempotencyKey: "approval-run", Now: fixedTime()}); err != nil {
		t.Fatalf("PersistDeliveryRun: %v", err)
	}
	task := taskFixture(run, "a")
	task.State = TaskAwaitingApproval
	task.AuthorizationFingerprint = ""
	task.PlanFingerprint = "sha256:" + stringsRepeat("3", 64)
	if _, err := PersistTask(ctx, store, task, PersistOptions{IdempotencyKey: "approval-task", Now: fixedTime()}); err != nil {
		t.Fatalf("PersistTask: %v", err)
	}
	return run
}

func deliveryTableRows(t *testing.T, ctx context.Context, store storage.Store) int {
	t.Helper()
	var total int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		for _, table := range []string{
			"delivery_runs",
			"delivery_plan_fingerprints",
			"delivery_tasks",
			"delivery_dependency_edges",
			"delivery_attempts",
			"delivery_decisions",
			"delivery_approvals",
			"delivery_overrides",
			"delivery_idempotency",
		} {
			var count int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
				return err
			}
			total += count
		}
		return nil
	}); err != nil {
		t.Fatalf("count delivery table rows: %v", err)
	}
	return total
}

func deliveryRunFixture(projectID, runID string) DeliveryRun {
	return DeliveryRun{
		DeliveryRunID:            runID,
		ProjectID:                projectID,
		State:                    RunApproved,
		IntentSummary:            "test delivery",
		InputFingerprint:         "sha256:" + stringsRepeat("1", 64),
		PolicyFingerprint:        "sha256:" + stringsRepeat("2", 64),
		PlanFingerprint:          "sha256:" + stringsRepeat("3", 64),
		AuthorizationFingerprint: "sha256:" + stringsRepeat("4", 64),
		MaxSideEffectClass:       "provider-launch",
		PolicyVersion:            "test-policy-v1",
		ApprovalStatus:           "approved",
		OverrideStatus:           "none",
		CreatedBy:                actor(),
		UpdatedBy:                actor(),
		Host:                     host(),
	}
}

func fingerprintFixture(run DeliveryRun) PlanFingerprint {
	return PlanFingerprint{
		ProjectID:                run.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		InputFingerprint:         run.InputFingerprint,
		PolicyFingerprint:        run.PolicyFingerprint,
		PlanFingerprint:          run.PlanFingerprint,
		AuthorizationFingerprint: run.AuthorizationFingerprint,
		CreatedBy:                actor(),
		Host:                     host(),
	}
}

func taskFixture(run DeliveryRun, key string) Task {
	return Task{
		ProjectID:                run.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		TaskKey:                  key,
		Title:                    "task " + key,
		RequirementsJSON:         `{"do":"work"}`,
		ScopeJSON:                `{"paths":["README.md"]}`,
		Permission:               "write",
		SideEffectClass:          "repo-write",
		PolicyVersion:            run.PolicyVersion,
		PlanFingerprint:          run.PlanFingerprint,
		AuthorizationFingerprint: run.AuthorizationFingerprint,
		CreatedBy:                actor(),
		UpdatedBy:                actor(),
		Host:                     host(),
	}
}

func approvalFixture(run DeliveryRun) Approval {
	return Approval{
		ProjectID:                run.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		ApprovalKind:             "plan",
		AuthorizationFingerprint: run.AuthorizationFingerprint,
		InputFingerprint:         run.InputFingerprint,
		PolicyFingerprint:        run.PolicyFingerprint,
		PlanFingerprint:          run.PlanFingerprint,
		ApprovedSideEffectClass:  run.MaxSideEffectClass,
		ApprovedScopeJSON:        `{"paths":["README.md"]}`,
		ApprovedBy:               actor(),
		Status:                   "active",
		CreatedBy:                actor(),
		UpdatedBy:                actor(),
		Host:                     host(),
	}
}

func decisionFixture(run DeliveryRun) Decision {
	return Decision{
		ProjectID:         run.ProjectID,
		DeliveryRunID:     run.DeliveryRunID,
		DecisionKey:       "policy.override-required",
		DecisionKind:      "policy",
		DecidedBy:         actor(),
		InputsFingerprint: run.AuthorizationFingerprint,
		OutputJSON:        `{"requires_override":true}`,
		PolicyVersion:     run.PolicyVersion,
		SideEffectClass:   "none",
		CreatedBy:         actor(),
		Host:              host(),
	}
}

func overrideFixture(run DeliveryRun, decisionID string) Override {
	return Override{
		ProjectID:                run.ProjectID,
		DeliveryRunID:            run.DeliveryRunID,
		OverrideKind:             "recovery",
		AuthorizationFingerprint: run.AuthorizationFingerprint,
		InputFingerprint:         run.InputFingerprint,
		PolicyFingerprint:        run.PolicyFingerprint,
		PlanFingerprint:          run.PlanFingerprint,
		PolicyDecisionID:         decisionID,
		OverriddenBy:             actor(),
		Justification:            "test recovery override",
		LimitsJSON:               `{"max_attempts":1}`,
		Status:                   "active",
		ExpiresAt:                CanonicalTimestamp(fixedTime().Add(time.Hour)),
		CreatedBy:                actor(),
		UpdatedBy:                actor(),
		Host:                     host(),
	}
}

func actor() Actor {
	return Actor{ActorKind: "user", ActorID: "local-user", Display: "Local user", DecisionAuthority: "user", Source: "test"}
}

func host() Host {
	return Host{HostKind: "codex", HostID: "host-test", SessionID: "session-test", ProcessID: 1, LoopcoderVersion: "0.8.0", Platform: runtime.GOOS + "-" + runtime.GOARCH}
}

func fixedTime() time.Time {
	return time.Date(2026, 7, 11, 1, 2, 3, 0, time.UTC)
}

func fixedText() string {
	return CanonicalTimestamp(fixedTime())
}

func jsonNumber(t *testing.T, value string) any {
	t.Helper()
	return json.Number(value)
}

func assertCount(t *testing.T, ctx context.Context, store storage.Store, query string, want int) {
	t.Helper()
	var got int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, query).Scan(&got)
	}); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("count query %q = %d, want %d", query, got, want)
	}
}

type dependencyEdgeRaceRole string

const (
	dependencyEdgeRaceWinner dependencyEdgeRaceRole = "winner"
	dependencyEdgeRaceLoser  dependencyEdgeRaceRole = "loser"
)

type dependencyEdgeRaceResult struct {
	edge DependencyEdge
	err  error
}

type dependencyEdgeInsertRaceGate struct {
	loserReady       chan struct{}
	loserRelease     chan struct{}
	loserReadyOnce   sync.Once
	loserReleaseOnce sync.Once
}

func newDependencyEdgeInsertRaceGate() *dependencyEdgeInsertRaceGate {
	return &dependencyEdgeInsertRaceGate{
		loserReady:   make(chan struct{}),
		loserRelease: make(chan struct{}),
	}
}

func (g *dependencyEdgeInsertRaceGate) beforeInsert(ctx context.Context, role dependencyEdgeRaceRole) error {
	switch role {
	case dependencyEdgeRaceWinner:
		select {
		case <-g.loserReady:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	case dependencyEdgeRaceLoser:
		g.loserReadyOnce.Do(func() { close(g.loserReady) })
		select {
		case <-g.loserRelease:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	default:
		return nil
	}
}

func (g *dependencyEdgeInsertRaceGate) releaseLoser() {
	g.loserReleaseOnce.Do(func() { close(g.loserRelease) })
}

type dependencyEdgeRaceStore struct {
	path string
	db   *sql.DB
	role dependencyEdgeRaceRole
	gate *dependencyEdgeInsertRaceGate
}

func openDependencyEdgeRaceStore(t *testing.T, path string, role dependencyEdgeRaceRole, gate *dependencyEdgeInsertRaceGate) *dependencyEdgeRaceStore {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open race sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
	} {
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			_ = db.Close()
			t.Fatalf("configure race sqlite: %v", err)
		}
	}
	return &dependencyEdgeRaceStore{path: path, db: db, role: role, gate: gate}
}

func (s *dependencyEdgeRaceStore) Close() error { return s.db.Close() }

func (s *dependencyEdgeRaceStore) Path() string { return s.path }

func (s *dependencyEdgeRaceStore) Now() time.Time { return fixedTime() }

func (s *dependencyEdgeRaceStore) Health(context.Context) (storage.Health, error) {
	return storage.Health{Path: s.path, Exists: true, SchemaVersion: storage.CurrentSchemaVersion, OK: true}, nil
}

func (s *dependencyEdgeRaceStore) WithTx(ctx context.Context, fn func(storage.Tx) error) error {
	return s.withConn(ctx, fn)
}

func (s *dependencyEdgeRaceStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	return s.withConn(ctx, fn)
}

func (s *dependencyEdgeRaceStore) withConn(ctx context.Context, fn func(storage.Tx) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(dependencyEdgeRaceTx{conn: conn, role: s.role, gate: s.gate})
}

type dependencyEdgeRaceTx struct {
	conn *sql.Conn
	role dependencyEdgeRaceRole
	gate *dependencyEdgeInsertRaceGate
}

func (tx dependencyEdgeRaceTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if isDependencyEdgeInsert(query) {
		if err := tx.gate.beforeInsert(ctx, tx.role); err != nil {
			return nil, err
		}
	}
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx dependencyEdgeRaceTx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.conn.QueryContext(ctx, query, args...)
}

func (tx dependencyEdgeRaceTx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

type dependencyEdgeInsertFailureStore struct {
	storage.Store
	err error
}

func (s dependencyEdgeInsertFailureStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	return s.Store.WithWriteTx(ctx, func(tx storage.Tx) error {
		return fn(dependencyEdgeInsertFailureTx{Tx: tx, err: s.err})
	})
}

type dependencyEdgeInsertFailureTx struct {
	storage.Tx
	err error
}

func (tx dependencyEdgeInsertFailureTx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if isDependencyEdgeInsert(query) {
		return nil, tx.err
	}
	return tx.Tx.Exec(ctx, query, args...)
}

func isDependencyEdgeInsert(query string) bool {
	return strings.Contains(query, "INSERT INTO delivery_dependency_edges")
}

func createV9Fixture(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`CREATE TABLE migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`,
		`CREATE TABLE projects (id TEXT PRIMARY KEY, remote_url TEXT NOT NULL DEFAULT '', github_owner TEXT NOT NULL DEFAULT '', github_name TEXT NOT NULL DEFAULT '', local_path TEXT NOT NULL, default_branch TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL, local_path_canonical TEXT NOT NULL DEFAULT '', git_root TEXT NOT NULL DEFAULT '', remote_url_normalized TEXT NOT NULL DEFAULT '', identity_source TEXT NOT NULL DEFAULT '', detached_at TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE runs (id TEXT PRIMARY KEY, project_id TEXT REFERENCES projects(id) ON DELETE SET NULL, parent_run_id TEXT REFERENCES runs(id) ON DELETE SET NULL, issue_number INTEGER, status TEXT NOT NULL DEFAULT '', started_at TEXT, ended_at TEXT, updated_at TEXT NOT NULL, root_run_id TEXT NOT NULL DEFAULT '', depth INTEGER NOT NULL DEFAULT 0, origin TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE run_events (id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, sequence INTEGER NOT NULL, ts TEXT NOT NULL, event_type TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}', UNIQUE(run_id, sequence))`,
		`CREATE TABLE run_edges (parent_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, child_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, edge_type TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, root_run_id TEXT NOT NULL DEFAULT '', plan_id TEXT NOT NULL DEFAULT '', child_key TEXT NOT NULL DEFAULT '', depth INTEGER NOT NULL DEFAULT 0, ordinal INTEGER NOT NULL DEFAULT -1, scope_json TEXT NOT NULL DEFAULT '{}', permission TEXT NOT NULL DEFAULT '', aggregation_json TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '', PRIMARY KEY(parent_run_id, child_run_id))`,
		`CREATE TABLE reports (id TEXT PRIMARY KEY, run_id TEXT REFERENCES runs(id) ON DELETE SET NULL, role TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', started_at TEXT, ended_at TEXT, payload_json TEXT NOT NULL, created_at TEXT NOT NULL, project_id TEXT NOT NULL DEFAULT '', source_path TEXT NOT NULL DEFAULT '', source_hash TEXT NOT NULL DEFAULT '', source_kind TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE child_plans (plan_id TEXT PRIMARY KEY, parent_run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE, root_run_id TEXT NOT NULL, schema_version TEXT NOT NULL, max_depth INTEGER NOT NULL, max_concurrency INTEGER NOT NULL, plan_json TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE run_claims (run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE, executor_id TEXT NOT NULL, claim_generation INTEGER NOT NULL, claimed_at TEXT NOT NULL, lease_expires_at TEXT NOT NULL, heartbeat_at TEXT NOT NULL, phase TEXT NOT NULL DEFAULT 'claimed', provider_idempotency_key TEXT NOT NULL DEFAULT '', provider_receipt TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE legacy_import_records (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id), run_id TEXT, record_type TEXT NOT NULL, source_path TEXT NOT NULL, source_line INTEGER NOT NULL DEFAULT 0, source_hash TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}', imported_at TEXT NOT NULL)`,
		`CREATE TABLE legacy_import_status (project_id TEXT PRIMARY KEY REFERENCES projects(id), repo_path TEXT NOT NULL, started_at TEXT NOT NULL, completed_at TEXT NOT NULL, status TEXT NOT NULL, scanned_count INTEGER NOT NULL DEFAULT 0, imported_count INTEGER NOT NULL DEFAULT 0, skipped_count INTEGER NOT NULL DEFAULT 0, malformed_count INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO migrations(version, name, applied_at) VALUES (1, 'initial runtime schema', '2026-01-01T00:00:00Z'), (2, 'project identity fields', '2026-01-01T00:00:00Z'), (3, 'legacy local state import metadata', '2026-01-01T00:00:00Z'), (4, 'scrub project remote urls', '2026-01-01T00:00:00Z'), (5, 'preserve project history on registry removal', '2026-01-01T00:00:00Z'), (6, 'reconcile physical project identities', '2026-01-01T00:00:00Z'), (7, 'nested child plans and durable run graph', '2026-01-01T00:00:00Z'), (8, 'nested child execution claims', '2026-01-01T00:00:00Z'), (9, 'nested child claim lifecycle phase', '2026-01-01T00:00:00Z')`,
		`INSERT INTO projects(id, local_path, created_at, updated_at) VALUES ('proj_v9', '/repo', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`INSERT INTO runs(id, project_id, status, started_at, updated_at, root_run_id, depth, origin, created_at) VALUES ('run_legacy', 'proj_v9', 'running', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'run_legacy', 0, 'legacy', '2026-01-01T00:00:00Z')`,
		`INSERT INTO run_claims(run_id, executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at, phase) VALUES ('run_legacy', 'executor-a', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:30:00Z', '2026-01-01T00:00:00Z', 'executing')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec fixture statement: %v\n%s", err, statement)
		}
	}
}

func stringsRepeat(s string, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		out += s
	}
	return out
}

func TestSupersedeMarksApprovalRowsStaleWithSpecStatus(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	proposal, err := Plan(ctx, store, PlanProposalInput{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := Decide(ctx, store, DecisionOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		Action:                           DecisionActionApprove,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "approve-before-supersede",
		Now:                              fixedTime().Add(time.Minute),
	}); err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	result, err := Decide(ctx, store, DecisionOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		Action:                           DecisionActionSupersede,
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "supersede-approved-plan",
		Now:                              fixedTime().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Decide supersede: %v", err)
	}
	if result.ApprovalStatus != "stale" {
		t.Fatalf("supersede run approval status = %q, want stale", result.ApprovalStatus)
	}
	var status string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM delivery_approvals WHERE project_id = ? AND delivery_run_id = ?`, run.ProjectID, run.DeliveryRunID).Scan(&status)
	}); err != nil {
		t.Fatalf("query approval status: %v", err)
	}
	// Spec 0801 Approval.status enum: active, rejected, expired, revoked, stale.
	if status != "stale" {
		t.Fatalf("approval row status = %q, want spec-legal %q", status, "stale")
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_approvals WHERE status = 'active'`, 0)
}

func TestDecideAndContinueRejectMissingExpectedFingerprint(t *testing.T) {
	ctx := context.Background()
	store := openDeliveryStore(t)
	defer store.Close()
	run := seedApprovalRun(t, ctx, store)
	if _, err := Plan(ctx, store, PlanProposalInput{ProjectID: run.ProjectID, DeliveryRunID: run.DeliveryRunID}); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	_, err := Decide(ctx, store, DecisionOptions{
		ProjectID:      run.ProjectID,
		DeliveryRunID:  run.DeliveryRunID,
		Action:         DecisionActionApprove,
		Actor:          actor(),
		Host:           host(),
		IdempotencyKey: "approve-no-fingerprint",
		Now:            fixedTime().Add(time.Minute),
	})
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Decide without expected fingerprint error = %v, want ErrInvalidRecord", err)
	}
	// Continue may omit the optimistic-concurrency guard: authorization is
	// enforced by the active-approval lookup keyed on the CURRENT proposal
	// fingerprint, so with no approval it must refuse with ErrApprovalRequired.
	_, err = Continue(ctx, store, ContinueOptions{
		ProjectID:      run.ProjectID,
		DeliveryRunID:  run.DeliveryRunID,
		Actor:          actor(),
		Host:           host(),
		IdempotencyKey: "continue-no-fingerprint",
		Now:            fixedTime().Add(2 * time.Minute),
	})
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("Continue without approval error = %v, want ErrApprovalRequired", err)
	}
	assertCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_approvals`, 0)
}
