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
			r.CorrelationID = "parent/child:alpha"
			r.CorrelationSequence = 1
			r.TaskID = "task-unicode"
			r.Phase = "dispatching"
			r.Status = "pending"
			r.NextAction = ActionState{State: "continue", Summary: "wait for provider completion - unicode " + string(rune(0x03A9))}
			r.OccurredAt = "2026-07-13T12:00:04Z"
		}),
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
	want := "parent/child:alpha/1,corr-a/1"
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
	var human narrowWriter
	if err := RenderHuman(&human, first.Views); err != nil {
		t.Fatalf("RenderHuman redirected/narrow writer: %v", err)
	}
	if strings.Contains(human.String(), "\x1b[") || !strings.Contains(human.String(), "unsupported-pending-unacknowledged") {
		t.Fatalf("human render not no-color/redirect-safe:\n%s", human.String())
	}

	second, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", After: first.NextCursor}, fixedTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReadReceipts second: %v", err)
	}
	got = viewKeys(second.Views)
	if strings.Join(got, ",") != "corr-a/2,corr-b/1" {
		t.Fatalf("resume read order = %v, want corr-a/2,corr-b/1", got)
	}
}

func TestRenderJSONLWriteFailuresDoNotCorruptEarlierCompleteRecords(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	for i, correlation := range []string{"corr-first", "corr-second"} {
		receipt := mutate(baseReceipt(), func(r *ProgressReceipt) {
			r.CorrelationID = correlation
			r.CorrelationSequence = int64(i + 1)
			r.OccurredAt = fixedTime.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano)
		})
		if _, err := PersistReceipt(ctx, store, receipt); err != nil {
			t.Fatalf("PersistReceipt %s: %v", correlation, err)
		}
	}
	batch, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", Limit: 2}, fixedTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReadReceipts: %v", err)
	}

	firstFail := &jsonlFailWriter{failOnWrite: 1}
	if err := RenderJSONL(firstFail, batch.Views); err == nil {
		t.Fatal("RenderJSONL first write succeeded, want failure")
	}
	if firstFail.buf.Len() != 0 {
		t.Fatalf("first failed write accepted bytes: %q", firstFail.buf.String())
	}

	laterFail := &jsonlFailWriter{failOnWrite: 2, partialBytes: 7}
	if err := RenderJSONL(laterFail, batch.Views); err == nil {
		t.Fatal("RenderJSONL later write succeeded, want failure")
	}
	lines := strings.Split(laterFail.buf.String(), "\n")
	var first ReceiptView
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first complete JSONL record corrupted: %v\n%s", err, laterFail.buf.String())
	}
	if first.Receipt.CorrelationID != "corr-first" {
		t.Fatalf("first record correlation = %q", first.Receipt.CorrelationID)
	}
	if len(lines) < 2 || len(lines[1]) != laterFail.partialBytes {
		t.Fatalf("short-write boundary not documented by fixture: lines=%q", lines)
	}
}

type jsonlFailWriter struct {
	buf          bytes.Buffer
	writes       int
	failOnWrite  int
	partialBytes int
}

func (w *jsonlFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failOnWrite {
		n := w.partialBytes
		if n > len(p) {
			n = len(p)
		}
		if n > 0 {
			_, _ = w.buf.Write(p[:n])
		}
		return n, errors.New("short writer")
	}
	return w.buf.Write(p)
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
	insertPartialProgressRecord(t, ctx, store, "corr-keep", 3)

	batch, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr-keep"}, fixedTime)
	if err != nil {
		t.Fatalf("ReadReceipts: %v", err)
	}
	if got := strings.Join(viewKeys(batch.Views), ","); got != "corr-keep/1" {
		t.Fatalf("filtered views = %s, want corr-keep/1", got)
	}
	if len(batch.Diagnostics) != 2 || batch.Diagnostics[0].Code != "progress-receipt-skipped" || batch.Diagnostics[1].Code != "progress-receipt-skipped" {
		t.Fatalf("diagnostics = %#v, want skipped unknown and partial records", batch.Diagnostics)
	}
}

