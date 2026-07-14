package progress

import (
	"context"
	"errors"
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

func TestDeliveryOutboxRedactsEvidenceBeforePersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	canary := "AKIA" + strings.Repeat("A", 16)

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
			EvidenceRef:       "callback-token=" + canary,
			Summary:           "provider output contained " + canary,
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
	if strings.Contains(string(data), canary) {
		t.Fatal("sqlite bytes contain unredacted runtime canary")
	}
}

func baseObligation() DeliveryObligation {
	return DeliveryObligation{
		OriginKind:        "progress-receipt",
		OriginID:          "corr_progress",
		SinkKind:          "host",
		SinkID:            "attached-session",
		TransportContract: "host-jsonl-v1",
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
