package budget

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestReserveCommitReleaseHappyPath(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(1, 0).UTC()}
	store := openBudgetStore(t, clock)
	defer store.Close()

	scope := Scope{ScopeKind: ScopeProject, ProjectID: "proj_budget"}
	policy := upsertPolicy(t, ctx, store, scope, 100, "project")
	reserved, err := Reserve(ctx, store, ReserveRequest{
		ScopeChain:            []Scope{scope},
		QuantityKind:          providerinventory.QuantityTotalTokens,
		WindowKind:            providerinventory.WindowUnbounded,
		RequestedValue:        40,
		LeaseExpiresAt:        clock.Now().Add(time.Hour),
		IdempotencyKey:        "happy-reserve",
		RequirementConfidence: providerinventory.ConfidenceExact,
		Actor:                 testActor(),
		Host:                  testHost(),
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if len(reserved.Reservation.PolicyIDs) != 1 || reserved.Reservation.PolicyIDs[0] != policy.BudgetPolicyID {
		t.Fatalf("reservation policy ids = %#v, want %s", reserved.Reservation.PolicyIDs, policy.BudgetPolicyID)
	}
	committed, err := Commit(ctx, store, MutationRequest{
		ReservationID:  reserved.Reservation.BudgetReservationID,
		IdempotencyKey: "happy-commit",
		Generation:     reserved.Reservation.Generation,
		Value:          25,
		Actor:          testActor(),
		Host:           testHost(),
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	released, err := Release(ctx, store, MutationRequest{
		ReservationID:  committed.Reservation.BudgetReservationID,
		IdempotencyKey: "happy-release",
		Generation:     committed.Reservation.Generation,
		Actor:          testActor(),
		Host:           testHost(),
	})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if released.Reservation.State != StateReleased || released.Reservation.CommittedValue != 25 || released.Reservation.ReleasedValue != 15 {
		t.Fatalf("released reservation = %#v, want committed 25 released 15", released.Reservation)
	}
	summary := onlySummary(t, ctx, store, "proj_budget")
	if summary.ReservedValue != 0 || summary.CommittedValue != 25 || summary.AvailableValue != 75 {
		t.Fatalf("summary = %#v, want reserved 0 committed 25 available 75", summary)
	}
}

func TestOverReservationAtAnyScopeRollsBackAllAggregates(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		limitedKind ScopeKind
	}{
		{name: "machine", limitedKind: ScopeMachine},
		{name: "project", limitedKind: ScopeProject},
		{name: "delivery-run", limitedKind: ScopeDeliveryRun},
		{name: "task", limitedKind: ScopeTask},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{now: time.Unix(2, 0).UTC()}
			store := openBudgetStore(t, clock)
			defer store.Close()
			chain := []Scope{
				{ScopeKind: ScopeMachine},
				{ScopeKind: ScopeProject, ProjectID: "proj_budget"},
				{ScopeKind: ScopeDeliveryRun, ProjectID: "proj_budget", DeliveryRunID: "run_budget"},
				{ScopeKind: ScopeTask, ProjectID: "proj_budget", DeliveryRunID: "run_budget", TaskID: "task_budget"},
			}
			for _, scope := range chain {
				ceiling := int64(10)
				if scope.ScopeKind == tc.limitedKind {
					ceiling = 5
				}
				upsertPolicy(t, ctx, store, scope, ceiling, string(scope.ScopeKind))
			}
			_, err := Reserve(ctx, store, ReserveRequest{
				ScopeChain:            chain,
				QuantityKind:          providerinventory.QuantityTotalTokens,
				WindowKind:            providerinventory.WindowUnbounded,
				RequestedValue:        6,
				LeaseExpiresAt:        clock.Now().Add(time.Hour),
				IdempotencyKey:        "over-" + tc.name,
				RequirementConfidence: providerinventory.ConfidenceExact,
				Actor:                 testActor(),
				Host:                  testHost(),
			})
			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("Reserve error = %v, want ErrBudgetExhausted", err)
			}
			summaries, err := Summaries(ctx, store, "proj_budget")
			if err != nil {
				t.Fatalf("Summaries: %v", err)
			}
			for _, summary := range summaries {
				if summary.ReservedValue != 0 || summary.CommittedValue != 0 {
					t.Fatalf("scope %s summary = %#v, want no partial aggregate mutation", summary.Scope.ScopeKind, summary)
				}
			}
		})
	}
}

