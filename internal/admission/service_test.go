package admission_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/admission"
	"github.com/jasonhnd/loopcoder/internal/home"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
}

func openSvc(t *testing.T, live admission.LivenessProbe, now func() time.Time) *admission.Service {
	t.Helper()
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "home"), "")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := layout.OpenMachine(ctx, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ms.Close() })
	if err := admission.Ensure(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if now == nil {
		now = fixedNow
	}
	if live == nil {
		live = admission.StaticLivenessProbe{Live: false, Unknown: false}
	}
	svc, err := admission.New(ms, admission.Options{Now: now, Live: live})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func baseReq(role admission.Role, project, key string) admission.Request {
	return admission.Request{
		ProjectID:      project,
		JobID:          "job-1",
		AttemptID:      "att-1",
		Role:           role,
		Processes:      1,
		RSSBytes:       64 << 20,
		CPURate:        0.2,
		IdempotencyKey: key,
		LeaseTTL:       time.Minute,
	}
}

func TestClaimAdmitAndIdempotentReplay(t *testing.T) {
	ctx := context.Background()
	svc := openSvc(t, nil, nil)
	d, err := svc.Claim(ctx, baseReq(admission.RoleWorker, "proj-a", "idem-1"))
	if err != nil || !d.Admitted {
		t.Fatalf("claim: admitted=%v err=%v reasons=%v", d.Admitted, err, d.Reasons)
	}
	if d.ReservationID == "" || d.Generation != 1 {
		t.Fatalf("id/gen: %#v", d)
	}
	if d.SchemaVersion != admission.SchemaVersion {
		t.Fatalf("schema %s", d.SchemaVersion)
	}
	// Explainable views present.
	if d.Requested.Workers != 1 || d.Reserved.Workers != 1 {
		t.Fatalf("views: req=%#v res=%#v", d.Requested, d.Reserved)
	}

	d2, err := svc.Claim(ctx, baseReq(admission.RoleWorker, "proj-a", "idem-1"))
	if err != nil || !d2.Admitted || !d2.Replay {
		t.Fatalf("replay: %#v err=%v", d2, err)
	}
	if d2.ReservationID != d.ReservationID {
		t.Fatalf("replay id mismatch")
	}
}

func TestConcurrentWorkersRespectBudget(t *testing.T) {
	ctx := context.Background()
	svc := openSvc(t, nil, nil)

	var wg sync.WaitGroup
	const n = 8
	results := make([]admission.Decision, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.Claim(ctx, baseReq(admission.RoleWorker, "proj-"+string(rune('a'+i)), "idem-w-"+string(rune('0'+i))))
		}(i)
	}
	wg.Wait()

	admitted := 0
	for i := 0; i < n; i++ {
		if results[i].Admitted {
			admitted++
			if errs[i] != nil {
				t.Fatalf("admitted with err: %v", errs[i])
			}
		} else if !errors.Is(errs[i], admission.ErrDenied) {
			t.Fatalf("deny err: %v reasons=%v", errs[i], results[i].Reasons)
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted=%d want 1", admitted)
	}
}

func TestVerifierBlockedWhileWorkerActive(t *testing.T) {
	ctx := context.Background()
	svc := openSvc(t, nil, nil)
	if _, err := svc.Claim(ctx, baseReq(admission.RoleWorker, "proj-a", "w1")); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Claim(ctx, baseReq(admission.RoleVerifier, "proj-b", "v1"))
	if d.Admitted || !errors.Is(err, admission.ErrDenied) {
		t.Fatalf("expected deny: %#v err=%v", d, err)
	}
	found := false
	for _, r := range d.Reasons {
		if r == "verifier_blocked_while_worker_active" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons %v", d.Reasons)
	}
}

func TestRenewGenerationFenceAndRelease(t *testing.T) {
	ctx := context.Background()
	svc := openSvc(t, nil, nil)
	d, err := svc.Claim(ctx, baseReq(admission.RoleLocalTest, "proj-a", "t1"))
	if err != nil {
		t.Fatal(err)
	}
	r1, err := svc.Renew(ctx, d.ReservationID, d.Generation, time.Minute)
	if err != nil || r1.Generation != 2 {
		t.Fatalf("renew: %#v err=%v", r1, err)
	}
	if _, err := svc.Renew(ctx, d.ReservationID, 1, time.Minute); !errors.Is(err, admission.ErrGenerationMismatch) {
		t.Fatalf("stale gen: %v", err)
	}
	// Second local_test blocked until release.
	d2, err := svc.Claim(ctx, baseReq(admission.RoleLocalTest, "proj-b", "t2"))
	if d2.Admitted {
		t.Fatal("expected second local_test denied")
	}
	_ = err
	rel, err := svc.Release(ctx, d.ReservationID, r1.Generation)
	if err != nil || rel.State != admission.StateReleased {
		t.Fatalf("release: %#v err=%v", rel, err)
	}
	// Idempotent release.
	rel2, err := svc.Release(ctx, d.ReservationID, r1.Generation)
	if err != nil || rel2.State != admission.StateReleased {
		t.Fatalf("release2: %#v err=%v", rel2, err)
	}
	d3, err := svc.Claim(ctx, baseReq(admission.RoleLocalTest, "proj-b", "t2"))
	if err != nil || !d3.Admitted {
		t.Fatalf("after release: %#v err=%v", d3, err)
	}
}

func TestExpiredUnknownLivenessAttentionRequired(t *testing.T) {
	ctx := context.Background()
	clock := fixedNow()
	svc := openSvc(t, admission.AlwaysUnknownProbe{}, func() time.Time { return clock })

	d, err := svc.Claim(ctx, admission.Request{
		ProjectID: "proj-a", Role: admission.RoleWorker, Processes: 1,
		IdempotencyKey: "exp-1", LeaseTTL: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Advance past lease.
	clock = clock.Add(2 * time.Second)
	// Next claim should not steal capacity; attention holds worker slot.
	d2, err := svc.Claim(ctx, admission.Request{
		ProjectID: "proj-b", Role: admission.RoleWorker, Processes: 1,
		IdempotencyKey: "exp-2", LeaseTTL: time.Minute,
	})
	if d2.Admitted {
		t.Fatalf("should not reassign: %#v err=%v", d2, err)
	}
	if err != nil && !errors.Is(err, admission.ErrDenied) {
		t.Fatalf("unexpected claim err: %v", err)
	}
	got, ok, err := svc.Get(ctx, d.ReservationID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.State != admission.StateAttentionRequired {
		t.Fatalf("state=%s reason=%s", got.State, got.AttentionReason)
	}
	// Unknown liveness stays attention_required under resolve without release.
	_, err = svc.ResolveAttention(ctx, d.ReservationID, false)
	if !errors.Is(err, admission.ErrAttentionRequired) {
		t.Fatalf("unknown resolve: %v", err)
	}
}

func TestExpiredDeadProcessFreesBudget(t *testing.T) {
	ctx := context.Background()
	clock := fixedNow()
	svc := openSvc(t, admission.StaticLivenessProbe{Live: false, Unknown: false}, func() time.Time { return clock })
	d, err := svc.Claim(ctx, admission.Request{
		ProjectID: "proj-a", Role: admission.RoleWorker, Processes: 1,
		IdempotencyKey: "dead-1", LeaseTTL: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Second)
	d2, err := svc.Claim(ctx, admission.Request{
		ProjectID: "proj-b", Role: admission.RoleWorker, Processes: 1,
		IdempotencyKey: "dead-2", LeaseTTL: time.Minute,
	})
	if err != nil || !d2.Admitted {
		t.Fatalf("should admit after dead expire: %#v err=%v", d2, err)
	}
	got, ok, _ := svc.Get(ctx, d.ReservationID)
	if !ok || got.State != admission.StateExpired {
		t.Fatalf("expired state=%v ok=%v", got.State, ok)
	}
}

func TestObserveEmitsOneEnforcementTransition(t *testing.T) {
	ctx := context.Background()
	svc := openSvc(t, nil, nil)
	d, err := svc.Claim(ctx, baseReq(admission.RoleWorker, "proj-a", "obs-1"))
	if err != nil {
		t.Fatal(err)
	}
	use := admission.ObservedUse{ProcessCount: 5, RSSBytes: 1 << 30, CPURate: 2.0}
	e1, err := svc.Observe(ctx, d.ReservationID, use)
	if err != nil {
		t.Fatal(err)
	}
	if len(e1) == 0 {
		t.Fatal("expected enforcement requests")
	}
	e2, err := svc.Observe(ctx, d.ReservationID, use)
	if err != nil {
		t.Fatal(err)
	}
	if len(e2) != 0 {
		t.Fatalf("duplicate transitions: %#v", e2)
	}
}

func TestProcessBudgetExhausted(t *testing.T) {
	ctx := context.Background()
	layout, err := home.EnsureMinimumLayout(filepath.Join(t.TempDir(), "home"), "")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := layout.OpenMachine(ctx, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	defer ms.Close()
	_ = admission.Ensure(ctx, ms)
	// Tiny process budget.
	svc, err := admission.New(ms, admission.Options{
		Budget: admission.Budget{
			MaxActiveWorkers:  4,
			MaxLocalTests:     4,
			MaxChildProcesses: 2,
			MaxRSSBytes:       admission.DefaultMaxRSSBytes,
			MaxCPURate:        admission.DefaultMaxCPURate,
		},
		Now:  fixedNow,
		Live: admission.StaticLivenessProbe{},
	})
	if err != nil {
		t.Fatal(err)
	}
	d1, err := svc.Claim(ctx, admission.Request{
		ProjectID: "p1", Role: admission.RoleWorker, Processes: 2, IdempotencyKey: "p1",
	})
	if err != nil || !d1.Admitted {
		t.Fatalf("%#v %v", d1, err)
	}
	d2, err := svc.Claim(ctx, admission.Request{
		ProjectID: "p2", Role: admission.RoleWorker, Processes: 1, IdempotencyKey: "p2",
	})
	if d2.Admitted || !errors.Is(err, admission.ErrDenied) {
		t.Fatalf("want process deny: %#v err=%v", d2, err)
	}
	// Decision must explain denied processes without foreign project content.
	if d2.Denied.Processes < 1 {
		t.Fatalf("denied view: %#v", d2.Denied)
	}
	for _, r := range d2.Reasons {
		if r == "process_budget_exhausted" {
			return
		}
	}
	t.Fatalf("reasons %v", d2.Reasons)
}

func TestFailedClaimNoPartialConsumption(t *testing.T) {
	ctx := context.Background()
	svc := openSvc(t, nil, nil)
	// Invalid role-less: use empty project
	_, err := svc.Claim(ctx, admission.Request{Role: admission.RoleWorker, IdempotencyKey: "x"})
	if !errors.Is(err, admission.ErrInvalidRequest) {
		t.Fatalf("err=%v", err)
	}
	// Budget still free.
	d, err := svc.Claim(ctx, baseReq(admission.RoleWorker, "proj-a", "ok"))
	if err != nil || !d.Admitted {
		t.Fatalf("%#v %v", d, err)
	}
	if d.Reserved.Workers != 1 {
		t.Fatalf("reserved %#v", d.Reserved)
	}
}
