// Package usageledger records LoopCoder-local usage facts without claiming
// they are provider-global quota facts.
package usageledger

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/providerinventory"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/reportquery"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	UsageRecordSchema         = "loopcoder.usage_record.v1"
	UsageReconciliationSchema = "loopcoder.usage_reconciliation.v1"
	QuotaUsageBudgetSchema    = "loopcoder.quota_usage_budget_json.v1"

	maxReportRecords = 2000
)

var (
	ErrDuplicateReplay              = errors.New("ErrDuplicateReplay")
	ErrUsageRecordMalformed         = errors.New("ErrUsageRecordMalformed")
	ErrUsageReconciliationMalformed = errors.New("ErrUsageReconciliationMalformed")
	ErrUsageReconciliationConflict  = errors.New("ErrUsageReconciliationConflict")
)

type EventKind string

const (
	EventEstimate             EventKind = "estimate"
	EventReservationCreated   EventKind = "reservation-created"
	EventStarted              EventKind = "started"
	EventStreamUpdate         EventKind = "stream-update"
	EventCompletion           EventKind = "completion"
	EventCancellation         EventKind = "cancellation"
	EventFailure              EventKind = "failure"
	EventReservationCommitted EventKind = "reservation-committed"
	EventReservationReleased  EventKind = "reservation-released"
	EventProviderReported     EventKind = "provider-reported"
	EventCorrection           EventKind = "correction"
)

type ReconciliationOutcome string

const (
	OutcomeMatched        ReconciliationOutcome = "matched"
	OutcomeProviderHigher ReconciliationOutcome = "provider-higher"
	OutcomeProviderLower  ReconciliationOutcome = "provider-lower"
	OutcomePartial        ReconciliationOutcome = "partial"
	OutcomeConflicting    ReconciliationOutcome = "conflicting"
	OutcomeUnavailable    ReconciliationOutcome = "unavailable"
)

type UsageRecord struct {
	SchemaVersion         string                         `json:"schema_version"`
	UsageRecordID         string                         `json:"usage_record_id"`
	EventKind             EventKind                      `json:"event_kind"`
	EventTime             string                         `json:"event_time"`
	ProjectID             string                         `json:"project_id,omitempty"`
	DeliveryRunID         string                         `json:"delivery_run_id,omitempty"`
	TaskID                string                         `json:"task_id,omitempty"`
	AttemptID             string                         `json:"attempt_id,omitempty"`
	WorkerID              string                         `json:"worker_id,omitempty"`
	SubAgentID            string                         `json:"sub_agent_id,omitempty"`
	AdapterID             string                         `json:"adapter_id,omitempty"`
	AccountProfileID      string                         `json:"account_profile_id,omitempty"`
	ModelCapabilityID     string                         `json:"model_capability_id,omitempty"`
	BudgetReservationID   string                         `json:"budget_reservation_id,omitempty"`
	QuantityKind          providerinventory.QuantityKind `json:"quantity_kind"`
	Value                 int64                          `json:"value"`
	Unit                  string                         `json:"unit"`
	ValueScale            int                            `json:"value_scale"`
	OriginalQuantityJSON  json.RawMessage                `json:"original_quantity_json,omitempty"`
	Confidence            providerinventory.Confidence   `json:"confidence"`
	Estimator             string                         `json:"estimator,omitempty"`
	EstimatorVersion      string                         `json:"estimator_version,omitempty"`
	SourceRecordIDs       []string                       `json:"source_record_ids"`
	IdempotencyKey        string                         `json:"idempotency_key"`
	DedupeKey             string                         `json:"dedupe_key,omitempty"`
	ReplacesUsageRecordID string                         `json:"replaces_usage_record_id,omitempty"`
	GapReasons            []string                       `json:"gap_reasons"`
}

