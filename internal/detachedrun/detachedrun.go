// Package detachedrun owns the durable local authority record for bounded
// detached dispatch runs.
package detachedrun

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jasonhnd/loopcoder/internal/storage"
)

const (
	SchemaSupervisor = "loopcoder.detached_run_supervisor.v1"

	PhaseClaimed       = "claimed"
	PhaseSpawned       = "spawned"
	PhaseWorkerStarted = "worker-started"
	PhaseProviderOpen  = "provider-exposed"
	PhaseTerminal      = "terminal"

	StatusNotStarted       = "not-started"
	StatusRunning          = "running"
	StatusCancelling       = "cancelling"
	StatusSucceeded        = "succeeded"
	StatusFailed           = "failed"
	StatusCancelled        = "cancelled"
	StatusNeedsHuman       = "needs-human"
	StatusRetryable        = "retryable"
	ClassificationTerminal = "terminal"
)

type ErrorCode string

const (
	ErrInvalidRecordCode ErrorCode = "ErrInvalidRecord"
	ErrClaimConflictCode ErrorCode = "ErrClaimConflict"
	ErrStaleClaimCode    ErrorCode = "ErrStaleClaim"
	ErrTerminalCode      ErrorCode = "ErrTerminal"
	ErrUnsupportedCode   ErrorCode = "ErrUnsupportedDetachedRun"
)

var (
	ErrInvalidRecord = &TypedError{Code: ErrInvalidRecordCode}
	ErrClaimConflict = &TypedError{Code: ErrClaimConflictCode}
	ErrStaleClaim    = &TypedError{Code: ErrStaleClaimCode}
	ErrTerminal      = &TypedError{Code: ErrTerminalCode}
	ErrUnsupported   = &TypedError{Code: ErrUnsupportedCode}
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

type ClaimRequest struct {
	ProjectID           string
	RunID               string
	Owner               string
	LeaseExpiresAt      time.Time
	IssueNumber         int
	Attempt             int
	BaseBranch          string
	Branch              string
	Provider            string
	Model               string
	Effort              string
	ReceiptPolicy       string
	DeliverySinks       []string
	CancellationChannel string
	WorkerLease         map[string]any
	RecoveryEvidence    []Evidence
	Clocks              map[string]any
	Payload             map[string]any
	Now                 time.Time
}

type Evidence struct {
	Kind       string `json:"kind"`
	ID         string `json:"id,omitempty"`
	Summary    string `json:"summary"`
	Confidence string `json:"confidence"`
}

type Fence struct {
	RunID      string `json:"run_id"`
	Owner      string `json:"supervisor_owner"`
	Generation int64  `json:"supervisor_generation"`
}

type Record struct {
	SchemaVersion       string         `json:"schema_version"`
	RecordVersion       int            `json:"record_version"`
	ProjectID           string         `json:"project_id"`
	RunID               string         `json:"run_id"`
	Owner               string         `json:"supervisor_owner"`
	Generation          int64          `json:"supervisor_generation"`
	LaunchPhase         string         `json:"launch_phase"`
	Status              string         `json:"status"`
	Classification      string         `json:"classification"`
	IssueNumber         int            `json:"issue_number,omitempty"`
	Attempt             int            `json:"attempt,omitempty"`
	BaseBranch          string         `json:"base_branch,omitempty"`
	Branch              string         `json:"branch,omitempty"`
	Provider            string         `json:"provider,omitempty"`
	Model               string         `json:"model,omitempty"`
	Effort              string         `json:"effort,omitempty"`
	ProcessPID          int            `json:"process_pid,omitempty"`
	ProcessAuthority    string         `json:"process_authority,omitempty"`
	ReceiptPolicy       string         `json:"receipt_policy,omitempty"`
	DeliverySinks       []string       `json:"delivery_sinks,omitempty"`
	CancellationChannel string         `json:"cancellation_channel,omitempty"`
	WorkerLease         map[string]any `json:"worker_lease,omitempty"`
	RecoveryEvidence    []Evidence     `json:"recovery_evidence,omitempty"`
	Clocks              map[string]any `json:"clocks,omitempty"`
	ProviderExposed     bool           `json:"provider_exposed,omitempty"`
	LaunchReceiptID     string         `json:"launch_receipt_id,omitempty"`
	TerminalReceiptID   string         `json:"terminal_receipt_id,omitempty"`
	CancelRequestedAt   string         `json:"cancel_requested_at,omitempty"`
	ClaimedAt           string         `json:"claimed_at"`
	LeaseExpiresAt      string         `json:"lease_expires_at"`
	HeartbeatAt         string         `json:"heartbeat_at"`
	ProcessStartedAt    string         `json:"process_started_at,omitempty"`
	WorkerStartedAt     string         `json:"worker_started_at,omitempty"`
	TerminalAt          string         `json:"terminal_at,omitempty"`
	TerminalErrorCode   string         `json:"terminal_error_code,omitempty"`
	TerminalError       string         `json:"terminal_error,omitempty"`
	Payload             map[string]any `json:"payload,omitempty"`
	CreatedAt           string         `json:"created_at"`
	UpdatedAt           string         `json:"updated_at"`
}

type StatusResult struct {
	Record       Record `json:"record"`
	ReplayAction string `json:"replay_action"`
	Execute      bool   `json:"execute"`
	NeedsHuman   bool   `json:"needs_human"`
	Terminal     bool   `json:"terminal"`
	CanRecover   bool   `json:"can_recover"`
	Reason       string `json:"reason,omitempty"`
}

type TransitionRequest struct {
	Fence             Fence
	Phase             string
	Status            string
	Classification    string
	ProcessPID        int
	ProcessAuthority  string
	ProviderExposed   bool
	LaunchReceiptID   string
	TerminalReceiptID string
	TerminalErrorCode string
	TerminalError     string
	LeaseExpiresAt    time.Time
	Now               time.Time
}

func Claim(ctx context.Context, store storage.Store, req ClaimRequest) (Record, error) {
	if store == nil {
		return Record{}, typed(ErrInvalidRecordCode, "store is required")
	}
	req = normalizeClaim(req, store.Now())
	if err := validateClaim(req); err != nil {
		return Record{}, err
	}
	var out Record
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		existing, ok, err := loadTx(ctx, tx, req.RunID)
		if err != nil {
			return err
		}
		if ok {
			if terminal(existing.Status) {
				return typed(ErrTerminalCode, "run %s is terminal", req.RunID)
			}
			return typed(ErrClaimConflictCode, "run %s is already owned by %s generation %d", req.RunID, existing.Owner, existing.Generation)
		}
		out = recordFromClaim(req)
		return insertTx(ctx, tx, out)
	})
	return out, err
}

