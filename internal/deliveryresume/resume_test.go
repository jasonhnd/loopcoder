package deliveryresume_test

import (
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/deliveryresume"
)

func t0() time.Time { return time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC) }

func baseSnap() deliveryresume.RunSnapshot {
	return deliveryresume.RunSnapshot{
		RunID:                 "run1",
		WorkerLaunchCount:     1,
		WorkerCleanupTerminal: true,
		Stages: map[deliveryresume.StageName]deliveryresume.StageEvidence{
			deliveryresume.StageWorker: {
				Stage: deliveryresume.StageWorker, Outcome: deliveryresume.OutcomeCompleted,
				ReceiptPresent: true, ObservedComplete: true,
			},
			deliveryresume.StageCommit: {
				Stage: deliveryresume.StageCommit, Outcome: deliveryresume.OutcomeIncomplete,
			},
			deliveryresume.StagePush:      {Stage: deliveryresume.StagePush, Outcome: deliveryresume.OutcomeIncomplete},
			deliveryresume.StagePR:        {Stage: deliveryresume.StagePR, Outcome: deliveryresume.OutcomeIncomplete},
			deliveryresume.StageCIWait:    {Stage: deliveryresume.StageCIWait, Outcome: deliveryresume.OutcomeIncomplete},
			deliveryresume.StageVerifier:  {Stage: deliveryresume.StageVerifier, Outcome: deliveryresume.OutcomeIncomplete},
			deliveryresume.StageHumanGate: {Stage: deliveryresume.StageHumanGate, Outcome: deliveryresume.OutcomeIncomplete},
		},
	}
}

func TestResumeCommitAfterWorkerNoWorkerReplay(t *testing.T) {
	snap := baseSnap()
	app := deliveryresume.NewApplier()
	plan, err := deliveryresume.Resume(snap, false, t0, app)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NextAction != deliveryresume.ActionResumeCommit {
		t.Fatalf("next=%s", plan.NextAction)
	}
	if plan.WorkerLaunchCount != 1 {
		t.Fatalf("launch count changed %d", plan.WorkerLaunchCount)
	}
	if app.WorkerLaunches() != 0 {
		t.Fatal("must not launch worker")
	}
	// dry-run no mutation
	app2 := deliveryresume.NewApplier()
	_, err = deliveryresume.Resume(snap, true, t0, app2)
	if err != nil {
		t.Fatal(err)
	}
	if len(app2.SideEffects) != 0 {
		t.Fatal("dry-run mutated")
	}
}