func TestReadReceiptsRedactsCorruptValidationDiagnosticBeforeBounding(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	canary := secretCanary()
	insertCorruptTimestampProgressRecord(t, ctx, store, "corr-secret", 1, "api_"+"key="+canary)

	batch, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr-secret"}, fixedTime)
	if err != nil {
		t.Fatalf("ReadReceipts: %v", err)
	}
	if len(batch.Views) != 0 || len(batch.Diagnostics) != 1 {
		t.Fatalf("batch = %#v, want one diagnostic and no views", batch)
	}
	diagnostic := batch.Diagnostics[0]
	if diagnostic.Code != "progress-receipt-skipped" || !strings.Contains(diagnostic.Message, "ErrInvalidRecord") || !strings.Contains(diagnostic.Message, "[REDACTED]") {
		t.Fatalf("diagnostic = %#v, want typed redacted warning", diagnostic)
	}
	assertNoCanaryFragments(t, diagnostic.Message, canary)
	if len([]rune(diagnostic.Message)) > maxTextRunes {
		t.Fatalf("diagnostic length = %d, want bounded", len([]rune(diagnostic.Message)))
	}
}

func TestReadReceiptsCursorScopeMismatchIsRejectedButR1CursorStillWorks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	for _, receipt := range []ProgressReceipt{
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
			r.OccurredAt = "2026-07-13T12:00:06Z"
		}),
	} {
		if _, err := PersistReceipt(ctx, store, receipt); err != nil {
			t.Fatalf("PersistReceipt: %v", err)
		}
	}

	first, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr-a", TaskID: "task-a", Limit: 1}, fixedTime)
	if err != nil {
		t.Fatalf("ReadReceipts first: %v", err)
	}
	if len(first.Views) != 1 || first.NextCursor == "" {
		t.Fatalf("first = %#v, want one view and cursor", first)
	}
	_, err = ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr-other", TaskID: "task-a", After: first.NextCursor}, fixedTime)
	if !errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), "cursor scope mismatch") {
		t.Fatalf("scope mismatch err = %v, want typed cursor mismatch", err)
	}

	oldCursor := encodeCursor(cursorPayload{
		Version:             1,
		OccurredAt:          first.Views[0].Receipt.OccurredAt,
		CorrelationID:       first.Views[0].Receipt.CorrelationID,
		CorrelationSequence: first.Views[0].Receipt.CorrelationSequence,
		StorageOrder:        first.Views[0].StorageOrder,
	})
	second, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", CorrelationID: "corr-a", TaskID: "task-a", After: oldCursor}, fixedTime)
	if err != nil {
		t.Fatalf("ReadReceipts old r1 cursor: %v", err)
	}
	if len(second.Views) != 1 || second.Views[0].Receipt.CorrelationSequence != 2 {
		t.Fatalf("old cursor replay = %#v, want second receipt", second.Views)
	}
}

func TestFollowReceiptsCancellationAndNilDependencies(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := runFollowPromptly(t, canceled, store, func(ReceiptBatch) error {
		t.Fatal("emit called after cancellation with no rows")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("empty follow cancel err = %v, want context.Canceled", err)
	}

	if _, err := PersistReceipt(ctx, store, baseReceipt()); err != nil {
		t.Fatalf("PersistReceipt: %v", err)
	}
	canceledRows, cancelRows := context.WithCancel(ctx)
	cancelRows()
	err = runFollowPromptly(t, canceledRows, store, func(ReceiptBatch) error {
		t.Fatal("emit called after cancellation with queued rows")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("queued follow cancel err = %v, want context.Canceled", err)
	}

	if err := FollowReceipts(ctx, nil, FollowOptions{}, func() time.Time { return fixedTime }, func(ReceiptBatch) error { return nil }); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("nil store err = %v, want ErrInvalidRecord", err)
	}
	if err := FollowReceipts(ctx, store, FollowOptions{}, nil, func(ReceiptBatch) error { return nil }); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("nil clock err = %v, want ErrInvalidRecord", err)
	}
}

