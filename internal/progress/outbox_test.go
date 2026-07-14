package progress

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestPersistReceiptWithObligationIsAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	first, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation first: %v", err)
	}
	if !first.Receipt.Inserted || !first.Inserted || first.Obligation.Status != DeliveryPending {
		t.Fatalf("first result = %#v, want inserted pending receipt and obligation", first)
	}
	second, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation replay: %v", err)
	}
	if second.Receipt.Inserted || second.Inserted || second.Obligation.ObligationID != first.Obligation.ObligationID {
		t.Fatalf("replay result = %#v, want idempotent existing obligation %s", second, first.Obligation.ObligationID)
	}
	assertProgressCount(t, ctx, store, `SELECT COUNT(*) FROM progress_receipts`, 1)
	assertProgressCount(t, ctx, store, `SELECT COUNT(*) FROM progress_delivery_obligations`, 1)

	batch, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"}, fixedTime)
	if err != nil {
		t.Fatalf("ReadReceipts: %v", err)
	}
	if len(batch.Views) != 1 || batch.Views[0].DeliveryState.State != DeliveryPending {
		t.Fatalf("delivery state = %#v, want durable pending state", batch.Views)
	}
}

func TestPersistReceiptWithObligationRejectsConflictingContractReplay(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	first, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation first: %v", err)
	}
	originalPayload := deliveryObligationPayload(t, ctx, store, first.Obligation.ObligationID)

	tests := []struct {
		name   string
		mutate func(*DeliveryObligation)
	}{
		{
			name: "ack policy",
			mutate: func(obligation *DeliveryObligation) {
				obligation.AckPolicy = DeliveryAckPolicyNone
			},
		},
		{
			name: "max attempts",
			mutate: func(obligation *DeliveryObligation) {
				obligation.MaxAttempts = first.Obligation.MaxAttempts + 1
			},
		},
		{
			name: "expiry policy",
			mutate: func(obligation *DeliveryObligation) {
				obligation.ExpiresAt = fixedTime.Add(time.Hour).Format(time.RFC3339Nano)
			},
		},
		{
			name: "supersession policy",
			mutate: func(obligation *DeliveryObligation) {
				obligation.SupersededByObligationID = "pdelobl_superseding"
			},
		},
		{
			name: "sink detail",
			mutate: func(obligation *DeliveryObligation) {
				obligation.SinkID = "another-session"
			},
		},
		{
			name: "transport detail",
			mutate: func(obligation *DeliveryObligation) {
				obligation.TransportContract = "host-jsonl-v2"
			},
		},
		{
			name: "initial state",
			mutate: func(obligation *DeliveryObligation) {
				obligation.Status = DeliveryRetryableFailure
				obligation.NextAttemptAt = fixedTime.Add(time.Minute).Format(time.RFC3339Nano)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conflicting := baseObligation()
			tt.mutate(&conflicting)
			if _, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), conflicting); !errors.Is(err, ErrDuplicateReplay) {
				t.Fatalf("conflicting replay error = %v, want ErrDuplicateReplay", err)
			}
			if got := deliveryObligationPayload(t, ctx, store, first.Obligation.ObligationID); got != originalPayload {
				t.Fatalf("original obligation payload changed:\noriginal=%s\nchanged=%s", originalPayload, got)
			}
			assertProgressCount(t, ctx, store, `SELECT COUNT(*) FROM progress_delivery_obligations`, 1)
		})
	}
}

func TestPersistReceiptWithObligationExactReplayAfterClaimReturnsCurrentRecord(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation first: %v", err)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-replay-current",
		LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	replayed, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation replay after claim: %v", err)
	}
	if replayed.Inserted || replayed.Obligation.ObligationID != created.Obligation.ObligationID ||
		replayed.Obligation.Status != DeliveryAttempting || replayed.Obligation.ClaimGeneration != claim.ClaimGeneration {
		t.Fatalf("replayed = %#v, want existing current attempting obligation", replayed)
	}
	assertProgressCount(t, ctx, store, `SELECT COUNT(*) FROM progress_delivery_obligations`, 1)
}

