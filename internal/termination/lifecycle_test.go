package termination_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/termination"
)

type memEvents struct {
	mu   sync.Mutex
	list []termination.Transition
}

func (m *memEvents) Write(t termination.Transition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.list = append(m.list, t)
	return nil
}

type fakeCtrl struct {
	mu         sync.Mutex
	alive      bool
	generation int64
	pid        int
	termCount  int32
	killCount  int32
	// ignoreTerm keeps process alive through SIGTERM.
	ignoreTerm bool
	// signalGens records generations that were signalled.
	signalGens []int64
}

func (f *fakeCtrl) Signal(target termination.Target, kind termination.SignalKind) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if target.Generation != f.generation || target.RootPID != f.pid {
		return termination.ErrGenerationMismatch
	}
	f.signalGens = append(f.signalGens, target.Generation)
	switch kind {
	case termination.SignalTerm:
		atomic.AddInt32(&f.termCount, 1)
		if !f.ignoreTerm {
			f.alive = false
		}
	case termination.SignalKill:
		atomic.AddInt32(&f.killCount, 1)
		f.alive = false
	}
	return nil
}

func (f *fakeCtrl) Alive(target termination.Target) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if target.Generation != f.generation || target.RootPID != f.pid {
		return false, termination.ErrGenerationMismatch
	}
	return f.alive, nil
}

