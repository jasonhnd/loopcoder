package usageledger

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/reportquery"
	"github.com/jasonhnd/loopcoder/internal/state"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

func TestUsageRecordsFromReporterDedupesAndNormalizesQuantities(t *testing.T) {
	now := fixedUsageNow()
	report := usageReport(730, usageSplit(120, 30, 150), now)
	records := []reportquery.Record{
		{Report: report, Source: "attempt", RunID: "run-usage", Path: "workers/job.attempt.json"},
		{Report: report, Source: "relay-ledger", RunID: "run-usage", Path: "relay/record.attest"},
	}

	result := UsageRecordsFromReporter(records, "proj_usage", now)
	if result.MalformedCount != 0 {
		t.Fatalf("MalformedCount = %d, want 0", result.MalformedCount)
	}
	if len(result.Records) != 3 {
		t.Fatalf("records = %d, want input/output/total without duplicate replay: %#v", len(result.Records), result.Records)
	}
	seen := map[providerinventory.QuantityKind]bool{}
	for _, record := range result.Records {
		if err := ValidateUsageRecord(record); err != nil {
			t.Fatalf("ValidateUsageRecord(%s): %v", record.UsageRecordID, err)
		}
		if record.ProjectID != "proj_usage" || record.AdapterID != "codex" || record.Unit != "token" || record.ValueScale != 0 {
			t.Fatalf("record scope/unit = %#v", record)
		}
		if record.Confidence != providerinventory.ConfidenceExact {
			t.Fatalf("record confidence = %q, want exact provider-reported fact", record.Confidence)
		}
		seen[record.QuantityKind] = true
	}
	for _, want := range []providerinventory.QuantityKind{providerinventory.QuantityInputTokens, providerinventory.QuantityOutputTokens, providerinventory.QuantityTotalTokens} {
		if !seen[want] {
			t.Fatalf("missing quantity kind %s in %#v", want, result.Records)
		}
	}

	budget := BuildQuotaUsageBudget(result.Records, now, nil)
	if budget.Confidence != providerinventory.ConfidenceEstimated {
		t.Fatalf("budget confidence = %q, want estimated local-only summary", budget.Confidence)
	}
	if !containsString(budget.GapReasons, "loopcoder-local-ledger-not-provider-global") {
		t.Fatalf("budget gaps = %#v, want local-only provider-global caveat", budget.GapReasons)
	}
}