func Get(ctx context.Context, store storage.Store, runID string) (Record, error) {
	if store == nil {
		return Record{}, typed(ErrInvalidRecordCode, "store is required")
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return Record{}, typed(ErrInvalidRecordCode, "run_id is required")
	}
	var out Record
	err := store.WithTx(ctx, func(tx storage.Tx) error {
		record, ok, err := loadTx(ctx, tx, runID)
		if err != nil {
			return err
		}
		if !ok {
			return sql.ErrNoRows
		}
		out = record
		return nil
	})
	return out, err
}

func Reconcile(ctx context.Context, store storage.Store, runID string, now time.Time) (StatusResult, error) {
	record, err := Get(ctx, store, runID)
	if err != nil {
		return StatusResult{}, err
	}
	if now.IsZero() {
		now = store.Now()
	}
	result := StatusResult{Record: record}
	if terminal(record.Status) {
		result.ReplayAction = "reused-terminal"
		result.Terminal = true
		return result, nil
	}
	if record.ProviderExposed && strings.TrimSpace(record.LaunchReceiptID) == "" {
		result.ReplayAction = "needs-human"
		result.NeedsHuman = true
		result.Reason = "provider exposure is ambiguous without a durable launch receipt"
		return result, nil
	}
	if beforeOrEqual(now, parseTime(record.LeaseExpiresAt)) {
		result.ReplayAction = "observe-running"
		result.Execute = false
		result.Reason = "active supervisor lease"
		if record.Status == StatusNotStarted {
			result.CanRecover = false
		}
		return result, nil
	}
	switch record.LaunchPhase {
	case PhaseClaimed, PhaseSpawned:
		result.ReplayAction = "retryable"
		result.CanRecover = true
		result.Reason = "lease expired before worker start"
	case PhaseWorkerStarted:
		result.ReplayAction = "needs-human"
		result.NeedsHuman = true
		result.Reason = "worker may have started before a durable launch receipt"
	case PhaseProviderOpen:
		result.ReplayAction = "needs-human"
		result.NeedsHuman = true
		result.Reason = "provider exposure may have occurred"
	default:
		result.ReplayAction = "needs-human"
		result.NeedsHuman = true
		result.Reason = "unknown launch phase"
	}
	return result, nil
}

