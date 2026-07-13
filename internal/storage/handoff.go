package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	HandoffTransactionSchema = "loopcoder.handoff_transaction.v1"
	HandoffPolicyVersion     = "0830.handoff.v1"

	HandoffStatusTransferred = "transferred"
	HandoffStatusNeedsHuman  = "needs-human"
	HandoffStatusBlocked     = "blocked"

	HandoffTriggerQuotaAvailability = "quota-availability"

	SideEffectStateNone          = "none"
	SideEffectStateNotStarted    = "not-started"
	SideEffectStateReadOnly      = "read-only"
	SideEffectStateUnknown       = "unknown"
	SideEffectStateAmbiguous     = "ambiguous"
	SideEffectStateProviderStart = "provider-started"
)

const (
	ErrHandoffRequiredCode          FederationErrorCode = "ErrHandoffRequired"
	ErrHandoffReplayMismatchCode    FederationErrorCode = "ErrHandoffReplayMismatch"
	ErrUnsupportedHandoffReasonCode FederationErrorCode = "ErrUnsupportedHandoffReason"
	ErrAmbiguousHandoffEvidenceCode FederationErrorCode = "ErrAmbiguousHandoffEvidence"
	ErrHandoffAuthorityMismatchCode FederationErrorCode = "ErrHandoffAuthorityMismatch"
	ErrHandoffSideEffectUnknownCode FederationErrorCode = "ErrHandoffSideEffectUnknown"
	ErrHandoffAcceptedTaskStaleCode FederationErrorCode = "ErrHandoffAcceptedTaskStale"
)

type HandoffRequest struct {
	IdempotencyKey              string
	ProjectID                   string
	DeliveryRunID               string
	TaskID                      string
	ParentRunID                 string
	ChildRunID                  string
	SourceAttemptID             string
	SourceExecutorID            string
	SourceClaimGeneration       int64
	TriggerKind                 string
	ReasonCodes                 []string
	EvidenceRecordIDs           []string
	TriggerSnapshotJSON         string
	PolicyVersion               string
	SideEffectState             string
	DestinationRoutePlaceholder string
	DestinationExecutorID       string
	DestinationLeaseExpiresAt   string
	RequestedAt                 string
}

type HandoffTransaction struct {
	SchemaVersion               string   `json:"schema_version"`
	RecordVersion               int      `json:"record_version"`
	HandoffID                   string   `json:"handoff_id"`
	ProjectID                   string   `json:"project_id"`
	DeliveryRunID               string   `json:"delivery_run_id"`
	TaskID                      string   `json:"task_id"`
	ParentRunID                 string   `json:"parent_run_id"`
	ChildRunID                  string   `json:"child_run_id"`
	SourceAttemptID             string   `json:"source_attempt_id"`
	SourceExecutorID            string   `json:"source_executor_id"`
	SourceClaimGeneration       int64    `json:"source_claim_generation"`
	SourceClaimSnapshotJSON     string   `json:"source_claim_snapshot_json"`
	TriggerKind                 string   `json:"trigger_kind"`
	ReasonCodes                 []string `json:"reason_codes"`
	EvidenceRecordIDs           []string `json:"evidence_record_ids"`
	TriggerSnapshotJSON         string   `json:"trigger_snapshot_json"`
	PolicyVersion               string   `json:"policy_version"`
	InputFingerprint            string   `json:"input_fingerprint,omitempty"`
	PolicyFingerprint           string   `json:"policy_fingerprint,omitempty"`
	PlanFingerprint             string   `json:"plan_fingerprint,omitempty"`
	AuthorizationFingerprint    string   `json:"authorization_fingerprint,omitempty"`
	AcceptedTaskFingerprint     string   `json:"accepted_task_fingerprint"`
	AcceptedTaskSnapshotJSON    string   `json:"accepted_task_snapshot_json"`
	SideEffectState             string   `json:"side_effect_state"`
	DestinationRoutePlaceholder string   `json:"destination_route_placeholder"`
	DestinationExecutorID       string   `json:"destination_executor_id"`
	DestinationClaimGeneration  int64    `json:"destination_claim_generation"`
	HandoffGeneration           int64    `json:"handoff_generation"`
	HandoffStatus               string   `json:"handoff_status"`
	NextAction                  string   `json:"next_action"`
	RequestFingerprint          string   `json:"request_fingerprint"`
	IdempotencyKey              string   `json:"idempotency_key"`
	CreatedAt                   string   `json:"created_at"`
	UpdatedAt                   string   `json:"updated_at"`
}

