// Package waitstate provides deterministic, provider-free waiting for external
// orchestration state. It deliberately has no dependency on agent.Runner: a
// poll, progress receipt, timeout, or wake decision cannot invoke a model.
package waitstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	SnapshotSchema      = "loopcoder.wait_snapshot.v1"
	StatePacketSchema   = "loopcoder.wait_state_packet.v1"
	ReceiptSchema       = "loopcoder.wait_receipt.v1"
	MaxStatePacketBytes = 8 * 1024

	StopTransition = "consequential-transition"
	StopTimeout    = "explicit-timeout"
	StopCanceled   = "canceled"
)

type Kind string

const (
	KindGitHubCI       Kind = "github-ci"
	KindApproval       Kind = "approval"
	KindQuotaReset     Kind = "quota-reset"
	KindDeliveryOutbox Kind = "delivery-outbox"
	KindDetachedWorker Kind = "detached-worker"
)

type State string

const (
	StateWaiting     State = "waiting"
	StateReady       State = "ready"
	StateTerminal    State = "terminal"
	StateUnavailable State = "unavailable"
	StateRateLimited State = "rate-limited"
)

type Reference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	URL  string `json:"url,omitempty"`
}

// Observation is intentionally compact and excludes arbitrary message, log,
// prompt, and transcript fields. Code and references are bounded again before
// entering a state packet.
type Observation struct {
	EventID       string
	State         State
	Code          string
	References    []Reference
	RetryAfter    time.Duration
	Consequential bool
	Terminal      bool
}

type Probe func(context.Context) (Observation, error)
type ReceiptFunc func(context.Context, Receipt) error
type WakeFunc func(context.Context, WakeDecision) error

type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

type Policy struct {
	MinPollInterval time.Duration
	MaxPollInterval time.Duration
	ReceiptCadence  time.Duration
	Timeout         time.Duration
	JitterPercent   int
}

func DefaultPolicy() Policy {
	return Policy{
		MinPollInterval: 15 * time.Second,
		MaxPollInterval: 2 * time.Minute,
		ReceiptCadence:  5 * time.Minute,
		Timeout:         30 * time.Minute,
		JitterPercent:   10,
	}
}

type Options struct {
	Kind    Kind
	WaitID  string
	Policy  Policy
	Clock   Clock
	Probe   Probe
	Receipt ReceiptFunc
	Wake    WakeFunc
	Initial Snapshot
}

type Receipt struct {
	SchemaVersion string `json:"schema_version"`
	Kind          Kind   `json:"kind"`
	WaitID        string `json:"wait_id"`
	State         State  `json:"state"`
	Code          string `json:"code"`
	PollAttempt   int    `json:"poll_attempt"`
	OccurredAt    string `json:"occurred_at"`
}

type WakeDecision struct {
	DecisionKey string          `json:"decision_key"`
	Reason      string          `json:"reason"`
	OccurredAt  string          `json:"occurred_at"`
	Packet      json.RawMessage `json:"state_packet"`
}

type Snapshot struct {
	SchemaVersion            string        `json:"schema_version"`
	Kind                     Kind          `json:"kind"`
	WaitID                   string        `json:"wait_id"`
	StartedAt                string        `json:"started_at"`
	NextReceiptAt            string        `json:"next_receipt_at"`
	LastEventID              string        `json:"last_event_id,omitempty"`
	LastState                State         `json:"last_state,omitempty"`
	LastCode                 string        `json:"last_code,omitempty"`
	LastDecisionKey          string        `json:"last_decision_key,omitempty"`
	LastDeliveredDecisionKey string        `json:"last_delivered_decision_key,omitempty"`
	PendingWake              *WakeDecision `json:"pending_wake,omitempty"`
	PollAttempt              int           `json:"poll_attempt"`
}