func MarkSpawned(ctx context.Context, store storage.Store, fence Fence, pid int, authority string, now time.Time) (Record, error) {
	return Transition(ctx, store, TransitionRequest{
		Fence:            fence,
		Phase:            PhaseSpawned,
		Status:           StatusRunning,
		Classification:   StatusRunning,
		ProcessPID:       pid,
		ProcessAuthority: authority,
		Now:              now,
	})
}

func MarkWorkerStarted(ctx context.Context, store storage.Store, fence Fence, now time.Time) (Record, error) {
	return Transition(ctx, store, TransitionRequest{
		Fence:          fence,
		Phase:          PhaseWorkerStarted,
		Status:         StatusRunning,
		Classification: StatusRunning,
		Now:            now,
	})
}

func MarkProviderExposed(ctx context.Context, store storage.Store, fence Fence, receiptID string, now time.Time) (Record, error) {
	return Transition(ctx, store, TransitionRequest{
		Fence:           fence,
		Phase:           PhaseProviderOpen,
		Status:          StatusRunning,
		Classification:  StatusRunning,
		ProviderExposed: true,
		LaunchReceiptID: receiptID,
		Now:             now,
	})
}

func Complete(ctx context.Context, store storage.Store, fence Fence, status, terminalReceiptID, errorCode, errorMessage string, now time.Time) (Record, error) {
	classification := ClassificationTerminal
	if strings.TrimSpace(status) == StatusNeedsHuman {
		classification = StatusNeedsHuman
	}
	return Transition(ctx, store, TransitionRequest{
		Fence:             fence,
		Phase:             PhaseTerminal,
		Status:            status,
		Classification:    classification,
		TerminalReceiptID: terminalReceiptID,
		TerminalErrorCode: errorCode,
		TerminalError:     errorMessage,
		Now:               now,
	})
}

func RequestCancel(ctx context.Context, store storage.Store, runID string, now time.Time) (Record, error) {
	if store == nil {
		return Record{}, typed(ErrInvalidRecordCode, "store is required")
	}
	if now.IsZero() {
		now = store.Now()
	}
	at := format(now)
	var out Record
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		record, ok, err := loadTx(ctx, tx, runID)
		if err != nil {
			return err
		}
		if !ok {
			return sql.ErrNoRows
		}
		if terminal(record.Status) {
			out = record
			return nil
		}
		res, err := tx.Exec(ctx, `UPDATE detached_run_supervisors
			SET record_version = record_version + 1, status = ?, classification = ?, cancel_requested_at = ?, updated_at = ?
			WHERE run_id = ? AND supervisor_owner = ? AND supervisor_generation = ?`,
			StatusCancelling, StatusCancelling, at, at, record.RunID, record.Owner, record.Generation)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err == nil && affected != 1 {
			return typed(ErrStaleClaimCode, "cancel update affected %d rows", affected)
		}
		out, _, err = loadTx(ctx, tx, runID)
		return err
	})
	return out, err
}

func Transition(ctx context.Context, store storage.Store, req TransitionRequest) (Record, error) {
	if store == nil {
		return Record{}, typed(ErrInvalidRecordCode, "store is required")
	}
	req = normalizeTransition(req, store.Now())
	if err := validateFence(req.Fence); err != nil {
		return Record{}, err
	}
	var out Record
	err := store.WithWriteTx(ctx, func(tx storage.Tx) error {
		existing, ok, err := loadTx(ctx, tx, req.Fence.RunID)
		if err != nil {
			return err
		}
		if !ok {
			return sql.ErrNoRows
		}
		if existing.Owner != req.Fence.Owner || existing.Generation != req.Fence.Generation {
			return typed(ErrStaleClaimCode, "stale supervisor fence for %s", req.Fence.RunID)
		}
		if terminal(existing.Status) && req.Phase != PhaseTerminal {
			return typed(ErrTerminalCode, "run %s is terminal", req.Fence.RunID)
		}
		out = mergeTransition(existing, req)
		return updateTx(ctx, tx, out, existing)
	})
	return out, err
}

func (r Record) Fence() Fence {
	return Fence{RunID: r.RunID, Owner: r.Owner, Generation: r.Generation}
}

