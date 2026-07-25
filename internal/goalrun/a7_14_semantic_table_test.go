package goalrun

import "testing"

func TestA714_SemanticTable_TerminalFailureClasses(t *testing.T) {
	// Succeeded: empty class only.
	if err := requireTerminalFailureSemantics("succeeded", ""); err != nil {
		t.Fatal(err)
	}
	if err := requireTerminalFailureSemantics("succeeded", "anything"); err == nil {
		t.Fatal("succeeded+class must fail")
	}
	// Cancelled: exact typed abort only.
	if err := requireTerminalFailureSemantics("cancelled", "forced_interrupt"); err != nil {
		t.Fatal(err)
	}
	if err := requireTerminalFailureSemantics("cancelled", "model_unavailable"); err == nil {
		t.Fatal("cancelled+MU must fail")
	}
	if err := requireTerminalFailureSemantics("cancelled", "Forced_Interrupt"); err == nil {
		t.Fatal("case alias must fail")
	}
	// MU failed.
	if err := requireTerminalFailureSemantics("failed", "model_unavailable"); err != nil {
		t.Fatal(err)
	}
	// Generic failed enumerated.
	if err := requireTerminalFailureSemantics("failed", "executor_error"); err != nil {
		t.Fatal(err)
	}
	if err := requireTerminalFailureSemantics("failed", ""); err == nil {
		t.Fatal("empty failed class must fail")
	}
	if err := requireTerminalFailureSemantics("failed", "not_a_real_class"); err == nil {
		t.Fatal("unknown failed class must fail")
	}
	if err := requireTerminalFailureSemantics("failed", " forced_interrupt"); err == nil {
		t.Fatal("padded class must fail")
	}
	// Typed abort on failed rejected.
	if err := requireTerminalFailureSemantics("failed", "forced_interrupt"); err == nil {
		t.Fatal("typed abort requires cancelled")
	}
	// Skipped: empty class.
	if err := requireTerminalFailureSemantics("skipped", ""); err != nil {
		t.Fatal(err)
	}
	if err := requireTerminalFailureSemantics("skipped", "x"); err == nil {
		t.Fatal("skipped+class must fail")
	}
	// Aliases rejected.
	for _, term := range []string{"Succeeded", "canceled", "aborted", " failed"} {
		if err := requireTerminalFailureSemantics(term, ""); err == nil {
			t.Fatalf("alias terminal %q must fail", term)
		}
	}
}
