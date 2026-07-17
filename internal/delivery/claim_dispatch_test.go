package delivery

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestClaimOneReadyTaskSelectsZeroOrOne(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	store := openClaimDispatchStore(t, ctx, now)
	run := seedClaimableRun(t, ctx, store, now, "task-a", "task-b")

	first, err := ClaimOneReadyTask(ctx, store, ClaimDispatchOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		ExpectedAuthorizationFingerprint: run.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "claim-1",
		Now:                              now,
		HostEnforcement:                  SupportedHostEnforcement("test", "unit"),
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !first.Claimed || first.Outcome != OutcomeClaimedDispatch {
		t.Fatalf("first claim = %#v", first)
	}
	if first.TaskKey != "task-a" {
		t.Fatalf("selected task_key = %q, want deterministic first task-a", first.TaskKey)
	}

	second, err := ClaimOneReadyTask(ctx, store, ClaimDispatchOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		ExpectedAuthorizationFingerprint: run.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "claim-2",
		Now:                              now.Add(time.Second),
		HostEnforcement:                  SupportedHostEnforcement("test", "unit"),
	})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if !second.Claimed || second.TaskKey != "task-b" {
		t.Fatalf("second claim = %#v, want task-b", second)
	}

	third, err := ClaimOneReadyTask(ctx, store, ClaimDispatchOptions{
		ProjectID:                        run.ProjectID,
		DeliveryRunID:                    run.DeliveryRunID,
		ExpectedAuthorizationFingerprint: run.AuthorizationFingerprint,
		Actor:                            actor(),
		Host:                             host(),
		IdempotencyKey:                   "claim-3",
		Now:                              now.Add(2 * time.Second),
		HostEnforcement:                  SupportedHostEnforcement("test", "unit"),
	})
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if third.Claimed || third.Outcome != OutcomeNoReadyTask {
		t.Fatalf("third claim = %#v, want no ready task", third)
	}
}

func TestClaimOneReadyTaskConcurrentSingleWinner(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	store := openClaimDispatchStore(t, ctx, now)
	run := seedClaimableRun(t, ctx, store, now, "only-task")

	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := ClaimOneReadyTask(ctx, store, ClaimDispatchOptions{
				ProjectID:                        run.ProjectID,
				DeliveryRunID:                    run.DeliveryRunID,
				ExpectedAuthorizationFingerprint: run.AuthorizationFingerprint,
				Actor:                            actor(),
				Host:                             host(),
				IdempotencyKey:                   "race-" + string(rune('a'+i)),
				Now:                              now,
				HostEnforcement:                  SupportedHostEnforcement("test", "unit"),
			})
			if err != nil {
				return
			}
			if result.Claimed {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners.Load())
	}
}

func openClaimDispatchStore(t *testing.T, ctx context.Context, now time.Time) storage.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedClaimableRun(t *testing.T, ctx context.Context, store storage.Store, now time.Time, taskKeys ...string) DeliveryRun {
	t.Helper()
	seedProject(t, ctx, store, "proj-claim")
	run := deliveryRunFixture("proj-claim", "drun-claim")
	run.State = RunQueued
	run.ApprovalStatus = "approved"
	// Empty authorization fingerprints skip approval-row lookup in claim authority.
	run.AuthorizationFingerprint = ""
	run, err := PersistDeliveryRun(ctx, store, run, PersistOptions{Now: now, IdempotencyKey: "seed-run"})
	if err != nil {
		t.Fatalf("persist run: %v", err)
	}
	for _, key := range taskKeys {
		task := taskFixture(run, key)
		task.State = TaskReady
		task.Title = "Issue #1010 " + key
		task.AuthorizationFingerprint = ""
		if _, err := PersistTask(ctx, store, task, PersistOptions{Now: now, IdempotencyKey: "seed-task-" + key}); err != nil {
			t.Fatalf("persist task %s: %v", key, err)
		}
	}
	return run
}
