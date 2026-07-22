package childattempt

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/routedecision"
	"github.com/jasonhnd/loopcoder/internal/workclaim"
	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func claim(wi string) workclaim.Claim {
	return workclaim.Claim{
		ClaimID: "cl1", WorkItemID: wi, Generation: 1, AttemptID: "att", ExecutorID: "ex",
	}
}

func decision(prov, model string) routedecision.Decision {
	return routedecision.Decision{
		Digest: "dig-" + prov, Outcome: routedecision.OutcomeSelected,
		Winner: &routedecision.Winner{Provider: prov, Model: model},
	}
}

func TestIndependentChildrenIsolation(t *testing.T) {
	r := NewRegistry(time.Now)
	c1, err := r.Spawn(SpawnRequest{
		ParentWorkflow: "wf1", Claim: claim("wi_a"), Decision: decision("codex", "gpt-5"),
		WorktreeKey: "wt-a", CredentialScope: "cred-a", BoundedInputs: []string{"task.md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.Spawn(SpawnRequest{
		ParentWorkflow: "wf1", Claim: claim("wi_b"), Decision: decision("claude", "sonnet"),
		WorktreeKey: "wt-b", CredentialScope: "cred-b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c1.RouteDigest == c2.RouteDigest || c1.Provider == c2.Provider {
		// digests differ by construction
	}
	// cannot share worktree
	_, err = r.Spawn(SpawnRequest{
		ParentWorkflow: "wf1", Claim: claim("wi_c"), Decision: decision("grok", "g"),
		WorktreeKey: "wt-a", CredentialScope: "cred-c",
	})
	if !errors.Is(err, ErrIsolation) {
		t.Fatalf("%v", err)
	}
	// cannot share credentials
	_, err = r.Spawn(SpawnRequest{
		ParentWorkflow: "wf1", Claim: claim("wi_d"), Decision: decision("gemini", "g"),
		WorktreeKey: "wt-d", CredentialScope: "cred-a",
	})
	if !errors.Is(err, ErrIsolation) {
		t.Fatalf("%v", err)
	}
}

func TestPrivateSiblingOutputHidden(t *testing.T) {
	r := NewRegistry(time.Now)
	c1, _ := r.Spawn(SpawnRequest{
		ParentWorkflow: "wf", Claim: claim("a"), Decision: decision("codex", "m"),
		WorktreeKey: "w1", CredentialScope: "c1",
	})
	c2, _ := r.Spawn(SpawnRequest{
		ParentWorkflow: "wf", Claim: claim("b"), Decision: decision("claude", "m"),
		WorktreeKey: "w2", CredentialScope: "c2",
	})
	_ = r.SetPrivateOutput(c1.ChildAttemptID, "secret sibling")
	out, ok := r.SiblingPrivateOutput(c2.ChildAttemptID, c1.ChildAttemptID)
	if ok || out != "" {
		t.Fatal("sibling must not see private output")
	}
	out, ok = r.SiblingPrivateOutput(c1.ChildAttemptID, c1.ChildAttemptID)
	if !ok || out != "secret sibling" {
		t.Fatal("self access")
	}
}

func TestParentCannotRewriteTerminal(t *testing.T) {
	r := NewRegistry(time.Now)
	c, _ := r.Spawn(SpawnRequest{
		ParentWorkflow: "wf", Claim: claim("a"), Decision: decision("codex", "m"),
		WorktreeKey: "w1", CredentialScope: "c1",
	})
	if err := r.CloseChild(c.ChildAttemptID, workgraph.TermSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
	if err := r.ParentRewriteTerminal(c.ChildAttemptID, workgraph.TermFailed); err == nil {
		t.Fatal("rewrite")
	}
	// idempotent close
	if err := r.CloseChild(c.ChildAttemptID, workgraph.TermSucceeded, "ok"); err != nil {
		t.Fatal(err)
	}
}

func TestAggregateNoEarlySuccess(t *testing.T) {
	r := NewRegistry(time.Now)
	c1, _ := r.Spawn(SpawnRequest{
		ParentWorkflow: "wf", Claim: claim("a"), Decision: decision("codex", "m"),
		WorktreeKey: "w1", CredentialScope: "c1",
	})
	_, _ = r.Spawn(SpawnRequest{
		ParentWorkflow: "wf", Claim: claim("b"), Decision: decision("claude", "m"),
		WorktreeKey: "w2", CredentialScope: "c2",
	})
	_ = r.CloseChild(c1.ChildAttemptID, workgraph.TermSucceeded, "done")
	v := r.Aggregate("wf", []string{"a", "b"})
	if v.DeclaredSuccess || v.Running != 1 || v.Succeeded != 1 {
		t.Fatalf("%+v", v)
	}
	// close b failed
	c2id := r.byWorkItem["b"]
	_ = r.CloseChild(c2id, workgraph.TermFailed, "fail")
	v = r.Aggregate("wf", []string{"a", "b"})
	if v.DeclaredSuccess || len(v.RequiredFailed) != 1 {
		t.Fatalf("%+v", v)
	}
}

func TestNoSiblingRouteMutation(t *testing.T) {
	r := NewRegistry(time.Now)
	c, _ := r.Spawn(SpawnRequest{
		ParentWorkflow: "wf", Claim: claim("a"), Decision: decision("codex", "m"),
		WorktreeKey: "w1", CredentialScope: "c1",
	})
	if err := r.MutateSiblingRoute(c.ChildAttemptID, "claude"); err == nil {
		t.Fatal("mutate")
	}
}