func TestDeliveryOutboxClaimAttemptAckAndCursorFencing(t *testing.T) {
	ctx := context.Background()
	now := fixedTime
	store, err := storage.Open(ctx, storage.Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	defer store.Close()

	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-a",
		LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	if claim.ClaimGeneration != 1 || claim.Status != DeliveryAttempting {
		t.Fatalf("claim = %#v, want generation 1 attempting", claim)
	}
	if _, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-b",
		LeaseExpiresAt: fixedTime.Add(2 * time.Minute).Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("concurrent claim error = %v, want ErrClaimConflict", err)
	}

	attempted, err := RecordDeliveryAttemptResult(ctx, store, AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		ResultStatus: DeliveryDeliveredUnacknowledged,
		Evidence: DeliveryEvidence{
			EvidenceKind:      "host-accepted",
			EvidenceRef:       "host-event-1",
			Summary:           "host accepted a bounded delivery envelope",
			Confidence:        "exact",
			TransportContract: "host-jsonl-v1",
		},
	})
	if err != nil {
		t.Fatalf("RecordDeliveryAttemptResult: %v", err)
	}
	if attempted.Status != DeliveryDeliveredUnacknowledged || attempted.AttemptCount != 1 {
		t.Fatalf("attempted = %#v, want delivered-unacknowledged attempt 1", attempted)
	}
	if _, err := AcknowledgeDelivery(ctx, store, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		Evidence: DeliveryEvidence{
			EvidenceKind:      "stdout-bytes",
			EvidenceRef:       "stdout-1",
			Confidence:        "exact",
			TransportContract: "host-jsonl-v1",
		},
	}); !errors.Is(err, ErrEvidenceRejected) {
		t.Fatalf("stdout ack error = %v, want ErrEvidenceRejected", err)
	}
	if err := AdvanceDeliveryReplayCursor(ctx, store, CursorAdvanceRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration + 1,
		OriginKind:   "host-jsonl",
		OriginID:     "session-a",
		CursorValue:  "cursor-1",
	}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale cursor advance error = %v, want ErrStaleClaim", err)
	}

	acked, err := AcknowledgeDelivery(ctx, store, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		Evidence: DeliveryEvidence{
			EvidenceKind:      "host-visible",
			EvidenceRef:       "host-visible-1",
			Summary:           "host reported visible delivery",
			Confidence:        "exact",
			TransportContract: "host-jsonl-v1",
		},
	})
	if err != nil {
		t.Fatalf("AcknowledgeDelivery: %v", err)
	}
	if acked.Status != DeliveryAcknowledged {
		t.Fatalf("acked status = %q, want acknowledged", acked.Status)
	}
	if _, err := AcknowledgeDelivery(ctx, store, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		Evidence: DeliveryEvidence{
			EvidenceKind:      "host-visible",
			EvidenceRef:       "host-visible-1",
			Summary:           "host reported visible delivery",
			Confidence:        "exact",
			TransportContract: "host-jsonl-v1",
		},
	}); err != nil {
		t.Fatalf("duplicate AcknowledgeDelivery: %v", err)
	}
	assertProgressCount(t, ctx, store, `SELECT COUNT(*) FROM progress_delivery_acknowledgments`, 1)

	now = fixedTime.Add(2 * time.Minute)
	takeover, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-b",
		LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if !errors.Is(err, ErrClaimConflict) || takeover.ClaimGeneration != 0 {
		t.Fatalf("terminal takeover = %#v err=%v, want terminal claim conflict", takeover, err)
	}
}

func TestDeliveryOutboxAttemptResultReplayDoesNotDoubleCount(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-replay",
		LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	req := AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		ResultStatus: DeliveryRetryableFailure,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-error", EvidenceRef: "host-error-replay", TransportContract: "host-jsonl-v1"},
		ErrorCode:    "temporary",
		StartedAt:    fixedTime.Format(time.RFC3339Nano),
		CompletedAt:  fixedTime.Format(time.RFC3339Nano),
	}
	first, err := RecordDeliveryAttemptResult(ctx, store, req)
	if err != nil {
		t.Fatalf("RecordDeliveryAttemptResult first: %v", err)
	}
	second, err := RecordDeliveryAttemptResult(ctx, store, req)
	if err != nil {
		t.Fatalf("RecordDeliveryAttemptResult replay: %v", err)
	}
	if first.AttemptCount != 1 || second.AttemptCount != 1 || second.ClaimGeneration != claim.ClaimGeneration {
		t.Fatalf("replay counts first=%#v second=%#v, want one recorded attempt at original generation", first, second)
	}
	assertProgressCount(t, ctx, store, `SELECT COUNT(*) FROM progress_delivery_attempts`, 1)

	conflict := req
	conflict.Evidence.EvidenceRef = "host-error-conflict"
	if _, err := RecordDeliveryAttemptResult(ctx, store, conflict); !errors.Is(err, ErrDuplicateReplay) {
		t.Fatalf("conflicting replay error = %v, want ErrDuplicateReplay", err)
	}
	assertProgressCount(t, ctx, store, `SELECT COUNT(*) FROM progress_delivery_attempts`, 1)
}

func TestDeliveryOutboxConcurrentAttemptResultReplayIsSingleTransition(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-race",
		LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close seed: %v", err)
	}

	req := AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		ResultStatus: DeliveryRetryableFailure,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-error", EvidenceRef: "host-error-race", TransportContract: "host-jsonl-v1"},
		ErrorCode:    "temporary",
		StartedAt:    fixedTime.Format(time.RFC3339Nano),
		CompletedAt:  fixedTime.Format(time.RFC3339Nano),
	}
	ready := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			conn, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			<-ready
			_, err = RecordDeliveryAttemptResult(ctx, conn, req)
			errs <- err
		}()
	}
	close(ready)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent RecordDeliveryAttemptResult[%d]: %v", i, err)
		}
	}

	verify, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open verify: %v", err)
	}
	defer verify.Close()
	assertProgressCount(t, ctx, verify, `SELECT COUNT(*) FROM progress_delivery_attempts`, 1)
	loaded, err := ReadDeliveryObligation(ctx, verify, "proj_progress", "run_progress", created.Obligation.ObligationID)
	if err != nil {
		t.Fatalf("ReadDeliveryObligation: %v", err)
	}
	if loaded.AttemptCount != 1 || loaded.Status != DeliveryRetryableFailure || loaded.ClaimGeneration != claim.ClaimGeneration {
		t.Fatalf("loaded = %#v, want one retryable transition at generation %d", loaded, claim.ClaimGeneration)
	}
}

