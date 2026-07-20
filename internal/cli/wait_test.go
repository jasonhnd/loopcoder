package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/detachedrun"
	"github.com/jasonhnd/loopcoder/internal/progress"
	"github.com/jasonhnd/loopcoder/internal/registry"
	"github.com/jasonhnd/loopcoder/internal/runtimepath"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestWaitQuotaResetRequiresUntil(t *testing.T) {
	var stderr bytes.Buffer
	exit := RunWithDeps([]string{"wait", "quota-reset"}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC) },
	})
	if exit != 2 || !strings.Contains(stderr.String(), "--until") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
}

func TestWaitQuotaResetRejectsPastUntil(t *testing.T) {
	var stderr bytes.Buffer
	past := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	exit := RunWithDeps([]string{"wait", "quota-reset", "--until", past}, &bytes.Buffer{}, &stderr, Deps{
		Now: func() time.Time { return time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC) },
	})
	if exit != 2 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
}

func TestWaitApprovalApproveAndRejectPathsZeroProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	repo := t.TempDir()
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	registered, err := registry.Register(ctx, registry.Options{RepoPath: repo, Now: func() time.Time { return now }}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	roots, err := runtimepath.Resolve(ctx, repo)
	if err != nil || !roots.Registered {
		t.Fatalf("Resolve: %#v %v", roots, err)
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	actor := delivery.Actor{ActorKind: "system", ActorID: "test", DecisionAuthority: "test", Source: "test"}
	host := delivery.Host{HostKind: "cli", HostID: "test"}
	run, err := delivery.PersistDeliveryRun(ctx, store, delivery.DeliveryRun{
		DeliveryRunID:            "run-approval-wait",
		ProjectID:                registered.Project.ProjectID,
		State:                    delivery.RunAwaitingApproval,
		IntentSummary:            "wait approval",
		InputFingerprint:         "sha256:in",
		PolicyFingerprint:        "sha256:pol",
		PlanFingerprint:          "sha256:plan",
		AuthorizationFingerprint: "sha256:auth",
		PolicyVersion:            "v1",
		MaxSideEffectClass:       "provider_launch",
		ApprovalStatus:           "required",
		OverrideStatus:           "none",
		CreatedBy:                actor,
		UpdatedBy:                actor,
		Host:                     host,
	}, delivery.PersistOptions{Now: now, IdempotencyKey: "approval-wait-seed"})
	if err != nil {
		t.Fatalf("PersistDeliveryRun: %v", err)
	}

	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delivery_runs SET approval_status = 'rejected' WHERE delivery_run_id = ?`, run.DeliveryRunID)
		return err
	}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	clock := &waitFakeClock{now: now}
	var stdout, stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"wait", "approval", "--repo", repo, "--run", run.DeliveryRunID, "--format", "json", "--timeout", "1m",
	}, &stdout, &stderr, Deps{Now: func() time.Time { return clock.Now() }, WaitClock: clock})
	if exit != 2 {
		t.Fatalf("reject exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("json: %v body=%s", err, stdout.String())
	}
	if payload["provider_calls"] != float64(0) {
		t.Fatalf("provider_calls=%v", payload["provider_calls"])
	}

	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE delivery_runs SET approval_status = 'approved' WHERE delivery_run_id = ?`, run.DeliveryRunID)
		return err
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	exit = RunWithDeps([]string{
		"wait", "approval", "--repo", repo, "--run", run.DeliveryRunID, "--format", "json", "--timeout", "1m",
		"--wait-id", "approval-restart-1",
	}, &stdout, &stderr, Deps{Now: func() time.Time { return clock.Now() }, WaitClock: clock})
	if exit != 0 {
		t.Fatalf("approve exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
}

func TestWaitOutboxTerminalFailureZeroProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	repo := t.TempDir()
	now := time.Date(2026, 7, 18, 13, 0, 0, 0, time.UTC)
	ctx := context.Background()
	registered, err := registry.Register(ctx, registry.Options{RepoPath: repo, Now: func() time.Time { return now }}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	roots, err := runtimepath.Resolve(ctx, repo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	receipt := progress.ProgressReceipt{
		ProjectID:           registered.Project.ProjectID,
		DeliveryRunID:       "run-outbox-wait",
		RunID:               "run-outbox-wait",
		TaskID:              "task-outbox",
		AttemptID:           "att-outbox-1",
		AttemptOrdinal:      1,
		CorrelationID:       "corr-outbox",
		CorrelationSequence: 1,
		Phase:               "waiting",
		Status:              "pending",
		OccurredAt:          now.UTC().Format(time.RFC3339Nano),
	}
	obligation := progress.DeliveryObligation{
		OriginKind:        "progress-receipt",
		OriginID:          "corr-outbox",
		SinkKind:          "host",
		SinkID:            "attached-session",
		TransportContract: "host-jsonl-v1",
		AckPolicy:         progress.DeliveryAckPolicyRequired,
		MaxAttempts:       2,
	}
	created, err := progress.PersistReceiptWithObligation(ctx, store, receipt, obligation)
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE progress_delivery_obligations SET status = ? WHERE obligation_id = ?`,
			progress.DeliveryTerminalFailure, created.Obligation.ObligationID)
		return err
	}); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}
	clock := &waitFakeClock{now: now}
	var stdout, stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"wait", "outbox", "--repo", repo, "--run", "run-outbox-wait", "--format", "json", "--timeout", "1m",
	}, &stdout, &stderr, Deps{Now: func() time.Time { return clock.Now() }, WaitClock: clock})
	if exit != 2 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"provider_calls": 0`) && !strings.Contains(stdout.String(), `"provider_calls":0`) {
		t.Fatalf("want zero providers: %s", stdout.String())
	}
}

func TestWaitDetachedWorkerTerminalAndRestartCheckpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LOOPCODER_HOME", home)
	repo := t.TempDir()
	now := time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC)
	ctx := context.Background()
	registered, err := registry.Register(ctx, registry.Options{RepoPath: repo, Now: func() time.Time { return now }}, registry.DefaultDeps())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	roots, err := runtimepath.Resolve(ctx, repo)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	store, err := storage.Open(ctx, storage.Options{Path: roots.DatabasePath, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	claim, err := detachedrun.Claim(ctx, store, detachedrun.ClaimRequest{
		ProjectID:      registered.Project.ProjectID,
		RunID:          "run-worker-wait",
		Owner:          "owner-wait",
		LeaseExpiresAt: now.Add(time.Hour),
		IssueNumber:    42,
		Attempt:        1,
		BaseBranch:     "pre-prod",
		Provider:       "codex",
		Model:          "default",
		Now:            now,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := detachedrun.Complete(ctx, store, claim.Fence(), detachedrun.StatusSucceeded, "receipt-1", "done", "worker finished", now.Add(time.Minute)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	clock := &waitFakeClock{now: now}
	var stdout, stderr bytes.Buffer
	exit := RunWithDeps([]string{
		"wait", "detached-worker", "--repo", repo, "--run", claim.RunID, "--format", "json",
		"--wait-id", "worker-wait-1", "--timeout", "1m",
	}, &stdout, &stderr, Deps{Now: func() time.Time { return clock.Now() }, WaitClock: clock})
	if exit != 0 {
		t.Fatalf("first exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	exit = RunWithDeps([]string{
		"wait", "detached-worker", "--repo", repo, "--run", claim.RunID, "--format", "json",
		"--wait-id", "worker-wait-1", "--timeout", "1m",
	}, &stdout, &stderr, Deps{Now: func() time.Time { return clock.Now() }, WaitClock: clock})
	if exit != 0 {
		t.Fatalf("restart exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	entries, _ := filepath.Glob(filepath.Join(roots.ProjectRoot, "waits", "*.json"))
	if len(entries) == 0 {
		t.Fatalf("expected checkpoint files under %s", filepath.Join(roots.ProjectRoot, "waits"))
	}
}

type waitFakeClock struct {
	now time.Time
}

func (c *waitFakeClock) Now() time.Time { return c.now.UTC() }

func (c *waitFakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}