type UsageReconciliation struct {
	SchemaVersion            string                         `json:"schema_version"`
	UsageReconciliationID    string                         `json:"usage_reconciliation_id"`
	ProjectID                string                         `json:"project_id,omitempty"`
	ProviderSnapshotID       string                         `json:"provider_snapshot_id,omitempty"`
	LocalRecordIDs           []string                       `json:"local_record_ids"`
	ScopeKey                 string                         `json:"scope_key"`
	QuantityKind             providerinventory.QuantityKind `json:"quantity_kind"`
	WindowKind               providerinventory.WindowKind   `json:"window_kind"`
	WindowStart              string                         `json:"window_start,omitempty"`
	WindowEnd                string                         `json:"window_end,omitempty"`
	LocalTotal               int64                          `json:"local_total"`
	ProviderTotal            *int64                         `json:"provider_total,omitempty"`
	Delta                    *int64                         `json:"delta,omitempty"`
	DeltaConfidence          providerinventory.Confidence   `json:"delta_confidence"`
	Outcome                  ReconciliationOutcome          `json:"outcome"`
	CorrectionUsageRecordIDs []string                       `json:"correction_usage_record_ids"`
	IdempotencyKey           string                         `json:"idempotency_key"`
}

type UsageSummary struct {
	ProjectID         string                         `json:"project_id,omitempty"`
	AdapterID         string                         `json:"adapter_id,omitempty"`
	AccountProfileID  string                         `json:"account_profile_id,omitempty"`
	ModelCapabilityID string                         `json:"model_capability_id,omitempty"`
	Model             string                         `json:"model,omitempty"`
	DeliveryRunID     string                         `json:"delivery_run_id,omitempty"`
	TaskID            string                         `json:"task_id,omitempty"`
	WorkerID          string                         `json:"worker_id,omitempty"`
	SubAgentID        string                         `json:"sub_agent_id,omitempty"`
	QuantityKind      providerinventory.QuantityKind `json:"quantity_kind"`
	Unit              string                         `json:"unit"`
	ValueScale        int                            `json:"value_scale"`
	TotalValue        int64                          `json:"total_value"`
	RecordCount       int                            `json:"record_count"`
	UsageRecordIDs    []string                       `json:"usage_record_ids"`
	Confidence        providerinventory.Confidence   `json:"confidence"`
	GapReasons        []string                       `json:"gap_reasons"`
}

type QuotaUsageBudget struct {
	SchemaVersion         string                                   `json:"schema_version"`
	GeneratedAt           string                                   `json:"generated_at"`
	QuotaUsageFingerprint string                                   `json:"quota_usage_fingerprint"`
	Confidence            providerinventory.Confidence             `json:"confidence"`
	QuotaSources          []providerinventory.QuotaTelemetrySource `json:"quota_sources"`
	QuotaSnapshots        []providerinventory.QuotaSnapshot        `json:"quota_snapshots"`
	UsageSummary          []UsageSummary                           `json:"usage_summary"`
	BudgetSummary         []any                                    `json:"budget_summary"`
	AvailabilityScores    []any                                    `json:"availability_scores"`
	CircuitBreakers       []any                                    `json:"circuit_breakers"`
	GapReasons            []string                                 `json:"gap_reasons"`
}

type IngestionResult struct {
	Records        []UsageRecord `json:"records"`
	MalformedCount int           `json:"malformed_count"`
	GapReasons     []string      `json:"gap_reasons"`
}

type RefreshResult struct {
	ScannedRecords  int      `json:"scanned_records"`
	InsertedRecords int      `json:"inserted_records"`
	ExistingRecords int      `json:"existing_records"`
	MalformedCount  int      `json:"malformed_count"`
	GapReasons      []string `json:"gap_reasons"`
}

type Query struct {
	ProjectID         string
	AdapterID         string
	AccountProfileID  string
	ModelCapabilityID string
	DeliveryRunID     string
	TaskID            string
	WorkerID          string
	SubAgentID        string
	QuantityKind      providerinventory.QuantityKind
}

func (k *EventKind) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("%w: invalid event_kind enum: %v", providerinventory.ErrInvalidRecord, err)
	}
	if !knownEventKind(EventKind(value)) {
		return fmt.Errorf("%w: unknown event_kind %q", providerinventory.ErrInvalidRecord, value)
	}
	*k = EventKind(value)
	return nil
}

func (o *ReconciliationOutcome) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("%w: invalid outcome enum: %v", providerinventory.ErrInvalidRecord, err)
	}
	if !knownReconciliationOutcome(ReconciliationOutcome(value)) {
		return fmt.Errorf("%w: unknown outcome %q", providerinventory.ErrInvalidRecord, value)
	}
	*o = ReconciliationOutcome(value)
	return nil
}