type handoffReplayConflict struct {
	existing HandoffTransaction
}

func (e handoffReplayConflict) Error() string {
	return "handoff transaction replay changed payload"
}

func RecordHandoffTransaction(ctx context.Context, store Store, req HandoffRequest) (HandoffTransaction, error) {
	if store == nil {
		return HandoffTransaction{}, federationError(ErrHandoffRequiredCode, "store is required")
	}
	req = normalizeHandoffRequest(req, store.Now())
	if err := validateHandoffRequest(req); err != nil {
		return HandoffTransaction{}, err
	}
	var out HandoffTransaction
	err := withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			record, err := recordHandoffTransactionTx(ctx, tx, req)
			if err != nil {
				return err
			}
			out = record
			return nil
		})
	})
	var replayConflict handoffReplayConflict
	if err != nil && strings.TrimSpace(req.IdempotencyKey) != "" && !strings.HasPrefix(err.Error(), "storage write transaction: begin immediate") {
		if txErr := store.WithTx(ctx, func(tx Tx) error {
			existing, ok, loadErr := loadHandoffByIdempotencyTx(ctx, tx, req.ProjectID, req.DeliveryRunID, req.IdempotencyKey)
			if loadErr != nil || !ok {
				return loadErr
			}
			if existing.RequestFingerprint == handoffRequestFingerprint(req) {
				out = existing
				return nil
			}
			return handoffReplayConflict{existing: existing}
		}); txErr == nil && out.HandoffID != "" {
			return out, nil
		} else if txErr != nil {
			if asHandoffReplayConflict(txErr, &replayConflict) {
				return replayConflict.existing, federationError(ErrHandoffReplayMismatchCode, "idempotency key %s replays with different handoff payload", req.IdempotencyKey)
			}
		}
	}
	return out, err
}

