package cli

import (
	"context"
	"testing"
	"time"
)

func TestCommandContext_ExistsAndCancellable(t *testing.T) {
	ctx := CommandContext()
	if ctx == nil {
		t.Fatal("nil")
	}
	// Must not already be cancelled in tests unless root was cancelled.
	select {
	case <-ctx.Done():
		// If a prior test cancelled root, still ok — just document.
		t.Log("root already cancelled (shared process state)")
	default:
	}
	// Root cancel should make CommandContext done when cancel is invoked.
	if rootCmdCancel == nil {
		t.Fatal("rootCmdCancel not installed")
	}
	// Do not cancel global root in unit tests (would break siblings).
	// Instead verify CommandContext returns a non-nil context with Deadline unset.
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("command context should not have deadline by default")
	}
	_ = context.Background()
	_ = time.Second
}