func UsageRecordsFromReporter(records []reportquery.Record, projectID string, observedAt time.Time) IngestionResult {
	var out []UsageRecord
	var gaps []string
	malformed := 0
	seen := map[string]bool{}
	for _, record := range records {
		result := usageRecordsFromOneReport(record, projectID, observedAt)
		if result.MalformedCount > 0 {
			malformed += result.MalformedCount
		}
		gaps = append(gaps, result.GapReasons...)
		for _, usageRecord := range result.Records {
			if seen[usageRecord.UsageRecordID] {
				continue
			}
			seen[usageRecord.UsageRecordID] = true
			out = append(out, usageRecord)
		}
	}
	sortUsageRecords(out)
	return IngestionResult{
		Records:        out,
		MalformedCount: malformed,
		GapReasons:     malformedGap(dedupeStrings(gaps), malformed),
	}
}

func RefreshUsageLedgerFromReports(ctx context.Context, store storage.Store, repoPath, projectID string, observedAt time.Time) (RefreshResult, error) {
	if store == nil {
		return RefreshResult{}, errors.New("refresh usage ledger: storage store is required")
	}
	records, err := reportquery.List(reportquery.Options{RepoPath: repoPath, Limit: maxReportRecords})
	if err != nil {
		return RefreshResult{}, err
	}
	ingested := UsageRecordsFromReporter(records, projectID, observedAt)
	result := RefreshResult{
		ScannedRecords: len(records),
		MalformedCount: ingested.MalformedCount,
		GapReasons:     append([]string(nil), ingested.GapReasons...),
	}
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		for _, record := range ingested.Records {
			inserted, err := insertUsageRecord(ctx, tx, record)
			if err != nil {
				return err
			}
			if inserted {
				result.InsertedRecords++
			} else {
				result.ExistingRecords++
			}
		}
		return nil
	})
	return result, err
}

func BuildQuotaUsageBudgetFromReports(ctx context.Context, store storage.Store, repoPath, projectID string, observedAt time.Time) (QuotaUsageBudget, error) {
	var records []UsageRecord
	var gaps []string
	if store != nil {
		loaded, err := QueryUsageRecords(ctx, store, Query{ProjectID: projectID})
		if err != nil {
			return QuotaUsageBudget{}, err
		}
		records = loaded
	}
	if len(records) == 0 {
		reports, err := reportquery.List(reportquery.Options{RepoPath: repoPath, Limit: maxReportRecords})
		if err != nil {
			return QuotaUsageBudget{}, err
		}
		ingested := UsageRecordsFromReporter(reports, projectID, observedAt)
		records = ingested.Records
		gaps = append(gaps, ingested.GapReasons...)
		gaps = append(gaps, "persisted-ledger-empty", "derived-from-reports-fallback")
	}
	return BuildQuotaUsageBudget(records, observedAt, gaps), nil
}

func BuildQuotaUsageBudget(records []UsageRecord, observedAt time.Time, gapReasons []string) QuotaUsageBudget {
	summaries := SummarizeUsageRecords(records)
	confidence := providerinventory.ConfidenceUnknown
	if len(summaries) > 0 {
		confidence = providerinventory.ConfidenceEstimated
		gapReasons = append(gapReasons, "loopcoder-local-ledger-not-provider-global")
	}
	gapReasons = dedupeStrings(gapReasons)
	budget := QuotaUsageBudget{
		SchemaVersion:      QuotaUsageBudgetSchema,
		GeneratedAt:        formatTime(observedAt),
		Confidence:         confidence,
		QuotaSources:       []providerinventory.QuotaTelemetrySource{},
		QuotaSnapshots:     []providerinventory.QuotaSnapshot{},
		UsageSummary:       summaries,
		BudgetSummary:      []any{},
		AvailabilityScores: []any{},
		CircuitBreakers:    []any{},
		GapReasons:         gapReasons,
	}
	budget.QuotaUsageFingerprint = fingerprintBudget(budget)
	return budget
}

