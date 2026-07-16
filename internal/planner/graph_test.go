package planner

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
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

func TestAcceptGraphProposalPersistsImmutableVersionAndReplays(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store := openGraphStore(t, ctx, path)
	seedGraphProject(t, ctx, store, "proj_graph")
	input := graphInput()
	input.Tasks = []TaskInput{
		{TaskKey: "implement", Title: "Implement change", Scope: taskrequirements.Scope{Paths: []string{"internal/foo/foo.go"}, AllowsRepoWrite: true}},
		{TaskKey: "verify", Title: "Verify change", Scope: taskrequirements.Scope{Paths: []string{"internal/foo/foo_test.go"}, Tests: true, AllowsRepoWrite: true}},
	}
	input.Edges = []EdgeInput{{FromTaskKey: "implement", ToTaskKey: "verify", EdgeKind: "requires"}}
	proposal, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}
	accepted, err := AcceptGraphProposal(ctx, store, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
	})
	if err != nil {
		t.Fatalf("AcceptGraphProposal() error = %v", err)
	}
	replayed, err := AcceptGraphProposal(ctx, store, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("AcceptGraphProposal() replay error = %v", err)
	}
	if replayed.GraphVersionID != accepted.GraphVersionID || replayed.CanonicalProposalHash != accepted.CanonicalProposalHash {
		t.Fatalf("replayed accepted version = %#v, want %#v", replayed, accepted)
	}
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM accepted_task_graph_versions`, 1)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM task_graph_validations`, 1)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_tasks`, 2)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_dependency_edges`, 1)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM task_requirements`, 2)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened := openGraphStore(t, ctx, path)
	defer reopened.Close()
	loaded, err := LoadAcceptedGraphVersion(ctx, reopened, proposal.ProjectID, proposal.DeliveryRunID)
	if err != nil {
		t.Fatalf("LoadAcceptedGraphVersion() error = %v", err)
	}
	if loaded.CanonicalProposalHash != accepted.CanonicalProposalHash || loaded.Proposal.PlanFingerprint != proposal.PlanFingerprint || len(loaded.Proposal.Tasks) != 2 {
		t.Fatalf("loaded accepted graph = %#v, want exact accepted proposal", loaded)
	}
}

func TestAcceptGraphProposalRejectsAdversarialMutationAtomically(t *testing.T) {
	ctx := context.Background()
	store := openGraphStore(t, ctx, filepath.Join(t.TempDir(), "loopcoder.db"))
	defer store.Close()
	seedGraphProject(t, ctx, store, "proj_graph")
	input := graphInput()
	input.Tasks = []TaskInput{
		{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{}},
		{TaskKey: "b", Title: "B", Scope: taskrequirements.Scope{}},
	}
	input.Edges = []EdgeInput{{FromTaskKey: "a", ToTaskKey: "b", EdgeKind: "requires"}}
	proposal, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}
	proposal.Edges[0].ToTaskID = proposal.Edges[0].FromTaskID
	_, err = AcceptGraphProposal(ctx, store, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
	})
	if !errors.Is(err, taskrequirements.ErrGraphCycleDetected) {
		t.Fatalf("AcceptGraphProposal() error = %v, want ErrGraphCycleDetected", err)
	}
	assertGraphAcceptanceEmpty(t, ctx, store)
}

func TestAcceptGraphProposalRejectsChangedMaterialInputAndStaleExpectedFingerprint(t *testing.T) {
	ctx := context.Background()
	store := openGraphStore(t, ctx, filepath.Join(t.TempDir(), "loopcoder.db"))
	defer store.Close()
	seedGraphProject(t, ctx, store, "proj_graph")
	input := graphInput()
	input.Tasks = []TaskInput{{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{AllowsRepoWrite: true}}}
	proposal, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}
	_, err = AcceptGraphProposal(ctx, store, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
	})
	if !errors.Is(err, delivery.ErrStaleApproval) {
		t.Fatalf("stale expected fingerprint error = %v, want ErrStaleApproval", err)
	}
	assertGraphAcceptanceEmpty(t, ctx, store)

	changed := proposal
	changed.Tasks = append([]TaskProposal(nil), proposal.Tasks...)
	changed.Tasks[0].Requirement.ProvenanceSummary = "tampered after approval"
	_, err = AcceptGraphProposal(ctx, store, changed, AcceptOptions{
		ExpectedAuthorizationFingerprint: changed.AuthorizationFingerprint,
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
	})
	if !errors.Is(err, delivery.ErrStaleApproval) {
		t.Fatalf("changed requirement error = %v, want ErrStaleApproval", err)
	}
	assertGraphAcceptanceEmpty(t, ctx, store)
}

func TestAcceptGraphProposalConcurrentSameGraphIsIdempotentAndChangedGraphLoses(t *testing.T) {
	ctx := context.Background()
	store := openGraphStore(t, ctx, filepath.Join(t.TempDir(), "loopcoder.db"))
	defer store.Close()
	seedGraphProject(t, ctx, store, "proj_graph")
	input := graphInput()
	input.Tasks = []TaskInput{{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{AllowsRepoWrite: true}}}
	proposal, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan AcceptedGraphVersion, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := AcceptGraphProposal(ctx, store, proposal, AcceptOptions{
				ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
				Actor:                            graphActor(),
				Host:                             graphHost(),
				Now:                              input.Now,
			})
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent AcceptGraphProposal() error = %v", err)
	}
	var first string
	for result := range results {
		if first == "" {
			first = result.GraphVersionID
		}
		if result.GraphVersionID != first {
			t.Fatalf("concurrent graph version = %q, want %q", result.GraphVersionID, first)
		}
	}
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM accepted_task_graph_versions`, 1)

	changedInput := input
	changedInput.Tasks = []TaskInput{{TaskKey: "a", Title: "Changed A", Scope: taskrequirements.Scope{AllowsRepoWrite: true}}}
	changed, err := BuildGraphProposal(changedInput)
	if err != nil {
		t.Fatalf("BuildGraphProposal() changed error = %v", err)
	}
	_, err = AcceptGraphProposal(ctx, store, changed, AcceptOptions{
		ExpectedAuthorizationFingerprint: changed.AuthorizationFingerprint,
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now.Add(time.Minute),
	})
	if !errors.Is(err, delivery.ErrStaleApproval) {
		t.Fatalf("changed graph after accepted error = %v, want ErrStaleApproval", err)
	}
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM accepted_task_graph_versions`, 1)
}

func TestAcceptGraphProposalTransactionFailureRollsBackResidualState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store := openGraphStore(t, ctx, path)
	seedGraphProject(t, ctx, store, "proj_graph")
	input := graphInput()
	input.Tasks = []TaskInput{{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{AllowsRepoWrite: true}}}
	proposal, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}
	injected := errors.New("injected transaction failure")
	_, err = AcceptGraphProposal(ctx, store, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
		afterRunPersistedForTest: func() error {
			return injected
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("AcceptGraphProposal() error = %v, want injected failure", err)
	}
	assertGraphAcceptanceEmpty(t, ctx, store)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened := openGraphStore(t, ctx, path)
	defer reopened.Close()
	assertGraphAcceptanceEmpty(t, ctx, reopened)
}

func TestAcceptGraphProposalCrashBeforeCommitRollsBackAfterReopenAndRetries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store := openGraphStore(t, ctx, path)
	seedGraphProject(t, ctx, store, "proj_graph")
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	input := graphInput()
	input.Tasks = []TaskInput{{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{AllowsRepoWrite: true}}}
	proposal, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}

	cause := &injectedGraphCommitCause{phase: "before-commit"}
	failing := openGraphStoreWithOptions(t, ctx, storage.Options{
		Path: path,
		Now:  func() time.Time { return fixedGraphTime() },
		WriteTxCommitHookForTest: func(context.Context, storage.Tx, func(context.Context) error) error {
			return cause
		},
	})
	_, err = AcceptGraphProposal(ctx, failing, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph-crash-before-commit",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
	})
	if !errors.Is(err, cause) {
		t.Fatalf("AcceptGraphProposal() error = %v, want original commit cause", err)
	}
	if err := failing.Close(); err != nil {
		t.Fatalf("close failed store: %v", err)
	}

	reopened := openGraphStore(t, ctx, path)
	assertGraphAcceptanceEmpty(t, ctx, reopened)
	accepted, err := AcceptGraphProposal(ctx, reopened, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph-crash-before-commit",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
	})
	if err != nil {
		t.Fatalf("AcceptGraphProposal() retry after reopen error = %v", err)
	}
	replayed, err := AcceptGraphProposal(ctx, reopened, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph-crash-before-commit",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("AcceptGraphProposal() replay after retry error = %v", err)
	}
	if replayed.GraphVersionID != accepted.GraphVersionID || replayed.CanonicalProposalHash != accepted.CanonicalProposalHash {
		t.Fatalf("replayed accepted version = %#v, want %#v", replayed, accepted)
	}
	assertAcceptedGraphAuthorityCounts(t, ctx, reopened, 1, 1, 1, 1, 1, 1)
	if err := reopened.Close(); err != nil {
		t.Fatalf("close reopened store: %v", err)
	}
}

func TestAcceptGraphProposalAmbiguousCommitReopensAndReplaysIdempotently(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store := openGraphStore(t, ctx, path)
	seedGraphProject(t, ctx, store, "proj_graph")
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	input := graphInput()
	input.Tasks = []TaskInput{{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{AllowsRepoWrite: true}}}
	proposal, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}

	cause := &injectedGraphCommitCause{phase: "after-commit"}
	failing := openGraphStoreWithOptions(t, ctx, storage.Options{
		Path: path,
		Now:  func() time.Time { return fixedGraphTime() },
		WriteTxCommitHookForTest: func(commitCtx context.Context, _ storage.Tx, commit func(context.Context) error) error {
			if err := commit(commitCtx); err != nil {
				return err
			}
			return cause
		},
	})
	_, err = AcceptGraphProposal(ctx, failing, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph-ambiguous-commit",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
	})
	if !errors.Is(err, cause) {
		t.Fatalf("AcceptGraphProposal() error = %v, want original ambiguous commit cause", err)
	}
	if err := failing.Close(); err != nil {
		t.Fatalf("close failed store: %v", err)
	}

	reopened := openGraphStore(t, ctx, path)
	defer reopened.Close()
	loaded, err := LoadAcceptedGraphVersion(ctx, reopened, proposal.ProjectID, proposal.DeliveryRunID)
	if err != nil {
		t.Fatalf("LoadAcceptedGraphVersion() after ambiguous commit error = %v", err)
	}
	replayed, err := AcceptGraphProposal(ctx, reopened, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph-ambiguous-commit",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("AcceptGraphProposal() replay after ambiguous commit error = %v", err)
	}
	if replayed.GraphVersionID != loaded.GraphVersionID || replayed.CanonicalProposalHash != loaded.CanonicalProposalHash {
		t.Fatalf("replayed accepted version = %#v, want loaded %#v", replayed, loaded)
	}
	assertAcceptedGraphAuthorityCounts(t, ctx, reopened, 1, 1, 1, 1, 1, 1)
}

func TestAcceptGraphProposalCrashBeforeCommitUnderSQLiteContention(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store := openGraphStore(t, ctx, path)
	seedGraphProject(t, ctx, store, "proj_graph")
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}

	input := graphInput()
	input.Tasks = []TaskInput{{TaskKey: "a", Title: "A", Scope: taskrequirements.Scope{AllowsRepoWrite: true}}}
	proposal, err := BuildGraphProposal(input)
	if err != nil {
		t.Fatalf("BuildGraphProposal() error = %v", err)
	}

	locker := openGraphStore(t, ctx, path)
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- locker.WithWriteTx(ctx, func(storage.Tx) error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	cause := &injectedGraphCommitCause{phase: "contention-before-commit"}
	contender := openGraphStoreWithOptions(t, ctx, storage.Options{
		Path: path,
		Now:  func() time.Time { return fixedGraphTime() },
		WriteTxCommitHookForTest: func(context.Context, storage.Tx, func(context.Context) error) error {
			return cause
		},
	})
	acceptDone := make(chan error, 1)
	go func() {
		_, err := AcceptGraphProposal(ctx, contender, proposal, AcceptOptions{
			ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
			IdempotencyKey:                   "accept-graph-contention-crash",
			Actor:                            graphActor(),
			Host:                             graphHost(),
			Now:                              input.Now,
		})
		acceptDone <- err
	}()
	select {
	case err := <-acceptDone:
		t.Fatalf("AcceptGraphProposal() completed while competing write transaction was still open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatalf("release write lock: %v", err)
	}
	if err := locker.Close(); err != nil {
		t.Fatalf("close locker: %v", err)
	}
	err = <-acceptDone
	if !errors.Is(err, cause) {
		t.Fatalf("AcceptGraphProposal() under contention error = %v, want original cause", err)
	}
	if err := contender.Close(); err != nil {
		t.Fatalf("close contender: %v", err)
	}

	reopened := openGraphStore(t, ctx, path)
	defer reopened.Close()
	assertGraphAcceptanceEmpty(t, ctx, reopened)
	accepted, err := AcceptGraphProposal(ctx, reopened, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph-contention-crash",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now,
	})
	if err != nil {
		t.Fatalf("AcceptGraphProposal() retry after contention crash error = %v", err)
	}
	replayed, err := AcceptGraphProposal(ctx, reopened, proposal, AcceptOptions{
		ExpectedAuthorizationFingerprint: proposal.AuthorizationFingerprint,
		IdempotencyKey:                   "accept-graph-contention-crash",
		Actor:                            graphActor(),
		Host:                             graphHost(),
		Now:                              input.Now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("AcceptGraphProposal() replay after contention retry error = %v", err)
	}
	if replayed.GraphVersionID != accepted.GraphVersionID || replayed.CanonicalProposalHash != accepted.CanonicalProposalHash {
		t.Fatalf("replayed accepted version = %#v, want %#v", replayed, accepted)
	}
	assertAcceptedGraphAuthorityCounts(t, ctx, reopened, 1, 1, 1, 1, 1, 1)
}

type injectedGraphCommitCause struct {
	phase string
}

func (e *injectedGraphCommitCause) Error() string {
	return "injected delivery transaction " + e.phase
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

func openGraphStore(t *testing.T, ctx context.Context, path string) storage.Store {
	t.Helper()
	return openGraphStoreWithOptions(t, ctx, storage.Options{Path: path, Now: func() time.Time { return fixedGraphTime() }})
}

func openGraphStoreWithOptions(t *testing.T, ctx context.Context, opts storage.Options) storage.Store {
	t.Helper()
	store, err := storage.Open(ctx, opts)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	return store
}

func seedGraphProject(t *testing.T, ctx context.Context, store storage.Store, projectID string) {
	t.Helper()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?)`, projectID, "/repo/"+projectID, delivery.CanonicalTimestamp(fixedGraphTime()), delivery.CanonicalTimestamp(fixedGraphTime()))
		return err
	}); err != nil {
		t.Fatalf("seed graph project: %v", err)
	}
}