func normalizeClaim(req ClaimRequest, fallback time.Time) ClaimRequest {
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.RunID = strings.TrimSpace(req.RunID)
	req.Owner = strings.TrimSpace(req.Owner)
	if req.Now.IsZero() {
		req.Now = fallback
	}
	if req.LeaseExpiresAt.IsZero() {
		req.LeaseExpiresAt = req.Now.Add(30 * time.Minute)
	}
	req.BaseBranch = strings.TrimSpace(req.BaseBranch)
	req.Branch = strings.TrimSpace(req.Branch)
	req.Provider = strings.TrimSpace(req.Provider)
	req.Model = strings.TrimSpace(req.Model)
	req.Effort = strings.TrimSpace(req.Effort)
	if strings.TrimSpace(req.ReceiptPolicy) == "" {
		req.ReceiptPolicy = "durable-progress-receipts"
	}
	if strings.TrimSpace(req.CancellationChannel) == "" {
		req.CancellationChannel = "detached-run:" + req.RunID
	}
	if req.WorkerLease == nil {
		req.WorkerLease = map[string]any{}
	}
	if req.Clocks == nil {
		req.Clocks = map[string]any{
			"supervisor_heartbeat": "detached-run-supervisor",
			"meaningful_progress":  "worker-attempt-progress",
			"receipt_generation":   "progress-supervisor",
			"delivery_attempt":     "progress-outbox",
			"worker_lease":         "agent-ownership-locks",
			"watchdog":             "supervisedexec",
			"budget":               "budget-reservations",
		}
	}
	if req.Payload == nil {
		req.Payload = map[string]any{}
	}
	return req
}

func validateClaim(req ClaimRequest) error {
	if req.ProjectID == "" || req.RunID == "" || req.Owner == "" {
		return typed(ErrInvalidRecordCode, "project_id, run_id, and owner are required")
	}
	if !req.LeaseExpiresAt.After(req.Now) {
		return typed(ErrInvalidRecordCode, "lease_expires_at must be after now")
	}
	return nil
}

func recordFromClaim(req ClaimRequest) Record {
	now := format(req.Now)
	return Record{
		SchemaVersion:       SchemaSupervisor,
		RecordVersion:       1,
		ProjectID:           req.ProjectID,
		RunID:               req.RunID,
		Owner:               req.Owner,
		Generation:          1,
		LaunchPhase:         PhaseClaimed,
		Status:              StatusNotStarted,
		Classification:      StatusNotStarted,
		IssueNumber:         req.IssueNumber,
		Attempt:             req.Attempt,
		BaseBranch:          req.BaseBranch,
		Branch:              req.Branch,
		Provider:            req.Provider,
		Model:               req.Model,
		Effort:              req.Effort,
		ReceiptPolicy:       req.ReceiptPolicy,
		DeliverySinks:       append([]string(nil), req.DeliverySinks...),
		CancellationChannel: req.CancellationChannel,
		WorkerLease:         cloneMap(req.WorkerLease),
		RecoveryEvidence:    append([]Evidence(nil), req.RecoveryEvidence...),
		Clocks:              cloneMap(req.Clocks),
		ClaimedAt:           now,
		LeaseExpiresAt:      format(req.LeaseExpiresAt),
		HeartbeatAt:         now,
		CreatedAt:           now,
		UpdatedAt:           now,
		Payload:             cloneMap(req.Payload),
	}
}

func normalizeTransition(req TransitionRequest, fallback time.Time) TransitionRequest {
	req.Phase = strings.TrimSpace(req.Phase)
	req.Status = strings.TrimSpace(req.Status)
	req.Classification = strings.TrimSpace(req.Classification)
	if req.Now.IsZero() {
		req.Now = fallback
	}
	return req
}

func validateFence(f Fence) error {
	if strings.TrimSpace(f.RunID) == "" || strings.TrimSpace(f.Owner) == "" || f.Generation <= 0 {
		return typed(ErrInvalidRecordCode, "run_id, owner, and generation fence are required")
	}
	return nil
}

