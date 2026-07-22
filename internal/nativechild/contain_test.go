package nativechild

import (
	"errors"
	"testing"
	"time"
)

func TestForbiddenPolicyBlocksStart(t *testing.T) {
	c, err := NewController(Attempt{
		AttemptID: "att1", Provider: "claude", Model: "claude-sonnet-4-5",
		Policy: PolicyForbidden, ParentProcessID: "p1",
	}, DefaultBudgets(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if c.InvocationFlag() != "native_subagents=forbidden" {
		t.Fatal(c.InvocationFlag())
	}
	_, err = c.ObserveStart("s1", "proc1", "p1")
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("%v", err)
	}
	if c.CanTerminalClean() {
		t.Fatal("violation blocks terminal")
	}
}

func TestAllowedChildrenUnderParent(t *testing.T) {
	c, _ := NewController(Attempt{
		AttemptID: "att1", Provider: "codex", Model: "gpt-5.2-codex",
		Policy: PolicyAllowed, ParentProcessID: "root",
	}, DefaultBudgets(), time.Now)
	if c.InvocationFlag() != "native_subagents=allowed" {
		t.Fatal(c.InvocationFlag())
	}
	s, err := c.ObserveStart("s1", "c1", "root")
	if err != nil {
		t.Fatal(err)
	}
	if s.ParentAttempt != "att1" || s.Provider != "codex" {
		t.Fatalf("%+v", s)
	}
	// not a workitem — no ownership fields
	for _, d := range DisallowedOwnership() {
		if d == "" {
			t.Fatal("empty")
		}
	}
	c.SetParentUsage(100, 1000, 100, 10)
	if err := c.SampleUsage("s1", 50, 500, 50, 5); err != nil {
		t.Fatal(err)
	}
	st := c.Status("running_tools")
	if st.CompletionInferredFromChildProse {
		t.Fatal("must not infer from child prose")
	}
	if st.NativeChildActive != 1 || st.TopLevelProgress != "running_tools" {
		t.Fatalf("%+v", st)
	}
	// aggregate is sum not multiplied limits
	if st.Aggregate.TotalCPUms != 50 || st.Aggregate.ParentCPUms != 100 {
		t.Fatalf("%+v", st.Aggregate)
	}
	_ = c.Join("s1")
	if !c.CanTerminalClean() {
		t.Fatal("should clean after join")
	}
}

func TestEscapeBlocksTerminal(t *testing.T) {
	c, _ := NewController(Attempt{
		AttemptID: "att1", Provider: "grok", Model: "grok-4.5",
		Policy: PolicyAllowed, ParentProcessID: "root",
	}, DefaultBudgets(), time.Now)
	_, err := c.ObserveStart("s1", "c1", "other_tree")
	if !errors.Is(err, ErrEscape) {
		t.Fatalf("%v", err)
	}
	// mark escape on allowed child under parent then escape
	_, _ = c.ObserveStart("s2", "c2", "root")
	c.MarkEscape("s2")
	if c.CanTerminalClean() {
		t.Fatal("escape blocks")
	}
	st := c.CancelParent()
	if st.NativeChildActive != 0 {
		t.Fatal("cancel joins")
	}
}

func TestBudgetNotMultiplied(t *testing.T) {
	b := DefaultBudgets()
	b.MaxCPUms = 1000
	c, _ := NewController(Attempt{
		AttemptID: "att1", Provider: "claude", Model: "x",
		Policy: PolicyAllowed, ParentProcessID: "root",
	}, b, time.Now)
	c.SetParentUsage(600, 0, 0, 0)
	_, _ = c.ObserveStart("s1", "c1", "root")
	// child 500 → total 1100 > 1000
	err := c.SampleUsage("s1", 500, 0, 0, 0)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("%v", err)
	}
}

func TestCancelJoinsAll(t *testing.T) {
	c, _ := NewController(Attempt{
		AttemptID: "att1", Provider: "gemini", Model: "flash",
		Policy: PolicyAllowed, ParentProcessID: "root",
	}, DefaultBudgets(), time.Now)
	_, _ = c.ObserveStart("s1", "c1", "root")
	_, _ = c.ObserveStart("s2", "c2", "child_of:root")
	st := c.CancelParent()
	if st.NativeChildActive != 0 || st.NativeChildTotal != 2 {
		t.Fatalf("%+v", st)
	}
}
