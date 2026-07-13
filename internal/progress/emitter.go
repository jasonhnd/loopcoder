package progress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	DefaultMaxGenerationSilence = 5 * time.Minute
	MinMaxGenerationSilence     = time.Second
	MaxMaxGenerationSilence     = time.Hour

	ReasonStateChange          = "state-change"
	ReasonMaxGenerationSilence = "max-generation-silence"
	ReasonTerminal             = "terminal"

	KnownAliveNoMeaningfulProgress = "alive-but-no-meaningful-progress"
	KnownWaitingCI                 = "waiting-for-ci"
	KnownWaitingApproval           = "waiting-for-approval"
	KnownQuotaBlocked              = "quota-blocked"
	KnownFallbackInProgress        = "fallback-in-progress"
	KnownHostOffline               = "host-offline"
	KnownDeliveryPending           = "delivery-pending"
	KnownRecoveryInProgress        = "recovery-in-progress"
	KnownBlocked                   = "blocked"
	KnownCancellationInProgress    = "cancellation-in-progress"
	KnownTerminal                  = "terminal"

	// DefaultMaxSilenceInterval is the v0.8 receipt-generation SLO: an active
	// LoopCoder-owned run should not go more than five minutes without a durable
	// generated receipt.
	DefaultMaxSilenceInterval = 5 * time.Minute
	// MinMaxSilenceInterval and MaxMaxSilenceInterval are documented guardrails
	// for tests and future config surfaces. Values outside this range are
	// rejected so a bad config cannot busy-loop or silently disable receipts.
	MinMaxSilenceInterval = time.Second
	MaxMaxSilenceInterval = time.Hour
)

var (
	ErrEmitterConfig = errors.New("progress emitter config")
	ErrEmitterClosed = errors.New("progress emitter closed")
)

type Clock interface {
	Now() time.Time
	NewTicker(time.Duration) Ticker
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type TickSource = Ticker
type TickerFactory func(time.Duration) TickSource

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) NewTicker(interval time.Duration) Ticker {
	return realTicker{ticker: time.NewTicker(interval)}
}

type realTicker struct {
	ticker *time.Ticker
}

func (t realTicker) C() <-chan time.Time { return t.ticker.C }
func (t realTicker) Stop()               { t.ticker.Stop() }

type Observation struct {
	ProjectID           string
	DeliveryRunID       string
	RunID               string
	TaskID              string
	AttemptID           string
	AttemptOrdinal      int
	CorrelationID       string
	Phase               string
	Status              string
	KnownState          string
	Reason              string
	TaskCounts          TaskCounts
	Provider            ProviderIdentity
	Heartbeat           AgeEvidence
	Progress            AgeEvidence
	HeartbeatObservedAt time.Time
	HeartbeatState      string
	ProgressObservedAt  time.Time
	ProgressState       string
	Evidence            []EvidenceRef
	QuotaBudget         QuotaBudgetState
	Blocker             ActionState
	NextAction          ActionState
	GapReasons          []string
	OccurredAt          time.Time
	Terminal            bool
}

type DeliveryFunc func(context.Context, ProgressReceipt) error
type DeliveryAttemptFunc = DeliveryFunc

type EmitterConfig struct {
	MaxGenerationSilence time.Duration
	NewTicker            TickerFactory
	Deliver              DeliveryAttemptFunc
}

type EmitterOptions struct {
	Store              storage.Store
	ProjectID          string
	DeliveryRunID      string
	RunID              string
	CorrelationID      string
	MaxSilenceInterval time.Duration
	Clock              Clock
	Deliver            DeliveryFunc
}

type EmitResult struct {
	WriteResult
	Emitted           bool
	DeliveryAttempted bool
	DeliveryErr       error
}

type Emitter struct {
	store         storage.Store
	projectID     string
	deliveryRunID string
	runID         string
	correlationID string
	interval      time.Duration
	clock         Clock
	deliver       DeliveryFunc

	mu                 sync.Mutex
	latest             Observation
	hasLatest          bool
	lastObservationKey string
	lastDurableAt      time.Time
	closed             bool
	started            bool

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	doneCh    chan struct{}
}