func TestDeliveryOutboxRetrySchedulingAndScopedReadAPIsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	now := fixedTime
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{
		Path: path,
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)

	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-a",
		LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	retry, err := RecordDeliveryAttemptResult(ctx, store, AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		ResultStatus: DeliveryRetryableFailure,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-error", EvidenceRef: "host-error-1", TransportContract: "host-jsonl-v1"},
		ErrorCode:    "transport-timeout",
	})
	if err != nil {
		t.Fatalf("RecordDeliveryAttemptResult retry: %v", err)
	}
	wantNext := fixedTime.Add(15 * time.Second).Format(time.RFC3339Nano)
	if retry.NextAttemptAt != wantNext || retry.Status != DeliveryRetryableFailure {
		t.Fatalf("retry obligation next/status = %q/%q, want %q/%q", retry.NextAttemptAt, retry.Status, wantNext, DeliveryRetryableFailure)
	}
	if _, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-b",
		LeaseExpiresAt: now.Add(2 * time.Minute).Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("early retry claim error = %v, want ErrClaimConflict", err)
	}
	dueNow, err := ListDeliveryObligations(ctx, store, DeliveryObligationFilter{
		ProjectID:     "proj_progress",
		DeliveryRunID: "run_progress",
		Status:        DeliveryRetryableFailure,
		DueAtOrBefore: now.Format(time.RFC3339Nano),
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("ListDeliveryObligations due now: %v", err)
	}
	if len(dueNow) != 0 {
		t.Fatalf("due-now obligations = %#v, want none before next_attempt_at", dueNow)
	}
	if err := AdvanceDeliveryReplayCursor(ctx, store, CursorAdvanceRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		OriginKind:   "host-jsonl",
		OriginID:     "session-a",
		CursorValue:  "cursor-1",
	}); err != nil {
		t.Fatalf("AdvanceDeliveryReplayCursor: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return now.Add(20 * time.Second) }})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	read, err := ReadDeliveryObligation(ctx, reopened, "proj_progress", "run_progress", created.Obligation.ObligationID)
	if err != nil {
		t.Fatalf("ReadDeliveryObligation: %v", err)
	}
	if read.NextAttemptAt != wantNext {
		t.Fatalf("reopened next_attempt_at = %q, want %q", read.NextAttemptAt, wantNext)
	}
	list, err := ListDeliveryObligations(ctx, reopened, DeliveryObligationFilter{
		ProjectID:     "proj_progress",
		DeliveryRunID: "run_progress",
		Status:        DeliveryRetryableFailure,
		DueAtOrBefore: now.Add(20 * time.Second).Format(time.RFC3339Nano),
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("ListDeliveryObligations: %v", err)
	}
	if len(list) != 1 || list[0].ObligationID != created.Obligation.ObligationID {
		t.Fatalf("due list = %#v, want retry obligation", list)
	}
	attempts, err := ListDeliveryAttempts(ctx, reopened, DeliveryAttemptFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", ObligationID: created.Obligation.ObligationID})
	if err != nil {
		t.Fatalf("ListDeliveryAttempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].NextAttemptAt != wantNext || attempts[0].ErrorCode != "transport-timeout" {
		t.Fatalf("attempts = %#v, want retry attempt with next_attempt_at", attempts)
	}
	cursors, err := ListDeliveryReplayCursors(ctx, reopened, DeliveryCursorFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", OriginKind: "host-jsonl"})
	if err != nil {
		t.Fatalf("ListDeliveryReplayCursors: %v", err)
	}
	if len(cursors) != 1 || cursors[0].CursorValue != "cursor-1" {
		t.Fatalf("cursors = %#v, want advanced cursor", cursors)
	}
}

func TestDeliveryOutboxNoAckPolicyTerminalsWithoutAcknowledgment(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	obligation := baseObligation()
	obligation.AckPolicy = DeliveryAckPolicyNone
	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), obligation)
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	if created.Obligation.RequiredAck || created.Obligation.AckPolicy != DeliveryAckPolicyNone {
		t.Fatalf("created ack contract = required:%t policy:%q, want no-ack", created.Obligation.RequiredAck, created.Obligation.AckPolicy)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-noack",
		LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	delivered, err := RecordDeliveryAttemptResult(ctx, store, AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		ResultStatus: DeliveryDeliveredUnacknowledged,
		Evidence: DeliveryEvidence{
			EvidenceKind:      "host-accepted",
			EvidenceRef:       "host-event-noack",
			TransportContract: "host-jsonl-v1",
		},
	})
	if err != nil {
		t.Fatalf("RecordDeliveryAttemptResult: %v", err)
	}
	if delivered.Status != DeliveryAcknowledged || delivered.RequiredAck {
		t.Fatalf("delivered no-ack state = %#v, want terminal acknowledged without required ack", delivered)
	}
	if _, err := AcknowledgeDelivery(ctx, store, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-visible", EvidenceRef: "host-visible-noack", TransportContract: "host-jsonl-v1"},
	}); !errors.Is(err, ErrEvidenceRejected) {
		t.Fatalf("no-ack explicit ack error = %v, want ErrEvidenceRejected", err)
	}
	acks, err := ListDeliveryAcknowledgments(ctx, store, DeliveryAckFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"})
	if err != nil {
		t.Fatalf("ListDeliveryAcknowledgments: %v", err)
	}
	if len(acks) != 0 {
		t.Fatalf("acks = %#v, want none for no-ack contract", acks)
	}
	if _, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-late",
		LeaseExpiresAt: fixedTime.Add(2 * time.Minute).Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("terminal no-ack claim error = %v, want ErrClaimConflict", err)
	}
}

func TestDeliveryOutboxStaleClaimCannotRecordAfterLeaseTakeover(t *testing.T) {
	ctx := context.Background()
	now := fixedTime
	store, err := storage.Open(ctx, storage.Options{
		Path: filepath.Join(t.TempDir(), "loopcoder.db"),
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	defer store.Close()

	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	first, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-a",
		LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	now = fixedTime.Add(2 * time.Minute)
	second, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-b",
		LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if second.ClaimGeneration != first.ClaimGeneration+1 {
		t.Fatalf("takeover generation = %d, want %d", second.ClaimGeneration, first.ClaimGeneration+1)
	}
	_, err = RecordDeliveryAttemptResult(ctx, store, AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   first.ClaimOwner,
		Generation:   first.ClaimGeneration,
		ResultStatus: DeliveryUnsupported,
		Evidence:     DeliveryEvidence{EvidenceKind: "unsupported", EvidenceRef: "unsupported-1"},
	})
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale attempt error = %v, want ErrStaleClaim", err)
	}
}

func TestDeliveryOutboxFailureInjectionMatrix(t *testing.T) {
	t.Run("busy locked write path fails without persisting", func(t *testing.T) {
		ctx := context.Background()
		store := newStore(t, ctx)
		defer store.Close()
		errStore := writeErrorStore{Store: store, err: fmt.Errorf("storage write transaction: begin immediate: database is locked (SQLITE_BUSY)")}
		if _, err := PersistReceiptWithObligation(ctx, errStore, baseReceipt(), baseObligation()); err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") {
			t.Fatalf("busy error = %v, want SQLITE_BUSY", err)
		}
		assertProgressCount(t, ctx, store, `SELECT COUNT(*) FROM progress_delivery_obligations`, 0)
	})

	t.Run("disk full commit failure rolls back receipt and obligation", func(t *testing.T) {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "loopcoder.db")
		seed, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
		if err != nil {
			t.Fatalf("Open seed: %v", err)
		}
		insertProject(t, ctx, seed)
		if err := seed.Close(); err != nil {
			t.Fatalf("Close seed: %v", err)
		}
		store, err := storage.Open(ctx, storage.Options{
			Path: path,
			Now:  func() time.Time { return fixedTime },
			WriteTxCommitHookForTest: func(context.Context, storage.Tx, func(context.Context) error) error {
				return fmt.Errorf("disk full while committing progress outbox")
			},
		})
		if err != nil {
			t.Fatalf("Open failing: %v", err)
		}
		_, err = PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("commit failure = %v, want disk full", err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close failing: %v", err)
		}
		verify, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
		if err != nil {
			t.Fatalf("Open verify: %v", err)
		}
		defer verify.Close()
		assertProgressCount(t, ctx, verify, `SELECT COUNT(*) FROM progress_receipts`, 0)
		assertProgressCount(t, ctx, verify, `SELECT COUNT(*) FROM progress_delivery_obligations`, 0)
	})

	t.Run("canceled context does not persist", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		store := newStore(t, context.Background())
		defer store.Close()
		if _, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation()); err == nil {
			t.Fatal("canceled PersistReceiptWithObligation succeeded")
		}
		assertProgressCount(t, context.Background(), store, `SELECT COUNT(*) FROM progress_delivery_obligations`, 0)
	})
}