func QuotaUsageRefs(records []UsageRecord, confidence providerinventory.Confidence, gapReasons []string) providerinventory.QuotaUsageRefs {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.UsageRecordID)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return providerinventory.EmptyQuotaUsageRefs()
	}
	gapReasons = dedupeStrings(append(gapReasons, "loopcoder-local-ledger-not-provider-global"))
	return providerinventory.QuotaUsageRefs{
		SchemaVersion:         providerinventory.QuotaUsageRefsSchema,
		QuotaUsageFingerprint: "sha256:" + hashHex("quota_usage_refs", strings.Join(ids, "\x00"), strings.Join(gapReasons, "\x00")),
		QuotaSnapshotIDs:      []string{},
		UsageRecordIDs:        ids,
		BudgetPolicyIDs:       []string{},
		BudgetReservationIDs:  []string{},
		AvailabilityScoreIDs:  []string{},
		CircuitBreakerIDs:     []string{},
		Confidence:            confidence,
		HardIneligibleReasons: []string{},
		GapReasons:            gapReasons,
	}
}

func QueryUsageRecords(ctx context.Context, store storage.Store, query Query) ([]UsageRecord, error) {
	if store == nil {
		return nil, errors.New("query usage records: storage store is required")
	}
	var records []UsageRecord
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		clauses := []string{"1=1"}
		args := []any{}
		add := func(column, value string) {
			if strings.TrimSpace(value) == "" {
				return
			}
			clauses = append(clauses, column+" = ?")
			args = append(args, strings.TrimSpace(value))
		}
		add("project_id", query.ProjectID)
		add("adapter_id", query.AdapterID)
		add("account_profile_id", query.AccountProfileID)
		add("model_capability_id", query.ModelCapabilityID)
		add("delivery_run_id", query.DeliveryRunID)
		add("task_id", query.TaskID)
		add("worker_id", query.WorkerID)
		add("sub_agent_id", query.SubAgentID)
		if query.QuantityKind != "" {
			add("quantity_kind", string(query.QuantityKind))
		}
		rows, err := tx.Query(ctx, `SELECT payload_json FROM usage_records WHERE `+strings.Join(clauses, " AND ")+` ORDER BY event_time, usage_record_id`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return err
			}
			var record UsageRecord
			if err := json.Unmarshal([]byte(payload), &record); err != nil {
				return err
			}
			records = append(records, record)
		}
		return rows.Err()
	})
	return records, err
}

func ReconcileUsage(local []UsageRecord, providerRecord UsageRecord, scopeKey string, windowKind providerinventory.WindowKind) (UsageReconciliation, error) {
	providerRecord = normalizeUsageRecord(providerRecord)
	if err := ValidateUsageRecord(providerRecord); err != nil {
		return UsageReconciliation{}, err
	}
	var localIDs []string
	var localTotal int64
	var gaps []string
	for _, record := range local {
		record = normalizeUsageRecord(record)
		if record.QuantityKind != providerRecord.QuantityKind || record.Unit != providerRecord.Unit || record.ValueScale != providerRecord.ValueScale {
			gaps = append(gaps, "reconciliation-scope-excluded")
			continue
		}
		localIDs = append(localIDs, record.UsageRecordID)
		localTotal += record.Value
	}
	sort.Strings(localIDs)
	providerTotal := providerRecord.Value
	delta := providerTotal - localTotal
	outcome := OutcomeMatched
	switch {
	case len(localIDs) == 0:
		outcome = OutcomePartial
	case delta > 0:
		outcome = OutcomeProviderHigher
		gaps = append(gaps, "provider-higher-may-include-out-of-band-usage")
	case delta < 0:
		outcome = OutcomeProviderLower
		gaps = append(gaps, "provider-lower-may-reflect-rounding-or-partial-local-records")
	}
	key := strings.Join([]string{
		"usage-reconciliation",
		scopeKey,
		string(providerRecord.QuantityKind),
		string(windowKind),
		providerRecord.UsageRecordID,
		strings.Join(localIDs, ","),
		strconv.FormatInt(localTotal, 10),
		strconv.FormatInt(providerTotal, 10),
	}, "\x00")
	reconciliation := UsageReconciliation{
		SchemaVersion:            UsageReconciliationSchema,
		UsageReconciliationID:    "urec_" + hashBase32(key)[:26],
		ProjectID:                providerRecord.ProjectID,
		ProviderSnapshotID:       providerRecord.UsageRecordID,
		LocalRecordIDs:           localIDs,
		ScopeKey:                 scopeKey,
		QuantityKind:             providerRecord.QuantityKind,
		WindowKind:               windowKind,
		LocalTotal:               localTotal,
		ProviderTotal:            &providerTotal,
		Delta:                    &delta,
		DeltaConfidence:          providerinventory.ConfidenceEstimated,
		Outcome:                  outcome,
		CorrectionUsageRecordIDs: []string{},
		IdempotencyKey:           key,
	}
	if err := ValidateUsageReconciliation(reconciliation); err != nil {
		return UsageReconciliation{}, err
	}
	if outcome == OutcomeProviderHigher || outcome == OutcomeProviderLower {
		return reconciliation, ErrUsageReconciliationConflict
	}
	_ = gaps
	return reconciliation, nil
}

