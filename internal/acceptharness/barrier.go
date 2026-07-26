package acceptharness

import (
	"context"
	"sync"
)

// Barrier is an explicit synchronization point. Waiters block until Release or
// context cancellation — never on wall-clock timers for correctness.
type Barrier struct {
	once sync.Once
	ch   chan struct{}
}

// NewBarrier returns a closed-when-released barrier.
func NewBarrier() *Barrier {
	return &Barrier{ch: make(chan struct{})}
}

// Release unblocks all waiters. Safe to call multiple times.
func (b *Barrier) Release() {
	b.once.Do(func() { close(b.ch) })
}

// Wait blocks until Release or ctx is done.
func (b *Barrier) Wait(ctx context.Context) error {
	select {
	case <-b.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Done returns the underlying channel for select statements.
func (b *Barrier) Done() <-chan struct{} {
	return b.ch
}
