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

	"github.com/jasonhnd/loopcoder/internal/delivery"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

var fixedTime = time.Date(2026, 7, 13, 12, 0, 5, 0, time.UTC)

func TestProgressReceiptWriteIdempotentAndDistinctTransitions(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	first, err := PersistReceipt(ctx, store, baseReceipt())
	if err != nil {
		t.Fatalf("PersistReceipt first: %v", err)
	}
	if !first.Inserted {
		t.Fatal("first write was not inserted")
	}

	duplicate := baseReceipt()
	duplicate.ProgressReceiptID = "prec_duplicate_request_id"
	replayed, err := PersistReceipt(ctx, store, duplicate)
	if err != nil {
		t.Fatalf("PersistReceipt duplicate: %v", err)
	}
	if replayed.Inserted {
		t.Fatal("duplicate semantic write inserted a second row")
	}
	if replayed.Receipt.ProgressReceiptID != first.Receipt.ProgressReceiptID || countReceipts(t, ctx, store) != 1 {
		t.Fatalf("duplicate replay mismatch: first=%s replay=%s count=%d", first.Receipt.ProgressReceiptID, replayed.Receipt.ProgressReceiptID, countReceipts(t, ctx, store))
	}

	transition := baseReceipt()
	transition.CorrelationSequence = 2
	transition.Phase = "verifying"
	transition.Status = "running"
	transition.Evidence[0].Summary = "worker finished; verifier started"
	second, err := PersistReceipt(ctx, store, transition)
	if err != nil {
		t.Fatalf("PersistReceipt transition: %v", err)
	}
	if !second.Inserted || countReceipts(t, ctx, store) != 2 {
		t.Fatalf("distinct transition not visible: inserted=%t count=%d", second.Inserted, countReceipts(t, ctx, store))
	}

	conflict := baseReceipt()
	conflict.ProgressReceiptID = first.Receipt.ProgressReceiptID
	conflict.CorrelationSequence = 3
	conflict.Status = "running"
	if _, err := PersistReceipt(ctx, store, conflict); !errors.Is(err, ErrDuplicateReplay) {
		t.Fatalf("duplicate id with distinct semantic error = %v, want ErrDuplicateReplay", err)
	}
}

func TestProgressReplayDeterministicAcrossConcurrentCorrelations(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	defer store.Close()

	for _, receipt := range []ProgressReceipt{
		mutate(baseReceipt(), func(r *ProgressReceipt) { r.CorrelationID = "corr-b"; r.CorrelationSequence = 1; r.TaskID = "task_b" }),
		mutate(baseReceipt(), func(r *ProgressReceipt) {
			r.CorrelationID = "corr-a"
			r.CorrelationSequence = 2
			r.TaskID = "task_a"
			r.Status = "running"
		}),
		mutate(baseReceipt(), func(r *ProgressReceipt) {
			r.CorrelationID = "corr-a"
			r.CorrelationSequence = 1
			r.TaskID = "task_a"
			r.Status = "pending"
		}),
	} {
		if _, err := PersistReceipt(ctx, store, receipt); err != nil {
			t.Fatalf("PersistReceipt %s/%d: %v", receipt.CorrelationID, receipt.CorrelationSequence, err)
		}
	}

	replay, err := Replay(ctx, store, ReplayFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	got := make([]string, 0, len(replay))
	for _, receipt := range replay {
		got = append(got, fmt.Sprintf("%s/%d", receipt.CorrelationID, receipt.CorrelationSequence))
	}
	want := []string{"corr-a/1", "corr-a/2", "corr-b/1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("replay order = %v, want %v", got, want)
	}
}

func TestProgressReceiptRestartAndListAPIs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	written, err := PersistReceipt(ctx, store, baseReceipt())
	if err != nil {
		t.Fatalf("PersistReceipt: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime.Add(time.Minute) }})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	loaded, err := LoadReceipt(ctx, reopened, written.Receipt.ProgressReceiptID)
	if err != nil {
		t.Fatalf("LoadReceipt: %v", err)
	}
	if loaded.SemanticFingerprint != written.Receipt.SemanticFingerprint {
		t.Fatalf("loaded fingerprint = %q, want %q", loaded.SemanticFingerprint, written.Receipt.SemanticFingerprint)
	}
	list, err := ListReceipts(ctx, reopened, ListFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress", Status: "pending"})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	if len(list) != 1 || list[0].ProgressReceiptID != written.Receipt.ProgressReceiptID {
		t.Fatalf("list = %#v, want written receipt", list)
	}
}

func TestProgressReceiptRedactsBeforePersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	defer store.Close()

	awsCanary := "AKIA" + strings.Repeat("A", 16)
	apiCanary := "sk-" + strings.Repeat("b", 24)
	bearerCanary := "Bearer " + strings.Repeat("c", 24)
	apiAssignment := "api_" + "key" + "="
	tokenAssignment := "to" + "ken" + "="
	rawProviderOutput := "provider stdout contained " + awsCanary + " " + apiAssignment + apiCanary + " " + bearerCanary
	receipt := baseReceipt()
	receipt.Evidence = append(receipt.Evidence, EvidenceRef{
		RecordKind:     "provider-output",
		RecordID:       "stdout",
		Summary:        rawProviderOutput,
		Classification: "provider-diagnostic",
		Confidence:     "exact",
	})
	receipt.Blocker.Summary = "blocked by " + tokenAssignment + apiCanary
	receipt.NextAction.Summary = "inspect " + bearerCanary
	written, err := PersistReceipt(ctx, store, receipt)
	if err != nil {
		t.Fatalf("PersistReceipt: %v", err)
	}
	if !written.Receipt.Redaction.Redacted || written.Receipt.Redaction.SecretFindingCount < 3 {
		t.Fatalf("redaction state = %#v, want secret findings", written.Receipt.Redaction)
	}
	dbBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile db: %v", err)
	}
	dbText := string(dbBytes)
	for _, forbidden := range []string{awsCanary, apiCanary, bearerCanary, rawProviderOutput} {
		if strings.Contains(dbText, forbidden) {
			t.Fatalf("database bytes retained forbidden provider/credential text %q", forbidden)
		}
	}
	payload, err := ListReceipts(ctx, store, ListFilter{ProjectID: "proj_progress", DeliveryRunID: "run_progress"})
	if err != nil {
		t.Fatalf("ListReceipts: %v", err)
	}
	encoded := mustJSON(t, payload)
	for _, forbidden := range []string{awsCanary, apiCanary, bearerCanary, rawProviderOutput} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("API payload retained forbidden provider/credential text %q: %s", forbidden, encoded)
		}
	}
}