func SummarizeUsageRecords(records []UsageRecord) []UsageSummary {
	type group struct {
		key     string
		summary UsageSummary
	}
	groups := map[string]*group{}
	for _, record := range records {
		record = normalizeUsageRecord(record)
		if err := ValidateUsageRecord(record); err != nil {
			continue
		}
		key := strings.Join([]string{
			record.ProjectID, record.AdapterID, record.AccountProfileID, record.ModelCapabilityID,
			record.DeliveryRunID, record.TaskID, record.WorkerID, record.SubAgentID,
			string(record.QuantityKind), record.Unit, strconv.Itoa(record.ValueScale),
		}, "\x00")
		current := groups[key]
		if current == nil {
			current = &group{key: key, summary: UsageSummary{
				ProjectID:         record.ProjectID,
				AdapterID:         record.AdapterID,
				AccountProfileID:  record.AccountProfileID,
				ModelCapabilityID: record.ModelCapabilityID,
				Model:             modelFromOriginalQuantity(record.OriginalQuantityJSON),
				DeliveryRunID:     record.DeliveryRunID,
				TaskID:            record.TaskID,
				WorkerID:          record.WorkerID,
				SubAgentID:        record.SubAgentID,
				QuantityKind:      record.QuantityKind,
				Unit:              record.Unit,
				ValueScale:        record.ValueScale,
				UsageRecordIDs:    []string{},
				Confidence:        providerinventory.ConfidenceEstimated,
				GapReasons:        []string{"loopcoder-local-ledger-not-provider-global"},
			}}
			groups[key] = current
		}
		current.summary.TotalValue += record.Value
		current.summary.RecordCount++
		current.summary.UsageRecordIDs = append(current.summary.UsageRecordIDs, record.UsageRecordID)
		current.summary.GapReasons = append(current.summary.GapReasons, record.GapReasons...)
	}
	out := make([]UsageSummary, 0, len(groups))
	for _, group := range groups {
		sort.Strings(group.summary.UsageRecordIDs)
		group.summary.GapReasons = dedupeStrings(group.summary.GapReasons)
		out = append(out, group.summary)
	}
	sort.Slice(out, func(i, j int) bool {
		left := strings.Join([]string{out[i].ProjectID, out[i].AdapterID, out[i].DeliveryRunID, out[i].TaskID, string(out[i].QuantityKind), out[i].Model}, "\x00")
		right := strings.Join([]string{out[j].ProjectID, out[j].AdapterID, out[j].DeliveryRunID, out[j].TaskID, string(out[j].QuantityKind), out[j].Model}, "\x00")
		return left < right
	})
	return out
}

