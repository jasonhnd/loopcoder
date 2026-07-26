package directdelivery_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/ciwatch"
	"github.com/jasonhnd/loopcoder/internal/commitstage"
	"github.com/jasonhnd/loopcoder/internal/deliveryresume"
	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/directdelivery"
	"github.com/jasonhnd/loopcoder/internal/directrun"
	"github.com/jasonhnd/loopcoder/internal/prstage"
	"github.com/jasonhnd/loopcoder/internal/pushstage"
	"github.com/jasonhnd/loopcoder/internal/routepin"
)

// testDeps injects Fake* ports explicitly — production never auto-wires them.
func testDeps(t *testing.T, now func() time.Time, baseSHA string, owned []string) directdelivery.Deps {
	t.Helper()
	fg := commitstage.NewFakeGit(baseSHA)
	fg.SetDirty(owned)
	return directdelivery.Deps{
		Now:    now,
		Git:    fg,
		Remote: pushstage.NewFakeRemote(),
		GitHub: prstage.NewFakeGitHub(),
		ObserveCI: func(_ context.Context, pr int, head string, reqChecks []string) (ciwatch.RemoteSnapshot, error) {
			cs := make([]ciwatch.CheckState, 0, len(reqChecks))
			for _, n := range reqChecks {
				cs = append(cs, ciwatch.CheckState{Name: n, Conclusion: "success", Required: true})
			}
			return ciwatch.RemoteSnapshot{PRNumber: pr, HeadOID: head, Checks: cs, ObservedAt: now()}, nil
		},
		VerifierRoute: routepin.Fields{
			Provider: "fixture", Model: "fixture-verifier", Effort: "low",
			Permission: "read-only", SubagentPolicy: routepin.SubagentForbidden,
		},
		AllowNilHookExec:          true,
		AllowSyntheticLocalVerify: true,
		AllowSyntheticVerifier:    true,
	}
}

func TestDeliveryReachesHumanGateOnce(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC) }
	baseSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	owned := []string{"docs/CHANGE.md"}
	svc := directdelivery.Service{Deps: testDeps(t, now, baseSHA, owned)}
	worker := directrun.Result{
		RunID: "run_test1314", AttemptID: "att_test1314", ProjectID: "proj",
		State: directattempt.StateCleanupTerminal, ProviderLaunchN: 1,
		RouteDigest: "route-digest-fixture", WorktreePath: "/tmp/fixture-wt",
		BaseSHA: baseSHA, ChangedPaths: owned,
		Message: "worker cleanup-terminal",
	}
	res, err := svc.Execute(context.Background(), directdelivery.Request{
		Worker: worker, Repo: "acme/demo", Issue: "42", BaseBranch: "pre-prod",
		OwnedPaths: owned, RequiredChecks: []string{"verify", "test", "race", "security"},
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

func TestFailClosedWithoutPorts(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC) }
	svc := directdelivery.Service{Deps: directdelivery.Deps{Now: now}}
	worker := directrun.Result{
		RunID: "run_fail", AttemptID: "att_fail",
		State: directattempt.StateCleanupTerminal, ProviderLaunchN: 1,
		RouteDigest: "rd", BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ChangedPaths: []string{"a.go"},
	}
	_, err := svc.Execute(context.Background(), directdelivery.Request{
		Worker: worker, Repo: "acme/demo", Issue: "9", BaseBranch: "pre-prod",
		RequiredChecks: []string{"verify"},
	})
	if err == nil {
		t.Fatal("expected fail closed without ports")
	}
	if !strings.Contains(err.Error(), "git port required") {
		t.Fatalf("want git port required, got %v", err)
	}
}

func TestFailClosedNoSyntheticOwnerOrIssue(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 22, 18, 0, 0, 0, time.UTC) }
	baseSHA := "cccccccccccccccccccccccccccccccccccccccc"
	owned := []string{"docs/CHANGE.md"}
	svc := directdelivery.Service{Deps: testDeps(t, now, baseSHA, owned)}
	worker := directrun.Result{
		RunID: "run_syn", AttemptID: "att_syn",
		State: directattempt.StateCleanupTerminal, ProviderLaunchN: 1,
		RouteDigest: "rd", BaseSHA: baseSHA, ChangedPaths: owned,
	}
	_, err := svc.Execute(context.Background(), directdelivery.Request{
		Worker: worker, Repo: "not-a-slug", Issue: "abc", BaseBranch: "pre-prod",
		OwnedPaths: owned, RequiredChecks: []string{"verify"},
	})
	if err == nil {
		t.Fatal("expected fail closed for synthetic owner/issue")
	}
}

func TestIdempotentSecondExecuteDoesNotReplayWorker(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 7, 22, 18, 5, 0, 0, time.UTC) }
	baseSHA := "dddddddddddddddddddddddddddddddddddddddd"
	owned := []string{"docs/CHANGE.md"}
	svc := directdelivery.Service{Deps: testDeps(t, now, baseSHA, owned)}
	worker := directrun.Result{
		RunID: "run_idem", AttemptID: "att_idem",
		State: directattempt.StateCleanupTerminal, ProviderLaunchN: 1,
		RouteDigest: "rd", BaseSHA: baseSHA, ChangedPaths: owned,
	}
	a, err := svc.Execute(context.Background(), directdelivery.Request{
		Worker: worker, Repo: "acme/demo", Issue: "9", BaseBranch: "pre-prod",
		OwnedPaths: owned, RequiredChecks: []string{"verify", "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fresh fakes for second execute (stores are per-service-deps instance).
	svc2 := directdelivery.Service{Deps: testDeps(t, now, baseSHA, owned)}
	b, err := svc2.Execute(context.Background(), directdelivery.Request{
		Worker: worker, Repo: "acme/demo", Issue: "9", BaseBranch: "pre-prod",
		OwnedPaths: owned, RequiredChecks: []string{"verify", "test"},
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
