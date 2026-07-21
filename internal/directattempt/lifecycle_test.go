package directattempt_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/directattempt"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
	"github.com/jasonhnd/loopcoder/internal/routepin"
	"github.com/jasonhnd/loopcoder/internal/uisub"
)

func t0() time.Time { return time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC) }

func setup(t *testing.T) (*directattempt.Engine, routepin.Fields, string) {
	t.Helper()
	pins := routepin.NewStore(t0, func(string, string) bool { return true })
	fields := routepin.Fields{Provider: "fixture", Model: "m0", Effort: "low", Permission: "default", SubagentPolicy: routepin.SubagentForbidden}
	pin, err := pins.Persist("proj", "att1", fields)
	if err != nil {
		t.Fatal(err)
	}
	pin, _ = pins.Acknowledge(pin.PinID)
	ledger := uisub.NewLedger("proj", 32, t0)
	_ = ledger.RegisterClient(uisub.ClientIdentity{ClientID: "term", SessionID: "s", ProjectID: "proj", Required: true})
	launches := 0
	eng := &directattempt.Engine{
		Attempts: directattempt.NewStore(t0),
		Pins:     pins,
		Ledger:   ledger,
		Provider: func(ctx context.Context, req providerexec.Request) (providerexec.Outcome, error) {
			launches++
			return providerexec.Outcome{ExitCode: 0, RequestID: req.RequestID}, nil
		},
		Reserve: func(string) error { return nil },
		Release: func(string) error { return nil },
	}
	_, err = eng.Attempts.Create("proj", "run1", "att1", pin.Digest, "/wt/att1", "deadbeef", "idem-1")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = eng.Attempts.Admit("att1")
	return eng, fields, pin.Digest
}

func TestLaunchOnceAfterStartRendered(t *testing.T) {
	eng, fields, digest := setup(t)
	env, err := eng.PrepareStart("att1", "proj", 1)
	if err != nil {
		t.Fatal(err)
	}
	// without rendered
	_, err = eng.TryLaunch(context.Background(), directattempt.LaunchBundle{
		AttemptID: "att1", Route: fields, RouteDigest: digest,
		WorktreePath: "/wt/att1", BaseSHA: "deadbeef", IdempotencyKey: "idem-1",
		StartEventID: env.EventID, StartDigest: env.ContentDigest, RequiredClient: "term",
	})
	if !errors.Is(err, directattempt.ErrNotReady) {
		t.Fatalf("err=%v", err)
	}
	// render ack
	_ = eng.Ledger.Acknowledge(uisub.Ack{
		ClientID: "term", EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
	})
	a, err := eng.TryLaunch(context.Background(), directattempt.LaunchBundle{
		AttemptID: "att1", Route: fields, RouteDigest: digest,
		WorktreePath: "/wt/att1", BaseSHA: "deadbeef", IdempotencyKey: "idem-1",
		StartEventID: env.EventID, StartDigest: env.ContentDigest, RequiredClient: "term",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !a.ProviderLaunched || a.State != directattempt.StateProcessTerminal {
		// provider exits immediately in fake
		if a.State != directattempt.StateProcessTerminal && a.State != directattempt.StateRunning {
			t.Fatalf("%+v", a)
		}
	}
	// second launch blocked
	_, err = eng.TryLaunch(context.Background(), directattempt.LaunchBundle{
		AttemptID: "att1", Route: fields, RouteDigest: digest,
		WorktreePath: "/wt/att1", BaseSHA: "deadbeef", IdempotencyKey: "idem-1",
		StartEventID: env.EventID, StartDigest: env.ContentDigest, RequiredClient: "term",
	})
	if !errors.Is(err, directattempt.ErrDuplicateLaunch) && !errors.Is(err, directattempt.ErrNotReady) {
		t.Fatalf("err=%v", err)
	}
}

func TestExitNotCompletionWithoutCleanup(t *testing.T) {
	eng, fields, digest := setup(t)
	env, _ := eng.PrepareStart("att1", "proj", 1)
	_ = eng.Ledger.Acknowledge(uisub.Ack{
		ClientID: "term", EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
	})
	_, err := eng.TryLaunch(context.Background(), directattempt.LaunchBundle{
		AttemptID: "att1", Route: fields, RouteDigest: digest,
		WorktreePath: "/wt/att1", BaseSHA: "deadbeef", IdempotencyKey: "idem-1",
		StartEventID: env.EventID, StartDigest: env.ContentDigest, RequiredClient: "term",
	})
	if err != nil {
		t.Fatal(err)
	}
	// incomplete cleanup
	_, err = eng.Attempts.CompleteCleanup("att1", true, false, true)
	if err == nil {
		t.Fatal("expected incomplete cleanup error")
	}
	a, err := eng.FinishCleanup("att1")
	if err != nil || a.State != directattempt.StateCleanupTerminal {
		t.Fatalf("%+v err=%v", a, err)
	}
	if !a.OutputFlushed || !a.ChildrenJoined || !a.ReservationReleased {
		t.Fatalf("%+v", a)
	}
}

func TestDigestMismatchBlocks(t *testing.T) {
	eng, fields, digest := setup(t)
	env, _ := eng.PrepareStart("att1", "proj", 1)
	_ = eng.Ledger.Acknowledge(uisub.Ack{
		ClientID: "term", EventID: env.EventID, Digest: env.ContentDigest, Stage: uisub.StageRendered,
	})
	_, err := eng.TryLaunch(context.Background(), directattempt.LaunchBundle{
		AttemptID: "att1", Route: fields, RouteDigest: digest,
		WorktreePath: "/wt/WRONG", BaseSHA: "deadbeef", IdempotencyKey: "idem-1",
		StartEventID: env.EventID, StartDigest: env.ContentDigest, RequiredClient: "term",
	})
	if !errors.Is(err, directattempt.ErrDigestMismatch) {
		t.Fatalf("err=%v", err)
	}
}
