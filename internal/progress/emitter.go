package progress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
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
)

var (
	ErrEmitterConfig = errors.New("progress emitter config")
	ErrEmitterClosed = errors.New("progress emitter closed")
)

type TickSource interface {
	C() <-chan time.Time
	Stop()
}

type TickerFactory func(time.Duration) TickSource

type DeliveryAttemptFunc func(context.Context, ProgressReceipt) error

type EmitterConfig struct {
	MaxGenerationSilence time.Duration
	NewTicker            TickerFactory
	Deliver              DeliveryAttemptFunc
}

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
	Terminal            bool
	TaskCounts          TaskCounts
	Provider            ProviderIdentity
	HeartbeatObservedAt time.Time
	HeartbeatState      string
	ProgressObservedAt  time.Time
	ProgressState       string
	Evidence            []EvidenceRef
	QuotaBudget         QuotaBudgetState
	Blocker             ActionState
	NextAction          ActionState
	GapReasons          []string
}

type EmitResult struct {
	WriteResult
	Emitted           bool
	DeliveryAttempted bool
	DeliveryErr       error
}

type Emitter struct {
	store  storage.Store
	config EmitterConfig

	mu                 sync.Mutex
	closed             bool
	latest             Observation
	haveLatest         bool
	lastObservationKey string
	lastDurableAt      time.Time
	nextSequence       int64
	sequenceLoadedFor  string
	terminalEmitted    bool
}

type Loop struct {
	emitter *Emitter
	ticker  TickSource
	stop    chan struct{}
	done    chan error
	once    sync.Once
}

type realTickSource struct {
	ticker *time.Ticker
}

func (t realTickSource) C() <-chan time.Time {
	return t.ticker.C
}

func (t realTickSource) Stop() {
	t.ticker.Stop()
}

func NewEmitter(store storage.Store, config EmitterConfig) (*Emitter, error) {
	if store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	normalized, err := normalizeEmitterConfig(config)
	if err != nil {
		return nil, err
	}
	return &Emitter{store: store, config: normalized}, nil
}

func normalizeEmitterConfig(config EmitterConfig) (EmitterConfig, error) {
	if config.MaxGenerationSilence == 0 {
		config.MaxGenerationSilence = DefaultMaxGenerationSilence
	}
	if config.MaxGenerationSilence < MinMaxGenerationSilence || config.MaxGenerationSilence > MaxMaxGenerationSilence {
		return EmitterConfig{}, fmt.Errorf("%w: max generation silence must be between %s and %s", ErrEmitterConfig, MinMaxGenerationSilence, MaxMaxGenerationSilence)
	}
	if config.NewTicker == nil {
		config.NewTicker = func(interval time.Duration) TickSource {
			return realTickSource{ticker: time.NewTicker(interval)}
		}
	}
	return config, nil
}

func (e *Emitter) Observe(observation Observation) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrEmitterClosed
	}
	e.latest = normalizeObservationDefaults(observation)
	e.haveLatest = true
	return nil
}

func (e *Emitter) Emit(ctx context.Context, observation Observation) (EmitResult, error) {
	return e.emit(ctx, observation, false)
}

func (e *Emitter) EmitTerminal(ctx context.Context, observation Observation) (EmitResult, error) {
	observation.Terminal = true
	if strings.TrimSpace(observation.Reason) == "" {
		observation.Reason = ReasonTerminal
	}
	return e.emit(ctx, observation, true)
}

func (e *Emitter) Start(ctx context.Context, initial Observation) (*Loop, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(initial.ProjectID) != "" || strings.TrimSpace(initial.DeliveryRunID) != "" {
		if _, err := e.Emit(ctx, initial); err != nil {
			return nil, err
		}
	}
	ticker := e.config.NewTicker(e.config.MaxGenerationSilence)
	loop := &Loop{
		emitter: e,
		ticker:  ticker,
		stop:    make(chan struct{}),
		done:    make(chan error, 1),
	}
	go loop.run(ctx)
	return loop, nil
}

func (l *Loop) Stop() error {
	var err error
	l.once.Do(func() {
		close(l.stop)
		err = <-l.done
	})
	return err
}

func (l *Loop) Terminal(ctx context.Context, observation Observation) (EmitResult, error) {
	result, err := l.emitter.EmitTerminal(ctx, observation)
	stopErr := l.Stop()
	if err != nil {
		return result, err
	}
	if stopErr != nil {
		return result, stopErr
	}
	return result, nil
}

func (l *Loop) run(ctx context.Context) {
	defer l.ticker.Stop()
	var lastErr error
	for {
		select {
		case <-l.stop:
			l.done <- lastErr
			return
		case <-ctx.Done():
			l.done <- ctx.Err()
			return
		case <-l.ticker.C():
			if _, err := l.emitter.emitPeriodic(ctx); err != nil {
				lastErr = err
			}
		}
	}
}

