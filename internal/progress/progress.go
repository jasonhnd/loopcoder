// Package progress defines and persists provider-neutral v0.8 progress receipts.
package progress

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	SchemaProgressReceipt = "loopcoder.progress_receipt.v1"
	Unknown               = "unknown"
	PolicyVersion         = "progress-receipt-v1"

	maxTextRunes       = 240
	maxIdentifierRunes = 120
)

type ErrorCode string

const (
	ErrInvalidRecordCode        ErrorCode = "ErrInvalidRecord"
	ErrDuplicateReplayCode      ErrorCode = "ErrDuplicateReplay"
	ErrUnknownRecordVersionCode ErrorCode = "ErrUnknownRecordVersion"
	ErrMissingReferenceCode     ErrorCode = "ErrMissingReference"
	ErrClaimConflictCode        ErrorCode = "ErrClaimConflict"
	ErrStaleClaimCode           ErrorCode = "ErrStaleClaim"
	ErrEvidenceRejectedCode     ErrorCode = "ErrEvidenceRejected"
)

var (
	ErrInvalidRecord        = &TypedError{Code: ErrInvalidRecordCode}
	ErrDuplicateReplay      = &TypedError{Code: ErrDuplicateReplayCode}
	ErrUnknownRecordVersion = &TypedError{Code: ErrUnknownRecordVersionCode}
	ErrMissingReference     = &TypedError{Code: ErrMissingReferenceCode}
	ErrClaimConflict        = &TypedError{Code: ErrClaimConflictCode}
	ErrStaleClaim           = &TypedError{Code: ErrStaleClaimCode}
	ErrEvidenceRejected     = &TypedError{Code: ErrEvidenceRejectedCode}
)

type TypedError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message,omitempty"`
}

