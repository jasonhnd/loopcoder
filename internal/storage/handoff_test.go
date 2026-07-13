package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRecordHandoffTransactionTransfersOnceAndFencesOldOwner(t *testing.T) {
	ctx := context.Background()
	store, claim := handoffFixture(t, ctx)
	defer store.Close()

	req := handoffRequestFixture(claim)
	record, err := RecordHandoffTransaction(ctx, store, req)
	if err != nil {
		t.Fatalf("RecordHandoffTransaction: %v", err)
	}
	if record.HandoffStatus != HandoffStatusTransferred || record.HandoffGeneration != 2 || record.DestinationExecutorID == claim.ExecutorID {
		t.Fatalf("handoff = %#v, want transferred generation 2 away from old owner", record)
	}
	if record.AcceptedTaskFingerprint != "sha256:req" || record.NextAction != "await-successor-route" {
		t.Fatalf("handoff evidence = %#v, want accepted task fingerprint and next action", record)
	}
	err = CompleteClaimedChildRun(ctx, store, claim.ParentRunID, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, "succeeded", "2026-01-01T00:03:00Z", "stale old owner", "")
	if !IsStaleChildRunClaim(err) {
		t.Fatalf("old owner completion error = %v, want stale claim", err)
	}
	if !errors.Is(err, &FederationError{Code: ErrStaleClaimCode}) {
		t.Fatalf("old owner completion error = %v, want typed ErrStaleClaim", err)
	}
	if err := RenewChildRunClaim(ctx, store, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, fixedNow(), fixedNow().Add(time.Hour)); !IsStaleChildRunClaim(err) {
		t.Fatalf("old owner renew error = %v, want stale claim", err)
	}
	registration, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, record.DestinationExecutorID, record.HandoffGeneration)
	if err != nil {
		t.Fatalf("handoff owner launch validation: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, registration.ChildAgentID, AgentActionLaunch, claim.ExecutorID, claim.ClaimGeneration, "2026-01-01T00:03:00Z"); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("old owner registration transition error = %v, want stale claim", err)
	}
	if registration.ExecutorID != record.DestinationExecutorID || registration.ClaimGeneration != record.HandoffGeneration {
		t.Fatalf("handoff registration authority = %s/%d, want %s/%d", registration.ExecutorID, registration.ClaimGeneration, record.DestinationExecutorID, record.HandoffGeneration)
	}
	if _, err := TransitionAgentRegistration(ctx, store, registration.ChildAgentID, AgentActionLaunch, record.DestinationExecutorID, record.HandoffGeneration, "2026-01-01T00:03:00Z"); err != nil {
		t.Fatalf("handoff owner launch transition: %v", err)
	}
	if _, err := TransitionAgentRegistration(ctx, store, registration.ChildAgentID, AgentActionHeartbeat, record.DestinationExecutorID, record.HandoffGeneration, "2026-01-01T00:03:01Z"); err != nil {
		t.Fatalf("handoff owner heartbeat transition: %v", err)
	}
	if err := CompleteClaimedChildRun(ctx, store, claim.ParentRunID, claim.RunID, record.DestinationExecutorID, record.HandoffGeneration, "succeeded", "2026-01-01T00:03:02Z", "handoff owner terminal", "receipt-after-handoff"); err != nil {
		t.Fatalf("handoff owner completion: %v", err)
	}
}

