package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestUsageReporterNormalizationRoundTripAndReplayDedupe(t *testing.T) {
	ctx := context.Background()
	now := fixedInventoryNow()
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: fixedInventoryNow})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	if err := insertStoredReportForUsageTest(ctx, store, "report-1", "project-usage", "run-usage", validUsageReport(10, 20, 30), now); err != nil {
		t.Fatalf("insert report: %v", err)
	}
	if err := insertStoredReportForUsageTest(ctx, store, "report-duplicate", "project-usage", "run-usage", validUsageReport(10, 20, 30), now); err != nil {
		t.Fatalf("insert duplicate report: %v", err)
	}
	if err := RefreshUsageLedgerFromReports(ctx, store, "project-usage", now); err != nil {
		t.Fatalf("RefreshUsageLedgerFromReports: %v", err)
	}
	if err := RefreshUsageLedgerFromReports(ctx, store, "project-usage", now.Add(time.Minute)); err != nil {
		t.Fatalf("RefreshUsageLedgerFromReports replay: %v", err)
	}
	ledger, err := LoadQuotaUsageBudget(ctx, store, "project-usage", true)
	if err != nil {
		t.Fatalf("LoadQuotaUsageBudget: %v", err)
	}
	if len(ledger.UsageRecords) != 5 {
		t.Fatalf("usage record count = %d, want 5 normalized quantities without duplicate replay", len(ledger.UsageRecords))
	}
	byKind := map[QuantityKind]int64{}
	for _, record := range ledger.UsageRecords {
		byKind[record.QuantityKind] += record.Value
		if record.Confidence != ConfidenceExact {
			t.Fatalf("reporter-derived record confidence = %s, want exact for local/provider-reported fact", record.Confidence)
		}
		if len(record.SourceRecordIDs) == 0 || record.IdempotencyKey == "" {
			t.Fatalf("record missing source or idempotency: %#v", record)
		}
	}
	if byKind[QuantityInputTokens] != 10 || byKind[QuantityOutputTokens] != 20 || byKind[QuantityTotalTokens] != 30 || byKind[QuantityRequests] != 1 || byKind[QuantityWallMS] != 1234 {
		t.Fatalf("normalized totals = %#v", byKind)
	}
}

func TestUsageEstimatedVsExactConfidenceSeparation(t *testing.T) {
	now := fixedInventoryNow()
	exact := mustUsageRecordsFromReport(t, StoredReportRecord{ReportID: "exact", ProjectID: "project-usage", RunID: "run", Report: validUsageReport(1, 2, 3)}, now)
	estimate, err := EstimateRemainingUsage(100, exact, nil, "estimate-key", now)
	if err != nil {
		t.Fatalf("EstimateRemainingUsage: %v", err)
	}
	if estimate.Confidence != ConfidenceEstimated || estimate.Estimator == nil || !containsString(estimate.GapReasons, "estimate-not-exact-quota") {
		t.Fatalf("estimate = %#v, want estimated with estimator metadata", estimate)
	}
	estimate.Confidence = ConfidenceExact
	if err := ValidateUsageRecord(estimate); !errors.Is(err, ErrUsageEstimateCannotBeExact) {
		t.Fatalf("ValidateUsageRecord heuristic exact error = %v, want ErrUsageEstimateCannotBeExact", err)
	}
}

func TestUsageReconciliationRecordsDisagreementAndCorrection(t *testing.T) {
	now := fixedInventoryNow()
	local := mustUsageRecordsFromReport(t, StoredReportRecord{ReportID: "local", ProjectID: "project-usage", RunID: "run", Report: validUsageReport(10, 5, 15)}, now)
	localTotals := filterUsageKind(local, QuantityTotalTokens)
	provider := normalizeUsageRecord(localTotals[0])
	provider.UsageRecordID = usageRecordID("provider-total")
	provider.IdempotencyKey = "provider-total"
	provider.EventKind = UsageEventProviderReported
	provider.Value = 18
	provider.SourceRecordIDs = []string{"provider-receipt:fixture"}

	reconciliation, corrections, err := ReconcileUsage(provider, localTotals, "reconcile-key", now)
	if err != nil {
		t.Fatalf("ReconcileUsage: %v", err)
	}
	if reconciliation.Outcome != UsageReconciliationProviderHigher || reconciliation.Delta == nil || *reconciliation.Delta != 3 {
		t.Fatalf("reconciliation = %#v, want provider-higher delta 3", reconciliation)
	}
	if len(corrections) != 1 || corrections[0].EventKind != UsageEventCorrection || corrections[0].Value != 3 {
		t.Fatalf("corrections = %#v, want append-only correction", corrections)
	}
	if !containsString(reconciliation.GapReasons, "provider-higher-may-include-out-of-band-usage") {
		t.Fatalf("reconciliation gaps = %#v", reconciliation.GapReasons)
	}
}