func (e *TypedError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

func (e *TypedError) Is(target error) bool {
	var typed *TypedError
	if !errors.As(target, &typed) {
		return false
	}
	return e.Code == typed.Code
}

func typed(code ErrorCode, format string, args ...any) error {
	msg := ""
	if format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	return &TypedError{Code: code, Message: msg}
}

type ProgressReceipt struct {
	SchemaVersion       string           `json:"schema_version"`
	RecordVersion       int              `json:"record_version"`
	ProgressReceiptID   string           `json:"progress_receipt_id"`
	ProjectID           string           `json:"project_id"`
	DeliveryRunID       string           `json:"delivery_run_id"`
	RunID               string           `json:"run_id"`
	TaskID              string           `json:"task_id"`
	AttemptID           string           `json:"attempt_id"`
	AttemptOrdinal      int              `json:"attempt_ordinal"`
	CorrelationID       string           `json:"correlation_id"`
	CorrelationSequence int64            `json:"correlation_sequence"`
	Phase               string           `json:"phase"`
	Status              string           `json:"status"`
	TaskCounts          TaskCounts       `json:"task_counts"`
	Provider            ProviderIdentity `json:"provider"`
	Heartbeat           AgeEvidence      `json:"heartbeat"`
	Progress            AgeEvidence      `json:"progress"`
	Evidence            []EvidenceRef    `json:"evidence"`
	QuotaBudget         QuotaBudgetState `json:"quota_budget"`
	Blocker             ActionState      `json:"blocker"`
	NextAction          ActionState      `json:"next_action"`
	Redaction           RedactionState   `json:"redaction"`
	GapReasons          []string         `json:"gap_reasons"`
	OccurredAt          string           `json:"occurred_at"`
	PersistedAt         string           `json:"persisted_at"`
	SemanticFingerprint string           `json:"semantic_fingerprint"`
}

type TaskCounts struct {
	Total     int `json:"total"`
	Ready     int `json:"ready"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Blocked   int `json:"blocked"`
	Unknown   int `json:"unknown"`
}

type ProviderIdentity struct {
	ProviderID           string `json:"provider_id"`
	ModelID              string `json:"model_id"`
	AccountProfileID     string `json:"account_profile_id"`
	ModelCapabilityID    string `json:"model_capability_id"`
	ProviderConfidence   string `json:"provider_confidence"`
	ProviderInstallation string `json:"provider_installation_id"`
}

type AgeEvidence struct {
	State      string `json:"state"`
	ObservedAt string `json:"observed_at"`
	AgeMillis  int64  `json:"age_millis"`
}

type EvidenceRef struct {
	RecordKind     string `json:"record_kind"`
	RecordID       string `json:"record_id"`
	Summary        string `json:"summary"`
	Classification string `json:"classification"`
	Confidence     string `json:"confidence"`
}

type QuotaBudgetState struct {
	State               string   `json:"state"`
	Confidence          string   `json:"confidence"`
	BudgetPolicyID      string   `json:"budget_policy_id"`
	BudgetReservationID string   `json:"budget_reservation_id"`
	RemainingQuantity   int64    `json:"remaining_quantity"`
	Unit                string   `json:"unit"`
	GapReasons          []string `json:"gap_reasons"`
}

type ActionState struct {
	State   string `json:"state"`
	Summary string `json:"summary"`
}

type RedactionState struct {
	Redacted           bool     `json:"redacted"`
	Truncated          bool     `json:"truncated"`
	Markers            []string `json:"markers"`
	SecretFindingCount int      `json:"secret_finding_count"`
	OriginalBytes      int      `json:"original_bytes"`
	PersistedBytes     int      `json:"persisted_bytes"`
}

type WriteResult struct {
	Receipt      ProgressReceipt
	Inserted     bool
	StorageOrder int64
}

type ListFilter struct {
	ProjectID     string
	DeliveryRunID string
	CorrelationID string
	TaskID        string
	Status        string
	Limit         int
}

type ReplayFilter struct {
	ProjectID     string
	DeliveryRunID string
	Limit         int
}

func WriteReceipt(ctx context.Context, store storage.Store, receipt ProgressReceipt) (WriteResult, error) {
	return PersistReceipt(ctx, store, receipt)
}

func PersistReceipt(ctx context.Context, store storage.Store, receipt ProgressReceipt) (WriteResult, error) {
	return persistReceipt(ctx, store, receipt, false)
}

// PersistReceiptNextSequence assigns the next correlation_sequence atomically
// with receipt insertion. It is for supervisor-owned emitters that may overlap
// during restart or detached supervision; explicit replay payloads should keep
// using PersistReceipt with their recorded sequence.
func PersistReceiptNextSequence(ctx context.Context, store storage.Store, receipt ProgressReceipt) (WriteResult, error) {
	return persistReceipt(ctx, store, receipt, true)
}

func persistReceipt(ctx context.Context, store storage.Store, receipt ProgressReceipt, nextSequence bool) (WriteResult, error) {
	if store == nil {
		return WriteResult{}, typed(ErrInvalidRecordCode, "store is required")
	}
	now := store.Now()
	normalized, err := NormalizeReceipt(receipt, now)
	if err != nil {
		return WriteResult{}, err
	}
	var result WriteResult
	err = store.WithWriteTx(ctx, func(tx storage.Tx) error {
		var err error
		result, err = persistNormalizedReceiptTx(ctx, tx, normalized, now, nextSequence)
		return err
	})
	return result, err
}

func persistNormalizedReceiptTx(ctx context.Context, tx storage.Tx, normalized ProgressReceipt, now time.Time, nextSequence bool) (WriteResult, error) {
	if err := ensureProject(ctx, tx, normalized.ProjectID); err != nil {
		return WriteResult{}, err
	}
	if nextSequence {
		var sequence int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(correlation_sequence), 0) + 1
			FROM progress_receipts
			WHERE project_id = ? AND delivery_run_id = ? AND correlation_id = ?`,
			normalized.ProjectID, normalized.DeliveryRunID, normalized.CorrelationID).Scan(&sequence); err != nil {
			return WriteResult{}, fmt.Errorf("allocate progress receipt sequence: %w", err)
		}
		normalized.CorrelationSequence = sequence
		normalized.ProgressReceiptID = ""
		normalized.SemanticFingerprint = ""
		var err error
		normalized, err = NormalizeReceipt(normalized, now)
		if err != nil {
			return WriteResult{}, err
		}
	}
	payload, err := canonicalJSON(normalized)
	if err != nil {
		return WriteResult{}, typed(ErrInvalidRecordCode, "canonical progress receipt: %v", err)
	}
	taskCounts, err := canonicalJSON(normalized.TaskCounts)
	if err != nil {
		return WriteResult{}, err
	}
	provider, err := canonicalJSON(normalized.Provider)
	if err != nil {
		return WriteResult{}, err
	}
	heartbeat, err := canonicalJSON(normalized.Heartbeat)
	if err != nil {
		return WriteResult{}, err
	}
	progressAge, err := canonicalJSON(normalized.Progress)
	if err != nil {
		return WriteResult{}, err
	}
	evidence, err := canonicalJSON(normalized.Evidence)
	if err != nil {
		return WriteResult{}, err
	}
	quotaBudget, err := canonicalJSON(normalized.QuotaBudget)
	if err != nil {
		return WriteResult{}, err
	}
	blocker, err := canonicalJSON(normalized.Blocker)
	if err != nil {
		return WriteResult{}, err
	}
	nextAction, err := canonicalJSON(normalized.NextAction)
	if err != nil {
		return WriteResult{}, err
	}
	redaction, err := canonicalJSON(normalized.Redaction)
	if err != nil {
		return WriteResult{}, err
	}
	gaps, err := canonicalJSON(normalized.GapReasons)
	if err != nil {
		return WriteResult{}, err
	}
	insert, err := tx.Exec(ctx, `INSERT OR IGNORE INTO progress_receipts(
			progress_receipt_id, schema_version, record_version, project_id, delivery_run_id, run_id, task_id,
			attempt_id, attempt_ordinal, correlation_id, correlation_sequence, semantic_fingerprint, phase, status,
			provider_id, model_id, heartbeat_age_millis, progress_age_millis, occurred_at, persisted_at,
			task_counts_json, provider_json, heartbeat_json, progress_json, evidence_json, quota_budget_json,
			blocker_json, next_action_json, redaction_json, gap_reasons_json, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		normalized.ProgressReceiptID, normalized.SchemaVersion, normalized.RecordVersion, normalized.ProjectID, normalized.DeliveryRunID,
		normalized.RunID, normalized.TaskID, normalized.AttemptID, normalized.AttemptOrdinal, normalized.CorrelationID,
		normalized.CorrelationSequence, normalized.SemanticFingerprint, normalized.Phase, normalized.Status,
		normalized.Provider.ProviderID, normalized.Provider.ModelID, normalized.Heartbeat.AgeMillis, normalized.Progress.AgeMillis,
		normalized.OccurredAt, normalized.PersistedAt, string(taskCounts), string(provider), string(heartbeat), string(progressAge),
		string(evidence), string(quotaBudget), string(blocker), string(nextAction), string(redaction), string(gaps), string(payload))
	if err != nil {
		return WriteResult{}, fmt.Errorf("persist progress receipt: %w", err)
	}
	rows, err := insert.RowsAffected()
	if err != nil {
		return WriteResult{}, fmt.Errorf("persist progress receipt: rows affected: %w", err)
	}
	stored, order, err := receiptBySemantic(ctx, tx, normalized.ProjectID, normalized.DeliveryRunID, normalized.CorrelationID, normalized.SemanticFingerprint)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WriteResult{}, typed(ErrDuplicateReplayCode, "progress_receipt_id %q already exists for different semantic content", normalized.ProgressReceiptID)
		}
		return WriteResult{}, err
	}
	return WriteResult{Receipt: stored, Inserted: rows == 1, StorageOrder: order}, nil
}

func LoadReceipt(ctx context.Context, store storage.Store, progressReceiptID string) (ProgressReceipt, error) {
	if store == nil {
		return ProgressReceipt{}, typed(ErrInvalidRecordCode, "store is required")
	}
	progressReceiptID = strings.TrimSpace(progressReceiptID)
	if progressReceiptID == "" {
		return ProgressReceipt{}, typed(ErrInvalidRecordCode, "progress_receipt_id is required")
	}
	var receipt ProgressReceipt
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		row := tx.QueryRow(ctx, `SELECT payload_json FROM progress_receipts WHERE progress_receipt_id = ?`, progressReceiptID)
		return scanPayload(row, &receipt)
	})
	return receipt, err
}

func ListReceipts(ctx context.Context, store storage.Store, filter ListFilter) ([]ProgressReceipt, error) {
	if store == nil {
		return nil, typed(ErrInvalidRecordCode, "store is required")
	}
	query := `SELECT payload_json FROM progress_receipts`
	var clauses []string
	var args []any
	add := func(condition string, value any) {
		clauses = append(clauses, condition)
		args = append(args, value)
	}
	if strings.TrimSpace(filter.ProjectID) != "" {
		add(`project_id = ?`, strings.TrimSpace(filter.ProjectID))
	}
	if strings.TrimSpace(filter.DeliveryRunID) != "" {
		add(`delivery_run_id = ?`, strings.TrimSpace(filter.DeliveryRunID))
	}
	if strings.TrimSpace(filter.CorrelationID) != "" {
		add(`correlation_id = ?`, strings.TrimSpace(filter.CorrelationID))
	}
	if strings.TrimSpace(filter.TaskID) != "" {
		add(`task_id = ?`, strings.TrimSpace(filter.TaskID))
	}
	if strings.TrimSpace(filter.Status) != "" {
		add(`status = ?`, strings.TrimSpace(filter.Status))
	}
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	query += ` ORDER BY occurred_at, correlation_id, correlation_sequence, storage_order`
	limit := boundedLimit(filter.Limit)
	query += ` LIMIT ?`
	args = append(args, limit)
	var out []ProgressReceipt
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("list progress receipts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				return fmt.Errorf("list progress receipts: scan: %w", err)
			}
			receipt, err := decodePersistedPayload([]byte(payload))
			if err != nil {
				return err
			}
			out = append(out, receipt)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list progress receipts: rows: %w", err)
		}
		return nil
	})
	return out, err
}

func Replay(ctx context.Context, store storage.Store, filter ReplayFilter) ([]ProgressReceipt, error) {
	if strings.TrimSpace(filter.ProjectID) == "" || strings.TrimSpace(filter.DeliveryRunID) == "" {
		return nil, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required for replay")
	}
	return ListReceipts(ctx, store, ListFilter{ProjectID: filter.ProjectID, DeliveryRunID: filter.DeliveryRunID, Limit: filter.Limit})
}

func NormalizeReceipt(receipt ProgressReceipt, now time.Time) (ProgressReceipt, error) {
	nowText := canonicalTimestamp(now)
	receipt.SchemaVersion = firstNonEmpty(receipt.SchemaVersion, SchemaProgressReceipt)
	if receipt.SchemaVersion != SchemaProgressReceipt {
		return ProgressReceipt{}, typed(ErrUnknownRecordVersionCode, "unsupported progress receipt schema %q", receipt.SchemaVersion)
	}
	if receipt.RecordVersion == 0 {
		receipt.RecordVersion = 1
	}
	receipt.ProjectID = cleanRequiredID("project_id", receipt.ProjectID)
	receipt.DeliveryRunID = cleanRequiredID("delivery_run_id", receipt.DeliveryRunID)
	receipt.RunID = sanitizeID(firstNonEmpty(receipt.RunID, receipt.DeliveryRunID))
	receipt.TaskID = sanitizeID(firstNonEmpty(receipt.TaskID, Unknown))
	receipt.AttemptID = sanitizeID(firstNonEmpty(receipt.AttemptID, Unknown))
	receipt.CorrelationID = sanitizeID(firstNonEmpty(receipt.CorrelationID, receipt.AttemptID))
	receipt.Phase = sanitizeEnum(firstNonEmpty(receipt.Phase, Unknown))
	receipt.Status = sanitizeEnum(firstNonEmpty(receipt.Status, Unknown))
	receipt.Provider = normalizeProvider(receipt.Provider)
	receipt.TaskCounts = normalizeTaskCounts(receipt.TaskCounts)
	receipt.Heartbeat = normalizeAge(receipt.Heartbeat, now)
	receipt.Progress = normalizeAge(receipt.Progress, now)
	receipt.Evidence = normalizeEvidence(receipt.Evidence, &receipt.Redaction)
	receipt.QuotaBudget = normalizeQuotaBudget(receipt.QuotaBudget, &receipt.Redaction)
	receipt.Blocker = normalizeAction(receipt.Blocker, &receipt.Redaction)
	receipt.NextAction = normalizeAction(receipt.NextAction, &receipt.Redaction)
	receipt.GapReasons = sanitizeStringSlice(receipt.GapReasons, maxIdentifierRunes, &receipt.Redaction)
	receipt.Redaction.Markers = normalizeMarkers(receipt.Redaction.Markers)
	receipt.OccurredAt = normalizeTimestampOr(receipt.OccurredAt, nowText)
	receipt.PersistedAt = normalizeTimestampOr(receipt.PersistedAt, nowText)
	semantic, err := semanticFingerprint(receipt)
	if err != nil {
		return ProgressReceipt{}, err
	}
	receipt.SemanticFingerprint = semantic
	if strings.TrimSpace(receipt.ProgressReceiptID) == "" {
		receipt.ProgressReceiptID = receiptID(semantic)
	} else {
		receipt.ProgressReceiptID = sanitizeID(receipt.ProgressReceiptID)
	}
	payload, err := canonicalJSON(receipt)
	if err != nil {
		return ProgressReceipt{}, typed(ErrInvalidRecordCode, "canonical progress receipt: %v", err)
	}
	receipt.Redaction.PersistedBytes = len(payload)
	if err := validateReceipt(receipt); err != nil {
		return ProgressReceipt{}, err
	}
	return receipt, nil
}

func DecodeReceiptJSON(data []byte, now time.Time) (ProgressReceipt, error) {
	var receipt ProgressReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return ProgressReceipt{}, typed(ErrInvalidRecordCode, "decode progress receipt: %v", err)
	}
	return NormalizeReceipt(receipt, now)
}

func semanticFingerprint(receipt ProgressReceipt) (string, error) {
	canonical := map[string]any{
		"schema_version":       receipt.SchemaVersion,
		"project_id":           receipt.ProjectID,
		"delivery_run_id":      receipt.DeliveryRunID,
		"run_id":               receipt.RunID,
		"task_id":              receipt.TaskID,
		"attempt_id":           receipt.AttemptID,
		"attempt_ordinal":      receipt.AttemptOrdinal,
		"correlation_id":       receipt.CorrelationID,
		"correlation_sequence": receipt.CorrelationSequence,
		"phase":                receipt.Phase,
		"status":               receipt.Status,
		"task_counts":          receipt.TaskCounts,
		"provider":             receipt.Provider,
		"heartbeat":            receipt.Heartbeat,
		"progress":             receipt.Progress,
		"evidence":             receipt.Evidence,
		"quota_budget":         receipt.QuotaBudget,
		"blocker":              receipt.Blocker,
		"next_action":          receipt.NextAction,
		"gap_reasons":          receipt.GapReasons,
		"redaction_markers":    receipt.Redaction.Markers,
		"redacted":             receipt.Redaction.Redacted,
		"truncated":            receipt.Redaction.Truncated,
		"secret_finding_count": receipt.Redaction.SecretFindingCount,
	}
	digest, _, err := digestCanonicalJSON(canonical)
	if err != nil {
		return "", typed(ErrInvalidRecordCode, "semantic progress receipt: %v", err)
	}
	return digest, nil
}

func validateReceipt(receipt ProgressReceipt) error {
	if receipt.RecordVersion != 1 {
		return typed(ErrUnknownRecordVersionCode, "unsupported progress receipt record version %d", receipt.RecordVersion)
	}
	for label, value := range map[string]string{
		"progress_receipt_id":  receipt.ProgressReceiptID,
		"project_id":           receipt.ProjectID,
		"delivery_run_id":      receipt.DeliveryRunID,
		"run_id":               receipt.RunID,
		"correlation_id":       receipt.CorrelationID,
		"phase":                receipt.Phase,
		"status":               receipt.Status,
		"semantic_fingerprint": receipt.SemanticFingerprint,
		"occurred_at":          receipt.OccurredAt,
		"persisted_at":         receipt.PersistedAt,
	} {
		if strings.TrimSpace(value) == "" {
			return typed(ErrInvalidRecordCode, "%s is required", label)
		}
		if !utf8.ValidString(value) {
			return typed(ErrInvalidRecordCode, "%s must be valid UTF-8", label)
		}
	}
	if receipt.CorrelationSequence < 0 || receipt.AttemptOrdinal < 0 {
		return typed(ErrInvalidRecordCode, "correlation_sequence and attempt_ordinal must be non-negative")
	}
	if err := validateCounts(receipt.TaskCounts); err != nil {
		return err
	}
	if receipt.Heartbeat.AgeMillis < -1 || receipt.Progress.AgeMillis < -1 {
		return typed(ErrInvalidRecordCode, "heartbeat and progress ages must be -1 or greater")
	}
	for _, ts := range []string{receipt.OccurredAt, receipt.PersistedAt, receipt.Heartbeat.ObservedAt, receipt.Progress.ObservedAt} {
		if strings.TrimSpace(ts) == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
			return typed(ErrInvalidRecordCode, "invalid timestamp %q", ts)
		}
	}
	return nil
}

func validateCounts(counts TaskCounts) error {
	for label, value := range map[string]int{
		"total": counts.Total, "ready": counts.Ready, "running": counts.Running,
		"succeeded": counts.Succeeded, "failed": counts.Failed, "blocked": counts.Blocked, "unknown": counts.Unknown,
	} {
		if value < -1 {
			return typed(ErrInvalidRecordCode, "task_counts.%s must be -1 or greater", label)
		}
	}
	return nil
}

func normalizeProvider(provider ProviderIdentity) ProviderIdentity {
	provider.ProviderID = sanitizeID(firstNonEmpty(provider.ProviderID, Unknown))
	provider.ModelID = sanitizeID(firstNonEmpty(provider.ModelID, Unknown))
	provider.AccountProfileID = sanitizeID(firstNonEmpty(provider.AccountProfileID, Unknown))
	provider.ModelCapabilityID = sanitizeID(firstNonEmpty(provider.ModelCapabilityID, Unknown))
	provider.ProviderConfidence = sanitizeEnum(firstNonEmpty(provider.ProviderConfidence, Unknown))
	provider.ProviderInstallation = sanitizeID(firstNonEmpty(provider.ProviderInstallation, Unknown))
	return provider
}

func normalizeTaskCounts(counts TaskCounts) TaskCounts {
	if counts.Total == 0 && counts.Ready == 0 && counts.Running == 0 && counts.Succeeded == 0 && counts.Failed == 0 && counts.Blocked == 0 && counts.Unknown == 0 {
		return TaskCounts{Total: -1, Ready: -1, Running: -1, Succeeded: -1, Failed: -1, Blocked: -1, Unknown: -1}
	}
	return counts
}

func normalizeAge(age AgeEvidence, now time.Time) AgeEvidence {
	age.State = sanitizeEnum(firstNonEmpty(age.State, Unknown))
	age.ObservedAt = normalizeTimestampOr(age.ObservedAt, "")
	if age.AgeMillis == 0 && age.State == Unknown && age.ObservedAt == "" {
		age.AgeMillis = -1
	}
	if age.AgeMillis < 0 && age.ObservedAt != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, age.ObservedAt); err == nil {
			age.AgeMillis = now.UTC().Sub(parsed.UTC()).Milliseconds()
			if age.AgeMillis < 0 {
				age.AgeMillis = 0
			}
		}
	}
	if age.AgeMillis < 0 {
		age.AgeMillis = -1
	}
	return age
}

func normalizeEvidence(values []EvidenceRef, redaction *RedactionState) []EvidenceRef {
	if len(values) == 0 {
		return []EvidenceRef{}
	}
	out := make([]EvidenceRef, 0, len(values))
	for _, value := range values {
		value.RecordKind = sanitizeID(firstNonEmpty(value.RecordKind, Unknown))
		value.RecordID = sanitizeID(firstNonEmpty(value.RecordID, Unknown))
		value.Summary = sanitizeText(value.Summary, maxTextRunes, redaction)
		value.Classification = sanitizeEnum(firstNonEmpty(value.Classification, "local-diagnostic"))
		value.Confidence = sanitizeEnum(firstNonEmpty(value.Confidence, Unknown))
		out = append(out, value)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].RecordKind != out[j].RecordKind {
			return out[i].RecordKind < out[j].RecordKind
		}
		if out[i].RecordID != out[j].RecordID {
			return out[i].RecordID < out[j].RecordID
		}
		return out[i].Summary < out[j].Summary
	})
	return out
}

func normalizeQuotaBudget(value QuotaBudgetState, redaction *RedactionState) QuotaBudgetState {
	value.State = sanitizeEnum(firstNonEmpty(value.State, Unknown))
	value.Confidence = sanitizeEnum(firstNonEmpty(value.Confidence, Unknown))
	value.BudgetPolicyID = sanitizeID(firstNonEmpty(value.BudgetPolicyID, Unknown))
	value.BudgetReservationID = sanitizeID(firstNonEmpty(value.BudgetReservationID, Unknown))
	value.Unit = sanitizeID(firstNonEmpty(value.Unit, Unknown))
	if value.RemainingQuantity == 0 && value.Confidence == Unknown {
		value.RemainingQuantity = -1
	}
	value.GapReasons = sanitizeStringSlice(value.GapReasons, maxIdentifierRunes, redaction)
	return value
}

func normalizeAction(value ActionState, redaction *RedactionState) ActionState {
	value.State = sanitizeEnum(firstNonEmpty(value.State, Unknown))
	value.Summary = sanitizeText(value.Summary, maxTextRunes, redaction)
	return value
}

func normalizeMarkers(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	values = sanitizeStringSlice(values, maxIdentifierRunes, nil)
	sort.Strings(values)
	out := values[:0]
	var last string
	for _, value := range values {
		if value == "" || value == last {
			continue
		}
		out = append(out, value)
		last = value
	}
	return out
}

func cleanRequiredID(label, value string) string {
	_ = label
	return strings.TrimSpace(value)
}

func sanitizeID(value string) string {
	return sanitizeText(value, maxIdentifierRunes, nil)
}

func sanitizeEnum(value string) string {
	value = sanitizeID(value)
	value = strings.ToLower(value)
	if value == "" {
		return Unknown
	}
	return value
}

func sanitizeStringSlice(values []string, max int, redaction *RedactionState) []string {
	if len(values) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = sanitizeText(value, max, redaction)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sanitizeText(value string, maxRunes int, redaction *RedactionState) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
	if redaction != nil {
		redaction.OriginalBytes += len(value)
	}
	findings := 0
	for _, pattern := range secretPatterns {
		matches := pattern.FindAllStringIndex(value, -1)
		if len(matches) == 0 {
			continue
		}
		findings += len(matches)
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	value = genericSecretPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := genericSecretPattern.FindStringSubmatch(match)
		if len(parts) < 3 || !looksOpaque(parts[2]) {
			return match
		}
		findings++
		return parts[1] + "[REDACTED]"
	})
	truncated := false
	runes := []rune(value)
	if maxRunes > 0 && len(runes) > maxRunes {
		value = string(runes[:maxRunes])
		truncated = true
	}
	if redaction != nil {
		if findings > 0 {
			redaction.Redacted = true
			redaction.SecretFindingCount += findings
			redaction.Markers = append(redaction.Markers, "secret-redacted")
		}
		if truncated {
			redaction.Truncated = true
			redaction.Markers = append(redaction.Markers, "text-truncated")
		}
	}
	return value
}

func DiagnosticMessage(value string) string {
	return sanitizeText(value, maxTextRunes, nil)
}

func looksOpaque(value string) bool {
	value = strings.Trim(value, `"'`)
	if len(value) < 12 {
		return false
	}
	hasDigit := false
	hasPunct := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case strings.ContainsRune("._~+/=_-", r):
			hasPunct = true
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		default:
			return false
		}
	}
	return hasDigit || hasPunct
}

func normalizeTimestampOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return canonicalTimestamp(parsed)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func receiptID(semantic string) string {
	return "prec_" + strings.TrimPrefix(semantic, "sha256:")[:40]
}

func boundedLimit(limit int) int {
	if limit <= 0 {
		return 500
	}
	if limit > 5000 {
		return 5000
	}
	return limit
}

func ensureProject(ctx context.Context, tx storage.Tx, projectID string) error {
	var found int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&found); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrMissingReferenceCode, "project %q does not exist", projectID)
		}
		return fmt.Errorf("progress receipt project lookup: %w", err)
	}
	return nil
}

func receiptBySemantic(ctx context.Context, tx storage.Tx, projectID, deliveryRunID, correlationID, semantic string) (ProgressReceipt, int64, error) {
	row := tx.QueryRow(ctx, `SELECT storage_order, payload_json FROM progress_receipts
		WHERE project_id = ? AND delivery_run_id = ? AND correlation_id = ? AND semantic_fingerprint = ?`,
		projectID, deliveryRunID, correlationID, semantic)
	var order int64
	var payload string
	if err := row.Scan(&order, &payload); err != nil {
		return ProgressReceipt{}, 0, err
	}
	receipt, err := decodePersistedPayload([]byte(payload))
	return receipt, order, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPayload(row rowScanner, receipt *ProgressReceipt) error {
	var payload string
	if err := row.Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return typed(ErrMissingReferenceCode, "progress receipt not found")
		}
		return fmt.Errorf("load progress receipt: %w", err)
	}
	decoded, err := decodePersistedPayload([]byte(payload))
	if err != nil {
		return err
	}
	*receipt = decoded
	return nil
}