func TestRecordHandoffTransactionStaleOwnershipLockFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, claim := handoffFixture(t, ctx)
	defer store.Close()

	if err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE agent_ownership_locks SET claim_generation = claim_generation + 98 WHERE run_id = ?`, claim.RunID)
		return err
	}); err != nil {
		t.Fatalf("stale ownership lock: %v", err)
	}
	record, err := RecordHandoffTransaction(ctx, store, handoffRequestFixture(claim))
	if err != nil {
		t.Fatalf("RecordHandoffTransaction: %v", err)
	}
	if record.HandoffStatus != HandoffStatusNeedsHuman || record.NextAction != "human-review-ownership-state" {
		t.Fatalf("handoff = %#v, want needs-human ownership review", record)
	}
	if err := RenewChildRunClaim(ctx, store, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, fixedNow(), fixedNow().Add(time.Hour)); !IsStaleChildRunClaim(err) {
		t.Fatalf("old owner renew error = %v, want stale claim", err)
	}
	if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, record.DestinationExecutorID, record.HandoffGeneration); err == nil {
		t.Fatalf("needs-human successor launch unexpectedly succeeded")
	}
	var lockState string
	var lockGeneration int64
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT state, claim_generation FROM agent_ownership_locks WHERE run_id = ?`, claim.RunID).Scan(&lockState, &lockGeneration)
	}); err != nil {
		t.Fatalf("query ownership lock: %v", err)
	}
	if lockState != OwnershipStateNeedsHuman || lockGeneration != record.HandoffGeneration {
		t.Fatalf("ownership lock = %s/%d, want needs-human/%d", lockState, lockGeneration, record.HandoffGeneration)
	}
}