type Loop struct {
	emitter *Emitter
}

func NewEmitter(args ...any) (*Emitter, error) {
	switch len(args) {
	case 1:
		opts, ok := args[0].(EmitterOptions)
		if !ok {
			return nil, typed(ErrInvalidRecordCode, "NewEmitter requires EmitterOptions")
		}
		return newEmitterFromOptions(opts, true)
	case 2:
		store, ok := args[0].(storage.Store)
		if !ok {
			return nil, typed(ErrInvalidRecordCode, "NewEmitter requires storage.Store")
		}
		config, ok := args[1].(EmitterConfig)
		if !ok {
			return nil, typed(ErrInvalidRecordCode, "NewEmitter requires EmitterConfig")
		}
		interval := config.MaxGenerationSilence
		if interval == 0 {
			interval = DefaultMaxSilenceInterval
		}
		opts := EmitterOptions{
			Store:              store,
			MaxSilenceInterval: interval,
			Clock:              storeTickerClock{store: store, factory: config.NewTicker},
			Deliver:            config.Deliver,
		}
		return newEmitterFromOptions(opts, false)
	default:
		return nil, typed(ErrInvalidRecordCode, "NewEmitter argument count is invalid")
	}
}

func newEmitterFromOptions(opts EmitterOptions, requireIdentity bool) (*Emitter, error) {
	if opts.Store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	projectID := strings.TrimSpace(opts.ProjectID)
	deliveryRunID := strings.TrimSpace(opts.DeliveryRunID)
	if requireIdentity && (projectID == "" || deliveryRunID == "") {
		return nil, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = realClock{}
	}
	interval, err := boundMaxSilence(opts.MaxSilenceInterval)
	if err != nil {
		return nil, err
	}
	return &Emitter{
		store:         opts.Store,
		projectID:     projectID,
		deliveryRunID: deliveryRunID,
		runID:         firstNonEmpty(opts.RunID, deliveryRunID),
		correlationID: firstNonEmpty(opts.CorrelationID, deliveryRunID),
		interval:      interval,
		clock:         clock,
		deliver:       opts.Deliver,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}, nil
}

type storeTickerClock struct {
	store   storage.Store
	factory TickerFactory
}

func (c storeTickerClock) Now() time.Time {
	if c.store == nil {
		return time.Now().UTC()
	}
	return c.store.Now().UTC()
}

func (c storeTickerClock) NewTicker(interval time.Duration) Ticker {
	if c.factory != nil {
		return c.factory(interval)
	}
	return realClock{}.NewTicker(interval)
}

func (e *Emitter) Start(ctx context.Context, initial ...Observation) (*Loop, error) {
	if e == nil {
		return nil, ErrEmitterClosed
	}
	if len(initial) > 0 {
		if _, err := e.Emit(ctx, initial[0]); err != nil {
			return nil, err
		}
	}
	e.startOnce.Do(func() {
		e.mu.Lock()
		e.started = true
		e.mu.Unlock()
		go e.run(ctx)
	})
	return &Loop{emitter: e}, nil
}

func (l *Loop) Stop() error {
	if l == nil || l.emitter == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return l.emitter.Stop(ctx)
}

func (e *Emitter) Observe(observation Observation) error {
	if e == nil {
		return ErrEmitterClosed
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrEmitterClosed
	}
	e.latest = cloneObservation(observation)
	e.hasLatest = true
	return nil
}