func TestDeliveryOutboxCrashReplayAndMaxAttemptBoundaries(t *testing.T) {
	ctx := context.Background()
	now := fixedTime
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	first, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-before-send",
		LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close before send: %v", err)
	}

	now = fixedTime.Add(2 * time.Minute)
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("reopen before send: %v", err)
	}
	beforeSend, err := ListDeliveryAttempts(ctx, reopened, DeliveryAttemptFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", ObligationID: created.Obligation.ObligationID})
	if err != nil {
		t.Fatalf("ListDeliveryAttempts before send: %v", err)
	}
	if len(beforeSend) != 0 {
		t.Fatalf("crash before send attempts = %#v, want none", beforeSend)
	}
	second, err := ClaimDeliveryObligation(ctx, reopened, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-after-restart",
		LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("takeover after restart: %v", err)
	}
	if second.ClaimGeneration != first.ClaimGeneration+1 {
		t.Fatalf("takeover generation = %d, want %d", second.ClaimGeneration, first.ClaimGeneration+1)
	}
	delivered, err := RecordDeliveryAttemptResult(ctx, reopened, AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   second.ClaimOwner,
		Generation:   second.ClaimGeneration,
		ResultStatus: DeliveryDeliveredUnacknowledged,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-accepted", EvidenceRef: "host-event-crash-after-send", TransportContract: "host-jsonl-v1"},
	})
	if err != nil {
		t.Fatalf("RecordDeliveryAttemptResult delivered: %v", err)
	}
	if delivered.Status != DeliveryDeliveredUnacknowledged {
		t.Fatalf("delivered status = %q, want delivered-unacknowledged", delivered.Status)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close after send: %v", err)
	}

	replayed, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return now.Add(time.Minute) }})
	if err != nil {
		t.Fatalf("replay after send: %v", err)
	}
	defer replayed.Close()
	loaded, err := ReadDeliveryObligation(ctx, replayed, "proj_progress", "run_progress", created.Obligation.ObligationID)
	if err != nil {
		t.Fatalf("ReadDeliveryObligation replay: %v", err)
	}
	if loaded.Status != DeliveryDeliveredUnacknowledged || loaded.AttemptCount != 1 {
		t.Fatalf("replayed obligation = %#v, want delivered-unacknowledged attempt 1", loaded)
	}
	assertProgressCount(t, ctx, replayed, `SELECT COUNT(*) FROM progress_delivery_acknowledgments`, 0)
	if _, err := ClaimDeliveryObligation(ctx, replayed, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-after-send",
		LeaseExpiresAt: now.Add(2 * time.Minute).Format(time.RFC3339Nano),
	}); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("delivered-unack claim error = %v, want ErrClaimConflict", err)
	}
	if _, err := AcknowledgeDelivery(ctx, replayed, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   second.ClaimOwner,
		Generation:   second.ClaimGeneration,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-visible", EvidenceRef: "host-visible-stale", TransportContract: "wrong-contract"},
	}); !errors.Is(err, ErrEvidenceRejected) {
		t.Fatalf("wrong transport ack error = %v, want ErrEvidenceRejected", err)
	}
	acked, err := AcknowledgeDelivery(ctx, replayed, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   second.ClaimOwner,
		Generation:   second.ClaimGeneration,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-visible", EvidenceRef: "host-visible-stale", TransportContract: "host-jsonl-v1"},
	})
	if err != nil {
		t.Fatalf("expired-lease AcknowledgeDelivery: %v", err)
	}
	if acked.Status != DeliveryAcknowledged || acked.AttemptCount != 1 {
		t.Fatalf("acked after expired lease = %#v, want acknowledged once without resend", acked)
	}
	if _, err := AcknowledgeDelivery(ctx, replayed, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   second.ClaimOwner,
		Generation:   second.ClaimGeneration,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-visible", EvidenceRef: "host-visible-stale", TransportContract: "host-jsonl-v1"},
	}); err != nil {
		t.Fatalf("duplicate expired-lease AcknowledgeDelivery: %v", err)
	}
	if _, err := AcknowledgeDelivery(ctx, replayed, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   "supervisor-wrong",
		Generation:   second.ClaimGeneration,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-visible", EvidenceRef: "host-visible-stale", TransportContract: "host-jsonl-v1"},
	}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("wrong owner ack error = %v, want ErrStaleClaim", err)
	}
	if _, err := AcknowledgeDelivery(ctx, replayed, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   second.ClaimOwner,
		Generation:   second.ClaimGeneration + 1,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-visible", EvidenceRef: "host-visible-stale", TransportContract: "host-jsonl-v1"},
	}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("later generation ack error = %v, want ErrStaleClaim", err)
	}
	assertProgressCount(t, ctx, replayed, `SELECT COUNT(*) FROM progress_delivery_attempts`, 1)
	assertProgressCount(t, ctx, replayed, `SELECT COUNT(*) FROM progress_delivery_acknowledgments`, 1)
}

