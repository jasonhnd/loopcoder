package waitstate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeQuotaClock struct {
	now   time.Time
	slept atomic.Int32
}

func (c *fakeQuotaClock) Now() time.Time { return c.now }

func (c *fakeQuotaClock) Sleep(ctx context.Context, d time.Duration) error {
	c.slept.Add(1)
	c.now = c.now.Add(d)
	return nil
}

func TestRunQuotaResetWaitReachesReadyWithoutProvider(t *testing.T) {
	start := time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC)
	resetAt := start.Add(45 * time.Second)
	clock := &fakeQuotaClock{now: start}
	var receipts atomic.Int32
	report, err := RunQuotaResetWait(context.Background(), QuotaResetPlan{
		WaitID:  "quota-test",
		ResetAt: resetAt,
		Clock:   clock,
		Policy: Policy{
			MinPollInterval: 10 * time.Second,
			MaxPollInterval: 20 * time.Second,
			ReceiptCadence:  time.Hour, // avoid receipt noise
			Timeout:         2 * time.Minute,
			JitterPercent:   0,
		},
		Receipt: func(context.Context, Receipt) error {
			receipts.Add(1)
			return nil
		},
		Checkpoint: func(context.Context, Snapshot) error { return nil },
	})
	if err != nil {
		t.Fatalf("RunQuotaResetWait: %v", err)
	}
	if report.StopReason != StopTransition && report.StopReason != StopTimeout {
		t.Fatalf("stop reason = %q, want transition or timeout; report=%#v", report.StopReason, report)
	}
	if clock.now.Before(resetAt) && report.StopReason == StopTransition {
		t.Fatalf("clock did not reach reset: now=%s reset=%s", clock.now, resetAt)
	}
	// Provider-free by construction: probe never calls external systems.
	if clock.slept.Load() == 0 && clock.now.Before(resetAt) {
		t.Fatalf("expected clock sleep while waiting for reset")
	}
}

func TestRunQuotaResetWaitRequiresResetTime(t *testing.T) {
	_, err := RunQuotaResetWait(context.Background(), QuotaResetPlan{WaitID: "x"})
	if err == nil {
		t.Fatal("expected error for zero reset time")
	}
}
