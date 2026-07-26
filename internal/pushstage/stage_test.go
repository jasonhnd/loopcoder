package pushstage_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/pushstage"
)

func t0() time.Time { return time.Date(2026, 7, 23, 3, 0, 0, 0, time.UTC) }

func intent(newOID string) pushstage.Intent {
	return pushstage.Intent{
		AttemptID: "att1", IdempotencyKey: "push-1",
		RemoteName: "origin", Branch: "ordinary/issue-1133",
		ExpectedOldOID: "", ExpectedNewOID: newOID,
		CommitReceiptKey: "c1", HookReceiptOK: true, CommitReceiptOK: true,
		RemoteURLDigest: "sha256:deadbeefdeadbeef",
	}
}

func TestFirstPushAndIdempotentAdopt(t *testing.T) {
	rem := pushstage.NewFakeRemote()
	svc := &pushstage.Service{Store: pushstage.NewStore(t0), Remote: rem}
	in, err := svc.Freeze(intent("commitsha0001"))
	if err != nil {
		t.Fatal(err)
	}
	r1, err := svc.PushOrAdopt(context.Background(), in.IdempotencyKey)
	if err != nil || !r1.OK || r1.Receipt == nil || r1.Receipt.NewOID != "commitsha0001" {
		t.Fatalf("%+v err=%v", r1, err)
	}
	r2, err := svc.PushOrAdopt(context.Background(), in.IdempotencyKey)
	if err != nil || !r2.Adopted || r2.Receipt.NewOID != r1.Receipt.NewOID {
		t.Fatalf("retry %+v err=%v", r2, err)
	}
}

func TestTimeoutAfterApplyAdopts(t *testing.T) {
	rem := pushstage.NewFakeRemote()
	svc := &pushstage.Service{
		Store: pushstage.NewStore(t0), Remote: rem,
		FailPushWith: pushstage.ErrTimeout, AfterFailApplied: true,
	}
	in, _ := svc.Freeze(intent("commitsha0002"))
	r, err := svc.PushOrAdopt(context.Background(), in.IdempotencyKey)
	if err != nil || !r.OK || r.Reconcile != pushstage.ReconcileApplied {
		t.Fatalf("%+v err=%v", r, err)
	}
}

func TestTimeoutBeforeApplyNotApplied(t *testing.T) {
	rem := pushstage.NewFakeRemote()
	svc := &pushstage.Service{
		Store: pushstage.NewStore(t0), Remote: rem,
		FailPushWith: pushstage.ErrTimeout, AfterFailApplied: false,
	}
	in, _ := svc.Freeze(intent("commitsha0003"))
	r, err := svc.PushOrAdopt(context.Background(), in.IdempotencyKey)
	if !errors.Is(err, pushstage.ErrTimeout) || r.Reconcile != pushstage.ReconcileNotApplied {
		t.Fatalf("%+v err=%v", r, err)
	}
}

func TestConflictAndAuthAndRateLimit(t *testing.T) {
	rem := pushstage.NewFakeRemote()
	rem.SetRef("origin", "ordinary/issue-1133", "othercommit")
	svc := &pushstage.Service{Store: pushstage.NewStore(t0), Remote: rem}
	in := intent("commitsha0004")
	in.ExpectedOldOID = "expectedold"
	_, _ = svc.Freeze(in)
	r, err := svc.PushOrAdopt(context.Background(), in.IdempotencyKey)
	if r.Failure != pushstage.FailConflict && r.Failure != pushstage.FailNonFastForward {
		t.Fatalf("%+v err=%v", r, err)
	}

	rem2 := pushstage.NewFakeRemote()
	rem2.SetAuth(false)
	svc2 := &pushstage.Service{Store: pushstage.NewStore(t0), Remote: rem2}
	in2 := intent("commitsha0005")
	in2.IdempotencyKey = "push-auth"
	_, _ = svc2.Freeze(in2)
	r, err = svc2.PushOrAdopt(context.Background(), in2.IdempotencyKey)
	if r.Failure != pushstage.FailAuth {
		t.Fatalf("%+v err=%v", r, err)
	}

	rem3 := pushstage.NewFakeRemote()
	rem3.SetRateLimited(true)
	svc3 := &pushstage.Service{Store: pushstage.NewStore(t0), Remote: rem3}
	in3 := intent("commitsha0006")
	in3.IdempotencyKey = "push-rl"
	_, _ = svc3.Freeze(in3)
	r, err = svc3.PushOrAdopt(context.Background(), in3.IdempotencyKey)
	if r.Failure != pushstage.FailRateLimit {
		t.Fatalf("%+v err=%v", r, err)
	}
}

func TestRequiresReceiptsNoCredentialsInReceipt(t *testing.T) {
	svc := &pushstage.Service{Store: pushstage.NewStore(t0), Remote: pushstage.NewFakeRemote()}
	in := intent("commitsha0007")
	in.CommitReceiptOK = false
	if _, err := svc.Freeze(in); !errors.Is(err, pushstage.ErrNotReady) {
		t.Fatalf("err=%v", err)
	}
	in.CommitReceiptOK = true
	in.HookReceiptOK = false
	if _, err := svc.Freeze(in); !errors.Is(err, pushstage.ErrNotReady) {
		t.Fatalf("err=%v", err)
	}
	in.HookReceiptOK = true
	in.RemoteURLDigest = "https://user:ghp_SECRET@github.com/x/y.git"
	if _, err := svc.Freeze(in); err == nil {
		t.Fatal("must reject credential URL")
	}
}

func TestScrubEnv(t *testing.T) {
	out := pushstage.ScrubEnv([]string{"PATH=/bin", "GH_TOKEN=x", "GIT_DIR=/evil", "HOME=/u"})
	j := strings.Join(out, ",")
	if strings.Contains(j, "TOKEN") || strings.Contains(j, "GIT_DIR") {
		t.Fatal(out)
	}
}

func TestAlreadyRemoteAdoptsWithoutPush(t *testing.T) {
	rem := pushstage.NewFakeRemote()
	rem.SetRef("origin", "ordinary/issue-1133", "commitsha0008")
	svc := &pushstage.Service{Store: pushstage.NewStore(t0), Remote: rem}
	in := intent("commitsha0008")
	in.IdempotencyKey = "push-exist"
	_, _ = svc.Freeze(in)
	r, err := svc.PushOrAdopt(context.Background(), in.IdempotencyKey)
	if err != nil || !r.OK || r.Receipt.NewOID != "commitsha0008" {
		t.Fatalf("%+v err=%v", r, err)
	}
}