func TestDeliveryOutboxMaxAttemptBoundsPersistFinalAttempt(t *testing.T) {
	for _, maxAttempts := range []int{1, 2} {
		t.Run(fmt.Sprintf("max_attempts_%d", maxAttempts), func(t *testing.T) {
			ctx := context.Background()
			now := fixedTime
			store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return now }})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			insertProject(t, ctx, store)
			defer store.Close()

			obligation := baseObligation()
			obligation.MaxAttempts = maxAttempts
			created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), obligation)
			if err != nil {
				t.Fatalf("PersistReceiptWithObligation: %v", err)
			}

			var terminal DeliveryObligation
			for attempt := 1; attempt <= maxAttempts; attempt++ {
				claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
					ObligationID:   created.Obligation.ObligationID,
					ClaimOwner:     fmt.Sprintf("supervisor-%d", attempt),
					LeaseExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano),
				})
				if err != nil {
					t.Fatalf("claim %d: %v", attempt, err)
				}
				terminal, err = RecordDeliveryAttemptResult(ctx, store, AttemptResultRequest{
					ObligationID: created.Obligation.ObligationID,
					ClaimOwner:   claim.ClaimOwner,
					Generation:   claim.ClaimGeneration,
					ResultStatus: DeliveryRetryableFailure,
					Evidence:     DeliveryEvidence{EvidenceKind: "host-error", EvidenceRef: fmt.Sprintf("host-error-%d", attempt), TransportContract: "host-jsonl-v1"},
					ErrorCode:    "temporary",
				})
				if err != nil {
					t.Fatalf("attempt result %d: %v", attempt, err)
				}
				if attempt < maxAttempts {
					if terminal.Status != DeliveryRetryableFailure || terminal.NextAttemptAt == "" || terminal.AttemptCount != attempt {
						t.Fatalf("intermediate attempt %d = %#v, want scheduled retry", attempt, terminal)
					}
					now = fixedTime.Add(time.Duration(attempt) * time.Minute)
				}
			}
			if terminal.Status != DeliveryTerminalFailure || terminal.NextAttemptAt != "" || terminal.AttemptCount != maxAttempts {
				t.Fatalf("terminal = %#v, want terminal failure with %d attempts and no next_attempt_at", terminal, maxAttempts)
			}
			if terminal.ClaimOwner != "" || terminal.ClaimedAt != "" || terminal.ClaimExpiresAt != "" {
				t.Fatalf("terminal claim authority = owner:%q claimed:%q expires:%q, want cleared", terminal.ClaimOwner, terminal.ClaimedAt, terminal.ClaimExpiresAt)
			}
			attempts, err := ListDeliveryAttempts(ctx, store, DeliveryAttemptFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", ObligationID: created.Obligation.ObligationID})
			if err != nil {
				t.Fatalf("ListDeliveryAttempts: %v", err)
			}
			if len(attempts) != maxAttempts {
				t.Fatalf("attempt row count = %d, want %d: %#v", len(attempts), maxAttempts, attempts)
			}
			for i, attempt := range attempts {
				wantOrdinal := i + 1
				if attempt.AttemptOrdinal != wantOrdinal {
					t.Fatalf("attempt ordinal[%d] = %d, want %d", i, attempt.AttemptOrdinal, wantOrdinal)
				}
			}
			final := attempts[len(attempts)-1]
			if final.Evidence.EvidenceRef != fmt.Sprintf("host-error-%d", maxAttempts) || final.NextAttemptAt != "" || final.ErrorCode != "temporary" {
				t.Fatalf("final attempt = %#v, want terminal evidence with no next retry", final)
			}
			if _, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{ObligationID: created.Obligation.ObligationID, ClaimOwner: "supervisor-extra", LeaseExpiresAt: now.Add(2 * time.Minute).Format(time.RFC3339Nano)}); !errors.Is(err, ErrClaimConflict) {
				t.Fatalf("terminal claim error = %v, want ErrClaimConflict", err)
			}
		})
	}
}

