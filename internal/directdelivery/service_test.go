package directdelivery_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/deliveryresume"
	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/directdelivery"
	"github.com/jasonhnd/loopcoder/internal/directrun"
)

func TestDeliveryReachesHumanGateOnce(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC) }
	svc := directdelivery.Service{Deps: directdelivery.Deps{Now: now}}
	worker := directrun.Result{
		RunID: "run_test1314", AttemptID: "att_test1314", ProjectID: "proj",
		State: directattempt.StateCleanupTerminal, ProviderLaunchN: 1,
		RouteDigest: "route-digest-fixture", WorktreePath: "/tmp/fixture-wt",
		Message: "worker cleanup-terminal",
	}
	res, err := svc.Execute(context.Background(), directdelivery.Request{
		Worker: worker, Repo: "acme/demo", Issue: "42", BaseBranch: "pre-prod",
		OwnedPaths: []string{"docs/CHANGE.md"},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Status != directdelivery.StatusHumanGate {
		t.Fatalf("status=%s err=%s events=%v", res.Status, res.Error, res.Events)
	}
	if res.PRNumber <= 0 {
		t.Fatalf("missing PR: %+v", res)
	}
	if res.CommitSHA == "" {
		t.Fatal("missing commit")
	}
	if res.AutoMerge {
		t.Fatal("auto_merge must be false")
	}
	if res.WorkerLaunches != 1 {
		t.Fatalf("worker launches=%d", res.WorkerLaunches)
	}
	if res.ResumeNext == deliveryresume.ActionNewWorker {
		t.Fatal("resume must not propose new worker")
	}
	// required ordered milestones
	joined := strings.Join(res.Events, "\n")
	for _, want := range []string{
		"worker.cleanup_terminal",
		"localverify.ok:",
		"commit.ok:",
		"hookpolicy.ok",
		"push.ok",
		"push.idempotent_adopt",
		"pr.opened:",
		"ci.ready",
		"verifier.blocked_before_ci",
		"verifier.pass",
		"human_gate.await_owner",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing event prefix %q in %v", want, res.Events)
		}
	}
}

func TestRejectsNonCleanupTerminal(t *testing.T) {
	svc := directdelivery.Service{}
	_, err := svc.Execute(context.Background(), directdelivery.Request{
		Worker: directrun.Result{
			RunID: "r", State: directattempt.StateProcessTerminal, ProviderLaunchN: 1,
		},
		Repo: "acme/demo", Issue: "1",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestIdempotentSecondExecuteDoesNotReplayWorker(t *testing.T) {
	// Two independent delivery executes with same worker evidence must each
	// keep WorkerLaunches==1 (delivery never launches workers).
	now := func() time.Time { return time.Date(2026, 7, 22, 18, 5, 0, 0, time.UTC) }
	svc := directdelivery.Service{Deps: directdelivery.Deps{Now: now}}
	worker := directrun.Result{
		RunID: "run_idem", AttemptID: "att_idem",
		State: directattempt.StateCleanupTerminal, ProviderLaunchN: 1,
		RouteDigest: "rd",
	}
	a, err := svc.Execute(context.Background(), directdelivery.Request{
		Worker: worker, Repo: "acme/demo", Issue: "9",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Execute(context.Background(), directdelivery.Request{
		Worker: worker, Repo: "acme/demo", Issue: "9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.WorkerLaunches != 1 || b.WorkerLaunches != 1 {
		t.Fatalf("launches a=%d b=%d", a.WorkerLaunches, b.WorkerLaunches)
	}
	if a.Status != directdelivery.StatusHumanGate || b.Status != directdelivery.StatusHumanGate {
		t.Fatalf("a=%s b=%s", a.Status, b.Status)
	}
}