func TestRefreshUsageLedgerFromRealAttemptFilesIsIdempotent(t *testing.T) {
	ctx := context.Background()
	now := fixedUsageNow()
	repo := t.TempDir()
	runID := state.RunIDForIssue(730, now)
	report := usageReport(730, usageTotal(4096), now)
	if _, err := state.WriteAttempt(repo, runID, state.AttemptRecord{
		Version:        1,
		JobID:          "job-730-1",
		Issue:          730,
		Attempt:        1,
		Provider:       "codex",
		PID:            123,
		Phase:          "codex_exited",
		Status:         "succeeded",
		StartedAt:      report.StartedAt,
		HeartbeatAt:    report.EndedAt,
		LastProgressAt: report.EndedAt,
		Report:         &report,
	}); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	store, err := storage.Open(ctx, storage.Options{Path: filepath.Join(t.TempDir(), "loopcoder.db"), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	first, err := RefreshUsageLedgerFromReports(ctx, store, repo, "proj_usage", now)
	if err != nil {
		t.Fatalf("RefreshUsageLedgerFromReports first: %v", err)
	}
	if first.ScannedRecords == 0 || first.InsertedRecords == 0 {
		t.Fatalf("first refresh did not ingest real attempt report: %#v", first)
	}
	second, err := RefreshUsageLedgerFromReports(ctx, store, repo, "proj_usage", now.Add(time.Second))
	if err != nil {
		t.Fatalf("RefreshUsageLedgerFromReports second: %v", err)
	}
	if second.InsertedRecords != 0 || second.ExistingRecords != first.InsertedRecords {
		t.Fatalf("second refresh = %#v, want idempotent existing records", second)
	}
	loaded, err := QueryUsageRecords(ctx, store, Query{ProjectID: "proj_usage", AdapterID: "codex", QuantityKind: providerinventory.QuantityTotalTokens})
	if err != nil {
		t.Fatalf("QueryUsageRecords: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Value != 4096 {
		t.Fatalf("loaded records = %#v, want one total-token record from persisted attempt report", loaded)
	}
}

func TestMalformedReporterUsageIsCountedAsGap(t *testing.T) {
	now := fixedUsageNow()
	bad := int64(-1)
	report := usageReport(730, reporter.Usage{TotalTokens: &bad}, now)
	result := UsageRecordsFromReporter([]reportquery.Record{{Report: report, Source: "attempt", RunID: "run-usage"}}, "proj_usage", now)
	if result.MalformedCount != 1 {
		t.Fatalf("MalformedCount = %d, want 1", result.MalformedCount)
	}
	if len(result.Records) != 0 {
		t.Fatalf("malformed usage produced records: %#v", result.Records)
	}
	if !containsString(result.GapReasons, "malformed-report-payloads:1") {
		t.Fatalf("gap reasons = %#v, want malformed count", result.GapReasons)
	}
}

func TestReconcileUsagePreservesProviderHigherDisagreement(t *testing.T) {
	now := fixedUsageNow()
	local := UsageRecordsFromReporter([]reportquery.Record{{Report: usageReport(730, usageTotal(100), now), Source: "attempt", RunID: "run-usage"}}, "proj_usage", now).Records
	provider := usageProviderRecord("proj_usage", providerinventory.QuantityTotalTokens, 175, now)

	reconciliation, err := ReconcileUsage(local, provider, "provider:codex/project:proj_usage", providerinventory.WindowRolling)
	if !errors.Is(err, ErrUsageReconciliationConflict) {
		t.Fatalf("ReconcileUsage error = %v, want ErrUsageReconciliationConflict", err)
	}
	if reconciliation.Outcome != OutcomeProviderHigher {
		t.Fatalf("outcome = %q, want provider-higher", reconciliation.Outcome)
	}
	if reconciliation.LocalTotal != 100 || reconciliation.ProviderTotal == nil || *reconciliation.ProviderTotal != 175 {
		t.Fatalf("totals = local %d provider %#v", reconciliation.LocalTotal, reconciliation.ProviderTotal)
	}
	if len(reconciliation.CorrectionUsageRecordIDs) != 0 {
		t.Fatalf("corrections = %#v, want no fabricated correction", reconciliation.CorrectionUsageRecordIDs)
	}
}

func fixedUsageNow() time.Time {
	return time.Unix(0, 0).UTC().Add(730 * time.Hour)
}

func usageReport(issue int, usage reporter.Usage, start time.Time) reporter.Report {
	return reporter.Report{
		WorkID:      state.RunIDForIssue(issue, start),
		Issue:       issue,
		Role:        reporter.RoleWorker,
		Provider:    "codex",
		Model:       "gpt-fixture-" + strconv.Itoa(issue),
		ModelSource: reporter.ModelSourceParsed,
		Effort:      "high",
		Permission:  reporter.PermissionWrite,
		Action:      "implement issue #" + strconv.Itoa(issue),
		ExitCode:    0,
		StartedAt:   start.UTC().Format(time.RFC3339Nano),
		EndedAt:     start.Add(42 * time.Second).UTC().Format(time.RFC3339Nano),
		DurationMS:  int64((42 * time.Second).Milliseconds()),
		Usage:       usage,
		Verified:    true,
	}
}

func usageProviderRecord(projectID string, kind providerinventory.QuantityKind, value int64, at time.Time) UsageRecord {
	key := "provider-record" + strconv.FormatInt(value, 10)
	return normalizeUsageRecord(UsageRecord{
		UsageRecordID:   "usage_" + hashBase32(key)[:26],
		EventKind:       EventProviderReported,
		EventTime:       at.UTC().Format(time.RFC3339Nano),
		ProjectID:       projectID,
		AdapterID:       "codex",
		QuantityKind:    kind,
		Value:           value,
		Unit:            "token",
		ValueScale:      0,
		Confidence:      providerinventory.ConfidenceExact,
		SourceRecordIDs: []string{"provider_fixture"},
		IdempotencyKey:  key,
		GapReasons:      []string{"provider-reported-total"},
	})
}

func usageSplit(input, output, total int64) reporter.Usage {
	return reporter.Usage{
		InputTokens:  &input,
		OutputTokens: &output,
		TotalTokens:  &total,
	}
}

func usageTotal(total int64) reporter.Usage {
	return reporter.Usage{TotalTokens: &total}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
