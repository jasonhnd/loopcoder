package detachedrun

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestSupervisorClaimFencesTransitionsAndTerminalReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 1, 2, 3, 0, time.UTC)
	store := newDetachedStore(t, now)
	record, err := Claim(ctx, store, ClaimRequest{
		ProjectID:      "proj_detached",
		RunID:          "run-detached",
		Owner:          "supervisor-a",
		LeaseExpiresAt: now.Add(time.Hour),
		IssueNumber:    898,
		Attempt:        1,
		Now:            now,
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if record.Status != StatusNotStarted || record.LaunchPhase != PhaseClaimed || record.Generation != 1 {
		t.Fatalf("claim record = %#v", record)
	}
	if _, err := Claim(ctx, store, ClaimRequest{ProjectID: "proj_detached", RunID: "run-detached", Owner: "supervisor-b", LeaseExpiresAt: now.Add(time.Hour), Now: now}); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("second Claim error = %v, want ErrClaimConflict", err)
	}
	if _, err := MarkSpawned(ctx, store, Fence{RunID: record.RunID, Owner: "wrong", Generation: record.Generation}, 1234, "process-tree", now); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale MarkSpawned error = %v, want ErrStaleClaim", err)
	}
	spawned, err := MarkSpawned(ctx, store, record.Fence(), 1234, "process-tree", now.Add(time.Second))
	if err != nil {
		t.Fatalf("MarkSpawned: %v", err)
	}
	if spawned.ProcessPID != 1234 || spawned.ProcessStartedAt == "" || spawned.Status != StatusRunning {
		t.Fatalf("spawned = %#v", spawned)
	}
	done, err := Complete(ctx, store, record.Fence(), StatusSucceeded, "receipt-terminal", "", "", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done.Status != StatusSucceeded || done.TerminalReceiptID != "receipt-terminal" || done.Classification != ClassificationTerminal {
		t.Fatalf("complete = %#v", done)
	}
	replay, err := Reconcile(ctx, store, record.RunID, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("Reconcile terminal: %v", err)
	}
	if !replay.Terminal || replay.ReplayAction != "reused-terminal" || replay.Execute {
		t.Fatalf("terminal replay = %#v", replay)
	}
}

func TestReconcileCrashWindows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 2, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		mutate      func(storage.Store, Record)
		wantAction  string
		wantHuman   bool
		wantRecover bool
	}{
		{
			name:        "claimed before worker start retryable after lease",
			wantAction:  "retryable",
			wantRecover: true,
		},
		{
			name: "worker started before launch receipt needs human",
			mutate: func(store storage.Store, r Record) {
				_, err := MarkWorkerStarted(ctx, store, r.Fence(), now.Add(time.Second))
				if err != nil {
					t.Fatalf("MarkWorkerStarted: %v", err)
				}
			},
			wantAction: "needs-human",
			wantHuman:  true,
		},
		{
			name: "provider exposure without receipt needs human",
			mutate: func(store storage.Store, r Record) {
				_, err := MarkProviderExposed(ctx, store, r.Fence(), "", now.Add(time.Second))
				if err != nil {
					t.Fatalf("MarkProviderExposed: %v", err)
				}
			},
			wantAction: "needs-human",
			wantHuman:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newDetachedStore(t, now)
			record, err := Claim(ctx, store, ClaimRequest{
				ProjectID:      "proj_detached",
				RunID:          "run-detached-" + t.Name(),
				Owner:          "supervisor",
				LeaseExpiresAt: now.Add(time.Minute),
				Now:            now,
			})
			if err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if tt.mutate != nil {
				tt.mutate(store, record)
			}
			got, err := Reconcile(ctx, store, record.RunID, now.Add(2*time.Minute))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got.ReplayAction != tt.wantAction || got.NeedsHuman != tt.wantHuman || got.CanRecover != tt.wantRecover || got.Execute {
				t.Fatalf("Reconcile = %#v, want action=%s human=%v recover=%v execute=false", got, tt.wantAction, tt.wantHuman, tt.wantRecover)
			}
		})
	}
}

func newDetachedStore(t *testing.T, now time.Time) storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.WithWriteTx(context.Background(), func(tx storage.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			"proj_detached", "/repo", now.Format(time.RFC3339), now.Format(time.RFC3339))
		return err
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return store
}