func TestRecordHandoffTransactionDerivesSideEffectStateFromDurableEvidence(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(Store, federationClaim)
	}{
		{
			name: "executing phase",
			mutate: func(store Store, claim federationClaim) {
				if err := UpdateChildRunClaimPhase(ctx, store, claim.ParentRunID, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, ClaimPhaseExecuting, "2026-01-01T00:01:00Z", ""); err != nil {
					t.Fatalf("set executing phase: %v", err)
				}
			},
		},
		{
			name: "claim provider receipt",
			mutate: func(store Store, claim federationClaim) {
				if err := UpdateChildRunClaimPhase(ctx, store, claim.ParentRunID, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, ClaimPhaseClaimed, "2026-01-01T00:01:00Z", "provider-receipt-runtime"); err != nil {
					t.Fatalf("set claim receipt: %v", err)
				}
			},
		},
		{
			name: "attempt provider receipt",
			mutate: func(store Store, claim federationClaim) {
				if err := store.WithWriteTx(ctx, func(tx Tx) error {
					_, err := tx.Exec(ctx, `UPDATE delivery_attempts SET provider_receipt = ? WHERE attempt_id = ?`, "attempt-runtime-receipt", "attempt-handoff-a")
					return err
				}); err != nil {
					t.Fatalf("set attempt receipt: %v", err)
				}
			},
		},
		{
			name: "ambiguous claim phase",
			mutate: func(store Store, claim federationClaim) {
				if err := store.WithWriteTx(ctx, func(tx Tx) error {
					_, err := tx.Exec(ctx, `UPDATE run_claims SET phase = ? WHERE run_id = ?`, "provider-unknown", claim.RunID)
					return err
				}); err != nil {
					t.Fatalf("set ambiguous claim phase: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, claim := handoffFixture(t, ctx)
			defer store.Close()
			tc.mutate(store, claim)
			req := handoffRequestFixture(claim)
			req.SideEffectState = SideEffectStateNotStarted
			record, err := RecordHandoffTransaction(ctx, store, req)
			if err != nil {
				t.Fatalf("RecordHandoffTransaction: %v", err)
			}
			if record.HandoffStatus != HandoffStatusNeedsHuman || record.NextAction != "human-review-side-effect-state" {
				t.Fatalf("handoff = %#v, want needs-human side-effect review", record)
			}
			if record.SideEffectState == SideEffectStateNotStarted {
				t.Fatalf("side effect state trusted caller input; got %q", record.SideEffectState)
			}
			if _, err := ValidateNativeChildLaunch(ctx, store, claim.RunID, record.DestinationExecutorID, record.HandoffGeneration); err == nil {
				t.Fatalf("needs-human handoff launch unexpectedly succeeded")
			}
		})
	}
}

func TestRecordHandoffTransactionDurableSideEffectReplayAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, claim := handoffFixtureAtPath(t, ctx, path)
	if err := UpdateChildRunClaimPhase(ctx, store, claim.ParentRunID, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, ClaimPhaseExecuting, "2026-01-01T00:01:00Z", ""); err != nil {
		t.Fatalf("set executing phase: %v", err)
	}
	req := handoffRequestFixture(claim)
	req.SideEffectState = SideEffectStateNotStarted
	first, err := RecordHandoffTransaction(ctx, store, req)
	if err != nil {
		t.Fatalf("first handoff: %v", err)
	}
	if first.HandoffStatus != HandoffStatusNeedsHuman {
		t.Fatalf("first handoff status = %q, want needs-human", first.HandoffStatus)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	replayed, err := RecordHandoffTransaction(ctx, reopened, req)
	if err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	if replayed.HandoffID != first.HandoffID || replayed.SideEffectState != first.SideEffectState || replayed.HandoffStatus != HandoffStatusNeedsHuman {
		t.Fatalf("replayed handoff = %#v, want %#v", replayed, first)
	}
}

func TestRecordHandoffTransactionReplayAndChangedPayload(t *testing.T) {
	ctx := context.Background()
	store, claim := handoffFixture(t, ctx)
	defer store.Close()

	req := handoffRequestFixture(claim)
	first, err := RecordHandoffTransaction(ctx, store, req)
	if err != nil {
		t.Fatalf("first handoff: %v", err)
	}
	replayed, err := RecordHandoffTransaction(ctx, store, req)
	if err != nil {
		t.Fatalf("replay handoff: %v", err)
	}
	if replayed.HandoffID != first.HandoffID || replayed.HandoffGeneration != first.HandoffGeneration {
		t.Fatalf("replay = %#v, want same handoff %#v", replayed, first)
	}
	changed := req
	changed.ReasonCodes = []string{"rate-limited-429"}
	_, err = RecordHandoffTransaction(ctx, store, changed)
	if !errors.Is(err, &FederationError{Code: ErrHandoffReplayMismatchCode}) {
		t.Fatalf("changed replay error = %v, want ErrHandoffReplayMismatch", err)
	}
}

func TestRecordHandoffTransactionUnsupportedEvidenceNeedsHuman(t *testing.T) {
	ctx := context.Background()
	store, claim := handoffFixture(t, ctx)
	defer store.Close()

	req := handoffRequestFixture(claim)
	req.IdempotencyKey = "handoff-ambiguous"
	req.ReasonCodes = []string{"unknown-telemetry"}
	req.TriggerSnapshotJSON = `{"reason":"free-form unavailable"}`
	record, err := RecordHandoffTransaction(ctx, store, req)
	if err != nil {
		t.Fatalf("ambiguous handoff: %v", err)
	}
	if record.HandoffStatus != HandoffStatusNeedsHuman || record.NextAction != "human-review-ambiguous-evidence" || record.DestinationRoutePlaceholder != "needs-human" {
		t.Fatalf("ambiguous handoff = %#v, want needs-human without automatic route", record)
	}
	var runStatus, taskState, terminalCode string
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = ?`, claim.RunID).Scan(&runStatus); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT state, terminal_error_code FROM delivery_tasks WHERE task_id = 'task-a'`).Scan(&taskState, &terminalCode)
	}); err != nil {
		t.Fatalf("query needs-human state: %v", err)
	}
	if runStatus != "needs-human" || taskState != "needs-human" || terminalCode != string(ErrAmbiguousHandoffEvidenceCode) {
		t.Fatalf("state after ambiguous handoff = run:%q task:%q code:%q", runStatus, taskState, terminalCode)
	}
	err = CompleteClaimedChildRun(ctx, store, claim.ParentRunID, claim.RunID, claim.ExecutorID, claim.ClaimGeneration, "succeeded", "2026-01-01T00:03:00Z", "stale old owner", "")
	if !IsStaleChildRunClaim(err) {
		t.Fatalf("old owner after needs-human error = %v, want stale claim", err)
	}
}

