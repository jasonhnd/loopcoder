package acceptharness

import (
	"sync"
	"time"
)

// Clock is an injectable time source. Tests must not sleep on wall clock for
// correctness.
type Clock interface {
	Now() time.Time
	Advance(d time.Duration)
}

// ManualClock is a deterministic clock starting at a fixed instant.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock returns a clock at the given time (UTC normalized).
func NewManualClock(start time.Time) *ManualClock {
	if start.IsZero() {
		start = time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	}
	return &ManualClock{now: start.UTC()}
}

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