func TestDeliveryOutboxListAPIsFailClosedOnFuturePayloadVersions(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{ObligationID: created.Obligation.ObligationID, ClaimOwner: "supervisor-a", LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	if _, err := RecordDeliveryAttemptResult(ctx, store, AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		ResultStatus: DeliveryRetryableFailure,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-error", EvidenceRef: "host-error-future", TransportContract: "host-jsonl-v1"},
		ErrorCode:    "temporary",
	}); err != nil {
		t.Fatalf("RecordDeliveryAttemptResult: %v", err)
	}
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE progress_delivery_attempts SET payload_json = json_set(payload_json, '$.schema_version', 'loopcoder.progress_delivery_attempt.v2')`)
		return err
	}); err != nil {
		t.Fatalf("mutate attempt payload: %v", err)
	}
	if _, err := ListDeliveryAttempts(ctx, store, DeliveryAttemptFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"}); !errors.Is(err, ErrUnknownRecordVersion) {
		t.Fatalf("future attempt list error = %v, want ErrUnknownRecordVersion", err)
	}
}

func TestDeliveryOutboxRedactsEvidenceBeforePersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	credentialCanary := "AKIA" + strings.Repeat("A", 16)
	callbackToken := "callback_" + "token=" + strings.Repeat("c", 24)
	rawPrompt := "raw prompt included secret=" + strings.Repeat("p", 24)
	rawProviderOutput := "provider output contained " + credentialCanary

	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-a",
		LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	if _, err := RecordDeliveryAttemptResult(ctx, store, AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		ResultStatus: DeliveryRetryableFailure,
		Evidence: DeliveryEvidence{
			EvidenceKind:      "host-error",
			EvidenceRef:       callbackToken,
			Summary:           rawProviderOutput + " and " + rawPrompt,
			Confidence:        "exact",
			TransportContract: "host-jsonl-v1",
		},
		ErrorCode: "transport",
	}); err != nil {
		t.Fatalf("RecordDeliveryAttemptResult: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sqlite bytes: %v", err)
	}
	for _, forbidden := range []string{credentialCanary, callbackToken, rawPrompt, rawProviderOutput} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("sqlite bytes contain unredacted runtime canary %q", forbidden)
		}
	}
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	attempts, err := ListDeliveryAttempts(ctx, reopened, DeliveryAttemptFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"})
	if err != nil {
		t.Fatalf("ListDeliveryAttempts: %v", err)
	}
	encoded := mustJSON(t, attempts)
	for _, forbidden := range []string{credentialCanary, callbackToken, rawPrompt, rawProviderOutput} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("canonical JSON contains unredacted runtime canary %q: %s", forbidden, encoded)
		}
	}
}

func TestDeliveryOutboxDoesNotMutateUnrelatedAuthorityTables(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()
	seedOutboxAuthorityBoundaryRows(t, ctx, store)
	before := snapshotOutboxAuthorityTables(t, ctx, store)

	created, err := PersistReceiptWithObligation(ctx, store, baseReceipt(), baseObligation())
	if err != nil {
		t.Fatalf("PersistReceiptWithObligation: %v", err)
	}
	claim, err := ClaimDeliveryObligation(ctx, store, ClaimRequest{
		ObligationID:   created.Obligation.ObligationID,
		ClaimOwner:     "supervisor-boundary",
		LeaseExpiresAt: fixedTime.Add(time.Minute).Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("ClaimDeliveryObligation: %v", err)
	}
	if _, err := RecordDeliveryAttemptResult(ctx, store, AttemptResultRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		ResultStatus: DeliveryDeliveredUnacknowledged,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-accepted", EvidenceRef: "host-event-boundary", TransportContract: "host-jsonl-v1"},
	}); err != nil {
		t.Fatalf("RecordDeliveryAttemptResult: %v", err)
	}
	if _, err := AcknowledgeDelivery(ctx, store, AcknowledgmentRequest{
		ObligationID: created.Obligation.ObligationID,
		ClaimOwner:   claim.ClaimOwner,
		Generation:   claim.ClaimGeneration,
		Evidence:     DeliveryEvidence{EvidenceKind: "host-visible", EvidenceRef: "host-visible-boundary", TransportContract: "host-jsonl-v1"},
	}); err != nil {
		t.Fatalf("AcknowledgeDelivery: %v", err)
	}
	after := snapshotOutboxAuthorityTables(t, ctx, store)
	if before != after {
		t.Fatalf("authority tables changed:\nbefore=%s\nafter=%s", before, after)
	}
}

type writeErrorStore struct {
	storage.Store
	err error
}

func (s writeErrorStore) WithWriteTx(context.Context, func(storage.Tx) error) error {
	return s.err
}

func baseObligation() DeliveryObligation {
	return DeliveryObligation{
		OriginKind:        "progress-receipt",
		OriginID:          "corr_progress",
		SinkKind:          "host",
		SinkID:            "attached-session",
		TransportContract: "host-jsonl-v1",
		AckPolicy:         DeliveryAckPolicyRequired,
		MaxAttempts:       2,
	}
}

func assertProgressCount(t *testing.T, ctx context.Context, store storage.Store, query string, want int) {
	t.Helper()
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, query).Scan(&count)
	}); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if count != want {
		t.Fatalf("count for %q = %d, want %d", query, count, want)
	}
}

func deliveryObligationPayload(t *testing.T, ctx context.Context, store storage.Store, obligationID string) string {
	t.Helper()
	var payload string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload_json FROM progress_delivery_obligations WHERE obligation_id = ?`, obligationID).Scan(&payload)
	}); err != nil {
		t.Fatalf("load delivery obligation payload: %v", err)
	}
	return payload
}

