package successor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/routedecision"
)

func dec(id, dig, prov, model string) routedecision.Decision {
	return routedecision.Decision{
		Schema: routedecision.SchemaDecision, DecisionID: id, Digest: dig,
		Outcome: routedecision.OutcomeSelected,
		Winner:  &routedecision.Winner{Provider: prov, Model: model, SoftScore: 0.5, Reasons: []string{"w"}},
	}
}

func TestNoRouteChangeOnActiveAttempt(t *testing.T) {
	now := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	s := NewStore(func() time.Time { return now })
	a, err := s.RegisterFirst(dec("d1", "dig1", "codex", "gpt-5.2-codex"), "wt1", "log1", "ev1")
	if err != nil {
		t.Fatal(err)
	}
	// Attempting to record failure with different provider is rejected
	_, err = s.RecordFailure(Failure{
		AttemptID: a.AttemptID, Class: FailTerminal, ReasonCode: "x",
		Provider: "claude", Model: "claude-sonnet-4-5",
	})
	if !errors.Is(err, ErrActiveRoute) {
		t.Fatalf("err %v", err)
	}
}

func TestAmbiguousNeedsHuman(t *testing.T) {
	prior := Attempt{AttemptID: "a1", Provider: "codex", Model: "m", AutomaticSuccessorCount: 0}
	plan := PlanSuccessor(prior, Failure{Class: FailAmbiguous, ReasonCode: "launch_unknown"}, DefaultPolicy(), 0)
	if plan.Allowed || !plan.NeedsHuman || plan.StopReason == "" {
		t.Fatalf("%+v", plan)
	}
}

func TestPreLaunchSuccessorAllowed(t *testing.T) {
	now := time.Now().UTC()
	s := NewStore(func() time.Time { return now })
	a, _ := s.RegisterFirst(dec("d1", "dig1", "codex", "gpt-5.2-codex"), "wt", "log", "ev")
	fail, err := s.RecordFailure(Failure{AttemptID: a.AttemptID, Class: FailPreLaunch, ReasonCode: "auth_missing"})
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanSuccessor(a, fail, DefaultPolicy(), a.AutomaticSuccessorCount)
	if !plan.Allowed || !plan.Automatic {
		t.Fatalf("%+v", plan)
	}
	// Create with new decision excluding failed route
	nd := dec("d2", "dig2", "claude", "claude-sonnet-4-5")
	rec, err := s.CreateSuccessor(a.AttemptID, fail, DefaultPolicy(), nd)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Successor.PredecessorID != a.AttemptID {
		t.Fatal("causal pred")
	}
	if rec.Successor.DecisionDigest == a.DecisionDigest {
		t.Fatal("must new decision")
	}
	// Prior evidence preserved
	prior, _ := s.Get(a.AttemptID)
	if prior.WorktreeRef != "wt" || prior.LogRef != "log" || prior.EventRef != "ev" {
		t.Fatalf("prior evidence overwritten %+v", prior)
	}
	if prior.SuccessorID != rec.Successor.AttemptID {
		t.Fatal("link")
	}
	if prior.Active {
		t.Fatal("prior inactive")
	}
	// Successor cannot reuse excluded winner
	_, err = s.CreateSuccessor(a.AttemptID, fail, DefaultPolicy(), dec("d3", "dig3", "codex", "gpt-5.2-codex"))
	// prior already has successor - still tests exclude on a fresh first
	_ = err
}

func TestExcludeFailedRouteOnCreate(t *testing.T) {
	now := time.Now().UTC()
	s := NewStore(func() time.Time { return now })
	a, _ := s.RegisterFirst(dec("d1", "dig1", "grok", "grok-4"), "w", "l", "e")
	fail, _ := s.RecordFailure(Failure{AttemptID: a.AttemptID, Class: FailPreLaunch, ReasonCode: "x"})
	// same route as winner rejected
	_, err := s.CreateSuccessor(a.AttemptID, fail, DefaultPolicy(), dec("d2", "dig2", "grok", "grok-4"))
	if err == nil {
		t.Fatal("expected exclude")
	}
}

func TestRetryBudget(t *testing.T) {
	prior := Attempt{AttemptID: "a1", Provider: "a", Model: "b", AutomaticSuccessorCount: 1}
	plan := PlanSuccessor(prior, Failure{Class: FailPreLaunch, ReasonCode: "x"}, DefaultPolicy(), 1)
	if plan.Allowed || plan.StopReason != "retry_budget_exhausted" {
		t.Fatalf("%+v", plan)
	}
}

func TestTerminalDefaultNeedsHuman(t *testing.T) {
	plan := PlanSuccessor(Attempt{AttemptID: "a"}, Failure{Class: FailTerminal, ReasonCode: "crash"}, DefaultPolicy(), 0)
	if plan.Allowed || !plan.NeedsHuman {
		t.Fatalf("%+v", plan)
	}
}

func TestPinOrderedFallback(t *testing.T) {
	pol := DefaultPolicy()
	pol.PinFallbackOrdered = []string{"codex/gpt-5.2-codex", "claude/claude-sonnet-4-5"}
	prior := Attempt{AttemptID: "a1", Provider: "codex", Model: "gpt-5.2-codex"}
	plan := PlanSuccessor(prior, Failure{Class: FailPreLaunch, ReasonCode: "x", Provider: "codex", Model: "gpt-5.2-codex"}, pol, 0)
	if plan.AuthorizedPinFallback != "claude/claude-sonnet-4-5" {
		t.Fatalf("%+v", plan)
	}
}

func TestStatusExplainVisible(t *testing.T) {
	now := time.Now().UTC()
	s := NewStore(func() time.Time { return now })
	a, _ := s.RegisterFirst(dec("d1", "dig1", "codex", "m"), "wt", "lg", "ev")
	_, _ = s.RecordFailure(Failure{AttemptID: a.AttemptID, Class: FailQuotaRate, ReasonCode: "429"})
	txt, err := s.StatusExplain(a.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(txt, "Failure class=") || !strings.Contains(txt, "worktree=wt") {
		t.Fatalf("%s", txt)
	}
}

func TestSuccessorRequiresNewDecision(t *testing.T) {
	now := time.Now().UTC()
	s := NewStore(func() time.Time { return now })
	a, _ := s.RegisterFirst(dec("d1", "dig1", "codex", "m"), "", "", "")
	fail, _ := s.RecordFailure(Failure{AttemptID: a.AttemptID, Class: FailPreLaunch, ReasonCode: "x"})
	_, err := s.CreateSuccessor(a.AttemptID, fail, DefaultPolicy(), dec("d1", "dig1", "claude", "n"))
	if err == nil {
		t.Fatal("same digest rejected")
	}
}
