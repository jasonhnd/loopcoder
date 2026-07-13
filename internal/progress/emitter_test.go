package progress

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestEmitterFakeClockTwentyMinuteFixtureHasNoReceiptGap(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	ticks := newManualTicker()
	emitter := newTestEmitter(t, store, EmitterConfig{
		MaxGenerationSilence: 5 * time.Minute,
		NewTicker: func(time.Duration) TickSource {
			return ticks
		},
	})

	loop, err := emitter.Start(ctx, aliveObservation(clock.Now()))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < 4; i++ {
		clock.Advance(5 * time.Minute)
		ticks.Tick(clock.Now())
		waitForReceiptCount(t, ctx, store, i+2)
	}
	if err := loop.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	receipts := listEmitterReceipts(t, ctx, store)
	if len(receipts) != 5 {
		t.Fatalf("receipt count = %d, want 5", len(receipts))
	}
	for i := 1; i < len(receipts); i++ {
		prev := mustParseTime(t, receipts[i-1].OccurredAt)
		next := mustParseTime(t, receipts[i].OccurredAt)
		if gap := next.Sub(prev); gap > 5*time.Minute {
			t.Fatalf("gap %d = %s, want <= 5m", i, gap)
		}
	}
	if receipts[len(receipts)-1].Progress.AgeMillis != int64((20*time.Minute + 4*time.Second).Milliseconds()) {
		t.Fatalf("last progress age = %d, want truthful stale age", receipts[len(receipts)-1].Progress.AgeMillis)
	}
}

func TestEmitterAliveNoProgressDoesNotResetIndependentStallDeadline(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	emitter := newTestEmitter(t, store, EmitterConfig{MaxGenerationSilence: 5 * time.Minute})
	stallDeadline := clock.Now().Add(7 * time.Minute)

	if _, err := emitter.Emit(ctx, aliveObservation(clock.Now())); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	clock.Advance(5 * time.Minute)
	if _, err := emitter.emitPeriodic(ctx); err != nil {
		t.Fatalf("emitPeriodic: %v", err)
	}
	clock.Advance(3 * time.Minute)
	if clock.Now().Before(stallDeadline) {
		t.Fatalf("clock = %s before stall deadline %s", clock.Now(), stallDeadline)
	}

	receipts := listEmitterReceipts(t, ctx, store)
	last := receipts[len(receipts)-1]
	if last.Progress.State != "stale" || last.Blocker.State != "none" || last.NextAction.State != "continue" {
		t.Fatalf("stalled receipt = %#v, want alive/no-progress without blocker invention", last)
	}
	if !containsString(last.GapReasons, "no-meaningful-progress-observed") {
		t.Fatalf("gap reasons = %v, want no-meaningful-progress-observed", last.GapReasons)
	}
}

func TestEmitterDeduplicatesUnchangedPeriodicObservationBeforePolicyGap(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	emitter := newTestEmitter(t, store, EmitterConfig{MaxGenerationSilence: 5 * time.Minute})

	if _, err := emitter.Emit(ctx, aliveObservation(clock.Now())); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	clock.Advance(4 * time.Minute)
	result, err := emitter.emitPeriodic(ctx)
	if err != nil {
		t.Fatalf("emitPeriodic: %v", err)
	}
	if result.Emitted || countReceipts(t, ctx, store) != 1 {
		t.Fatalf("early periodic result/count = %#v/%d, want no new durable receipt", result, countReceipts(t, ctx, store))
	}
	clock.Advance(time.Minute)
	result, err = emitter.emitPeriodic(ctx)
	if err != nil {
		t.Fatalf("emitPeriodic at policy: %v", err)
	}
	if !result.Emitted || countReceipts(t, ctx, store) != 2 {
		t.Fatalf("policy periodic result/count = %#v/%d, want durable receipt", result, countReceipts(t, ctx, store))
	}
}