func mergeTransition(existing Record, req TransitionRequest) Record {
	out := existing
	out.RecordVersion++
	if req.Phase != "" {
		out.LaunchPhase = req.Phase
	}
	if req.Status != "" {
		out.Status = req.Status
	}
	if req.Classification != "" {
		out.Classification = req.Classification
	}
	if req.ProcessPID > 0 {
		out.ProcessPID = req.ProcessPID
	}
	if strings.TrimSpace(req.ProcessAuthority) != "" {
		out.ProcessAuthority = strings.TrimSpace(req.ProcessAuthority)
	}
	if req.ProviderExposed {
		out.ProviderExposed = true
	}
	if strings.TrimSpace(req.LaunchReceiptID) != "" {
		out.LaunchReceiptID = strings.TrimSpace(req.LaunchReceiptID)
	}
	if strings.TrimSpace(req.TerminalReceiptID) != "" {
		out.TerminalReceiptID = strings.TrimSpace(req.TerminalReceiptID)
	}
	if strings.TrimSpace(req.TerminalErrorCode) != "" {
		out.TerminalErrorCode = bound(req.TerminalErrorCode, 120)
	}
	if strings.TrimSpace(req.TerminalError) != "" {
		out.TerminalError = bound(req.TerminalError, 500)
	}
	if !req.LeaseExpiresAt.IsZero() {
		out.LeaseExpiresAt = format(req.LeaseExpiresAt)
	}
	now := format(req.Now)
	out.HeartbeatAt = now
	out.UpdatedAt = now
	switch req.Phase {
	case PhaseSpawned:
		if out.ProcessStartedAt == "" {
			out.ProcessStartedAt = now
		}
	case PhaseWorkerStarted, PhaseProviderOpen:
		if out.WorkerStartedAt == "" {
			out.WorkerStartedAt = now
		}
	case PhaseTerminal:
		if out.TerminalAt == "" {
			out.TerminalAt = now
		}
	}
	return out
}

func insertTx(ctx context.Context, tx storage.Tx, r Record) error {
	sinks, workerLease, evidence, clocks, payload, err := encodeJSONFields(r)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO detached_run_supervisors(
		run_id, project_id, schema_version, record_version, supervisor_owner, supervisor_generation,
		launch_phase, status, classification, issue_number, attempt, base_branch, branch, provider, model, effort,
		process_pid, process_authority, receipt_policy, delivery_sinks_json, cancellation_channel, worker_lease_json,
		recovery_evidence_json, clocks_json, provider_exposed, launch_receipt_id, terminal_receipt_id,
		cancel_requested_at, claimed_at, lease_expires_at, heartbeat_at, process_started_at, worker_started_at,
		terminal_at, terminal_error_code, terminal_error, payload_json, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.ProjectID, r.SchemaVersion, r.RecordVersion, r.Owner, r.Generation,
		r.LaunchPhase, r.Status, r.Classification, r.IssueNumber, r.Attempt, r.BaseBranch, r.Branch, r.Provider, r.Model, r.Effort,
		r.ProcessPID, r.ProcessAuthority, r.ReceiptPolicy, sinks, r.CancellationChannel, workerLease,
		evidence, clocks, boolInt(r.ProviderExposed), r.LaunchReceiptID, r.TerminalReceiptID,
		r.CancelRequestedAt, r.ClaimedAt, r.LeaseExpiresAt, r.HeartbeatAt, r.ProcessStartedAt, r.WorkerStartedAt,
		r.TerminalAt, r.TerminalErrorCode, r.TerminalError, payload, r.CreatedAt, r.UpdatedAt)
	return err
}

func updateTx(ctx context.Context, tx storage.Tx, r, existing Record) error {
	sinks, workerLease, evidence, clocks, payload, err := encodeJSONFields(r)
	if err != nil {
		return err
	}
	res, err := tx.Exec(ctx, `UPDATE detached_run_supervisors SET
		record_version = ?, launch_phase = ?, status = ?, classification = ?, process_pid = ?, process_authority = ?,
		receipt_policy = ?, delivery_sinks_json = ?, cancellation_channel = ?, worker_lease_json = ?,
		recovery_evidence_json = ?, clocks_json = ?, provider_exposed = ?, launch_receipt_id = ?, terminal_receipt_id = ?,
		cancel_requested_at = ?, lease_expires_at = ?, heartbeat_at = ?, process_started_at = ?, worker_started_at = ?,
		terminal_at = ?, terminal_error_code = ?, terminal_error = ?, payload_json = ?, updated_at = ?
		WHERE run_id = ? AND supervisor_owner = ? AND supervisor_generation = ? AND record_version = ?`,
		r.RecordVersion, r.LaunchPhase, r.Status, r.Classification, r.ProcessPID, r.ProcessAuthority,
		r.ReceiptPolicy, sinks, r.CancellationChannel, workerLease,
		evidence, clocks, boolInt(r.ProviderExposed), r.LaunchReceiptID, r.TerminalReceiptID,
		r.CancelRequestedAt, r.LeaseExpiresAt, r.HeartbeatAt, r.ProcessStartedAt, r.WorkerStartedAt,
		r.TerminalAt, r.TerminalErrorCode, r.TerminalError, payload, r.UpdatedAt,
		existing.RunID, existing.Owner, existing.Generation, existing.RecordVersion)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err == nil && affected != 1 {
		return typed(ErrStaleClaimCode, "transition affected %d rows", affected)
	}
	return nil
}

