package providerinventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/reportquery"
	"github.com/jasonhnd/loopcoder/internal/reporter"
	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	UsageRecordSchema         = "loopcoder.usage_record.v1"
	UsageReconciliationSchema = "loopcoder.usage_reconciliation.v1"
	QuotaUsageBudgetSchema    = "loopcoder.quota_usage_budget_json.v1"
)

var (
	ErrDuplicateReplay              = errors.New("ErrDuplicateReplay")
	ErrUsageReconciliationConflict  = errors.New("ErrUsageReconciliationConflict")
	ErrCrossProjectReference        = errors.New("ErrCrossProjectReference")
	ErrUsageRetentionProtected      = errors.New("ErrUsageRetentionProtected")
	ErrUsageEstimateCannotBeExact   = errors.New("ErrUsageEstimateCannotBeExact")
	ErrUsageRecordMalformed         = errors.New("ErrUsageRecordMalformed")
	ErrUsageReconciliationMalformed = errors.New("ErrUsageReconciliationMalformed")
)

type UsageEventKind string

const (
	UsageEventEstimate             UsageEventKind = "estimate"
	UsageEventReservationCreated   UsageEventKind = "reservation-created"
	UsageEventStarted              UsageEventKind = "started"
	UsageEventStreamUpdate         UsageEventKind = "stream-update"
	UsageEventCompletion           UsageEventKind = "completion"
	UsageEventCancellation         UsageEventKind = "cancellation"
	UsageEventFailure              UsageEventKind = "failure"
	UsageEventReservationCommitted UsageEventKind = "reservation-committed"
	UsageEventReservationReleased  UsageEventKind = "reservation-released"
	UsageEventProviderReported     UsageEventKind = "provider-reported"
	UsageEventCorrection           UsageEventKind = "correction"
)

type UsageReconciliationOutcome string

const (
	UsageReconciliationMatched        UsageReconciliationOutcome = "matched"
	UsageReconciliationProviderHigher UsageReconciliationOutcome = "provider-higher"
	UsageReconciliationProviderLower  UsageReconciliationOutcome = "provider-lower"
	UsageReconciliationPartial        UsageReconciliationOutcome = "partial"
	UsageReconciliationConflicting    UsageReconciliationOutcome = "conflicting"
	UsageReconciliationUnavailable    UsageReconciliationOutcome = "unavailable"
)

type UsageRetentionOutcome string

const (
	UsageRetentionRetainProtected UsageRetentionOutcome = "retain-protected"
	UsageRetentionRetainWindow    UsageRetentionOutcome = "retain-window"
	UsageRetentionDeleteEligible  UsageRetentionOutcome = "delete-eligible"
	UsageRetentionRefuse          UsageRetentionOutcome = "refuse"
)

type UsageEstimator struct {
	Name             string            `json:"name"`
	EstimatorVersion string            `json:"estimator_version"`
	Inputs           map[string]string `json:"inputs"`
	CapturedAt       string            `json:"captured_at"`
	ErrorBound       string            `json:"error_bound,omitempty"`
	Heuristic        bool              `json:"heuristic"`
}

