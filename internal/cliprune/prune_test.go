package cliprune_test

import (
	"strings"
	"testing"

	"github.com/jasonhnd/loopcoder/internal/cliprune"
)

func TestHelpExcludesHidden(t *testing.T) {
	cat := cliprune.DefaultCatalog()
	help := strings.Join(cliprune.HelpLines(cat), "\n")
	if strings.Contains(help, "compile") || strings.Contains(help, "dispatch") {
		t.Fatalf("hidden in help: %s", help)
	}
	if !strings.Contains(help, "doctor") || !strings.Contains(help, "export-v08") {
		t.Fatalf("missing supported/compat: %s", help)
	}
}

func TestCompletions(t *testing.T) {
	c := cliprune.Completions(cliprune.DefaultCatalog())
	for _, n := range c {
		if n == "tick" || n == "promote" {
			t.Fatal("hidden in completions")
		}
	}
}

func TestInvokeHidden(t *testing.T) {
	r := cliprune.Invoke(cliprune.DefaultCatalog(), "compile")
	if r.Allowed {
		t.Fatal(r)
	}
	r2 := cliprune.Invoke(cliprune.DefaultCatalog(), "doctor")
	if !r2.Allowed {
		t.Fatal(r2)
	}
}

func TestHistoricalSpecsInert(t *testing.T) {
	if err := cliprune.AssertNoCompilerActive(cliprune.HistoricalSpecs()); err != nil {
		t.Fatal(err)
	}
}

func TestHiddenRequiresEvidence(t *testing.T) {
	cat := []cliprune.Command{{
		Name: "foo", Visibility: cliprune.VisHidden, ReplacementEvidenceOK: false,
	}}
	r := cliprune.Invoke(cat, "foo")
	if !r.Allowed {
		t.Fatal("without evidence still wired")
	}
}