func (e *Emitter) emitPeriodic(ctx context.Context) (EmitResult, error) {
	e.mu.Lock()
	if !e.haveLatest || e.closed || e.terminalEmitted {
		e.mu.Unlock()
		return EmitResult{}, nil
	}
	observation := e.latest
	now := e.store.Now().UTC()
	if !e.lastDurableAt.IsZero() && now.Sub(e.lastDurableAt) < e.config.MaxGenerationSilence {
		e.mu.Unlock()
		return EmitResult{}, nil
	}
	observation.Reason = ReasonMaxGenerationSilence
	if !containsString(observation.GapReasons, ReasonMaxGenerationSilence) {
		observation.GapReasons = append(append([]string{}, observation.GapReasons...), ReasonMaxGenerationSilence)
	}
	e.mu.Unlock()
	return e.emit(ctx, observation, true)
}

func (e *Emitter) emit(ctx context.Context, observation Observation, force bool) (EmitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	observation = normalizeObservationDefaults(observation)
	key, err := observationKey(observation)
	if err != nil {
		return EmitResult{}, err
	}
	now := e.store.Now().UTC()

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return EmitResult{}, ErrEmitterClosed
	}
	if observation.Terminal && e.terminalEmitted {
		e.mu.Unlock()
		return EmitResult{}, nil
	}
	if !force && key == e.lastObservationKey && !e.lastDurableAt.IsZero() && now.Sub(e.lastDurableAt) < e.config.MaxGenerationSilence {
		e.latest = observation
		e.haveLatest = true
		e.mu.Unlock()
		return EmitResult{}, nil
	}
	correlationID := observationCorrelationID(observation)
	sequenceKey := observation.ProjectID + "\x00" + observation.DeliveryRunID + "\x00" + correlationID
	if e.sequenceLoadedFor != sequenceKey {
		next, err := nextCorrelationSequence(ctx, e.store, observation.ProjectID, observation.DeliveryRunID, correlationID)
		if err != nil {
			e.mu.Unlock()
			return EmitResult{}, err
		}
		e.nextSequence = next
		e.sequenceLoadedFor = sequenceKey
	}
	sequence := e.nextSequence
	e.nextSequence++
	receipt := receiptFromObservation(observation, correlationID, sequence, now)
	e.mu.Unlock()

	written, err := PersistReceipt(ctx, e.store, receipt)
	if err != nil {
		return EmitResult{}, err
	}

	result := EmitResult{WriteResult: written, Emitted: written.Inserted}
	e.mu.Lock()
	if written.Inserted {
		e.lastDurableAt = now
		e.lastObservationKey = key
	}
	e.latest = observation
	e.haveLatest = true
	if observation.Terminal && written.Inserted {
		e.terminalEmitted = true
	}
	e.mu.Unlock()

	if e.config.Deliver != nil {
		result.DeliveryAttempted = true
		result.DeliveryErr = e.config.Deliver(ctx, written.Receipt)
	}
	return result, nil
}

func observationCorrelationID(observation Observation) string {
	if strings.TrimSpace(observation.CorrelationID) != "" {
		return strings.TrimSpace(observation.CorrelationID)
	}
	if strings.TrimSpace(observation.AttemptID) != "" {
		return "supervisor-" + strings.TrimSpace(observation.AttemptID)
	}
	if strings.TrimSpace(observation.TaskID) != "" {
		return "supervisor-" + strings.TrimSpace(observation.TaskID)
	}
	return "supervisor-" + strings.TrimSpace(observation.DeliveryRunID)
}

