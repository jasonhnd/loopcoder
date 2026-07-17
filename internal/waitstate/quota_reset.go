package waitstate

import (
	"context"
	"fmt"
	"time"
)

// QuotaResetPlan is a provider-free local wait for a known quota reset.
// It never invokes a provider; the probe only consults the wall clock.
type QuotaResetPlan struct {
	WaitID     string
	ResetAt    time.Time
	Policy     Policy
	Clock      Clock
	Receipt    ReceiptFunc
	Wake       WakeFunc
	Checkpoint CheckpointFunc
}

// RunQuotaResetWait waits until ResetAt (or timeout/cancel) using the shared
// waitstate machine. Provider invocation count is definitionally zero.
func RunQuotaResetWait(ctx context.Context, plan QuotaResetPlan) (Report, error) {
	if plan.ResetAt.IsZero() {
		return Report{}, fmt.Errorf("quota reset time is required")
	}
	if plan.WaitID == "" {
		plan.WaitID = "quota-reset-" + plan.ResetAt.UTC().Format("20060102T150405Z")
	}
	policy := plan.Policy
	if policy == (Policy{}) {
		policy = DefaultPolicy()
	}
	// Bound the wait to the reset horizon plus a small grace if no timeout set.
	if policy.Timeout <= 0 {
		if plan.Clock == nil {
			policy.Timeout = time.Until(plan.ResetAt) + 5*time.Minute
			if policy.Timeout < 5*time.Minute {
				policy.Timeout = 5 * time.Minute
			}
		} else {
			left := plan.ResetAt.Sub(plan.Clock.Now())
			if left < 0 {
				left = 0
			}
			policy.Timeout = left + 5*time.Minute
		}
	}
	clock := plan.Clock
	if clock == nil {
		clock = systemClock{}
	}
	resetAt := plan.ResetAt.UTC()
	return WatchQuotaReset(ctx, Options{
		Kind:   KindQuotaReset,
		WaitID: plan.WaitID,
		Policy: policy,
		Clock:  clock,
		Probe: func(context.Context) (Observation, error) {
			now := clock.Now().UTC()
			if !now.Before(resetAt) {
				return Observation{
					EventID:       "quota-reset-reached:" + resetAt.Format(time.RFC3339Nano),
					State:         StateReady,
					Code:          "quota-reset-reached",
					Consequential: true,
					References: []Reference{{
						Kind: "quota-reset-at",
						ID:   resetAt.Format(time.RFC3339Nano),
					}},
				}, nil
			}
			return Observation{
				EventID:    "quota-reset-pending:" + now.Format(time.RFC3339Nano),
				State:      StateWaiting,
				Code:       "quota-reset-pending",
				RetryAfter: resetAt.Sub(now),
				References: []Reference{{
					Kind: "quota-reset-at",
					ID:   resetAt.Format(time.RFC3339Nano),
				}},
			}, nil
		},
		Receipt:    plan.Receipt,
		Wake:       plan.Wake,
		Checkpoint: plan.Checkpoint,
	})
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