type Report struct {
	Kind                Kind            `json:"kind"`
	WaitID              string          `json:"wait_id"`
	StopReason          string          `json:"stop_reason"`
	Polls               int             `json:"polls"`
	Receipts            int             `json:"receipts"`
	WakeDecisions       int             `json:"wake_decisions"`
	WakeDelivered       int             `json:"wake_delivered"`
	ProviderInvocations int             `json:"provider_invocations"`
	LastPacket          json.RawMessage `json:"last_packet,omitempty"`
	Snapshot            Snapshot        `json:"snapshot"`
}

type PacketInput struct {
	Kind          Kind
	WaitID        string
	PreviousState State
	CurrentState  State
	EventID       string
	Code          string
	References    []Reference
	ObservedAt    time.Time
}

type statePacket struct {
	SchemaVersion string      `json:"schema_version"`
	Kind          Kind        `json:"kind"`
	WaitID        string      `json:"wait_id"`
	PreviousState State       `json:"previous_state,omitempty"`
	CurrentState  State       `json:"current_state"`
	EventID       string      `json:"event_id"`
	Code          string      `json:"code"`
	ObservedAt    string      `json:"observed_at"`
	References    []Reference `json:"references,omitempty"`
}

func Run(ctx context.Context, opts Options) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts.WaitID = boundedToken(opts.WaitID, 160)
	if !validKind(opts.Kind) {
		return Report{}, fmt.Errorf("unsupported wait kind %q", opts.Kind)
	}
	if opts.WaitID == "" {
		return Report{}, errors.New("wait_id is required")
	}
	if opts.Probe == nil {
		return Report{}, errors.New("probe is required")
	}
	if opts.Clock == nil {
		opts.Clock = wallClock{}
	}
	policy, err := normalizePolicy(opts.Policy)
	if err != nil {
		return Report{}, err
	}

	now := opts.Clock.Now().UTC()
	snapshot, err := initializeSnapshot(opts.Kind, opts.WaitID, opts.Initial, now, policy)
	if err != nil {
		return Report{}, err
	}
	report := Report{Kind: opts.Kind, WaitID: opts.WaitID, Snapshot: snapshot}
	finish := func(reason string, runErr error) (Report, error) {
		report.StopReason = reason
		report.Snapshot = snapshot
		return report, runErr
	}

	if snapshot.PendingWake != nil && opts.Wake != nil && snapshot.PendingWake.DecisionKey != snapshot.LastDeliveredDecisionKey {
		if err := opts.Wake(ctx, *snapshot.PendingWake); err == nil {
			snapshot.LastDeliveredDecisionKey = snapshot.PendingWake.DecisionKey
			snapshot.PendingWake = nil
			report.WakeDelivered++
		}
	}

	startedAt, _ := time.Parse(time.RFC3339Nano, snapshot.StartedAt)
	deadline := startedAt.Add(policy.Timeout)
	for {
		if err := ctx.Err(); err != nil {
			return finish(StopCanceled, err)
		}
		now = opts.Clock.Now().UTC()
		if !now.Before(deadline) {
			if err := emitReceipt(ctx, opts, &snapshot, &report, now, true, policy); err != nil {
				return finish(StopTimeout, err)
			}
			obs := Observation{EventID: "timeout", State: StateTerminal, Code: "wait-timeout", Consequential: true, Terminal: true}
			if err := decideWake(ctx, opts, &snapshot, &report, obs, now, StopTimeout); err != nil {
				return finish(StopTimeout, err)
			}
			return finish(StopTimeout, nil)
		}
		if err := emitReceipt(ctx, opts, &snapshot, &report, now, false, policy); err != nil {
			return finish(StopCanceled, err)
		}

		previousState := snapshot.LastState
		obs, probeErr := opts.Probe(ctx)
		report.Polls++
		if probeErr != nil {
			obs = Observation{EventID: "probe-unavailable", State: StateUnavailable, Code: "probe-unavailable"}
		}
		obs = normalizeObservation(obs)
		transition := obs.EventID != snapshot.LastEventID || obs.State != snapshot.LastState || obs.Code != snapshot.LastCode
		if transition {
			snapshot.LastEventID = obs.EventID
			snapshot.LastState = obs.State
			snapshot.LastCode = obs.Code
			snapshot.PollAttempt = 0
		} else {
			snapshot.PollAttempt++
		}
		if transition && consequential(obs) {
			if err := decideWakeWithPrevious(ctx, opts, &snapshot, &report, obs, previousState, now, StopTransition); err != nil {
				return finish(StopTransition, err)
			}
		}
		if obs.Terminal || obs.State == StateReady || obs.State == StateTerminal {
			return finish(StopTransition, nil)
		}

		delay := nextDelay(policy, opts.WaitID, snapshot.PollAttempt, obs.RetryAfter)
		nextReceipt, _ := time.Parse(time.RFC3339Nano, snapshot.NextReceiptAt)
		if until := nextReceipt.Sub(now); until > 0 && until < delay {
			delay = until
		}
		if until := deadline.Sub(now); until > 0 && until < delay {
			delay = until
		}
		if delay <= 0 {
			delay = policy.MinPollInterval
		}
		if err := opts.Clock.Sleep(ctx, delay); err != nil {
			return finish(StopCanceled, err)
		}
	}
}