func seedOutboxAuthorityBoundaryRows(t *testing.T, ctx context.Context, store storage.Store) {
	t.Helper()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		for _, statement := range []string{
			`INSERT INTO runs(id, project_id, status, updated_at, root_run_id, depth, origin, created_at)
				VALUES ('run-boundary', 'proj_progress', 'running', '2026-07-13T12:00:00Z', 'run-boundary', 0, 'test', '2026-07-13T12:00:00Z')`,
			`INSERT INTO run_claims(run_id, executor_id, claim_generation, claimed_at, lease_expires_at, heartbeat_at, phase, provider_idempotency_key, provider_receipt)
				VALUES ('run-boundary', 'worker-boundary', 7, '2026-07-13T12:00:00Z', '2026-07-13T12:10:00Z', '2026-07-13T12:00:01Z', 'executing', 'provider-clock-boundary', 'provider-receipt-boundary')`,
			`INSERT INTO delivery_runs(delivery_run_id, run_id, schema_version, record_version, project_id, root_run_id, parent_run_id, state, intent_summary, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint, policy_version, max_side_effect_class, approval_status, override_status, created_at, updated_at, created_by_json, updated_by_json, host_json)
				VALUES ('run_progress', 'run-boundary', 'loopcoder.delivery_run.v1', 1, 'proj_progress', 'run-boundary', NULL, 'approved', 'boundary fixture', 'sha256:input-boundary', 'sha256:policy-boundary', 'sha256:plan-boundary', 'sha256:auth-boundary', '0805.agent_federation.v1', 'repo-write', 'approved', 'none', '2026-07-13T12:00:00Z', '2026-07-13T12:00:00Z', '{}', '{}', '{}')`,
			`INSERT INTO provider_installations(provider_installation_id, schema_version, record_version, scope, project_id, adapter_id, adapter_declaration_id, provider_display_name, executable_name, executable_identity_json, canonical_path_redacted, discovery_source, discovery_order, platform, version_confidence, installation_state, usable_for_invocation, created_at, updated_at, captured_at, freshness_state, confidence, side_effect_class, classification, payload_json)
				VALUES ('pinst-boundary', 'loopcoder.provider_installation.v1', 1, 'project', 'proj_progress', 'adapter-boundary', 'adapterdecl-boundary', 'provider boundary', 'provider-bin', '{}', '/redacted/provider-bin', 'fixture', 1, 'darwin-arm64', 'exact', 'ready', 'yes', '2026-07-13T12:00:00Z', '2026-07-13T12:00:00Z', '2026-07-13T12:00:00Z', 'fresh', 'exact', 'none', 'local-fixture', '{}')`,
			`INSERT INTO quota_telemetry_sources(quota_source_id, schema_version, record_version, adapter_id, source_kind, source_key, source_schema_version, network_declared, payload_json)
				VALUES ('qsrc-boundary', 'loopcoder.quota_source.v1', 1, 'adapter-boundary', 'fixture', 'quota-boundary', 'fixture.quota.v1', 0, '{}')`,
			`INSERT INTO quota_snapshots(quota_snapshot_id, quota_source_id, source_kind, adapter_id, scope_key, quantity_kind, unit, window_kind, confidence, freshness_state, captured_at, payload_json)
				VALUES ('qsnap-boundary', 'qsrc-boundary', 'fixture', 'adapter-boundary', 'project:proj_progress', 'requests', 'request', 'daily', 'exact', 'fresh', '2026-07-13T12:00:00Z', '{}')`,
			`INSERT INTO budget_policies(budget_policy_id, project_id, delivery_run_id, task_id, sub_agent_id, adapter_id, account_profile_id, model_capability_id, scope_kind, scope_key, quantity_kind, unit, value_scale, window_kind, policy_mode, overflow_behavior, ceiling_value, policy_version, payload_json)
				VALUES ('bpol-boundary', 'proj_progress', 'run_progress', '', '', 'adapter-boundary', 'acct-boundary', 'mcap-boundary', 'project', 'project:proj_progress', 'requests', 'request', 0, 'daily', 'hard', 'block', 10, 'policy-boundary', '{}')`,
			`INSERT INTO budget_aggregates(budget_policy_id, reserved_value, committed_value, updated_at)
				VALUES ('bpol-boundary', 2, 1, '2026-07-13T12:00:00Z')`,
			`INSERT INTO budget_reservations(budget_reservation_id, idempotency_key, request_fingerprint, requester_id, quantity_kind, unit, value_scale, requested_value, reserved_value, committed_value, released_value, state, generation, lease_expires_at, scope_key, policy_ids_json, payload_json, created_at, updated_at)
				VALUES ('bres-boundary', 'idem-boundary', 'sha256:budget-boundary', 'scheduler-boundary', 'requests', 'request', 0, 3, 2, 1, 0, 'active', 5, '2026-07-13T12:10:00Z', 'project:proj_progress', '["bpol-boundary"]', '{}', '2026-07-13T12:00:00Z', '2026-07-13T12:00:00Z')`,
			`INSERT INTO nested_scheduler_resource_reservations(reservation_id, run_id, parent_run_id, root_run_id, resource_kind, resource_key, executor_id, claim_generation, join_identity, state, lease_expires_at, heartbeat_at, created_at, updated_at)
				VALUES ('nsres-boundary', 'run-boundary', 'run-boundary', 'run-boundary', 'provider', 'provider-boundary', 'worker-boundary', 7, 'join-boundary', 'active', '2026-07-13T12:10:00Z', '2026-07-13T12:00:01Z', '2026-07-13T12:00:00Z', '2026-07-13T12:00:00Z')`,
			`INSERT INTO routing_decisions(routing_decision_id, schema_version, record_version, project_id, delivery_run_id, task_id, task_requirement_id, decision_key, decision_kind, routing_policy_profile_id, plan_fingerprint, policy_fingerprint, routing_fingerprint, candidate_generation_status, decision_status, chosen_candidate_id, payload_json, created_at, updated_at, decided_by_json, host_json)
				VALUES ('rdec-boundary', 'loopcoder.routing_decision.v1', 1, 'proj_progress', 'run_progress', 'task_progress', 'treq-boundary', 'route-boundary', 'routing', 'rprofile-boundary', 'sha256:plan-boundary', 'sha256:policy-boundary', 'sha256:route-boundary', 'complete', 'selected', 'candidate-boundary', '{}', '2026-07-13T12:00:00Z', '2026-07-13T12:00:00Z', '{}', '{}')`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("seed authority boundary row: %w\n%s", err, statement)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed authority boundary rows: %v", err)
	}
}

