package progress

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestEmitterTwentyMinuteFixtureHonorsMaxSilence(t *testing.T) {
	ctx := context.Background()
	clock := newManualClock(fixedTime)
	store := newClockStore(t, ctx, clock)
	defer store.Close()

	emitter, err := NewEmitter(EmitterOptions{
		Store:              store,
		ProjectID:          "proj_progress",
		DeliveryRunID:      "run_progress",
		RunID:              "run_progress",
		CorrelationID:      "corr_fixture",
		MaxSilenceInterval: 5 * time.Minute,
		Clock:              clock,
	})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	emitter.Start(ctx)
	if _, err := emitter.Emit(ctx, runningObservation(clock.Now())); err != nil {
		t.Fatalf("Emit initial: %v", err)
	}
	for i := 0; i < 4; i++ {
		clock.Advance(5 * time.Minute)
		waitForReceiptCount(t, ctx, store, i+2)
	}
	if err := emitter.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	receipts, err := ListReceipts(ctx, store, ListFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr_fixture"})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	if len(receipts) != 5 {
		t.Fatalf("receipt count = %d, want 5", len(receipts))
	}
	for i := 1; i < len(receipts); i++ {
		prev := mustParseReceiptTime(t, receipts[i-1].PersistedAt)
		next := mustParseReceiptTime(t, receipts[i].PersistedAt)
		if gap := next.Sub(prev); gap > 5*time.Minute {
			t.Fatalf("receipt persistence gap %s between %s and %s exceeds policy", gap, receipts[i-1].PersistedAt, receipts[i].PersistedAt)
		}
	}
	periodic := 0
	for _, receipt := range receipts[1:] {
		if containsString(receipt.GapReasons, "max-generation-silence") {
			periodic++
		}
		if receipt.OccurredAt != receipts[0].OccurredAt {
			t.Fatalf("periodic occurred_at = %s, want original observation time %s", receipt.OccurredAt, receipts[0].OccurredAt)
		}
	}
	if periodic != 4 {
		t.Fatalf("periodic max-silence receipts = %d, want 4", periodic)
	}
	if !containsString(receipts[len(receipts)-1].GapReasons, "max-generation-silence") {
		t.Fatalf("last periodic gap reasons = %#v, want max-generation-silence", receipts[len(receipts)-1].GapReasons)
	}
	if receipts[len(receipts)-1].Progress.AgeMillis != int64((20 * time.Minute).Milliseconds()) {
		t.Fatalf("last progress age = %d, want 20 minutes of no meaningful progress", receipts[len(receipts)-1].Progress.AgeMillis)
	}
}