// The named entrypoints make the five supported wait authorities explicit
// while keeping their timing, receipt, restart, and wake semantics identical.
func WatchGitHubCI(ctx context.Context, opts Options) (Report, error) {
	return runKind(ctx, KindGitHubCI, opts)
}

func WatchApproval(ctx context.Context, opts Options) (Report, error) {
	return runKind(ctx, KindApproval, opts)
}

func WatchQuotaReset(ctx context.Context, opts Options) (Report, error) {
	return runKind(ctx, KindQuotaReset, opts)
}

func WatchDeliveryOutbox(ctx context.Context, opts Options) (Report, error) {
	return runKind(ctx, KindDeliveryOutbox, opts)
}

func WatchDetachedWorker(ctx context.Context, opts Options) (Report, error) {
	return runKind(ctx, KindDetachedWorker, opts)
}

func runKind(ctx context.Context, kind Kind, opts Options) (Report, error) {
	if opts.Kind != "" && opts.Kind != kind {
		return Report{}, fmt.Errorf("wait kind %q does not match watcher %q", opts.Kind, kind)
	}
	opts.Kind = kind
	return Run(ctx, opts)
}

func decideWake(ctx context.Context, opts Options, snapshot *Snapshot, report *Report, obs Observation, now time.Time, reason string) error {
	return decideWakeWithPrevious(ctx, opts, snapshot, report, obs, snapshot.LastState, now, reason)
}

func decideWakeWithPrevious(ctx context.Context, opts Options, snapshot *Snapshot, report *Report, obs Observation, previous State, now time.Time, reason string) error {
	key := decisionKey(opts.Kind, opts.WaitID, obs)
	if key == snapshot.LastDecisionKey {
		return nil
	}
	packet, err := BuildStatePacket(PacketInput{
		Kind: opts.Kind, WaitID: opts.WaitID, PreviousState: previous, CurrentState: obs.State,
		EventID: obs.EventID, Code: obs.Code, References: obs.References, ObservedAt: now,
	})
	if err != nil {
		return err
	}
	decision := WakeDecision{DecisionKey: key, Reason: reason, OccurredAt: timestamp(now), Packet: packet}
	// Persist the decision in the snapshot before attempting host delivery. A
	// disconnected host can recover it through status/attach without creating a
	// second orchestration decision.
	snapshot.LastDecisionKey = key
	snapshot.PendingWake = &decision
	report.WakeDecisions++
	report.LastPacket = packet
	if opts.Wake == nil {
		return nil
	}
	if err := opts.Wake(ctx, decision); err != nil {
		return nil
	}
	snapshot.LastDeliveredDecisionKey = key
	snapshot.PendingWake = nil
	report.WakeDelivered++
	return nil
}

