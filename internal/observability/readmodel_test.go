package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestDetailCanonicalPaginationRedactionAndCrossProjectIsolation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	seedBaseRun(t, ctx, store, "proj-a", "run-a")
	seedBaseRun(t, ctx, store, "proj-b", "run-b")
	insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-a-1", "corr-a-1", `{"status":"ok","token":"secret-should-not-cross"}`)
	insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-a-2", "corr-a-2", `{"status":"still-ok"}`)
	insertProgress(t, ctx, store, "proj-b", "run-b", "receipt-b-1", "corr-b-1", `{"foreign":"must-not-leak"}`)
	insertUsage(t, ctx, store, "proj-a", "run-a", "usage-a-1")
	insertTask(t, ctx, store, "proj-a", "run-a", "task-a-1", 1, "")

	detail, err := LoadDetail(ctx, store, Options{
		ProjectID:     "proj-a",
		DeliveryRunID: "run-a",
		Limit:         3,
		Now:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("LoadDetail returned error: %v", err)
	}
	if !detail.Page.Truncated || detail.Page.NextCursor == "" || detail.Page.Returned != 3 {
		t.Fatalf("page = %#v, want bounded continuation after 3 rows", detail.Page)
	}
	data, err := DetailJSON(detail)
	if err != nil {
		t.Fatalf("DetailJSON returned error: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{"secret-should-not-cross", "must-not-leak", "receipt-b-1", "proj-b", "run-b"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("canonical detail leaked %q:\n%s", forbidden, text)
		}
	}
	if !strings.Contains(text, `"payload_exposed":false`) {
		t.Fatalf("canonical detail missing redaction contract:\n%s", text)
	}

	replayed, err := LoadDetail(ctx, store, Options{
		ProjectID:     "proj-a",
		DeliveryRunID: "run-a",
		Limit:         3,
		Now:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("LoadDetail replay returned error: %v", err)
	}
	replayedData, err := DetailJSON(replayed)
	if err != nil {
		t.Fatalf("DetailJSON replay returned error: %v", err)
	}
	if string(replayedData) != text {
		t.Fatalf("canonical JSON changed across replay:\nfirst=%s\nsecond=%s", text, string(replayedData))
	}

	next, err := LoadDetail(ctx, store, Options{
		ProjectID:     "proj-a",
		DeliveryRunID: "run-a",
		Cursor:        detail.Page.NextCursor,
		Limit:         3,
		Now:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("LoadDetail next page returned error: %v", err)
	}
	if next.Page.Truncated {
		t.Fatalf("second page unexpectedly truncated: %#v", next.Page)
	}
}

func TestDefaultObservedAtIsDurableAndRestartStable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openTestStoreAt(t, path)
	seedBaseRun(t, ctx, store, "proj-a", "run-a")
	insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-a-1", "corr-a-1", `{"status":"ok"}`)
	insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-a-2", "corr-a-2", `{"status":"still-ok"}`)

	firstSummary, err := LoadSummary(ctx, store, Options{ProjectID: "proj-a", DeliveryRunID: "run-a"})
	if err != nil {
		t.Fatalf("LoadSummary returned error: %v", err)
	}
	firstDetail, err := LoadDetail(ctx, store, Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Limit: 1, Sections: []string{"progress"}})
	if err != nil {
		t.Fatalf("LoadDetail returned error: %v", err)
	}
	if firstSummary.Snapshot.ObservedAt != ts() || firstDetail.Snapshot.ObservedAt != ts() {
		t.Fatalf("observed_at summary=%q detail=%q, want durable fixture time %q", firstSummary.Snapshot.ObservedAt, firstDetail.Snapshot.ObservedAt, ts())
	}
	firstSummaryJSON := mustCanonical(t, firstSummary)
	firstDetailJSON := mustCanonical(t, firstDetail)
	cursor := firstDetail.Page.NextCursor
	if cursor == "" {
		t.Fatal("first detail did not return a continuation cursor")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened := openTestStoreAt(t, path)
	defer reopened.Close()
	secondSummary, err := LoadSummary(ctx, reopened, Options{ProjectID: "proj-a", DeliveryRunID: "run-a"})
	if err != nil {
		t.Fatalf("LoadSummary after reopen returned error: %v", err)
	}
	secondDetail, err := LoadDetail(ctx, reopened, Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Limit: 1, Sections: []string{"progress"}})
	if err != nil {
		t.Fatalf("LoadDetail after reopen returned error: %v", err)
	}
	if got := mustCanonical(t, secondSummary); string(got) != string(firstSummaryJSON) {
		t.Fatalf("summary JSON changed after reopen:\nfirst=%s\nsecond=%s", firstSummaryJSON, got)
	}
	if got := mustCanonical(t, secondDetail); string(got) != string(firstDetailJSON) {
		t.Fatalf("detail JSON changed after reopen:\nfirst=%s\nsecond=%s", firstDetailJSON, got)
	}
	if _, err := LoadDetail(ctx, reopened, Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Limit: 1, Sections: []string{"progress"}, Cursor: cursor}); err != nil {
		t.Fatalf("unchanged restart rejected continuation cursor: %v", err)
	}
}