func TestProgressReceiptFixtureAndFutureVersionBehavior(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "progress_receipt.v1.json"))
	if err != nil {
		t.Fatalf("ReadFile fixture: %v", err)
	}
	receipt, err := DecodeReceiptJSON(data, fixedTime)
	if err != nil {
		t.Fatalf("DecodeReceiptJSON fixture: %v", err)
	}
	if receipt.SchemaVersion != SchemaProgressReceipt || receipt.RecordVersion != 1 || receipt.SemanticFingerprint == "" || receipt.ProgressReceiptID == "" {
		t.Fatalf("fixture normalized receipt = %#v", receipt)
	}

	minimal := []byte(`{"schema_version":"loopcoder.progress_receipt.v1","record_version":1,"project_id":"proj_progress","delivery_run_id":"run_progress","correlation_id":"corr_minimal","correlation_sequence":1,"phase":"unknown","status":"unknown"}`)
	decoded, err := DecodeReceiptJSON(minimal, fixedTime)
	if err != nil {
		t.Fatalf("DecodeReceiptJSON minimal v0.8 payload: %v", err)
	}
	if decoded.TaskCounts.Total != -1 || decoded.Provider.ProviderID != Unknown || decoded.Heartbeat.AgeMillis != -1 {
		t.Fatalf("minimal defaults did not preserve explicit unknowns: %#v", decoded)
	}

	future := []byte(`{"schema_version":"loopcoder.progress_receipt.v2","record_version":1}`)
	if _, err := DecodeReceiptJSON(future, fixedTime); !errors.Is(err, ErrUnknownRecordVersion) {
		t.Fatalf("future schema error = %v, want ErrUnknownRecordVersion", err)
	}
}

