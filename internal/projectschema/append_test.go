package projectschema_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/projectschema"
)

func appendNow() time.Time {
	return time.Date(2026, 7, 21, 19, 0, 0, 0, time.UTC)
}

func TestAppendMonotonicIdempotentAndConflict(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "home"), "proj_ap")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := layout.OpenProject(ctx, "proj_ap", appendNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	if err := projectschema.Ensure(ctx, ps); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.SeedProject(ctx, ps, "proj_ap", "o/r", appendNow()); err != nil {
		t.Fatal(err)
	}

	req := projectschema.AppendRequest{
		ProjectID: "proj_ap", AggregateKind: "job", AggregateID: "job-1",
		Kind: "job.queued", IdempotencyKey: "job-1:queue",
		PayloadJSON: `{"n":1}`, CheckpointName: "events", RecordedAt: appendNow(),
	}
	r1, err := projectschema.Append(ctx, ps, req)
	if err != nil {
		t.Fatalf("append1: %v", err)
	}
	if r1.Sequence != 1 || r1.Reused {
		t.Fatalf("r1 = %#v", r1)
	}
	r2, err := projectschema.Append(ctx, ps, req)
	if err != nil {
		t.Fatalf("append2: %v", err)
	}
	if !r2.Reused || r2.Sequence != 1 || r2.EventID != r1.EventID {
		t.Fatalf("idempotent reuse failed: %#v", r2)
	}
	// Conflict: same key, different payload
	req.PayloadJSON = `{"n":2}`
	_, err = projectschema.Append(ctx, ps, req)
	if err == nil || !isConflict(err) {
		t.Fatalf("error = %v, want conflict", err)
	}
	// New key gets sequence 2
	req.IdempotencyKey = "job-1:start"
	req.Kind = "job.running"
	req.PayloadJSON = `{"n":1}`
	r3, err := projectschema.Append(ctx, ps, req)
	if err != nil {
		t.Fatal(err)
	}
	if r3.Sequence != 2 {
		t.Fatalf("sequence = %d", r3.Sequence)
	}
}

func TestConcurrentAppendsGapFree(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "home"), "proj_conc")
	if err != nil {
		t.Fatal(err)
	}
	// Two handles on same file path — open/close carefully with serial commits
	// via concurrent Append on one store (SQLite serializes writers).
	ps, err := layout.OpenProject(ctx, "proj_conc", appendNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	if err := projectschema.Ensure(ctx, ps); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.SeedProject(ctx, ps, "proj_conc", "o/r", appendNow()); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	seqs := make(chan int64, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := projectschema.Append(ctx, ps, projectschema.AppendRequest{
				ProjectID: "proj_conc", AggregateKind: "work_item", AggregateID: "w1",
				Kind: "tick", IdempotencyKey: "tick-" + itoa(i),
				PayloadJSON: `{}`, RecordedAt: appendNow().Add(time.Duration(i) * time.Millisecond),
			})
			if err != nil {
				errs <- err
				return
			}
			seqs <- r.Sequence
		}(i)
	}
	wg.Wait()
	close(seqs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int64]bool{}
	for s := range seqs {
		if seen[s] {
			t.Fatalf("duplicate sequence %d", s)
		}
		seen[s] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d sequences, want %d", len(seen), n)
	}
	for i := int64(1); i <= n; i++ {
		if !seen[i] {
			t.Fatalf("missing sequence %d", i)
		}
	}
}

func isConflict(err error) bool {
	return err != nil && (err == projectschema.ErrIdempotencyConflict ||
		(err.Error() != "" && (contains(err.Error(), "conflict") || contains(err.Error(), "idempotency"))))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