func TestDetailCursorDetectsRelevantMutationBetweenPages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, context.Context, storage.Store)
	}{
		{
			name: "insert",
			mutate: func(t *testing.T, ctx context.Context, store storage.Store) {
				insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-page-999", "corr-page-999", `{"status":"inserted"}`)
			},
		},
		{
			name: "update",
			mutate: func(t *testing.T, ctx context.Context, store storage.Store) {
				mustWrite(t, ctx, store, func(tx storage.Tx) error {
					_, err := tx.Exec(ctx, `UPDATE progress_receipts SET status = 'mutated', persisted_at = ?, payload_json = ? WHERE progress_receipt_id = 'receipt-page-002'`, "2026-01-02T03:04:06Z", `{"status":"mutated"}`)
					return err
				})
			},
		},
		{
			name: "delete",
			mutate: func(t *testing.T, ctx context.Context, store storage.Store) {
				mustWrite(t, ctx, store, func(tx storage.Tx) error {
					_, err := tx.Exec(ctx, `DELETE FROM progress_receipts WHERE progress_receipt_id = 'receipt-page-003'`)
					return err
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t)
			defer store.Close()
			seedBaseRun(t, ctx, store, "proj-a", "run-a")
			for i := 0; i < 5; i++ {
				insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-page-"+pad3(i), "corr-page-"+pad3(i), `{"status":"ok"}`)
			}
			first, err := LoadDetail(ctx, store, Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Sections: []string{"progress"}, Limit: 2})
			if err != nil {
				t.Fatalf("LoadDetail first page returned error: %v", err)
			}
			if first.Page.NextCursor == "" {
				t.Fatal("first page did not return a continuation cursor")
			}
			tc.mutate(t, ctx, store)
			_, err = LoadDetail(ctx, store, Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Sections: []string{"progress"}, Limit: 2, Cursor: first.Page.NextCursor})
			var qerr *QueryError
			if !errors.As(err, &qerr) || qerr.Code != ErrConsistencyGapCode {
				t.Fatalf("continuation error = %v, want consistency gap", err)
			}
		})
	}
}