func nextCorrelationSequence(ctx context.Context, store storage.Store, projectID, deliveryRunID, correlationID string) (int64, error) {
	var max int64
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(MAX(correlation_sequence), 0) FROM progress_receipts WHERE project_id = ? AND delivery_run_id = ? AND correlation_id = ?`,
			projectID, deliveryRunID, correlationID).Scan(&max)
	})
	if err != nil {
		return 0, fmt.Errorf("load progress receipt correlation sequence: %w", err)
	}
	return max + 1, nil
}

func receiptFromObservation(observation Observation, correlationID string, sequence int64, now time.Time) ProgressReceipt {
	observation = normalizeObservationDefaults(observation)
	heartbeat := AgeEvidence{State: firstNonEmpty(observation.HeartbeatState, Unknown)}
	if !observation.HeartbeatObservedAt.IsZero() {
		heartbeat.ObservedAt = delivery.CanonicalTimestamp(observation.HeartbeatObservedAt.UTC())
		heartbeat.AgeMillis = -1
	}
	progressAge := AgeEvidence{State: firstNonEmpty(observation.ProgressState, Unknown)}
	if !observation.ProgressObservedAt.IsZero() {
		progressAge.ObservedAt = delivery.CanonicalTimestamp(observation.ProgressObservedAt.UTC())
		progressAge.AgeMillis = -1
	}
	evidence := append([]EvidenceRef{}, observation.Evidence...)
	evidence = append(evidence, EvidenceRef{
		RecordKind:     "supervisor-state",
		RecordID:       firstNonEmpty(observation.KnownState, observation.Reason, Unknown),
		Summary:        supervisorSummary(observation),
		Classification: "supervisor-observation",
		Confidence:     "exact",
	})
	return ProgressReceipt{
		ProjectID:           observation.ProjectID,
		DeliveryRunID:       observation.DeliveryRunID,
		RunID:               firstNonEmpty(observation.RunID, observation.DeliveryRunID),
		TaskID:              firstNonEmpty(observation.TaskID, Unknown),
		AttemptID:           firstNonEmpty(observation.AttemptID, Unknown),
		AttemptOrdinal:      observation.AttemptOrdinal,
		CorrelationID:       correlationID,
		CorrelationSequence: sequence,
		Phase:               observation.Phase,
		Status:              observation.Status,
		TaskCounts:          observation.TaskCounts,
		Provider:            observation.Provider,
		Heartbeat:           heartbeat,
		Progress:            progressAge,
		Evidence:            evidence,
		QuotaBudget:         observation.QuotaBudget,
		Blocker:             observation.Blocker,
		NextAction:          observation.NextAction,
		GapReasons:          observation.GapReasons,
		OccurredAt:          delivery.CanonicalTimestamp(now),
	}
}

func normalizeObservationDefaults(observation Observation) Observation {
	observation.ProjectID = strings.TrimSpace(observation.ProjectID)
	observation.DeliveryRunID = strings.TrimSpace(observation.DeliveryRunID)
	observation.RunID = firstNonEmpty(observation.RunID, observation.DeliveryRunID)
	observation.CorrelationID = strings.TrimSpace(observation.CorrelationID)
	observation.Reason = sanitizeEnum(firstNonEmpty(observation.Reason, ReasonStateChange))
	observation.KnownState = sanitizeEnum(firstNonEmpty(observation.KnownState, KnownAliveNoMeaningfulProgress))
	if observation.Terminal {
		observation.KnownState = KnownTerminal
		if observation.Reason == "" || observation.Reason == ReasonStateChange {
			observation.Reason = ReasonTerminal
		}
	}
	if strings.TrimSpace(observation.Phase) == "" {
		observation.Phase = observation.KnownState
	}
	if strings.TrimSpace(observation.Status) == "" {
		observation.Status = statusForKnownState(observation.KnownState)
	}
	if strings.TrimSpace(observation.HeartbeatState) == "" {
		observation.HeartbeatState = Unknown
	}
	if strings.TrimSpace(observation.ProgressState) == "" {
		observation.ProgressState = progressStateForKnownState(observation.KnownState)
	}
	observation.GapReasons = append([]string{}, observation.GapReasons...)
	switch observation.KnownState {
	case KnownAliveNoMeaningfulProgress:
		observation.GapReasons = appendReason(observation.GapReasons, "no-meaningful-progress-observed")
	case KnownHostOffline:
		observation.GapReasons = appendReason(observation.GapReasons, KnownHostOffline)
	case KnownDeliveryPending:
		observation.GapReasons = appendReason(observation.GapReasons, KnownDeliveryPending)
	}
	observation.Blocker = defaultBlocker(observation.KnownState, observation.Blocker)
	observation.NextAction = defaultNextAction(observation.KnownState, observation.NextAction)
	observation.QuotaBudget = defaultQuotaBudget(observation.KnownState, observation.QuotaBudget)
	return observation
}

func statusForKnownState(state string) string {
	switch state {
	case KnownWaitingApproval, KnownWaitingCI, KnownDeliveryPending:
		return "waiting"
	case KnownQuotaBlocked, KnownBlocked, KnownHostOffline:
		return "blocked"
	case KnownCancellationInProgress:
		return "cancelling"
	case KnownTerminal:
		return "terminal"
	default:
		return "running"
	}
}

func progressStateForKnownState(state string) string {
	if state == KnownAliveNoMeaningfulProgress {
		return "stale"
	}
	return Unknown
}

func defaultBlocker(state string, current ActionState) ActionState {
	if strings.TrimSpace(current.State) != "" || strings.TrimSpace(current.Summary) != "" {
		return current
	}
	switch state {
	case KnownWaitingApproval:
		return ActionState{State: "waiting", Summary: "waiting for approval"}
	case KnownWaitingCI:
		return ActionState{State: "waiting", Summary: "waiting for CI"}
	case KnownQuotaBlocked:
		return ActionState{State: "quota-blocked", Summary: "quota exhausted or unavailable"}
	case KnownHostOffline:
		return ActionState{State: "host-offline", Summary: "host delivery is offline"}
	case KnownBlocked:
		return ActionState{State: "blocked", Summary: "supervisor reports blocked state"}
	default:
		return ActionState{State: "none"}
	}
}

func defaultNextAction(state string, current ActionState) ActionState {
	if strings.TrimSpace(current.State) != "" || strings.TrimSpace(current.Summary) != "" {
		return current
	}
	switch state {
	case KnownWaitingApproval:
		return ActionState{State: "wait", Summary: "wait for approval"}
	case KnownWaitingCI:
		return ActionState{State: "wait", Summary: "wait for CI result"}
	case KnownQuotaBlocked:
		return ActionState{State: "wait", Summary: "wait for quota recovery or policy change"}
	case KnownFallbackInProgress:
		return ActionState{State: "fallback-in-progress", Summary: "continue provider fallback"}
	case KnownHostOffline, KnownDeliveryPending:
		return ActionState{State: "delivery-pending", Summary: "persist receipt; delivery remains pending"}
	case KnownRecoveryInProgress:
		return ActionState{State: "recover", Summary: "continue supervisor recovery"}
	case KnownCancellationInProgress:
		return ActionState{State: "cancel", Summary: "continue cancellation"}
	case KnownTerminal:
		return ActionState{State: "none", Summary: "run is terminal"}
	default:
		return ActionState{State: "continue", Summary: "continue supervising"}
	}
}

func defaultQuotaBudget(state string, current QuotaBudgetState) QuotaBudgetState {
	if strings.TrimSpace(current.State) != "" || strings.TrimSpace(current.Confidence) != "" || current.RemainingQuantity != 0 {
		return current
	}
	if state == KnownQuotaBlocked {
		return QuotaBudgetState{State: "exhausted", Confidence: "exact", RemainingQuantity: 0, Unit: Unknown, GapReasons: []string{"quota-blocked"}}
	}
	return current
}

func supervisorSummary(observation Observation) string {
	switch observation.KnownState {
	case KnownAliveNoMeaningfulProgress:
		return "alive; no meaningful progress observed"
	case KnownWaitingCI:
		return "waiting for CI"
	case KnownWaitingApproval:
		return "waiting for approval"
	case KnownQuotaBlocked:
		return "quota blocked"
	case KnownFallbackInProgress:
		return "provider fallback in progress"
	case KnownHostOffline:
		return "host offline; receipt delivery not implied"
	case KnownDeliveryPending:
		return "delivery pending; acknowledgment not implied"
	case KnownRecoveryInProgress:
		return "recovery in progress"
	case KnownCancellationInProgress:
		return "cancellation in progress"
	case KnownTerminal:
		return "terminal state observed"
	default:
		return "supervisor state observed"
	}
}

func observationKey(observation Observation) (string, error) {
	canonical := map[string]any{
		"project_id":            observation.ProjectID,
		"delivery_run_id":       observation.DeliveryRunID,
		"run_id":                observation.RunID,
		"task_id":               observation.TaskID,
		"attempt_id":            observation.AttemptID,
		"attempt_ordinal":       observation.AttemptOrdinal,
		"correlation_id":        observation.CorrelationID,
		"phase":                 observation.Phase,
		"status":                observation.Status,
		"known_state":           observation.KnownState,
		"reason":                observation.Reason,
		"terminal":              observation.Terminal,
		"task_counts":           observation.TaskCounts,
		"provider":              observation.Provider,
		"heartbeat_observed_at": observation.HeartbeatObservedAt.UTC().Format(time.RFC3339Nano),
		"heartbeat_state":       observation.HeartbeatState,
		"progress_observed_at":  observation.ProgressObservedAt.UTC().Format(time.RFC3339Nano),
		"progress_state":        observation.ProgressState,
		"evidence":              observation.Evidence,
		"quota_budget":          observation.QuotaBudget,
		"blocker":               observation.Blocker,
		"next_action":           observation.NextAction,
		"gap_reasons":           observation.GapReasons,
	}
	digest, _, err := delivery.DigestCanonicalJSON(canonical)
	if err != nil {
		return "", typed(ErrInvalidRecordCode, "semantic progress observation: %v", err)
	}
	return digest, nil
}

func appendReason(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}
	return append(values, value)
}

func containsString(values []string, value string) bool {
	for _, current := range values {
		if current == value {
			return true
		}
	}
	return false
}
