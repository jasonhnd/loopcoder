package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	SideEffectStateIdempotent    = "idempotent"
	SideEffectStateReceiptBacked = "receipt-backed"
	SideEffectStateUnknown       = "unknown"
	SideEffectStateAmbiguous     = "ambiguous"
	SideEffectStateIrreversible  = "irreversible"
	SideEffectStateProviderStart = "provider-started"
)

const (
	HandoffSideEffectReconciliationSchema = "loopcoder.handoff_side_effect_reconciliation.v1"
	HandoffContinuationProofSchema        = "loopcoder.handoff_continuation_proof.v1"

	HandoffSideEffectCodeNotStarted        = "handoff.side_effect.not_started"
	HandoffSideEffectCodeReadOnly          = "handoff.side_effect.read_only"
	HandoffSideEffectCodeIdempotentExact   = "handoff.side_effect.idempotent_exact_key"
	HandoffSideEffectCodeReceiptBacked     = "handoff.side_effect.receipt_backed_exact"
	HandoffSideEffectCodeAmbiguous         = "handoff.side_effect.ambiguous"
	HandoffSideEffectCodeIrreversible      = "handoff.side_effect.irreversible"
	HandoffSideEffectCodeFingerprintDrift  = "handoff.side_effect.fingerprint_drift"
	HandoffSideEffectCodeMissingDurability = "handoff.side_effect.missing_durable_row"
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

type HandoffSideEffectReconciliation struct {
	SchemaVersion              string   `json:"schema_version"`
	Code                       string   `json:"code"`
	State                      string   `json:"state"`
	SourceAttemptID            string   `json:"source_attempt_id"`
	SourceExecutorID           string   `json:"source_executor_id"`
	SourceClaimGeneration      int64    `json:"source_claim_generation"`
	HandoffID                  string   `json:"handoff_id,omitempty"`
	HandoffGeneration          int64    `json:"handoff_generation,omitempty"`
	OwnershipFence             string   `json:"ownership_fence"`
	ExecutionOutcome           string   `json:"execution_outcome"`
	ProviderIdempotencyKeyHash string   `json:"provider_idempotency_key_hash,omitempty"`
	ProviderReceiptHash        string   `json:"provider_receipt_hash,omitempty"`
	BudgetReservationIDs       []string `json:"budget_reservation_ids,omitempty"`
	EvidenceRecordIDs          []string `json:"evidence_record_ids,omitempty"`
	AutomaticContinuation      bool     `json:"automatic_continuation"`
	NeedsHuman                 bool     `json:"needs_human"`
	Reason                     string   `json:"reason"`
}

type HandoffContinuationProof struct {
	SchemaVersion              string   `json:"schema_version"`
	Code                       string   `json:"code"`
	State                      string   `json:"state"`
	SourceAttemptID            string   `json:"source_attempt_id"`
	SourceExecutorID           string   `json:"source_executor_id"`
	SourceClaimGeneration      int64    `json:"source_claim_generation"`
	HandoffID                  string   `json:"handoff_id"`
	HandoffGeneration          int64    `json:"handoff_generation"`
	OwnershipFence             string   `json:"ownership_fence"`
	ExecutionOutcome           string   `json:"execution_outcome"`
	ProviderIdempotencyKeyHash string   `json:"provider_idempotency_key_hash,omitempty"`
	ProviderReceiptHash        string   `json:"provider_receipt_hash,omitempty"`
	EvidenceRecordIDs          []string `json:"evidence_record_ids,omitempty"`
	DecisionRef                string   `json:"decision_ref"`
	CompletedBoundary          string   `json:"completed_boundary"`
	CheckpointID               string   `json:"checkpoint_id"`
	ProofFingerprint           string   `json:"proof_fingerprint"`
	AutomaticContinuation      bool     `json:"automatic_continuation"`
}

type HandoffSuccessorCandidate struct {
	RoutingDecisionID      string
	RoutingCandidateID     string
	AdapterID              string
	ProviderInstallationID string
	AccountProfileID       string
	ModelCapabilityID      string
}

type HandoffSuccessorRequest struct {
	HandoffID           string
	HandoffGeneration   int64
	SourceAttemptID     string
	RoutingDecisionID   string
	RoutingFingerprint  string
	BudgetReservationID string
	BudgetPolicyIDs     []string
	ReusableEvidenceIDs []string
	ContinuationProof   HandoffContinuationProof
	Candidate           HandoffSuccessorCandidate
	Cancelled           bool
	CreatedAt           string
	ActorJSON           string
	HostJSON            string
}

type HandoffOwnershipLockSnapshot struct {
	LockID         string `json:"lock_id"`
	LockGeneration int64  `json:"lock_generation"`
}

type HandoffSuccessorLaunch struct {
	HandoffID              string                         `json:"handoff_id"`
	HandoffGeneration      int64                          `json:"handoff_generation"`
	AttemptID              string                         `json:"attempt_id"`
	SourceAttemptID        string                         `json:"source_attempt_id"`
	TaskID                 string                         `json:"task_id"`
	DeliveryRunID          string                         `json:"delivery_run_id"`
	ProjectID              string                         `json:"project_id"`
	ExecutorID             string                         `json:"executor_id"`
	ClaimGeneration        int64                          `json:"claim_generation"`
	ProviderIdempotencyKey string                         `json:"provider_idempotency_key"`
	RoutingDecisionID      string                         `json:"routing_decision_id"`
	RoutingCandidateID     string                         `json:"routing_candidate_id"`
	BudgetReservationID    string                         `json:"budget_reservation_id"`
	ReusableEvidenceIDs    []string                       `json:"reusable_evidence_ids"`
	ContinuationProof      HandoffContinuationProof       `json:"continuation_proof"`
	OwnershipLocks         []HandoffOwnershipLockSnapshot `json:"ownership_locks,omitempty"`
	LaunchExposed          bool                           `json:"launch_exposed"`
	LaunchPhase            string                         `json:"launch_phase,omitempty"`
	ProviderReceipt        string                         `json:"provider_receipt,omitempty"`
	LeaseExpiresAt         string                         `json:"lease_expires_at,omitempty"`
	Replay                 bool                           `json:"replay"`
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

func LoadHandoffTransaction(ctx context.Context, store Store, handoffID string) (HandoffTransaction, error) {
	if store == nil {
		return HandoffTransaction{}, federationError(ErrHandoffRequiredCode, "store is required")
	}
	handoffID = strings.TrimSpace(handoffID)
	if handoffID == "" {
		return HandoffTransaction{}, federationError(ErrHandoffRequiredCode, "handoff_id is required")
	}
	var out HandoffTransaction
	err := store.WithTx(ctx, func(tx Tx) error {
		record, ok, err := loadHandoffTx(ctx, tx, `WHERE handoff_id = ?`, handoffID)
		if err != nil {
			return err
		}
		if !ok {
			return federationError(ErrHandoffRequiredCode, "handoff %s is missing", handoffID)
		}
		out = record
		return nil
	})
	return out, err
}

func ReconcileHandoffSideEffect(ctx context.Context, store Store, handoff HandoffTransaction) (HandoffSideEffectReconciliation, error) {
	if store == nil {
		return HandoffSideEffectReconciliation{}, federationError(ErrHandoffRequiredCode, "store is required")
	}
	var out HandoffSideEffectReconciliation
	err := withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			current, ok, err := loadHandoffTx(ctx, tx, `WHERE handoff_id = ?`, handoff.HandoffID)
			if err != nil {
				return err
			}
			if !ok {
				return federationError(ErrHandoffRequiredCode, "handoff %s is missing", handoff.HandoffID)
			}
			if handoff.RequestFingerprint != "" && current.RequestFingerprint != handoff.RequestFingerprint {
				return federationError(ErrHandoffReplayMismatchCode, "handoff request fingerprint changed before reconciliation")
			}
			rec, err := reconcileHandoffSideEffectTx(ctx, tx, current)
			if err != nil {
				return err
			}
			if err := insertHandoffSideEffectDecisionTx(ctx, tx, current, rec, store.Now()); err != nil {
				return err
			}
			out = rec
			return nil
		})
	})
	return out, err
}