func TestDetailCursorReuseFailsClosedWithoutScopeLeak(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	seedBaseRun(t, ctx, store, "proj-a", "run-a")
	seedBaseRun(t, ctx, store, "proj-b", "run-b")
	for i := 0; i < 4; i++ {
		insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-a-"+pad3(i), "corr-a-"+pad3(i), `{"status":"ok"}`)
		insertProgress(t, ctx, store, "proj-b", "run-b", "receipt-b-"+pad3(i), "corr-b-"+pad3(i), `{"status":"ok"}`)
	}
	first, err := LoadDetail(ctx, store, Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Sections: []string{"progress"}, Limit: 2})
	if err != nil {
		t.Fatalf("LoadDetail first page returned error: %v", err)
	}
	if first.Page.NextCursor == "" {
		t.Fatal("first page did not return a continuation cursor")
	}
	tampered, _, err := decodeCursor(first.Page.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	tampered.Ordering = "other-order"
	tamperedCursor, err := encodeCursor(tampered)
	if err != nil {
		t.Fatalf("encode tampered cursor: %v", err)
	}
	cases := []struct {
		name string
		opts Options
	}{
		{name: "project", opts: Options{ProjectID: "proj-b", DeliveryRunID: "run-b", Sections: []string{"progress"}, Limit: 2, Cursor: first.Page.NextCursor}},
		{name: "run", opts: Options{ProjectID: "proj-b", DeliveryRunID: "run-a", Sections: []string{"progress"}, Limit: 2, Cursor: first.Page.NextCursor}},
		{name: "sections", opts: Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Sections: []string{"plans_tasks"}, Limit: 2, Cursor: first.Page.NextCursor}},
		{name: "limit", opts: Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Sections: []string{"progress"}, Limit: 3, Cursor: first.Page.NextCursor}},
		{name: "ordering", opts: Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Sections: []string{"progress"}, Limit: 2, Cursor: tamperedCursor}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadDetail(ctx, store, tc.opts)
			var qerr *QueryError
			if !errors.As(err, &qerr) || qerr.Code != ErrStaleCursorCode {
				t.Fatalf("cursor reuse error = %v, want stale_cursor", err)
			}
			for _, forbidden := range []string{"proj-a", "run-a", "proj-b", "run-b", "receipt-a", "receipt-b"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("cursor error leaked %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestInventoryProvenanceRemainsProjectScoped(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	seedBaseRun(t, ctx, store, "proj-a", "run-a")
	insertProviderInstallation(t, ctx, store, "proj-a", "pinst-a")

	detail, err := LoadDetail(ctx, store, Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Sections: []string{"inventory"}, Limit: 10})
	if err != nil {
		t.Fatalf("LoadDetail returned error: %v", err)
	}
	var installation *Entry
	for i := range detail.Entries {
		if detail.Entries[i].Kind == "provider_installation" {
			installation = &detail.Entries[i]
			break
		}
	}
	if installation == nil {
		t.Fatalf("provider installation entry missing: %#v", detail.Entries)
	}
	if installation.DeliveryRunID != "" {
		t.Fatalf("inventory entry delivery_run_id = %q, want project-only provenance", installation.DeliveryRunID)
	}
	if got := installation.Correlation["delivery_run_id"]; got != "" {
		t.Fatalf("inventory correlation delivery_run_id = %q, want absent", got)
	}
	if len(installation.SourceRefs) != 1 || installation.SourceRefs[0].DeliveryRunID != "" || installation.SourceRefs[0].ProjectID != "proj-a" {
		t.Fatalf("inventory source refs = %#v, want project-only source ref", installation.SourceRefs)
	}

	summary, err := LoadSummary(ctx, store, Options{ProjectID: "proj-a", DeliveryRunID: "run-a", Sections: []string{"inventory"}})
	if err != nil {
		t.Fatalf("LoadSummary returned error: %v", err)
	}
	for _, count := range summary.Counts {
		if count.Kind != "provider_installation" {
			continue
		}
		if len(count.SourceRefs) != 1 || count.SourceRefs[0].DeliveryRunID != "" {
			t.Fatalf("summary inventory source refs = %#v, want project-only", count.SourceRefs)
		}
		return
	}
	t.Fatal("summary provider installation count missing")
}

