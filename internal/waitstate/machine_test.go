package waitstate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestThirtyMinuteCIWaitUsesNoProviderAndEmitsPolicyReceipts(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)}
	receipts := 0
	report, err := Run(context.Background(), Options{
		Kind:   KindGitHubCI,
		WaitID: "pr-971",
		Clock:  clock,
		Policy: Policy{
			MinPollInterval: time.Minute,
			MaxPollInterval: time.Minute,
			ReceiptCadence:  5 * time.Minute,
			Timeout:         30 * time.Minute,
		},
		Probe: func(context.Context) (Observation, error) {
			return Observation{EventID: "checks-pending", State: StateWaiting, Code: "required-checks-pending"}, nil
		},
		Receipt: func(context.Context, Receipt) error {
			receipts++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.StopReason != StopTimeout || report.ProviderInvocations != 0 {
		t.Fatalf("report = %#v, want timeout and zero provider invocations", report)
	}
	if elapsed := clock.now.Sub(time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)); elapsed != 30*time.Minute {
		t.Fatalf("elapsed = %s, want 30m", elapsed)
	}
	if receipts != 7 || report.Receipts != receipts {
		t.Fatalf("receipts = callback:%d report:%d, want initial plus each five-minute boundary", receipts, report.Receipts)
	}
	if report.Polls < 25 || report.Polls > 31 {
		t.Fatalf("polls = %d, want bounded cadence", report.Polls)
	}
}

func TestApprovalAndQuotaWaitsUseNoProvider(t *testing.T) {
	for _, kind := range []Kind{KindApproval, KindQuotaReset} {
		t.Run(string(kind), func(t *testing.T) {
			clock := &fakeClock{now: time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)}
			calls := 0
			report, err := Run(context.Background(), Options{
				Kind: kind, WaitID: "wait-1", Clock: clock,
				Policy: Policy{MinPollInterval: time.Minute, MaxPollInterval: time.Minute, ReceiptCadence: 5 * time.Minute, Timeout: 2 * time.Minute},
				Probe: func(context.Context) (Observation, error) {
					calls++
					if calls == 2 {
						return Observation{EventID: "ready-1", State: StateReady, Code: "authority-available", Consequential: true}, nil
					}
					return Observation{EventID: "waiting-1", State: StateWaiting, Code: "authority-pending"}, nil
				},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if report.StopReason != StopTransition || report.WakeDecisions != 1 || report.ProviderInvocations != 0 {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestNamedWatchersCoverEveryWaitAuthority(t *testing.T) {
	watchers := []struct {
		kind Kind
		run  func(context.Context, Options) (Report, error)
	}{
		{KindGitHubCI, WatchGitHubCI},
		{KindApproval, WatchApproval},
		{KindQuotaReset, WatchQuotaReset},
		{KindDeliveryOutbox, WatchDeliveryOutbox},
		{KindDetachedWorker, WatchDetachedWorker},
	}
	for _, watcher := range watchers {
		t.Run(string(watcher.kind), func(t *testing.T) {
			clock := &fakeClock{now: time.Date(2026, 7, 16, 1, 30, 0, 0, time.UTC)}
			report, err := watcher.run(context.Background(), Options{
				WaitID: "authority-1", Clock: clock, Policy: fastPolicy(),
				Probe: func(context.Context) (Observation, error) {
					return Observation{EventID: "ready", State: StateReady, Code: "ready"}, nil
				},
			})
			if err != nil {
				t.Fatalf("watcher: %v", err)
			}
			if report.Kind != watcher.kind || report.ProviderInvocations != 0 {
				t.Fatalf("report = %#v", report)
			}
		})
	}
}

func TestDuplicateTransitionProducesAtMostOneWakeDecisionAcrossRestart(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)}
	wakes := 0
	first, err := Run(context.Background(), Options{
		Kind: KindDeliveryOutbox, WaitID: "outbox-1", Clock: clock,
		Policy: fastPolicy(),
		Probe: func(context.Context) (Observation, error) {
			return Observation{EventID: "obligation-42-ack", State: StateReady, Code: "delivery-acknowledged", Consequential: true}, nil
		},
		Wake: func(context.Context, WakeDecision) error {
			wakes++
			return nil
		},
		Checkpoint: func(context.Context, Snapshot) error { return nil },
	})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, err := Run(context.Background(), Options{
		Kind: KindDeliveryOutbox, WaitID: "outbox-1", Clock: clock,
		Policy:  fastPolicy(),
		Initial: first.Snapshot,
		Probe: func(context.Context) (Observation, error) {
			return Observation{EventID: "obligation-42-ack", State: StateReady, Code: "delivery-acknowledged", Consequential: true}, nil
		},
		Wake: func(context.Context, WakeDecision) error {
			wakes++
			return nil
		},
		Checkpoint: func(context.Context, Snapshot) error { return nil },
	})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if first.WakeDecisions != 1 || second.WakeDecisions != 0 || wakes != 1 {
		t.Fatalf("decisions first=%d second=%d wakes=%d, want 1/0/1", first.WakeDecisions, second.WakeDecisions, wakes)
	}
}

func TestWakeDecisionIsCheckpointedBeforeDelivery(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)}
	checkpointed := false
	delivered := false
	_, err := Run(context.Background(), Options{
		Kind: KindApproval, WaitID: "approval-checkpoint", Clock: clock, Policy: fastPolicy(),
		Probe: func(context.Context) (Observation, error) {
			return Observation{EventID: "approved", State: StateReady, Code: "approved", Consequential: true}, nil
		},
		Checkpoint: func(_ context.Context, snapshot Snapshot) error {
			if !delivered && snapshot.PendingWake != nil && snapshot.LastDecisionKey != "" {
				checkpointed = true
			}
			return nil
		},
		Wake: func(context.Context, WakeDecision) error {
			if !checkpointed {
				t.Fatal("wake delivered before durable decision checkpoint")
			}
			delivered = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !checkpointed || !delivered {
		t.Fatalf("checkpointed=%v delivered=%v, want both", checkpointed, delivered)
	}
}

func TestStatePacketIsBoundedAndCannotCarryRawLogsPromptsOrTranscripts(t *testing.T) {
	refs := make([]Reference, 0, 500)
	for i := 0; i < 500; i++ {
		refs = append(refs, Reference{
			Kind: "github-check",
			ID:   fmt.Sprintf("check-%04d-%s", i, strings.Repeat("x", 300)),
			URL:  fmt.Sprintf("https://example.invalid/check/%d?token=raw-secret-query", i),
		})
	}
	packet, err := BuildStatePacket(PacketInput{
		Kind: KindGitHubCI, WaitID: "pr-967", PreviousState: StateWaiting, CurrentState: StateReady,
		EventID: strings.Repeat("event", 1000), Code: strings.Repeat("code", 1000), References: refs,
		ObservedAt: time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildStatePacket: %v", err)
	}
	if len(packet) > MaxStatePacketBytes {
		t.Fatalf("packet bytes = %d, want <= %d", len(packet), MaxStatePacketBytes)
	}
	text := string(packet)
	for _, forbidden := range []string{"raw-secret-query", "token=", "raw_logs", "prompt", "transcript", strings.Repeat("event", 200)} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("packet contains forbidden raw material %q", forbidden)
		}
	}
}

func TestRestartUnavailableRateLimitAndHostDisconnectConvergeWithoutBusyLoop(t *testing.T) {
	start := time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC)
	clock := &fakeClock{now: start, cancelAfterSleeps: 2}
	probes := 0
	first, err := Run(context.Background(), Options{
		Kind: KindDetachedWorker, WaitID: "worker-1", Clock: clock,
		Policy: Policy{MinPollInterval: time.Second, MaxPollInterval: 4 * time.Second, ReceiptCadence: 5 * time.Minute, Timeout: time.Hour},
		Probe: func(context.Context) (Observation, error) {
			probes++
			if probes == 1 {
				return Observation{}, errors.New("github unavailable: raw response must not enter packet")
			}
			return Observation{EventID: "rate-limit", State: StateRateLimited, Code: "rate-limited", RetryAfter: 3 * time.Second}, nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run error = %v, want context canceled", err)
	}
	if first.Polls != 2 || len(clock.sleeps) != 2 {
		t.Fatalf("first report polls=%d sleeps=%v", first.Polls, clock.sleeps)
	}
	for _, delay := range clock.sleeps {
		if delay < time.Second {
			t.Fatalf("busy-loop delay = %s", delay)
		}
	}

	resumeClock := &fakeClock{now: clock.now}
	second, err := Run(context.Background(), Options{
		Kind: KindDetachedWorker, WaitID: "worker-1", Clock: resumeClock,
		Policy:  fastPolicy(),
		Initial: first.Snapshot,
		Probe: func(context.Context) (Observation, error) {
			return Observation{EventID: "worker-terminal", State: StateTerminal, Code: "worker-succeeded", Consequential: true, Terminal: true}, nil
		},
		// A disconnected or capability-incompatible host has no Wake callback.
	})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.StopReason != StopTransition || second.WakeDecisions != 1 || second.WakeDelivered != 0 {
		t.Fatalf("second report = %#v", second)
	}
	if second.Snapshot.PendingWake == nil {
		t.Fatal("host-disconnected wake decision was not preserved for read-only status/attach recovery")
	}
}

func TestPollingAndReceiptsDoNotRenewExternalAuthority(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)}
	claimGeneration := int64(7)
	budgetRemaining := int64(9000)
	watchdogDeadline := clock.now.Add(time.Hour)
	_, err := Run(context.Background(), Options{
		Kind: KindApproval, WaitID: "approval-1", Clock: clock,
		Policy: Policy{MinPollInterval: time.Minute, MaxPollInterval: time.Minute, ReceiptCadence: time.Minute, Timeout: 3 * time.Minute},
		Probe: func(context.Context) (Observation, error) {
			return Observation{EventID: "approval-pending", State: StateWaiting, Code: "approval-pending"}, nil
		},
		Receipt: func(context.Context, Receipt) error { return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if claimGeneration != 7 || budgetRemaining != 9000 || !watchdogDeadline.Equal(time.Date(2026, 7, 16, 6, 0, 0, 0, time.UTC)) {
		t.Fatalf("wait path mutated authority: claim=%d budget=%d watchdog=%s", claimGeneration, budgetRemaining, watchdogDeadline)
	}
}

func fastPolicy() Policy {
	return Policy{MinPollInterval: time.Second, MaxPollInterval: time.Second, ReceiptCadence: 5 * time.Minute, Timeout: time.Minute}
}

type fakeClock struct {
	now               time.Time
	sleeps            []time.Duration
	cancelAfterSleeps int
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, delay time.Duration) error {
	c.sleeps = append(c.sleeps, delay)
	c.now = c.now.Add(delay)
	if c.cancelAfterSleeps > 0 && len(c.sleeps) >= c.cancelAfterSleeps {
		return context.Canceled
	}
	return nil
}