func snapshotOutboxAuthorityTables(t *testing.T, ctx context.Context, store storage.Store) string {
	t.Helper()
	tables := []string{
		"runs",
		"run_claims",
		"provider_installations",
		"quota_snapshots",
		"budget_reservations",
		"budget_aggregates",
		"nested_scheduler_resource_reservations",
		"routing_decisions",
	}
	parts := make([]string, 0, len(tables))
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		for _, table := range tables {
			var encoded string
			query := fmt.Sprintf(`SELECT COALESCE(json_group_array(json(payload)), '[]') FROM (SELECT json_object('rowid', rowid, 'payload', json_quote(payload_json)) AS payload FROM %s ORDER BY rowid)`, table)
			if table == "runs" || table == "run_claims" || table == "budget_aggregates" || table == "nested_scheduler_resource_reservations" {
				query = fmt.Sprintf(`SELECT COALESCE(json_group_array(json(payload)), '[]') FROM (SELECT json_object('rowid', rowid, 'data', quote(%s)) AS payload FROM %s ORDER BY rowid)`, authoritySnapshotConcat(table), table)
			}
			if err := tx.QueryRow(ctx, query).Scan(&encoded); err != nil {
				return err
			}
			parts = append(parts, table+"="+encoded)
		}
		return nil
	}); err != nil {
		t.Fatalf("snapshot authority tables: %v", err)
	}
	return strings.Join(parts, "\n")
}

func authoritySnapshotConcat(table string) string {
	switch table {
	case "runs":
		return `id || '|' || COALESCE(status, '') || '|' || COALESCE(updated_at, '')`
	case "run_claims":
		return `run_id || '|' || executor_id || '|' || claim_generation || '|' || lease_expires_at || '|' || heartbeat_at || '|' || phase || '|' || provider_idempotency_key || '|' || provider_receipt`
	case "budget_aggregates":
		return `budget_policy_id || '|' || reserved_value || '|' || committed_value || '|' || record_version || '|' || updated_at`
	case "nested_scheduler_resource_reservations":
		return `reservation_id || '|' || run_id || '|' || resource_kind || '|' || resource_key || '|' || executor_id || '|' || claim_generation || '|' || state || '|' || lease_expires_at || '|' || heartbeat_at`
	default:
		return `''`
	}
}
