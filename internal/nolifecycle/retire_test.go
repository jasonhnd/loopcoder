package nolifecycle_test

import (
	"testing"

	"github.com/jasonhnd/loopcoder/internal/nolifecycle"
)

func TestDenyLifecycleWrites(t *testing.T) {
	for _, kind := range []nolifecycle.WriterKind{
		nolifecycle.WriterProgress, nolifecycle.WriterRelay, nolifecycle.WriterOutbox,
	} {
		for _, act := range []nolifecycle.Action{
			nolifecycle.ActionCreate, nolifecycle.ActionFlush, nolifecycle.ActionClose, nolifecycle.ActionWrite,
		} {
			d := nolifecycle.Evaluate(kind, act, false)
			if d.Allowed {
				t.Fatalf("%s %s allowed", kind, act)
			}
		}
	}
}

func TestAllowPureProjection(t *testing.T) {
	d := nolifecycle.Evaluate(nolifecycle.WriterReporter, nolifecycle.ActionProject, true)
	if !d.Allowed {
		t.Fatal(d)
	}
	d2 := nolifecycle.Evaluate(nolifecycle.WriterProgress, nolifecycle.ActionProject, true)
	if d2.Allowed {
		t.Fatal("progress is not pure projection")
	}
	d3 := nolifecycle.Evaluate(nolifecycle.WriterReporter, nolifecycle.ActionProject, false)
	if d3.Allowed {
		t.Fatal("must require events")
	}
}

func TestCompatCommandsDenied(t *testing.T) {
	for _, c := range []string{"progress", "relay-flush", "outbox-close"} {
		if !nolifecycle.CompatCommandDenied(c) {
			t.Fatalf("%s should deny", c)
		}
	}
	if nolifecycle.CompatCommandDenied("status") {
		t.Fatal("status ok")
	}
}

func TestProjectFromEvents(t *testing.T) {
	r, err := nolifecycle.ProjectFromEvents([]string{"e2", "e1"}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if r.Schema != nolifecycle.UISchema {
		t.Fatal(r.Schema)
	}
	if r.Events[0] != "e1" || r.Events[1] != "e2" {
		t.Fatalf("%v", r.Events)
	}
	if _, err := nolifecycle.ProjectFromEvents(nil, "x"); err == nil {
		t.Fatal("empty events")
	}
}

func TestInventory(t *testing.T) {
	inv := nolifecycle.DefaultInventory()
	if len(inv) < 5 {
		t.Fatal(len(inv))
	}
	foundRemoved := false
	foundProj := false
	for _, e := range inv {
		if e.Disposition == "removed_writer" {
			foundRemoved = true
		}
		if e.Disposition == "pure_projection" {
			foundProj = true
		}
	}
	if !foundRemoved || !foundProj {
		t.Fatalf("%#v", inv)
	}
}
