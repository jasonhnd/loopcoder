package budget

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	_, err = Commit(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "replay-commit", Generation: first.Reservation.Generation, Value: 10, Actor: testActor(), Host: testHost()})
	if !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("stale commit replay error = %v, want ErrReservationExpired", err)
	}
	replayedCommit, err := Commit(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "replay-commit", Generation: committed.Reservation.Generation, Value: 10, Actor: testActor(), Host: testHost()})
	if err != nil {
		t.Fatalf("Commit current-generation replay: %v", err)
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
	_, err = Release(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "replay-release", Generation: committed.Reservation.Generation, Actor: testActor(), Host: testHost()})
	if !errors.Is(err, ErrReservationExpired) {
		t.Fatalf("stale release replay error = %v, want ErrReservationExpired", err)
	}
	replayedRelease, err := Release(ctx, store, MutationRequest{ReservationID: first.Reservation.BudgetReservationID, IdempotencyKey: "replay-release", Generation: released.Reservation.Generation, Actor: testActor(), Host: testHost()})
	if err != nil {
		t.Fatalf("Release current-generation replay: %v", err)
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

func TestSoftBudgetWarnOnlyRecordsBreachAndAllowsReserve(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(7, 0).UTC()}
	store := openBudgetStore(t, clock)
	defer store.Close()
	scope := Scope{ScopeKind: ScopeProject, ProjectID: "proj_budget"}
	policy := upsertPolicyMode(t, ctx, store, scope, PolicySoft, OverflowWarnOnly, 5, "soft-warn")

	result, err := Reserve(ctx, store, ReserveRequest{
		ScopeChain:            []Scope{scope},
		QuantityKind:          providerinventory.QuantityTotalTokens,
		WindowKind:            providerinventory.WindowUnbounded,
		RequestedValue:        8,
		LeaseExpiresAt:        clock.Now().Add(time.Hour),
		IdempotencyKey:        "soft-warn-reserve",
		RequesterID:           "requester-soft",
		RequirementConfidence: providerinventory.ConfidenceExact,
		Actor:                 testActor(),
		Host:                  testHost(),
	})
	if err != nil {
		t.Fatalf("Reserve soft warn-only: %v", err)
	}
	if !containsString(result.Reservation.GapReasons, "soft-budget-warn-only:"+policy.BudgetPolicyID) {
		t.Fatalf("reservation gap reasons = %#v, want soft warning", result.Reservation.GapReasons)
	}
	summary := onlySummary(t, ctx, store, "proj_budget")
	if summary.ReservedValue != 8 || !containsString(summary.GapReasons, "soft-budget-overflow:"+policy.BudgetPolicyID) {
		t.Fatalf("summary = %#v, want soft overflow signal", summary)
	}
}

func TestSoftBudgetRequiresApprovalForBreach(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(8, 0).UTC()}
	store := openBudgetStore(t, clock)
	defer store.Close()
	scope := Scope{ScopeKind: ScopeProject, ProjectID: "proj_budget"}
	policy := upsertPolicyMode(t, ctx, store, scope, PolicySoft, OverflowRequiresApproval, 5, "soft-approval")

	_, err := Reserve(ctx, store, ReserveRequest{
		ScopeChain:            []Scope{scope},
		QuantityKind:          providerinventory.QuantityTotalTokens,
		WindowKind:            providerinventory.WindowUnbounded,
		RequestedValue:        8,
		LeaseExpiresAt:        clock.Now().Add(time.Hour),
		IdempotencyKey:        "soft-approval-denied",
		RequesterID:           "requester-soft",
		RequirementConfidence: providerinventory.ConfidenceExact,
		Actor:                 testActor(),
		Host:                  testHost(),
	})
	if !errors.Is(err, ErrBudgetApprovalRequired) {
		t.Fatalf("Reserve soft requires-approval error = %v, want ErrBudgetApprovalRequired", err)
	}
	if summary := onlySummary(t, ctx, store, "proj_budget"); summary.ReservedValue != 0 {
		t.Fatalf("summary = %#v, want no reservation without approval", summary)
	}

	result, err := Reserve(ctx, store, ReserveRequest{
		ScopeChain:            []Scope{scope},
		QuantityKind:          providerinventory.QuantityTotalTokens,
		WindowKind:            providerinventory.WindowUnbounded,
		RequestedValue:        8,
		LeaseExpiresAt:        clock.Now().Add(time.Hour),
		IdempotencyKey:        "soft-approval-granted",
		RequesterID:           "requester-soft",
		RequirementConfidence: providerinventory.ConfidenceExact,
		ApprovalID:            "approval_soft_budget",
		Actor:                 testActor(),
		Host:                  testHost(),
	})
	if err != nil {
		t.Fatalf("Reserve soft with approval: %v", err)
	}
	if !containsString(result.Reservation.GapReasons, "soft-budget-approved:"+policy.BudgetPolicyID) {
		t.Fatalf("reservation gap reasons = %#v, want approved soft breach", result.Reservation.GapReasons)
	}
}

