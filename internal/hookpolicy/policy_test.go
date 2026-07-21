package hookpolicy_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/hookpolicy"
)

func TestDefaultRespectsHooks(t *testing.T) {
	disc := hookpolicy.DiscoverPreflight([]string{"hooks/pre-commit"}, nil)
	p, err := hookpolicy.Freeze(hookpolicy.ModeRespect, false, "", disc, []string{"a.go"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := &hookpolicy.Runner{
		Exec: func(ctx context.Context, hook string, env []string) (int, []byte, time.Duration, bool, error) {
			for _, e := range env {
				if e == "SECRET_TOKEN=x" {
					t.Fatal("secret env leaked")
				}
			}
			return 0, []byte("ok"), time.Millisecond, false, nil
		},
		ScrubEnv: hookpolicy.DefaultScrub,
	}
	res, err := r.Reconcile(context.Background(), p, "commit", []string{"PATH=/bin", "SECRET_TOKEN=x"})
	if err != nil || len(res) == 0 || res[0].Outcome != "pass" {
		t.Fatalf("%+v err=%v", res, err)
	}
	if res[0].OutputDigest == "" {
		t.Fatal("need output digest not raw")
	}
}

func TestBypassRequiresAuth(t *testing.T) {
	disc := hookpolicy.DiscoverPreflight([]string{"pre-commit"}, nil)
	_, err := hookpolicy.Freeze(hookpolicy.ModeApprovedBypass, false, "owner", disc, nil, time.Now())
	if !errors.Is(err, hookpolicy.ErrBypassDenied) {
		t.Fatalf("err=%v", err)
	}
	p, err := hookpolicy.Freeze(hookpolicy.ModeApprovedBypass, true, "owner approved no-verify", disc, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := &hookpolicy.Runner{}
	res, err := r.Reconcile(context.Background(), p, "commit", nil)
	if err != nil || !res[0].VisibleBypass || res[0].Outcome != "skipped_bypass" {
		t.Fatalf("%+v err=%v", res, err)
	}
	ev := hookpolicy.Bundle(p, res)
	if !ev.VisibleBypass || ev.BypassReason == "" {
		t.Fatal(ev)
	}
}

func TestTimeoutAndMutationBlock(t *testing.T) {
	disc := hookpolicy.DiscoverPreflight([]string{"pre-commit"}, nil)
	p, _ := hookpolicy.Freeze(hookpolicy.ModeRespect, false, "", disc, []string{"a.go"}, time.Now())
	r := &hookpolicy.Runner{
		Exec: func(ctx context.Context, hook string, env []string) (int, []byte, time.Duration, bool, error) {
			return 0, nil, time.Second, false, context.DeadlineExceeded
		},
	}
	_, err := r.Reconcile(context.Background(), p, "commit", nil)
	if !errors.Is(err, hookpolicy.ErrTimeout) {
		t.Fatalf("err=%v", err)
	}
	r.Exec = func(ctx context.Context, hook string, env []string) (int, []byte, time.Duration, bool, error) {
		return 0, []byte("x"), time.Millisecond, true, nil
	}
	_, err = r.Reconcile(context.Background(), p, "commit", nil)
	if !errors.Is(err, hookpolicy.ErrMutation) {
		t.Fatalf("err=%v", err)
	}
}

func TestNoInferBypassFromProse(t *testing.T) {
	if hookpolicy.InferBypassFromProse("recovery should use --no-verify") {
		t.Fatal("must not infer")
	}
}

func TestUnsupportedBlocks(t *testing.T) {
	p, _ := hookpolicy.Freeze(hookpolicy.ModeUnsupported, false, "", hookpolicy.Discovery{}, nil, time.Now())
	r := &hookpolicy.Runner{}
	res, err := r.Reconcile(context.Background(), p, "commit", nil)
	if !errors.Is(err, hookpolicy.ErrUnsupported) || !res[0].BlocksDelivery {
		t.Fatalf("%+v err=%v", res, err)
	}
}