func recordHandoffTransactionTx(ctx context.Context, tx Tx, req HandoffRequest) (HandoffTransaction, error) {
	fingerprint := handoffRequestFingerprint(req)
	if existing, ok, err := loadHandoffByIdempotencyTx(ctx, tx, req.ProjectID, req.DeliveryRunID, req.IdempotencyKey); err != nil {
		return HandoffTransaction{}, err
	} else if ok {
		if existing.RequestFingerprint != fingerprint {
			return HandoffTransaction{}, federationError(ErrHandoffReplayMismatchCode, "idempotency key %s replays with different handoff payload", req.IdempotencyKey)
		}
		return existing, nil
	}
	if existing, ok, err := loadHandoffBySourceAttemptTx(ctx, tx, req.ProjectID, req.DeliveryRunID, req.TaskID, req.SourceAttemptID); err != nil {
		return HandoffTransaction{}, err
	} else if ok {
		if existing.RequestFingerprint != fingerprint {
			return HandoffTransaction{}, federationError(ErrHandoffReplayMismatchCode, "source attempt %s replays with different handoff payload", req.SourceAttemptID)
		}
		return existing, nil
	}

	task, err := loadHandoffTaskSnapshotTx(ctx, tx, req)
	if err != nil {
		return HandoffTransaction{}, err
	}
	claim, ok, err := currentRunClaim(ctx, tx, req.ChildRunID)
	if err != nil {
		return HandoffTransaction{}, err
	}
	if !ok {
		return HandoffTransaction{}, federationError(ErrHandoffAuthorityMismatchCode, "run %s has no execution claim", req.ChildRunID)
	}
	if claim.ExecutorID != req.SourceExecutorID || claim.ClaimGeneration != req.SourceClaimGeneration {
		return HandoffTransaction{}, federationError(ErrStaleClaimCode, "source owner/generation no longer owns run %s", req.ChildRunID)
	}
	if req.ParentRunID != "" {
		if _, ok, err := currentRunEdgeStatus(ctx, tx, req.ParentRunID, req.ChildRunID); err != nil {
			return HandoffTransaction{}, err
		} else if !ok {
			return HandoffTransaction{}, federationError(ErrHandoffAuthorityMismatchCode, "edge %s/%s is missing", req.ParentRunID, req.ChildRunID)
		}
	}

	status, nextAction, terminalCode := classifyHandoff(req)
	destinationGeneration := claim.ClaimGeneration + 1
	destinationOwner := strings.TrimSpace(req.DestinationExecutorID)
	if destinationOwner == "" {
		destinationOwner = stableID("handoff_", req.ProjectID, req.DeliveryRunID, req.TaskID, fmt.Sprint(destinationGeneration))
	}
	lease := strings.TrimSpace(req.DestinationLeaseExpiresAt)
	if lease == "" {
		if parsed, err := time.Parse(time.RFC3339Nano, req.RequestedAt); err == nil {
			lease = formatTimestamp(parsed.UTC().Add(15 * time.Minute))
		}
	}
	if status != HandoffStatusTransferred {
		destinationOwner = stableID("handoff_needs_human_", req.ProjectID, req.DeliveryRunID, req.TaskID, fmt.Sprint(destinationGeneration))
		lease = req.RequestedAt
	}
	route := strings.TrimSpace(req.DestinationRoutePlaceholder)
	if route == "" {
		route = "route-pending"
	}
	if status != HandoffStatusTransferred {
		route = "needs-human"
	}

	sourceClaimSnapshot, err := compactJSON(map[string]any{
		"run_id":                   claim.RunID,
		"executor_id":              claim.ExecutorID,
		"claim_generation":         claim.ClaimGeneration,
		"claimed_at":               claim.ClaimedAt,
		"lease_expires_at":         claim.LeaseExpiresAt,
		"heartbeat_at":             claim.HeartbeatAt,
		"claim_phase":              claim.ClaimPhase,
		"provider_idempotency_key": claim.ProviderKey,
		"provider_receipt":         claim.ProviderReceipt,
	})
	if err != nil {
		return HandoffTransaction{}, err
	}

	record := HandoffTransaction{
		SchemaVersion:               HandoffTransactionSchema,
		RecordVersion:               1,
		HandoffID:                   stableID("hnd_", req.ProjectID, req.DeliveryRunID, req.TaskID, req.SourceAttemptID, fingerprint),
		ProjectID:                   req.ProjectID,
		DeliveryRunID:               req.DeliveryRunID,
		TaskID:                      req.TaskID,
		ParentRunID:                 req.ParentRunID,
		ChildRunID:                  req.ChildRunID,
		SourceAttemptID:             req.SourceAttemptID,
		SourceExecutorID:            req.SourceExecutorID,
		SourceClaimGeneration:       req.SourceClaimGeneration,
		SourceClaimSnapshotJSON:     sourceClaimSnapshot,
		TriggerKind:                 req.TriggerKind,
		ReasonCodes:                 sortedCopyAgent(req.ReasonCodes),
		EvidenceRecordIDs:           sortedCopyAgent(req.EvidenceRecordIDs),
		TriggerSnapshotJSON:         req.TriggerSnapshotJSON,
		PolicyVersion:               req.PolicyVersion,
		InputFingerprint:            task.inputFingerprint,
		PolicyFingerprint:           task.policyFingerprint,
		PlanFingerprint:             task.planFingerprint,
		AuthorizationFingerprint:    task.authorizationFingerprint,
		AcceptedTaskFingerprint:     task.acceptedTaskFingerprint,
		AcceptedTaskSnapshotJSON:    task.acceptedTaskSnapshotJSON,
		SideEffectState:             req.SideEffectState,
		DestinationRoutePlaceholder: route,
		DestinationExecutorID:       destinationOwner,
		DestinationClaimGeneration:  destinationGeneration,
		HandoffGeneration:           destinationGeneration,
		HandoffStatus:               status,
		NextAction:                  nextAction,
		RequestFingerprint:          fingerprint,
		IdempotencyKey:              req.IdempotencyKey,
		CreatedAt:                   req.RequestedAt,
		UpdatedAt:                   req.RequestedAt,
	}
	reasonsJSON, err := json.Marshal(record.ReasonCodes)
	if err != nil {
		return HandoffTransaction{}, err
	}
	evidenceJSON, err := json.Marshal(record.EvidenceRecordIDs)
	if err != nil {
		return HandoffTransaction{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE run_claims
		SET executor_id = ?, claim_generation = ?, claimed_at = ?, lease_expires_at = ?, heartbeat_at = ?, phase = ?, provider_receipt = ''
		WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`,
		record.DestinationExecutorID, record.DestinationClaimGeneration, req.RequestedAt, lease, req.RequestedAt, ClaimPhaseClaimed,
		req.ChildRunID, req.SourceExecutorID, req.SourceClaimGeneration); err != nil {
		return HandoffTransaction{}, fmt.Errorf("record handoff: fence claim: %w", err)
	}
	if err := requireRowsAffected(ctx, tx, req.ChildRunID, record.DestinationExecutorID, record.DestinationClaimGeneration); err != nil {
		return HandoffTransaction{}, err
	}
	if status == HandoffStatusTransferred {
		if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
			RunID:       req.ChildRunID,
			ParentRunID: req.ParentRunID,
			ChildRunID:  req.ChildRunID,
			Status:      "waiting",
			UpdatedAt:   req.RequestedAt,
			Reason:      "execution handoff after " + strings.Join(record.ReasonCodes, ","),
			Source:      "handoff",
		}); err != nil {
			return HandoffTransaction{}, err
		}
	} else {
		if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
			RunID:       req.ChildRunID,
			ParentRunID: req.ParentRunID,
			ChildRunID:  req.ChildRunID,
			Status:      status,
			UpdatedAt:   req.RequestedAt,
			Reason:      string(terminalCode),
			Source:      "handoff",
		}); err != nil {
			return HandoffTransaction{}, err
		}
	}
	if err := updateDeliveryAfterHandoffTx(ctx, tx, req, status, terminalCode); err != nil {
		return HandoffTransaction{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO handoff_transactions(
			handoff_id, schema_version, record_version, project_id, delivery_run_id, task_id, child_run_id, parent_run_id,
			source_attempt_id, source_executor_id, source_claim_generation, source_claim_snapshot_json, trigger_kind,
			reason_codes_json, evidence_record_ids_json, trigger_snapshot_json, policy_version, input_fingerprint,
			policy_fingerprint, plan_fingerprint, authorization_fingerprint, accepted_task_fingerprint, accepted_task_snapshot_json,
			side_effect_state, destination_route_placeholder, destination_executor_id, destination_claim_generation, handoff_generation,
			handoff_status, next_action, request_fingerprint, idempotency_key, created_at, updated_at
		) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.HandoffID, record.SchemaVersion, record.ProjectID, record.DeliveryRunID, record.TaskID, record.ChildRunID, record.ParentRunID,
		record.SourceAttemptID, record.SourceExecutorID, record.SourceClaimGeneration, record.SourceClaimSnapshotJSON, record.TriggerKind,
		string(reasonsJSON), string(evidenceJSON), record.TriggerSnapshotJSON, record.PolicyVersion, record.InputFingerprint,
		record.PolicyFingerprint, record.PlanFingerprint, record.AuthorizationFingerprint, record.AcceptedTaskFingerprint, record.AcceptedTaskSnapshotJSON,
		record.SideEffectState, record.DestinationRoutePlaceholder, record.DestinationExecutorID, record.DestinationClaimGeneration, record.HandoffGeneration,
		record.HandoffStatus, record.NextAction, record.RequestFingerprint, record.IdempotencyKey, record.CreatedAt, record.UpdatedAt); err != nil {
		if IsConstraint(err) {
			if existing, ok, loadErr := loadHandoffBySourceAttemptTx(ctx, tx, req.ProjectID, req.DeliveryRunID, req.TaskID, req.SourceAttemptID); loadErr == nil && ok && existing.RequestFingerprint == fingerprint {
				return existing, nil
			}
		}
		return HandoffTransaction{}, fmt.Errorf("record handoff: insert transaction: %w", err)
	}
	return record, nil
}

type handoffTaskSnapshot struct {
	inputFingerprint         string
	policyFingerprint        string
	planFingerprint          string
	authorizationFingerprint string
	acceptedTaskFingerprint  string
	acceptedTaskSnapshotJSON string
}

func loadHandoffTaskSnapshotTx(ctx context.Context, tx Tx, req HandoffRequest) (handoffTaskSnapshot, error) {
	var runProject, runInput, runPolicy, runPlan, runAuth, runPolicyVersion string
	if err := tx.QueryRow(ctx, `SELECT project_id, input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint, policy_version
		FROM delivery_runs WHERE delivery_run_id = ?`, req.DeliveryRunID).Scan(&runProject, &runInput, &runPolicy, &runPlan, &runAuth, &runPolicyVersion); err != nil {
		return handoffTaskSnapshot{}, federationError(ErrHandoffAcceptedTaskStaleCode, "delivery run %s is missing", req.DeliveryRunID)
	}
	var taskProject, taskRun, taskState, taskPolicyVersion, taskPlan, taskAuth, taskScope, taskPermission, taskSideEffect string
	if err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, state, policy_version, plan_fingerprint, authorization_fingerprint, scope_json, permission, side_effect_class
		FROM delivery_tasks WHERE task_id = ?`, req.TaskID).Scan(&taskProject, &taskRun, &taskState, &taskPolicyVersion, &taskPlan, &taskAuth, &taskScope, &taskPermission, &taskSideEffect); err != nil {
		return handoffTaskSnapshot{}, federationError(ErrHandoffAcceptedTaskStaleCode, "delivery task %s is missing", req.TaskID)
	}
	if runProject != req.ProjectID || taskProject != req.ProjectID || taskRun != req.DeliveryRunID || taskPlan != runPlan || taskAuth != runAuth {
		return handoffTaskSnapshot{}, federationError(ErrHandoffAcceptedTaskStaleCode, "delivery task authority no longer matches accepted run authority")
	}
	var attemptTask, attemptRun, attemptProject, attemptExecutor, attemptState string
	var attemptGeneration int64
	if err := tx.QueryRow(ctx, `SELECT task_id, delivery_run_id, project_id, executor_id, claim_generation, state
		FROM delivery_attempts WHERE attempt_id = ?`, req.SourceAttemptID).Scan(&attemptTask, &attemptRun, &attemptProject, &attemptExecutor, &attemptGeneration, &attemptState); err != nil {
		return handoffTaskSnapshot{}, federationError(ErrHandoffAcceptedTaskStaleCode, "source attempt %s is missing", req.SourceAttemptID)
	}
	if attemptTask != req.TaskID || attemptRun != req.DeliveryRunID || attemptProject != req.ProjectID || attemptExecutor != req.SourceExecutorID || attemptGeneration != req.SourceClaimGeneration {
		return handoffTaskSnapshot{}, federationError(ErrHandoffAuthorityMismatchCode, "source attempt does not match requested owner/generation")
	}
	var requirementFingerprint, riskTier string
	err := tx.QueryRow(ctx, `SELECT task_requirement_fingerprint, risk_tier
		FROM task_requirements
		WHERE project_id = ? AND delivery_run_id = ? AND task_id = ? AND plan_fingerprint = ?`,
		req.ProjectID, req.DeliveryRunID, req.TaskID, runPlan).Scan(&requirementFingerprint, &riskTier)
	if err != nil && err != sql.ErrNoRows {
		return handoffTaskSnapshot{}, fmt.Errorf("inspect accepted task requirement: %w", err)
	}
	var graphID, graphFingerprint, approvalID, routingProfileFingerprint string
	err = tx.QueryRow(ctx, `SELECT graph_version_id, graph_fingerprint, approval_id, routing_policy_profile_fingerprint
		FROM accepted_task_graph_versions
		WHERE project_id = ? AND delivery_run_id = ? AND authorization_fingerprint = ?`,
		req.ProjectID, req.DeliveryRunID, runAuth).Scan(&graphID, &graphFingerprint, &approvalID, &routingProfileFingerprint)
	if err != nil && err != sql.ErrNoRows {
		return handoffTaskSnapshot{}, fmt.Errorf("inspect accepted task graph: %w", err)
	}
	accepted := firstNonEmptyAgent(requirementFingerprint, graphFingerprint, digestJSON(map[string]string{
		"project_id": req.ProjectID, "delivery_run_id": req.DeliveryRunID, "task_id": req.TaskID, "plan_fingerprint": runPlan, "authorization_fingerprint": runAuth,
	}))
	snapshotJSON, err := compactJSON(map[string]any{
		"task_id":                            req.TaskID,
		"task_state":                         taskState,
		"task_policy_version":                taskPolicyVersion,
		"run_policy_version":                 runPolicyVersion,
		"permission":                         taskPermission,
		"side_effect_class":                  taskSideEffect,
		"scope_json":                         taskScope,
		"risk_tier":                          riskTier,
		"task_requirement_fingerprint":       requirementFingerprint,
		"accepted_graph_version_id":          graphID,
		"accepted_graph_fingerprint":         graphFingerprint,
		"approval_id":                        approvalID,
		"routing_policy_profile_fingerprint": routingProfileFingerprint,
		"source_attempt_state":               attemptState,
	})
	if err != nil {
		return handoffTaskSnapshot{}, err
	}
	return handoffTaskSnapshot{
		inputFingerprint:         runInput,
		policyFingerprint:        runPolicy,
		planFingerprint:          runPlan,
		authorizationFingerprint: runAuth,
		acceptedTaskFingerprint:  accepted,
		acceptedTaskSnapshotJSON: snapshotJSON,
	}, nil
}

func updateDeliveryAfterHandoffTx(ctx context.Context, tx Tx, req HandoffRequest, status string, terminalCode FederationErrorCode) error {
	attemptState := "stale"
	taskState := "claimed"
	if status != HandoffStatusTransferred {
		attemptState = "needs-human"
		taskState = "needs-human"
	}
	if _, err := tx.Exec(ctx, `UPDATE delivery_attempts
		SET state = ?, record_version = record_version + 1, updated_at = ?, ended_at = COALESCE(NULLIF(ended_at, ''), ?),
			terminal_error_code = CASE WHEN ? <> '' THEN ? ELSE terminal_error_code END
		WHERE attempt_id = ? AND executor_id = ? AND claim_generation = ?`,
		attemptState, req.RequestedAt, req.RequestedAt, string(terminalCode), string(terminalCode), req.SourceAttemptID, req.SourceExecutorID, req.SourceClaimGeneration); err != nil {
		return fmt.Errorf("record handoff: update source attempt: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE delivery_tasks
		SET state = ?, record_version = record_version + 1, updated_at = ?,
			terminal_error_code = CASE WHEN ? <> '' THEN ? ELSE terminal_error_code END,
			ended_at = CASE WHEN ? <> '' THEN COALESCE(NULLIF(ended_at, ''), ?) ELSE ended_at END
		WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
		taskState, req.RequestedAt, string(terminalCode), string(terminalCode), string(terminalCode), req.RequestedAt,
		req.ProjectID, req.DeliveryRunID, req.TaskID); err != nil {
		return fmt.Errorf("record handoff: update task: %w", err)
	}
	return nil
}

func classifyHandoff(req HandoffRequest) (string, string, FederationErrorCode) {
	if !handoffSideEffectSafe(req.SideEffectState) {
		return HandoffStatusNeedsHuman, "human-review-side-effect-state", ErrHandoffSideEffectUnknownCode
	}
	for _, reason := range req.ReasonCodes {
		switch normalizeHandoffReason(reason) {
		case "quota-confidence-insufficient", "stale-evidence", "unknown-telemetry", "malformed-response", "transport":
			return HandoffStatusNeedsHuman, "human-review-ambiguous-evidence", ErrAmbiguousHandoffEvidenceCode
		}
		if !handoffReasonAllowsTransfer(reason) {
			return HandoffStatusNeedsHuman, "human-review-unsupported-reason", ErrUnsupportedHandoffReasonCode
		}
	}
	return HandoffStatusTransferred, "await-successor-route", ""
}

func handoffReasonAllowsTransfer(reason string) bool {
	switch normalizeHandoffReason(reason) {
	case "quota-exhausted", "rate-limited-429", "budget-exhausted", "open-breaker", "model-unavailable", "provider-outage", "installation-unavailable":
		return true
	default:
		return false
	}
}

func handoffSideEffectSafe(state string) bool {
	switch normalizeHandoffSideEffectState(state) {
	case SideEffectStateNone, SideEffectStateNotStarted, SideEffectStateReadOnly:
		return true
	default:
		return false
	}
}

func normalizeHandoffRequest(req HandoffRequest, now time.Time) HandoffRequest {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	req.ProjectID = strings.TrimSpace(req.ProjectID)
	req.DeliveryRunID = strings.TrimSpace(req.DeliveryRunID)
	req.TaskID = strings.TrimSpace(req.TaskID)
	req.ParentRunID = strings.TrimSpace(req.ParentRunID)
	req.ChildRunID = strings.TrimSpace(req.ChildRunID)
	req.SourceAttemptID = strings.TrimSpace(req.SourceAttemptID)
	req.SourceExecutorID = strings.TrimSpace(req.SourceExecutorID)
	req.TriggerKind = strings.TrimSpace(req.TriggerKind)
	if req.TriggerKind == "" {
		req.TriggerKind = HandoffTriggerQuotaAvailability
	}
	req.ReasonCodes = normalizeHandoffReasonList(req.ReasonCodes)
	req.EvidenceRecordIDs = sortedCopyAgent(req.EvidenceRecordIDs)
	req.TriggerSnapshotJSON = compactJSONString(req.TriggerSnapshotJSON)
	req.PolicyVersion = strings.TrimSpace(req.PolicyVersion)
	if req.PolicyVersion == "" {
		req.PolicyVersion = HandoffPolicyVersion
	}
	req.SideEffectState = normalizeHandoffSideEffectState(req.SideEffectState)
	req.DestinationRoutePlaceholder = strings.TrimSpace(req.DestinationRoutePlaceholder)
	req.DestinationExecutorID = strings.TrimSpace(req.DestinationExecutorID)
	req.DestinationLeaseExpiresAt = strings.TrimSpace(req.DestinationLeaseExpiresAt)
	req.RequestedAt = strings.TrimSpace(req.RequestedAt)
	if req.RequestedAt == "" {
		req.RequestedAt = formatTimestamp(now.UTC())
	}
	return req
}

func validateHandoffRequest(req HandoffRequest) error {
	required := map[string]string{
		"idempotency_key":    req.IdempotencyKey,
		"project_id":         req.ProjectID,
		"delivery_run_id":    req.DeliveryRunID,
		"task_id":            req.TaskID,
		"parent_run_id":      req.ParentRunID,
		"child_run_id":       req.ChildRunID,
		"source_attempt_id":  req.SourceAttemptID,
		"source_executor_id": req.SourceExecutorID,
		"trigger_kind":       req.TriggerKind,
		"policy_version":     req.PolicyVersion,
		"side_effect_state":  req.SideEffectState,
		"requested_at":       req.RequestedAt,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return federationError(ErrHandoffRequiredCode, "%s is required", field)
		}
	}
	if req.SourceClaimGeneration <= 0 {
		return federationError(ErrHandoffRequiredCode, "source_claim_generation must be positive")
	}
	if len(req.ReasonCodes) == 0 {
		return federationError(ErrHandoffRequiredCode, "at least one typed reason code is required")
	}
	if err := requireNonZeroTimestamp(req.RequestedAt, "requested_at"); err != nil {
		return err
	}
	if req.DestinationLeaseExpiresAt != "" {
		if err := requireNonZeroTimestamp(req.DestinationLeaseExpiresAt, "destination_lease_expires_at"); err != nil {
			return err
		}
	}
	if !json.Valid([]byte(req.TriggerSnapshotJSON)) {
		return federationError(ErrInvalidRecordCode, "trigger_snapshot_json is invalid")
	}
	return nil
}

func loadHandoffByIdempotencyTx(ctx context.Context, tx Tx, projectID, deliveryRunID, key string) (HandoffTransaction, bool, error) {
	return loadHandoffTx(ctx, tx, `WHERE project_id = ? AND delivery_run_id = ? AND idempotency_key = ?`, projectID, deliveryRunID, key)
}

func loadHandoffBySourceAttemptTx(ctx context.Context, tx Tx, projectID, deliveryRunID, taskID, sourceAttemptID string) (HandoffTransaction, bool, error) {
	return loadHandoffTx(ctx, tx, `WHERE project_id = ? AND delivery_run_id = ? AND task_id = ? AND source_attempt_id = ?`, projectID, deliveryRunID, taskID, sourceAttemptID)
}

func loadHandoffTx(ctx context.Context, tx Tx, where string, args ...any) (HandoffTransaction, bool, error) {
	query := `SELECT
			handoff_id, schema_version, record_version, project_id, delivery_run_id, task_id, child_run_id, parent_run_id,
			source_attempt_id, source_executor_id, source_claim_generation, source_claim_snapshot_json, trigger_kind,
			reason_codes_json, evidence_record_ids_json, trigger_snapshot_json, policy_version, input_fingerprint,
			policy_fingerprint, plan_fingerprint, authorization_fingerprint, accepted_task_fingerprint, accepted_task_snapshot_json,
			side_effect_state, destination_route_placeholder, destination_executor_id, destination_claim_generation, handoff_generation,
			handoff_status, next_action, request_fingerprint, idempotency_key, created_at, updated_at
		FROM handoff_transactions ` + where + ` ORDER BY created_at, handoff_id LIMIT 1`
	var record HandoffTransaction
	var reasonsJSON, evidenceJSON string
	err := tx.QueryRow(ctx, query, args...).Scan(
		&record.HandoffID, &record.SchemaVersion, &record.RecordVersion, &record.ProjectID, &record.DeliveryRunID, &record.TaskID, &record.ChildRunID, &record.ParentRunID,
		&record.SourceAttemptID, &record.SourceExecutorID, &record.SourceClaimGeneration, &record.SourceClaimSnapshotJSON, &record.TriggerKind,
		&reasonsJSON, &evidenceJSON, &record.TriggerSnapshotJSON, &record.PolicyVersion, &record.InputFingerprint,
		&record.PolicyFingerprint, &record.PlanFingerprint, &record.AuthorizationFingerprint, &record.AcceptedTaskFingerprint, &record.AcceptedTaskSnapshotJSON,
		&record.SideEffectState, &record.DestinationRoutePlaceholder, &record.DestinationExecutorID, &record.DestinationClaimGeneration, &record.HandoffGeneration,
		&record.HandoffStatus, &record.NextAction, &record.RequestFingerprint, &record.IdempotencyKey, &record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return HandoffTransaction{}, false, nil
		}
		return HandoffTransaction{}, false, fmt.Errorf("load handoff transaction: %w", err)
	}
	record.ReasonCodes = decodeStringList(reasonsJSON)
	record.EvidenceRecordIDs = decodeStringList(evidenceJSON)
	return record, true, nil
}