func assertGraphAcceptanceEmpty(t *testing.T, ctx context.Context, store storage.Store) {
	t.Helper()
	for _, table := range []string{
		"delivery_runs",
		"delivery_plan_fingerprints",
		"delivery_tasks",
		"delivery_dependency_edges",
		"delivery_decisions",
		"delivery_approvals",
		"delivery_idempotency",
		"fallback_decisions",
		"replan_decisions",
		"verification_decisions",
		"task_requirements",
		"task_graph_validations",
		"accepted_task_graph_versions",
	} {
		assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM `+table, 0)
	}
}

func assertAcceptedGraphAuthorityCounts(t *testing.T, ctx context.Context, store storage.Store, runs, approvals, tasks, requirements, versions, idempotency int) {
	t.Helper()
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_runs`, runs)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_approvals`, approvals)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_tasks`, tasks)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM task_requirements`, requirements)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM accepted_task_graph_versions`, versions)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_idempotency`, idempotency)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM delivery_decisions`, 0)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM fallback_decisions`, 0)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM replan_decisions`, 0)
	assertGraphCount(t, ctx, store, `SELECT COUNT(*) FROM verification_decisions`, 0)
}

func assertGraphCount(t *testing.T, ctx context.Context, store storage.Store, query string, want int) {
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
		"task_graph_validations",
		"accepted_task_graph_versions",
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
