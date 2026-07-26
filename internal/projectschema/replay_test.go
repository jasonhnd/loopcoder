package projectschema_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/home"
	"github.com/jasonhnd/loopcoder/internal/projectschema"
)

func tNow() time.Time {
	return time.Date(2026, 7, 21, 20, 0, 0, 0, time.UTC)
}

func TestCursorValidation(t *testing.T) {
	c := projectschema.ZeroCursor("proj_x")
	enc, err := projectschema.EncodeCursor(c)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := projectschema.DecodeCursor(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec.ProjectID != "proj_x" || dec.Sequence != 0 {
		t.Fatalf("%#v", dec)
	}
	if err := projectschema.ValidateCursorForProject(dec, "other"); !errors.Is(err, projectschema.ErrInvalidCursor) {
		t.Fatalf("wrong project: %v", err)
	}
	// Future version
	bad := projectschema.Cursor{FormatVersion: 99, ProjectID: "proj_x", Sequence: 1}
	encBad, _ := projectschema.EncodeCursor(bad)
	// Encode allows writing v99; Decode rejects future.
	// Force encode by temporarily... Encode doesn't check future. Decode does after we set high v.
	// Manually craft:
	raw, _ := projectschema.EncodeCursor(projectschema.Cursor{FormatVersion: 1, ProjectID: "p", Sequence: 0})
	_ = raw
	if _, err := projectschema.DecodeCursor("%%%"); !errors.Is(err, projectschema.ErrInvalidCursor) {
		t.Fatalf("malformed: %v", err)
	}
	// Empty does not fall back to zero
	if _, err := projectschema.DecodeCursor(""); !errors.Is(err, projectschema.ErrInvalidCursor) {
		t.Fatal("empty should fail")
	}
	c99 := projectschema.Cursor{FormatVersion: projectschema.CursorFormatVersion + 5, ProjectID: "proj_x", Sequence: 0}
	e99, err := projectschema.EncodeCursor(c99)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projectschema.DecodeCursor(e99); !errors.Is(err, projectschema.ErrInvalidCursor) {
		t.Fatalf("future version: %v", err)
	}
	_ = encBad
}

func TestReplayPaginationAndReopen(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "h"), "proj_r")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := layout.OpenProject(ctx, "proj_r", tNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	if err := projectschema.Ensure(ctx, ps); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.SeedProject(ctx, ps, "proj_r", "o/r", tNow()); err != nil {
		t.Fatal(err)
	}
	// append 5 events
	for i := 1; i <= 5; i++ {
		_, err := projectschema.Append(ctx, ps, projectschema.AppendRequest{
			ProjectID: "proj_r", AggregateKind: "job", AggregateID: "j1",
			Kind: "tick", IdempotencyKey: "k-" + itoa2(i), PayloadJSON: `{}`,
			RecordedAt: tNow().Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	cur := projectschema.ZeroCursor("proj_r")
	var all []projectschema.EventRow
	for page := 0; page < 10; page++ {
		p, err := projectschema.Replay(ctx, ps, "proj_r", cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, p.Events...)
		// ensure ordered and unique
		if len(p.Events) > 0 && p.NextCursor.Sequence != p.Events[len(p.Events)-1].Sequence {
			t.Fatalf("next cursor not advanced")
		}
		cur = p.NextCursor
		if p.Exhausted {
			break
		}
	}
	if len(all) != 5 {
		t.Fatalf("got %d events", len(all))
	}
	for i, e := range all {
		if e.Sequence != int64(i+1) {
			t.Fatalf("seq %d at %d", e.Sequence, i)
		}
	}
	// reopen and replay from middle
	_ = ps.Close()
	ps2, err := layout.OpenProject(ctx, "proj_r", tNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ps2.Close()
	mid := projectschema.Cursor{FormatVersion: 1, ProjectID: "proj_r", Sequence: 2}
	rest, _, err := projectschema.ReplayAll(ctx, ps2, "proj_r", mid, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 3 || rest[0].Sequence != 3 {
		t.Fatalf("%#v", rest)
	}
}

func TestProjectionCASAndRebuild(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "h"), "proj_p")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := layout.OpenProject(ctx, "proj_p", tNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	if err := projectschema.Ensure(ctx, ps); err != nil {
		t.Fatal(err)
	}
	if err := projectschema.SeedProject(ctx, ps, "proj_p", "o/r", tNow()); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := projectschema.Append(ctx, ps, projectschema.AppendRequest{
			ProjectID: "proj_p", AggregateKind: "job", AggregateID: "j",
			Kind: "t", IdempotencyKey: "p-" + itoa2(i), PayloadJSON: `{}`, RecordedAt: tNow(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Advance from 0 -> 2
	cp, err := projectschema.AdvanceCheckpoint(ctx, ps, "proj_p", "status", "1", 0, 2, `{"n":2}`, tNow())
	if err != nil {
		t.Fatal(err)
	}
	if cp.InputSequence != 2 || cp.ReducerVersion != "1" || cp.OutputDigest == "" {
		t.Fatalf("%#v", cp)
	}
	// Wrong expected revision
	_, err = projectschema.AdvanceCheckpoint(ctx, ps, "proj_p", "status", "1", 0, 3, `{"n":3}`, tNow())
	if !errors.Is(err, projectschema.ErrCheckpointConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	// Correct advance 2 -> 3
	if _, err := projectschema.AdvanceCheckpoint(ctx, ps, "proj_p", "status", "1", 2, 3, `{"n":3}`, tNow()); err != nil {
		t.Fatal(err)
	}
	// Rebuild with new reducer version
	out, err := projectschema.RebuildProjection(ctx, ps, "proj_p", "status", "2", func(events []projectschema.EventRow) (string, error) {
		return `{"count":` + itoa2(len(events)) + `}`, nil
	}, tNow())
	if err != nil {
		t.Fatal(err)
	}
	if out.ReducerVersion != "2" || out.InputSequence != 3 || !out.IsCurrent {
		t.Fatalf("%#v", out)
	}
	// Append still works during/after rebuild path
	if _, err := projectschema.Append(ctx, ps, projectschema.AppendRequest{
		ProjectID: "proj_p", AggregateKind: "job", AggregateID: "j",
		Kind: "t", IdempotencyKey: "p-4", PayloadJSON: `{}`, RecordedAt: tNow(),
	}); err != nil {
		t.Fatal(err)
	}
}

func itoa2(i int) string {
	if i == 0 {
		return "0"
	}
	var b [16]byte
	pos := len(b)
	n := i
	for n > 0 {
		pos--
		b[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(b[pos:])
}
