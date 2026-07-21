package geminiexec_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/geminiexec"
	"github.com/jasonhnd/loopcoder/internal/providerexec"
)

func baseReq() providerexec.Request {
	req, err := providerexec.NewRequest(providerexec.Request{
		RequestID: "r1", ProjectID: "p", AttemptID: "a",
		Route:   providerexec.Route{Provider: "gemini", Model: "flash", Effort: "low", Permission: "default"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		panic(err)
	}
	return req
}

func TestPlanAndSuccess(t *testing.T) {
	a := &geminiexec.Adapter{Planner: geminiexec.Planner{Caps: geminiexec.DefaultCaps()}}
	plan, err := a.Planner.Plan(baseReq())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Model != "gemini-2.5-flash" || plan.Binary != "gemini" || !plan.EnvScrubbed {
		t.Fatalf("%+v", plan)
	}
	if !contains(plan.Args, "gemini-2.5-flash") {
		t.Fatalf("args=%v", plan.Args)
	}
	out, err := a.Execute(context.Background(), baseReq())
	if err != nil || out.Failure != "" {
		t.Fatalf("%+v err=%v", out, err)
	}
	if out.ActualRoute.Model != "gemini-2.5-flash" || out.ExitCode != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestUnsupportedModelAndEffort(t *testing.T) {
	a := &geminiexec.Adapter{Planner: geminiexec.Planner{Caps: geminiexec.DefaultCaps()}}
	req := baseReq()
	req.Route.Model = "nope-model"
	_, err := a.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected unsupported model")
	}
	req = baseReq()
	req.Route.Effort = "ultra"
	_, err = a.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected unsupported effort")
	}
}

func TestTypedFailures(t *testing.T) {
	cases := map[string]providerexec.FailureClass{
		"timeout":    providerexec.FailTimeout,
		"auth":       providerexec.FailAuth,
		"rate_limit": providerexec.FailRateLimit,
		"cancel":     providerexec.FailCancelled,
		"malformed":  providerexec.FailMalformed,
		"flood":      providerexec.FailMalformed,
		"nonzero":    providerexec.FailProcess,
		"escape":     providerexec.FailProcess,
		"mismatch":   providerexec.FailRouteMismatch,
	}
	for mode, want := range cases {
		a := &geminiexec.Adapter{Planner: geminiexec.Planner{Caps: geminiexec.DefaultCaps()}, Mode: mode}
		out, _ := a.Execute(context.Background(), baseReq())
		if out.Failure != want {
			t.Fatalf("%s: got %s want %s msg=%s", mode, out.Failure, want, out.Message)
		}
	}
}

func TestNotInstalled(t *testing.T) {
	caps := geminiexec.DefaultCaps()
	caps.Installed = false
	a := &geminiexec.Adapter{Planner: geminiexec.Planner{Caps: caps}}
	_, err := a.Execute(context.Background(), baseReq())
	if err == nil {
		t.Fatal("expected fail")
	}
}

func TestImmutableRetryIgnoresNewCatalog(t *testing.T) {
	a := &geminiexec.Adapter{Planner: geminiexec.Planner{Caps: geminiexec.DefaultCaps()}}
	p1, p2, err := geminiexec.ImmutableRetry(a, baseReq())
	if err != nil {
		t.Fatal(err)
	}
	if p1.Model != "gemini-2.5-flash" || p2.Model != "gemini-2.5-flash" {
		t.Fatalf("p1=%+v p2=%+v", p1, p2)
	}
	if p2.Model == "new-model" {
		t.Fatal("reinterpreted via new catalog")
	}
}

func TestScrubEnv(t *testing.T) {
	in := []string{"PATH=/bin", "GEMINI_MODEL=evil", "GITHUB_TOKEN=x", "FOO=1"}
	out := geminiexec.ScrubEnv(in)
	for _, e := range out {
		if strings.Contains(e, "GEMINI_MODEL") || strings.Contains(e, "TOKEN") {
			t.Fatalf("%v", out)
		}
	}
}

func TestProviderMismatch(t *testing.T) {
	a := &geminiexec.Adapter{Planner: geminiexec.Planner{Caps: geminiexec.DefaultCaps()}}
	req := baseReq()
	req.Route.Provider = "other"
	req.RouteDigest = req.Route.Digest()
	_, err := a.Execute(context.Background(), req)
	if err == nil {
		t.Fatal("expected provider rejection")
	}
	if !errors.Is(err, providerexec.ErrUnsupported) {
		t.Logf("err=%v", err)
	}
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
