package compatshim_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/compatshim"
)

func fixed() time.Time {
	return time.Date(2026, 7, 22, 20, 0, 0, 0, time.UTC)
}

func TestClassifyRemoved(t *testing.T) {
	for _, name := range []string{"compile", "dispatch", "tick"} {
		s, ok := compatshim.Classify(name)
		if !ok || s.Class != compatshim.ClassRemoved {
			t.Fatalf("%s: %#v", name, s)
		}
		d := compatshim.NewRegistry(fixed).Evaluate("p1", name)
		if d.Allowed {
			t.Fatalf("%s allowed", name)
		}
	}
}

func TestReadOnlyCompatPrefixedAndExcluded(t *testing.T) {
	r := compatshim.NewRegistry(fixed)
	d := r.Evaluate("p1", "status")
	if !d.Allowed || d.CompatPrefix == "" || !d.ExcludeFromV09Gates {
		t.Fatalf("%#v", d)
	}
	out := compatshim.FormatCompat("ok")
	if !strings.HasPrefix(out, compatshim.CompatOutputPrefix) {
		t.Fatalf("%q", out)
	}
	if compatshim.IncludeInV09Status("status") {
		t.Fatal("compat must not feed v0.9 gates")
	}
}

func TestV09RoutedExclusively(t *testing.T) {
	d := compatshim.NewRegistry(fixed).Evaluate("p1", "doctor")
	if !d.Allowed || d.Class != compatshim.ClassV09 {
		t.Fatalf("%#v", d)
	}
	if !compatshim.IncludeInV09Status("doctor") {
		t.Fatal("v09 should include in status")
	}
}

func TestWriterIsolationAfterV09(t *testing.T) {
	r := compatshim.NewRegistry(fixed)
	if err := r.BeginWrite("p1", "import-v09", compatshim.GenV09); err != nil {
		t.Fatal(err)
	}
	if r.Authority("p1").Writer != compatshim.GenV09 {
		t.Fatal(r.Authority("p1"))
	}
	// Any attempt at legacy mutation after v0.9 write fails.
	err := r.BeginWrite("p1", "write-progress", compatshim.GenV08)
	if err == nil {
		t.Fatal("legacy mutation must be refused")
	}
	// v0.9 may continue writing.
	if err := r.BeginWrite("p1", "import-v09", compatshim.GenV09); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMutationBeforeV09StillBlockedForRemoved(t *testing.T) {
	r := compatshim.NewRegistry(fixed)
	if err := r.BeginWrite("p1", "compile", compatshim.GenV08); err == nil {
		t.Fatal("removed mutating command must not write")
	}
}

func TestExporterClass(t *testing.T) {
	s, ok := compatshim.Classify("export-v08")
	if !ok || s.Class != compatshim.ClassExporter {
		t.Fatalf("%#v", s)
	}
	d := compatshim.NewRegistry(fixed).Evaluate("p1", "export-v08")
	if !d.Allowed || !d.ExcludeFromV09Gates {
		t.Fatalf("%#v", d)
	}
}

func TestUnknownCommandDenied(t *testing.T) {
	d := compatshim.NewRegistry(fixed).Evaluate("p1", "nope")
	if d.Allowed {
		t.Fatal("unknown must deny")
	}
}

func TestSchedulePresent(t *testing.T) {
	s := compatshim.DefaultSchedule()
	if s.ReadOnlyUntil == "" || s.RemovedEffective == "" {
		t.Fatalf("%#v", s)
	}
}

func TestMatrixCoversClasses(t *testing.T) {
	seen := map[compatshim.CommandClass]bool{}
	for _, s := range compatshim.Matrix() {
		seen[s.Class] = true
	}
	for _, c := range []compatshim.CommandClass{
		compatshim.ClassRemoved, compatshim.ClassReadOnly, compatshim.ClassExporter,
		compatshim.ClassUnsupported, compatshim.ClassV09,
	} {
		if !seen[c] {
			t.Fatalf("missing class %s", c)
		}
	}
}