func TestDetailSurfacesCorruptJSONAndUnsupportedRecordVersion(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	seedBaseRun(t, ctx, store, "proj-a", "run-a")
	insertTask(t, ctx, store, "proj-a", "run-a", "task-a-1", 99, "foreign-attempt-secret")
	insertCorruptProgress(t, ctx, store, "proj-a", "run-a", "receipt-corrupt")

	detail, err := LoadDetail(ctx, store, Options{
		ProjectID:     "proj-a",
		DeliveryRunID: "run-a",
		Sections:      []string{"plans_tasks", "progress"},
		Limit:         100,
		Now:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("LoadDetail returned error: %v", err)
	}
	data, err := DetailJSON(detail)
	if err != nil {
		t.Fatalf("DetailJSON returned error: %v", err)
	}
	text := string(data)
	for _, want := range []string{"corrupt_json", "unsupported_record_version", "dangling_or_cross_scope_ref", "receipt-corrupt", "task-a-1"} {
		if !strings.Contains(text, want) {
			t.Fatalf("canonical detail missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "foreign-attempt-secret") {
		t.Fatalf("dangling target leaked through canonical detail:\n%s", text)
	}
}

func TestSummaryHighCountBoundsSourceIDs(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	seedBaseRun(t, ctx, store, "proj-a", "run-a")
	for i := 0; i < sourceIDCap+7; i++ {
		insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-high-"+pad3(i), "corr-high-"+pad3(i), `{"ok":true}`)
	}

	summary, err := LoadSummary(ctx, store, Options{
		ProjectID:     "proj-a",
		DeliveryRunID: "run-a",
		Sections:      []string{"progress"},
		Now:           func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("LoadSummary returned error: %v", err)
	}
	if len(summary.Counts) != 1 {
		t.Fatalf("summary counts = %d, want 1", len(summary.Counts))
	}
	got := summary.Counts[0]
	if got.Count != sourceIDCap+7 || !got.SourceIDsTruncated || len(got.SourceRecordIDs) != sourceIDCap {
		t.Fatalf("progress count = %#v, want bounded source IDs with truncation", got)
	}
}

func TestReadModelRejectsForeignScopeAndDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	defer store.Close()
	seedBaseRun(t, ctx, store, "proj-a", "run-a")
	insertProgress(t, ctx, store, "proj-a", "run-a", "receipt-a", "corr-a", `{"ok":true}`)
	before := countRows(t, ctx, store, `SELECT COUNT(1) FROM progress_receipts`)

	_, err := LoadDetail(ctx, store, Options{
		ProjectID:     "proj-foreign",
		DeliveryRunID: "run-a",
		Now:           func() time.Time { return fixedNow },
	})
	var qerr *QueryError
	if !errors.As(err, &qerr) || qerr.Code != ErrNotFoundCode {
		t.Fatalf("foreign scoped query error = %v, want not_found", err)
	}

	if _, err := LoadDetail(ctx, store, Options{
		ProjectID:     "proj-a",
		DeliveryRunID: "run-a",
		Now:           func() time.Time { return fixedNow },
	}); err != nil {
		t.Fatalf("LoadDetail returned error: %v", err)
	}
	after := countRows(t, ctx, store, `SELECT COUNT(1) FROM progress_receipts`)
	if after != before {
		t.Fatalf("read model mutated progress receipts: before=%d after=%d", before, after)
	}
}

func TestDetailSchemaGoldenFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "detail_schema_golden.json"))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	var detail Detail
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatalf("unmarshal golden fixture: %v", err)
	}
	if detail.SchemaVersion != DetailSchemaVersion {
		t.Fatalf("schema version = %q, want %q", detail.SchemaVersion, DetailSchemaVersion)
	}
	canonical, err := CanonicalJSON(detail)
	if err != nil {
		t.Fatalf("golden fixture is not canonicalizable: %v", err)
	}
	text := string(canonical)
	for _, forbidden := range []string{"secret-should-not-cross", "foreign-attempt-secret", "must-not-leak"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("golden fixture retained forbidden marker %q:\n%s", forbidden, text)
		}
	}
	if !detail.Redaction.Applied || detail.Redaction.PayloadExposed {
		t.Fatalf("golden redaction = %#v, want applied with no payload exposure", detail.Redaction)
	}
}

func openTestStore(t *testing.T) storage.Store {
	t.Helper()
	return openTestStoreAt(t, filepath.Join(t.TempDir(), "state.db"))
}

func openTestStoreAt(t *testing.T, path string) storage.Store {
	t.Helper()
	store, err := storage.Open(context.Background(), storage.Options{
		Path: path,
		Now:  func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	return store
}

func mustCanonical(t *testing.T, v any) []byte {
	t.Helper()
	data, err := CanonicalJSON(v)
	if err != nil {
		t.Fatalf("CanonicalJSON returned error: %v", err)
	}
	return data
}

func seedBaseRun(t *testing.T, ctx context.Context, store storage.Store, projectID, runID string) {
	t.Helper()
	mustWrite(t, ctx, store, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?)`,
			projectID, "/repo/"+projectID, ts(), ts()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO runs(id, project_id, root_run_id, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			runID, projectID, runID, "running", ts(), ts()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO delivery_runs(
			delivery_run_id, run_id, schema_version, record_version, project_id, root_run_id, state,
			intent_summary, policy_version, max_side_effect_class, approval_status, override_status,
			created_at, updated_at, created_by_json, updated_by_json, host_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', '{}')`,
			runID, runID, "loopcoder.delivery_run.v1", 1, projectID, runID, "running",
			"test run", "policy.v1", "local-write", "approved", "none", ts(), ts())
		return err
	})
}

