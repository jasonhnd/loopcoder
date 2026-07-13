package progress

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestReadReceiptsBuildsViewsAndResumesFromCursor(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	fixtures := []ProgressReceipt{
		mutate(baseReceipt(), func(r *ProgressReceipt) {
			r.CorrelationID = "corr-b"
			r.CorrelationSequence = 1
			r.TaskID = "task-b"
			r.OccurredAt = "2026-07-13T12:00:06Z"
		}),
		mutate(baseReceipt(), func(r *ProgressReceipt) {
			r.CorrelationID = "corr-a"
			r.CorrelationSequence = 1
			r.TaskID = "task-a"
			r.OccurredAt = "2026-07-13T12:00:05Z"
		}),
		mutate(baseReceipt(), func(r *ProgressReceipt) {
			r.CorrelationID = "corr-a"
			r.CorrelationSequence = 2
			r.TaskID = "task-a"
			r.Status = "running"
			r.OccurredAt = "2026-07-13T12:00:05Z"
		}),
	}
	for _, receipt := range fixtures {
		if _, err := PersistReceipt(ctx, store, receipt); err != nil {
			t.Fatalf("PersistReceipt %s/%d: %v", receipt.CorrelationID, receipt.CorrelationSequence, err)
		}
	}

	first, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", Limit: 2}, fixedTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReadReceipts first: %v", err)
	}
	got := viewKeys(first.Views)
	want := "corr-a/1,corr-a/2"
	if strings.Join(got, ",") != want {
		t.Fatalf("first read order = %v, want %s", got, want)
	}
	if first.NextCursor == "" || first.Views[0].DeliveryState.State != "unsupported-pending-unacknowledged" {
		t.Fatalf("view metadata missing cursor/delivery state: %#v", first)
	}
	var stdout bytes.Buffer
	if err := RenderJSONL(&stdout, first.Views); err != nil {
		t.Fatalf("RenderJSONL: %v", err)
	}
	for lineNumber, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var view ReceiptView
		if err := json.Unmarshal([]byte(line), &view); err != nil {
			t.Fatalf("jsonl line %d did not parse: %v\n%s", lineNumber+1, err, line)
		}
		if view.RenderAuthority != "attached-consumer-write-only" {
			t.Fatalf("render authority = %q", view.RenderAuthority)
		}
	}

	second, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", After: first.NextCursor}, fixedTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReadReceipts second: %v", err)
	}
	got = viewKeys(second.Views)
	if strings.Join(got, ",") != "corr-b/1" {
		t.Fatalf("resume read order = %v, want corr-b/1", got)
	}
}

func TestReadReceiptsFiltersByCorrelationAndSkipsUnknownRecords(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	if _, err := PersistReceipt(ctx, store, mutate(baseReceipt(), func(r *ProgressReceipt) {
		r.CorrelationID = "corr-keep"
		r.CorrelationSequence = 1
	})); err != nil {
		t.Fatalf("PersistReceipt keep: %v", err)
	}
	if _, err := PersistReceipt(ctx, store, mutate(baseReceipt(), func(r *ProgressReceipt) {
		r.CorrelationID = "corr-drop"
		r.CorrelationSequence = 1
		r.TaskID = "task-drop"
	})); err != nil {
		t.Fatalf("PersistReceipt drop: %v", err)
	}
	insertUnknownProgressRecord(t, ctx, store, "corr-keep", 2)

	batch, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr-keep"}, fixedTime)
	if err != nil {
		t.Fatalf("ReadReceipts: %v", err)
	}
	if got := strings.Join(viewKeys(batch.Views), ","); got != "corr-keep/1" {
		t.Fatalf("filtered views = %s, want corr-keep/1", got)
	}
	if len(batch.Diagnostics) != 1 || batch.Diagnostics[0].Code != "progress-receipt-skipped" {
		t.Fatalf("diagnostics = %#v, want skipped unknown record", batch.Diagnostics)
	}
}

func TestHostOfflineTerminalReceiptIsConsumedOnceFromStoredCursor(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	if _, err := PersistReceipt(ctx, store, mutate(baseReceipt(), func(r *ProgressReceipt) {
		r.CorrelationID = "host-offline"
		r.CorrelationSequence = 7
		r.Phase = "supervisor"
		r.Status = "terminal"
		r.Blocker = ActionState{State: "blocked", Summary: "host offline"}
		r.NextAction = ActionState{State: "needs-human", Summary: "restart attached host"}
		r.Evidence = []EvidenceRef{{RecordKind: "terminal-receipt", RecordID: "host-offline", Summary: "attached host was offline", Classification: "local-diagnostic", Confidence: "exact"}}
	})); err != nil {
		t.Fatalf("PersistReceipt terminal: %v", err)
	}

	first, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "host-offline"}, fixedTime)
	if err != nil {
		t.Fatalf("ReadReceipts first: %v", err)
	}
	if len(first.Views) != 1 || first.NextCursor == "" {
		t.Fatalf("first read = %#v, want one terminal receipt with cursor", first)
	}
	second, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "host-offline", After: first.NextCursor}, fixedTime)
	if err != nil {
		t.Fatalf("ReadReceipts second: %v", err)
	}
	if len(second.Views) != 0 {
		t.Fatalf("cursor replay duplicated terminal receipt: %#v", second.Views)
	}
}

func TestFollowReceiptsStopsOnClosedConsumer(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	if _, err := PersistReceipt(ctx, store, baseReceipt()); err != nil {
		t.Fatalf("PersistReceipt: %v", err)
	}
	err := FollowReceipts(ctx, store, FollowOptions{ReadFilter: ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"}}, func() time.Time {
		return fixedTime
	}, func(ReceiptBatch) error {
		return io.ErrClosedPipe
	})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("FollowReceipts error = %v, want ErrClosedPipe", err)
	}
}

func viewKeys(views []ReceiptView) []string {
	out := make([]string, 0, len(views))
	for _, view := range views {
		out = append(out, fmt.Sprintf("%s/%d", view.Receipt.CorrelationID, view.Receipt.CorrelationSequence))
	}
	return out
}

func insertUnknownProgressRecord(t *testing.T, ctx context.Context, store storage.Store, correlationID string, sequence int64) {
	t.Helper()
	payload := `{"schema_version":"loopcoder.progress_receipt.v999","record_version":1}`
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO progress_receipts(
			progress_receipt_id, schema_version, record_version, project_id, delivery_run_id, run_id, task_id,
			attempt_id, attempt_ordinal, correlation_id, correlation_sequence, semantic_fingerprint, phase, status,
			provider_id, model_id, heartbeat_age_millis, progress_age_millis, occurred_at, persisted_at,
			task_counts_json, provider_json, heartbeat_json, progress_json, evidence_json, quota_budget_json,
			blocker_json, next_action_json, redaction_json, gap_reasons_json, payload_json
		) VALUES (?, ?, 999, 'proj_progress', 'run_progress', 'run_progress', 'task-progress',
			'att-progress', 1, ?, ?, ?, 'future-host', 'pending', 'future-host', 'future-model',
			-1, -1, '2026-07-13T12:00:07Z', '2026-07-13T12:00:07Z',
			'{}', '{}', '{}', '{}', '[]', '{}', '{}', '{}', '{}', '[]', ?)`,
			"prec_future_"+correlationID, SchemaProgressReceipt, correlationID, sequence, "sha256:future-"+correlationID, payload)
		return err
	}); err != nil {
		t.Fatalf("insert unknown progress record: %v", err)
	}
}
