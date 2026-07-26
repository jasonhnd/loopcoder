package rehydrate

import (
	"fmt"
	"time"
)

// FixtureTerminalMerged returns fake GitHub evidence for a merged PR handoff.
func FixtureTerminalMerged() RemoteEvidence {
	return RemoteEvidence{
		Schema: SchemaEvidence,
		Repo:   RepoRef{Owner: "acme", Name: "app", Visibility: "private"},
		Issue:  IssueRef{Number: 42, State: "closed", Title: "ship feature"},
		PR: PRRef{
			Number: 7, State: "closed", Merged: true,
			MergeSHA:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			HeadSHA:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			BaseBranch: "pre-prod", HeadBranch: "ordinary/issue-42",
		},
		Commits: []CommitRef{
			{SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Message: "feat: ship"},
			{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Message: "Merge PR #7"},
		},
		Checks: []CheckRef{
			{Name: "verify", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
		},
		Reviews:           []ReviewRef{{ID: "rev-1", State: "approved"}},
		RouteEvidenceRefs: []string{"route:direct:42", "evidence:sha:aaaa"},
	}
}

// FixtureInFlight returns open PR evidence that must not be adopted.
func FixtureInFlight() RemoteEvidence {
	ev := FixtureTerminalMerged()
	ev.PR.Merged = false
	ev.PR.State = "open"
	ev.PR.MergeSHA = ""
	ev.Issue.State = "open"
	ev.Checks = []CheckRef{{Name: "test", Status: "in_progress", Conclusion: ""}}
	return ev
}

// FixtureAmbiguous returns conflicting remote signals.
func FixtureAmbiguous() RemoteEvidence {
	ev := FixtureTerminalMerged()
	ev.PR.State = "open" // conflicts with Merged=true
	return ev
}

// RunHandoffCanary simulates Mac A terminal delivery and Mac B rehydrate from
// GitHub fixtures only — two isolated home stores, no shared DB.
func RunHandoffCanary() error {
	fixed := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	// Mac A "done" is only represented as remote evidence — no Mac A store is read.
	evidence := FixtureTerminalMerged()

	macB := NewStore("mac-b", func() time.Time { return fixed })
	r1 := macB.Rehydrate(Input{
		Evidence:  evidence,
		Checkout:  LocalCheckout{Path: "/Users/b/src/app", HeadSHA: evidence.PR.MergeSHA, Branch: "pre-prod"},
		MachineID: "mac-b",
	})
	if !r1.Allowed {
		return fmt.Errorf("mac B first rehydrate denied: %v", r1.Reasons)
	}
	if r1.Project == nil || r1.Project.ProjectID == "" {
		return fmt.Errorf("mac B missing project identity")
	}
	if r1.DeliveryState != StateMerged {
		return fmt.Errorf("want merged got %s", r1.DeliveryState)
	}
	if r1.Event == nil || r1.Event.PRNumber != 7 || r1.Event.IssueNumber != 42 {
		return fmt.Errorf("event missing remote identities: %#v", r1.Event)
	}
	if len(r1.Event.RouteEvidenceRefs) == 0 {
		return fmt.Errorf("route evidence refs not linked")
	}
	if r1.NewLocalExecutionID == "" {
		return fmt.Errorf("expected new local execution identity")
	}

	// Idempotent second rehydrate on Mac B.
	r2 := macB.Rehydrate(Input{
		Evidence:  evidence,
		Checkout:  LocalCheckout{Path: "/Users/b/src/app", HeadSHA: evidence.PR.MergeSHA, Branch: "pre-prod"},
		MachineID: "mac-b",
	})
	if !r2.Allowed || r2.Event == nil || !r2.Event.IdempotentReplay {
		return fmt.Errorf("second rehydrate should be idempotent: %#v", r2)
	}
	if macB.ProjectCount() != 1 {
		return fmt.Errorf("idempotent rehydrate must not create second project")
	}

	// Foreign in-flight adoption rejected.
	r3 := macB.Rehydrate(Input{
		Evidence:            evidence,
		Checkout:            LocalCheckout{Path: "/Users/b/src/app"},
		MachineID:           "mac-b",
		AttemptAdoptForeign: true,
		ForeignAttemptID:    "mac-a-attempt-99",
	})
	if r3.Allowed {
		return fmt.Errorf("foreign attempt adoption must be rejected")
	}
	if r3.Event == nil || !r3.Event.ForeignAttemptRejected {
		return fmt.Errorf("foreign rejection not recorded")
	}
	if r3.NewLocalExecutionID == "mac-a-attempt-99" {
		return fmt.Errorf("must not reuse foreign attempt id")
	}

	// In-flight remote cannot be adopted as live local attempt.
	r4 := macB.Rehydrate(Input{
		Evidence:  FixtureInFlight(),
		Checkout:  LocalCheckout{Path: "/Users/b/src/app"},
		MachineID: "mac-b",
	})
	if r4.Allowed {
		return fmt.Errorf("in-flight remote must not rehydrate as live attempt")
	}

	// Ambiguous requires reconciliation.
	r5 := macB.Rehydrate(Input{
		Evidence:  FixtureAmbiguous(),
		Checkout:  LocalCheckout{Path: "/Users/b/src/app"},
		MachineID: "mac-b",
	})
	if r5.Allowed {
		return fmt.Errorf("ambiguous remote must not auto-rehydrate")
	}

	// Same short name, different owner → isolated projects.
	other := evidence
	other.Repo.Owner = "other"
	other.Issue.Number = 99
	other.PR.Number = 3
	r6 := macB.Rehydrate(Input{
		Evidence:  other,
		Checkout:  LocalCheckout{Path: "/Users/b/src/other-app"},
		MachineID: "mac-b",
	})
	if !r6.Allowed {
		return fmt.Errorf("other owner repo should rehydrate: %v", r6.Reasons)
	}
	if r6.Project.ProjectID == r1.Project.ProjectID {
		return fmt.Errorf("same short name different owner must not share project_id")
	}
	if macB.ProjectCount() != 2 {
		return fmt.Errorf("want 2 isolated projects got %d", macB.ProjectCount())
	}

	// Visibility isolation: public vs private same owner/name fails closed.
	visFlip := evidence
	visFlip.Repo.Visibility = "public"
	r7 := macB.Rehydrate(Input{
		Evidence:  visFlip,
		Checkout:  LocalCheckout{Path: "/Users/b/src/app"},
		MachineID: "mac-b",
	})
	if r7.Allowed {
		return fmt.Errorf("visibility change must require explicit reconciliation")
	}

	// Fresh Mac C store cannot see Mac B projects (no shared DB).
	macC := NewStore("mac-c", func() time.Time { return fixed })
	if macC.ProjectCount() != 0 {
		return fmt.Errorf("mac C must start empty")
	}
	r8 := macC.Rehydrate(Input{
		Evidence:  evidence,
		Checkout:  LocalCheckout{Path: "/Users/c/src/app", HeadSHA: evidence.PR.MergeSHA, Branch: "pre-prod"},
		MachineID: "mac-c",
	})
	if !r8.Allowed {
		return fmt.Errorf("mac C rehydrate from GitHub only failed: %v", r8.Reasons)
	}
	if r8.Project.MachineID != "mac-c" {
		return fmt.Errorf("mac C project machine_id=%s", r8.Project.MachineID)
	}
	// New local execution identity differs across machines.
	if r8.NewLocalExecutionID == r1.NewLocalExecutionID {
		return fmt.Errorf("local execution ids must not collide across machines")
	}
	return nil
}