func ValidateUsageRecord(record UsageRecord) error {
	record = normalizeUsageRecord(record)
	if record.SchemaVersion != UsageRecordSchema {
		return fmt.Errorf("%w: schema_version must be %s", ErrUsageRecordMalformed, UsageRecordSchema)
	}
	if !strings.HasPrefix(record.UsageRecordID, "usage_") || strings.TrimSpace(record.IdempotencyKey) == "" {
		return fmt.Errorf("%w: usage_record_id and idempotency_key are required", ErrUsageRecordMalformed)
	}
	if !knownEventKind(record.EventKind) {
		return fmt.Errorf("%w: unknown event_kind %q", providerinventory.ErrInvalidRecord, record.EventKind)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.EventTime); err != nil {
		return fmt.Errorf("%w: event_time must be RFC3339", ErrUsageRecordMalformed)
	}
	if record.ValueScale < 0 || strings.TrimSpace(record.Unit) == "" {
		return fmt.Errorf("%w: unit and non-negative value_scale are required", ErrUsageRecordMalformed)
	}
	if record.QuantityKind == "" {
		return fmt.Errorf("%w: quantity_kind is required", ErrUsageRecordMalformed)
	}
	if record.Confidence == providerinventory.ConfidenceEstimated && strings.TrimSpace(record.Estimator) == "" {
		return fmt.Errorf("%w: estimator is required for estimated records", ErrUsageRecordMalformed)
	}
	if len(record.SourceRecordIDs) == 0 {
		return fmt.Errorf("%w: source_record_ids are required", ErrUsageRecordMalformed)
	}
	return nil
}

func ValidateUsageReconciliation(record UsageReconciliation) error {
	record = normalizeUsageReconciliation(record)
	if record.SchemaVersion != UsageReconciliationSchema {
		return fmt.Errorf("%w: schema_version must be %s", ErrUsageReconciliationMalformed, UsageReconciliationSchema)
	}
	if !strings.HasPrefix(record.UsageReconciliationID, "urec_") || strings.TrimSpace(record.IdempotencyKey) == "" {
		return fmt.Errorf("%w: usage_reconciliation_id and idempotency_key are required", ErrUsageReconciliationMalformed)
	}
	if !knownReconciliationOutcome(record.Outcome) {
		return fmt.Errorf("%w: unknown outcome %q", providerinventory.ErrInvalidRecord, record.Outcome)
	}
	if record.QuantityKind == "" || record.WindowKind == "" || strings.TrimSpace(record.ScopeKey) == "" {
		return fmt.Errorf("%w: scope_key, quantity_kind, and window_kind are required", ErrUsageReconciliationMalformed)
	}
	if record.LocalRecordIDs == nil || record.CorrectionUsageRecordIDs == nil {
		return fmt.Errorf("%w: local and correction id lists must be present", ErrUsageReconciliationMalformed)
	}
	return nil
}

func usageRecordsFromOneReport(record reportquery.Record, projectID string, observedAt time.Time) IngestionResult {
	report := record.Report
	if !report.HasUsage() {
		return IngestionResult{GapReasons: []string{"report-usage-absent"}}
	}
	if report.Usage.InputTokens == nil && report.Usage.OutputTokens == nil && report.Usage.TotalTokens == nil {
		return IngestionResult{GapReasons: []string{"report-usage-empty"}}
	}
	if invalidUsage(report.Usage) {
		return IngestionResult{MalformedCount: 1, GapReasons: []string{"malformed-report-payloads"}}
	}
	eventTime := firstReportTime(report.EndedAt, report.StartedAt, observedAt)
	sourceID := sourceRecordID(record)
	baseKey := stableReportKey(record)
	commonGaps := []string{"loopcoder-local-ledger-not-provider-global"}
	if strings.TrimSpace(record.Source) == "relay-ledger" || strings.TrimSpace(record.Source) == "relay-pending" {
		commonGaps = append(commonGaps, "relay-replay-deduped")
	}
	var out []UsageRecord
	add := func(kind providerinventory.QuantityKind, value int64, reportedField string, confidence providerinventory.Confidence, gaps []string) {
		original := originalQuantity(report, reportedField, value)
		key := strings.Join([]string{baseKey, string(kind), reportedField, strconv.FormatInt(value, 10)}, "\x00")
		usageRecord := normalizeUsageRecord(UsageRecord{
			UsageRecordID:        "usage_" + hashBase32(key)[:26],
			EventKind:            EventProviderReported,
			EventTime:            formatTime(eventTime),
			ProjectID:            projectID,
			DeliveryRunID:        firstNonEmpty(report.WorkID, record.RunID),
			TaskID:               taskID(report.Issue),
			AdapterID:            strings.TrimSpace(report.Provider),
			QuantityKind:         kind,
			Value:                value,
			Unit:                 unitForQuantity(kind),
			ValueScale:           0,
			OriginalQuantityJSON: original,
			Confidence:           confidence,
			SourceRecordIDs:      []string{sourceID},
			IdempotencyKey:       key,
			DedupeKey:            baseKey,
			GapReasons:           append(append([]string{}, commonGaps...), gaps...),
		})
		out = append(out, usageRecord)
	}
	if report.Usage.InputTokens != nil {
		add(providerinventory.QuantityInputTokens, *report.Usage.InputTokens, "input_tokens", providerinventory.ConfidenceExact, nil)
	}
	if report.Usage.OutputTokens != nil {
		add(providerinventory.QuantityOutputTokens, *report.Usage.OutputTokens, "output_tokens", providerinventory.ConfidenceExact, nil)
	}
	switch {
	case report.Usage.TotalTokens != nil:
		add(providerinventory.QuantityTotalTokens, *report.Usage.TotalTokens, "total_tokens", providerinventory.ConfidenceExact, nil)
	case report.Usage.InputTokens != nil && report.Usage.OutputTokens != nil:
		add(providerinventory.QuantityTotalTokens, *report.Usage.InputTokens+*report.Usage.OutputTokens, "input_plus_output_tokens", providerinventory.ConfidenceExact, []string{"total-derived-from-exact-input-output"})
	default:
		return IngestionResult{Records: out, GapReasons: []string{"partial-report-usage"}}
	}
	return IngestionResult{Records: out}
}