func TestAmbiguousPushReadBackThenAdopt(t *testing.T) {
	snap := baseSnap()
	snap.Stages[deliveryresume.StageCommit] = deliveryresume.StageEvidence{
		Stage: deliveryresume.StageCommit, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true,
	}
	snap.Stages[deliveryresume.StagePush] = deliveryresume.StageEvidence{
		Stage: deliveryresume.StagePush, Outcome: deliveryresume.OutcomeAmbiguous,
		Reason: "push timeout",
	}
	plan, err := deliveryresume.Resume(snap, true, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NextAction != deliveryresume.ActionReadBackThenRetry {
		t.Fatalf("next=%s", plan.NextAction)
	}
	// after read-back: adoptable
	snap.Stages[deliveryresume.StagePush] = deliveryresume.StageEvidence{
		Stage: deliveryresume.StagePush, Outcome: deliveryresume.OutcomeAdoptable,
		ObservedComplete: true, Reason: "remote oid matches",
	}
	plan, err = deliveryresume.Resume(snap, false, t0, deliveryresume.NewApplier())
	if err != nil {
		t.Fatal(err)
	}
	if plan.NextAction != deliveryresume.ActionAdoptSideEffect {
		t.Fatalf("next=%s", plan.NextAction)
	}
}

func TestDriftBlocksAutomaticResume(t *testing.T) {
	snap := baseSnap()
	snap.Stages[deliveryresume.StageCommit] = deliveryresume.StageEvidence{
		Stage: deliveryresume.StageCommit, Outcome: deliveryresume.OutcomeIncomplete,
		ExpectedRouteDigest: "route_a", RouteDigest: "route_b",
	}
	plan, err := deliveryresume.Resume(snap, true, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NextAction != deliveryresume.ActionNeedsOwner {
		t.Fatalf("next=%s steps=%+v", plan.NextAction, plan.Steps)
	}
	if plan.Steps[0].Reason == "" || plan.Steps[0].Mutates {
		t.Fatalf("%+v", plan.Steps[0])
	}
}

func TestConvergeStable(t *testing.T) {
	snap := baseSnap()
	act, ok, err := deliveryresume.Converge(snap, t0)
	if err != nil || !ok || act != deliveryresume.ActionResumeCommit {
		t.Fatalf("act=%s ok=%v err=%v", act, ok, err)
	}
	// complete all delivery → done
	for _, s := range []deliveryresume.StageName{
		deliveryresume.StageCommit, deliveryresume.StagePush, deliveryresume.StagePR,
		deliveryresume.StageCIWait, deliveryresume.StageVerifier, deliveryresume.StageHumanGate,
	} {
		snap.Stages[s] = deliveryresume.StageEvidence{Stage: s, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true}
	}
	act, ok, err = deliveryresume.Converge(snap, t0)
	if err != nil || !ok || act != deliveryresume.ActionDone {
		t.Fatalf("act=%s ok=%v err=%v", act, ok, err)
	}
}

func TestNoAutomaticWorkerReplayAfterCompletion(t *testing.T) {
	snap := baseSnap()
	// mark all delivery done except human gate
	for _, s := range []deliveryresume.StageName{
		deliveryresume.StageCommit, deliveryresume.StagePush, deliveryresume.StagePR,
		deliveryresume.StageCIWait, deliveryresume.StageVerifier,
	} {
		snap.Stages[s] = deliveryresume.StageEvidence{Stage: s, Outcome: deliveryresume.OutcomeCompleted, ReceiptPresent: true}
	}
	snap.Stages[deliveryresume.StageHumanGate] = deliveryresume.StageEvidence{
		Stage: deliveryresume.StageHumanGate, Outcome: deliveryresume.OutcomeNeedsHuman,
	}
	plan, err := deliveryresume.Resume(snap, true, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NextAction != deliveryresume.ActionAwaitHumanGate {
		t.Fatalf("next=%s", plan.NextAction)
	}
	if plan.NextAction == deliveryresume.ActionNewWorker {
		t.Fatal("worker replay")
	}
}

func TestWorkerIncompleteNeedsOwnerUnlessApproved(t *testing.T) {
	snap := baseSnap()
	snap.WorkerCleanupTerminal = false
	snap.Stages[deliveryresume.StageWorker] = deliveryresume.StageEvidence{
		Stage: deliveryresume.StageWorker, Outcome: deliveryresume.OutcomeIncomplete,
	}
	plan, err := deliveryresume.Resume(snap, true, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NextAction != deliveryresume.ActionNeedsOwner {
		t.Fatalf("next=%s", plan.NextAction)
	}
	snap.OwnerApprovedNewWorker = true
	plan, err = deliveryresume.Resume(snap, true, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NextAction != deliveryresume.ActionNewWorker {
		t.Fatalf("next=%s", plan.NextAction)
	}
}

func TestDryRunListsEvidenceAndReasons(t *testing.T) {
	snap := baseSnap()
	plan, err := deliveryresume.Resume(snap, true, t0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || len(plan.EvidenceSummary) == 0 || len(plan.Steps) == 0 {
		t.Fatalf("%+v", plan)
	}
	if plan.Steps[0].Reason == "" || plan.Steps[0].SideEffect == "" {
		t.Fatalf("%+v", plan.Steps[0])
	}
}