func decodePersistedPayload(data []byte) (ProgressReceipt, error) {
	var header struct {
		SchemaVersion string `json:"schema_version"`
		RecordVersion int    `json:"record_version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return ProgressReceipt{}, typed(ErrInvalidRecordCode, "decode persisted progress receipt header: %v", err)
	}
	if header.SchemaVersion != SchemaProgressReceipt || header.RecordVersion != 1 {
		return ProgressReceipt{}, typed(ErrUnknownRecordVersionCode, "unsupported progress receipt %q record version %d", header.SchemaVersion, header.RecordVersion)
	}
	var receipt ProgressReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return ProgressReceipt{}, typed(ErrInvalidRecordCode, "decode persisted progress receipt: %v", err)
	}
	if err := validateReceipt(receipt); err != nil {
		return ProgressReceipt{}, err
	}
	return receipt, nil
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[A-Z0-9]{16}`),
	regexp.MustCompile(`(?i)(github_pat|gh[pousr]_|sk-ant-|sk-|xox[baprs]-)[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`(?i)(cookie|set-cookie):\s*[^;\n]+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`[A-Za-z]:\\[^\s"']{8,}`),
	regexp.MustCompile(`/[A-Za-z0-9._@%+=:,/-]{12,}`),
}

var genericSecretPattern = regexp.MustCompile(`(?i)\b((?:token|secret|password|api[_-]?key|credential)\s*[=:]\s*["']?)([A-Za-z0-9._~+/=_-]{12,})["']?`)