func MarkHandoffSideEffectNeedsHuman(ctx context.Context, store Store, handoff HandoffTransaction, rec HandoffSideEffectReconciliation) error {
	if store == nil {
		return federationError(ErrHandoffRequiredCode, "store is required")
	}
	return withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			current, ok, err := loadHandoffTx(ctx, tx, `WHERE handoff_id = ?`, handoff.HandoffID)
			if err != nil {
				return err
			}
			if !ok {
				return federationError(ErrHandoffRequiredCode, "handoff %s is missing", handoff.HandoffID)
			}
			if current.RequestFingerprint != handoff.RequestFingerprint {
				return federationError(ErrHandoffReplayMismatchCode, "handoff request fingerprint changed before needs-human reconciliation")
			}
			at := formatTimestamp(store.Now().UTC())
			rec.AutomaticContinuation = false
			rec.NeedsHuman = true
			if strings.TrimSpace(rec.Reason) == "" {
				rec.Reason = "handoff side-effect reconciliation requires human review"
			}
			if err := insertHandoffSideEffectDecisionTx(ctx, tx, current, rec, store.Now()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE handoff_transactions
				SET handoff_status = ?, next_action = ?, updated_at = ?
				WHERE handoff_id = ? AND request_fingerprint = ?`,
				HandoffStatusNeedsHuman, "human-review-side-effect-reconciliation", at, current.HandoffID, current.RequestFingerprint); err != nil {
				return fmt.Errorf("mark handoff side-effect needs-human: %w", err)
			}
			if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
				RunID:       current.ChildRunID,
				ParentRunID: current.ParentRunID,
				ChildRunID:  current.ChildRunID,
				Status:      HandoffStatusNeedsHuman,
				UpdatedAt:   at,
				Reason:      rec.Code,
				Source:      "handoff-side-effect-reconciliation",
			}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE delivery_tasks
				SET state = ?, ended_at = COALESCE(NULLIF(ended_at, ''), ?), updated_at = ?,
					terminal_error_code = ?, error_message = ?
				WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
				HandoffStatusNeedsHuman, at, at, ErrHandoffSideEffectUnknownCode, rec.Code, current.ProjectID, current.DeliveryRunID, current.TaskID); err != nil {
				return fmt.Errorf("mark handoff task needs-human: %w", err)
			}
			return nil
		})
	})
}

func BuildHandoffContinuationProof(handoff HandoffTransaction, rec HandoffSideEffectReconciliation) (HandoffContinuationProof, error) {
	rec.State = normalizeHandoffSideEffectState(rec.State)
	if strings.TrimSpace(rec.Code) == "" {
		rec.Code = handoffSideEffectCode(rec.State)
	}
	proof := HandoffContinuationProof{
		SchemaVersion:              HandoffContinuationProofSchema,
		Code:                       strings.TrimSpace(rec.Code),
		State:                      rec.State,
		SourceAttemptID:            strings.TrimSpace(firstNonEmptyAgent(rec.SourceAttemptID, handoff.SourceAttemptID)),
		SourceExecutorID:           strings.TrimSpace(firstNonEmptyAgent(rec.SourceExecutorID, handoff.SourceExecutorID)),
		SourceClaimGeneration:      firstPositiveAgent(rec.SourceClaimGeneration, handoff.SourceClaimGeneration),
		HandoffID:                  handoff.HandoffID,
		HandoffGeneration:          handoff.HandoffGeneration,
		OwnershipFence:             strings.TrimSpace(rec.OwnershipFence),
		ExecutionOutcome:           strings.TrimSpace(rec.ExecutionOutcome),
		ProviderIdempotencyKeyHash: strings.TrimSpace(rec.ProviderIdempotencyKeyHash),
		ProviderReceiptHash:        strings.TrimSpace(rec.ProviderReceiptHash),
		EvidenceRecordIDs:          sortedCopyAgent(firstNonEmptySlice(rec.EvidenceRecordIDs, handoff.EvidenceRecordIDs)),
		DecisionRef:                "handoff-side-effect:" + handoff.HandoffID,
		CompletedBoundary:          handoffContinuationBoundary(rec),
		AutomaticContinuation:      rec.AutomaticContinuation && !rec.NeedsHuman,
	}
	proof.CheckpointID = handoffContinuationCheckpointID(proof)
	proof.ProofFingerprint = handoffContinuationProofFingerprint(proof)
	if err := validateHandoffContinuationProof(handoff, rec, proof); err != nil {
		return HandoffContinuationProof{}, err
	}
	return proof, nil
}

func PrepareHandoffSuccessorLaunch(ctx context.Context, store Store, req HandoffSuccessorRequest) (HandoffSuccessorLaunch, error) {
	if store == nil {
		return HandoffSuccessorLaunch{}, federationError(ErrHandoffRequiredCode, "store is required")
	}
	req = normalizeHandoffSuccessorRequest(req, store.Now())
	if err := validateHandoffSuccessorRequest(req); err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	var out HandoffSuccessorLaunch
	err := withRetry(ctx, func() error {
		return store.WithWriteTx(ctx, func(tx Tx) error {
			launch, err := prepareHandoffSuccessorLaunchTx(ctx, tx, req)
			if err != nil {
				return err
			}
			out = launch
			return nil
		})
	})
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

	sideEffectRecord, err := deriveHandoffSideEffectRecordTx(ctx, tx, req, task, claim)
	if err != nil {
		return HandoffTransaction{}, err
	}
	effectiveSideEffectState := sideEffectRecord.State
	status, nextAction, terminalCode := classifyHandoff(req, effectiveSideEffectState)
	requestedAt, err := time.Parse(time.RFC3339Nano, req.RequestedAt)
	if err != nil {
		return HandoffTransaction{}, federationError(ErrInvalidRecordCode, "requested_at is invalid")
	}
	if status == HandoffStatusTransferred {
		status, nextAction, terminalCode, err = validateHandoffTransferAuthorityTx(ctx, tx, req, requestedAt.UTC())
		if err != nil {
			return HandoffTransaction{}, err
		}
	}
	destinationGeneration := claim.ClaimGeneration + 1
	destinationOwner := strings.TrimSpace(req.DestinationExecutorID)
	if destinationOwner == "" {
		destinationOwner = stableID("handoff_", req.ProjectID, req.DeliveryRunID, req.TaskID, fmt.Sprint(destinationGeneration))
	}
	lease := strings.TrimSpace(req.DestinationLeaseExpiresAt)
	if lease == "" {
		lease = formatTimestamp(requestedAt.UTC().Add(15 * time.Minute))
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
		"run_id":                     claim.RunID,
		"executor_id":                claim.ExecutorID,
		"claim_generation":           claim.ClaimGeneration,
		"claimed_at":                 claim.ClaimedAt,
		"lease_expires_at":           claim.LeaseExpiresAt,
		"heartbeat_at":               claim.HeartbeatAt,
		"claim_phase":                claim.ClaimPhase,
		"provider_idempotency_key":   claim.ProviderKey,
		"provider_receipt":           claim.ProviderReceipt,
		"effective_side_effect":      effectiveSideEffectState,
		"requested_side_effect":      req.SideEffectState,
		"side_effect_reconciliation": sideEffectRecord,
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
		SideEffectState:             effectiveSideEffectState,
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
	if _, err := tx.Exec(ctx, `UPDATE child_execution_requests
		SET claim_generation = ?, lifecycle_status = ?, updated_at = ?
		WHERE child_run_id = ? AND legacy_ambiguous = 0 AND claim_generation IN (0, ?)`,
		record.DestinationClaimGeneration, ClaimPhaseClaimed, req.RequestedAt, req.ChildRunID, req.SourceClaimGeneration); err != nil {
		return HandoffTransaction{}, fmt.Errorf("record handoff: transfer child execution request fence: %w", err)
	}
	if err := reconcileHandoffAgentAuthorityTx(ctx, tx, req, record, status, terminalCode, lease); err != nil {
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
	if err := updateDeliveryAfterHandoffTx(ctx, tx, req, status, terminalCode, claim.ProviderReceipt); err != nil {
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
	taskState                string
	taskPermission           string
	taskSideEffect           string
	attemptState             string
	attemptProviderKey       string
	attemptProviderReceipt   string
	attemptSideEffect        string
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
	var attemptTask, attemptRun, attemptProject, attemptExecutor, attemptState, attemptProviderKey, attemptProviderReceipt, attemptSideEffect string
	var attemptGeneration int64
	if err := tx.QueryRow(ctx, `SELECT task_id, delivery_run_id, project_id, executor_id, claim_generation, state, provider_idempotency_key, provider_receipt, side_effect_class
		FROM delivery_attempts WHERE attempt_id = ?`, req.SourceAttemptID).Scan(&attemptTask, &attemptRun, &attemptProject, &attemptExecutor, &attemptGeneration, &attemptState, &attemptProviderKey, &attemptProviderReceipt, &attemptSideEffect); err != nil {
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
		taskState:                strings.ToLower(strings.TrimSpace(taskState)),
		taskPermission:           normalizePermission(taskPermission),
		taskSideEffect:           normalizeSideEffectClass(taskSideEffect),
		attemptState:             strings.ToLower(strings.TrimSpace(attemptState)),
		attemptProviderKey:       strings.TrimSpace(attemptProviderKey),
		attemptProviderReceipt:   strings.TrimSpace(attemptProviderReceipt),
		attemptSideEffect:        normalizeSideEffectClass(attemptSideEffect),
	}, nil
}

func deriveHandoffSideEffectRecordTx(ctx context.Context, tx Tx, req HandoffRequest, task handoffTaskSnapshot, claim ClaimResult) (HandoffSideEffectReconciliation, error) {
	base := HandoffSideEffectReconciliation{
		SchemaVersion:              HandoffSideEffectReconciliationSchema,
		SourceAttemptID:            req.SourceAttemptID,
		SourceExecutorID:           req.SourceExecutorID,
		SourceClaimGeneration:      req.SourceClaimGeneration,
		OwnershipFence:             "source-owner",
		ExecutionOutcome:           normalizeClaimPhase(claim.ClaimPhase),
		ProviderIdempotencyKeyHash: hashSecretEvidence(firstNonEmptyAgent(task.attemptProviderKey, claim.ProviderKey)),
		ProviderReceiptHash:        hashSecretEvidence(firstNonEmptyAgent(task.attemptProviderReceipt, claim.ProviderReceipt)),
		BudgetReservationIDs:       nil,
		EvidenceRecordIDs:          sortedCopyAgent(req.EvidenceRecordIDs),
	}
	record := func(state, code, reason string, automatic bool) HandoffSideEffectReconciliation {
		next := base
		next.State = state
		next.Code = code
		next.Reason = reason
		next.AutomaticContinuation = automatic
		next.NeedsHuman = !automatic
		return next
	}
	requested := normalizeHandoffSideEffectState(req.SideEffectState)
	if !handoffSideEffectSafe(requested) {
		return record(requested, handoffSideEffectCode(requested), "caller reported unsafe side-effect state", false), nil
	}
	if strings.TrimSpace(claim.ProviderReceipt) != "" || strings.TrimSpace(task.attemptProviderReceipt) != "" {
		return record(SideEffectStateReceiptBacked, HandoffSideEffectCodeReceiptBacked, "source effect has an exact durable provider receipt", true), nil
	}
	if registration, ok, err := loadAgentRegistrationByRunTx(ctx, tx, req.ChildRunID); err != nil {
		return HandoffSideEffectReconciliation{}, err
	} else if ok {
		if strings.TrimSpace(registration.ProviderReceipt) != "" {
			return record(SideEffectStateAmbiguous, HandoffSideEffectCodeAmbiguous, "agent registration already contains provider receipt state", false), nil
		}
		switch normalizeAgentRegistrationState(registration.RegistrationState) {
		case AgentStateRegistered, AgentStatePlanned:
		case AgentStateLaunching, AgentStateRunning, AgentStateCancelling:
			if handoffSourceAttemptHasExactKey(task, claim) && !handoffSourceEffectIrreversible(task) {
				return record(SideEffectStateIdempotent, HandoffSideEffectCodeIdempotentExact, "source effect can be reconciled under the exact durable idempotency key", true), nil
			}
			return record(handoffCrashWindowState(task), handoffCrashWindowCode(task), "source effect entered a provider crash window without exact reusable proof", false), nil
		case AgentStateSucceeded, AgentStateFailed, AgentStateCancelled:
			if handoffSourceAttemptHasExactKey(task, claim) && !handoffSourceEffectIrreversible(task) {
				return record(SideEffectStateIdempotent, HandoffSideEffectCodeIdempotentExact, "source terminal effect can be reconciled under the exact durable idempotency key", true), nil
			}
			return record(handoffCrashWindowState(task), handoffCrashWindowCode(task), "source terminal effect has no exact reusable receipt or key", false), nil
		case AgentStateNeedsHuman, AgentStateSuperseded:
			return record(SideEffectStateAmbiguous, HandoffSideEffectCodeAmbiguous, "source registration requires human reconciliation", false), nil
		default:
			return record(SideEffectStateAmbiguous, HandoffSideEffectCodeAmbiguous, "source registration state is unknown", false), nil
		}
	}
	switch normalizeClaimPhase(claim.ClaimPhase) {
	case ClaimPhaseClaimed:
	case ClaimPhaseLaunching, ClaimPhaseExecuting, ClaimPhaseCompleted:
		if handoffSourceAttemptHasExactKey(task, claim) && !handoffSourceEffectIrreversible(task) {
			return record(SideEffectStateIdempotent, HandoffSideEffectCodeIdempotentExact, "source claim can be reconciled under the exact durable idempotency key", true), nil
		}
		return record(handoffCrashWindowState(task), handoffCrashWindowCode(task), "source claim entered a provider crash window without exact reusable proof", false), nil
	default:
		return record(SideEffectStateAmbiguous, HandoffSideEffectCodeAmbiguous, "source claim phase is unknown", false), nil
	}
	if handoffDeliveryStateImpliesSideEffect(task.attemptState) || handoffDeliveryStateImpliesSideEffect(task.taskState) {
		if handoffSourceAttemptHasExactKey(task, claim) && !handoffSourceEffectIrreversible(task) {
			return record(SideEffectStateIdempotent, HandoffSideEffectCodeIdempotentExact, "source delivery state can be reconciled under the exact durable idempotency key", true), nil
		}
		return record(handoffCrashWindowState(task), handoffCrashWindowCode(task), "source delivery state implies execution without exact reusable proof", false), nil
	}
	if task.taskPermission == PermissionReadOnly && sideEffectRank(task.taskSideEffect) <= sideEffectRank(SideEffectLocalRead) && sideEffectRank(task.attemptSideEffect) <= sideEffectRank(SideEffectLocalRead) {
		return record(SideEffectStateReadOnly, HandoffSideEffectCodeReadOnly, "accepted task is read-only", true), nil
	}
	return record(SideEffectStateNotStarted, HandoffSideEffectCodeNotStarted, "source attempt has not reached a side-effect boundary", true), nil
}

func handoffSourceAttemptHasExactKey(task handoffTaskSnapshot, claim ClaimResult) bool {
	attemptKey := strings.TrimSpace(task.attemptProviderKey)
	claimKey := strings.TrimSpace(claim.ProviderKey)
	return attemptKey != "" && claimKey != "" && attemptKey == claimKey
}

func handoffSourceEffectIrreversible(task handoffTaskSnapshot) bool {
	return sideEffectRank(task.taskSideEffect) >= sideEffectRank(SideEffectExternalWrite) ||
		sideEffectRank(task.attemptSideEffect) >= sideEffectRank(SideEffectExternalWrite)
}

func handoffCrashWindowState(task handoffTaskSnapshot) string {
	if handoffSourceEffectIrreversible(task) {
		return SideEffectStateIrreversible
	}
	return SideEffectStateAmbiguous
}

func handoffCrashWindowCode(task handoffTaskSnapshot) string {
	if handoffSourceEffectIrreversible(task) {
		return HandoffSideEffectCodeIrreversible
	}
	return HandoffSideEffectCodeAmbiguous
}

func handoffDeliveryStateImpliesSideEffect(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "succeeded", "failed", "cancelled", "timed_out", "abandoned":
		return true
	default:
		return false
	}
}

func reconcileHandoffSideEffectTx(ctx context.Context, tx Tx, handoff HandoffTransaction) (HandoffSideEffectReconciliation, error) {
	snapshot, err := decodeHandoffSideEffectSnapshot(handoff)
	if err != nil {
		return HandoffSideEffectReconciliation{}, err
	}
	if snapshot.SourceAttemptID != "" && snapshot.SourceAttemptID != handoff.SourceAttemptID {
		return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "handoff source attempt snapshot changed")
	}
	if snapshot.SourceExecutorID != "" && snapshot.SourceExecutorID != handoff.SourceExecutorID {
		return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "handoff source executor snapshot changed")
	}
	if snapshot.SourceClaimGeneration > 0 && snapshot.SourceClaimGeneration != handoff.SourceClaimGeneration {
		return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "handoff source generation snapshot changed")
	}
	var attemptState, attemptProviderKey, attemptReceipt, attemptSideEffect string
	err = tx.QueryRow(ctx, `SELECT state, provider_idempotency_key, provider_receipt, side_effect_class
		FROM delivery_attempts WHERE attempt_id = ? AND project_id = ? AND delivery_run_id = ? AND task_id = ?`,
		handoff.SourceAttemptID, handoff.ProjectID, handoff.DeliveryRunID, handoff.TaskID).Scan(&attemptState, &attemptProviderKey, &attemptReceipt, &attemptSideEffect)
	if errors.Is(err, sql.ErrNoRows) {
		rec := snapshot
		rec.Code = HandoffSideEffectCodeMissingDurability
		rec.State = SideEffectStateAmbiguous
		rec.AutomaticContinuation = false
		rec.NeedsHuman = true
		rec.Reason = "source attempt row is missing; safety cannot be inferred from absence"
		rec.HandoffID = handoff.HandoffID
		rec.HandoffGeneration = handoff.HandoffGeneration
		return rec, nil
	}
	if err != nil {
		return HandoffSideEffectReconciliation{}, fmt.Errorf("reconcile handoff source attempt: %w", err)
	}
	var claimExecutor, claimPhase, claimProviderKey, claimReceipt string
	var claimGeneration int64
	err = tx.QueryRow(ctx, `SELECT executor_id, claim_generation, phase, provider_idempotency_key, provider_receipt
		FROM run_claims WHERE run_id = ?`, handoff.ChildRunID).Scan(&claimExecutor, &claimGeneration, &claimPhase, &claimProviderKey, &claimReceipt)
	if errors.Is(err, sql.ErrNoRows) {
		rec := snapshot
		rec.Code = HandoffSideEffectCodeMissingDurability
		rec.State = SideEffectStateAmbiguous
		rec.AutomaticContinuation = false
		rec.NeedsHuman = true
		rec.Reason = "handoff ownership fence row is missing"
		rec.HandoffID = handoff.HandoffID
		rec.HandoffGeneration = handoff.HandoffGeneration
		return rec, nil
	}
	if err != nil {
		return HandoffSideEffectReconciliation{}, fmt.Errorf("reconcile handoff claim: %w", err)
	}
	if claimExecutor != handoff.DestinationExecutorID || claimGeneration != handoff.HandoffGeneration {
		return HandoffSideEffectReconciliation{}, federationError(ErrStaleClaimCode, "handoff ownership fence changed before side-effect reconciliation")
	}
	rec := snapshot
	rec.HandoffID = handoff.HandoffID
	rec.HandoffGeneration = handoff.HandoffGeneration
	rec.OwnershipFence = "destination-owner"
	rec.ExecutionOutcome = strings.ToLower(strings.TrimSpace(firstNonEmptyAgent(attemptState, claimPhase)))
	rec.BudgetReservationIDs = handoffSourceBudgetReservationIDsTx(ctx, tx, handoff.ChildRunID)
	if rec.State == "" {
		rec.State = normalizeHandoffSideEffectState(handoff.SideEffectState)
		rec.Code = handoffSideEffectCode(rec.State)
		rec.AutomaticContinuation = handoffSideEffectSafe(rec.State)
		rec.NeedsHuman = !rec.AutomaticContinuation
	}
	if normalizeHandoffSideEffectState(handoff.SideEffectState) != normalizeHandoffSideEffectState(rec.State) {
		return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "handoff side-effect state differs from reconciliation snapshot")
	}
	if !handoffSideEffectSafe(rec.State) {
		rec.AutomaticContinuation = false
		rec.NeedsHuman = true
		if rec.Reason == "" {
			rec.Reason = "source side-effect state requires human reconciliation"
		}
		return rec, nil
	}
	switch normalizeHandoffSideEffectState(rec.State) {
	case SideEffectStateReceiptBacked:
		if rec.ProviderReceiptHash == "" {
			return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "receipt-backed handoff has no durable receipt hash")
		}
		if hashSecretEvidence(attemptReceipt) != rec.ProviderReceiptHash {
			return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "receipt-backed handoff provider receipt no longer matches durable proof")
		}
	case SideEffectStateIdempotent:
		if rec.ProviderIdempotencyKeyHash == "" {
			return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "idempotent handoff has no durable idempotency key hash")
		}
		if hashSecretEvidence(attemptProviderKey) != rec.ProviderIdempotencyKeyHash {
			return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "idempotent handoff key no longer matches durable proof")
		}
	case SideEffectStateNone, SideEffectStateNotStarted, SideEffectStateReadOnly:
		if strings.TrimSpace(attemptReceipt) != "" {
			return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "non-executed handoff gained a provider receipt before successor launch")
		}
		if normalizeSideEffectClass(attemptSideEffect) == SideEffectExternalWrite && normalizeHandoffSideEffectState(rec.State) == SideEffectStateNotStarted {
			return HandoffSideEffectReconciliation{}, federationError(ErrHandoffReplayMismatchCode, "not-started handoff source attempt became irreversible")
		}
	}
	rec.AutomaticContinuation = true
	rec.NeedsHuman = false
	if rec.Reason == "" {
		rec.Reason = "source side-effect reconciliation permits automatic continuation"
	}
	return rec, nil
}

func decodeHandoffSideEffectSnapshot(handoff HandoffTransaction) (HandoffSideEffectReconciliation, error) {
	var payload struct {
		ProviderIDempotencyKey   string                          `json:"provider_idempotency_key"`
		ProviderReceipt          string                          `json:"provider_receipt"`
		EffectiveSideEffect      string                          `json:"effective_side_effect"`
		SideEffectReconciliation HandoffSideEffectReconciliation `json:"side_effect_reconciliation"`
	}
	if err := json.Unmarshal([]byte(handoff.SourceClaimSnapshotJSON), &payload); err != nil {
		return HandoffSideEffectReconciliation{}, federationError(ErrInvalidRecordCode, "handoff source claim snapshot is invalid")
	}
	rec := payload.SideEffectReconciliation
	if rec.SchemaVersion == "" {
		rec.SchemaVersion = HandoffSideEffectReconciliationSchema
		rec.SourceAttemptID = handoff.SourceAttemptID
		rec.SourceExecutorID = handoff.SourceExecutorID
		rec.SourceClaimGeneration = handoff.SourceClaimGeneration
		rec.State = normalizeHandoffSideEffectState(firstNonEmptyAgent(payload.EffectiveSideEffect, handoff.SideEffectState))
		rec.Code = handoffSideEffectCode(rec.State)
		rec.ProviderIdempotencyKeyHash = hashSecretEvidence(payload.ProviderIDempotencyKey)
		rec.ProviderReceiptHash = hashSecretEvidence(payload.ProviderReceipt)
		rec.EvidenceRecordIDs = sortedCopyAgent(handoff.EvidenceRecordIDs)
		rec.AutomaticContinuation = handoffSideEffectSafe(rec.State)
		rec.NeedsHuman = !rec.AutomaticContinuation
	}
	rec.State = normalizeHandoffSideEffectState(rec.State)
	rec.Code = firstNonEmptyAgent(rec.Code, handoffSideEffectCode(rec.State))
	rec.EvidenceRecordIDs = sortedCopyAgent(firstNonEmptySlice(rec.EvidenceRecordIDs, handoff.EvidenceRecordIDs))
	return rec, nil
}

func handoffSourceBudgetReservationIDsTx(ctx context.Context, tx Tx, childRunID string) []string {
	rows, err := tx.Query(ctx, `SELECT abb.budget_reservation_id
		FROM agent_registrations ar
		JOIN agent_budget_bindings abb ON abb.child_agent_id = ar.id
		WHERE ar.child_run_id = ?
		ORDER BY abb.budget_reservation_id`, childRunID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && strings.TrimSpace(id) != "" {
			ids = append(ids, id)
		}
	}
	return sortedCopyAgent(ids)
}

func insertHandoffSideEffectDecisionTx(ctx context.Context, tx Tx, handoff HandoffTransaction, rec HandoffSideEffectReconciliation, now time.Time) error {
	output, err := compactJSON(rec)
	if err != nil {
		return err
	}
	at := formatTimestamp(now.UTC())
	decisionID := stableID("ddec_hse_", handoff.HandoffID, rec.Code, rec.State)
	_, err = tx.Exec(ctx, `INSERT INTO delivery_decisions(
			decision_id, schema_version, record_version, project_id, delivery_run_id, task_id, decision_key, decision_kind,
			decided_by_json, inputs_fingerprint, output_json, alternatives_json, heuristic, heuristic_reason, policy_version,
			side_effect_class, created_at, created_by_json, host_json)
		VALUES (?, 'loopcoder.delivery_decision.v1', 1, ?, ?, ?, ?, 'handoff-side-effect-reconciliation', '{}', ?, ?, 'null', 0, ?, ?, ?, ?, '{}', '{}')
		ON CONFLICT(project_id, delivery_run_id, decision_key) DO NOTHING`,
		decisionID, handoff.ProjectID, handoff.DeliveryRunID, handoff.TaskID, "handoff-side-effect:"+handoff.HandoffID,
		digestJSON(map[string]any{"handoff_id": handoff.HandoffID, "request_fingerprint": handoff.RequestFingerprint, "code": rec.Code}),
		output, rec.Reason, HandoffPolicyVersion, rec.State, at)
	return err
}

func loadHandoffSideEffectDecisionTx(ctx context.Context, tx Tx, handoff HandoffTransaction) (HandoffSideEffectReconciliation, bool, error) {
	var raw string
	err := tx.QueryRow(ctx, `SELECT output_json FROM delivery_decisions
		WHERE project_id = ? AND delivery_run_id = ? AND decision_key = ? AND decision_kind = 'handoff-side-effect-reconciliation'`,
		handoff.ProjectID, handoff.DeliveryRunID, "handoff-side-effect:"+handoff.HandoffID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return HandoffSideEffectReconciliation{}, false, nil
	}
	if err != nil {
		return HandoffSideEffectReconciliation{}, false, err
	}
	var rec HandoffSideEffectReconciliation
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return HandoffSideEffectReconciliation{}, false, federationError(ErrInvalidRecordCode, "handoff side-effect decision payload is invalid")
	}
	rec.State = normalizeHandoffSideEffectState(rec.State)
	rec.Code = firstNonEmptyAgent(rec.Code, handoffSideEffectCode(rec.State))
	rec.EvidenceRecordIDs = sortedCopyAgent(rec.EvidenceRecordIDs)
	return rec, true, nil
}

func handoffContinuationBoundary(rec HandoffSideEffectReconciliation) string {
	switch normalizeHandoffSideEffectState(rec.State) {
	case SideEffectStateReceiptBacked:
		return "source-boundary:provider-receipt"
	case SideEffectStateIdempotent:
		return "source-boundary:idempotency-key"
	case SideEffectStateReadOnly:
		return "source-boundary:read-only"
	case SideEffectStateNotStarted, SideEffectStateNone:
		return "source-boundary:not-started"
	default:
		return "source-boundary:" + normalizeHandoffSideEffectState(rec.State)
	}
}

func handoffContinuationCheckpointID(proof HandoffContinuationProof) string {
	return stableID("hcp_",
		proof.HandoffID,
		fmt.Sprint(proof.HandoffGeneration),
		proof.SourceAttemptID,
		fmt.Sprint(proof.SourceClaimGeneration),
		proof.Code,
		proof.State,
		proof.ProviderReceiptHash,
		proof.ProviderIdempotencyKeyHash,
		proof.CompletedBoundary,
		proof.DecisionRef,
	)
}

func handoffContinuationProofFingerprint(proof HandoffContinuationProof) string {
	proof.CheckpointID = ""
	proof.ProofFingerprint = ""
	return digestJSON(proof)
}

func validateHandoffContinuationProof(handoff HandoffTransaction, rec HandoffSideEffectReconciliation, proof HandoffContinuationProof) error {
	if proof.SchemaVersion != HandoffContinuationProofSchema {
		return federationError(ErrInvalidRecordCode, "handoff continuation proof schema is invalid")
	}
	if !proof.AutomaticContinuation || rec.NeedsHuman || !rec.AutomaticContinuation {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof is not approved for automatic continuation")
	}
	if proof.HandoffID != handoff.HandoffID || proof.HandoffGeneration != handoff.HandoffGeneration ||
		proof.SourceAttemptID != handoff.SourceAttemptID || proof.SourceExecutorID != handoff.SourceExecutorID ||
		proof.SourceClaimGeneration != handoff.SourceClaimGeneration {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof does not match source authority")
	}
	if proof.State != normalizeHandoffSideEffectState(rec.State) || proof.Code != firstNonEmptyAgent(rec.Code, handoffSideEffectCode(rec.State)) {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof changed reconciliation state")
	}
	if proof.DecisionRef != "handoff-side-effect:"+handoff.HandoffID || proof.CompletedBoundary != handoffContinuationBoundary(rec) {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof changed boundary reference")
	}
	if proof.OwnershipFence != rec.OwnershipFence || proof.ExecutionOutcome != rec.ExecutionOutcome {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof changed source fence")
	}
	if proof.ProviderReceiptHash != strings.TrimSpace(rec.ProviderReceiptHash) || proof.ProviderIdempotencyKeyHash != strings.TrimSpace(rec.ProviderIdempotencyKeyHash) {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof changed reusable hash")
	}
	switch normalizeHandoffSideEffectState(rec.State) {
	case SideEffectStateReceiptBacked:
		if proof.ProviderReceiptHash == "" {
			return federationError(ErrHandoffReplayMismatchCode, "receipt-backed continuation proof is missing receipt hash")
		}
	case SideEffectStateIdempotent:
		if proof.ProviderIdempotencyKeyHash == "" {
			return federationError(ErrHandoffReplayMismatchCode, "idempotent continuation proof is missing idempotency key hash")
		}
	case SideEffectStateReadOnly, SideEffectStateNotStarted, SideEffectStateNone:
	default:
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof state %s is not automatically reusable", rec.State)
	}
	if strings.Join(proof.EvidenceRecordIDs, "\x00") != strings.Join(sortedCopyAgent(firstNonEmptySlice(rec.EvidenceRecordIDs, handoff.EvidenceRecordIDs)), "\x00") {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof changed evidence refs")
	}
	if proof.CheckpointID != handoffContinuationCheckpointID(proof) || proof.ProofFingerprint != handoffContinuationProofFingerprint(proof) {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof checkpoint changed")
	}
	return nil
}

func firstNonEmptySlice(primary, fallback []string) []string {
	if len(primary) > 0 {
		return primary
	}
	return fallback
}

func validateHandoffTransferAuthorityTx(ctx context.Context, tx Tx, req HandoffRequest, requestedAt time.Time) (string, string, FederationErrorCode, error) {
	record, ok, err := loadAgentRegistrationByRunTx(ctx, tx, req.ChildRunID)
	if err != nil {
		return "", "", "", err
	}
	if !ok {
		return HandoffStatusTransferred, "await-successor-route", "", nil
	}
	if record.ProjectID != req.ProjectID || record.DeliveryRunID != req.DeliveryRunID || record.TaskID != req.TaskID || record.ParentRunID != req.ParentRunID || record.RunID != req.ChildRunID {
		return HandoffStatusNeedsHuman, "human-review-authority-state", ErrHandoffAuthorityMismatchCode, nil
	}
	if record.ExecutorID != req.SourceExecutorID || record.ClaimGeneration != req.SourceClaimGeneration {
		return HandoffStatusNeedsHuman, "human-review-authority-state", ErrStaleClaimCode, nil
	}
	if normalizeAgentRegistrationState(record.RegistrationState) != AgentStateRegistered {
		return HandoffStatusNeedsHuman, "human-review-side-effect-state", ErrHandoffSideEffectUnknownCode, nil
	}
	if strings.TrimSpace(record.ProviderReceipt) != "" {
		return HandoffStatusNeedsHuman, "human-review-side-effect-state", ErrHandoffSideEffectUnknownCode, nil
	}
	if err := validateLiveBudgetReservationTx(ctx, tx, record, requestedAt); err != nil {
		return HandoffStatusNeedsHuman, "human-review-budget-state", federationCodeFromError(err, ErrChildBudgetRequiredCode), nil
	}
	if record.Permission != PermissionReadOnly {
		if err := validateLiveOwnershipLocksTx(ctx, tx, record, req.SourceClaimGeneration, requestedAt); err != nil {
			return HandoffStatusNeedsHuman, "human-review-ownership-state", federationCodeFromError(err, ErrOwnershipRequiredCode), nil
		}
	}
	return HandoffStatusTransferred, "await-successor-route", "", nil
}

func reconcileHandoffAgentAuthorityTx(ctx context.Context, tx Tx, req HandoffRequest, handoff HandoffTransaction, status string, terminalCode FederationErrorCode, lease string) error {
	record, ok, err := loadAgentRegistrationByRunTx(ctx, tx, req.ChildRunID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	scope, err := loadScopeGrantTx(ctx, tx, record)
	if err != nil {
		return err
	}
	record.ExecutorID = handoff.DestinationExecutorID
	record.ClaimGeneration = handoff.HandoffGeneration
	record.UpdatedAt = req.RequestedAt
	record.RecordVersion++
	if status != HandoffStatusTransferred {
		record.RegistrationState = AgentStateNeedsHuman
		record.TerminalErrorCode = string(terminalCode)
	} else if record.Permission != PermissionReadOnly {
		var err error
		record, err = ensureHandoffProviderReceiptOwnershipTx(ctx, tx, req, record, lease)
		if err != nil {
			return err
		}
	}
	fingerprint, payloadHash, err := handoffAgentRegistrationFingerprints(record, scope)
	if err != nil {
		return err
	}
	record.AgentFederationFingerprint = fingerprint
	record.RegistrationPayloadHash = payloadHash
	scope.AgentFederationFingerprint = fingerprint
	result, err := tx.Exec(ctx, `UPDATE agent_registrations
		SET ownership_lock_ids_json = ?, claim_generation = ?, executor_id = ?, registration_state = ?, agent_federation_fingerprint = ?,
			registration_payload_hash = ?, terminal_error_code = ?, updated_at = ?, record_version = ?
		WHERE id = ? AND child_run_id = ?`,
		mustJSONList(record.OwnershipLockIDs), record.ClaimGeneration, record.ExecutorID, record.RegistrationState, record.AgentFederationFingerprint,
		record.RegistrationPayloadHash, record.TerminalErrorCode, record.UpdatedAt, record.RecordVersion,
		record.ChildAgentID, record.RunID)
	if err != nil {
		return fmt.Errorf("record handoff: update agent registration: %w", err)
	}
	if affected, err := result.RowsAffected(); err == nil && affected != 1 {
		return federationError(ErrHandoffAuthorityMismatchCode, "updated %d agent registrations, want 1", affected)
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_scope_grants
		SET agent_federation_fingerprint = ?, updated_at = ?
		WHERE id = ? AND child_agent_id = ?`,
		scope.AgentFederationFingerprint, req.RequestedAt, record.ScopeGrantID, record.ChildAgentID); err != nil {
		return fmt.Errorf("record handoff: update agent scope fingerprint: %w", err)
	}
	if status == HandoffStatusTransferred {
		if record.Permission == PermissionReadOnly || len(record.OwnershipLockIDs) == 0 {
			return nil
		}
		result, err := tx.Exec(ctx, `UPDATE agent_ownership_locks
			SET claim_generation = ?, lease_expires_at = ?, heartbeat_at = ?, lock_generation = lock_generation + 1, updated_at = ?
			WHERE child_agent_id = ? AND run_id = ? AND claim_generation = ? AND state = ?`,
			handoff.HandoffGeneration, lease, req.RequestedAt, req.RequestedAt,
			record.ChildAgentID, req.ChildRunID, req.SourceClaimGeneration, OwnershipStateHeld)
		if err != nil {
			return fmt.Errorf("record handoff: transfer ownership locks: %w", err)
		}
		if affected, err := result.RowsAffected(); err == nil && affected != int64(len(record.OwnershipLockIDs)) {
			return federationError(ErrOwnershipRequiredCode, "transferred %d ownership locks, want %d", affected, len(record.OwnershipLockIDs))
		}
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE agent_ownership_locks
		SET state = ?, claim_generation = ?, lease_expires_at = ?, heartbeat_at = ?, updated_at = ?
		WHERE child_agent_id = ? AND run_id = ? AND state NOT IN (?, ?, ?, ?)`,
		OwnershipStateNeedsHuman, handoff.HandoffGeneration, req.RequestedAt, req.RequestedAt, req.RequestedAt,
		record.ChildAgentID, req.ChildRunID, OwnershipStateReleased, OwnershipStateExpired, OwnershipStateConflict, OwnershipStateNeedsHuman)
	if err != nil {
		return fmt.Errorf("record handoff: fence ownership locks to needs-human: %w", err)
	}
	return nil
}

func ensureHandoffProviderReceiptOwnershipTx(ctx context.Context, tx Tx, req HandoffRequest, record AgentRegistration, lease string) (AgentRegistration, error) {
	resourceKind := "provider-receipt"
	resourceKey := canonicalResourceKey(resourceKind, record.RunID)
	var existingID string
	err := tx.QueryRow(ctx, `SELECT id FROM agent_ownership_locks
		WHERE child_agent_id = ? AND run_id = ? AND resource_kind = ? AND resource_key = ?
			AND state NOT IN (?, ?, ?, ?)
		ORDER BY id LIMIT 1`,
		record.ChildAgentID, record.RunID, resourceKind, resourceKey,
		OwnershipStateReleased, OwnershipStateExpired, OwnershipStateConflict, OwnershipStateNeedsHuman).Scan(&existingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AgentRegistration{}, fmt.Errorf("record handoff: inspect provider receipt ownership: %w", err)
	}
	if existingID == "" {
		lock := AgentOwnershipLock{
			SchemaVersion:        AgentOwnershipLockSchema,
			AgentOwnershipLockID: stableID("alock_", record.ProjectID, resourceKind, resourceKey, record.ChildAgentID),
			ProjectID:            record.ProjectID,
			DeliveryRunID:        record.DeliveryRunID,
			ChildAgentID:         record.ChildAgentID,
			RunID:                record.RunID,
			ClaimGeneration:      req.SourceClaimGeneration,
			LockGeneration:       1,
			ResourceKind:         resourceKind,
			ResourceKey:          resourceKey,
			LockMode:             "write",
			State:                OwnershipStateHeld,
			LeaseExpiresAt:       lease,
			HeartbeatAt:          req.RequestedAt,
			CreatedAt:            req.RequestedAt,
			UpdatedAt:            req.RequestedAt,
		}
		if err := insertOwnershipLockTx(ctx, tx, lock); err != nil {
			return AgentRegistration{}, fmt.Errorf("record handoff: add provider receipt ownership: %w", err)
		}
		existingID = lock.AgentOwnershipLockID
	}
	if !stringSetAgent(record.OwnershipLockIDs)[existingID] {
		record.OwnershipLockIDs = sortedCopyAgent(append(record.OwnershipLockIDs, existingID))
	}
	return record, nil
}

func handoffAgentRegistrationFingerprints(record AgentRegistration, scope AgentScopeGrant) (string, string, error) {
	scope.AgentFederationFingerprint = ""
	scopeBytes, err := json.Marshal(scopePayload(scope))
	if err != nil {
		return "", "", federationError(ErrInvalidRecordCode, "marshal scope: %v", err)
	}
	input := federationFingerprintInput{
		SchemaVersion:            AgentRegistrationSchema,
		ProjectID:                record.ProjectID,
		DeliveryRunID:            record.DeliveryRunID,
		RootRunID:                record.RootRunID,
		ParentRunID:              record.ParentRunID,
		RunID:                    record.RunID,
		ParentAgentID:            record.ParentAgentID,
		TaskID:                   record.TaskID,
		AttemptID:                record.AttemptID,
		PlanID:                   record.PlanID,
		ChildKey:                 record.ChildKey,
		AdapterID:                record.AdapterID,
		ProviderInstallationID:   record.ProviderInstallationID,
		AccountProfileID:         record.AccountProfileID,
		ModelCapabilityID:        record.ModelCapabilityID,
		RoutingDecisionID:        record.RoutingDecisionID,
		ProviderSessionRef:       record.ProviderSessionRef,
		ScopeCanonicalJSON:       string(scopeBytes),
		Permission:               record.Permission,
		SideEffectClass:          record.SideEffectClass,
		BudgetBindingIDs:         sortedCopyAgent(record.BudgetBindingIDs),
		OwnershipLockIDs:         sortedCopyAgent(record.OwnershipLockIDs),
		ClaimGeneration:          record.ClaimGeneration,
		ExecutorID:               record.ExecutorID,
		ProviderIDempotencyKey:   record.ProviderIDempotencyKey,
		CancellationChannel:      record.CancellationChannel,
		ExpectedOutputsHash:      hashString(record.ExpectedOutputsJSON),
		PlanFingerprint:          record.PlanFingerprint,
		PolicyFingerprint:        record.PolicyFingerprint,
		AuthorizationFingerprint: record.AuthorizationFingerprint,
	}
	fingerprint := digestJSON(input)
	payloadHash := digestJSON(registrationCanonicalPayload{
		FingerprintInput:  input,
		RegistrationState: AgentStateRegistered,
		Classification:    record.Classification,
		GapReasons:        sortedCopyAgent(record.GapReasons),
	})
	return fingerprint, payloadHash, nil
}

func federationCodeFromError(err error, fallback FederationErrorCode) FederationErrorCode {
	var typed *FederationError
	if errors.As(err, &typed) && typed.Code != "" {
		return typed.Code
	}
	return fallback
}

func updateDeliveryAfterHandoffTx(ctx context.Context, tx Tx, req HandoffRequest, status string, terminalCode FederationErrorCode, claimProviderReceipt string) error {
	attemptState := "stale"
	taskState := "claimed"
	if status != HandoffStatusTransferred {
		attemptState = "needs-human"
		taskState = "needs-human"
	}
	if _, err := tx.Exec(ctx, `UPDATE delivery_attempts
		SET state = ?, record_version = record_version + 1, updated_at = ?, ended_at = COALESCE(NULLIF(ended_at, ''), ?),
			terminal_error_code = CASE WHEN ? <> '' THEN ? ELSE terminal_error_code END,
			provider_receipt = CASE WHEN provider_receipt = '' AND ? <> '' THEN ? ELSE provider_receipt END
		WHERE attempt_id = ? AND executor_id = ? AND claim_generation = ?`,
		attemptState, req.RequestedAt, req.RequestedAt, string(terminalCode), string(terminalCode), strings.TrimSpace(claimProviderReceipt), strings.TrimSpace(claimProviderReceipt), req.SourceAttemptID, req.SourceExecutorID, req.SourceClaimGeneration); err != nil {
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

func classifyHandoff(req HandoffRequest, sideEffectState string) (string, string, FederationErrorCode) {
	if !handoffSideEffectSafe(sideEffectState) {
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
	case SideEffectStateNone, SideEffectStateNotStarted, SideEffectStateReadOnly, SideEffectStateIdempotent, SideEffectStateReceiptBacked:
		return true
	default:
		return false
	}
}

func handoffSideEffectCode(state string) string {
	switch normalizeHandoffSideEffectState(state) {
	case SideEffectStateNone, SideEffectStateNotStarted:
		return HandoffSideEffectCodeNotStarted
	case SideEffectStateReadOnly:
		return HandoffSideEffectCodeReadOnly
	case SideEffectStateIdempotent:
		return HandoffSideEffectCodeIdempotentExact
	case SideEffectStateReceiptBacked:
		return HandoffSideEffectCodeReceiptBacked
	case SideEffectStateIrreversible:
		return HandoffSideEffectCodeIrreversible
	default:
		return HandoffSideEffectCodeAmbiguous
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

func normalizeHandoffSuccessorRequest(req HandoffSuccessorRequest, now time.Time) HandoffSuccessorRequest {
	req.HandoffID = strings.TrimSpace(req.HandoffID)
	req.SourceAttemptID = strings.TrimSpace(req.SourceAttemptID)
	req.RoutingDecisionID = strings.TrimSpace(req.RoutingDecisionID)
	req.RoutingFingerprint = strings.TrimSpace(req.RoutingFingerprint)
	req.BudgetReservationID = strings.TrimSpace(req.BudgetReservationID)
	req.BudgetPolicyIDs = sortedCopyAgent(req.BudgetPolicyIDs)
	req.ReusableEvidenceIDs = sortedCopyAgent(req.ReusableEvidenceIDs)
	req.ContinuationProof = normalizeHandoffContinuationProof(req.ContinuationProof)
	req.Candidate.RoutingDecisionID = strings.TrimSpace(firstNonEmptyAgent(req.Candidate.RoutingDecisionID, req.RoutingDecisionID))
	req.Candidate.RoutingCandidateID = strings.TrimSpace(req.Candidate.RoutingCandidateID)
	req.Candidate.AdapterID = strings.TrimSpace(req.Candidate.AdapterID)
	req.Candidate.ProviderInstallationID = strings.TrimSpace(req.Candidate.ProviderInstallationID)
	req.Candidate.AccountProfileID = strings.TrimSpace(req.Candidate.AccountProfileID)
	req.Candidate.ModelCapabilityID = strings.TrimSpace(req.Candidate.ModelCapabilityID)
	req.CreatedAt = strings.TrimSpace(req.CreatedAt)
	if req.CreatedAt == "" {
		req.CreatedAt = formatTimestamp(now.UTC())
	}
	req.ActorJSON = compactJSONString(req.ActorJSON)
	if req.ActorJSON == "" || req.ActorJSON == "null" {
		req.ActorJSON = "{}"
	}
	req.HostJSON = compactJSONString(req.HostJSON)
	if req.HostJSON == "" || req.HostJSON == "null" {
		req.HostJSON = "{}"
	}
	return req
}

func normalizeHandoffContinuationProof(proof HandoffContinuationProof) HandoffContinuationProof {
	proof.SchemaVersion = strings.TrimSpace(proof.SchemaVersion)
	proof.Code = strings.TrimSpace(proof.Code)
	proof.State = normalizeHandoffSideEffectState(proof.State)
	proof.SourceAttemptID = strings.TrimSpace(proof.SourceAttemptID)
	proof.SourceExecutorID = strings.TrimSpace(proof.SourceExecutorID)
	proof.HandoffID = strings.TrimSpace(proof.HandoffID)
	proof.OwnershipFence = strings.TrimSpace(proof.OwnershipFence)
	proof.ExecutionOutcome = strings.TrimSpace(proof.ExecutionOutcome)
	proof.ProviderIdempotencyKeyHash = strings.TrimSpace(proof.ProviderIdempotencyKeyHash)
	proof.ProviderReceiptHash = strings.TrimSpace(proof.ProviderReceiptHash)
	proof.EvidenceRecordIDs = sortedCopyAgent(proof.EvidenceRecordIDs)
	proof.DecisionRef = strings.TrimSpace(proof.DecisionRef)
	proof.CompletedBoundary = strings.TrimSpace(proof.CompletedBoundary)
	proof.CheckpointID = strings.TrimSpace(proof.CheckpointID)
	proof.ProofFingerprint = strings.TrimSpace(proof.ProofFingerprint)
	return proof
}

func validateHandoffSuccessorRequest(req HandoffSuccessorRequest) error {
	required := map[string]string{
		"handoff_id":            req.HandoffID,
		"source_attempt_id":     req.SourceAttemptID,
		"routing_decision_id":   req.RoutingDecisionID,
		"routing_candidate_id":  req.Candidate.RoutingCandidateID,
		"adapter_id":            req.Candidate.AdapterID,
		"provider_installation": req.Candidate.ProviderInstallationID,
		"account_profile_id":    req.Candidate.AccountProfileID,
		"model_capability_id":   req.Candidate.ModelCapabilityID,
		"budget_reservation_id": req.BudgetReservationID,
		"created_at":            req.CreatedAt,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return federationError(ErrHandoffRequiredCode, "%s is required", field)
		}
	}
	if req.HandoffGeneration <= 0 {
		return federationError(ErrHandoffRequiredCode, "handoff_generation must be positive")
	}
	if err := requireNonZeroTimestamp(req.CreatedAt, "created_at"); err != nil {
		return err
	}
	if !json.Valid([]byte(req.ActorJSON)) || !json.Valid([]byte(req.HostJSON)) {
		return federationError(ErrInvalidRecordCode, "actor_json and host_json must be valid JSON")
	}
	if req.ContinuationProof.SchemaVersion != HandoffContinuationProofSchema || strings.TrimSpace(req.ContinuationProof.CheckpointID) == "" ||
		strings.TrimSpace(req.ContinuationProof.ProofFingerprint) == "" {
		return federationError(ErrHandoffRequiredCode, "durable handoff continuation proof is required")
	}
	if len(req.BudgetPolicyIDs) == 0 {
		return federationError(ErrChildBudgetRequiredCode, "at least one budget policy id is required")
	}
	if len(req.ReusableEvidenceIDs) > 32 {
		return federationError(ErrInvalidRecordCode, "too many reusable evidence refs")
	}
	for _, ref := range req.ReusableEvidenceIDs {
		if !validHandoffReusableRef(ref) {
			return federationError(ErrInvalidRecordCode, "invalid reusable evidence ref %q", ref)
		}
	}
	return nil
}

func validHandoffReusableRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > 160 || strings.Contains(ref, "/") || strings.Contains(ref, "\\") || strings.Contains(ref, "..") {
		return false
	}
	lower := strings.ToLower(ref)
	for _, banned := range []string{"token", "secret", "credential", "session", "akia"} {
		if strings.Contains(lower, banned) {
			return false
		}
	}
	for _, r := range ref {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == ':' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func handoffSuccessorAttemptID(handoff HandoffTransaction) string {
	return stableID("att_handoff_", handoff.HandoffID, fmt.Sprint(handoff.HandoffGeneration), handoff.SourceAttemptID)
}

func handoffSuccessorProviderKey(handoff HandoffTransaction) string {
	return "handoff-successor:" + handoff.HandoffID + ":" + fmt.Sprint(handoff.HandoffGeneration)
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

func prepareHandoffSuccessorLaunchTx(ctx context.Context, tx Tx, req HandoffSuccessorRequest) (HandoffSuccessorLaunch, error) {
	handoff, ok, err := loadHandoffTx(ctx, tx, `WHERE handoff_id = ?`, req.HandoffID)
	if err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	if !ok {
		return HandoffSuccessorLaunch{}, federationError(ErrHandoffRequiredCode, "handoff %s is missing", req.HandoffID)
	}
	if err := validateHandoffLaunchFenceTx(ctx, tx, handoff, req); err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	attemptID := handoffSuccessorAttemptID(handoff)
	providerKey := handoffSuccessorProviderKey(handoff)
	if existing, ok, err := loadHandoffSuccessorAttemptTx(ctx, tx, handoff, attemptID); err != nil {
		return HandoffSuccessorLaunch{}, err
	} else if ok {
		if existing.HandoffID != handoff.HandoffID || existing.HandoffGeneration != handoff.HandoffGeneration ||
			existing.SourceAttemptID != handoff.SourceAttemptID || existing.RoutingDecisionID != req.RoutingDecisionID ||
			existing.RoutingCandidateID != req.Candidate.RoutingCandidateID ||
			existing.ExecutorID != handoff.DestinationExecutorID || existing.ClaimGeneration != handoff.HandoffGeneration {
			return HandoffSuccessorLaunch{}, federationError(ErrHandoffReplayMismatchCode, "successor attempt replay changed handoff payload")
		}
		if existing.BudgetReservationID != req.BudgetReservationID && !(existing.LaunchPhase == ClaimPhaseCompleted && strings.TrimSpace(existing.ProviderReceipt) == "") {
			return HandoffSuccessorLaunch{}, federationError(ErrHandoffReplayMismatchCode, "successor attempt replay changed handoff reservation")
		}
		if err := validateHandoffContinuationProofTx(ctx, tx, handoff, req.ContinuationProof); err != nil {
			return HandoffSuccessorLaunch{}, err
		}
		if existing.ContinuationProof.CheckpointID == "" || existing.ContinuationProof.CheckpointID != req.ContinuationProof.CheckpointID ||
			existing.ContinuationProof.ProofFingerprint != req.ContinuationProof.ProofFingerprint {
			return HandoffSuccessorLaunch{}, federationError(ErrHandoffReplayMismatchCode, "successor attempt replay changed continuation proof")
		}
		existing.Replay = true
		return existing, nil
	}
	if req.Cancelled {
		return HandoffSuccessorLaunch{}, federationError(ErrHandoffAuthorityMismatchCode, "handoff successor launch cancelled before authority exposure")
	}
	sourceOrdinal, sideEffectClass, err := sourceAttemptForSuccessorTx(ctx, tx, handoff)
	if err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	if err := bindSuccessorBudgetTx(ctx, tx, handoff, req); err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	registration, ok, err := loadAgentRegistrationByRunTx(ctx, tx, handoff.ChildRunID)
	if err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	if !ok {
		return HandoffSuccessorLaunch{}, federationError(ErrAgentRegistrationRequiredCode, "run %s has no registration", handoff.ChildRunID)
	}
	if err := updateHandoffSuccessorRegistrationTx(ctx, tx, registration, req, attemptID, providerKey); err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	if err := insertHandoffSuccessorAttemptTx(ctx, tx, handoff, req, attemptID, providerKey, sourceOrdinal+1, sideEffectClass); err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	ownershipSnapshot, err := handoffOwnershipLockSnapshotTx(ctx, tx, handoff.ChildRunID, handoff.DestinationExecutorID, handoff.HandoffGeneration, req.CreatedAt)
	if err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	if err := insertHandoffSuccessorDecisionTx(ctx, tx, handoff, req, attemptID, providerKey, ownershipSnapshot); err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	if err := claimHandoffSuccessorLaunchTx(ctx, tx, handoff, req.CreatedAt); err != nil {
		return HandoffSuccessorLaunch{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE delivery_tasks
		SET state = 'claimed', active_attempt_id = ?, attempt_count = CASE WHEN attempt_count < ? THEN ? ELSE attempt_count END,
			updated_at = ?, terminal_error_code = '', error_message = ''
		WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`,
		attemptID, sourceOrdinal+1, sourceOrdinal+1, req.CreatedAt, handoff.ProjectID, handoff.DeliveryRunID, handoff.TaskID); err != nil {
		return HandoffSuccessorLaunch{}, fmt.Errorf("handoff successor: update task: %w", err)
	}
	return HandoffSuccessorLaunch{
		HandoffID:              handoff.HandoffID,
		HandoffGeneration:      handoff.HandoffGeneration,
		AttemptID:              attemptID,
		SourceAttemptID:        handoff.SourceAttemptID,
		TaskID:                 handoff.TaskID,
		DeliveryRunID:          handoff.DeliveryRunID,
		ProjectID:              handoff.ProjectID,
		ExecutorID:             handoff.DestinationExecutorID,
		ClaimGeneration:        handoff.HandoffGeneration,
		ProviderIdempotencyKey: providerKey,
		RoutingDecisionID:      req.RoutingDecisionID,
		RoutingCandidateID:     req.Candidate.RoutingCandidateID,
		BudgetReservationID:    req.BudgetReservationID,
		ReusableEvidenceIDs:    sortedCopyAgent(req.ReusableEvidenceIDs),
		ContinuationProof:      req.ContinuationProof,
		OwnershipLocks:         ownershipSnapshot,
		LaunchExposed:          true,
		LaunchPhase:            ClaimPhaseLaunching,
	}, nil
}