func (f *fakeCtrl) Wait(ctx context.Context, target termination.Target) error {
	for {
		f.mu.Lock()
		alive := f.alive
		genOK := target.Generation == f.generation
		f.mu.Unlock()
		if !genOK {
			return termination.ErrGenerationMismatch
		}
		if !alive {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type fakeTree struct {
	owned   int
	escaped int
	unknown bool
}

func (t fakeTree) Snapshot(termination.Target) (int, int, bool, error) {
	return t.owned, t.escaped, t.unknown, nil
}

type countFlush struct{ n int32 }

func (c *countFlush) Flush(context.Context, string) error {
	atomic.AddInt32(&c.n, 1)
	return nil
}

type countRelease struct{ n int32 }

func (c *countRelease) Release(_ context.Context, _ string, _ int64) error {
	atomic.AddInt32(&c.n, 1)
	return nil
}

func target(gen int64) termination.Target {
	return termination.Target{
		AttemptID:             "att-1",
		Generation:            gen,
		RootPID:               4242,
		ReservationID:         "res-1",
		ReservationGeneration: 1,
	}
}

func newLife(t *testing.T, ctrl *fakeCtrl, tree termination.TreeView, flush termination.OutputFlusher, rel termination.ReservationReleaser, ev termination.EventWriter) *termination.Lifecycle {
	t.Helper()
	lc, err := termination.New(termination.Options{
		Policy: termination.Policy{
			Grace:          30 * time.Millisecond,
			HardAfterGrace: 30 * time.Millisecond,
			CleanupBound:   2 * time.Second,
		},
		Ctrl:    ctrl,
		Tree:    tree,
		Flush:   flush,
		Release: rel,
		Events:  ev,
	})
	if err != nil {
		t.Fatal(err)
	}
	return lc
}

func TestCooperativeStopIdempotent(t *testing.T) {
	ctrl := &fakeCtrl{alive: true, generation: 1, pid: 4242}
	flush := &countFlush{}
	rel := &countRelease{}
	ev := &memEvents{}
	lc := newLife(t, ctrl, fakeTree{}, flush, rel, ev)

	ctx, cancel := context.WithCancel(context.Background())
	res, err := lc.Stop(ctx, target(1), termination.ReasonCancel)
	if err != nil || !res.TerminalClean {
		t.Fatalf("stop: %#v err=%v", res, err)
	}
	if atomic.LoadInt32(&ctrl.termCount) < 1 {
		t.Fatal("expected term")
	}
	if atomic.LoadInt32(&flush.n) != 1 || atomic.LoadInt32(&rel.n) != 1 {
		t.Fatalf("flush=%d release=%d", flush.n, rel.n)
	}

	// Idempotent repeat.
	res2, err := lc.Stop(ctx, target(1), termination.ReasonCancel)
	if err != nil || !res2.TerminalClean {
		t.Fatalf("repeat: %#v err=%v", res2, err)
	}
	if atomic.LoadInt32(&flush.n) != 1 || atomic.LoadInt32(&rel.n) != 1 {
		t.Fatal("repeat must not double flush/release")
	}
	// Wrong generation cannot signal after terminal.
	ctrl.generation = 2
	ctrl.alive = true
	_, err = lc.Stop(ctx, target(2), termination.ReasonCancel)
	// genSeen only stores terminal gen for attempt — gen 2 is different attempt path
	// Actually genSeen[att-1]=1, so gen 2 hits generation mismatch
	if !errors.Is(err, termination.ErrGenerationMismatch) {
		// Our markTerminal stores gen 1; stop gen 2 compares last!=generation → mismatch
		t.Fatalf("want gen mismatch, got %v", err)
	}
	cancel()
}

func TestUncooperativeEscalatesToKill(t *testing.T) {
	ctrl := &fakeCtrl{alive: true, generation: 1, pid: 4242, ignoreTerm: true}
	lc := newLife(t, ctrl, fakeTree{}, &countFlush{}, &countRelease{}, &memEvents{})
	res, err := lc.Stop(context.Background(), target(1), termination.ReasonTimeout)
	if err != nil || !res.TerminalClean {
		t.Fatalf("%#v err=%v", res, err)
	}
	if atomic.LoadInt32(&ctrl.termCount) < 1 || atomic.LoadInt32(&ctrl.killCount) < 1 {
		t.Fatalf("term=%d kill=%d", ctrl.termCount, ctrl.killCount)
	}
}

func TestAlreadyExitedStillFlushesAndReleases(t *testing.T) {
	ctrl := &fakeCtrl{alive: false, generation: 1, pid: 4242}
	flush := &countFlush{}
	rel := &countRelease{}
	lc := newLife(t, ctrl, fakeTree{}, flush, rel, &memEvents{})
	res, err := lc.Stop(context.Background(), target(1), termination.ReasonSuccess)
	if err != nil || !res.TerminalClean {
		t.Fatalf("%#v err=%v", res, err)
	}
	if atomic.LoadInt32(&ctrl.termCount) != 0 {
		t.Fatal("should not signal exited process")
	}
	if flush.n != 1 || rel.n != 1 {
		t.Fatal("flush/release required")
	}
}

func TestEscapedChildAttentionDoesNotRelease(t *testing.T) {
	ctrl := &fakeCtrl{alive: true, generation: 1, pid: 4242}
	rel := &countRelease{}
	lc := newLife(t, ctrl, fakeTree{escaped: 1}, &countFlush{}, rel, &memEvents{})
	res, err := lc.Stop(context.Background(), target(1), termination.ReasonCancel)
	if !errors.Is(err, termination.ErrAttentionRequired) || res.TerminalClean {
		t.Fatalf("%#v err=%v", res, err)
	}
	if res.Evidence.EscapedChildren != 1 {
		t.Fatalf("evidence %#v", res.Evidence)
	}
	if atomic.LoadInt32(&rel.n) != 0 {
		t.Fatal("must not release reservation on escape")
	}
}

func TestUnknownChildrenAttention(t *testing.T) {
	ctrl := &fakeCtrl{alive: true, generation: 1, pid: 4242}
	lc := newLife(t, ctrl, fakeTree{unknown: true}, &countFlush{}, &countRelease{}, &memEvents{})
	res, err := lc.Stop(context.Background(), target(1), termination.ReasonCancel)
	if !errors.Is(err, termination.ErrAttentionRequired) {
		t.Fatalf("%#v err=%v", res, err)
	}
}

func TestCallerCancelDoesNotSkipCleanup(t *testing.T) {
	ctrl := &fakeCtrl{alive: true, generation: 1, pid: 4242}
	flush := &countFlush{}
	rel := &countRelease{}
	lc := newLife(t, ctrl, fakeTree{}, flush, rel, &memEvents{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	res, err := lc.Stop(ctx, target(1), termination.ReasonCancel)
	if err != nil || !res.TerminalClean {
		t.Fatalf("cleanup must complete: %#v err=%v", res, err)
	}
	if flush.n != 1 || rel.n != 1 {
		t.Fatal("cleanup skipped")
	}
}

func TestForeignGenerationCannotBeSignalled(t *testing.T) {
	ctrl := &fakeCtrl{alive: true, generation: 5, pid: 4242}
	lc := newLife(t, ctrl, fakeTree{}, &countFlush{}, &countRelease{}, &memEvents{})
	_, err := lc.Stop(context.Background(), target(1), termination.ReasonCancel)
	if !errors.Is(err, termination.ErrGenerationMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestOwnedDescendantsBlockTerminalClean(t *testing.T) {
	ctrl := &fakeCtrl{alive: true, generation: 1, pid: 4242}
	rel := &countRelease{}
	lc := newLife(t, ctrl, fakeTree{owned: 2}, &countFlush{}, rel, &memEvents{})
	res, err := lc.Stop(context.Background(), target(1), termination.ReasonCancel)
	if !errors.Is(err, termination.ErrAttentionRequired) || res.TerminalClean {
		t.Fatalf("%#v err=%v", res, err)
	}
	if res.Evidence.AttentionReason != "owned_descendants_remain" {
		t.Fatalf("reason=%s", res.Evidence.AttentionReason)
	}
	if atomic.LoadInt32(&rel.n) != 0 {
		t.Fatal("must not release while owned descendants remain")
	}
}