func (e *Emitter) Stop(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if !started {
		return nil
	}
	e.stopOnce.Do(func() {
		close(e.stopCh)
	})
	select {
	case <-e.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Emitter) Emit(ctx context.Context, observation Observation) (EmitResult, error) {
	return e.emit(ctx, observation, false)
}

func (e *Emitter) EmitTerminal(ctx context.Context, observation Observation) (EmitResult, error) {
	if strings.TrimSpace(observation.Reason) == "" {
		observation.Reason = ReasonTerminal
	}
	return e.Terminal(ctx, observation)
}

func (e *Emitter) Terminal(ctx context.Context, observation Observation) (EmitResult, error) {
	observation.Terminal = true
	result, err := e.emit(ctx, observation, false)
	if err != nil {
		return result, err
	}
	if err := e.Stop(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func (e *Emitter) run(ctx context.Context) {
	defer close(e.doneCh)
	ticker := e.clock.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ticker.C():
			_, _ = e.emitPeriodic(ctx)
		}
	}
}

func (e *Emitter) emitPeriodic(ctx context.Context) (EmitResult, error) {
	return e.emit(ctx, Observation{}, true)
}

func (e *Emitter) emit(ctx context.Context, observation Observation, periodic bool) (EmitResult, error) {
	if e == nil {
		return EmitResult{}, ErrEmitterClosed
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return EmitResult{}, ErrEmitterClosed
	}
	now := e.clock.Now().UTC()
	if periodic {
		if !e.hasLatest {
			return EmitResult{}, nil
		}
		observation = e.latest
		observation.Terminal = false
		if !e.lastDurableAt.IsZero() && now.Sub(e.lastDurableAt) < e.interval {
			return EmitResult{}, nil
		}
		if observationKey(observation) == e.lastObservationKey {
			observation.GapReasons = appendUnique(observation.GapReasons, "max-generation-silence")
		}
	} else {
		e.latest = cloneObservation(observation)
		e.hasLatest = true
	}

	receipt := e.receiptFor(observation, now, periodic)
	result, err := PersistReceiptNextSequence(ctx, e.store, receipt)
	if err != nil {
		return EmitResult{WriteResult: result}, err
	}
	if !periodic {
		e.lastObservationKey = observationKey(observation)
	}
	e.lastDurableAt = now
	out := EmitResult{WriteResult: result, Emitted: result.Inserted}
	if observation.Terminal {
		e.closed = true
	}
	if e.deliver != nil {
		out.DeliveryAttempted = true
		out.DeliveryErr = e.deliver(ctx, result.Receipt)
	}
	return out, err
}

func (e *Emitter) receiptFor(observation Observation, now time.Time, periodic bool) ProgressReceipt {
	observation = normalizeObservation(observation, now)
	occurredAt := observation.OccurredAt.UTC()
	if occurredAt.IsZero() {
		occurredAt = now
	}
	evidence := append([]EvidenceRef(nil), observation.Evidence...)
	if periodic {
		evidence = append(evidence, EvidenceRef{
			RecordKind:     "supervisor-clock",
			RecordID:       e.correlationID,
			Summary:        "max-silence receipt generated from supervisor state",
			Classification: "local-diagnostic",
			Confidence:     "exact",
		})
	}
	return ProgressReceipt{
		ProjectID:      firstNonEmpty(observation.ProjectID, e.projectID),
		DeliveryRunID:  firstNonEmpty(observation.DeliveryRunID, e.deliveryRunID),
		RunID:          firstNonEmpty(observation.RunID, e.runID, observation.DeliveryRunID, e.deliveryRunID),
		TaskID:         observation.TaskID,
		AttemptID:      observation.AttemptID,
		AttemptOrdinal: observation.AttemptOrdinal,
		CorrelationID:  firstNonEmpty(observation.CorrelationID, e.correlationID, observation.AttemptID),
		Phase:          firstNonEmpty(observation.Phase, Unknown),
		Status:         firstNonEmpty(observation.Status, Unknown),
		TaskCounts:     observation.TaskCounts,
		Provider:       observation.Provider,
		Heartbeat:      observation.Heartbeat,
		Progress:       observation.Progress,
		Evidence:       evidence,
		QuotaBudget:    observation.QuotaBudget,
		Blocker:        observation.Blocker,
		NextAction:     observation.NextAction,
		GapReasons:     append([]string(nil), observation.GapReasons...),
		OccurredAt:     deliveryTimestamp(occurredAt),
	}
}

func normalizeObservation(observation Observation, now time.Time) Observation {
	if observation.Heartbeat.ObservedAt == "" && !observation.HeartbeatObservedAt.IsZero() {
		observation.Heartbeat = AgeEvidence{
			State:      firstNonEmpty(observation.HeartbeatState, "exact"),
			ObservedAt: deliveryTimestamp(observation.HeartbeatObservedAt),
			AgeMillis:  now.Sub(observation.HeartbeatObservedAt.UTC()).Milliseconds(),
		}
	}
	if observation.Progress.ObservedAt == "" && !observation.ProgressObservedAt.IsZero() {
		observation.Progress = AgeEvidence{
			State:      firstNonEmpty(observation.ProgressState, "exact"),
			ObservedAt: deliveryTimestamp(observation.ProgressObservedAt),
			AgeMillis:  now.Sub(observation.ProgressObservedAt.UTC()).Milliseconds(),
		}
	}
	if observation.KnownState != "" {
		observation.GapReasons = appendUnique(observation.GapReasons, observation.KnownState)
		if observation.Status == "" {
			observation.Status = observation.KnownState
		}
		if observation.Blocker.State == "" && observation.NextAction.State == "" {
			observation.Blocker, observation.NextAction = knownStateActions(observation.KnownState)
		}
	}
	if observation.Reason != "" {
		observation.GapReasons = appendUnique(observation.GapReasons, observation.Reason)
	}
	if observation.Terminal {
		observation.GapReasons = appendUnique(observation.GapReasons, ReasonTerminal)
	}
	observation.Heartbeat = refreshObservationAge(observation.Heartbeat, now)
	observation.Progress = refreshObservationAge(observation.Progress, now)
	return observation
}

func refreshObservationAge(age AgeEvidence, now time.Time) AgeEvidence {
	if strings.TrimSpace(age.ObservedAt) == "" {
		return age
	}
	parsed, err := time.Parse(time.RFC3339Nano, age.ObservedAt)
	if err != nil {
		return age
	}
	age.AgeMillis = now.UTC().Sub(parsed.UTC()).Milliseconds()
	if age.AgeMillis < 0 {
		age.AgeMillis = 0
	}
	if age.State == "" || age.State == Unknown {
		age.State = "exact"
	}
	return age
}

func knownStateActions(known string) (ActionState, ActionState) {
	switch known {
	case KnownWaitingCI, KnownWaitingApproval:
		return ActionState{State: "waiting"}, ActionState{State: "wait"}
	case KnownQuotaBlocked:
		return ActionState{State: "quota-blocked"}, ActionState{State: "wait"}
	case KnownFallbackInProgress:
		return ActionState{State: "none"}, ActionState{State: "fallback-in-progress"}
	case KnownCancellationInProgress:
		return ActionState{State: "none"}, ActionState{State: "cancel"}
	case KnownRecoveryInProgress:
		return ActionState{State: "none"}, ActionState{State: "recover"}
	case KnownHostOffline:
		return ActionState{State: "host-offline"}, ActionState{State: "delivery-pending"}
	case KnownDeliveryPending:
		return ActionState{State: "none"}, ActionState{State: "delivery-pending"}
	case KnownBlocked:
		return ActionState{State: "blocked"}, ActionState{State: "wait"}
	default:
		return ActionState{State: "none"}, ActionState{State: "continue"}
	}
}

func boundMaxSilence(interval time.Duration) (time.Duration, error) {
	if interval <= 0 {
		return DefaultMaxSilenceInterval, nil
	}
	if interval < MinMaxSilenceInterval {
		return 0, fmt.Errorf("%w: max silence interval %s is below minimum %s", ErrEmitterConfig, interval, MinMaxSilenceInterval)
	}
	if interval > MaxMaxSilenceInterval {
		return 0, fmt.Errorf("%w: max silence interval %s exceeds maximum %s", ErrEmitterConfig, interval, MaxMaxSilenceInterval)
	}
	return interval, nil
}

func observationKey(observation Observation) string {
	return strings.Join([]string{
		observation.Phase,
		observation.Status,
		observation.Blocker.State,
		observation.Blocker.Summary,
		observation.NextAction.State,
		observation.NextAction.Summary,
		strings.Join(observation.GapReasons, ","),
	}, "\x00")
}

func cloneObservation(observation Observation) Observation {
	observation.Evidence = append([]EvidenceRef(nil), observation.Evidence...)
	observation.GapReasons = append([]string(nil), observation.GapReasons...)
	return observation
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func deliveryTimestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