func insertTask(t *testing.T, ctx context.Context, store storage.Store, projectID, runID, taskID string, recordVersion int, activeAttemptID string) {
	t.Helper()
	mustWrite(t, ctx, store, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO delivery_tasks(
			task_id, schema_version, record_version, project_id, delivery_run_id, task_key, state, title,
			requirements_json, scope_json, permission, side_effect_class, policy_version, plan_fingerprint,
			authorization_fingerprint, attempt_count, active_attempt_id, depends_on_json, created_at, updated_at,
			created_by_json, updated_by_json, host_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '{}', '{}', 'write', 'local-write', 'policy.v1', 'planfp', 'authfp', 0, ?, '[]', ?, ?, '{}', '{}', '{}')`,
			taskID, "loopcoder.delivery_task.v1", recordVersion, projectID, runID, taskID, "planned", "Task "+taskID, activeAttemptID, ts(), ts())
		return err
	})
}

func insertProgress(t *testing.T, ctx context.Context, store storage.Store, projectID, runID, receiptID, correlationID, payload string) {
	t.Helper()
	mustWrite(t, ctx, store, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO progress_receipts(
			progress_receipt_id, schema_version, record_version, project_id, delivery_run_id, run_id,
			correlation_id, correlation_sequence, semantic_fingerprint, phase, status, provider_id, model_id,
			occurred_at, persisted_at, payload_json
		) VALUES (?, ?, 1, ?, ?, ?, ?, 1, ?, 'running', 'observed', 'codex', 'gpt-test', ?, ?, ?)`,
			receiptID, "loopcoder.progress_receipt.v1", projectID, runID, runID, correlationID, "sem-"+receiptID, ts(), ts(), payload)
		return err
	})
}

func insertCorruptProgress(t *testing.T, ctx context.Context, store storage.Store, projectID, runID, receiptID string) {
	t.Helper()
	mustWrite(t, ctx, store, func(tx storage.Tx) error {
		if _, err := tx.Exec(ctx, `PRAGMA ignore_check_constraints = ON`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO progress_receipts(
			progress_receipt_id, schema_version, record_version, project_id, delivery_run_id, run_id,
			correlation_id, correlation_sequence, semantic_fingerprint, phase, status, provider_id, model_id,
			occurred_at, persisted_at, payload_json
		) VALUES (?, ?, 1, ?, ?, ?, ?, 1, ?, 'running', 'observed', 'codex', 'gpt-test', ?, ?, ?)`,
			receiptID, "loopcoder.progress_receipt.v1", projectID, runID, runID, "corr-corrupt", "sem-"+receiptID, ts(), ts(), `{"unterminated":`)
		return err
	})
}

func insertUsage(t *testing.T, ctx context.Context, store storage.Store, projectID, runID, usageID string) {
	t.Helper()
	mustWrite(t, ctx, store, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO usage_records(
			usage_record_id, project_id, delivery_run_id, event_kind, event_time, quantity_kind, unit,
			value, confidence, idempotency_key, payload_json
		) VALUES (?, ?, ?, 'commit', ?, 'tokens', 'token', 7, 'observed', ?, '{}')`,
			usageID, projectID, runID, ts(), "idem-"+usageID)
		return err
	})
}

func insertProviderInstallation(t *testing.T, ctx context.Context, store storage.Store, projectID, installationID string) {
	t.Helper()
	mustWrite(t, ctx, store, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_installations(
			provider_installation_id, schema_version, record_version, scope, project_id, adapter_id,
			adapter_declaration_id, provider_display_name, executable_name, executable_identity_json,
			canonical_path, canonical_path_redacted, discovery_source, discovery_order, platform,
			version, version_confidence, installation_state, usable_for_invocation, known_limitations_json,
			created_at, updated_at, created_by_json, updated_by_json, host_json, policy_version,
			captured_at, stale_after, freshness_state, confidence, side_effect_class, classification,
			source_json, evidence_json, gap_reasons_json, terminal_error_code, payload_json
		) VALUES (?, 'loopcoder.provider_installation.v1', 1, 'project', ?, 'codex',
			'adecl-codex', 'Codex', 'codex', '{}', '/usr/bin/codex', '<redacted>', 'fixture', 1, 'darwin',
			'codex 1.0.0', 'exact', 'available', 'yes', '[]', ?, ?, '{}', '{}', '{}', 'policy.v1',
			?, '', 'fresh', 'observed', 'local-read', 'available', '{}', '{}', '[]', '', ?)`,
			installationID, projectID, ts(), ts(), ts(), fmt.Sprintf(`{"provider_installation_id":%q}`, installationID))
		return err
	})
}

func mustWrite(t *testing.T, ctx context.Context, store storage.Store, fn func(storage.Tx) error) {
	t.Helper()
	if err := store.WithWriteTx(ctx, fn); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}

func countRows(t *testing.T, ctx context.Context, store storage.Store, query string) int {
	t.Helper()
	var count int
	if err := store.WithTx(ctx, func(tx storage.Tx) error {
		return tx.QueryRow(ctx, query).Scan(&count)
	}); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

func ts() string {
	return fixedNow.Format(time.RFC3339Nano)
}

func pad3(i int) string {
	if i < 10 {
		return "00" + string(rune('0'+i))
	}
	if i < 100 {
		return "0" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	return string(rune('0'+i/100)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i%10))
}