func emitReceipt(ctx context.Context, opts Options, snapshot *Snapshot, report *Report, now time.Time, force bool, policy Policy) error {
	next, err := time.Parse(time.RFC3339Nano, snapshot.NextReceiptAt)
	if err != nil {
		return fmt.Errorf("invalid next receipt time: %w", err)
	}
	if !force && now.Before(next) {
		return nil
	}
	receipt := Receipt{
		SchemaVersion: ReceiptSchema,
		Kind:          opts.Kind, WaitID: opts.WaitID, State: snapshot.LastState,
		Code: snapshot.LastCode, PollAttempt: snapshot.PollAttempt, OccurredAt: timestamp(now),
	}
	if receipt.State == "" {
		receipt.State = StateWaiting
	}
	if receipt.Code == "" {
		receipt.Code = "watch-started"
	}
	if opts.Receipt != nil {
		if err := opts.Receipt(ctx, receipt); err != nil {
			return err
		}
	}
	report.Receipts++
	for !next.After(now) {
		next = next.Add(policy.ReceiptCadence)
	}
	snapshot.NextReceiptAt = timestamp(next)
	return nil
}

func BuildStatePacket(input PacketInput) ([]byte, error) {
	if !validKind(input.Kind) {
		return nil, fmt.Errorf("unsupported wait kind %q", input.Kind)
	}
	packet := statePacket{
		SchemaVersion: StatePacketSchema,
		Kind:          input.Kind,
		WaitID:        boundedToken(input.WaitID, 160),
		PreviousState: normalizeState(input.PreviousState),
		CurrentState:  normalizeState(input.CurrentState),
		EventID:       boundedToken(input.EventID, 160),
		Code:          boundedToken(input.Code, 120),
		ObservedAt:    timestamp(input.ObservedAt),
		References:    sanitizeReferences(input.References),
	}
	if packet.CurrentState == "" {
		packet.CurrentState = StateWaiting
	}
	for {
		data, err := json.Marshal(packet)
		if err != nil {
			return nil, fmt.Errorf("marshal state packet: %w", err)
		}
		if len(data) <= MaxStatePacketBytes {
			return data, nil
		}
		if len(packet.References) == 0 {
			return nil, fmt.Errorf("state packet exceeds %d bytes", MaxStatePacketBytes)
		}
		packet.References = packet.References[:len(packet.References)-1]
	}
}

func initializeSnapshot(kind Kind, waitID string, initial Snapshot, now time.Time, policy Policy) (Snapshot, error) {
	if initial.SchemaVersion != "" {
		if initial.SchemaVersion != SnapshotSchema || initial.Kind != kind || boundedToken(initial.WaitID, 160) != waitID {
			return Snapshot{}, errors.New("restart snapshot does not match wait identity")
		}
		if _, err := time.Parse(time.RFC3339Nano, initial.StartedAt); err != nil {
			return Snapshot{}, fmt.Errorf("invalid snapshot started_at: %w", err)
		}
		if _, err := time.Parse(time.RFC3339Nano, initial.NextReceiptAt); err != nil {
			return Snapshot{}, fmt.Errorf("invalid snapshot next_receipt_at: %w", err)
		}
		return initial, nil
	}
	return Snapshot{
		SchemaVersion: SnapshotSchema,
		Kind:          kind,
		WaitID:        waitID,
		StartedAt:     timestamp(now),
		NextReceiptAt: timestamp(now),
	}, nil
}

func normalizePolicy(policy Policy) (Policy, error) {
	defaults := DefaultPolicy()
	if policy == (Policy{}) {
		policy = defaults
	}
	if policy.MinPollInterval == 0 {
		policy.MinPollInterval = defaults.MinPollInterval
	}
	if policy.MaxPollInterval == 0 {
		policy.MaxPollInterval = defaults.MaxPollInterval
	}
	if policy.ReceiptCadence == 0 {
		policy.ReceiptCadence = defaults.ReceiptCadence
	}
	if policy.Timeout == 0 {
		policy.Timeout = defaults.Timeout
	}
	if policy.MinPollInterval <= 0 || policy.MaxPollInterval < policy.MinPollInterval {
		return Policy{}, errors.New("poll intervals must be positive and ordered")
	}
	if policy.ReceiptCadence < policy.MinPollInterval || policy.ReceiptCadence > time.Hour {
		return Policy{}, errors.New("receipt cadence must be between min poll interval and one hour")
	}
	if policy.Timeout < policy.MinPollInterval || policy.Timeout > 7*24*time.Hour {
		return Policy{}, errors.New("timeout must be bounded between min poll interval and seven days")
	}
	if policy.JitterPercent < 0 || policy.JitterPercent > 50 {
		return Policy{}, errors.New("jitter percent must be between zero and fifty")
	}
	return policy, nil
}