func TestProgressReceiptStorageFailureDoesNotPersist(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, ctx)
	inserted, err := PersistReceipt(ctx, store, baseReceipt())
	if err != nil || !inserted.Inserted {
		t.Fatalf("initial persist = %#v / %v", inserted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = PersistReceipt(ctx, store, mutate(baseReceipt(), func(r *ProgressReceipt) {
		r.CorrelationSequence = 2
		r.Status = "running"
	}))
	if err == nil {
		t.Fatal("PersistReceipt after close succeeded, want storage error")
	}
}

func TestProgressReceiptRequiresExistingProject(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	if _, err := PersistReceipt(ctx, store, baseReceipt()); !errors.Is(err, ErrMissingReference) {
		t.Fatalf("missing project error = %v, want ErrMissingReference", err)
	}
	if countReceipts(t, ctx, store) != 0 {
		t.Fatalf("missing project write persisted rows")
	}
}

func TestProgressReceiptMigrationV25Registered(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "loopcoder.db")
	store, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	health, err := store.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.SchemaVersion != storage.CurrentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", health.SchemaVersion, storage.CurrentSchemaVersion)
	}
	var migrationName string
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT name FROM migrations WHERE version = 25`).Scan(&migrationName)
	}); err != nil {
		t.Fatalf("query v25 migration: %v", err)
	}
	if migrationName != "canonical progress receipts" {
		t.Fatalf("v25 migration name = %q", migrationName)
	}

	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `DROP TABLE progress_receipts`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DROP TABLE handoff_transactions`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM migrations WHERE version IN (25, 26)`)
		return err
	}); err != nil {
		t.Fatalf("simulate v24 database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := storage.Open(ctx, storage.Options{Path: path, Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("reopen simulated v24 database: %v", err)
	}
	defer reopened.Close()
	if !tableExists(t, ctx, reopened, "progress_receipts") {
		t.Fatal("v25 migration did not recreate progress_receipts")
	}
}

func baseReceipt() ProgressReceipt {
	return ProgressReceipt{
		ProjectID:           "proj_progress",
		DeliveryRunID:       "run_progress",
		RunID:               "run_progress",
		TaskID:              "task_progress",
		AttemptID:           "att_progress_1",
		AttemptOrdinal:      1,
		CorrelationID:       "corr_progress",
		CorrelationSequence: 1,
		Phase:               "dispatching",
		Status:              "pending",
		TaskCounts:          TaskCounts{Total: 2, Ready: 1, Running: 1, Succeeded: 0, Failed: 0, Blocked: 0, Unknown: 0},
		Provider: ProviderIdentity{
			ProviderID:           "codex",
			ModelID:              "gpt-5.5",
			AccountProfileID:     Unknown,
			ModelCapabilityID:    Unknown,
			ProviderConfidence:   "exact",
			ProviderInstallation: Unknown,
		},
		Heartbeat:   AgeEvidence{State: "exact", ObservedAt: "2026-07-13T12:00:00Z", AgeMillis: 5000},
		Progress:    AgeEvidence{State: "exact", ObservedAt: "2026-07-13T12:00:01Z", AgeMillis: 4000},
		Evidence:    []EvidenceRef{{RecordKind: "attempt-sidecar", RecordID: "attempt-1", Summary: "attempt sidecar updated", Classification: "local-diagnostic", Confidence: "exact"}},
		QuotaBudget: QuotaBudgetState{State: Unknown, Confidence: Unknown, BudgetPolicyID: Unknown, BudgetReservationID: Unknown, RemainingQuantity: -1, Unit: Unknown, GapReasons: []string{"not-collected"}},
		Blocker:     ActionState{State: "none"},
		NextAction:  ActionState{State: "continue", Summary: "wait for provider completion"},
		OccurredAt:  "2026-07-13T12:00:05Z",
	}
}

func mutate(receipt ProgressReceipt, fn func(*ProgressReceipt)) ProgressReceipt {
	fn(&receipt)
	return receipt
}

func newStore(t *testing.T, ctx context.Context) storage.Store {
	t.Helper()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return fixedTime }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	insertProject(t, ctx, store)
	return store
}

func insertProject(t *testing.T, ctx context.Context, store storage.Store) {
	t.Helper()
	if err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, local_path_canonical, display_name, identity_source, created_at, updated_at)
			VALUES ('proj_progress', '/repo', '/repo', 'repo', 'local-path', '2026-07-13T12:00:00Z', '2026-07-13T12:00:00Z')
			ON CONFLICT(id) DO NOTHING`)
		return err
	}); err != nil {
		t.Fatalf("insert project: %v", err)
	}
}

func countReceipts(t *testing.T, ctx context.Context, store storage.Store) int {
	t.Helper()
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM progress_receipts`).Scan(&count)
	}); err != nil {
		t.Fatalf("count receipts: %v", err)
	}
	return count
}

func tableExists(t *testing.T, ctx context.Context, store storage.Store, table string) bool {
	t.Helper()
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count)
	}); err != nil {
		t.Fatalf("tableExists %s: %v", table, err)
	}
	return count > 0
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := delivery.CanonicalJSON(value)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	return string(data)
}