func handoffRequestFingerprint(req HandoffRequest) string {
	return digestJSON(struct {
		SchemaVersion               string   `json:"schema_version"`
		ProjectID                   string   `json:"project_id"`
		DeliveryRunID               string   `json:"delivery_run_id"`
		TaskID                      string   `json:"task_id"`
		ParentRunID                 string   `json:"parent_run_id"`
		ChildRunID                  string   `json:"child_run_id"`
		SourceAttemptID             string   `json:"source_attempt_id"`
		SourceExecutorID            string   `json:"source_executor_id"`
		SourceClaimGeneration       int64    `json:"source_claim_generation"`
		TriggerKind                 string   `json:"trigger_kind"`
		ReasonCodes                 []string `json:"reason_codes"`
		EvidenceRecordIDs           []string `json:"evidence_record_ids"`
		TriggerSnapshotJSON         string   `json:"trigger_snapshot_json"`
		PolicyVersion               string   `json:"policy_version"`
		SideEffectState             string   `json:"side_effect_state"`
		DestinationRoutePlaceholder string   `json:"destination_route_placeholder,omitempty"`
		DestinationExecutorID       string   `json:"destination_executor_id,omitempty"`
	}{
		SchemaVersion:               HandoffTransactionSchema,
		ProjectID:                   req.ProjectID,
		DeliveryRunID:               req.DeliveryRunID,
		TaskID:                      req.TaskID,
		ParentRunID:                 req.ParentRunID,
		ChildRunID:                  req.ChildRunID,
		SourceAttemptID:             req.SourceAttemptID,
		SourceExecutorID:            req.SourceExecutorID,
		SourceClaimGeneration:       req.SourceClaimGeneration,
		TriggerKind:                 req.TriggerKind,
		ReasonCodes:                 sortedCopyAgent(req.ReasonCodes),
		EvidenceRecordIDs:           sortedCopyAgent(req.EvidenceRecordIDs),
		TriggerSnapshotJSON:         req.TriggerSnapshotJSON,
		PolicyVersion:               req.PolicyVersion,
		SideEffectState:             req.SideEffectState,
		DestinationRoutePlaceholder: req.DestinationRoutePlaceholder,
		DestinationExecutorID:       req.DestinationExecutorID,
	})
}