func insertUsageRecord(ctx context.Context, tx storage.Tx, record UsageRecord) (bool, error) {
	record = normalizeUsageRecord(record)
	if err := ValidateUsageRecord(record); err != nil {
		return false, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return false, err
	}
	res, err := tx.Exec(ctx, `INSERT OR IGNORE INTO usage_records(
		usage_record_id, project_id, delivery_run_id, task_id, attempt_id, worker_id,
		sub_agent_id, adapter_id, account_profile_id, model_capability_id, event_kind,
		event_time, quantity_kind, unit, value, value_scale, confidence,
		idempotency_key, dedupe_key, payload_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.UsageRecordID, record.ProjectID, record.DeliveryRunID, record.TaskID,
		record.AttemptID, record.WorkerID, record.SubAgentID, record.AdapterID,
		record.AccountProfileID, record.ModelCapabilityID, string(record.EventKind),
		record.EventTime, string(record.QuantityKind), record.Unit, record.Value,
		record.ValueScale, string(record.Confidence), record.IdempotencyKey,
		record.DedupeKey, string(payload))
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

func normalizeUsageRecord(record UsageRecord) UsageRecord {
	if record.SchemaVersion == "" {
		record.SchemaVersion = UsageRecordSchema
	}
	if record.EventKind == "" {
		record.EventKind = EventProviderReported
	}
	if record.QuantityKind == "" {
		record.QuantityKind = providerinventory.QuantityProviderDefined
	}
	if record.Unit == "" {
		record.Unit = unitForQuantity(record.QuantityKind)
	}
	if record.Confidence == "" {
		record.Confidence = providerinventory.ConfidenceUnknown
	}
	if record.SourceRecordIDs == nil {
		record.SourceRecordIDs = []string{}
	}
	if record.GapReasons == nil {
		record.GapReasons = []string{}
	}
	record.SourceRecordIDs = dedupeStrings(record.SourceRecordIDs)
	record.GapReasons = dedupeStrings(record.GapReasons)
	return record
}

func normalizeUsageReconciliation(record UsageReconciliation) UsageReconciliation {
	if record.SchemaVersion == "" {
		record.SchemaVersion = UsageReconciliationSchema
	}
	if record.WindowKind == "" {
		record.WindowKind = providerinventory.WindowUnknown
	}
	if record.DeltaConfidence == "" {
		record.DeltaConfidence = providerinventory.ConfidenceUnknown
	}
	if record.Outcome == "" {
		record.Outcome = OutcomeUnavailable
	}
	if record.LocalRecordIDs == nil {
		record.LocalRecordIDs = []string{}
	}
	if record.CorrectionUsageRecordIDs == nil {
		record.CorrectionUsageRecordIDs = []string{}
	}
	record.LocalRecordIDs = dedupeStrings(record.LocalRecordIDs)
	record.CorrectionUsageRecordIDs = dedupeStrings(record.CorrectionUsageRecordIDs)
	return record
}

func invalidUsage(usage reporter.Usage) bool {
	return (usage.InputTokens != nil && *usage.InputTokens < 0) ||
		(usage.OutputTokens != nil && *usage.OutputTokens < 0) ||
		(usage.TotalTokens != nil && *usage.TotalTokens < 0)
}

func originalQuantity(report reporter.Report, field string, value int64) json.RawMessage {
	payload := map[string]any{
		"provider":       report.Provider,
		"model":          report.Model,
		"reported_field": field,
		"value":          value,
		"unit":           "token",
		"source":         "reporter.usage." + field,
	}
	data, _ := json.Marshal(payload)
	return data
}

func sourceRecordID(record reportquery.Record) string {
	return "report_" + hashBase32(strings.Join([]string{
		record.Source,
		record.RunID,
		record.Path,
		stableReportKey(record),
	}, "\x00"))[:26]
}

func stableReportKey(record reportquery.Record) string {
	report := record.Report
	key := strings.Join([]string{
		strings.TrimSpace(report.WorkID),
		strings.TrimSpace(record.RunID),
		string(report.Role),
		strings.TrimSpace(report.Provider),
		strings.TrimSpace(report.Model),
		strings.TrimSpace(report.Action),
		strings.TrimSpace(report.StartedAt),
		strings.TrimSpace(report.EndedAt),
		strconv.Itoa(report.ExitCode),
	}, "\x00")
	if strings.Trim(key, "\x00") == "" {
		key = strings.Join([]string{record.Source, record.Path}, "\x00")
	}
	return key
}

func firstReportTime(endedAt, startedAt string, fallback time.Time) time.Time {
	for _, value := range []string{endedAt, startedAt} {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return fallback.UTC()
}

func taskID(issue int) string {
	if issue <= 0 {
		return ""
	}
	return "issue-" + strconv.Itoa(issue)
}

func unitForQuantity(kind providerinventory.QuantityKind) string {
	switch kind {
	case providerinventory.QuantityInputTokens, providerinventory.QuantityOutputTokens, providerinventory.QuantityTotalTokens:
		return "token"
	case providerinventory.QuantityRequests:
		return "request"
	case providerinventory.QuantityWallMS:
		return "millisecond"
	case providerinventory.QuantityConcurrency:
		return "slot"
	default:
		return "provider-defined"
	}
}

func modelFromOriginalQuantity(raw json.RawMessage) string {
	var payload struct {
		Model string `json:"model"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	return payload.Model
}

func fingerprintBudget(budget QuotaUsageBudget) string {
	copyBudget := budget
	copyBudget.QuotaUsageFingerprint = ""
	data, _ := json.Marshal(copyBudget)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func knownEventKind(kind EventKind) bool {
	switch kind {
	case EventEstimate, EventReservationCreated, EventStarted, EventStreamUpdate, EventCompletion, EventCancellation, EventFailure, EventReservationCommitted, EventReservationReleased, EventProviderReported, EventCorrection:
		return true
	default:
		return false
	}
}

func knownReconciliationOutcome(outcome ReconciliationOutcome) bool {
	switch outcome {
	case OutcomeMatched, OutcomeProviderHigher, OutcomeProviderLower, OutcomePartial, OutcomeConflicting, OutcomeUnavailable:
		return true
	default:
		return false
	}
}

func sortUsageRecords(records []UsageRecord) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].EventTime != records[j].EventTime {
			return records[i].EventTime < records[j].EventTime
		}
		return records[i].UsageRecordID < records[j].UsageRecordID
	})
}

func malformedGap(gaps []string, count int) []string {
	if count <= 0 {
		return gaps
	}
	out := make([]string, 0, len(gaps)+1)
	for _, gap := range gaps {
		if gap == "malformed-report-payloads" {
			continue
		}
		out = append(out, gap)
	}
	out = append(out, "malformed-report-payloads:"+strconv.Itoa(count))
	sort.Strings(out)
	return out
}

func dedupeStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func hashBase32(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return strings.ToLower(encoded)
}

func hashHex(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