func claimHandoffSuccessorLaunchTx(ctx context.Context, tx Tx, handoff HandoffTransaction, at string) error {
	result, err := tx.Exec(ctx, `UPDATE run_claims
		SET phase = ?, heartbeat_at = ?, provider_receipt = ''
		WHERE run_id = ? AND executor_id = ? AND claim_generation = ? AND phase = ?`,
		ClaimPhaseLaunching, at, handoff.ChildRunID, handoff.DestinationExecutorID, handoff.HandoffGeneration, ClaimPhaseClaimed)
	if err != nil {
		return fmt.Errorf("handoff successor: claim launch ownership: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != 1 {
		return federationError(ErrStaleClaimCode, "handoff successor launch ownership is no longer claimable")
	}
	if err := transitionRunStatusTx(ctx, tx, RunStatusTransition{
		RunID:       handoff.ChildRunID,
		ParentRunID: handoff.ParentRunID,
		ChildRunID:  handoff.ChildRunID,
		Status:      "launching",
		UpdatedAt:   at,
		Reason:      "handoff successor launch claimed",
		Source:      "handoff",
	}); err != nil {
		return err
	}
	return nil
}

func validateHandoffLaunchFenceTx(ctx context.Context, tx Tx, handoff HandoffTransaction, req HandoffSuccessorRequest) error {
	if handoff.HandoffStatus != HandoffStatusTransferred || handoff.NextAction != "await-successor-route" {
		return federationError(ErrHandoffAuthorityMismatchCode, "handoff %s is %s/%s, not approved for successor routing", handoff.HandoffID, handoff.HandoffStatus, handoff.NextAction)
	}
	if handoff.HandoffGeneration != req.HandoffGeneration || handoff.SourceAttemptID != req.SourceAttemptID {
		return federationError(ErrHandoffReplayMismatchCode, "handoff generation or source attempt mismatch")
	}
	var claimExecutor string
	var claimGeneration int64
	if err := tx.QueryRow(ctx, `SELECT executor_id, claim_generation FROM run_claims WHERE run_id = ?`, handoff.ChildRunID).Scan(&claimExecutor, &claimGeneration); err != nil {
		return federationError(ErrAgentRegistrationRequiredCode, "claim for %s is missing", handoff.ChildRunID)
	}
	if claimExecutor != handoff.DestinationExecutorID || claimGeneration != handoff.HandoffGeneration {
		return federationError(ErrStaleClaimCode, "handoff successor no longer owns run %s", handoff.ChildRunID)
	}
	var runInput, runPolicy, runPlan, runAuth string
	if err := tx.QueryRow(ctx, `SELECT input_fingerprint, policy_fingerprint, plan_fingerprint, authorization_fingerprint
		FROM delivery_runs WHERE delivery_run_id = ? AND project_id = ?`, handoff.DeliveryRunID, handoff.ProjectID).Scan(&runInput, &runPolicy, &runPlan, &runAuth); err != nil {
		return federationError(ErrHandoffAcceptedTaskStaleCode, "delivery run %s is missing", handoff.DeliveryRunID)
	}
	if runInput != handoff.InputFingerprint || runPolicy != handoff.PolicyFingerprint || runPlan != handoff.PlanFingerprint || runAuth != handoff.AuthorizationFingerprint {
		return federationError(ErrHandoffAcceptedTaskStaleCode, "delivery run authority changed after handoff approval")
	}
	var taskPlan, taskAuth string
	if err := tx.QueryRow(ctx, `SELECT plan_fingerprint, authorization_fingerprint FROM delivery_tasks
		WHERE project_id = ? AND delivery_run_id = ? AND task_id = ?`, handoff.ProjectID, handoff.DeliveryRunID, handoff.TaskID).Scan(&taskPlan, &taskAuth); err != nil {
		return federationError(ErrHandoffAcceptedTaskStaleCode, "delivery task %s is missing", handoff.TaskID)
	}
	if taskPlan != handoff.PlanFingerprint || taskAuth != handoff.AuthorizationFingerprint {
		return federationError(ErrHandoffAcceptedTaskStaleCode, "delivery task authority changed after handoff approval")
	}
	if req.RoutingFingerprint != "" {
		var routeProject, routeRun, routeTask, routePlan, routePolicy, routeStatus, routeChosen, routeFingerprint string
		if err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, task_id, plan_fingerprint, policy_fingerprint, decision_status, chosen_candidate_id, routing_fingerprint
			FROM routing_decisions WHERE routing_decision_id = ?`, req.RoutingDecisionID).Scan(&routeProject, &routeRun, &routeTask, &routePlan, &routePolicy, &routeStatus, &routeChosen, &routeFingerprint); err != nil {
			return federationError(ErrHandoffAuthorityMismatchCode, "routing decision %s is missing", req.RoutingDecisionID)
		}
		if routeProject != handoff.ProjectID || routeRun != handoff.DeliveryRunID || routeTask != handoff.TaskID ||
			routePlan != handoff.PlanFingerprint || routePolicy == "" || routeStatus != "selected" ||
			routeChosen != req.Candidate.RoutingCandidateID || routeFingerprint != req.RoutingFingerprint {
			return federationError(ErrHandoffAuthorityMismatchCode, "routing decision does not match approved handoff authority")
		}
	}
	if err := validateHandoffContinuationProofTx(ctx, tx, handoff, req.ContinuationProof); err != nil {
		return err
	}
	return nil
}

func validateHandoffContinuationProofTx(ctx context.Context, tx Tx, handoff HandoffTransaction, proof HandoffContinuationProof) error {
	rec, ok, err := loadHandoffSideEffectDecisionTx(ctx, tx, handoff)
	if err != nil {
		return err
	}
	if !ok {
		return federationError(ErrHandoffReplayMismatchCode, "handoff continuation proof is missing durable reconciliation decision")
	}
	return validateHandoffContinuationProof(handoff, rec, proof)
}

func sourceAttemptForSuccessorTx(ctx context.Context, tx Tx, handoff HandoffTransaction) (int, string, error) {
	var ordinal int
	var sideEffectClass string
	err := tx.QueryRow(ctx, `SELECT attempt_ordinal, side_effect_class FROM delivery_attempts WHERE attempt_id = ?`, handoff.SourceAttemptID).Scan(&ordinal, &sideEffectClass)
	if err != nil {
		return 0, "", federationError(ErrHandoffAcceptedTaskStaleCode, "source attempt %s is missing", handoff.SourceAttemptID)
	}
	return ordinal, sideEffectClass, nil
}

func bindSuccessorBudgetTx(ctx context.Context, tx Tx, handoff HandoffTransaction, req HandoffSuccessorRequest) error {
	registration, ok, err := loadAgentRegistrationByRunTx(ctx, tx, handoff.ChildRunID)
	if err != nil {
		return err
	}
	if !ok {
		return federationError(ErrAgentRegistrationRequiredCode, "run %s has no registration", handoff.ChildRunID)
	}
	var reservationProject, reservationRun, reservationTask, reservationSubAgent, adapterID, accountID, modelID, state, lease string
	var generation, reserved int64
	var policyIDsJSON string
	if err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, task_id, sub_agent_id, adapter_id, account_profile_id,
			model_capability_id, reserved_value, state, generation, lease_expires_at, policy_ids_json
		FROM budget_reservations WHERE budget_reservation_id = ?`, req.BudgetReservationID).Scan(
		&reservationProject, &reservationRun, &reservationTask, &reservationSubAgent, &adapterID, &accountID, &modelID, &reserved, &state, &generation, &lease, &policyIDsJSON); err != nil {
		return federationError(ErrChildBudgetRequiredCode, "successor reservation %s is missing", req.BudgetReservationID)
	}
	if reservationProject != handoff.ProjectID || reservationRun != handoff.DeliveryRunID || reservationTask != handoff.TaskID ||
		reservationSubAgent != registration.ChildAgentID || adapterID != req.Candidate.AdapterID || accountID != req.Candidate.AccountProfileID ||
		modelID != req.Candidate.ModelCapabilityID || reserved <= 0 || (state != BudgetReservationStateActive && state != BudgetReservationStatePartiallyCommitted) {
		return federationError(ErrChildBudgetRequiredCode, "successor reservation does not match destination candidate")
	}
	leaseTime, err := time.Parse(time.RFC3339Nano, lease)
	createdAt, parseErr := time.Parse(time.RFC3339Nano, req.CreatedAt)
	if err != nil || parseErr != nil || !leaseTime.After(createdAt.UTC()) {
		return federationError(ErrChildBudgetRequiredCode, "successor reservation lease is invalid")
	}
	policyIDs := decodeStringList(policyIDsJSON)
	for _, want := range req.BudgetPolicyIDs {
		if !containsStringAgent(policyIDs, want) {
			return federationError(ErrChildBudgetRequiredCode, "successor reservation missing policy %s", want)
		}
	}
	binding := normalizeBudgetBinding(AgentBudgetBinding{
		ProjectID:           handoff.ProjectID,
		DeliveryRunID:       handoff.DeliveryRunID,
		ChildAgentID:        registration.ChildAgentID,
		BudgetPolicyID:      firstNonEmptyAgent(req.BudgetPolicyIDs...),
		BudgetReservationID: req.BudgetReservationID,
	}, AgentRegistrationRequest{ProjectID: handoff.ProjectID, DeliveryRunID: handoff.DeliveryRunID}, registration.ChildAgentID)
	if _, err := tx.Exec(ctx, `INSERT INTO agent_budget_bindings(
			id, project_id, delivery_run_id, child_agent_id, budget_policy_id, budget_reservation_id,
			reservation_scope, reserved_quantities_json, ancestor_budget_refs_json, reservation_state, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '[]', ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		binding.AgentBudgetBindingID, binding.ProjectID, binding.DeliveryRunID, binding.ChildAgentID, binding.BudgetPolicyID, binding.BudgetReservationID,
		"sub-agent", `{"local-policy":`+fmt.Sprint(reserved)+`}`, state, req.CreatedAt, req.CreatedAt); err != nil {
		return fmt.Errorf("handoff successor: bind budget reservation: %w", err)
	}
	return nil
}

func updateHandoffSuccessorRegistrationTx(ctx context.Context, tx Tx, record AgentRegistration, req HandoffSuccessorRequest, attemptID, providerKey string) error {
	scope, err := loadScopeGrantTx(ctx, tx, record)
	if err != nil {
		return err
	}
	bindingID := stableID("abudget_", record.ChildAgentID, req.BudgetReservationID, firstNonEmptyAgent(req.BudgetPolicyIDs...))
	record.AttemptID = attemptID
	record.AdapterID = req.Candidate.AdapterID
	record.ProviderInstallationID = req.Candidate.ProviderInstallationID
	record.AccountProfileID = req.Candidate.AccountProfileID
	record.ModelCapabilityID = req.Candidate.ModelCapabilityID
	record.RoutingDecisionID = req.RoutingDecisionID
	record.ProviderSessionRef = ""
	record.ProviderIDempotencyKey = providerKey
	record.BudgetBindingIDs = sortedCopyAgent([]string{bindingID})
	record.RegistrationState = AgentStateRegistered
	record.TerminalErrorCode = ""
	record.GapReasons = []string{}
	record.UpdatedAt = req.CreatedAt
	record.RecordVersion++
	fingerprint, payloadHash, err := handoffAgentRegistrationFingerprints(record, scope)
	if err != nil {
		return err
	}
	record.AgentFederationFingerprint = fingerprint
	record.RegistrationPayloadHash = payloadHash
	scope.AgentFederationFingerprint = fingerprint
	if _, err := tx.Exec(ctx, `UPDATE agent_scope_grants
		SET agent_federation_fingerprint = ?, updated_at = ?
		WHERE id = ? AND child_agent_id = ?`,
		scope.AgentFederationFingerprint, req.CreatedAt, record.ScopeGrantID, record.ChildAgentID); err != nil {
		return fmt.Errorf("handoff successor: update scope fingerprint: %w", err)
	}
	budgetBindingJSON, err := json.Marshal(record.BudgetBindingIDs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_registrations
		SET attempt_id = ?, adapter_id = ?, provider_installation_id = ?, account_profile_id = ?, model_capability_id = ?,
			routing_decision_id = ?, provider_session_ref = '', provider_idempotency_key = ?, budget_binding_ids_json = ?,
			registration_state = ?, terminal_error_code = '', gap_reasons_json = '[]', agent_federation_fingerprint = ?,
			registration_payload_hash = ?, updated_at = ?, record_version = ?
		WHERE id = ? AND child_run_id = ? AND executor_id = ? AND claim_generation = ?`,
		record.AttemptID, record.AdapterID, record.ProviderInstallationID, record.AccountProfileID, record.ModelCapabilityID,
		record.RoutingDecisionID, record.ProviderIDempotencyKey, string(budgetBindingJSON), record.RegistrationState,
		record.AgentFederationFingerprint, record.RegistrationPayloadHash, record.UpdatedAt, record.RecordVersion,
		record.ChildAgentID, record.RunID, record.ExecutorID, record.ClaimGeneration); err != nil {
		return fmt.Errorf("handoff successor: update agent registration: %w", err)
	}
	return nil
}

func insertHandoffSuccessorAttemptTx(ctx context.Context, tx Tx, handoff HandoffTransaction, req HandoffSuccessorRequest, attemptID, providerKey string, ordinal int, sideEffectClass string) error {
	_, err := tx.Exec(ctx, `INSERT INTO delivery_attempts(
			attempt_id, schema_version, record_version, project_id, delivery_run_id, task_id, attempt_ordinal, state,
			claim_generation, executor_id, provider_idempotency_key, side_effect_class, started_at, created_at, updated_at,
			created_by_json, updated_by_json, host_json)
		VALUES (?, 'loopcoder.attempt.v1', 1, ?, ?, ?, ?, 'claimed', ?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`,
		attemptID, handoff.ProjectID, handoff.DeliveryRunID, handoff.TaskID, ordinal, handoff.HandoffGeneration,
		handoff.DestinationExecutorID, providerKey, sideEffectClass, req.CreatedAt, req.CreatedAt, req.ActorJSON, req.ActorJSON, req.HostJSON)
	return err
}

func insertHandoffSuccessorDecisionTx(ctx context.Context, tx Tx, handoff HandoffTransaction, req HandoffSuccessorRequest, attemptID, providerKey string, ownershipSnapshot []HandoffOwnershipLockSnapshot) error {
	output, err := compactJSON(map[string]any{
		"schema_version":           "loopcoder.handoff_successor_launch.v1",
		"handoff_id":               handoff.HandoffID,
		"handoff_generation":       handoff.HandoffGeneration,
		"source_attempt_id":        handoff.SourceAttemptID,
		"successor_attempt_id":     attemptID,
		"routing_decision_id":      req.RoutingDecisionID,
		"routing_candidate_id":     req.Candidate.RoutingCandidateID,
		"budget_reservation_id":    req.BudgetReservationID,
		"reusable_evidence_ids":    sortedCopyAgent(req.ReusableEvidenceIDs),
		"continuation_proof":       req.ContinuationProof,
		"ownership_locks":          normalizeHandoffOwnershipLockSnapshot(ownershipSnapshot),
		"provider_idempotency_key": providerKey,
		"provider_session_ref":     "",
		"launch_exposed":           true,
		"launch_phase":             ClaimPhaseLaunching,
	})
	if err != nil {
		return err
	}
	decisionID := stableID("ddec_", handoff.HandoffID, fmt.Sprint(handoff.HandoffGeneration), attemptID)
	_, err = tx.Exec(ctx, `INSERT INTO delivery_decisions(
			decision_id, schema_version, record_version, project_id, delivery_run_id, task_id, decision_key, decision_kind,
			decided_by_json, inputs_fingerprint, output_json, alternatives_json, heuristic, heuristic_reason, policy_version,
			side_effect_class, created_at, created_by_json, host_json)
		VALUES (?, 'loopcoder.delivery_decision.v1', 1, ?, ?, ?, ?, 'handoff-successor-launch', ?, ?, ?, 'null', 0, '', ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, delivery_run_id, decision_key) DO NOTHING`,
		decisionID, handoff.ProjectID, handoff.DeliveryRunID, handoff.TaskID, "handoff-successor:"+handoff.HandoffID,
		req.ActorJSON, digestJSON(map[string]any{"handoff_id": handoff.HandoffID, "routing_fingerprint": req.RoutingFingerprint, "reservation_id": req.BudgetReservationID}),
		output, HandoffPolicyVersion, SideEffectStateProviderStart, req.CreatedAt, req.ActorJSON, req.HostJSON)
	return err
}

func handoffOwnershipLockSnapshotTx(ctx context.Context, tx Tx, childRunID, executorID string, claimGeneration int64, at string) ([]HandoffOwnershipLockSnapshot, error) {
	record, ok, err := loadAgentRegistrationByRunTx(ctx, tx, childRunID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, federationError(ErrAgentRegistrationRequiredCode, "run %s has no registration", childRunID)
	}
	if record.ExecutorID != executorID || record.ClaimGeneration != claimGeneration {
		return nil, federationError(ErrStaleClaimCode, "registration claim does not match handoff launch owner/generation")
	}
	return handoffOwnershipLockSnapshotForRegistrationTx(ctx, tx, record, at)
}

func handoffOwnershipLockSnapshotForRegistrationTx(ctx context.Context, tx Tx, record AgentRegistration, at string) ([]HandoffOwnershipLockSnapshot, error) {
	lockIDs := sortedCopyAgent(record.OwnershipLockIDs)
	if len(lockIDs) == 0 {
		if strings.TrimSpace(record.Permission) == PermissionReadOnly {
			return nil, nil
		}
		return nil, federationError(ErrOwnershipRequiredCode, "registration %s has no ownership locks", record.ChildAgentID)
	}
	atTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(at))
	if err != nil {
		return nil, federationError(ErrInvalidRecordCode, "handoff ownership snapshot time is invalid")
	}
	rows, err := tx.Query(ctx, `SELECT id, child_agent_id, run_id, claim_generation, lock_generation, state, lease_expires_at
		FROM agent_ownership_locks
		WHERE project_id = ? AND delivery_run_id = ? AND id IN (`+placeholders(len(lockIDs))+`)`,
		append([]any{record.ProjectID, record.DeliveryRunID}, stringsToAny(lockIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("snapshot handoff ownership locks: %w", err)
	}
	defer rows.Close()
	expected := stringSetAgent(lockIDs)
	seen := map[string]bool{}
	out := make([]HandoffOwnershipLockSnapshot, 0, len(lockIDs))
	for rows.Next() {
		var id, ownerID, runID, state, leaseExpiresAt string
		var claimGeneration, lockGeneration int64
		if err := rows.Scan(&id, &ownerID, &runID, &claimGeneration, &lockGeneration, &state, &leaseExpiresAt); err != nil {
			return nil, fmt.Errorf("scan handoff ownership snapshot: %w", err)
		}
		if !expected[id] {
			continue
		}
		seen[id] = true
		if ownerID != record.ChildAgentID || runID != record.RunID || claimGeneration != record.ClaimGeneration || lockGeneration <= 0 {
			return nil, federationError(ErrOwnershipStaleCode, "ownership lock %s no longer matches handoff launch owner/generation", id)
		}
		if state != OwnershipStateHeld {
			return nil, federationError(ErrOwnershipStaleCode, "ownership lock %s is %s", id, state)
		}
		leaseTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(leaseExpiresAt))
		if err != nil || !leaseTime.After(atTime.UTC()) {
			return nil, federationError(ErrOwnershipStaleCode, "ownership lock %s lease expired at %s", id, leaseExpiresAt)
		}
		out = append(out, HandoffOwnershipLockSnapshot{LockID: id, LockGeneration: lockGeneration})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan handoff ownership snapshot rows: %w", err)
	}
	for _, id := range lockIDs {
		if !seen[id] {
			return nil, federationError(ErrOwnershipStaleCode, "ownership lock %s is missing", id)
		}
	}
	return normalizeHandoffOwnershipLockSnapshot(out), nil
}

func refreshHandoffSuccessorLaunchOwnershipSnapshotTx(ctx context.Context, tx Tx, childRunID, executorID string, claimGeneration int64, at string) error {
	record, ok, err := loadAgentRegistrationByRunTx(ctx, tx, childRunID)
	if err != nil || !ok {
		return err
	}
	if record.ExecutorID != executorID || record.ClaimGeneration != claimGeneration || strings.TrimSpace(record.AttemptID) == "" {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT decision_id, output_json
		FROM delivery_decisions
		WHERE project_id = ? AND delivery_run_id = ? AND task_id = ? AND decision_kind = 'handoff-successor-launch'`,
		record.ProjectID, record.DeliveryRunID, record.TaskID)
	if err != nil {
		return fmt.Errorf("refresh handoff ownership snapshot: %w", err)
	}
	defer rows.Close()
	matches := map[string]map[string]any{}
	for rows.Next() {
		var decisionID, outputJSON string
		if err := rows.Scan(&decisionID, &outputJSON); err != nil {
			return fmt.Errorf("scan handoff ownership snapshot decision: %w", err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(outputJSON), &payload); err != nil {
			return federationError(ErrInvalidRecordCode, "handoff successor launch decision payload is invalid")
		}
		if strings.TrimSpace(fmt.Sprint(payload["successor_attempt_id"])) != record.AttemptID {
			continue
		}
		matches[decisionID] = payload
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan handoff ownership snapshot decisions: %w", err)
	}
	if len(matches) == 0 {
		return nil
	}
	snapshot, err := handoffOwnershipLockSnapshotForRegistrationTx(ctx, tx, record, at)
	if err != nil {
		return err
	}
	for decisionID, payload := range matches {
		payload["ownership_locks"] = normalizeHandoffOwnershipLockSnapshot(snapshot)
		updated, err := json.Marshal(payload)
		if err != nil {
			return federationError(ErrInvalidRecordCode, "marshal handoff ownership snapshot: %v", err)
		}
		outputJSON := string(updated)
		if _, err := tx.Exec(ctx, `UPDATE delivery_decisions SET output_json = ? WHERE decision_id = ?`, outputJSON, decisionID); err != nil {
			return fmt.Errorf("update handoff ownership snapshot: %w", err)
		}
	}
	return nil
}

func loadHandoffSuccessorAttemptTx(ctx context.Context, tx Tx, handoff HandoffTransaction, attemptID string) (HandoffSuccessorLaunch, bool, error) {
	var projectID, deliveryRunID, taskID, executorID, providerKey string
	var generation int64
	err := tx.QueryRow(ctx, `SELECT project_id, delivery_run_id, task_id, executor_id, claim_generation, provider_idempotency_key
		FROM delivery_attempts WHERE attempt_id = ?`, attemptID).Scan(&projectID, &deliveryRunID, &taskID, &executorID, &generation, &providerKey)
	if err == sql.ErrNoRows {
		return HandoffSuccessorLaunch{}, false, nil
	}
	if err != nil {
		return HandoffSuccessorLaunch{}, false, err
	}
	var outputJSON string
	err = tx.QueryRow(ctx, `SELECT output_json FROM delivery_decisions WHERE project_id = ? AND delivery_run_id = ? AND decision_key = ?`,
		projectID, deliveryRunID, "handoff-successor:"+handoff.HandoffID).Scan(&outputJSON)
	if err != nil && err != sql.ErrNoRows {
		return HandoffSuccessorLaunch{}, false, err
	}
	var payload struct {
		HandoffID           string                         `json:"handoff_id"`
		HandoffGeneration   int64                          `json:"handoff_generation"`
		SourceAttemptID     string                         `json:"source_attempt_id"`
		RoutingDecisionID   string                         `json:"routing_decision_id"`
		RoutingCandidateID  string                         `json:"routing_candidate_id"`
		BudgetReservationID string                         `json:"budget_reservation_id"`
		ReusableEvidenceIDs []string                       `json:"reusable_evidence_ids"`
		ContinuationProof   HandoffContinuationProof       `json:"continuation_proof"`
		OwnershipLocks      []HandoffOwnershipLockSnapshot `json:"ownership_locks"`
		LaunchPhase         string                         `json:"launch_phase"`
	}
	if outputJSON != "" {
		_ = json.Unmarshal([]byte(outputJSON), &payload)
	}
	var claimPhase, providerReceipt, leaseExpiresAt string
	_ = tx.QueryRow(ctx, `SELECT phase, provider_receipt, lease_expires_at FROM run_claims WHERE run_id = ?`, handoff.ChildRunID).Scan(&claimPhase, &providerReceipt, &leaseExpiresAt)
	return HandoffSuccessorLaunch{
		HandoffID:              firstNonEmptyAgent(payload.HandoffID, handoff.HandoffID),
		HandoffGeneration:      firstPositiveAgent(payload.HandoffGeneration, generation),
		AttemptID:              attemptID,
		SourceAttemptID:        firstNonEmptyAgent(payload.SourceAttemptID, handoff.SourceAttemptID),
		TaskID:                 taskID,
		DeliveryRunID:          deliveryRunID,
		ProjectID:              projectID,
		ExecutorID:             executorID,
		ClaimGeneration:        generation,
		ProviderIdempotencyKey: providerKey,
		RoutingDecisionID:      payload.RoutingDecisionID,
		RoutingCandidateID:     payload.RoutingCandidateID,
		BudgetReservationID:    payload.BudgetReservationID,
		ReusableEvidenceIDs:    sortedCopyAgent(payload.ReusableEvidenceIDs),
		ContinuationProof:      normalizeHandoffContinuationProof(payload.ContinuationProof),
		OwnershipLocks:         normalizeHandoffOwnershipLockSnapshot(payload.OwnershipLocks),
		LaunchExposed:          false,
		LaunchPhase:            firstNonEmptyAgent(claimPhase, payload.LaunchPhase),
		ProviderReceipt:        providerReceipt,
		LeaseExpiresAt:         leaseExpiresAt,
	}, true, nil
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

func normalizeHandoffOwnershipLockSnapshot(values []HandoffOwnershipLockSnapshot) []HandoffOwnershipLockSnapshot {
	byID := map[string]HandoffOwnershipLockSnapshot{}
	for _, value := range values {
		id := strings.TrimSpace(value.LockID)
		if id == "" || value.LockGeneration <= 0 {
			continue
		}
		byID[id] = HandoffOwnershipLockSnapshot{LockID: id, LockGeneration: value.LockGeneration}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]HandoffOwnershipLockSnapshot, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
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
	case "idempotent", "idempotent-exact-key", "exact-idempotency-key":
		return SideEffectStateIdempotent
	case "receipt-backed", "receipt_backed", "provider-receipt", "receipt":
		return SideEffectStateReceiptBacked
	case "provider-started", "launched", "started":
		return SideEffectStateProviderStart
	case "irreversible":
		return SideEffectStateIrreversible
	case "ambiguous":
		return SideEffectStateAmbiguous
	default:
		return firstNonEmptyAgent(value, SideEffectStateUnknown)
	}
}

func hashSecretEvidence(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return hashString(value)
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