func TestEmitterDoesNotRenewRunClaimLeaseOrHeartbeat(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	claimedAt := "2026-07-13T11:55:00Z"
	heartbeatAt := "2026-07-13T11:56:00Z"
	leaseExpiresAt := "2026-07-13T12:30:00Z"
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO runs(id, project_id, status, updated_at) VALUES ('run_progress', 'proj_progress', 'running', ?) ON CONFLICT(id) DO NOTHING`, claimedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO run_claims(run_id, executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at, phase, provider_idempotency_key, provider_receipt)
			VALUES ('run_progress', 'executor-a', 7, ?, ?, ?, 'running', 'provider-key', '')`, claimedAt, leaseExpiresAt, heartbeatAt)
		return err
	}); err != nil {
		t.Fatalf("seed run claim: %v", err)
	}
	emitter := newTestEmitter(t, store, EmitterConfig{MaxGenerationSilence: 5 * time.Minute})
	if _, err := emitter.Emit(ctx, aliveObservation(clock.Now())); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var gotGeneration int64
	var gotHeartbeat, gotLease string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT claim_generation, heartbeat_at, lease_expires_at FROM run_claims WHERE run_id = 'run_progress'`).Scan(&gotGeneration, &gotHeartbeat, &gotLease)
	}); err != nil {
		t.Fatalf("query run claim: %v", err)
	}
	if gotGeneration != 7 || gotHeartbeat != heartbeatAt || gotLease != leaseExpiresAt {
		t.Fatalf("claim mutated generation=%d heartbeat=%q lease=%q", gotGeneration, gotHeartbeat, gotLease)
	}
}

func TestEmitterImmediateTransitionsAndTruthfulKnownStates(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	emitter := newTestEmitter(t, store, EmitterConfig{MaxGenerationSilence: 5 * time.Minute})

	states := []string{
		KnownWaitingCI,
		KnownWaitingApproval,
		KnownQuotaBlocked,
		KnownFallbackInProgress,
		KnownCancellationInProgress,
		KnownRecoveryInProgress,
		KnownHostOffline,
		KnownDeliveryPending,
	}
	for _, state := range states {
		clock.Advance(time.Second)
		obs := aliveObservation(clock.Now())
		obs.KnownState = state
		obs.Phase = state
		obs.Status = ""
		if _, err := emitter.Emit(ctx, obs); err != nil {
			t.Fatalf("Emit %s: %v", state, err)
		}
	}

	receipts := listEmitterReceipts(t, ctx, store)
	if len(receipts) != len(states) {
		t.Fatalf("receipt count = %d, want %d", len(receipts), len(states))
	}
	assertReceiptState := func(index int, blocker, next string) {
		t.Helper()
		if receipts[index].Blocker.State != blocker || receipts[index].NextAction.State != next {
			t.Fatalf("receipt[%d] blocker/next = %q/%q, want %q/%q", index, receipts[index].Blocker.State, receipts[index].NextAction.State, blocker, next)
		}
	}
	assertReceiptState(0, "waiting", "wait")
	assertReceiptState(1, "waiting", "wait")
	assertReceiptState(2, "quota-blocked", "wait")
	assertReceiptState(3, "none", "fallback-in-progress")
	assertReceiptState(4, "none", "cancel")
	assertReceiptState(5, "none", "recover")
	assertReceiptState(6, "host-offline", "delivery-pending")
	assertReceiptState(7, "none", "delivery-pending")
}

func TestEmitterUsesNoProviderCallsAndDeliveryFailureDoesNotSuppressPersistence(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	var providerCalls atomic.Int64
	deliveryErr := errors.New("delivery unavailable")
	emitter := newTestEmitter(t, store, EmitterConfig{
		MaxGenerationSilence: 5 * time.Minute,
		Deliver: func(context.Context, ProgressReceipt) error {
			return deliveryErr
		},
	})

	result, err := emitter.Emit(ctx, aliveObservation(clock.Now()))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !result.Emitted || !result.DeliveryAttempted || !errors.Is(result.DeliveryErr, deliveryErr) {
		t.Fatalf("emit result = %#v, want persisted with delivery error recorded", result)
	}
	if providerCalls.Load() != 0 {
		t.Fatalf("provider calls = %d, want 0", providerCalls.Load())
	}
	if countReceipts(t, ctx, store) != 1 {
		t.Fatalf("receipt persistence suppressed by delivery failure")
	}
}

func TestEmitterTerminalStopsTickerAndEmitsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	ticks := newManualTicker()
	emitter := newTestEmitter(t, store, EmitterConfig{
		MaxGenerationSilence: 5 * time.Minute,
		NewTicker: func(time.Duration) TickSource {
			return ticks
		},
	})
	loop, err := emitter.Start(ctx, aliveObservation(clock.Now()))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	terminal := aliveObservation(clock.Now())
	terminal.Status = "succeeded"
	if _, err := loop.Terminal(ctx, terminal); err != nil {
		t.Fatalf("Terminal: %v", err)
	}
	clock.Advance(10 * time.Minute)
	ticks.Tick(clock.Now())
	time.Sleep(10 * time.Millisecond)

	receipts := listEmitterReceipts(t, ctx, store)
	if len(receipts) != 2 {
		t.Fatalf("receipt count after terminal tick = %d, want 2", len(receipts))
	}
	if receipts[1].Status != "succeeded" || receipts[1].NextAction.State != "none" {
		t.Fatalf("terminal receipt = %#v", receipts[1])
	}
	if result, err := emitter.EmitTerminal(ctx, terminal); err != nil || result.Emitted {
		t.Fatalf("second terminal = %#v / %v, want no-op", result, err)
	}
}

func TestEmitterCancellationJoinsTickerWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	ticks := newManualTicker()
	emitter := newTestEmitter(t, store, EmitterConfig{
		MaxGenerationSilence: 5 * time.Minute,
		NewTicker: func(time.Duration) TickSource {
			return ticks
		},
	})
	loop, err := emitter.Start(ctx, aliveObservation(clock.Now()))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()
	if err := loop.Stop(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop after cancellation = %v, want nil or context.Canceled", err)
	}
	if !ticks.stopped.Load() {
		t.Fatal("ticker was not stopped after cancellation join")
	}
}

func TestEmitterRestartReplayContinuesCorrelationSequence(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	emitter := newTestEmitter(t, store, EmitterConfig{MaxGenerationSilence: 5 * time.Minute})
	if _, err := emitter.Emit(ctx, aliveObservation(clock.Now())); err != nil {
		t.Fatalf("first Emit: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	clock.Advance(5 * time.Minute)
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	restarted := newTestEmitter(t, reopened, EmitterConfig{MaxGenerationSilence: 5 * time.Minute})
	obs := aliveObservation(clock.Now())
	obs.KnownState = KnownWaitingCI
	if _, err := restarted.Emit(ctx, obs); err != nil {
		t.Fatalf("restarted Emit: %v", err)
	}

	receipts := listEmitterReceipts(t, ctx, reopened)
	if len(receipts) != 2 || receipts[0].CorrelationSequence != 1 || receipts[1].CorrelationSequence != 2 {
		t.Fatalf("replay sequences = %#v, want 1 then 2", receipts)
	}
}

func TestEmitterDetachedNestedRaceSafe(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	emitter := newTestEmitter(t, store, EmitterConfig{MaxGenerationSilence: 5 * time.Minute})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			obs := aliveObservation(clock.Now())
			if index%2 == 0 {
				obs.RunID = "child_run"
				obs.TaskID = "child_task"
				obs.AttemptID = "child_attempt"
			}
			_ = emitter.Observe(obs)
			_, _ = emitter.Emit(ctx, obs)
		}(i)
	}
	wg.Wait()
	if countReceipts(t, ctx, store) == 0 {
		t.Fatal("detached/nested concurrent emitter did not persist any receipts")
	}
}

func TestEmitterRejectsOutOfBoundsSilenceConfiguration(t *testing.T) {
	ctx := context.Background()
	clock := newEmitterClock(fixedTime)
	store := newStoreWithClock(t, ctx, clock)
	defer store.Close()
	if _, err := NewEmitter(store, EmitterConfig{MaxGenerationSilence: time.Millisecond}); !errors.Is(err, ErrEmitterConfig) {
		t.Fatalf("NewEmitter short interval error = %v, want ErrEmitterConfig", err)
	}
	if _, err := NewEmitter(store, EmitterConfig{MaxGenerationSilence: 2 * time.Hour}); !errors.Is(err, ErrEmitterConfig) {
		t.Fatalf("NewEmitter long interval error = %v, want ErrEmitterConfig", err)
	}
}

type emitterClock struct {
	mu  sync.Mutex
	now time.Time
}

func newEmitterClock(now time.Time) *emitterClock {
	return &emitterClock{now: now.UTC()}
}

func (c *emitterClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *emitterClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

type manualTicker struct {
	ch      chan time.Time
	stopped atomic.Bool
}

func newManualTicker() *manualTicker {
	return &manualTicker{ch: make(chan time.Time, 16)}
}

func (t *manualTicker) C() <-chan time.Time {
	return t.ch
}

func (t *manualTicker) Stop() {
	t.stopped.Store(true)
}

func (t *manualTicker) Tick(now time.Time) {
	if t.stopped.Load() {
		return
	}
	t.ch <- now
}

func newStoreWithClock(t *testing.T, ctx context.Context, clock *emitterClock) storage.Store {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: clock.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	return store
}

func newTestEmitter(t *testing.T, store storage.Store, config EmitterConfig) *Emitter {
	t.Helper()
	emitter, err := NewEmitter(store, config)
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	return emitter
}

func aliveObservation(now time.Time) Observation {
	return Observation{
		ProjectID:           "proj_progress",
		DeliveryRunID:       "run_progress",
		RunID:               "run_progress",
		TaskID:              "task_progress",
		AttemptID:           "att_progress_1",
		AttemptOrdinal:      1,
		CorrelationID:       "corr_supervisor",
		Phase:               "supervising",
		Status:              "running",
		KnownState:          KnownAliveNoMeaningfulProgress,
		TaskCounts:          TaskCounts{Total: 1, Running: 1},
		HeartbeatObservedAt: now,
		HeartbeatState:      "exact",
		ProgressObservedAt:  now.Add(-4 * time.Second),
		ProgressState:       "stale",
		Provider:            ProviderIdentity{ProviderID: "codex", ModelID: "gpt-5.5", ProviderConfidence: "exact"},
	}
}

func listEmitterReceipts(t *testing.T, ctx context.Context, store storage.Store) []ProgressReceipt {
	t.Helper()
	receipts, err := ListReceipts(ctx, store, ListFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr_supervisor"})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	return receipts
}

func waitForReceiptCount(t *testing.T, ctx context.Context, store storage.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := countReceipts(t, ctx, store); got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("receipt count = %d, want at least %d", countReceipts(t, ctx, store), want)
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse time %q: %v", value, err)
	}
	return parsed
}