func TestRecordHandoffTransactionRestartReplay(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, claim := handoffFixtureAtPath(t, ctx, path)
	req := handoffRequestFixture(claim)
	first, err := RecordHandoffTransaction(ctx, store, req)
	if err != nil {
		t.Fatalf("first handoff: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	replayed, err := RecordHandoffTransaction(ctx, reopened, req)
	if err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	if replayed.HandoffID != first.HandoffID || replayed.HandoffGeneration != first.HandoffGeneration {
		t.Fatalf("replay after reopen = %#v, want %#v", replayed, first)
	}
	var owner string
	var generation int64
	if err := reopened.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT executor_id, claim_generation FROM run_claims WHERE run_id = ?`, claim.RunID).Scan(&owner, &generation)
	}); err != nil {
		t.Fatalf("query claim after reopen: %v", err)
	}
	if owner != first.DestinationExecutorID || generation != first.HandoffGeneration {
		t.Fatalf("claim after reopen = %s/%d, want %s/%d", owner, generation, first.DestinationExecutorID, first.HandoffGeneration)
	}
}

func TestRecordHandoffTransactionRollbackBeforePersistRecoversOldOwner(t *testing.T) {
	ctx := context.Background()
	store, claim := handoffFixture(t, ctx)
	defer store.Close()

	forced := errors.New("forced crash before handoff persistence")
	var beforeLockState string
	var beforeLockGeneration int64
	if err := store.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT state, claim_generation FROM agent_ownership_locks WHERE run_id = ?`, claim.RunID).Scan(&beforeLockState, &beforeLockGeneration)
	}); err != nil {
		t.Fatalf("query ownership before rollback: %v", err)
	}
	err := store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_claims SET executor_id = ?, claim_generation = ? WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`,
			"handoff-crash", claim.ClaimGeneration+1, claim.RunID, claim.ExecutorID, claim.ClaimGeneration)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_ownership_locks SET claim_generation = ? WHERE run_id = ?`, claim.ClaimGeneration+1, claim.RunID); err != nil {
			return err
		}
		return forced
	})
	if !errors.Is(err, forced) {
		t.Fatalf("forced rollback error = %v", err)
	}
	var owner string
	var generation int64
	var afterLockState string
	var afterLockGeneration int64
	var handoffCount int
	if err := store.WithTx(ctx, func(tx Tx) error {
		if err := tx.QueryRow(ctx, `SELECT executor_id, claim_generation FROM run_claims WHERE run_id = ?`, claim.RunID).Scan(&owner, &generation); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT state, claim_generation FROM agent_ownership_locks WHERE run_id = ?`, claim.RunID).Scan(&afterLockState, &afterLockGeneration); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM handoff_transactions`).Scan(&handoffCount)
	}); err != nil {
		t.Fatalf("query rollback state: %v", err)
	}
	if owner != claim.ExecutorID || generation != claim.ClaimGeneration || handoffCount != 0 {
		t.Fatalf("rollback state owner=%s generation=%d handoffs=%d, want original owner and no record", owner, generation, handoffCount)
	}
	if afterLockState != beforeLockState || afterLockGeneration != beforeLockGeneration {
		t.Fatalf("rollback lock state = %s/%d, want %s/%d", afterLockState, afterLockGeneration, beforeLockState, beforeLockGeneration)
	}
	if _, err := RecordHandoffTransaction(ctx, store, handoffRequestFixture(claim)); err != nil {
		t.Fatalf("handoff after rollback: %v", err)
	}
}