func TestReservationIDAndSpecFieldsPersist(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{now: time.Unix(9, 0).UTC()}
	store := openBudgetStore(t, clock)
	defer store.Close()
	scope := Scope{ScopeKind: ScopeProject, ProjectID: "proj_budget"}
	policy := upsertPolicy(t, ctx, store, scope, 100, "project")

	reserved, err := Reserve(ctx, store, ReserveRequest{
		ScopeChain:                   []Scope{scope},
		QuantityKind:                 providerinventory.QuantityTotalTokens,
		WindowKind:                   providerinventory.WindowUnbounded,
		RequestedValue:               20,
		LeaseExpiresAt:               clock.Now().Add(time.Hour),
		IdempotencyKey:               "field-reserve",
		RequesterID:                  "requester-a",
		AuthorizationFingerprint:     "sha256:authorization",
		SourceEstimateUsageRecordIDs: []string{"usage_estimate_1"},
		RequirementConfidence:        providerinventory.ConfidenceExact,
		Actor:                        testActor(),
		Host:                         testHost(),
	})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	wantID := reservationID("field-reserve", policy.BudgetPolicyID, "requester-a")
	if reserved.Reservation.BudgetReservationID != wantID {
		t.Fatalf("reservation id = %q, want %q", reserved.Reservation.BudgetReservationID, wantID)
	}
	if reserved.Reservation.RequesterID != "requester-a" || reserved.Reservation.AuthorizationFingerprint != "sha256:authorization" || !containsString(reserved.Reservation.SourceEstimateUsageRecordIDs, "usage_estimate_1") {
		t.Fatalf("reservation fields = %#v, want requester/auth/source usage ids", reserved.Reservation)
	}
	committed, err := Commit(ctx, store, MutationRequest{
		ReservationID:  reserved.Reservation.BudgetReservationID,
		IdempotencyKey: "field-commit",
		Generation:     reserved.Reservation.Generation,
		Value:          7,
		UsageRecordIDs: []string{"usage_commit_1"},
		Actor:          testActor(),
		Host:           testHost(),
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	released, err := Release(ctx, store, MutationRequest{
		ReservationID:  committed.Reservation.BudgetReservationID,
		IdempotencyKey: "field-release",
		Generation:     committed.Reservation.Generation,
		UsageRecordIDs: []string{"usage_release_1"},
		Actor:          testActor(),
		Host:           testHost(),
	})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !containsString(committed.Reservation.CommitUsageRecordIDs, "usage_commit_1") || !containsString(released.Reservation.ReleaseUsageRecordIDs, "usage_release_1") {
		t.Fatalf("usage record ids commit=%#v release=%#v", committed.Reservation.CommitUsageRecordIDs, released.Reservation.ReleaseUsageRecordIDs)
	}
}

func TestWithBudgetRetryRetriesWrappedSQLiteBusy(t *testing.T) {
	busyErr := wrappedSQLiteBusyError(t)
	attempts := 0
	err := withBudgetRetry(context.Background(), func() error {
		attempts++
		if attempts == 1 {
			return busyErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withBudgetRetry: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want retry after wrapped SQLITE_BUSY", attempts)
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

func upsertPolicyMode(t *testing.T, ctx context.Context, store storage.Store, scope Scope, mode PolicyMode, overflow SoftOverflowBehavior, ceiling int64, ordinal string) Policy {
	t.Helper()
	policy, err := UpsertPolicy(ctx, store, PolicyInput{
		Scope:            scope,
		QuantityKind:     providerinventory.QuantityTotalTokens,
		WindowKind:       providerinventory.WindowUnbounded,
		PolicyMode:       mode,
		OverflowBehavior: overflow,
		CeilingValue:     ceiling,
		PolicyVersion:    "test-v1",
		Ordinal:          ordinal,
		Actor:            testActor(),
		Host:             testHost(),
	})
	if err != nil {
		t.Fatalf("UpsertPolicy: %v", err)
	}
	return policy
}

func upsertPolicy(t *testing.T, ctx context.Context, store storage.Store, scope Scope, ceiling int64, ordinal string) Policy {
	t.Helper()
	return upsertPolicyMode(t, ctx, store, scope, PolicyHard, "", ceiling, ordinal)
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func wrappedSQLiteBusyError(t *testing.T) error {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	db1, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db1: %v", err)
	}
	defer db1.Close()
	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	defer db2.Close()
	for _, db := range []*sql.DB{db1, db2} {
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 1`); err != nil {
			t.Fatalf("set busy timeout: %v", err)
		}
	}
	conn1, err := db1.Conn(ctx)
	if err != nil {
		t.Fatalf("conn1: %v", err)
	}
	defer conn1.Close()
	if _, err := conn1.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin lock: %v", err)
	}
	defer conn1.ExecContext(ctx, `ROLLBACK`)
	_, err = db2.ExecContext(ctx, `BEGIN IMMEDIATE`)
	if err == nil {
		t.Fatal("second BEGIN IMMEDIATE succeeded, want SQLITE_BUSY")
	}
	if !storage.IsBusy(err) {
		t.Fatalf("generated error %T %[1]v is not storage.IsBusy", err)
	}
	return fmt.Errorf("wrapped busy: %w", err)
}