func normalizeHandoffReasonList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = normalizeHandoffReason(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalizeHandoffReason(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeHandoffSideEffectState(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "no-side-effects":
		return SideEffectStateNone
	case "not-started", "not_started", "no-provider-launch", "provider-not-started":
		return SideEffectStateNotStarted
	case "read-only", "readonly":
		return SideEffectStateReadOnly
	case "provider-started", "launched", "started":
		return SideEffectStateProviderStart
	case "ambiguous":
		return SideEffectStateAmbiguous
	default:
		return firstNonEmptyAgent(value, SideEffectStateUnknown)
	}
}

func compactJSONString(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}"
	}
	var payload any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return raw
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return string(out)
}

func compactJSON(value any) (string, error) {
	out, err := json.Marshal(value)
	if err != nil {
		return "", federationError(ErrInvalidRecordCode, "marshal handoff payload: %v", err)
	}
	return string(out), nil
}

func asHandoffReplayConflict(err error, target *handoffReplayConflict) bool {
	if err == nil {
		return false
	}
	if value, ok := err.(handoffReplayConflict); ok {
		*target = value
		return true
	}
	return false
}

func requireRowsAffected(ctx context.Context, tx Tx, runID, executorID string, claimGeneration int64) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_claims WHERE run_id = ? AND executor_id = ? AND claim_generation = ?`, runID, executorID, claimGeneration).Scan(&count); err != nil {
		return fmt.Errorf("record handoff: verify fenced claim: %w", err)
	}
	if count != 1 {
		return federationError(ErrStaleClaimCode, "source owner/generation no longer owns run %s", runID)
	}
	return nil
}