func TestRecordHandoffTransactionMultiConnectionContention(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	storeA, claim := handoffFixtureAtPath(t, ctx, path)
	defer storeA.Close()
	storeB, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	defer storeB.Close()

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan HandoffTransaction, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := handoffRequestFixture(claim)
			req.IdempotencyKey = "handoff-contention-" + string(rune('a'+i))
			var target Store = storeA
			if i%2 == 1 {
				target = storeB
			}
			record, err := RecordHandoffTransaction(ctx, target, req)
			if err != nil {
				errs <- err
				return
			}
			results <- record
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("contended handoff error: %v", err)
	}
	var first HandoffTransaction
	count := 0
	for record := range results {
		if count == 0 {
			first = record
		}
		if record.HandoffID != first.HandoffID || record.HandoffGeneration != first.HandoffGeneration {
			t.Fatalf("contention record = %#v, want same as %#v", record, first)
		}
		count++
	}
	if count != workers {
		t.Fatalf("contention returned %d records, want %d", count, workers)
	}
	var rowCount int
	if err := storeA.WithTx(ctx, func(tx Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM handoff_transactions WHERE task_id = 'task-a'`).Scan(&rowCount)
	}); err != nil {
		t.Fatalf("query handoff count: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("handoff rows = %d, want 1", rowCount)
	}
}

func handoffFixture(t *testing.T, ctx context.Context) (Store, federationClaim) {
	t.Helper()
	return handoffFixtureAtPath(t, ctx, filepath.Join(t.TempDir(), "loopcoder.db"))
}

func handoffFixtureAtPath(t *testing.T, ctx context.Context, path string) (Store, federationClaim) {
	t.Helper()
	store, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	claim := createFederationClaim(t, ctx, store, "proj-handoff", "run-root-handoff", "run-child-handoff", "child-handoff")
	if err := seedHandoffAttempt(t, ctx, store, claim); err != nil {
		store.Close()
		t.Fatalf("seed handoff attempt: %v", err)
	}
	return store, claim
}

func seedHandoffAttempt(t *testing.T, ctx context.Context, store Store, claim federationClaim) error {
	t.Helper()
	if _, err := RegisterAgent(ctx, store, handoffFederationRequest(claim)); err != nil {
		return err
	}
	return store.WithWriteTx(ctx, func(tx Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delivery_attempts(
			attempt_id, schema_version, record_version, project_id, delivery_run_id, task_id, attempt_ordinal, state,
			claim_generation, executor_id, provider_idempotency_key, side_effect_class, started_at, created_at, updated_at,
			created_by_json, updated_by_json, host_json)
			VALUES ('attempt-handoff-a', 'loopcoder.attempt.v1', 1, 'proj-handoff', 'drun-a', 'task-a', 1, 'running',
			?, ?, ?, ?, '2026-01-01T00:00:01Z', '2026-01-01T00:00:01Z', '2026-01-01T00:00:01Z', '{}', '{}', '{}')`,
			claim.ClaimGeneration, claim.ExecutorID, claim.ProviderKey, SideEffectRepoWrite)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE delivery_tasks SET state = 'running', active_attempt_id = 'attempt-handoff-a', attempt_count = 1 WHERE project_id = 'proj-handoff' AND delivery_run_id = 'drun-a' AND task_id = 'task-a'`)
		return err
	})
}

func handoffFederationRequest(claim federationClaim) AgentRegistrationRequest {
	req := federationRequest(claim)
	scope := federationAuthorityScope("proj-handoff")
	req.ProjectID = "proj-handoff"
	req.Scope = scope
	req.ParentScope = &scope
	return req
}

func handoffRequestFixture(claim federationClaim) HandoffRequest {
	return HandoffRequest{
		IdempotencyKey:              "handoff-quota-a",
		ProjectID:                   "proj-handoff",
		DeliveryRunID:               "drun-a",
		TaskID:                      "task-a",
		ParentRunID:                 claim.ParentRunID,
		ChildRunID:                  claim.RunID,
		SourceAttemptID:             "attempt-handoff-a",
		SourceExecutorID:            claim.ExecutorID,
		SourceClaimGeneration:       claim.ClaimGeneration,
		TriggerKind:                 HandoffTriggerQuotaAvailability,
		ReasonCodes:                 []string{"quota-exhausted"},
		EvidenceRecordIDs:           []string{"availability-observation-a", "qsnap-a"},
		TriggerSnapshotJSON:         `{"reason_codes":["quota-exhausted"],"remaining":0}`,
		PolicyVersion:               HandoffPolicyVersion,
		SideEffectState:             SideEffectStateNotStarted,
		DestinationRoutePlaceholder: "route-pending",
		RequestedAt:                 "2026-01-01T00:02:00Z",
		DestinationLeaseExpiresAt:   fixedNow().Add(time.Hour).Format(time.RFC3339Nano),
	}
}
