package progress

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	DefaultFollowPollInterval = 250 * time.Millisecond
	MinFollowPollInterval     = 50 * time.Millisecond
	MaxFollowPollInterval     = 5 * time.Second
)

type Cursor string

type ReadFilter struct {
	ProjectID     string
	DeliveryRunID string
	CorrelationID string
	TaskID        string
	Limit         int
	After         Cursor
}

type ReceiptView struct {
	Cursor          Cursor            `json:"cursor"`
	StorageOrder    int64             `json:"storage_order,omitempty"`
	Receipt         ProgressReceipt   `json:"receipt"`
	ReceiptAge      ClockView         `json:"receipt_age"`
	HeartbeatAge    ClockView         `json:"process_heartbeat_age"`
	ProgressAge     ClockView         `json:"meaningful_progress_age"`
	Provider        ProviderView      `json:"provider"`
	QuotaBudget     QuotaBudgetView   `json:"quota_budget"`
	Blocker         ActionState       `json:"blocker"`
	NextAction      ActionState       `json:"next_action"`
	DeliveryState   DeliveryStateView `json:"delivery_state"`
	RenderAuthority string            `json:"render_authority"`
}

type ClockView struct {
	State       string `json:"state"`
	ObservedAt  string `json:"observed_at,omitempty"`
	AgeMillis   int64  `json:"age_millis"`
	Authority   string `json:"authority"`
	DisplayText string `json:"display_text"`
}

type ProviderView struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
	Confidence string `json:"confidence"`
}

type QuotaBudgetView struct {
	State             string   `json:"state"`
	Confidence        string   `json:"confidence"`
	RemainingQuantity int64    `json:"remaining_quantity"`
	Unit              string   `json:"unit"`
	GapReasons        []string `json:"gap_reasons,omitempty"`
}

type DeliveryStateView struct {
	State     string `json:"state"`
	Authority string `json:"authority"`
	Reason    string `json:"reason"`
}

type ReadDiagnostic struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	StorageOrder int64  `json:"storage_order,omitempty"`
}

type ReceiptBatch struct {
	Views       []ReceiptView    `json:"receipts"`
	NextCursor  Cursor           `json:"next_cursor,omitempty"`
	Diagnostics []ReadDiagnostic `json:"diagnostics,omitempty"`
}

type FollowOptions struct {
	ReadFilter
	PollInterval time.Duration
}

type receiptRow struct {
	storageOrder        int64
	occurredAt          string
	correlationID       string
	correlationSequence int64
	payload             string
}

type cursorPayload struct {
	Version             int    `json:"v"`
	ProjectID           string `json:"project_id,omitempty"`
	DeliveryRunID       string `json:"delivery_run_id,omitempty"`
	CorrelationIDFilter string `json:"correlation_id_filter,omitempty"`
	TaskIDFilter        string `json:"task_id_filter,omitempty"`
	OccurredAt          string `json:"occurred_at"`
	CorrelationID       string `json:"correlation_id"`
	CorrelationSequence int64  `json:"correlation_sequence"`
	StorageOrder        int64  `json:"storage_order"`
}

func ReadReceipts(ctx context.Context, store storage.Store, filter ReadFilter, now time.Time) (ReceiptBatch, error) {
	if store == nil {
		return ReceiptBatch{}, typed(ErrInvalidRecordCode, "store is required")
	}
	projectID := strings.TrimSpace(filter.ProjectID)
	deliveryRunID := strings.TrimSpace(filter.DeliveryRunID)
	if projectID == "" || deliveryRunID == "" {
		return ReceiptBatch{}, typed(ErrInvalidRecordCode, "project_id and delivery_run_id are required")
	}
	after, err := decodeCursor(filter.After)
	if err != nil {
		return ReceiptBatch{}, err
	}
	if err := validateCursorScope(after, filter); err != nil {
		return ReceiptBatch{}, err
	}
	rows, err := queryReceiptRows(ctx, store, filter, after)
	if err != nil {
		return ReceiptBatch{}, err
	}
	batch := ReceiptBatch{}
	for _, row := range rows {
		cursor := encodeCursor(cursorPayload{
			Version:             1,
			ProjectID:           projectID,
			DeliveryRunID:       deliveryRunID,
			CorrelationIDFilter: strings.TrimSpace(filter.CorrelationID),
			TaskIDFilter:        strings.TrimSpace(filter.TaskID),
			OccurredAt:          row.occurredAt,
			CorrelationID:       row.correlationID,
			CorrelationSequence: row.correlationSequence,
			StorageOrder:        row.storageOrder,
		})
		batch.NextCursor = cursor
		receipt, err := decodePersistedPayload([]byte(row.payload))
		if err != nil {
			batch.Diagnostics = append(batch.Diagnostics, ReadDiagnostic{
				Code:         "progress-receipt-skipped",
				Message:      boundedDiagnostic(err.Error()),
				StorageOrder: row.storageOrder,
			})
			continue
		}
		view := ViewReceipt(receipt, row.storageOrder, cursor, now)
		state, err := deliveryStateForReceipt(ctx, store, receipt.ProgressReceiptID)
		if err != nil {
			batch.Diagnostics = append(batch.Diagnostics, ReadDiagnostic{
				Code:         "progress-delivery-state-skipped",
				Message:      boundedDiagnostic(err.Error()),
				StorageOrder: row.storageOrder,
			})
		} else {
			view.DeliveryState = state
		}
		batch.Views = append(batch.Views, view)
	}
	return batch, nil
}