type UsageRecord struct {
	SchemaVersion         string          `json:"schema_version"`
	RecordVersion         int             `json:"record_version"`
	UsageRecordID         string          `json:"usage_record_id"`
	EventKind             UsageEventKind  `json:"event_kind"`
	EventTime             string          `json:"event_time"`
	ProjectID             *string         `json:"project_id,omitempty"`
	DeliveryRunID         string          `json:"delivery_run_id,omitempty"`
	TaskID                string          `json:"task_id,omitempty"`
	AttemptID             string          `json:"attempt_id,omitempty"`
	WorkerID              string          `json:"worker_id,omitempty"`
	SubAgentID            string          `json:"sub_agent_id,omitempty"`
	AdapterID             string          `json:"adapter_id,omitempty"`
	AccountProfileID      *string         `json:"account_profile_id,omitempty"`
	ModelCapabilityID     *string         `json:"model_capability_id,omitempty"`
	BudgetReservationID   string          `json:"budget_reservation_id,omitempty"`
	QuantityKind          QuantityKind    `json:"quantity_kind"`
	Value                 int64           `json:"value"`
	Unit                  string          `json:"unit"`
	ValueScale            int             `json:"value_scale"`
	OriginalQuantity      json.RawMessage `json:"original_quantity_json,omitempty"`
	Confidence            Confidence      `json:"confidence"`
	Estimator             *UsageEstimator `json:"estimator,omitempty"`
	SourceRecordIDs       []string        `json:"source_record_ids"`
	IdempotencyKey        string          `json:"idempotency_key"`
	DedupeKey             string          `json:"dedupe_key,omitempty"`
	ReplacesUsageRecordID string          `json:"replaces_usage_record_id,omitempty"`
	FreshnessState        FreshnessState  `json:"freshness_state"`
	RetainUntil           string          `json:"retain_until,omitempty"`
	GapReasons            []string        `json:"gap_reasons"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
	PolicyVersion         string          `json:"policy_version"`
}

type UsageReconciliation struct {
	SchemaVersion            string                     `json:"schema_version"`
	RecordVersion            int                        `json:"record_version"`
	UsageReconciliationID    string                     `json:"usage_reconciliation_id"`
	ProjectID                *string                    `json:"project_id,omitempty"`
	ProviderSnapshotID       string                     `json:"provider_snapshot_id,omitempty"`
	LocalRecordIDs           []string                   `json:"local_record_ids"`
	ScopeKey                 string                     `json:"scope_key"`
	QuantityKind             QuantityKind               `json:"quantity_kind"`
	WindowKind               WindowKind                 `json:"window_kind"`
	WindowStart              string                     `json:"window_start,omitempty"`
	WindowEnd                string                     `json:"window_end,omitempty"`
	LocalTotal               int64                      `json:"local_total"`
	ProviderTotal            *int64                     `json:"provider_total,omitempty"`
	Delta                    *int64                     `json:"delta,omitempty"`
	DeltaConfidence          Confidence                 `json:"delta_confidence"`
	Outcome                  UsageReconciliationOutcome `json:"outcome"`
	CorrectionUsageRecordIDs []string                   `json:"correction_usage_record_ids"`
	IdempotencyKey           string                     `json:"idempotency_key"`
	EventTime                string                     `json:"event_time"`
	GapReasons               []string                   `json:"gap_reasons"`
	CreatedAt                string                     `json:"created_at"`
	UpdatedAt                string                     `json:"updated_at"`
	PolicyVersion            string                     `json:"policy_version"`
}

type UsageSummary struct {
	ProjectID        string       `json:"project_id,omitempty"`
	DeliveryRunID    string       `json:"delivery_run_id,omitempty"`
	AdapterID        string       `json:"adapter_id,omitempty"`
	Model            string       `json:"model,omitempty"`
	QuantityKind     QuantityKind `json:"quantity_kind"`
	Unit             string       `json:"unit"`
	Value            int64        `json:"value"`
	Confidence       Confidence   `json:"confidence"`
	UsageRecordIDs   []string     `json:"usage_record_ids"`
	GapReasons       []string     `json:"gap_reasons"`
	EstimatorSummary string       `json:"estimator_summary,omitempty"`
}

type QuotaUsageBudgetReport struct {
	SchemaVersion         string                 `json:"schema_version"`
	GeneratedAt           string                 `json:"generated_at"`
	QuotaUsageFingerprint string                 `json:"quota_usage_fingerprint"`
	Confidence            Confidence             `json:"confidence"`
	QuotaSources          []QuotaTelemetrySource `json:"quota_sources"`
	QuotaSnapshots        []QuotaSnapshot        `json:"quota_snapshots"`
	UsageSummary          []UsageSummary         `json:"usage_summary"`
	UsageRecords          []UsageRecord          `json:"usage_records,omitempty"`
	Reconciliations       []UsageReconciliation  `json:"usage_reconciliations,omitempty"`
	BudgetSummary         []any                  `json:"budget_summary"`
	AvailabilityScores    []any                  `json:"availability_scores"`
	CircuitBreakers       []any                  `json:"circuit_breakers"`
	GapReasons            []string               `json:"gap_reasons"`
}

type StoredReportRecord struct {
	ReportID   string
	ProjectID  string
	RunID      string
	SourceKind string
	SourcePath string
	SourceHash string
	CreatedAt  string
	Report     reporter.Report
}

type storedReportLoadResult struct {
	Records           []StoredReportRecord
	MalformedPayloads int
}

type UsageRetentionDecision struct {
	UsageRecordID string                `json:"usage_record_id"`
	Outcome       UsageRetentionOutcome `json:"outcome"`
	RetainUntil   string                `json:"retain_until,omitempty"`
	Reason        string                `json:"reason"`
}

func (k *UsageEventKind) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "event_kind")
	if err != nil {
		return err
	}
	if !knownUsageEventKind(UsageEventKind(value)) {
		return fmt.Errorf("%w: unknown event_kind %q", ErrInvalidRecord, value)
	}
	*k = UsageEventKind(value)
	return nil
}

func (o *UsageReconciliationOutcome) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "outcome")
	if err != nil {
		return err
	}
	if !knownUsageReconciliationOutcome(UsageReconciliationOutcome(value)) {
		return fmt.Errorf("%w: unknown usage reconciliation outcome %q", ErrInvalidRecord, value)
	}
	*o = UsageReconciliationOutcome(value)
	return nil
}

func (o *UsageRetentionOutcome) UnmarshalJSON(data []byte) error {
	value, err := unmarshalEnumString(data, "outcome")
	if err != nil {
		return err
	}
	if !knownUsageRetentionOutcome(UsageRetentionOutcome(value)) {
		return fmt.Errorf("%w: unknown usage retention outcome %q", ErrInvalidRecord, value)
	}
	*o = UsageRetentionOutcome(value)
	return nil
}

func UsageRecordsFromReporter(record StoredReportRecord, observedAt time.Time) ([]UsageRecord, error) {
	report := record.Report
	eventTime := firstNonEmpty(report.EndedAt, report.StartedAt, record.CreatedAt, formatTime(observedAt))
	projectID := strings.TrimSpace(record.ProjectID)
	var projectPtr *string
	if projectID != "" {
		projectPtr = &projectID
	}
	sourceID := "report:" + strings.TrimSpace(record.ReportID)
	if strings.TrimSpace(record.SourceHash) != "" {
		sourceID = "report-hash:" + record.SourceHash
	}
	sourceIDs := []string{sourceID}
	if strings.TrimSpace(record.SourcePath) != "" {
		sourceIDs = append(sourceIDs, "path-hash:"+hashHex(record.SourcePath))
	}
	dedupe := reporterDedupeKey(report)
	base := []string{projectID, strings.TrimSpace(record.RunID), dedupe}
	var records []UsageRecord
	add := func(kind QuantityKind, value int64, confidence Confidence, original any, gaps []string) error {
		unit := unitForQuantity(kind, "")
		originalJSON, err := marshalRaw(original)
		if err != nil {
			return err
		}
		idempotency := usageIdempotencyKey(append(base, string(kind), strconv.FormatInt(value, 10))...)
		usage := normalizeUsageRecord(UsageRecord{
			UsageRecordID:    usageRecordID(idempotency),
			EventKind:        UsageEventCompletion,
			EventTime:        eventTime,
			ProjectID:        projectPtr,
			DeliveryRunID:    strings.TrimSpace(record.RunID),
			AttemptID:        strings.TrimSpace(report.WorkID),
			WorkerID:         strings.TrimSpace(report.WorkID),
			AdapterID:        strings.TrimSpace(report.Provider),
			QuantityKind:     kind,
			Value:            value,
			Unit:             unit,
			ValueScale:       0,
			OriginalQuantity: originalJSON,
			Confidence:       confidence,
			SourceRecordIDs:  sourceIDs,
			IdempotencyKey:   idempotency,
			DedupeKey:        dedupe,
			FreshnessState:   FreshnessFresh,
			RetainUntil:      "",
			GapReasons:       gaps,
			CreatedAt:        firstNonEmpty(record.CreatedAt, eventTime),
			UpdatedAt:        firstNonEmpty(record.CreatedAt, eventTime),
			PolicyVersion:    PolicyVersion,
		})
		if err := ValidateUsageRecord(usage); err != nil {
			return err
		}
		records = append(records, usage)
		return nil
	}
	if report.HasUsage() {
		if report.Usage.InputTokens != nil {
			if err := add(QuantityInputTokens, *report.Usage.InputTokens, ConfidenceExact, map[string]any{"field": "usage.input_tokens", "unit": "token", "value": *report.Usage.InputTokens}, usageGaps(report.Usage, QuantityInputTokens)); err != nil {
				return nil, err
			}
		}
		if report.Usage.OutputTokens != nil {
			if err := add(QuantityOutputTokens, *report.Usage.OutputTokens, ConfidenceExact, map[string]any{"field": "usage.output_tokens", "unit": "token", "value": *report.Usage.OutputTokens}, usageGaps(report.Usage, QuantityOutputTokens)); err != nil {
				return nil, err
			}
		}
		if report.Usage.TotalTokens != nil {
			if err := add(QuantityTotalTokens, *report.Usage.TotalTokens, ConfidenceExact, map[string]any{"field": "usage.total_tokens", "unit": "token", "value": *report.Usage.TotalTokens}, usageGaps(report.Usage, QuantityTotalTokens)); err != nil {
				return nil, err
			}
		} else if report.Usage.InputTokens != nil && report.Usage.OutputTokens != nil {
			total := *report.Usage.InputTokens + *report.Usage.OutputTokens
			if err := add(QuantityTotalTokens, total, ConfidenceExact, map[string]any{"field": "usage.input_tokens+usage.output_tokens", "unit": "token", "value": total, "conversion_rule": "exact-split-sum"}, []string{"provider-total-not-reported"}); err != nil {
				return nil, err
			}
		}
	}
	if report.HasDurationMS() {
		if err := add(QuantityWallMS, report.DurationMS, ConfidenceExact, map[string]any{"field": "duration_ms", "unit": "millisecond", "value": report.DurationMS}, []string{"local-wall-time-not-provider-billing"}); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(report.Provider) != "" {
		if err := add(QuantityRequests, 1, ConfidenceExact, map[string]any{"field": "report", "unit": "request", "value": 1}, []string{"local-launch-count-only"}); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func RefreshUsageLedgerFromReports(ctx context.Context, store storage.Store, projectID string, now time.Time) error {
	if store == nil {
		return errors.New("usage ledger refresh: storage store is required")
	}
	projectID = strings.TrimSpace(projectID)
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		loaded, err := loadStoredReports(ctx, tx, projectID)
		if err != nil {
			return err
		}
		for _, reportRecord := range loaded.Records {
			usageRecords, err := UsageRecordsFromReporter(reportRecord, now)
			if err != nil {
				return err
			}
			for _, usage := range usageRecords {
				if err := insertUsageRecord(ctx, tx, usage); err != nil {
					return err
				}
			}
		}
		if loaded.MalformedPayloads > 0 {
			return fmt.Errorf("usage ledger refresh: malformed-report-payloads:%d", loaded.MalformedPayloads)
		}
		return nil
	})
}

func LoadQuotaUsageBudget(ctx context.Context, store storage.Store, projectID string, includeRecords bool) (QuotaUsageBudgetReport, error) {
	if store == nil {
		return EmptyQuotaUsageBudget(time.Time{}), errors.New("quota usage budget load: storage store is required")
	}
	projectID = strings.TrimSpace(projectID)
	var quotaSources []QuotaTelemetrySource
	var quotaSnapshots []QuotaSnapshot
	var usageRecords []UsageRecord
	var reconciliations []UsageReconciliation
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		var err error
		quotaSources, err = loadQuotaSources(ctx, tx)
		if err != nil {
			return err
		}
		quotaSnapshots, err = loadQuotaSnapshots(ctx, tx)
		if err != nil {
			return err
		}
		usageRecords, err = loadUsageRecords(ctx, tx, projectID)
		if err != nil {
			return err
		}
		reconciliations, err = loadUsageReconciliations(ctx, tx, projectID)
		return err
	})
	if err != nil {
		return EmptyQuotaUsageBudget(store.Now()), err
	}
	now := store.Now()
	usageRecords = markUsageFreshness(usageRecords, now)
	report := QuotaUsageBudgetReport{
		SchemaVersion:      QuotaUsageBudgetSchema,
		GeneratedAt:        formatTime(now),
		Confidence:         usageReportConfidence(usageRecords, quotaSnapshots),
		QuotaSources:       quotaSources,
		QuotaSnapshots:     markQuotaFreshness(LinkQuotaConflicts(quotaSnapshots), now),
		UsageSummary:       SummarizeUsage(usageRecords),
		Reconciliations:    reconciliations,
		BudgetSummary:      []any{},
		AvailabilityScores: []any{},
		CircuitBreakers:    []any{},
		GapReasons:         usageReportGaps(usageRecords, quotaSnapshots),
	}
	if includeRecords {
		report.UsageRecords = usageRecords
	}
	report.QuotaUsageFingerprint = quotaUsageFingerprint(report)
	return report, nil
}

func BuildQuotaUsageBudgetFromReports(ctx context.Context, store storage.Store, projectID string, now time.Time, includeRecords bool) (QuotaUsageBudgetReport, error) {
	if store == nil {
		return EmptyQuotaUsageBudget(now), errors.New("quota usage budget from reports: storage store is required")
	}
	projectID = strings.TrimSpace(projectID)
	var usageRecords []UsageRecord
	malformedPayloads := 0
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		loaded, err := loadStoredReports(ctx, tx, projectID)
		if err != nil {
			return err
		}
		malformedPayloads = loaded.MalformedPayloads
		for _, reportRecord := range loaded.Records {
			records, err := UsageRecordsFromReporter(reportRecord, now)
			if err != nil {
				return err
			}
			usageRecords = append(usageRecords, records...)
		}
		return nil
	})
	if err != nil {
		return EmptyQuotaUsageBudget(now), err
	}
	return buildQuotaUsageBudgetFromUsageRecords(usageRecords, now, includeRecords, "derived-from-imported-reporter-records", malformedPayloads), nil
}

func BuildQuotaUsageBudgetFromReportQueryRecords(records []reportquery.Record, projectID string, now time.Time, includeRecords bool) (QuotaUsageBudgetReport, error) {
	stored := StoredReportRecordsFromReportQuery(projectID, records, now)
	var usageRecords []UsageRecord
	for _, reportRecord := range stored {
		records, err := UsageRecordsFromReporter(reportRecord, now)
		if err != nil {
			return EmptyQuotaUsageBudget(now), err
		}
		usageRecords = append(usageRecords, records...)
	}
	return buildQuotaUsageBudgetFromUsageRecords(usageRecords, now, includeRecords, "derived-from-local-reporter-files", 0), nil
}

func StoredReportRecordsFromReportQuery(projectID string, records []reportquery.Record, observedAt time.Time) []StoredReportRecord {
	projectID = strings.TrimSpace(projectID)
	out := make([]StoredReportRecord, 0, len(records))
	for _, record := range records {
		payload, err := json.Marshal(record.Report)
		if err != nil {
			continue
		}
		sourcePath := strings.TrimSpace(record.Path)
		sourceKind := strings.TrimSpace(record.Source)
		runID := strings.TrimSpace(record.RunID)
		sourceHash := rawSourceHash(payload)
		reportID := "reportquery_" + hashHex(projectID, runID, sourceKind, sourcePath, sourceHash)[:24]
		out = append(out, StoredReportRecord{
			ReportID:   reportID,
			ProjectID:  projectID,
			RunID:      runID,
			SourceKind: sourceKind,
			SourcePath: sourcePath,
			SourceHash: sourceHash,
			CreatedAt:  firstNonEmpty(record.Report.EndedAt, record.Report.StartedAt, formatTime(observedAt)),
			Report:     record.Report,
		})
	}
	return out
}

func buildQuotaUsageBudgetFromUsageRecords(usageRecords []UsageRecord, now time.Time, includeRecords bool, sourceGap string, malformedPayloads int) QuotaUsageBudgetReport {
	usageRecords = dedupeUsageRecords(usageRecords)
	gaps := append(usageReportGaps(usageRecords, nil), sourceGap)
	if malformedPayloads > 0 {
		gaps = append(gaps, fmt.Sprintf("malformed-report-payloads:%d", malformedPayloads))
	}
	report := QuotaUsageBudgetReport{
		SchemaVersion:      QuotaUsageBudgetSchema,
		GeneratedAt:        formatTime(now),
		Confidence:         usageReportConfidence(usageRecords, nil),
		QuotaSources:       []QuotaTelemetrySource{},
		QuotaSnapshots:     []QuotaSnapshot{},
		UsageSummary:       SummarizeUsage(usageRecords),
		Reconciliations:    []UsageReconciliation{},
		BudgetSummary:      []any{},
		AvailabilityScores: []any{},
		CircuitBreakers:    []any{},
		GapReasons:         dedupeStrings(gaps),
	}
	if includeRecords {
		report.UsageRecords = usageRecords
	}
	report.QuotaUsageFingerprint = quotaUsageFingerprint(report)
	return report
}

func EmptyQuotaUsageBudget(now time.Time) QuotaUsageBudgetReport {
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	report := QuotaUsageBudgetReport{
		SchemaVersion:         QuotaUsageBudgetSchema,
		GeneratedAt:           formatTime(now),
		QuotaUsageFingerprint: "sha256:" + hashHex("loopcoder.quota_usage_budget.v1", "empty"),
		Confidence:            ConfidenceUnknown,
		QuotaSources:          []QuotaTelemetrySource{},
		QuotaSnapshots:        []QuotaSnapshot{},
		UsageSummary:          []UsageSummary{},
		BudgetSummary:         []any{},
		AvailabilityScores:    []any{},
		CircuitBreakers:       []any{},
		GapReasons:            []string{"usage-ledger-empty"},
	}
	return report
}

func QuotaUsageRefsFromUsageRecords(records []UsageRecord) QuotaUsageRefs {
	ids := make([]string, 0, len(records))
	confidence := Confidence("")
	var gaps []string
	for _, record := range records {
		ids = append(ids, record.UsageRecordID)
		confidence = combineUsageConfidence(confidence, record.Confidence)
		gaps = append(gaps, record.GapReasons...)
	}
	sort.Strings(ids)
	refs := EmptyQuotaUsageRefs()
	refs.UsageRecordIDs = dedupeStrings(ids)
	if confidence == "" {
		confidence = ConfidenceUnknown
	}
	refs.Confidence = confidence
	refs.GapReasons = dedupeStrings(gaps)
	if len(refs.UsageRecordIDs) == 0 {
		refs.GapReasons = []string{"usage-ledger-empty"}
	}
	refs.QuotaUsageFingerprint = "sha256:" + hashHex("loopcoder.quota_usage_refs.v1", strings.Join(refs.UsageRecordIDs, "\x00"), string(refs.Confidence), strings.Join(refs.GapReasons, "\x00"))
	return refs
}

func ReconcileUsage(provider UsageRecord, local []UsageRecord, idempotencyKey string, now time.Time) (UsageReconciliation, []UsageRecord, error) {
	provider = normalizeUsageRecord(provider)
	if err := ValidateUsageRecord(provider); err != nil {
		return UsageReconciliation{}, nil, err
	}
	projectKey := ptrValue(provider.ProjectID)
	localTotal := int64(0)
	localIDs := make([]string, 0, len(local))
	for _, record := range local {
		record = normalizeUsageRecord(record)
		if err := ValidateUsageRecord(record); err != nil {
			return UsageReconciliation{}, nil, err
		}
		if ptrValue(record.ProjectID) != projectKey {
			return UsageReconciliation{}, nil, fmt.Errorf("%w: usage record %q crosses project scope", ErrCrossProjectReference, record.UsageRecordID)
		}
		if record.QuantityKind != provider.QuantityKind {
			continue
		}
		localTotal += record.Value
		localIDs = append(localIDs, record.UsageRecordID)
	}
	sort.Strings(localIDs)
	providerTotal := provider.Value
	delta := providerTotal - localTotal
	outcome := UsageReconciliationMatched
	gaps := []string{}
	switch {
	case delta > 0:
		outcome = UsageReconciliationProviderHigher
		gaps = append(gaps, "provider-higher-may-include-out-of-band-usage")
	case delta < 0:
		outcome = UsageReconciliationProviderLower
		gaps = append(gaps, "provider-lower-may-reflect-rounding-or-partial-local-records")
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		idempotencyKey = usageIdempotencyKey("reconcile", provider.UsageRecordID, strings.Join(localIDs, ","), strconv.FormatInt(providerTotal, 10))
	}
	corrections := []UsageRecord{}
	if delta != 0 {
		correctionKey := usageIdempotencyKey(idempotencyKey, "correction", strconv.FormatInt(delta, 10))
		correction := normalizeUsageRecord(UsageRecord{
			UsageRecordID:         usageRecordID(correctionKey),
			EventKind:             UsageEventCorrection,
			EventTime:             formatTime(now),
			ProjectID:             provider.ProjectID,
			DeliveryRunID:         provider.DeliveryRunID,
			AdapterID:             provider.AdapterID,
			QuantityKind:          provider.QuantityKind,
			Value:                 delta,
			Unit:                  provider.Unit,
			ValueScale:            provider.ValueScale,
			Confidence:            provider.Confidence,
			SourceRecordIDs:       append([]string{provider.UsageRecordID}, localIDs...),
			IdempotencyKey:        correctionKey,
			DedupeKey:             correctionKey,
			ReplacesUsageRecordID: "",
			FreshnessState:        FreshnessFresh,
			GapReasons:            gaps,
			CreatedAt:             formatTime(now),
			UpdatedAt:             formatTime(now),
			PolicyVersion:         PolicyVersion,
		})
		if err := ValidateUsageRecord(correction); err != nil {
			return UsageReconciliation{}, nil, err
		}
		corrections = append(corrections, correction)
	}
	correctionIDs := make([]string, 0, len(corrections))
	for _, correction := range corrections {
		correctionIDs = append(correctionIDs, correction.UsageRecordID)
	}
	reconciliation := normalizeUsageReconciliation(UsageReconciliation{
		UsageReconciliationID:    usageReconciliationID(idempotencyKey),
		ProjectID:                provider.ProjectID,
		ProviderSnapshotID:       provider.UsageRecordID,
		LocalRecordIDs:           localIDs,
		ScopeKey:                 usageScopeKey(provider),
		QuantityKind:             provider.QuantityKind,
		WindowKind:               WindowUnbounded,
		LocalTotal:               localTotal,
		ProviderTotal:            &providerTotal,
		Delta:                    &delta,
		DeltaConfidence:          provider.Confidence,
		Outcome:                  outcome,
		CorrectionUsageRecordIDs: correctionIDs,
		IdempotencyKey:           idempotencyKey,
		EventTime:                formatTime(now),
		GapReasons:               gaps,
		CreatedAt:                formatTime(now),
		UpdatedAt:                formatTime(now),
		PolicyVersion:            PolicyVersion,
	})
	if err := ValidateUsageReconciliation(reconciliation); err != nil {
		return UsageReconciliation{}, nil, err
	}
	return reconciliation, corrections, nil
}

func EstimateRemainingUsage(ceiling int64, committed []UsageRecord, reservations []UsageRecord, idempotencyKey string, now time.Time) (UsageRecord, error) {
	total := int64(0)
	sourceIDs := []string{}
	for _, record := range append(append([]UsageRecord{}, committed...), reservations...) {
		total += record.Value
		sourceIDs = append(sourceIDs, record.UsageRecordID)
	}
	remaining := ceiling - total
	if remaining < 0 {
		remaining = 0
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		idempotencyKey = usageIdempotencyKey("estimate-remaining", strconv.FormatInt(ceiling, 10), strconv.FormatInt(total, 10), formatTime(now))
	}
	record := normalizeUsageRecord(UsageRecord{
		UsageRecordID:   usageRecordID(idempotencyKey),
		EventKind:       UsageEventEstimate,
		EventTime:       formatTime(now),
		QuantityKind:    QuantityLocalPolicy,
		Value:           remaining,
		Unit:            unitForQuantity(QuantityLocalPolicy, ""),
		ValueScale:      0,
		Confidence:      ConfidenceEstimated,
		Estimator:       &UsageEstimator{Name: "conservative-local-ledger", EstimatorVersion: "v1", Inputs: map[string]string{"ceiling": strconv.FormatInt(ceiling, 10), "known_local_usage": strconv.FormatInt(total, 10)}, CapturedAt: formatTime(now), ErrorBound: "provider-side out-of-band usage unknown", Heuristic: true},
		SourceRecordIDs: sourceIDs,
		IdempotencyKey:  idempotencyKey,
		FreshnessState:  FreshnessFresh,
		GapReasons:      []string{"provider-wide-usage-unknown", "estimate-not-exact-quota"},
		CreatedAt:       formatTime(now),
		UpdatedAt:       formatTime(now),
		PolicyVersion:   PolicyVersion,
	})
	if err := ValidateUsageRecord(record); err != nil {
		return UsageRecord{}, err
	}
	return record, nil
}

func EvaluateUsageRetention(records []UsageRecord, now time.Time, minRetention time.Duration, protectedIDs map[string]bool) []UsageRetentionDecision {
	if minRetention < 0 {
		minRetention = 0
	}
	out := make([]UsageRetentionDecision, 0, len(records))
	for _, record := range records {
		id := strings.TrimSpace(record.UsageRecordID)
		if protectedIDs[id] {
			out = append(out, UsageRetentionDecision{UsageRecordID: id, Outcome: UsageRetentionRetainProtected, RetainUntil: record.RetainUntil, Reason: "referenced-by-active-runtime-state"})
			continue
		}
		eventTime, err := time.Parse(time.RFC3339Nano, record.EventTime)
		if err != nil {
			out = append(out, UsageRetentionDecision{UsageRecordID: id, Outcome: UsageRetentionRefuse, Reason: "invalid-event-time"})
			continue
		}
		retainUntil := eventTime.UTC().Add(minRetention)
		if strings.TrimSpace(record.RetainUntil) != "" {
			if explicit, err := time.Parse(time.RFC3339Nano, record.RetainUntil); err == nil && explicit.After(retainUntil) {
				retainUntil = explicit.UTC()
			}
		}
		if now.UTC().Before(retainUntil) {
			out = append(out, UsageRetentionDecision{UsageRecordID: id, Outcome: UsageRetentionRetainWindow, RetainUntil: formatTime(retainUntil), Reason: "within-retention-window"})
			continue
		}
		out = append(out, UsageRetentionDecision{UsageRecordID: id, Outcome: UsageRetentionDeleteEligible, RetainUntil: formatTime(retainUntil), Reason: "expired-retention-window"})
	}
	return out
}

func SummarizeUsage(records []UsageRecord) []UsageSummary {
	type key struct {
		project string
		run     string
		adapter string
		model   string
		kind    QuantityKind
		unit    string
	}
	byKey := map[key]*UsageSummary{}
	for _, record := range records {
		k := key{project: ptrValue(record.ProjectID), run: record.DeliveryRunID, adapter: record.AdapterID, model: ptrValue(record.ModelCapabilityID), kind: record.QuantityKind, unit: record.Unit}
		summary := byKey[k]
		if summary == nil {
			summary = &UsageSummary{
				ProjectID:      k.project,
				DeliveryRunID:  k.run,
				AdapterID:      k.adapter,
				Model:          k.model,
				QuantityKind:   k.kind,
				Unit:           k.unit,
				Confidence:     record.Confidence,
				UsageRecordIDs: []string{},
				GapReasons:     []string{},
			}
			byKey[k] = summary
		}
		summary.Value += record.Value
		summary.UsageRecordIDs = append(summary.UsageRecordIDs, record.UsageRecordID)
		summary.GapReasons = dedupeStrings(append(summary.GapReasons, record.GapReasons...))
		summary.Confidence = combineUsageConfidence(summary.Confidence, record.Confidence)
		if record.Estimator != nil && summary.EstimatorSummary == "" {
			summary.EstimatorSummary = record.Estimator.Name + "/" + record.Estimator.EstimatorVersion
		}
	}
	keys := make([]key, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.Join([]string{keys[i].project, keys[i].run, keys[i].adapter, string(keys[i].kind), keys[i].unit}, "\x00") <
			strings.Join([]string{keys[j].project, keys[j].run, keys[j].adapter, string(keys[j].kind), keys[j].unit}, "\x00")
	})
	out := make([]UsageSummary, 0, len(keys))
	for _, k := range keys {
		summary := *byKey[k]
		sort.Strings(summary.UsageRecordIDs)
		out = append(out, summary)
	}
	return out
}

func dedupeUsageRecords(records []UsageRecord) []UsageRecord {
	seen := map[string]bool{}
	out := make([]UsageRecord, 0, len(records))
	for _, record := range records {
		key := record.IdempotencyKey
		if key == "" {
			key = record.UsageRecordID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, record)
	}
	return out
}

func ValidateUsageRecord(record UsageRecord) error {
	record = normalizeUsageRecord(record)
	if !strings.HasPrefix(record.UsageRecordID, "usage_") {
		return fmt.Errorf("%w: usage_record_id must use usage_ prefix", ErrUsageRecordMalformed)
	}
	if !knownUsageEventKind(record.EventKind) {
		return fmt.Errorf("%w: unknown event_kind %q", ErrUsageRecordMalformed, record.EventKind)
	}
	if !knownQuantityKind(record.QuantityKind) {
		return fmt.Errorf("%w: unknown quantity_kind %q", ErrUsageRecordMalformed, record.QuantityKind)
	}
	if record.Unit == "" || record.Unit != unitForQuantity(record.QuantityKind, record.Unit) && record.QuantityKind != QuantityProviderDefined {
		return fmt.Errorf("%w: unit %q does not match quantity %q", ErrUsageRecordMalformed, record.Unit, record.QuantityKind)
	}
	if record.ValueScale < 0 || strings.TrimSpace(record.EventTime) == "" || strings.TrimSpace(record.IdempotencyKey) == "" {
		return fmt.Errorf("%w: usage record requires event_time, idempotency_key, and non-negative scale", ErrUsageRecordMalformed)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.EventTime); err != nil {
		return fmt.Errorf("%w: event_time must be RFC3339", ErrUsageRecordMalformed)
	}
	if record.Confidence == ConfidenceEstimated && record.Estimator == nil {
		return fmt.Errorf("%w: estimated usage requires estimator metadata", ErrUsageEstimateCannotBeExact)
	}
	if record.Confidence != ConfidenceEstimated && record.Estimator != nil && record.Estimator.Heuristic {
		return fmt.Errorf("%w: heuristic usage cannot be exact", ErrUsageEstimateCannotBeExact)
	}
	if len(record.SourceRecordIDs) == 0 {
		return fmt.Errorf("%w: source_record_ids is required", ErrUsageRecordMalformed)
	}
	return nil
}

func ValidateUsageReconciliation(record UsageReconciliation) error {
	record = normalizeUsageReconciliation(record)
	if !strings.HasPrefix(record.UsageReconciliationID, "urec_") {
		return fmt.Errorf("%w: usage_reconciliation_id must use urec_ prefix", ErrUsageReconciliationMalformed)
	}
	if !knownUsageReconciliationOutcome(record.Outcome) || !knownQuantityKind(record.QuantityKind) || !knownWindowKind(record.WindowKind) {
		return fmt.Errorf("%w: unknown reconciliation enum", ErrUsageReconciliationMalformed)
	}
	if strings.TrimSpace(record.ScopeKey) == "" || strings.TrimSpace(record.IdempotencyKey) == "" {
		return fmt.Errorf("%w: scope_key and idempotency_key are required", ErrUsageReconciliationMalformed)
	}
	if record.ProviderTotal != nil && record.Delta == nil {
		return fmt.Errorf("%w: provider_total requires delta", ErrUsageReconciliationMalformed)
	}
	return nil
}

func normalizeUsageRecord(record UsageRecord) UsageRecord {
	if record.SchemaVersion == "" {
		record.SchemaVersion = UsageRecordSchema
	}
	if record.RecordVersion == 0 {
		record.RecordVersion = 1
	}
	if record.EventKind == "" {
		record.EventKind = UsageEventCompletion
	}
	if record.QuantityKind == "" {
		record.QuantityKind = QuantityProviderDefined
	}
	if record.Unit == "" {
		record.Unit = unitForQuantity(record.QuantityKind, "")
	}
	if record.Confidence == "" {
		record.Confidence = ConfidenceUnknown
	}
	if record.FreshnessState == "" {
		record.FreshnessState = FreshnessFresh
	}
	if record.SourceRecordIDs == nil {
		record.SourceRecordIDs = []string{}
	}
	record.SourceRecordIDs = dedupeStrings(record.SourceRecordIDs)
	record.GapReasons = dedupeStrings(record.GapReasons)
	if record.PolicyVersion == "" {
		record.PolicyVersion = PolicyVersion
	}
	return record
}

func normalizeUsageReconciliation(record UsageReconciliation) UsageReconciliation {
	if record.SchemaVersion == "" {
		record.SchemaVersion = UsageReconciliationSchema
	}
	if record.RecordVersion == 0 {
		record.RecordVersion = 1
	}
	if record.WindowKind == "" {
		record.WindowKind = WindowUnbounded
	}
	if record.DeltaConfidence == "" {
		record.DeltaConfidence = ConfidenceUnknown
	}
	if record.Outcome == "" {
		record.Outcome = UsageReconciliationUnavailable
	}
	if record.LocalRecordIDs == nil {
		record.LocalRecordIDs = []string{}
	}
	if record.CorrectionUsageRecordIDs == nil {
		record.CorrectionUsageRecordIDs = []string{}
	}
	record.LocalRecordIDs = dedupeStrings(record.LocalRecordIDs)
	record.CorrectionUsageRecordIDs = dedupeStrings(record.CorrectionUsageRecordIDs)
	record.GapReasons = dedupeStrings(record.GapReasons)
	if record.PolicyVersion == "" {
		record.PolicyVersion = PolicyVersion
	}
	return record
}

func insertUsageRecord(ctx context.Context, tx storage.Tx, record UsageRecord) error {
	record = normalizeUsageRecord(record)
	if err := ValidateUsageRecord(record); err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	projectID := any(nil)
	if record.ProjectID != nil {
		projectID = *record.ProjectID
	}
	accountID := any(nil)
	if record.AccountProfileID != nil {
		accountID = *record.AccountProfileID
	}
	modelID := any(nil)
	if record.ModelCapabilityID != nil {
		modelID = *record.ModelCapabilityID
	}
	replaces := any(nil)
	if strings.TrimSpace(record.ReplacesUsageRecordID) != "" {
		replaces = record.ReplacesUsageRecordID
	}
	_, err = tx.Exec(ctx, `INSERT OR IGNORE INTO usage_records(
		usage_record_id, schema_version, record_version, project_id, delivery_run_id,
		task_id, attempt_id, worker_id, sub_agent_id, adapter_id, account_profile_id,
		model_capability_id, event_kind, event_time, quantity_kind, value, unit,
		value_scale, confidence, freshness_state, source_kind, idempotency_key,
		dedupe_key, replaces_usage_record_id, retain_until, payload_json
	) VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.UsageRecordID, record.SchemaVersion, record.RecordVersion, projectID,
		record.DeliveryRunID, record.TaskID, record.AttemptID, record.WorkerID,
		record.SubAgentID, record.AdapterID, accountID, modelID, string(record.EventKind),
		record.EventTime, string(record.QuantityKind), record.Value, record.Unit,
		record.ValueScale, string(record.Confidence), string(record.FreshnessState),
		"reporter", record.IdempotencyKey, record.DedupeKey, replaces, record.RetainUntil,
		string(payload))
	return err
}