func nextDelay(policy Policy, waitID string, attempt int, retryAfter time.Duration) time.Duration {
	shift := attempt
	if shift > 10 {
		shift = 10
	}
	delay := policy.MinPollInterval * time.Duration(1<<shift)
	if delay > policy.MaxPollInterval || delay < 0 {
		delay = policy.MaxPollInterval
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > policy.MaxPollInterval {
		delay = policy.MaxPollInterval
	}
	if policy.JitterPercent == 0 || delay == policy.MaxPollInterval && policy.MinPollInterval == policy.MaxPollInterval {
		return delay
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(fmt.Sprintf("%s:%d", waitID, attempt)))
	span := 2*policy.JitterPercent + 1
	percent := int(h.Sum32()%uint32(span)) - policy.JitterPercent
	jittered := delay + time.Duration(int64(delay)*int64(percent)/100)
	if jittered < policy.MinPollInterval {
		return policy.MinPollInterval
	}
	if jittered > policy.MaxPollInterval {
		return policy.MaxPollInterval
	}
	return jittered
}

func normalizeObservation(obs Observation) Observation {
	obs.EventID = boundedToken(obs.EventID, 160)
	obs.State = normalizeState(obs.State)
	obs.Code = boundedToken(obs.Code, 120)
	obs.References = sanitizeReferences(obs.References)
	if obs.EventID == "" {
		obs.EventID = "observation"
	}
	if obs.State == "" {
		obs.State = StateWaiting
	}
	if obs.Code == "" {
		obs.Code = "state-observed"
	}
	return obs
}

func consequential(obs Observation) bool {
	return obs.Consequential || obs.Terminal || obs.State == StateReady || obs.State == StateTerminal
}

func decisionKey(kind Kind, waitID string, obs Observation) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{string(kind), waitID, obs.EventID, string(obs.State), obs.Code}, "\x00")))
	return "wake_" + hex.EncodeToString(sum[:16])
}

func sanitizeReferences(refs []Reference) []Reference {
	if len(refs) > 64 {
		refs = refs[:64]
	}
	out := make([]Reference, 0, len(refs))
	for _, ref := range refs {
		ref.Kind = boundedToken(ref.Kind, 64)
		ref.ID = boundedToken(ref.ID, 160)
		ref.URL = sanitizeURL(ref.URL)
		if ref.Kind == "" || ref.ID == "" {
			continue
		}
		out = append(out, ref)
	}
	return out
}

func sanitizeURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	value = parsed.String()
	if len(value) > 500 {
		value = value[:500]
	}
	return value
}

var unsafeToken = regexp.MustCompile(`[^A-Za-z0-9._:/@+-]+`)

func boundedToken(value string, limit int) string {
	value = unsafeToken.ReplaceAllString(strings.TrimSpace(value), "-")
	value = strings.Trim(value, "-")
	if len(value) > limit {
		value = value[:limit]
	}
	return value
}

func normalizeState(state State) State {
	switch state {
	case StateWaiting, StateReady, StateTerminal, StateUnavailable, StateRateLimited:
		return state
	default:
		return ""
	}
}

func validKind(kind Kind) bool {
	switch kind {
	case KindGitHubCI, KindApproval, KindQuotaReset, KindDeliveryOutbox, KindDetachedWorker:
		return true
	default:
		return false
	}
}

func timestamp(value time.Time) string {
	if value.IsZero() {
		value = time.Now().UTC()
	}
	return value.UTC().Format(time.RFC3339Nano)
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

func (wallClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
