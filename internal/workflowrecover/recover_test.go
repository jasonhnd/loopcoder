package workflowrecover

import (
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/workgraph"
)

func TestCancelJoinsAndReleases(t *testing.T) {
	s := NewStore(time.Now)
	_ = s.Create("wf1", []ChildState{
		{ChildID: "c1", WorkItemID: "a", Required: true, Kind: "Running", ClaimStarted: true},
		{ChildID: "c2", WorkItemID: "b", Required: true, Kind: "UnstartedClaim", ClaimStarted: false},
		{ChildID: "c3", WorkItemID: "c", Required: false, Kind: "Ambiguous", ClaimStarted: true},
	}, 2, 1)
	rep, err := s.Cancel("wf1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.JoinedChildren) != 1 || rep.JoinedChildren[0] != "c1" {
		t.Fatalf("joined %+v", rep)
	}
	if len(rep.ReleasedClaims) != 1 || rep.ReleasedClaims[0] != "c2" {
		t.Fatalf("released %+v", rep)
	}
	if len(rep.Ambiguous) != 1 {
		t.Fatalf("ambiguous %+v", rep)
	}
	if !rep.IntegrationCancel {
		t.Fatal("integ cancel")
	}
}

func TestRestartAdoptsWithoutDuplicate(t *testing.T) {
	s := NewStore(time.Now)
	_ = s.Create("wf1", []ChildState{
		{ChildID: "c1", WorkItemID: "a", Required: true, Kind: "Running", ClaimStarted: true},
	}, 5, 3)
	rep, err := s.Restart("wf1", []string{"c1"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.ResumedWaveSeq != 5 || rep.ResumedIntegSeq != 3 || !rep.DuplicateBlocked {
		t.Fatalf("%+v", rep)
	}
	if len(rep.AdoptedLive) != 1 {
		t.Fatalf("%+v", rep)
	}
}

func TestParentTerminalAfterDurableChildren(t *testing.T) {
	s := NewStore(time.Now)
	_ = s.Create("wf1", []ChildState{
		{ChildID: "c1", WorkItemID: "a", Required: true, Kind: "Running", ClaimStarted: true},
		{ChildID: "c2", WorkItemID: "b", Required: true, Kind: "Running", ClaimStarted: true},
	}, 1, 0)
	if err := s.AcceptChildTerminal("wf1", "c1", workgraph.TermSucceeded, true); err != nil {
		t.Fatal(err)
	}
	// parent not yet
	p, _ := s.Project("wf1")
	if p.ParentTerminal != "" {
		t.Fatalf("early parent %s", p.ParentTerminal)
	}
	if err := s.AcceptChildTerminal("wf1", "c2", workgraph.TermSucceeded, true); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Project("wf1")
	if p.ParentTerminal != string(workgraph.TermSucceeded) {
		t.Fatalf("%+v", p)
	}
}

func TestPersistErrorSuppressesParentSuccess(t *testing.T) {
	s := NewStore(time.Now)
	_ = s.Create("wf1", []ChildState{
		{ChildID: "c1", WorkItemID: "a", Required: true, Kind: "Running", ClaimStarted: true},
	}, 1, 0)
	err := s.AcceptChildTerminal("wf1", "c1", workgraph.TermSucceeded, false)
	if err == nil {
		t.Fatal("expected persist error")
	}
	p, _ := s.Project("wf1")
	if p.ParentTerminal == string(workgraph.TermSucceeded) {
		t.Fatal("parent success suppressed")
	}
}

func TestProjectionPreservesEvents(t *testing.T) {
	s := NewStore(time.Now)
	_ = s.Create("wf1", []ChildState{
		{ChildID: "c1", WorkItemID: "a", Required: true, Kind: "Running", ClaimStarted: true},
	}, 1, 0)
	_ = s.AcceptChildTerminal("wf1", "c1", workgraph.TermFailed, true)
	p1, err := s.Project("wf1")
	if err != nil {
		t.Fatal(err)
	}
	ev1, _ := s.Events("wf1")
	p2, _ := s.Project("wf1")
	ev2, _ := s.Events("wf1")
	if !p1.SourceEventsIntact || p1.AuditDigest != p2.AuditDigest {
		t.Fatal("projection mutated")
	}
	if len(ev1) != len(ev2) || len(ev1) == 0 {
		t.Fatal("events")
	}
	if p1.EventRangeFrom == "" || p1.ParentTerminal != string(workgraph.TermFailed) {
		t.Fatalf("%+v", p1)
	}
}

func TestRequiredFailureParentFailed(t *testing.T) {
	s := NewStore(time.Now)
	_ = s.Create("wf1", []ChildState{
		{ChildID: "c1", WorkItemID: "a", Required: true, Kind: "Running", ClaimStarted: true},
		{ChildID: "c2", WorkItemID: "b", Required: true, Kind: "Running", ClaimStarted: true},
	}, 1, 0)
	_ = s.AcceptChildTerminal("wf1", "c1", workgraph.TermSucceeded, true)
	_ = s.AcceptChildTerminal("wf1", "c2", workgraph.TermFailed, true)
	p, _ := s.Project("wf1")
	if p.ParentTerminal != string(workgraph.TermFailed) {
		t.Fatalf("%s", p.ParentTerminal)
	}
}

func TestCancelAmbiguousNoParentSuccess(t *testing.T) {
	s := NewStore(time.Now)
	_ = s.Create("wf1", []ChildState{
		{ChildID: "c1", WorkItemID: "a", Required: true, Kind: "Ambiguous", ClaimStarted: true},
	}, 1, 0)
	_, err := s.Cancel("wf1", false)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := s.Project("wf1")
	if p.ParentTerminal == string(workgraph.TermSucceeded) {
		t.Fatal("no success with ambiguous")
	}
	_ = errors.New // keep import if needed
}