func TestSupervisorTickerOutlivesCallerContextUntilOwnerStops(t *testing.T) {
	callerCtx, cancelCaller := context.WithCancel(context.Background())
	clock := newManualClock(fixedTime)
	store := newClockStore(t, context.Background(), clock)
	defer store.Close()
	supervisor, err := NewSupervisor(SupervisorOptions{
		Store:              store,
		ProjectID:          "proj_progress",
		DeliveryRunID:      "run_progress",
		RunID:              "run_progress",
		MaxSilenceInterval: 5 * time.Minute,
		Clock:              clock,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if _, err := supervisor.Emit(callerCtx, runningObservation(clock.Now())); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	cancelCaller()
	clock.Advance(5 * time.Minute)
	waitForReceiptCount(t, context.Background(), store, 2)
	if got := countReceipts(t, context.Background(), store); got != 2 {
		t.Fatalf("receipt count after caller cancellation = %d, want initial + detached periodic", got)
	}
	if err := supervisor.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	clock.Advance(5 * time.Minute)
	time.Sleep(10 * time.Millisecond)
	if got := countReceipts(t, context.Background(), store); got != 2 {
		t.Fatalf("receipt count after owner stop = %d, want no further ticker receipts", got)
	}
}

func TestEmitterPostTerminalNonTerminalIsNoOp(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()
	emitter := mustEmitter(t, store, nil, "corr_terminal")

	if _, err := emitter.Terminal(ctx, terminalObservation(fixedTime)); err != nil {
		t.Fatalf("Terminal: %v", err)
	}
	if _, err := emitter.Emit(ctx, runningObservation(fixedTime.Add(time.Second))); !errors.Is(err, ErrEmitterClosed) {
		t.Fatalf("post-terminal Emit error = %v, want ErrEmitterClosed", err)
	}
	if got := countReceipts(t, ctx, store); got != 1 {
		t.Fatalf("receipt count after post-terminal emit = %d, want 1", got)
	}
}

func TestEmitterTerminalBlocksConcurrentOrdinaryEmit(t *testing.T) {
	ctx := context.Background()
	base := newStore(t, ctx)
	defer base.Close()
	blocking := newBlockingWriteStoreAfter(base, 0)
	emitter := mustEmitter(t, blocking, nil, "corr_terminal_barrier")

	terminalDone := make(chan error, 1)
	go func() {
		_, err := emitter.Terminal(ctx, terminalObservation(fixedTime))
		terminalDone <- err
	}()
	<-blocking.entered

	ordinaryDone := make(chan error, 1)
	go func() {
		_, err := emitter.Emit(ctx, runningObservation(fixedTime.Add(time.Second)))
		ordinaryDone <- err
	}()

	select {
	case err := <-ordinaryDone:
		t.Fatalf("ordinary emit completed before terminal barrier released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(blocking.release)
	if err := <-terminalDone; err != nil {
		t.Fatalf("Terminal: %v", err)
	}
	if err := <-ordinaryDone; !errors.Is(err, ErrEmitterClosed) {
		t.Fatalf("ordinary emit after terminal error = %v, want ErrEmitterClosed", err)
	}
	if got := countReceipts(t, ctx, base); got != 1 {
		t.Fatalf("receipt count = %d, want terminal only", got)
	}
}

func TestEmitterTerminalBlocksConcurrentPeriodicEmit(t *testing.T) {
	ctx := context.Background()
	clock := newManualClock(fixedTime)
	base := newClockStore(t, ctx, clock)
	defer base.Close()
	blocking := newBlockingWriteStoreAfter(base, 1)
	emitter := mustEmitter(t, blocking, clock, "corr_periodic_barrier")
	emitter.Start(ctx)
	if _, err := emitter.Emit(ctx, runningObservation(clock.Now())); err != nil {
		t.Fatalf("Emit initial: %v", err)
	}

	terminalDone := make(chan error, 1)
	go func() {
		_, err := emitter.Terminal(ctx, terminalObservation(clock.Now().Add(time.Minute)))
		terminalDone <- err
	}()
	<-blocking.entered
	clock.Advance(5 * time.Minute)

	select {
	case err := <-terminalDone:
		t.Fatalf("terminal completed before barrier released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(blocking.release)
	if err := <-terminalDone; err != nil {
		t.Fatalf("Terminal: %v", err)
	}
	waitForReceiptCount(t, ctx, base, 2)
	if got := countReceipts(t, ctx, base); got != 2 {
		t.Fatalf("receipt count = %d, want initial + terminal with no post-terminal periodic", got)
	}
}

func TestEmitterTerminalPersistenceFailureAllowsRetry(t *testing.T) {
	ctx := context.Background()
	base := newStore(t, ctx)
	defer base.Close()
	failing := &failFirstWriteStore{Store: base}
	emitter := mustEmitter(t, failing, nil, "corr_terminal_retry")

	if _, err := emitter.Terminal(ctx, terminalObservation(fixedTime)); err == nil {
		t.Fatal("first Terminal succeeded, want storage failure")
	}
	if got := countReceipts(t, ctx, base); got != 0 {
		t.Fatalf("receipt count after failed terminal = %d, want 0", got)
	}
	if _, err := emitter.Terminal(ctx, terminalObservation(fixedTime.Add(time.Second))); err != nil {
		t.Fatalf("retry Terminal: %v", err)
	}
	if got := countReceipts(t, ctx, base); got != 1 {
		t.Fatalf("receipt count after retry = %d, want 1", got)
	}
}

func TestEmitterDeliveryFailureDoesNotSuppressPersistence(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()
	emitter := mustEmitter(t, store, nil, "corr_delivery_failure")
	emitter.deliver = func(context.Context, ProgressReceipt) error {
		return errors.New("delivery unavailable")
	}
	if _, err := emitter.Emit(ctx, runningObservation(fixedTime)); err != nil {
		t.Fatalf("Emit with delivery failure: %v", err)
	}
	if got := countReceipts(t, ctx, store); got != 1 {
		t.Fatalf("receipt count = %d, want persisted receipt", got)
	}
}

func TestEmitterCompatibilityKnownStatesEmitTruthfulActions(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()
	emitter, err := NewEmitter(store, EmitterConfig{MaxGenerationSilence: 5 * time.Minute})
	if err != nil {
		t.Fatalf("NewEmitter compatibility: %v", err)
	}
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
	for _, known := range states {
		obs := runningObservation(fixedTime)
		obs.ProjectID = "proj_progress"
		obs.DeliveryRunID = "run_progress"
		obs.RunID = "run_progress"
		obs.CorrelationID = "corr_known_states"
		obs.Phase = known
		obs.Status = ""
		obs.KnownState = known
		obs.Blocker = ActionState{}
		obs.NextAction = ActionState{}
		if _, err := emitter.Emit(ctx, obs); err != nil {
			t.Fatalf("Emit %s: %v", known, err)
		}
	}
	receipts, err := ListReceipts(ctx, store, ListFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr_known_states"})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
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

func TestEmitterDoesNotRenewRunClaimLeaseOrHeartbeat(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
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
	emitter := mustEmitter(t, store, nil, "corr_claim_independent")
	if _, err := emitter.Emit(ctx, runningObservation(fixedTime)); err != nil {
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

func TestEmitterMaxSilenceBounds(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()
	if _, err := NewEmitter(EmitterOptions{
		Store:              store,
		ProjectID:          "proj_progress",
		DeliveryRunID:      "run_progress",
		CorrelationID:      "corr_bounds_low",
		MaxSilenceInterval: time.Millisecond,
	}); !errors.Is(err, ErrEmitterConfig) {
		t.Fatalf("low interval error = %v, want ErrEmitterConfig", err)
	}
	if _, err := NewEmitter(EmitterOptions{
		Store:              store,
		ProjectID:          "proj_progress",
		DeliveryRunID:      "run_progress",
		CorrelationID:      "corr_bounds_high",
		MaxSilenceInterval: 2 * time.Hour,
	}); !errors.Is(err, ErrEmitterConfig) {
		t.Fatalf("high interval error = %v, want ErrEmitterConfig", err)
	}
}

func TestEmitterOverlappingInstancesAllocateDistinctSequences(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()
	first := mustEmitter(t, store, nil, "corr_overlap")
	second := mustEmitter(t, store, nil, "corr_overlap")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, emitter := range []*Emitter{first, second} {
		wg.Add(1)
		go func(emitter *Emitter) {
			defer wg.Done()
			_, err := emitter.Emit(ctx, runningObservation(fixedTime))
			errs <- err
		}(emitter)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Emit overlap: %v", err)
		}
	}
	receipts, err := ListReceipts(ctx, store, ListFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr_overlap"})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	var sequences []int64
	for _, receipt := range receipts {
		sequences = append(sequences, receipt.CorrelationSequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	if len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 {
		t.Fatalf("sequences = %v, want [1 2]", sequences)
	}
}

func TestSupervisorTerminalCorrelationDoesNotCloseSiblingsAndCanRestart(t *testing.T) {
	ctx := context.Background()
	clock := newManualClock(fixedTime)
	store := newClockStore(t, ctx, clock)
	defer store.Close()
	supervisor, err := NewSupervisor(SupervisorOptions{
		Store:              store,
		ProjectID:          "proj_progress",
		DeliveryRunID:      "run_progress",
		RunID:              "run_progress",
		MaxSilenceInterval: 5 * time.Minute,
		Clock:              clock,
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	taskA := runningObservation(clock.Now())
	taskA.CorrelationID = "task:a"
	taskA.TaskID = "a"
	taskB := runningObservation(clock.Now())
	taskB.CorrelationID = "task:b"
	taskB.TaskID = "b"
	if _, err := supervisor.Emit(ctx, taskA); err != nil {
		t.Fatalf("Emit task A: %v", err)
	}
	if _, err := supervisor.Emit(ctx, taskB); err != nil {
		t.Fatalf("Emit task B: %v", err)
	}
	terminalA := terminalObservation(clock.Now().Add(time.Minute))
	terminalA.CorrelationID = "task:a"
	terminalA.TaskID = "a"
	if _, err := supervisor.Terminal(ctx, terminalA); err != nil {
		t.Fatalf("Terminal task A: %v", err)
	}
	taskB.Status = "running-after-a-terminal"
	if _, err := supervisor.Emit(ctx, taskB); err != nil {
		t.Fatalf("Emit task B after task A terminal: %v", err)
	}
	taskA.Status = "running-after-terminal"
	taskA.Phase = "restarted"
	if _, err := supervisor.Emit(ctx, taskA); err != nil {
		t.Fatalf("Emit task A after terminal should restart correlation: %v", err)
	}

	clock.Advance(5 * time.Minute)
	waitForReceiptCount(t, ctx, store, 6)
	if err := supervisor.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	taskAReceipts, err := ListReceipts(ctx, store, ListFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "task:a"})
	if err != nil {
		t.Fatalf("ListReceipts task A: %v", err)
	}
	taskBReceipts, err := ListReceipts(ctx, store, ListFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "task:b"})
	if err != nil {
		t.Fatalf("ListReceipts task B: %v", err)
	}
	if len(taskAReceipts) < 3 ||
		!receiptsContainStatus(taskAReceipts, "succeeded") ||
		!receiptsContainStatus(taskAReceipts, "running-after-terminal") {
		t.Fatalf("task A receipts = %#v, want initial + terminal + restarted state", taskAReceipts)
	}
	if len(taskBReceipts) != 3 || !containsString(taskBReceipts[len(taskBReceipts)-1].GapReasons, "max-generation-silence") {
		t.Fatalf("task B receipts = %#v, want sibling state change and periodic max-silence", taskBReceipts)
	}
}

func receiptsContainStatus(receipts []ProgressReceipt, status string) bool {
	for _, receipt := range receipts {
		if receipt.Status == status {
			return true
		}
	}
	return false
}

func mustEmitter(t *testing.T, store storage.Store, clock Clock, correlation string) *Emitter {
	t.Helper()
	emitter, err := NewEmitter(EmitterOptions{
		Store:              store,
		ProjectID:          "proj_progress",
		DeliveryRunID:      "run_progress",
		RunID:              "run_progress",
		CorrelationID:      correlation,
		MaxSilenceInterval: 5 * time.Minute,
		Clock:              clock,
	})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	return emitter
}

func runningObservation(at time.Time) Observation {
	return Observation{
		TaskID:         "task_progress",
		AttemptID:      "att_progress_1",
		AttemptOrdinal: 1,
		Phase:          "codex_started",
		Status:         "running",
		TaskCounts:     TaskCounts{Total: 1, Running: 1},
		Provider:       ProviderIdentity{ProviderID: "codex", ProviderConfidence: "exact"},
		Heartbeat:      AgeEvidence{State: "exact", ObservedAt: deliveryTimestamp(at), AgeMillis: 0},
		Progress:       AgeEvidence{State: "exact", ObservedAt: deliveryTimestamp(at), AgeMillis: 0},
		Blocker:        ActionState{State: "none"},
		NextAction:     ActionState{State: "wait-provider", Summary: "worker process is alive; no model-authored progress is inferred"},
		GapReasons:     []string{"alive-but-no-meaningful-progress"},
		OccurredAt:     at,
	}
}

func terminalObservation(at time.Time) Observation {
	obs := runningObservation(at)
	obs.Phase = "cleanup"
	obs.Status = "succeeded"
	obs.NextAction = ActionState{State: "complete", Summary: "worker attempt succeeded"}
	obs.GapReasons = nil
	return obs
}

func newClockStore(t *testing.T, ctx context.Context, clock *manualClock) storage.Store {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: clock.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	return store
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

func mustParseReceiptTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse receipt time %q: %v", value, err)
	}
	return parsed
}

type manualClock struct {
	mu  sync.Mutex
	now time.Time
	ch  chan time.Time
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now.UTC(), ch: make(chan time.Time, 16)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTicker(time.Duration) Ticker {
	return manualTicker{ch: c.ch}
}

func (c *manualClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	now := c.now
	c.mu.Unlock()
	c.ch <- now
}

type manualTicker struct {
	ch <-chan time.Time
}

func (t manualTicker) C() <-chan time.Time { return t.ch }
func (t manualTicker) Stop()               {}

type blockingWriteStore struct {
	storage.Store
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	skip    int
	blocked bool
}

func newBlockingWriteStoreAfter(store storage.Store, skip int) *blockingWriteStore {
	return &blockingWriteStore{Store: store, entered: make(chan struct{}), release: make(chan struct{}), skip: skip}
}

func (s *blockingWriteStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	block := false
	s.mu.Lock()
	if s.skip > 0 {
		s.skip--
	} else if !s.blocked {
		s.blocked = true
		block = true
		close(s.entered)
	}
	s.mu.Unlock()
	if block {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.WithWriteTx(ctx, fn)
}

type failFirstWriteStore struct {
	storage.Store
	once sync.Once
}

func (s *failFirstWriteStore) WithWriteTx(ctx context.Context, fn func(storage.Tx) error) error {
	failed := false
	s.once.Do(func() {
		failed = true
	})
	if failed {
		return errors.New("injected write failure")
	}
	return s.Store.WithWriteTx(ctx, fn)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