func loadTx(ctx context.Context, tx storage.Tx, runID string) (Record, bool, error) {
	var r Record
	var sinks, workerLease, evidence, clocks, payload string
	var providerExposed int
	err := tx.QueryRow(ctx, `SELECT
		run_id, project_id, schema_version, record_version, supervisor_owner, supervisor_generation,
		launch_phase, status, classification, issue_number, attempt, base_branch, branch, provider, model, effort,
		process_pid, process_authority, receipt_policy, delivery_sinks_json, cancellation_channel, worker_lease_json,
		recovery_evidence_json, clocks_json, provider_exposed, launch_receipt_id, terminal_receipt_id,
		cancel_requested_at, claimed_at, lease_expires_at, heartbeat_at, process_started_at, worker_started_at,
		terminal_at, terminal_error_code, terminal_error, payload_json, created_at, updated_at
		FROM detached_run_supervisors WHERE run_id = ?`, strings.TrimSpace(runID)).Scan(
		&r.RunID, &r.ProjectID, &r.SchemaVersion, &r.RecordVersion, &r.Owner, &r.Generation,
		&r.LaunchPhase, &r.Status, &r.Classification, &r.IssueNumber, &r.Attempt, &r.BaseBranch, &r.Branch, &r.Provider, &r.Model, &r.Effort,
		&r.ProcessPID, &r.ProcessAuthority, &r.ReceiptPolicy, &sinks, &r.CancellationChannel, &workerLease,
		&evidence, &clocks, &providerExposed, &r.LaunchReceiptID, &r.TerminalReceiptID,
		&r.CancelRequestedAt, &r.ClaimedAt, &r.LeaseExpiresAt, &r.HeartbeatAt, &r.ProcessStartedAt, &r.WorkerStartedAt,
		&r.TerminalAt, &r.TerminalErrorCode, &r.TerminalError, &payload, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	r.ProviderExposed = providerExposed == 1
	if err := decodeJSON(sinks, &r.DeliverySinks); err != nil {
		return Record{}, false, err
	}
	if err := decodeJSON(workerLease, &r.WorkerLease); err != nil {
		return Record{}, false, err
	}
	if err := decodeJSON(evidence, &r.RecoveryEvidence); err != nil {
		return Record{}, false, err
	}
	if err := decodeJSON(clocks, &r.Clocks); err != nil {
		return Record{}, false, err
	}
	if err := decodeJSON(payload, &r.Payload); err != nil {
		return Record{}, false, err
	}
	return r, true, nil
}

func encodeJSONFields(r Record) (string, string, string, string, string, error) {
	sinks, err := marshal(r.DeliverySinks)
	if err != nil {
		return "", "", "", "", "", err
	}
	lease, err := marshal(r.WorkerLease)
	if err != nil {
		return "", "", "", "", "", err
	}
	evidence, err := marshal(r.RecoveryEvidence)
	if err != nil {
		return "", "", "", "", "", err
	}
	clocks, err := marshal(r.Clocks)
	if err != nil {
		return "", "", "", "", "", err
	}
	payload, err := marshal(r.Payload)
	return sinks, lease, evidence, clocks, payload, err
}

func marshal(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decodeJSON(raw string, out any) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	return json.Unmarshal([]byte(raw), out)
}

func typed(code ErrorCode, format string, args ...any) error {
	msg := ""
	if format != "" {
		msg = fmt.Sprintf(format, args...)
	}
	return &TypedError{Code: code, Message: msg}
}

func terminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusNeedsHuman:
		return true
	default:
		return false
	}
}

func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func beforeOrEqual(a, b time.Time) bool {
	if b.IsZero() {
		return false
	}
	return a.Before(b) || a.Equal(b)
}

func format(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func bound(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