func TestUsageRetentionClockCrossingAndProtectedOutcomes(t *testing.T) {
	now := fixedInventoryNow()
	records := mustUsageRecordsFromReport(t, StoredReportRecord{ReportID: "retain", ProjectID: "project-usage", RunID: "run", Report: validUsageReport(1, 1, 2)}, now)
	total := filterUsageKind(records, QuantityTotalTokens)[0]
	before := EvaluateUsageRetention([]UsageRecord{total}, now.Add(29*24*time.Hour), 30*24*time.Hour, nil)[0]
	if before.Outcome != UsageRetentionRetainWindow {
		t.Fatalf("before retention boundary = %#v", before)
	}
	after := EvaluateUsageRetention([]UsageRecord{total}, now.Add(31*24*time.Hour), 30*24*time.Hour, nil)[0]
	if after.Outcome != UsageRetentionDeleteEligible {
		t.Fatalf("after retention boundary = %#v", after)
	}
	protected := EvaluateUsageRetention([]UsageRecord{total}, now.Add(31*24*time.Hour), 30*24*time.Hour, map[string]bool{total.UsageRecordID: true})[0]
	if protected.Outcome != UsageRetentionRetainProtected {
		t.Fatalf("protected retention = %#v", protected)
	}
}

func TestUsageEnumsFailClosed(t *testing.T) {
	var eventPayload struct {
		EventKind UsageEventKind `json:"event_kind"`
	}
	if err := json.Unmarshal([]byte(`{"event_kind":"telepathy"}`), &eventPayload); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("unknown usage event error = %v, want ErrInvalidRecord", err)
	}
	var reconciliationPayload struct {
		Outcome UsageReconciliationOutcome `json:"outcome"`
	}
	if err := json.Unmarshal([]byte(`{"outcome":"optimistic"}`), &reconciliationPayload); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("unknown reconciliation outcome error = %v, want ErrInvalidRecord", err)
	}
}

func insertStoredReportForUsageTest(ctx context.Context, store storage.Store, id, projectID, runID string, record reporter.Report, now time.Time) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO projects(id, local_path, created_at, updated_at) VALUES (?, ?, ?, ?)`, projectID, "fixture", formatTime(now), formatTime(now))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO runs(id, project_id, status, updated_at, created_at) VALUES (?, ?, ?, ?, ?)`, runID, projectID, "succeeded", formatTime(now), formatTime(now))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO reports(id, project_id, run_id, role, provider, model, started_at, ended_at, payload_json, created_at, source_path, source_hash, source_kind)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, projectID, runID, string(record.Role), record.Provider, record.Model, record.StartedAt, record.EndedAt, string(payload), formatTime(now), ".loopcoder/runs/run/events.jsonl", rawSourceHash(payload), "fixture")
		return err
	})
}

func validUsageReport(input, output, total int64) reporter.Report {
	now := fixedInventoryNow()
	return reporter.Report{
		WorkID:      "job-usage-1",
		Role:        reporter.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-fixture",
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionWrite,
		Action:      "implement fixture issue",
		ExitCode:    0,
		StartedAt:   formatTime(now.Add(-1234 * time.Millisecond)),
		EndedAt:     formatTime(now),
		DurationMS:  1234,
		Usage: reporter.Usage{
			InputTokens:  &input,
			OutputTokens: &output,
			TotalTokens:  &total,
		},
		Verified: true,
	}
}

func mustUsageRecordsFromReport(t *testing.T, record StoredReportRecord, now time.Time) []UsageRecord {
	t.Helper()
	records, err := UsageRecordsFromReporter(record, now)
	if err != nil {
		t.Fatalf("UsageRecordsFromReporter: %v", err)
	}
	return records
}

func filterUsageKind(records []UsageRecord, kind QuantityKind) []UsageRecord {
	var out []UsageRecord
	for _, record := range records {
		if record.QuantityKind == kind {
			out = append(out, record)
		}
	}
	return out
}