func InsertUsageRecord(ctx context.Context, store storage.Store, record UsageRecord) error {
	if store == nil {
		return errors.New("insert usage record: storage store is required")
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		return insertUsageRecord(ctx, tx, record)
	})
}

func InsertUsageReconciliation(ctx context.Context, store storage.Store, record UsageReconciliation) error {
	if store == nil {
		return errors.New("insert usage reconciliation: storage store is required")
	}
	record = normalizeUsageReconciliation(record)
	if err := ValidateUsageReconciliation(record); err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	projectID := any(nil)
	if record.ProjectID != nil {
		projectID = *record.ProjectID
	}
	providerTotal := any(nil)
	if record.ProviderTotal != nil {
		providerTotal = *record.ProviderTotal
	}
	delta := any(nil)
	if record.Delta != nil {
		delta = *record.Delta
	}
	return store.WithWriteTx(ctx, func(tx storage.Tx) error {
		_, err := tx.Exec(ctx, `INSERT OR IGNORE INTO usage_reconciliations(
			usage_reconciliation_id, schema_version, record_version, project_id,
			provider_snapshot_id, scope_key, quantity_kind, window_kind, window_start,
			window_end, local_total, provider_total, delta, delta_confidence, outcome,
			idempotency_key, event_time, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.UsageReconciliationID, record.SchemaVersion, record.RecordVersion,
			projectID, record.ProviderSnapshotID, record.ScopeKey, string(record.QuantityKind),
			string(record.WindowKind), record.WindowStart, record.WindowEnd, record.LocalTotal,
			providerTotal, delta, string(record.DeltaConfidence), string(record.Outcome),
			record.IdempotencyKey, record.EventTime, string(payload))
		return err
	})
}

func loadStoredReports(ctx context.Context, tx storage.Tx, projectID string) (storedReportLoadResult, error) {
	query := `SELECT id, COALESCE(project_id, ''), COALESCE(run_id, ''), source_kind, source_path, source_hash, created_at, payload_json FROM reports`
	args := []any{}
	if strings.TrimSpace(projectID) != "" {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY project_id, run_id, created_at, id`
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return storedReportLoadResult{}, err
	}
	defer rows.Close()
	var out []StoredReportRecord
	malformedPayloads := 0
	for rows.Next() {
		var record StoredReportRecord
		var payload string
		if err := rows.Scan(&record.ReportID, &record.ProjectID, &record.RunID, &record.SourceKind, &record.SourcePath, &record.SourceHash, &record.CreatedAt, &payload); err != nil {
			return storedReportLoadResult{}, err
		}
		if err := json.Unmarshal([]byte(payload), &record.Report); err != nil {
			malformedPayloads++
			continue
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return storedReportLoadResult{}, err
	}
	return storedReportLoadResult{Records: out, MalformedPayloads: malformedPayloads}, nil
}

func loadUsageRecords(ctx context.Context, tx storage.Tx, projectID string) ([]UsageRecord, error) {
	query := `SELECT payload_json FROM usage_records`
	args := []any{}
	if strings.TrimSpace(projectID) != "" {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY project_id, delivery_run_id, event_time, usage_record_id`
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRecord
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var record UsageRecord
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, err
		}
		out = append(out, normalizeUsageRecord(record))
	}
	return out, rows.Err()
}

func loadUsageReconciliations(ctx context.Context, tx storage.Tx, projectID string) ([]UsageReconciliation, error) {
	query := `SELECT payload_json FROM usage_reconciliations`
	args := []any{}
	if strings.TrimSpace(projectID) != "" {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY project_id, event_time, usage_reconciliation_id`
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageReconciliation
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var record UsageReconciliation
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, err
		}
		out = append(out, normalizeUsageReconciliation(record))
	}
	return out, rows.Err()
}

func loadQuotaSources(ctx context.Context, tx storage.Tx) ([]QuotaTelemetrySource, error) {
	rows, err := tx.Query(ctx, `SELECT payload_json FROM quota_telemetry_sources ORDER BY adapter_id, quota_source_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaTelemetrySource
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var record QuotaTelemetrySource
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func loadQuotaSnapshots(ctx context.Context, tx storage.Tx) ([]QuotaSnapshot, error) {
	rows, err := tx.Query(ctx, `SELECT payload_json FROM quota_snapshots ORDER BY adapter_id, captured_at, quota_snapshot_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaSnapshot
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var record QuotaSnapshot
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func markUsageFreshness(records []UsageRecord, now time.Time) []UsageRecord {
	out := append([]UsageRecord(nil), records...)
	for i := range out {
		if out[i].FreshnessState == FreshnessNotApplicable {
			continue
		}
		if quotaExpired(out[i].RetainUntil, now) {
			out[i].FreshnessState = FreshnessStale
			out[i].Confidence = ConfidenceStale
			out[i].GapReasons = dedupeStrings(append(out[i].GapReasons, "retention-window-expired"))
		}
	}
	return out
}

func reporterDedupeKey(report reporter.Report) string {
	return strings.Join([]string{
		strings.TrimSpace(report.WorkID),
		string(report.Role),
		strings.TrimSpace(report.Provider),
		strings.TrimSpace(report.Model),
		strings.TrimSpace(report.Action),
		strings.TrimSpace(report.StartedAt),
		strings.TrimSpace(report.EndedAt),
		strconv.Itoa(report.ExitCode),
	}, "\x00")
}

func usageGaps(usage reporter.Usage, kind QuantityKind) []string {
	var gaps []string
	if (usage.InputTokens == nil || usage.OutputTokens == nil) && kind != QuantityTotalTokens {
		gaps = append(gaps, "partial-usage")
	}
	if usage.TotalTokens != nil && usage.InputTokens != nil && usage.OutputTokens != nil && *usage.TotalTokens != *usage.InputTokens+*usage.OutputTokens {
		gaps = append(gaps, "provider-rounding-or-cache-adjustment")
	}
	return gaps
}

func marshalRaw(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}

func usageIdempotencyKey(parts ...string) string {
	return "usage-idem:" + hashHex(parts...)
}

func usageRecordID(idempotencyKey string) string {
	return "usage_" + hashBase32(idempotencyKey)[:26]
}

func usageReconciliationID(idempotencyKey string) string {
	return "urec_" + hashBase32(idempotencyKey)[:26]
}

func usageScopeKey(record UsageRecord) string {
	parts := []string{
		"project:" + ptrValue(record.ProjectID),
		"run:" + record.DeliveryRunID,
		"provider:" + record.AdapterID,
		"account:" + ptrValue(record.AccountProfileID),
		"model:" + ptrValue(record.ModelCapabilityID),
	}
	return strings.Join(parts, "/")
}

func usageReportConfidence(records []UsageRecord, snapshots []QuotaSnapshot) Confidence {
	confidence := Confidence("")
	for _, snapshot := range snapshots {
		confidence = combineUsageConfidence(confidence, snapshot.Confidence)
	}
	for _, record := range records {
		confidence = combineUsageConfidence(confidence, record.Confidence)
	}
	if confidence == "" {
		return ConfidenceUnknown
	}
	return confidence
}

func usageReportGaps(records []UsageRecord, snapshots []QuotaSnapshot) []string {
	var gaps []string
	if len(records) == 0 {
		gaps = append(gaps, "usage-ledger-empty")
	}
	for _, record := range records {
		gaps = append(gaps, record.GapReasons...)
	}
	for _, snapshot := range snapshots {
		gaps = append(gaps, snapshot.GapReasons...)
	}
	return dedupeStrings(gaps)
}

func quotaUsageFingerprint(report QuotaUsageBudgetReport) string {
	clone := report
	clone.GeneratedAt = ""
	clone.QuotaUsageFingerprint = ""
	data, err := json.Marshal(clone)
	if err != nil {
		return "sha256:" + hashHex("quota-usage-fingerprint-error", err.Error())
	}
	return rawSourceHash(data)
}

func combineUsageConfidence(left, right Confidence) Confidence {
	rank := map[Confidence]int{
		ConfidenceExact:       5,
		ConfidenceEstimated:   4,
		ConfidenceStale:       3,
		ConfidenceUnknown:     2,
		ConfidenceUnavailable: 1,
	}
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	if rank[right] < rank[left] {
		return right
	}
	return left
}

func knownUsageEventKind(kind UsageEventKind) bool {
	switch kind {
	case UsageEventEstimate, UsageEventReservationCreated, UsageEventStarted, UsageEventStreamUpdate, UsageEventCompletion, UsageEventCancellation, UsageEventFailure, UsageEventReservationCommitted, UsageEventReservationReleased, UsageEventProviderReported, UsageEventCorrection:
		return true
	default:
		return false
	}
}

func knownUsageReconciliationOutcome(outcome UsageReconciliationOutcome) bool {
	switch outcome {
	case UsageReconciliationMatched, UsageReconciliationProviderHigher, UsageReconciliationProviderLower, UsageReconciliationPartial, UsageReconciliationConflicting, UsageReconciliationUnavailable:
		return true
	default:
		return false
	}
}

func knownUsageRetentionOutcome(outcome UsageRetentionOutcome) bool {
	switch outcome {
	case UsageRetentionRetainProtected, UsageRetentionRetainWindow, UsageRetentionDeleteEligible, UsageRetentionRefuse:
		return true
	default:
		return false
	}
}