func TestHostJSONLContractIsCanonicalAndUnacknowledged(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	hosts := []string{"local", "codex", "claude", "paseo", "unknown", "future-host"}
	for i, host := range hosts {
		sequence := int64(i + 1)
		if _, err := PersistReceipt(ctx, store, mutate(baseReceipt(), func(r *ProgressReceipt) {
			r.CorrelationID = "host-" + host
			r.CorrelationSequence = sequence
			r.Provider.ProviderID = host
			r.Provider.ModelID = "model-" + host
			r.OccurredAt = fixedTime.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		})); err != nil {
			t.Fatalf("PersistReceipt host %s: %v", host, err)
		}
	}

	batch, err := ReadReceipts(ctx, store, ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"}, fixedTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("ReadReceipts: %v", err)
	}
	var stdout bytes.Buffer
	if err := RenderJSONL(&stdout, batch.Views); err != nil {
		t.Fatalf("RenderJSONL: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != len(hosts) {
		t.Fatalf("jsonl lines = %d, want %d:\n%s", len(lines), len(hosts), stdout.String())
	}
	for i, line := range lines {
		var view ReceiptView
		if err := json.Unmarshal([]byte(line), &view); err != nil {
			t.Fatalf("host jsonl line %d did not parse: %v\n%s", i+1, err, line)
		}
		if view.Provider.ProviderID != hosts[i] || view.DeliveryState.State != "unsupported-pending-unacknowledged" || view.RenderAuthority != "attached-consumer-write-only" {
			t.Fatalf("host view %d = %#v", i+1, view)
		}
		if view.DeliveryState.Authority != "durable-delivery-evidence" || !strings.Contains(view.DeliveryState.Reason, "no durable acknowledgement") {
			t.Fatalf("host jsonl did not keep honest unsupported delivery state:\n%s", line)
		}
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

type narrowWriter struct {
	bytes.Buffer
}

func (w *narrowWriter) Write(p []byte) (int, error) {
	return w.Buffer.Write(p)
}

func secretCanary() string {
	return "sk-" + strings.Repeat("Ab9_", 8)
}

func assertNoCanaryFragments(t *testing.T, text, canary string) {
	t.Helper()
	for _, forbidden := range []string{canary, canary[:8], canary[len(canary)-8:]} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("text leaked canary fragment %q:\n%s", forbidden, text)
		}
	}
}

func runFollowPromptly(t *testing.T, ctx context.Context, store storage.Store, emit func(ReceiptBatch) error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- FollowReceipts(ctx, store, FollowOptions{ReadFilter: ReadFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"}, PollInterval: time.Hour}, func() time.Time {
			return fixedTime
		}, emit)
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(250 * time.Millisecond):
		t.Fatal("FollowReceipts did not return promptly after cancellation")
		return nil
	}
}

func insertCorruptTimestampProgressRecord(t *testing.T, ctx context.Context, store storage.Store, correlationID string, sequence int64, invalidTimestamp string) {
	t.Helper()
	receipt, err := NormalizeReceipt(mutate(baseReceipt(), func(r *ProgressReceipt) {
		r.CorrelationID = correlationID
		r.CorrelationSequence = sequence
	}), fixedTime)
	if err != nil {
		t.Fatalf("NormalizeReceipt corrupt fixture base: %v", err)
	}
	receipt.OccurredAt = invalidTimestamp
	payload, err := canonicalJSON(receipt)
	if err != nil {
		t.Fatalf("canonical corrupt payload: %v", err)
	}
	insertRawProgressRecord(t, ctx, store, receipt, string(payload))
}

func insertPartialProgressRecord(t *testing.T, ctx context.Context, store storage.Store, correlationID string, sequence int64) {
	t.Helper()
	receipt, err := NormalizeReceipt(mutate(baseReceipt(), func(r *ProgressReceipt) {
		r.CorrelationID = correlationID
		r.CorrelationSequence = sequence
	}), fixedTime)
	if err != nil {
		t.Fatalf("NormalizeReceipt partial fixture base: %v", err)
	}
	payload := `{"schema_version":"loopcoder.progress_receipt.v1","record_version":1}`
	insertRawProgressRecord(t, ctx, store, receipt, payload)
}

func insertRawProgressRecord(t *testing.T, ctx context.Context, store storage.Store, receipt ProgressReceipt, payload string) {
	t.Helper()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO progress_receipts(
			progress_receipt_id, schema_version, record_version, project_id, delivery_run_id, run_id, task_id,
			attempt_id, attempt_ordinal, correlation_id, correlation_sequence, semantic_fingerprint, phase, status,
			provider_id, model_id, heartbeat_age_millis, progress_age_millis, occurred_at, persisted_at,
			task_counts_json, provider_json, heartbeat_json, progress_json, evidence_json, quota_budget_json,
			blocker_json, next_action_json, redaction_json, gap_reasons_json, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}', '{}', '[]', '{}', '{}', '{}', '{}', '[]', ?)`,
			receipt.ProgressReceiptID+"_"+fmt.Sprint(receipt.CorrelationSequence), SchemaProgressReceipt, 1, receipt.ProjectID, receipt.DeliveryRunID, receipt.RunID,
			receipt.TaskID, receipt.AttemptID, receipt.AttemptOrdinal, receipt.CorrelationID, receipt.CorrelationSequence,
			receipt.SemanticFingerprint+"_"+fmt.Sprint(receipt.CorrelationSequence), receipt.Phase, receipt.Status,
			receipt.Provider.ProviderID, receipt.Provider.ModelID, receipt.Heartbeat.AgeMillis, receipt.Progress.AgeMillis,
			receipt.OccurredAt, receipt.PersistedAt, payload)
		return err
	}); err != nil {
		t.Fatalf("insert raw progress record: %v", err)
	}
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