func FollowReceipts(ctx context.Context, store storage.Store, opts FollowOptions, now func() time.Time, emit func(ReceiptBatch) error) error {
	if store == nil {
		return typed(ErrInvalidRecordCode, "store is required")
	}
	if emit == nil {
		return typed(ErrInvalidRecordCode, "emit callback is required")
	}
	if now == nil {
		return typed(ErrInvalidRecordCode, "clock is required")
	}
	interval := boundFollowPollInterval(opts.PollInterval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	filter := opts.ReadFilter
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		batch, err := ReadReceipts(ctx, store, filter, now().UTC())
		if err != nil {
			return err
		}
		if len(batch.Views) > 0 || len(batch.Diagnostics) > 0 {
			if err := emit(batch); err != nil {
				return err
			}
			if batch.NextCursor != "" {
				filter.After = batch.NextCursor
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func ViewReceipt(receipt ProgressReceipt, storageOrder int64, cursor Cursor, now time.Time) ReceiptView {
	now = now.UTC()
	return ReceiptView{
		Cursor:          cursor,
		StorageOrder:    storageOrder,
		Receipt:         receipt,
		ReceiptAge:      receiptClock(receipt.OccurredAt, now),
		HeartbeatAge:    evidenceClock(receipt.Heartbeat, "progress_receipt.heartbeat"),
		ProgressAge:     evidenceClock(receipt.Progress, "progress_receipt.progress"),
		Provider:        ProviderView{ProviderID: receipt.Provider.ProviderID, ModelID: receipt.Provider.ModelID, Confidence: receipt.Provider.ProviderConfidence},
		QuotaBudget:     quotaBudgetView(receipt.QuotaBudget),
		Blocker:         receipt.Blocker,
		NextAction:      receipt.NextAction,
		DeliveryState:   unsupportedDeliveryState(),
		RenderAuthority: "attached-consumer-write-only",
	}
}

func RenderHuman(w io.Writer, views []ReceiptView) error {
	for _, view := range views {
		receipt := view.Receipt
		if _, err := fmt.Fprintf(w, "progress receipt %s\n", displayValue(receipt.ProgressReceiptID)); err != nil {
			return err
		}
		lines := []struct {
			label string
			value string
		}{
			{"run", fmt.Sprintf("%s correlation=%s sequence=%d", displayValue(receipt.DeliveryRunID), displayValue(receipt.CorrelationID), receipt.CorrelationSequence)},
			{"state", fmt.Sprintf("phase=%s status=%s task=%s attempt=%s", displayValue(receipt.Phase), displayValue(receipt.Status), displayValue(receipt.TaskID), displayValue(receipt.AttemptID))},
			{"receipt_age", fmt.Sprintf("%s authority=%s", view.ReceiptAge.DisplayText, view.ReceiptAge.Authority)},
			{"meaningful_progress_age", fmt.Sprintf("%s state=%s authority=%s", view.ProgressAge.DisplayText, displayValue(view.ProgressAge.State), view.ProgressAge.Authority)},
			{"process_heartbeat_age", fmt.Sprintf("%s state=%s authority=%s", view.HeartbeatAge.DisplayText, displayValue(view.HeartbeatAge.State), view.HeartbeatAge.Authority)},
			{"provider_model", fmt.Sprintf("%s/%s confidence=%s", displayValue(view.Provider.ProviderID), displayValue(view.Provider.ModelID), displayValue(view.Provider.Confidence))},
			{"quota_budget", fmt.Sprintf("state=%s remaining=%s confidence=%s gaps=%s", displayValue(view.QuotaBudget.State), quantityText(view.QuotaBudget.RemainingQuantity, view.QuotaBudget.Unit), displayValue(view.QuotaBudget.Confidence), displaySlice(view.QuotaBudget.GapReasons))},
			{"blocker", actionText(view.Blocker)},
			{"next_action", actionText(view.NextAction)},
			{"delivery_state", fmt.Sprintf("%s authority=%s reason=%s", view.DeliveryState.State, view.DeliveryState.Authority, view.DeliveryState.Reason)},
			{"render_authority", view.RenderAuthority},
			{"cursor", string(view.Cursor)},
		}
		for _, line := range lines {
			if _, err := fmt.Fprintf(w, "  %s: %s\n", line.label, line.value); err != nil {
				return err
			}
		}
	}
	return nil
}

func RenderJSON(w io.Writer, batch ReceiptBatch) error {
	batch.Diagnostics = nil
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(batch)
}

func RenderJSONL(w io.Writer, views []ReceiptView) error {
	encoder := json.NewEncoder(w)
	for _, view := range views {
		if err := encoder.Encode(view); err != nil {
			return err
		}
	}
	return nil
}

func queryReceiptRows(ctx context.Context, store storage.Store, filter ReadFilter, after cursorPayload) ([]receiptRow, error) {
	query := `SELECT storage_order, occurred_at, correlation_id, correlation_sequence, payload_json FROM progress_receipts`
	var clauses []string
	var args []any
	add := func(condition string, value ...any) {
		clauses = append(clauses, condition)
		args = append(args, value...)
	}
	add(`project_id = ?`, strings.TrimSpace(filter.ProjectID))
	add(`delivery_run_id = ?`, strings.TrimSpace(filter.DeliveryRunID))
	if strings.TrimSpace(filter.CorrelationID) != "" {
		add(`correlation_id = ?`, strings.TrimSpace(filter.CorrelationID))
	}
	if strings.TrimSpace(filter.TaskID) != "" {
		add(`task_id = ?`, strings.TrimSpace(filter.TaskID))
	}
	if after.Version != 0 {
		add(`(occurred_at > ? OR (occurred_at = ? AND correlation_id > ?) OR (occurred_at = ? AND correlation_id = ? AND correlation_sequence > ?) OR (occurred_at = ? AND correlation_id = ? AND correlation_sequence = ? AND storage_order > ?))`,
			after.OccurredAt,
			after.OccurredAt, after.CorrelationID,
			after.OccurredAt, after.CorrelationID, after.CorrelationSequence,
			after.OccurredAt, after.CorrelationID, after.CorrelationSequence, after.StorageOrder)
	}
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	query += ` ORDER BY occurred_at, correlation_id, correlation_sequence, storage_order LIMIT ?`
	args = append(args, boundedLimit(filter.Limit))
	var out []receiptRow
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("read progress receipts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row receiptRow
			if err := rows.Scan(&row.storageOrder, &row.occurredAt, &row.correlationID, &row.correlationSequence, &row.payload); err != nil {
				return fmt.Errorf("read progress receipts: scan: %w", err)
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read progress receipts: rows: %w", err)
		}
		return nil
	})
	return out, err
}

func encodeCursor(payload cursorPayload) Cursor {
	data, _ := canonicalJSON(payload)
	return Cursor(base64.RawURLEncoding.EncodeToString(data))
}

func decodeCursor(cursor Cursor) (cursorPayload, error) {
	text := strings.TrimSpace(string(cursor))
	if text == "" {
		return cursorPayload{}, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return cursorPayload{}, typed(ErrInvalidRecordCode, "invalid progress receipt cursor")
	}
	var payload cursorPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return cursorPayload{}, typed(ErrInvalidRecordCode, "invalid progress receipt cursor")
	}
	if payload.Version != 1 || strings.TrimSpace(payload.OccurredAt) == "" || strings.TrimSpace(payload.CorrelationID) == "" || payload.CorrelationSequence < 0 || payload.StorageOrder < 0 {
		return cursorPayload{}, typed(ErrInvalidRecordCode, "invalid progress receipt cursor")
	}
	return payload, nil
}

func validateCursorScope(cursor cursorPayload, filter ReadFilter) error {
	if cursor.Version == 0 || strings.TrimSpace(cursor.ProjectID) == "" {
		return nil
	}
	if cursor.ProjectID != strings.TrimSpace(filter.ProjectID) ||
		cursor.DeliveryRunID != strings.TrimSpace(filter.DeliveryRunID) ||
		cursor.CorrelationIDFilter != strings.TrimSpace(filter.CorrelationID) ||
		cursor.TaskIDFilter != strings.TrimSpace(filter.TaskID) {
		return typed(ErrInvalidRecordCode, "progress receipt cursor scope mismatch")
	}
	return nil
}

func receiptClock(occurredAt string, now time.Time) ClockView {
	age := int64(-1)
	if parsed, err := time.Parse(time.RFC3339Nano, occurredAt); err == nil {
		age = now.Sub(parsed.UTC()).Milliseconds()
		if age < 0 {
			age = 0
		}
	}
	return ClockView{State: "persisted-receipt", ObservedAt: occurredAt, AgeMillis: age, Authority: "progress_receipt.occurred_at", DisplayText: durationText(age)}
}

func evidenceClock(evidence AgeEvidence, authority string) ClockView {
	return ClockView{State: evidence.State, ObservedAt: evidence.ObservedAt, AgeMillis: evidence.AgeMillis, Authority: authority, DisplayText: durationText(evidence.AgeMillis)}
}

func quotaBudgetView(value QuotaBudgetState) QuotaBudgetView {
	return QuotaBudgetView{
		State:             value.State,
		Confidence:        value.Confidence,
		RemainingQuantity: value.RemainingQuantity,
		Unit:              value.Unit,
		GapReasons:        append([]string(nil), value.GapReasons...),
	}
}

func unsupportedDeliveryState() DeliveryStateView {
	return DeliveryStateView{
		State:     "unsupported-pending-unacknowledged",
		Authority: "durable-delivery-evidence",
		Reason:    "no durable acknowledgement, acceptance, or wake evidence is present in this schema",
	}
}

func deliveryStateForReceipt(ctx context.Context, store storage.Store, progressReceiptID string) (DeliveryStateView, error) {
	state := unsupportedDeliveryState()
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		row := tx.QueryRow(ctx, `SELECT status, transport_contract
			FROM progress_delivery_obligations
			WHERE progress_receipt_id = ?
			ORDER BY updated_at DESC, obligation_id DESC
			LIMIT 1`, progressReceiptID)
		var status, contract string
		if err := row.Scan(&status, &contract); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("read progress delivery state: %w", err)
		}
		state = DeliveryStateView{
			State:     status,
			Authority: "progress_delivery_obligations.status",
			Reason:    "transport_contract=" + displayValue(contract),
		}
		return nil
	})
	return state, err
}

func boundFollowPollInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return DefaultFollowPollInterval
	}
	if interval < MinFollowPollInterval {
		return MinFollowPollInterval
	}
	if interval > MaxFollowPollInterval {
		return MaxFollowPollInterval
	}
	return interval
}

func durationText(ageMillis int64) string {
	if ageMillis < 0 {
		return "unknown"
	}
	d := time.Duration(ageMillis) * time.Millisecond
	if d < time.Second {
		return d.String()
	}
	if d < time.Minute {
		return d.Truncate(time.Millisecond).String()
	}
	if d < time.Hour {
		return d.Truncate(time.Second).String()
	}
	return d.Truncate(time.Minute).String()
}

func displayValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return Unknown
	}
	return value
}

func displaySlice(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ",")
}

func quantityText(quantity int64, unit string) string {
	if quantity < 0 {
		return "unknown"
	}
	unit = displayValue(unit)
	if unit == Unknown {
		return fmt.Sprintf("%d", quantity)
	}
	return fmt.Sprintf("%d %s", quantity, unit)
}

func actionText(action ActionState) string {
	summary := displayValue(action.Summary)
	if summary == Unknown {
		return fmt.Sprintf("state=%s", displayValue(action.State))
	}
	return fmt.Sprintf("state=%s summary=%s", displayValue(action.State), summary)
}