func TestConcurrentReservationsDoNotOversubscribeHardCeiling(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(3, 0).UTC()}
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := Scope{ScopeKind: ScopeProject, ProjectID: "proj_budget"}
	upsertPolicy(t, ctx, store, scope, 5, "project")
	store.Close()

	const workers = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	successes := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			workerStore, err := storage.Open(ctx, storage.Options{Path: path, Now: clock.Now})
			if err != nil {
				t.Errorf("worker open: %v", err)
				return
			}
			defer workerStore.Close()
			_, err = Reserve(ctx, workerStore, ReserveRequest{
				ScopeChain:            []Scope{scope},
				QuantityKind:          providerinventory.QuantityTotalTokens,
				WindowKind:            providerinventory.WindowUnbounded,
				RequestedValue:        1,
				LeaseExpiresAt:        clock.Now().Add(time.Hour),
				IdempotencyKey:        "concurrent-reserve-" + string(rune('a'+index)),
				RequirementConfidence: providerinventory.ConfidenceExact,
				Actor:                 testActor(),
				Host:                  testHost(),
			})
			switch {
			case err == nil:
				successes <- true
			case errors.Is(err, ErrBudgetExhausted):
				successes <- false
			default:
				t.Errorf("Reserve worker %d: %v", index, err)
			}
		}(i)
	}
	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent reservations did not finish")
	}
	close(successes)
	var successCount int
	for success := range successes {
		if success {
			successCount++
		}
	}
	if successCount != 5 {
		t.Fatalf("successful reservations = %d, want 5", successCount)
	}
	verifyStore, err := storage.Open(ctx, storage.Options{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	defer verifyStore.Close()
	summary := onlySummary(t, ctx, verifyStore, "proj_budget")
	if summary.ReservedValue != 5 || summary.CommittedValue != 0 || summary.AvailableValue != 0 {
		t.Fatalf("summary = %#v, want exactly exhausted without negative balance", summary)
	}
}

func TestReserveCommitReleaseReplaysAreIdempotentAndFenced(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(4, 0).UTC()}
	store := openBudgetStore(t, clock)
	defer store.Close()
	scope := Scope{ScopeKind: ScopeProject, ProjectID: "proj_budget"}
	upsertPolicy(t, ctx, store, scope, 100, "project")
	req := ReserveRequest{
		ScopeChain:            []Scope{scope},
		QuantityKind:          providerinventory.QuantityTotalTokens,
		WindowKind:            providerinventory.WindowUnbounded,
		RequestedValue:        30,
		LeaseExpiresAt:        clock.Now().Add(time.Hour),
		IdempotencyKey:        "replay-reserve",
		RequirementConfidence: providerinventory.ConfidenceExact,
		Actor:                 testActor(),
		Host:                  testHost(),
	}
	first, err := Reserve(ctx, store, req)
	if err != nil {
		t.Fatalf("Reserve first: %v", err)
	}
	second, err := Reserve(ctx, store, req)
	if err != nil {
		t.Fatalf("Reserve replay: %v", err)
	}
	if !second.Replay || first.Reservation.BudgetReservationID != second.Reservation.BudgetReservationID {
		t.Fatalf("reserve replay = %#v, want same reservation replay", second)
	}
	committed, err := Commit(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "replay-commit", Generation: first.Reservation.Generation, Value: 10, Actor: testActor(), Host: testHost()})
	if err != nil {
		t.Fatalf("Commit first: %v", err)
	}
	replayedCommit, err := Commit(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "replay-commit", Generation: first.Reservation.Generation, Value: 10, Actor: testActor(), Host: testHost()})
	if err != nil {
		t.Fatalf("Commit replay: %v", err)
	}
	if !replayedCommit.Replay {
		t.Fatalf("commit replay = %#v, want replay", replayedCommit)
	}
	err = func() error {
		_, staleErr := Release(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "stale-release", Generation: first.Reservation.Generation, Value: 1, Actor: testActor(), Host: testHost()})
		return staleErr
	}()
	if !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("stale release error = %v, want ErrReservationExpired", err)
	}
	released, err := Release(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "replay-release", Generation: committed.Reservation.Generation, Actor: testActor(), Host: testHost()})
	if err != nil {
		t.Fatalf("Release first: %v", err)
	}
	replayedRelease, err := Release(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "replay-release", Generation: committed.Reservation.Generation, Actor: testActor(), Host: testHost()})
	if err != nil {
		t.Fatalf("Release replay: %v", err)
	}
	if !replayedRelease.Replay || released.Reservation.ReleasedValue != replayedRelease.Reservation.ReleasedValue {
		t.Fatalf("release replay = %#v, want same release", replayedRelease)
	}
	summary := onlySummary(t, ctx, store, "proj_budget")
	if summary.ReservedValue != 0 || summary.CommittedValue != 10 {
		t.Fatalf("summary = %#v, want no duplicate commit/release", summary)
	}
}

