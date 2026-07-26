package providerexec_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerexec"
)

func baseReq() providerexec.Request {
	return providerexec.Request{
		RequestID: "req1", ProjectID: "p", AttemptID: "a",
		Route:   providerexec.Route{Provider: "fixture", Model: "m0", Effort: "low", Permission: "default"},
		Timeout: 5 * time.Second,
	}
}

func TestFakeAndReferenceSameEnvelope(t *testing.T) {
	req, err := providerexec.NewRequest(baseReq())
	if err != nil {
		t.Fatal(err)
	}
	fake := providerexec.NewFake()
	ref := providerexec.NewReferenceFixture()
	o1, err := fake.Execute(context.Background(), req)
	if err != nil || o1.Failure != "" {
		t.Fatalf("fake %+v err=%v", o1, err)
	}
	o2, err := ref.Execute(context.Background(), req)
	if err != nil || o2.Failure != "" {
		t.Fatalf("ref %+v err=%v", o2, err)
	}
	if o1.RequestedRoute != o2.RequestedRoute {
		t.Fatal("requested route diverge")
	}
	if o1.ActualRoute != req.Route || o2.ActualRoute != req.Route {
		t.Fatal("actual must match request")
	}
	if o1.RouteDigest != req.RouteDigest || o2.RouteDigest != req.RouteDigest {
		t.Fatal("digest")
	}
	if o1.Process.Adapter == "" || o2.Process.Adapter == "" {
		t.Fatal("process evidence")
	}
}

func TestRouteMismatchAndUnsupported(t *testing.T) {
	fake := providerexec.NewFake()
	req := baseReq()
	req.Route.Model = "nope"
	req, err := providerexec.NewRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	o, err := fake.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if o.Failure != providerexec.FailUnsupported {
		t.Fatalf("%+v", o)
	}
	// native delegation rejected at NewRequest
	bad := baseReq()
	bad.Route.NativeDelegation = true
	if _, err := providerexec.NewRequest(bad); !errors.Is(err, providerexec.ErrUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestTypedFailures(t *testing.T) {
	cases := []providerexec.FailureClass{
		providerexec.FailTimeout, providerexec.FailAuth, providerexec.FailRateLimit,
		providerexec.FailMalformed, providerexec.FailProcess,
	}
	for _, fc := range cases {
		f := providerexec.NewFake()
		f.Behavior = fc
		req, _ := providerexec.NewRequest(baseReq())
		o, err := f.Execute(context.Background(), req)
		if err != nil || o.Failure != fc {
			t.Fatalf("want %s got %+v err=%v", fc, o, err)
		}
	}
}

func TestCancel(t *testing.T) {
	f := providerexec.NewFake()
	f.Delay = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	req, _ := providerexec.NewRequest(baseReq())
	o, err := f.Execute(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if o.Failure != providerexec.FailCancelled {
		t.Fatalf("%+v", o)
	}
}

func TestImmutableRequestClone(t *testing.T) {
	req, _ := providerexec.NewRequest(baseReq())
	cp := req.Clone()
	cp.Labels = map[string]string{"x": "y"}
	cp.Route.Model = "mutated"
	if req.Route.Model == "mutated" {
		t.Fatal("request mutated")
	}
}

func TestInventoryPublished(t *testing.T) {
	doc := providerexec.PublishInventory()
	if doc.Schema != providerexec.SchemaInventory || len(doc.Entries) < 4 {
		t.Fatalf("%+v", doc)
	}
	found := false
	for _, e := range doc.Entries {
		if e.Provider == "codex" && e.Class == providerexec.ClassReferenceReady {
			found = true
		}
	}
	if !found {
		t.Fatal("codex reference_ready missing")
	}
}

func TestReferenceLaunchPath(t *testing.T) {
	ref := providerexec.NewReferenceFixture()
	ref.Launch = func(ctx context.Context, req providerexec.Request, cmd string) (providerexec.ProcessEvidence, int, error) {
		return providerexec.ProcessEvidence{
			PID: 42, StartedAt: time.Now().UTC(), Command: cmd,
			Adapter: "agent-fixture-ref", Version: "1",
		}, 0, nil
	}
	req, _ := providerexec.NewRequest(baseReq())
	o, err := ref.Execute(context.Background(), req)
	if err != nil || o.Process.PID != 42 || o.Failure != "" {
		t.Fatalf("%+v err=%v", o, err)
	}
}