func TestRestartAndStaleLeaseRecoveryDoNotLeakReservation(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(5, 0).UTC()}
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := Scope{ScopeKind: ScopeProject, ProjectID: "proj_budget"}
	upsertPolicy(t, ctx, store, scope, 20, "project")
	reserved, err := Reserve(ctx, store, ReserveRequest{
		ScopeChain:            []Scope{scope},
		QuantityKind:          providerinventory.QuantityTotalTokens,
		WindowKind:            providerinventory.WindowUnbounded,
		RequestedValue:        12,
		LeaseExpiresAt:        clock.Now().Add(time.Minute),
		IdempotencyKey:        "restart-reserve",
		RequirementConfidence: providerinventory.ConfidenceExact,
		Actor:                 testActor(),
		Host:                  testHost(),
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	store.Close()

	clock.Advance(2 * time.Minute)
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := Commit(ctx, reopened, MutationRequest{ReservationID: reserved.Reservation.BudgetReservationID, IdempotencyKey: "expired-commit", Generation: reserved.Reservation.Generation, Value: 1, Actor: testActor(), Host: testHost()}); !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("expired commit error = %v, want ErrReservationExpired", err)
	}
	expired, err := ExpireStale(ctx, reopened, clock.Now(), testActor(), testHost())
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if len(expired) != 1 || expired[0].State != StateExpired || expired[0].ReleasedValue != 12 {
		t.Fatalf("expired reservations = %#v, want one released expired reservation", expired)
	}
	summary := onlySummary(t, ctx, reopened, "proj_budget")
	if summary.ReservedValue != 0 || summary.CommittedValue != 0 || summary.AvailableValue != 20 {
		t.Fatalf("summary = %#v, want full capacity recovered", summary)
	}
}

func TestEstimatedRequirementRequiresApproval(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(6, 0).UTC()}
	store := openBudgetStore(t, clock)
	defer store.Close()
	scope := Scope{ScopeKind: ScopeProject, ProjectID: "proj_budget"}
	upsertPolicy(t, ctx, store, scope, 100, "project")
	_, err := Reserve(ctx, store, ReserveRequest{
		ScopeChain:            []Scope{scope},
		QuantityKind:          providerinventory.QuantityTotalTokens,
		WindowKind:            providerinventory.WindowUnbounded,
		RequestedValue:        10,
		LeaseExpiresAt:        clock.Now().Add(time.Hour),
		IdempotencyKey:        "estimate-no-approval",
		RequirementConfidence: providerinventory.ConfidenceEstimated,
		Actor:                 testActor(),
		Host:                  testHost(),
	})
	if !errors.Is(err, providerinventory.ErrQuotaConfidenceInsufficient) {
		t.Fatalf("Reserve estimated without approval = %v, want ErrQuotaConfidenceInsufficient", err)
	}
	summary := onlySummary(t, ctx, store, "proj_budget")
	if summary.ReservedValue != 0 {
		t.Fatalf("summary = %#v, want no reservation without approval", summary)
	}
	_, err = Reserve(ctx, store, ReserveRequest{
		ScopeChain:            []Scope{scope},
		QuantityKind:          providerinventory.QuantityTotalTokens,
		WindowKind:            providerinventory.WindowUnbounded,
		RequestedValue:        10,
		LeaseExpiresAt:        clock.Now().Add(time.Hour),
		IdempotencyKey:        "estimate-with-approval",
		RequirementConfidence: providerinventory.ConfidenceEstimated,
		ApprovalID:            "approval_budget_estimate",
		Actor:                 testActor(),
		Host:                  testHost(),
	})
	if err != nil {
		t.Fatalf("Reserve estimated with approval: %v", err)
	}
	summary = onlySummary(t, ctx, store, "proj_budget")
	if summary.ReservedValue != 10 {
		t.Fatalf("summary = %#v, want approved estimated reservation", summary)
	}
}

func openBudgetStore(t *testing.T, clock *fakeClock) storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: clock.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func upsertPolicy(t *testing.T, ctx context.Context, store storage.Store, scope Scope, ceiling int64, ordinal string) Policy {
	t.Helper()
	policy, err := UpsertPolicy(ctx, store, PolicyInput{
		Scope:         scope,
		QuantityKind:  providerinventory.QuantityTotalTokens,
		WindowKind:    providerinventory.WindowUnbounded,
		PolicyMode:    PolicyHard,
		CeilingValue:  ceiling,
		PolicyVersion: "test-v1",
		Ordinal:       ordinal,
		Actor:         testActor(),
		Host:          testHost(),
	})
	if err != nil {
		t.Fatalf("UpsertPolicy: %v", err)
	}
	return policy
}

func onlySummary(t *testing.T, ctx context.Context, store storage.Store, projectID string) Summary {
	t.Helper()
	summaries, err := Summaries(ctx, store, projectID)
	if err != nil {
		t.Fatalf("Summaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summaries = %#v, want one", summaries)
	}
	return summaries[0]
}

func testActor() Actor {
	return Actor{ActorID: "test-actor", Role: "test"}
}

func testHost() Host {
	return Host{HostID: "test-host", Provider: "test-provider", Model: "test-model"}
}
